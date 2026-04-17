# Group 4: Miscellaneous Modifications — Analysis

**Scope:** Files that don't fit the main feature groups (tower validator, PriorityStrategy, etc.).
**Comparison:** Client fork (`xray_mobile_library/xray-core`) vs pristine v25.12.2 vs upstream v26.4.15 sync branch (`Xray-core` on `sync-upstream-v26.4.15`).

## Executive Summary

| # | File | Verdict |
|---|------|---------|
| 1 | `common/platform/platform.go` (TunFdKey) | **DROP** — already in upstream v26.4.15 |
| 2 | `main/distro/debug/debug.go` (deleted) | **KEEP** — iOS-specific security/size win |
| 3 | `proxy/wireguard/tun.go` (gvisor API fix) | **DROP** — upstream refactored differently |
| 4 | `transport/internet/splithttp/client.go` (CloseTransport) | **KEEP** — supports mobile memory reclaim |
| 5 | `transport/internet/splithttp/dialer.go` (periodic cleanup) | **KEEP** — iOS 50 MB memory pressure fix |
| 6 | `transport/internet/splithttp/mux.go` (CleanupIdleClients) | **KEEP** — paired with dialer.go change |
| 7 | `common/buf/data/test_MultiBufferReadAllToByte.dat` (deleted) | **KEEP** — trivially shrinks binary |

**Totals:** KEEP = 5, DROP = 2, INVESTIGATE = 0.

---

## 1. `common/platform/platform.go` — `TunFdKey` constant

**Diff:** Client adds one constant next to the other `xray.*` env-flag names:

```go
+   TunFdKey = "xray.tun.fd"
```

**Purpose:** Lets the NetworkExtension pass the TUN file descriptor to xray-core via the env-flag mechanism (`proxy/tun/tun_darwin.go` and `tun_android.go` read it through `NewEnvFlag(platform.TunFdKey)`).

**Upstream status:** Already present in v26.4.15 — the pristine v25.12.2 copy does not have it, but `Xray-core/common/platform/platform.go:26` on `sync-upstream-v26.4.15` defines the exact same constant, and the same callers (`tun_darwin.go:52`, `tun_android.go:27`) consume it.

**Verdict:** **DROP.** Identical code already exists in the upstream-synced unified fork. Nothing to port.

---

## 2. `main/distro/debug/debug.go` — DELETED

**Diff:** Client deletes this file entirely. The v25.12.2 file is:

```go
package debug

import ("net/http")

func init() {
    go func() { http.ListenAndServe(":6060", nil) }()
}
```

**Purpose of deletion:** The file spawns an unauthenticated HTTP server on port 6060 serving pprof (`net/http/pprof` is blank-imported somewhere in the default distro). This is useful for desktop debugging, dangerous on mobile:
- In a NetworkExtension, blindly binding `:6060` leaks diagnostic state to any process on the device.
- Silent `ListenAndServe` failures are ignored; on iOS the port may not even be available.
- Adds an unnecessary goroutine to the 50 MB budget.

**Upstream status:** Upstream v26.4.15 still ships `Xray-core/main/distro/debug/debug.go` unchanged.

**Verdict:** **KEEP** — iOS-specific. The unified fork should exclude this file (either delete, or guard with `//go:build !ios && !android && !mobile`). Trivial to port.

---

## 3. `proxy/wireguard/tun.go` — gvisor `udp.NewForwarder` callback signature

**Diff:** Only change is the UDP forwarder callback now returns `bool`:

```go
-   udpForwarder := udp.NewForwarder(stack, func(r *udp.ForwarderRequest) {
+   udpForwarder := udp.NewForwarder(stack, func(r *udp.ForwarderRequest) bool {
        go func(r *udp.ForwarderRequest) {
            ...
            handler(xnet.UDPDestination(...), gonet.NewUDPConn(&wq, ep))
        }(r)
+       return true
    })
```

**Purpose:** Client is on a newer gvisor version where `udp.NewForwarder`'s handler signature became `func(*udp.ForwarderRequest) bool` (handled/not-handled return). Pure compile-time adaptation — no behavioral change.

**Upstream status:** Upstream v26.4.15 replaced this whole block with a different approach (`gstack.SetTransportProtocolHandler(udp.ProtocolNumber, func(id, pkt) bool { ... })` directly, bypassing `udp.NewForwarder` entirely). The gvisor API issue is moot in upstream.

**Verdict:** **DROP.** Upstream's rewrite already supersedes this. If gvisor version mismatches happen again post-merge, fix then, but don't port this specific patch.

---

## 4. `transport/internet/splithttp/client.go` — `CloseTransport` method

**Diff:** Adds one method to `DefaultDialerClient`:

```go
+ // CloseTransport shuts down the underlying HTTP transport and releases
+ // connection pool resources (goroutines, TLS state, buffers).
+ func (c *DefaultDialerClient) CloseTransport() {
+     c.closed = true
+     c.client.CloseIdleConnections()
+ }
```

**Purpose:** Lets the dialer/mux layer forcibly tear down idle HTTP/1.1/HTTP/2/HTTP/3 transports (goroutine pools, TLS session caches, conn pools). Without this, stale `XmuxClient`s were dropped from the slice but their `http.Client.Transport` kept goroutines alive — an iOS NetworkExtension memory leak.

**Upstream status:** Upstream v26.4.15 refactored splithttp heavily (dialer.go 504 → 618 lines, renamed some types, likely xhttp-style work) but does NOT add a `CloseTransport` method. Searched for `CloseTransport|CloseIdleConnections` across upstream `transport/internet/splithttp/` — zero hits.

