package metrics

import (
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
