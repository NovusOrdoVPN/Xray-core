# Client ↔ Server Fork Unification — Next Steps (Handoff)

> **This document is the authoritative handoff for continuing the unification work
> in a fresh conversation.** Read it top to bottom before making any changes.
> Every claim here was verified on 2026-04-17 by a previous session.

---

## TL;DR

Two xray-core forks currently exist. The server one (this repo) was just rebased
onto upstream v26.4.15. The client one (at `xray/xray_mobile_library/xray-core/`)
is still on v25.12.2 with its own customizations. The goal is to merge the
client-specific customizations into this unified fork, then point the mobile
library at this repo and delete the duplicate.

All research and planning is done. **The remaining work is code-level porting.**

---

## Current state (as of 2026-04-17, commit `711fbfa7`)

- **Branch:** `sync-upstream-v26.4.15` (not merged to main, not pushed)
- **Last commit:** `711fbfa7 feat: re-apply fork customizations on upstream v26.4.15`
- **Working tree:** clean (until this preservation commit lands)
- **Build status:** `go build ./...` exits 0
- **Test status:** all affected packages pass (pre-existing geodata failures unrelated)
- **Backup:** `origin/backup-pre-rebase-2026-04-17` (the pre-rebase state of the fork)

### What the server fork already has (verified in this branch)

The 5 original features are re-applied with a maintainable structure:

| Feature | Lives in |
|---------|----------|
| Remote validator (tower HTTP auth) | `proxy/vless/inbound/validator_remote.go`, `validator_meta.go` (optional interface) |
| Relay validator (passthrough) | `proxy/vless/inbound/validator_relay.go` |
| XERR structured error responses | `proxy/vless/encoding/error_response.go`, `proxy/vless/inbound/error_response.go` |
| Online users metrics (`/online`, `/online-users`) | `app/metrics/online_endpoints.go`, `app/dispatcher/custom_online.go` |
| Relay mode + VlessRoute bytes 8:10 | inline in `inbound.go`, `outbound.go`, `session.go` |

Also: AuthVerified + ClientVersion proto fields (field 3, 4 in addons.proto).

All modifications are marked with `CUSTOM:` / `CUSTOM-BEGIN:` / `CUSTOM-END:`
comments — `grep -rn "CUSTOM" --include="*.go" --include="*.proto"` lists them.

---

## Remaining work

### 0. Preservation commit (this commit)

Added: research docs + patches + this handoff doc.
No code changes.

### 1. Port VLESS gaps (3 HIGH-severity items)

**Why this matters:** The gomobile wrapper at
`/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/vpntoolcore/xray.go:79-88`
calls `encoding.SetClientVersion` and `encoding.SetAuthVerifiedCallback`
directly. Without these, the unified fork won't compile as a gomobile
dependency, and the iOS client will lose version telemetry + auth-verified
event.

Reference: [research/01-vless-overlap.md](research/01-vless-overlap.md)

**Task 1a — package-level API in `proxy/vless/encoding/encoding.go`**

Add (near top of file, after the `Version` const):

```go
// CUSTOM-BEGIN: client-side VLESS state (used by gomobile wrapper)
var clientVersion string

// SetClientVersion sets the VLESS ClientVersion addon value to include in
// every outbound request. Called by the gomobile wrapper before starting xray.
func SetClientVersion(v string) { clientVersion = v }

// GetClientVersion returns the value set by SetClientVersion.
func GetClientVersion() string { return clientVersion }

var authVerifiedCallback func()

// SetAuthVerifiedCallback registers a callback fired by the client-side VLESS
// decoder when the server sets responseAddons.AuthVerified=true. Used by the
// gomobile wrapper to surface an "auth verified" event to the iOS UI.
func SetAuthVerifiedCallback(cb func()) { authVerifiedCallback = cb }

// ResetAuthVerifiedCallback clears the callback.
func ResetAuthVerifiedCallback() { authVerifiedCallback = nil }

func fireAuthVerified() {
	if authVerifiedCallback != nil {
		authVerifiedCallback()
	}
}
// CUSTOM-END
```

**Task 1b — fire the callback after decode in `DecodeResponseHeader`**

In the same file, after `DecodeHeaderAddons` succeeds, add:

```go
// CUSTOM: notify client of server-confirmed auth (gomobile wrapper consumes this).
if responseAddons.AuthVerified {
	fireAuthVerified()
}
```

Check the exact line — should be right before `return responseAddons, nil` in
`DecodeResponseHeader`. Verify the client's version of this file at
`/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/proxy/vless/encoding/encoding.go`
for exact placement.

