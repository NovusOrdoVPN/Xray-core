# Client ↔ Server Fork Unification — Consolidated Findings & Plan

## Context

Two xray-core forks currently exist in the project:
- **Server fork:** `/xray/Xray-core/` — based on upstream v26.4.15 with 5 custom features
- **Client fork:** `/xray/xray_mobile_library/xray-core/` — based on upstream v25.12.2 with its own customizations

Goal: **merge client customizations into the server fork**, then point `xray_mobile_library/xraymobile/go.mod` at the unified fork and delete the duplicate.

## Research results (4 groups analyzed in parallel)

| Group | Files | Verdict | Action |
|-------|-------|---------|--------|
| **1. VLESS overlap** (XERR, AuthVerified, ClientVersion) | 5 files | NEEDS_MERGE | Port 3 gaps to server fork |
| **2. PriorityStrategy router** | 4 files (1 NEW) | PORT_AS_IS | Copy new file + 3 integration points |
| **3. TUN proxy** | 13 files in proxy/tun/ | MIGRATION_SAFE | Zero work — client uses upstream's TUN directly |
| **4. Misc** | 7 files | 5 KEEP / 2 DROP | Port 4 files, drop 2 obsolete |

## Group 1 — VLESS gaps (NEEDS_MERGE)

**Wire protocol is compatible** (same magic bytes, same proto fields). BUT server fork is missing 3 client-side pieces:

### HIGH-1: Normal-mode ClientVersion write

Server fork `outbound.go:271-277` only writes `requestAddons.ClientVersion` when `h.relay == true`. Client writes it for EVERY outbound request via `encoding.GetClientVersion()`.

**Fix:** In unified fork `proxy/vless/outbound/outbound.go`, after setting `requestAddons.Flow`, add:
```go
if cv := encoding.GetClientVersion(); cv != "" {
    requestAddons.ClientVersion = cv
}
// relayClientVersion override still applies for relay mode
```

### HIGH-2: Missing package-level Client API in encoding package

Client has (in `proxy/vless/encoding/encoding.go`):
```go
var clientVersion string
func SetClientVersion(v string) { clientVersion = v }
func GetClientVersion() string { return clientVersion }

var authVerifiedCallback func()
func SetAuthVerifiedCallback(cb func()) { authVerifiedCallback = cb }
func ResetAuthVerifiedCallback() { authVerifiedCallback = nil }
func fireAuthVerified() { if authVerifiedCallback != nil { authVerifiedCallback() } }
```

Used by `xraymobile/vpntoolcore/xray.go:79-88` — without these, gomobile wrapper won't compile.

**Fix:** Port this whole block to unified fork `encoding.go`.

### HIGH-3: fireAuthVerified() call

Client calls `fireAuthVerified()` after decoding response addons when `responseAddons.AuthVerified == true`. Unified fork doesn't.

**Fix:** In `DecodeResponseHeader`, after successful `DecodeHeaderAddons`:
```go
if responseAddons.AuthVerified {
    fireAuthVerified()
}
```

## Group 2 — PriorityStrategy (PORT_AS_IS)

New router balancer strategy. Picks the first `Alive` outbound in sort order (e.g., `direct-00` before `relay-00`). Observatory-backed; falls back gracefully.

Also adds gomobile-facing `SetRouteChangeCallback`/`ClearRouteChangeCallback` API so Swift UI can show "Direct" vs "Enhanced" indicator.

**Files to port:**
- NEW: `app/router/strategy_priority.go` (131 lines) — copy as-is
- `app/router/config.go` — add `case "priority":` in `BalancingRule.Build`
- `infra/conf/router.go` — add "priority" to strategy whitelist
- `infra/conf/router_strategy.go` — add constant + empty config loader entry

Zero conflict risk with upstream. Zero server-side impact.

## Group 3 — TUN (MIGRATION_SAFE)

