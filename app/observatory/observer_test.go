package observatory

import (
	"reflect"
	"sync"
	"testing"
)

func TestObserverUpdateStatusPrunesStaleOutbounds(t *testing.T) {
	observer := &Observer{
		status: []*OutboundStatus{
			{
				OutboundTag:     "keep",
				Alive:           true,
				Delay:           42,
				LastErrorReason: "",
				LastSeenTime:    111,
				LastTryTime:     222,
			},
			{
				OutboundTag:     "drop",
				Alive:           false,
				Delay:           99999999,
				LastErrorReason: "probe failed",
				LastSeenTime:    333,
				LastTryTime:     444,
			},
		},
	}

	observer.clearRemovedOutbounds([]string{"keep"})

	if len(observer.status) != 1 {
		t.Fatalf("expected 1 status after pruning, got %d", len(observer.status))
	}

	got := observer.status[0]
	if got.OutboundTag != "keep" {
		t.Fatalf("expected remaining status for keep, got %q", got.OutboundTag)
	}
	if !got.Alive {
		t.Fatal("expected remaining status to preserve Alive field")
	}
	if got.Delay != 42 {
		t.Fatalf("expected remaining status to preserve Delay, got %d", got.Delay)
	}
	if got.LastSeenTime != 111 {
		t.Fatalf("expected remaining status to preserve LastSeenTime, got %d", got.LastSeenTime)
	}
	if got.LastTryTime != 222 {
		t.Fatalf("expected remaining status to preserve LastTryTime, got %d", got.LastTryTime)
	}
}

func TestObserverUpdateStatusClearsWhenNoOutboundsRemain(t *testing.T) {
	observer := &Observer{
		status: []*OutboundStatus{
			{OutboundTag: "drop-1"},
			{OutboundTag: "drop-2"},
		},
	}

	observer.clearRemovedOutbounds(nil)

	if len(observer.status) != 0 {
		t.Fatalf("expected all statuses to be removed, got %d", len(observer.status))
	}
}

func newTestObserver(rise, fall uint32) *Observer {
	return &Observer{config: &Config{Rise: rise, Fall: fall}, counters: map[string]*probeCounter{}}
}

func statusOf(o *Observer, tag string) *OutboundStatus {
	if i := o.findStatusLocationLockHolderOnly(tag); i != -1 {
		return o.status[i]
	}
	return nil
}

func TestRiseFallDefaultsAreUpstream(t *testing.T) {
	o := newTestObserver(0, 0) // zero values = upstream behaviour (1/1)
	o.updateStatusForResult("a", &ProbeResult{Alive: false, LastErrorReason: "x"})
	if statusOf(o, "a").Alive {
		t.Fatal("one failure must mark dead with fall=0 (upstream)")
	}
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 10})
	if !statusOf(o, "a").Alive {
		t.Fatal("one pass must mark alive with rise=0 (upstream)")
	}
}

func TestFallThreshold(t *testing.T) {
	o := newTestObserver(1, 2)
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 10})
	o.updateStatusForResult("a", &ProbeResult{Alive: false})
	if !statusOf(o, "a").Alive {
		t.Fatal("first failure must not mark dead when fall=2")
	}
	o.updateStatusForResult("a", &ProbeResult{Alive: false})
	if statusOf(o, "a").Alive {
		t.Fatal("second consecutive failure must mark dead")
	}
}

func TestRiseThresholdAfterDown(t *testing.T) {
	o := newTestObserver(2, 1)
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 10}) // cold start: one pass is enough
	if !statusOf(o, "a").Alive {
		t.Fatal("cold-start pass must mark alive immediately")
	}
	o.updateStatusForResult("a", &ProbeResult{Alive: false})
	if statusOf(o, "a").Alive {
		t.Fatal("fall=1: one failure marks dead")
	}
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 10})
	if statusOf(o, "a").Alive {
		t.Fatal("rise=2: one pass after down must not mark alive")
	}
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 10})
	if !statusOf(o, "a").Alive {
		t.Fatal("rise=2: second consecutive pass marks alive")
	}
	if statusOf(o, "a").Delay != 10 {
		t.Fatalf("delay must be recorded on the pass that revives, got %d", statusOf(o, "a").Delay)
	}
}