**Task 1c — normal-mode ClientVersion write in `proxy/vless/outbound/outbound.go`**

Currently, `outbound.go` writes `ClientVersion` only in relay mode. The client
writes it for every request. Find the `requestAddons := &encoding.Addons{...}`
block (around line 268). After the block, add:

```go
// CUSTOM: normal-mode ClientVersion (relay mode's relayClientVersion still
// takes precedence below; this handles direct outbound from gomobile clients).
if requestAddons.ClientVersion == "" {
	if cv := encoding.GetClientVersion(); cv != "" {
		requestAddons.ClientVersion = cv
	}
}
```

Place it AFTER the existing `if relayClientVersion != "" { ... }` block so
relay mode wins. Or place it BEFORE and let relay override — either works.

**Verification after 1a+1b+1c:** `go build ./...` still passes.

### 2. Port PriorityStrategy router (PORT_AS_IS)

Reference: [research/02-priority-strategy.md](research/02-priority-strategy.md)

Zero-conflict feature. 4 files, 1 new.

**Task 2a — copy the new file**

```bash
cp /Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/app/router/strategy_priority.go \
   app/router/strategy_priority.go
```

**Task 2b — register in `app/router/config.go`**

Find the `BalancingRule.Build` function (search for `func (br *BalancingRule) Build`).
Add a new case to the strategy switch:

```go
// CUSTOM: priority strategy (deterministic first-alive pick, used by client for failover)
case "priority":
	strategy = &PriorityStrategy{observer: observer}
```

Check the exact case statements by reading the client's `config.go`.

**Task 2c — whitelist in `infra/conf/router.go`**