Client's TUN is an older snapshot of upstream's TUN. Upstream v26.4.15's TUN is a strict superset with more features. iOS fd injection mechanism (`platform.TunFdKey = "xray.tun.fd"`, env var pickup in `tun_darwin.go`) is **byte-identical**.

**Zero porting work required:**
- Dart (VPNTool): no changes — `{name, MTU, userLevel}` JSON still works
- Swift (NetworkExtension): no changes — `VpntoolcoreSetTunFd` + fd scan + env-var export unchanged
- Go wrapper: no changes

**Bonus upgrades** the client gets for free from upstream:
- Better UDP fullcone queue (1024 slots with RWMutex vs 16 slots with Mutex)
- `Handler.Close()` for cleaner shutdown
- `AutoOutboundsInterface` available for future WiFi↔cellular switching

## Group 4 — Misc (5 KEEP, 2 DROP)

### KEEP (port to unified fork)

1. **`main/distro/debug/debug.go`** — client deleted the `:6060` pprof HTTP server (iOS security: don't expose a debug server on the NetworkExtension).

2. **splithttp trio** — atomic memory-reclamation feature for 50MB NetworkExtension. Upstream refactored splithttp heavily (dialer.go 504→618 lines), so port **by hand**, not `git apply`.
   - `splithttp/client.go` — adds `CloseTransport()` method
   - `splithttp/dialer.go` — adds `periodicCleanupGlobalMap()` goroutine, lazy 30s sweep, explicit `runtime.GC()`
   - `splithttp/mux.go` — adds `CleanupIdleClients()`, `IsEmpty()`, `closeXmuxConn()` helpers

   Consider wrapping `runtime.GC()` under a mobile build tag.

3. **`common/buf/data/test_MultiBufferReadAllToByte.dat`** — client deleted this 2KB test fixture. Trivial size win; delete from unified fork too if also unused there.

### DROP (already in upstream or obsolete)

1. **`common/platform/platform.go` `TunFdKey`** — already in upstream v26.4.15 (same string literal). No action.

2. **`proxy/wireguard/tun.go` gvisor API fix** — upstream rewrote this block entirely. Client's patch is obsolete.

## Execution plan

Order designed to minimize conflicts and allow incremental testing:

1. **Port Group 2 (PriorityStrategy)** — fully self-contained, no existing code touched beyond 3 small integration points.
2. **Port Group 1 (VLESS gaps)** — 3 small additions to existing files. All server-side behavior preserved (the normal-mode ClientVersion write only activates when SetClientVersion has been called, which only happens via gomobile).
3. **Port Group 4 KEEP (splithttp trio + debug.go)** — the splithttp changes need care due to upstream refactor.
4. **Build & test** unified fork on server-only path (what we have been testing).
5. **Wire up xray_mobile_library**:
   - Change `xraymobile/go.mod` `replace` directive to point at the unified fork directory
   - Test gomobile build (`xcframework` or whatever the build pipeline is)
6. **Delete duplicate** `xray_mobile_library/xray-core/` directory once gomobile build succeeds.

## Risk assessment

| Item | Risk | Mitigation |
|------|------|-----------|
| Normal-mode ClientVersion write | LOW — only activates when SetClientVersion() has been called (only gomobile does this) | None needed |
| `SetAuthVerifiedCallback` global state | LOW — package-global but gated by nil callback | Document clearly |
| splithttp hand-port | MEDIUM — upstream refactored these files substantially | Re-test memory under load on iOS |
| PriorityStrategy | LOW — new file + 3 small integration points | Trivial |
| TUN migration | LOW — byte-identical fd injection | Smoke test on iOS |

## Files ready for next step

- `/tmp/xray-client-analysis/patches/00-all-client-customizations.patch` — full master diff
- `/tmp/xray-client-analysis/patches/0{1-4}*.patch` — per-feature split patches
- `/tmp/xray-client-analysis/research/0{1-4}*.md` — per-group research reports
- This consolidated plan: `/tmp/xray-client-analysis/research/00-consolidated-plan.md`
