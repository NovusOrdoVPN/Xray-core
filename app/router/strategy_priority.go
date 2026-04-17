package router

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/extension"
)

// RouteChangeCallback is called when PriorityStrategy switches to a different outbound.
// The argument is the new outbound tag (e.g., "direct-00" or "relay-00").
var (
	routeChangeMu       sync.Mutex
	routeChangeCallback func(tag string)
)

// SetRouteChangeCallback registers a callback that fires when the active
// outbound changes. Call with nil to unregister.
func SetRouteChangeCallback(cb func(tag string)) {
	routeChangeMu.Lock()
	routeChangeCallback = cb
	routeChangeMu.Unlock()
}

// ClearRouteChangeCallback unregisters the route change callback.
// Called on xray stop to prevent stale notifications.
func ClearRouteChangeCallback() {
	routeChangeMu.Lock()
	routeChangeCallback = nil
	routeChangeMu.Unlock()
}

// PriorityStrategy picks the first alive outbound in sorted order.
// Tags like "direct-0", "direct-1", "relay-0" are tried in that order.
// Alive outbounds are preferred over unknown (not yet probed).
type PriorityStrategy struct {
	FallbackTag string

	ctx         context.Context
	observatory extension.Observatory

	mu         sync.Mutex
	lastPicked string
}

func (s *PriorityStrategy) InjectContext(ctx context.Context) {
	s.ctx = ctx
	common.Must(core.RequireFeatures(s.ctx, func(obs extension.Observatory) error {
		s.observatory = obs
		return nil
	}))
}

// Note on common.Must safety: RequireFeatures registers the callback and returns nil.
// If no observatory is configured, the callback simply never fires and s.observatory
// stays nil. PickOutbound handles nil observatory by picking the first candidate.

func (s *PriorityStrategy) GetPrincipleTarget(candidates []string) []string {
	if picked := s.PickOutbound(candidates); picked != "" {
		return []string{picked}
	}
	return candidates
}

func (s *PriorityStrategy) PickOutbound(candidates []string) string {
	if s.observatory == nil {
		if len(candidates) > 0 {
			return candidates[0]
		}
		return ""
	}

	observeReport, err := s.observatory.GetObservation(s.ctx)
	if err != nil {
		if len(candidates) > 0 {
			return candidates[0]
		}
		return ""
	}

	statusMap := make(map[string]*observatory.OutboundStatus)
	if result, ok := observeReport.(*observatory.ObservationResult); ok {
		for _, st := range result.Status {
			statusMap[st.OutboundTag] = st
		}
	}

	// Phase 1: First ALIVE outbound (verified working, in priority order).
	// Since candidates are sorted alphabetically, direct-00 < direct-01 < relay-00,
	// so direct is always preferred over relay when both are alive.
	for _, c := range candidates {
		if st, found := statusMap[c]; found && st.Alive {
			s.notifyIfChanged(c)
			return c
		}
	}

	// Phase 2: First UNKNOWN outbound (not yet probed — optimistic).
	// Only reached on cold start when no probes have completed yet.
	for _, c := range candidates {
		if _, found := statusMap[c]; !found {
			return c
		}
	}

	// Phase 3: All dead → empty → triggers fallbackTag
	return ""
}

func (s *PriorityStrategy) notifyIfChanged(tag string) {
	if tag == "" {
		return
	}
	s.mu.Lock()
	if tag == s.lastPicked {
		s.mu.Unlock()
		return
	}
	s.lastPicked = tag
	s.mu.Unlock()

	routeChangeMu.Lock()
	cb := routeChangeCallback
	routeChangeMu.Unlock()
	if cb != nil {
		cb(tag)
	}
}
