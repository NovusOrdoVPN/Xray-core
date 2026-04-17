//go:build ios || android

// CUSTOM: mobile-only splithttp memory-reclamation hooks.
//
// On memory-constrained mobile platforms (iOS NetworkExtension has a 50MB
// resident-set ceiling) we aggressively reclaim idle HTTP transport pools
// via three mechanisms:
//
//  1. Background goroutine that sweeps globalDialerMap every 60 seconds.
//     Handles the "VPN connected but idle" case where reactive cleanup
//     never fires.
//  2. Lazy sweep on the active-Dial path every 30 seconds, for bursty
//     workloads where the background goroutine is too slow.
//  3. Explicit CloseTransport() on discarded XmuxClients so Go's GC
//     doesn't hold onto http.Transport connection pools until its next
//     natural cycle.
//  4. runtime.GC() nudge after each cleanup pass to return freed memory
//     to the OS (default GOGC=100 is conservative).
//
// Desktop/server builds get no-op versions of these hooks (see
// cleanup_desktop.go) and thus match upstream's reactive-only cleanup
// behavior — zero new overhead, zero new lock contention.

package splithttp

import (
	"runtime"
	"time"
)

// lastCleanupTime is consulted by maybeLazyCleanup to throttle the active-path
// sweep to at most once per 30 seconds.
var lastCleanupTime time.Time

func init() {
	// Start the background cleanup goroutine on package load.
	go periodicCleanupGlobalMap()
}

// mobileForceGC is called at the end of each cleanup pass (from
// periodicCleanupGlobalMap) to prompt Go to release freed memory to the OS.
func mobileForceGC() {
	runtime.GC()
}

// maybeLazyCleanup performs the 30-second active-path sweep. Must be called
// with globalDialerAccess held (it's invoked from inside getHTTPClient's
// locked region).
func maybeLazyCleanup() {
	if time.Since(lastCleanupTime) > 30*time.Second {
		lastCleanupTime = time.Now()
		cleanupGlobalDialerMapLocked()
	}
}

// maybeCloseOnDiscardXmux is called from mux.go's GetXmuxClient when it
// discards an expired XmuxClient with no active streams. On mobile we
// forcibly close the HTTP transport; on desktop it's a no-op so behavior
// matches upstream.
func maybeCloseOnDiscardXmux(conn XmuxConn) {
	closeXmuxConn(conn)
}
