package metrics

import (
	"encoding/json"
	"strings"
	"testing"

	app_stats "github.com/xtls/xray-core/app/stats"
	feature_stats "github.com/xtls/xray-core/features/stats"
)

func fakeVisit(maps map[string]feature_stats.OnlineMap) func(func(string, feature_stats.OnlineMap) bool) {
	return func(fn func(string, feature_stats.OnlineMap) bool) {
		for name, om := range maps {
			if !fn(name, om) {
				return
			}
		}
	}
}

func TestOnlineUsersAttrsAndBreakdown(t *testing.T) {
	direct := app_stats.NewOnlineMap()
	proxy := app_stats.NewOnlineMap()
	direct.AddIP("u1")
	direct.SetAttr("u1", "os=ios;net=cell;asn=31163;via=direct-00")
	proxy.AddIP("u1") // same user, newer (same second → winner by mode election stays deterministic: later map wins ties by lastSeen only)
	proxy.SetAttr("u1", "os=ios;net=cell;asn=31163;via=relay-00-00")
	proxy.AddIP("u2") // no attr → unknown
	direct.AddIP("u3")
	direct.SetAttr("u3", "os=android;asn=8359;via=direct-01")
	maps := map[string]feature_stats.OnlineMap{
		"inbound>>>vless-reality>>>online": direct,
		"inbound>>>vless-proxy>>>online":   proxy,
	}

	resp := buildOnlineUsersResponseFrom(fakeVisit(maps))
	if len(resp.Direct)+len(resp.Proxy) != 3 {
		t.Fatalf("expected 3 users total, got direct=%d proxy=%d", len(resp.Direct), len(resp.Proxy))
	}
	if resp.Attrs["u3"] != "os=android;asn=8359;via=direct-01" {
		t.Fatalf("u3 attr missing: %q", resp.Attrs["u3"])
	}
	if _, ok := resp.Attrs["u2"]; ok {
		t.Fatal("users without an attr must not appear in attrs")
	}
	if a := resp.Attrs["u1"]; a == "" {
		t.Fatal("u1 attr must be present (from whichever map won)")
	}

	b := buildOnlineBreakdownFrom(fakeVisit(maps))
	if b["os"]["ios"] != 1 || b["os"]["android"] != 1 || b["os"]["unknown"] != 1 {
		t.Fatalf("os breakdown wrong: %+v", b["os"])
	}
	if b["asn"]["31163"] != 1 || b["asn"]["8359"] != 1 || b["asn"]["unknown"] != 1 {
		t.Fatalf("asn breakdown wrong: %+v", b["asn"])
	}
	if b["net"]["cell"] != 1 || b["net"]["unknown"] != 2 {
		t.Fatalf("net breakdown wrong: %+v", b["net"])
	}
	total := 0
	for _, n := range b["via"] {
		total += n
	}
	if total != 3 {
		t.Fatalf("every user must be counted exactly once per dimension, via total %d", total)
	}
}

func TestOnlineUsersDoorsPerInbound(t *testing.T) {
	direct := app_stats.NewOnlineMap()
	proxyTW := app_stats.NewOnlineMap()
	direct.AddIP("u1")
	proxyTW.AddIP("u1") // same user on two doors in the same second
	proxyTW.AddIP("u2")
	maps := map[string]feature_stats.OnlineMap{
		"inbound>>>vless-reality>>>online":               direct,
		"inbound>>>vless-reality-proxy-timeweb>>>online": proxyTW,
		"outbound>>>freedom>>>online":                    app_stats.NewOnlineMap(), // not an inbound → ignored
	}

	resp := buildOnlineUsersResponseFrom(fakeVisit(maps))

	// doors: one map per inbound, NOT deduplicated — u1 appears on both doors.
	if len(resp.Doors) != 2 {
		t.Fatalf("expected 2 doors, got %d: %v", len(resp.Doors), resp.Doors)
	}
	var u1Sec int64
	direct.ForEach(func(id string, lastSeen int64) bool {
		if id == "u1" {
			u1Sec = lastSeen
		}
		return true
	})
	if ms, ok := resp.Doors["vless-reality"]["u1"]; !ok || u1Sec == 0 || ms != u1Sec*1000 {
		t.Fatalf("u1 door value must be the map's lastSeen in ms: got %d want %d ok=%v", ms, u1Sec*1000, ok)
	}
	tw := resp.Doors["vless-reality-proxy-timeweb"]
	if len(tw) != 2 || tw["u1"] <= 0 || tw["u2"] <= 0 {
		t.Fatalf("timeweb door must hold u1 and u2: %v", tw)
	}
	// The elected direct/proxy value for a user is one of its door values (same ms conversion).
	for id, ms := range resp.Direct {
		if ms != resp.Doors["vless-reality"][id] && ms != tw[id] {
			t.Fatalf("direct[%s]=%d matches no door value", id, ms)
		}
	}
	for id, ms := range resp.Proxy {
		if ms != tw[id] {
			t.Fatalf("proxy[%s]=%d must equal the timeweb door value %d", id, ms, tw[id])
		}
	}
	// direct/proxy keep today's deduplicated contract: 2 distinct users in total.
	if len(resp.Direct)+len(resp.Proxy) != 2 {
		t.Fatalf("direct/proxy must stay deduplicated: direct=%d proxy=%d", len(resp.Direct), len(resp.Proxy))
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"doors":{"vless-reality":{"u1":`) {
		t.Fatalf("doors must be serialised under the key \"doors\" with sorted tags: %s", b)
	}
}

func TestOnlineUsersDoorsOmittedWhenNoInbounds(t *testing.T) {
	resp := buildOnlineUsersResponseFrom(fakeVisit(map[string]feature_stats.OnlineMap{}))
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"doors"`) {
		t.Fatalf("no inbound maps → no doors key (old-consumer compatibility): %s", b)
	}
	if !strings.Contains(string(b), `"direct":{}`) || !strings.Contains(string(b), `"proxy":{}`) {
		t.Fatalf("direct/proxy must still be present and empty: %s", b)
	}
}