**Verdict:** **KEEP** — genuine mobile memory fix. Port carefully: upstream's `DefaultDialerClient` still has `client *http.Client`, so the method drops in unchanged.

---

## 5. `transport/internet/splithttp/dialer.go` — periodic cleanup goroutine + lazy sweep

**Diff:** Three additions:

1. `lastCleanupTime time.Time` global.
2. `cleanupGlobalDialerMapLocked()` helper that iterates `globalDialerMap`, calls `manager.CleanupIdleClients()`, and deletes empty managers.
3. `periodicCleanupGlobalMap()` ticker goroutine running every 60 s, launched from `init()`, which also calls `runtime.GC()` after sweeping.
4. Inside `getHTTPClient`, a lazy sweep every 30 s on hot paths.

```go
+ // On memory-constrained platforms (iOS NetworkExtension, 50MB limit),
+ // nudge Go's GC to reclaim freed transport buffers promptly.
+ runtime.GC()
```

**Purpose:** iOS NetworkExtension memory ceiling (~50 MB). When users are connected-but-idle, no fresh `Dial()` calls fire, so the old `GetXmuxClient` cleanup-on-entry never runs and dead managers accumulate. Explicit `runtime.GC()` is a well-known workaround for Go's lazy GC under tight RSS limits — Go's scavenger is conservative by default.

**Upstream status:** Not in upstream. Grep for `periodicCleanup|lastCleanupTime|runtime.GC|CleanupIdleClients` in `Xray-core/transport/internet/splithttp/` returns nothing.

**Verdict:** **KEEP** — directly addresses the CLAUDE.md 50 MB constraint. Port as-is; pair with #4 and #6. Consider gating the aggressive `runtime.GC()` behind a build tag (e.g. `ios`) so desktop users don't pay the STW cost.

---

## 6. `transport/internet/splithttp/mux.go` — `CleanupIdleClients` / `IsEmpty` / `closeXmuxConn`

**Diff:** Three additions (all new, no existing logic touched except an inline `closeXmuxConn(xmuxClient.XmuxConn)` call in `GetXmuxClient` when evicting):

```go
+ func closeXmuxConn(conn XmuxConn) {
+     if closer, ok := conn.(interface{ CloseTransport() }); ok {
+         closer.CloseTransport()
+     }
+ }
+
+ func (m *XmuxManager) CleanupIdleClients() bool { ... }
+ func (m *XmuxManager) IsEmpty() bool { return len(m.xmuxClients) == 0 }
```

**Purpose:** `CleanupIdleClients` is the worker function the periodic goroutine (#5) calls. `closeXmuxConn` is the bridge that invokes `DefaultDialerClient.CloseTransport` (#4) when a client is evicted. `IsEmpty` is the signal for the outer map to delete empty managers.

**Upstream status:** v26.4.15 `mux.go` is byte-identical to v25.12.2 — no cleanup helpers, no `closeXmuxConn`.

**Verdict:** **KEEP** — trio (#4, #5, #6) is atomic. Port together or not at all.

---

## 7. `common/buf/data/test_MultiBufferReadAllToByte.dat` — DELETED

**Diff:** Client deleted a 2319-byte ASCII-art test fixture (the "Made with love / Thank you for your support" decorative file used only by unit tests).

**Purpose:** Shaves 2 KB and one file off the gomobile build output. Trivial but harmless.

**Upstream status:** Still in upstream (`Xray-core/common/buf/data/test_MultiBufferReadAllToByte.dat`, same 2319 bytes).

**Verdict:** **KEEP** — low-priority cosmetic cleanup. Doesn't affect runtime; delete in unified fork if convenient, skip if not. If tests rely on it (they do, in `buf/reader_test.go` or similar), leave it alone instead.

---

## Consolidated Porting Recommendation

**Port to unified fork (5 items):**

1. Remove `main/distro/debug/debug.go` on mobile builds (delete, or add `//go:build !mobile`).
2. Add `DefaultDialerClient.CloseTransport()` to `splithttp/client.go`.
3. Add `XmuxManager.CleanupIdleClients()`, `IsEmpty()`, and `closeXmuxConn()` helper to `splithttp/mux.go`, plus the `closeXmuxConn` call in the eviction path of `GetXmuxClient`.
4. Add `periodicCleanupGlobalMap()` goroutine + `lastCleanupTime` lazy sweep + `runtime.GC()` to `splithttp/dialer.go`. Gate `runtime.GC()` behind `//go:build ios || android` if desired.
5. Delete `common/buf/data/test_MultiBufferReadAllToByte.dat` only if build-size matters; otherwise leave — tests reference it.

**Drop (2 items):**

1. `TunFdKey` constant — already in upstream v26.4.15.
2. `proxy/wireguard/tun.go` gvisor signature fix — upstream rewrote the whole block; don't reapply this specific patch.

**Risk notes:**
- Items #2–#4 above (splithttp trio) land on HEAVILY-refactored upstream files (dialer.go grew 504 → 618 lines). Do the port by hand, not with `git apply`. Verify `XmuxManager`, `DefaultDialerClient`, and `globalDialerMap` still have the same field names in upstream before writing the patch.
- The `runtime.GC()` call hurts throughput on non-mobile; build-tag it.
- Consider adding a regression test: spin up an idle `DefaultDialerClient`, wait 90 s, assert `runtime.MemStats.HeapInuse` dropped — confirms the fix still works on future upstream syncs.