func TestRiseCounterResetsOnFailure(t *testing.T) {
	o := newTestObserver(2, 1)
	o.updateStatusForResult("a", &ProbeResult{Alive: false})
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 1})
	o.updateStatusForResult("a", &ProbeResult{Alive: false})
	o.updateStatusForResult("a", &ProbeResult{Alive: true, Delay: 1})
	if statusOf(o, "a").Alive {
		t.Fatal("a failure between passes must reset the rise counter")
	}
}

func TestClearRemovedOutboundsDropsCounters(t *testing.T) {
	o := newTestObserver(2, 1)
	o.updateStatusForResult("gone", &ProbeResult{Alive: false})
	o.clearRemovedOutbounds([]string{})
	if _, ok := o.counters["gone"]; ok {
		t.Fatal("counters of removed outbounds must be pruned")
	}
}

// fakeProbe lets tests script probe results per tag and record which tags were probed.
type fakeProbe struct {
	mu     sync.Mutex
	alive  map[string]bool
	probed []string
}

func (f *fakeProbe) probe(tag string) ProbeResult {
	f.mu.Lock()
	f.probed = append(f.probed, tag)
	f.mu.Unlock()
	return ProbeResult{Alive: f.alive[tag], Delay: 1}
}

func (f *fakeProbe) reset() {
	f.mu.Lock()
	f.probed = nil
	f.mu.Unlock()
}

func TestLazySkipsLowerTiersWhileUpperAlive(t *testing.T) {
	o := newTestObserver(1, 1)
	o.config.Lazy = true
	f := &fakeProbe{alive: map[string]bool{"direct-00": true, "relay-00-00": true, "relay-01-00": true}}
	o.probeTiers([]string{"direct-00", "relay-00-00", "relay-01-00"}, f.probe)
	if !reflect.DeepEqual(f.probed, []string{"direct-00"}) {
		t.Fatalf("only the first tier must be probed while it is alive, got %v", f.probed)
	}
	if statusOf(o, "relay-00-00") != nil {
		t.Fatal("unprobed tiers must keep no status (unknown)")
	}
}

func TestLazyProbesLowerTierWhenUpperDead(t *testing.T) {
	o := newTestObserver(1, 1)
	o.config.Lazy = true
	f := &fakeProbe{alive: map[string]bool{"direct-00": false, "relay-00-00": true, "relay-01-00": true}}
	o.probeTiers([]string{"direct-00", "relay-00-00", "relay-01-00"}, f.probe)
	if !reflect.DeepEqual(f.probed, []string{"direct-00", "relay-00-00"}) {
		t.Fatalf("expected direct then relay-00 tier only, got %v", f.probed)
	}
	if !statusOf(o, "relay-00-00").Alive {
		t.Fatal("relay-00-00 must be alive")
	}
}

func TestLazyRecoveredUpperTierWinsAgain(t *testing.T) {
	o := newTestObserver(2, 1)
	o.config.Lazy = true
	tags := []string{"direct-00", "relay-00-00"}
	f := &fakeProbe{alive: map[string]bool{"direct-00": false, "relay-00-00": true}}
	o.probeTiers(tags, f.probe) // direct dead, relay alive
	f.alive["direct-00"] = true
	f.reset()
	o.probeTiers(tags, f.probe) // one pass: still dead (rise=2), relay still probed
	if statusOf(o, "direct-00").Alive {
		t.Fatal("rise=2: one pass must not revive direct-00")
	}
	if !reflect.DeepEqual(f.probed, []string{"direct-00", "relay-00-00"}) {
		t.Fatalf("while direct is still dead the relay tier must be probed, got %v", f.probed)
	}
	f.reset()
	o.probeTiers(tags, f.probe) // second pass: alive again → relay tier skipped
	if !statusOf(o, "direct-00").Alive {
		t.Fatal("second pass must revive direct-00")
	}
	if !reflect.DeepEqual(f.probed, []string{"direct-00"}) {
		t.Fatalf("once direct is alive the relay tier must be skipped, got %v", f.probed)
	}
}

func TestLazyEverythingDeadProbesAllTiers(t *testing.T) {
	o := newTestObserver(1, 1)
	o.config.Lazy = true
	f := &fakeProbe{alive: map[string]bool{}}
	o.probeTiers([]string{"direct-00", "relay-00-00", "relay-01-00"}, f.probe)
	if len(f.probed) != 3 {
		t.Fatalf("with every tier dead all tiers must be probed, got %v", f.probed)
	}
}
