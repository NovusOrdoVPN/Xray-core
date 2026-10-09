package metrics

// CUSTOM: HTTP endpoints exposing concurrent online users per inbound.
//
// Consumed by the admin portal. Two endpoints are exposed:
//
//	GET /online         → { "inbound_tag": count, ... }
//	GET /online-users   → { "asOfMs": <int64>, "direct": {uuid: lastSeenMs}, "proxy": {uuid: lastSeenMs},
//	                        "attrs": {uuid: "os=…;via=…"}, "doors": {inboundTag: {uuid: lastSeenMs}} }
//
// Also publishes an "online" variable to expvar for /debug/vars consumers.
//
// This file is additive only; metrics.go calls registerOnlineEndpoints(c)
// from NewMetricsHandler. Keep the external JSON contract stable — the admin
// portal depends on exact field names and types.

import (
	"encoding/json"
	"expvar"
	"net/http"
	"strconv"
	"strings"
	"time"

	feature_stats "github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy/vless/encoding"
)

type onlineUsersResponse struct {
	AsOfMs int64            `json:"asOfMs"`
	Direct map[string]int64 `json:"direct"`
	Proxy  map[string]int64 `json:"proxy"`
	// CUSTOM: client telemetry — the metadata part of the client version string per
	// user ("os=ios;net=cell;asn=...;via=..."), only for users that sent one. The
	// presence collector turns these into per-platform/network/provider counts.
	Attrs map[string]string `json:"attrs,omitempty"`
	// CUSTOM: per-door presence — one map per inbound tag, NOT deduplicated across
	// doors. The presence collector buckets a user by the door it was last seen on
	// (e.g. a relay provider whose relays the app shows as country servers). direct /
	// proxy above keep their contract for older collectors. Omitted when there are
	// no inbound maps.
	Doors map[string]map[string]int64 `json:"doors,omitempty"`
}

type onlineUserPresence struct {
	mode       string
	lastSeenMs int64
	attr       string
}

// onlineMapVisitor abstracts stats.Manager.VisitOnlineMaps so the builders are testable.
type onlineMapVisitor func(func(string, feature_stats.OnlineMap) bool)