Find the strategy type whitelist (there's a list of valid strategy names).
Add `"priority"`.

**Task 2d — config loader in `infra/conf/router_strategy.go`**

Add a constant and an empty config loader entry for "priority" strategy.
It uses `strategyEmptyConfig` — no custom config schema needed.

**Task 2e — route change callback**

The client also has a package-global `SetRouteChangeCallback` /
`ClearRouteChangeCallback` API in `app/router/strategy_priority.go`. Check the
file — it's probably included already. This is used by the gomobile wrapper
to notify iOS UI when the active outbound changes (Direct/Enhanced indicator).

**Verification:** `go build ./...` + confirm `grep "PriorityStrategy"` finds the code.

### 3. Port Group 4 KEEP: debug.go + splithttp trio

Reference: [research/04-misc.md](research/04-misc.md)

**Task 3a — remove pprof server in `main/distro/debug/debug.go`**

Client removed the `:6060` pprof HTTP server (iOS security: don't expose a
debug server in NetworkExtension). Apply the same change. Compare:

- Client: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/main/distro/debug/debug.go`
- Unified fork: `main/distro/debug/debug.go`

Likely just removing a `net/http.ListenAndServe` call or similar.

**Task 3b — splithttp memory-reclamation (port-with-build-tags)**

**Decision made in previous session:** Port but gate `runtime.GC()` to mobile
only. Desktop builds shouldn't pay GC overhead for a problem they don't have.

Target structure:

```
transport/internet/splithttp/
├── client.go           — add CloseTransport() method (all platforms)
├── dialer.go           — add lazy 30s sweep + goroutine start (all platforms)
├── mux.go              — add CleanupIdleClients, IsEmpty, closeXmuxConn (all platforms)
├── cleanup_gc_mobile.go    — //go:build ios || android — runtime.GC() enabled
└── cleanup_gc_desktop.go   — //go:build !ios && !android — no-op
```

**Critical:** Upstream heavily refactored splithttp (dialer.go grew 504→618
lines). **Do NOT `git apply` the patch.** Port by reading the client's
intention and manually adding the hooks in the new upstream structure.

Source code to port from:
- `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/transport/internet/splithttp/client.go`
- `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/transport/internet/splithttp/dialer.go`
- `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/transport/internet/splithttp/mux.go`

Patch reference: [patches/04-misc.patch](patches/04-misc.patch)

**Verification:** `go build ./...` on desktop + verify no runtime.GC call fires
under a non-mobile build tag.

### 4. Group 4 DROP items (no action needed)

- `common/platform/platform.go` `TunFdKey` — already in upstream.
- `proxy/wireguard/tun.go` gvisor API fix — upstream rewrote this block.
  Patch is obsolete.

### 5. TUN proxy (no action needed)

Reference: [research/03-tun-proxy.md](research/03-tun-proxy.md)

Client's TUN was an older snapshot of upstream's TUN. Upstream v26.4.15's TUN
is a strict superset with identical iOS fd-injection mechanism
(`platform.TunFdKey = "xray.tun.fd"`). Zero porting work. Client gets upstream's
improvements (better UDP queue, cleaner shutdown) for free.

### 6. Build + test unified fork

```bash
cd /Users/arturdev/Developer/NovusOrdo/xray/Xray-core
go build ./...
go test ./proxy/vless/... ./app/router/... ./app/metrics/... ./app/stats/... \
        ./app/dispatcher/... ./transport/internet/splithttp/... \
        ./infra/conf/...
```

All should pass. The pre-existing geodata test failures
(`TestChinaSites`, `TestCompactDomainMatcher_PreservesMixedRuleIndices`,
`TestIPMatcher4CN`) are unrelated — they need `geosite.dat`/`geoip.dat` files.

### 7. Wire up xray_mobile_library to use the unified fork

**File:** `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xraymobile/go.mod`

Currently line 59 has a `replace` directive pointing at
`../xray-core` (the local copy). Change it to point at the unified fork:

```go
// CURRENT (to be removed):
replace github.com/xtls/xray-core => ../xray-core

// REPLACEMENT:
replace github.com/xtls/xray-core => ../../Xray-core
```

**Verify relative path correctness** before committing — double-check by
running `go mod tidy` and watching for "no Go files in..." errors.

Then build the gomobile xcframework:
```bash
cd /Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library
# whatever the existing build command is — check README.md or build-xcframework.sh
```

### 8. iOS smoke test

Before deleting the duplicate:

1. Build the xcframework against the unified fork
2. Link it into VPNTool's iOS app
3. Run VPNTool on a device
4. Verify:
   - VPN connects (Reality direct path)
   - Tower validator works (connect to a tower-backed server)
   - Failover works (PriorityStrategy picks alternate outbound)
   - Memory stays under 50MB under sustained load
   - "Auth verified" event fires (if UI displays it)
5. Optionally: test XHTTP Reality config to exercise the ported splithttp changes

### 9. Delete duplicate directory

Once iOS smoke test passes:

```bash
rm -rf /Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core
```

Verify nothing references it:
```bash
grep -r "xray_mobile_library/xray-core" /Users/arturdev/Developer/NovusOrdo/ --exclude-dir=.git
```

Commit the deletion + go.mod update.

### 10. Push and PR

Once everything is verified end-to-end:

```bash
git push origin sync-upstream-v26.4.15
```

Open a PR against `main` in the fork. Get it reviewed. Merge.

Then the parent NovusOrdo repo can start deploying the unified fork.

---

## Decisions already made (DO NOT re-debate)

- **PriorityStrategy** → PORT_AS_IS (no conflict with upstream)
- **TUN** → no work (client migrates to upstream's TUN)
- **splithttp** → port with mobile-only build tags for `runtime.GC()`
- **Obsolete items** → drop (platform.go TunFdKey, wireguard/tun.go gvisor fix)
- **Wire protocol compatibility** → verified (proto fields, XERR magic, constants match)

## Known risks

| Risk | Impact | Mitigation |
|------|--------|-----------|
| splithttp hand-port breaks upstream XMUX logic | VPN connections hang or leak | Write focused unit test OR iOS stress test before shipping |
| `SetAuthVerifiedCallback` global state | thread safety concerns | Document: must be called before xray.Start(); never change during run |
| normal-mode ClientVersion leaks version to non-tower servers | minor info leak | Acceptable — client versions aren't sensitive |
| gomobile xcframework size increase from added code | iOS binary bloat | Measure before/after — probably negligible (<100 KB) |

## If you get lost

- **Full context for a feature** → read `research/0X-<feature>.md`
- **Exact diff the client applied** → read `patches/` files
- **Overall strategy** → read `research/00-consolidated-plan.md`
- **This fork's previous sync history** → read `../../FORK-MAINTENANCE.md`
- **Server-side features already in place** → read `../research/` and `../verification/`

## Dependencies to verify on resume

Before starting work, confirm:

```bash
cd /Users/arturdev/Developer/NovusOrdo/xray/Xray-core
git log -1 --oneline          # should show 711fbfa7 or its descendant
git status                    # should be clean
go build ./...                # should exit 0
grep -rn "CUSTOM" --include="*.go" --include="*.proto" | wc -l   # should be ~47+
```

If any of these fail, something has drifted — investigate before proceeding.
