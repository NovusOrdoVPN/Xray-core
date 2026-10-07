package router

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/features/extension"
	"google.golang.org/protobuf/proto"
)

type fakeObservatory struct{ status []*observatory.OutboundStatus }

func (f *fakeObservatory) GetObservation(context.Context) (proto.Message, error) {
	return &observatory.ObservationResult{Status: f.status}, nil
}
func (f *fakeObservatory) Type() interface{} { return extension.ObservatoryType() }
func (f *fakeObservatory) Start() error      { return nil }
func (f *fakeObservatory) Close() error      { return nil }

// Pins the order the lazy observatory relies on: a tier that was never probed
// is "unknown" and is picked at once when everything above is dead; a probed
// alive outbound beats an unknown one; everything dead → "" (fallbackTag).
func TestPriorityPicksFirstAliveThenFirstUnknownThenFallback(t *testing.T) {
	obs := &fakeObservatory{status: []*observatory.OutboundStatus{
		{OutboundTag: "direct-00", Alive: false},
		{OutboundTag: "relay-00-00", Alive: false},
		// relay-01-00 never probed (lazy) → unknown
	}}
	s := &PriorityStrategy{observatory: obs}
	c := []string{"direct-00", "relay-00-00", "relay-01-00"}
	if got := s.PickOutbound(c); got != "relay-01-00" {
		t.Fatalf("all probed dead → first unknown, got %q", got)
	}
	obs.status[1].Alive = true
	if got := s.PickOutbound(c); got != "relay-00-00" {
		t.Fatalf("alive beats unknown, got %q", got)
	}
	obs.status[1].Alive = false
	obs.status = append(obs.status, &observatory.OutboundStatus{OutboundTag: "relay-01-00", Alive: false})
	if got := s.PickOutbound(c); got != "" {
		t.Fatalf("everything dead → empty (fallbackTag), got %q", got)
	}
	obs.status[0].Alive = true
	if got := s.PickOutbound(c); got != "direct-00" {
		t.Fatalf("recovered direct wins over everything, got %q", got)
	}
}