// inboundTagFromOnlineMapName extracts "tag" from "inbound>>>tag>>>online".
func inboundTagFromOnlineMapName(name string) string {
	parts := strings.Split(name, ">>>")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// classifyOnlineMode decides whether an inbound tag represents a proxy or
// direct connection path. Case-insensitive "proxy" substring wins.
func classifyOnlineMode(tag string) string {
	if strings.Contains(strings.ToLower(tag), "proxy") {
		return "proxy"
	}
	return "direct"
}

// buildOnlineUsersResponse aggregates all inbound online maps, deduplicating
// users across inbounds by electing the mode (direct/proxy) with the most
// recent activity for each user.
//
// Upstream OnlineMap.ForEach yields Unix *seconds*; convert to milliseconds for
// the admin portal's expected precision.
func buildOnlineUsersResponse(manager feature_stats.Manager) onlineUsersResponse {
	return buildOnlineUsersResponseFrom(manager.VisitOnlineMaps)
}

func buildOnlineUsersResponseFrom(visit onlineMapVisitor) onlineUsersResponse {
	rawByMode := map[string]map[string]int64{
		"direct": {},
		"proxy":  {},
	}
	attrByMode := map[string]map[string]string{
		"direct": {},
		"proxy":  {},
	}
	doors := map[string]map[string]int64{}

	visit(func(name string, om feature_stats.OnlineMap) bool {
		if !strings.HasPrefix(name, "inbound>>>") {
			return true
		}

		tag := inboundTagFromOnlineMapName(name)
		if tag == "" {
			return true
		}

		mode := classifyOnlineMode(tag)
		door := doors[tag]
		if door == nil {
			door = map[string]int64{}
			doors[tag] = door
		}
		om.ForEachAttr(func(userID string, lastSeen int64, attr string) bool {
			lastSeenMs := lastSeen * 1000
			if cur, ok := door[userID]; !ok || lastSeenMs > cur {
				door[userID] = lastSeenMs
			}
			if current, found := rawByMode[mode][userID]; !found || lastSeenMs >= current {
				rawByMode[mode][userID] = lastSeenMs
				if attr != "" {
					attrByMode[mode][userID] = attr
				}
			}
			return true
		})

		return true
	})

	winners := make(map[string]onlineUserPresence)
	for _, mode := range []string{"direct", "proxy"} {
		for userID, lastSeenMs := range rawByMode[mode] {
			if existing, found := winners[userID]; !found || lastSeenMs > existing.lastSeenMs {
				winners[userID] = onlineUserPresence{
					mode:       mode,
					lastSeenMs: lastSeenMs,
					attr:       attrByMode[mode][userID],
				}
			}
		}
	}

	resp := onlineUsersResponse{
		AsOfMs: time.Now().UnixMilli(),
		Direct: map[string]int64{},
		Proxy:  map[string]int64{},
		Attrs:  map[string]string{},
		Doors:  doors,
	}
	for userID, presence := range winners {
		if presence.mode == "proxy" {
			resp.Proxy[userID] = presence.lastSeenMs
		} else {
			resp.Direct[userID] = presence.lastSeenMs
		}
		if presence.attr != "" {
			resp.Attrs[userID] = presence.attr
		}
	}

	return resp
}

// buildOnlineBreakdownFrom counts the deduplicated online users per platform, network,
// provider (asn) and relay path, from the stored client metadata. Users without
// metadata (old apps) count as "unknown" in every dimension, so totals always match.
func buildOnlineBreakdownFrom(visit onlineMapVisitor) map[string]map[string]int {
	resp := buildOnlineUsersResponseFrom(visit)
	out := map[string]map[string]int{"os": {}, "net": {}, "asn": {}, "via": {}}
	count := func(dim, value string) {
		if value == "" {
			value = "unknown"
		}
		out[dim][value]++
	}
	users := make([]string, 0, len(resp.Direct)+len(resp.Proxy))
	for u := range resp.Direct {
		users = append(users, u)
	}
	for u := range resp.Proxy {
		users = append(users, u)
	}
	for _, u := range users {
		m := encoding.ParseClientMeta("||" + resp.Attrs[u])
		count("os", m.OS)
		count("net", m.Net)
		if m.ASN > 0 {
			count("asn", strconv.FormatUint(uint64(m.ASN), 10))
		} else {
			count("asn", "unknown")
		}
		count("via", m.Via)
	}
	return out
}

// registerOnlineEndpoints wires up the /online and /online-users HTTP handlers
// and publishes the "online" expvar. Called once from NewMetricsHandler.
func registerOnlineEndpoints(c *MetricsHandler) {
	expvar.Publish("online", expvar.Func(func() interface{} {
		resp := map[string]int{}
		c.statsManager.VisitOnlineMaps(func(name string, om feature_stats.OnlineMap) bool {
			if strings.HasPrefix(name, "inbound>>>") {
				if tag := inboundTagFromOnlineMapName(name); tag != "" {
					resp[tag] = om.Count()
				}
			}
			return true
		})
		return resp
	}))

	http.HandleFunc("/online", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{}
		c.statsManager.VisitOnlineMaps(func(name string, om feature_stats.OnlineMap) bool {
			if strings.HasPrefix(name, "inbound>>>") {
				if tag := inboundTagFromOnlineMapName(name); tag != "" {
					resp[tag] = om.Count()
				}
			}
			return true
		})
		// CUSTOM: client telemetry — per-dimension counts of the deduplicated online users.
		resp["breakdown"] = buildOnlineBreakdownFrom(c.statsManager.VisitOnlineMaps)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/online-users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildOnlineUsersResponse(c.statsManager))
	})
}
