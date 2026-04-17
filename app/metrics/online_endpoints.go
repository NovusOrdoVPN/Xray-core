package metrics

// CUSTOM: HTTP endpoints exposing concurrent online users per inbound.
//
// Consumed by the admin portal. Two endpoints are exposed:
//
//	GET /online         → { "inbound_tag": count, ... }
//	GET /online-users   → { "asOfMs": <int64>, "direct": {uuid: lastSeenMs}, "proxy": {uuid: lastSeenMs} }
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
	"strings"
	"time"

	feature_stats "github.com/xtls/xray-core/features/stats"
)

type onlineUsersResponse struct {
	AsOfMs int64            `json:"asOfMs"`
	Direct map[string]int64 `json:"direct"`
	Proxy  map[string]int64 `json:"proxy"`
}

type onlineUserPresence struct {
	mode       string
	lastSeenMs int64
}

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
	rawByMode := map[string]map[string]int64{
		"direct": {},
		"proxy":  {},
	}

	manager.VisitOnlineMaps(func(name string, om feature_stats.OnlineMap) bool {
		if !strings.HasPrefix(name, "inbound>>>") {
			return true
		}

		tag := inboundTagFromOnlineMapName(name)
		if tag == "" {
			return true
		}

		mode := classifyOnlineMode(tag)
		om.ForEach(func(userID string, lastSeen int64) bool {
			lastSeenMs := lastSeen * 1000
			if current, found := rawByMode[mode][userID]; !found || lastSeenMs > current {
				rawByMode[mode][userID] = lastSeenMs
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
				}
			}
		}
	}

	resp := onlineUsersResponse{
		AsOfMs: time.Now().UnixMilli(),
		Direct: map[string]int64{},
		Proxy:  map[string]int64{},
	}
	for userID, presence := range winners {
		if presence.mode == "proxy" {
			resp.Proxy[userID] = presence.lastSeenMs
		} else {
			resp.Direct[userID] = presence.lastSeenMs
		}
	}

	return resp
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
		resp := map[string]int{}
		c.statsManager.VisitOnlineMaps(func(name string, om feature_stats.OnlineMap) bool {
			if strings.HasPrefix(name, "inbound>>>") {
				if tag := inboundTagFromOnlineMapName(name); tag != "" {
					resp[tag] = om.Count()
				}
			}
			return true
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/online-users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildOnlineUsersResponse(c.statsManager))
	})
}
