//go:build !ios && !android

// CUSTOM: desktop/server no-ops for the splithttp memory-reclamation hooks.
//
// Desktop and server builds inherit upstream's reactive-only cleanup
// behavior: expired clients are removed from an XmuxManager's slice when
// the next GetXmuxClient call visits them, and their HTTP transport pools
// are released by Go's GC whenever it next runs.
//
// The mobile build (cleanup_mobile.go) adds aggressive proactive cleanup
// to meet iOS's 50MB NetworkExtension ceiling. Server processes don't have
// a comparable constraint and don't pay for those hooks here.
//
// Every function in this file must have a matching real implementation in
// cleanup_mobile.go (and vice versa).

package splithttp

// mobileForceGC is a no-op on desktop (see cleanup_mobile.go for mobile).
func mobileForceGC() {}

// maybeLazyCleanup is a no-op on desktop — upstream's reactive cleanup in
// GetXmuxClient is sufficient for desktop/server memory profiles.
func maybeLazyCleanup() {}

// maybeCloseOnDiscardXmux is a no-op on desktop — match upstream's behavior
// of just dropping the discarded client from the slice and letting Go's GC
// handle the underlying http.Transport eventually.
func maybeCloseOnDiscardXmux(conn XmuxConn) {}
