# Group 1 — VLESS Overlap (XERR, AuthVerified, ClientVersion)

## Executive Summary

**Verdict: NEEDS_MERGE.**

Wire protocol surface (proto fields, magic bytes, severity codes, XERR parser, DecodeResponseHeader branch) is **byte-identical** between the client fork and the server fork at `/Users/arturdev/Developer/NovusOrdo/xray/Xray-core/` (branch `sync-upstream-v26.4.15`). However, the unified fork is **missing three client-side pieces** that the iOS gomobile wrapper (`xray_mobile_library/vpntoolcore/xray.go:79-88`) depends on:

1. `encoding.SetClientVersion` / `GetClientVersion` package-level state (client-only API).
2. `encoding.SetAuthVerifiedCallback` / `ResetAuthVerifiedCallback` / `fireAuthVerified` callback machinery.
3. The **unconditional** write of `ClientVersion` in `outbound.go` for normal (non-relay) VLESS outbound.

Dropping these in favor of the server fork as-is would silently break: (a) `[xray] AUTH VERIFIED` event → `eventDispatcher.reportAuthSuccess()` in the iOS app, (b) any tower-side clientVersion enforcement/metrics tied to individual iOS sessions (the server fork only forwards ClientVersion when in relay mode — normal client paths would always send `""`).

## Per-File Verdict

| File | Server fork has it? | Semantics identical? | Client-specific gap |
|---|---|---|---|
| `proxy/vless/encoding/addons.proto` | Yes | Yes — fields 3 (AuthVerified bool) & 4 (ClientVersion string) match | None |
| `proxy/vless/encoding/addons.pb.go` | Yes (regenerated for protoc v7.34.1 vs client's v5.29.3) | Yes — same wire tags `varint,3` and `bytes,4`, same `GetAuthVerified()`/`GetClientVersion()` | None (different codegen vintage is fine — proto wire format invariant) |
| `proxy/vless/encoding/encoding.go` | Partial | XERR detection block is byte-identical | **MISSING:** callback machinery (`authCallbackMu/once/authCallback`, `SetAuthVerifiedCallback`, `ResetAuthVerifiedCallback`, `fireAuthVerified`), `SetClientVersion/GetClientVersion`, and the `if responseAddons.AuthVerified { fireAuthVerified() }` call after `DecodeHeaderAddons` in `DecodeResponseHeader` |
| `proxy/vless/encoding/error_response.go` | Yes | **Byte-identical** to client (magic `"XERR"`, `SeverityCritical=0x01`, `SeverityWarning=0x02`, `SeverityInfo=0x03`, same `TryParseServerError` logic and frame layout `[Magic:4][Version:1][Severity:1][Code:1][MsgLen:2][Message]`) | None |
| `proxy/vless/outbound/outbound.go` | Partial | Only writes `ClientVersion` in **relay mode** (gated by `if h.relay && …`) | **MISSING normal-mode write.** Client has `ClientVersion: encoding.GetClientVersion()` in every `requestAddons`, unconditionally |

## Detailed Findings

### HIGH — Normal-mode ClientVersion not written by server fork

Client patch (`01-vless-overlap.patch:285-294`, applied to `proxy/vless/outbound/outbound.go`):

```go
requestAddons := &encoding.Addons{
    Flow:          account.Flow,
    ClientVersion: encoding.GetClientVersion(),
}
```

Server fork (`Xray-core/proxy/vless/outbound/outbound.go:261-277`):

```go
request := &protocol.RequestHeader{ ... }
account := request.User.Account.(*vless.MemoryAccount)

requestAddons := &encoding.Addons{
    Flow: account.Flow,
}
// CUSTOM: propagate original client's version to exit server in relay mode.
if relayClientVersion != "" {
    requestAddons.ClientVersion = relayClientVersion
}
```

Impact: if we point the iOS app at a unified-fork XCFramework without change, every outbound VLESS request sends `ClientVersion=""`. The remote validator (`inbound/validator_remote.go:212-214`) only injects `clientVersion` into the tower HTTP payload when non-empty, so the tower loses per-session client-version telemetry and any enforcement that depends on it (see `docs/fork-maintenance/research/03-auth-verified-client-version.md`).

Merge path: extend the CUSTOM relay-mode block so that when `relayClientVersion == ""` (normal mode) it falls back to `encoding.GetClientVersion()`.

### HIGH — AuthVerified callback + package-level client version missing

Client patch (`01-vless-overlap.patch:101-151` in `encoding.go`) introduces package-level globals plus three exported functions used directly by the gomobile wrapper. Verified consumer: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/vpntoolcore/xray.go:79-88`:

```go
if v := getClientVersion(); v != "" {
    encoding.SetClientVersion(v)
}
encoding.SetAuthVerifiedCallback(func() {
    logMessage("[xray] AUTH VERIFIED — server confirmed authentication")
    eventDispatcher.reportAuthSuccess()
})
```

Server fork's `encoding.go` does **not** define any of these symbols (grep confirmed — only docs/diffs mention them). If we swap the XCFramework to the unified fork, the wrapper will fail to compile (`undefined: encoding.SetClientVersion`, `encoding.SetAuthVerifiedCallback`).

Additionally, server fork's `DecodeResponseHeader` (`encoding.go:166-203`) decodes the response addons but never fires a callback. The client's version calls `fireAuthVerified()` immediately after decode when `responseAddons.AuthVerified` is true. Without this, the iOS "auth success" UI signal will never trigger.

Merge path: port these client-only APIs into the unified fork (they don't conflict with server-side behavior; server never calls them).

### MEDIUM — Client does NOT patch `addons.go::EncodeHeaderAddons`

The server fork patched `EncodeHeaderAddons` so it emits the protobuf payload whenever `AuthVerified || ClientVersion != ""` (see `Xray-core/proxy/vless/encoding/addons.go:17-43`). The client does **not** have this change — patch file is empty for `addons.go`. Upstream behavior: only serialize when `Flow == XRV`.

In the current iOS stack this happens to be benign — Reality + XTLS-Vision sets `Flow=XRV`, so the encoder fires and ClientVersion is written on the wire. But if/when the client ever uses a non-XRV flow (e.g., a hypothetical WebSocket outbound with no flow, or raw VLESS+TLS), ClientVersion would be silently dropped by the client encoder even though it sets the field.

Merge path: adopting the unified fork's `addons.go` as-is on the client is a strict improvement — no regression risk for the current Reality+XRV path, plus forward-compat for any non-XRV transport.

### LOW — XERR block and `error_response.go` are byte-identical

Confirmed by direct comparison: magic string, severity constants (0x01/0x02/0x03), frame layout, `TryParseServerError` behavior, and the `if firstByte == 'X'` branch inside `DecodeResponseHeader` match across both forks. Safe to replace.

## Risk Assessment

- **Wire compat:** LOW. Proto field numbers, wire tags, XERR frame, severity codes all align.
- **Compile-time breakage risk on client replacement:** HIGH — `vpntoolcore/xray.go` references `encoding.SetClientVersion` / `encoding.SetAuthVerifiedCallback` which do not exist in the unified fork.
- **Silent telemetry loss:** HIGH — unified fork drops `ClientVersion` in normal (non-relay) outbound, so tower sees `clientVersion=""` for every regular iOS session.
- **Silent UI regression:** MEDIUM — auth-verified event stops firing, so the iOS app's "server confirmed authentication" signal is lost.

## Required Merges Into Server Fork

1. Port `SetClientVersion`, `GetClientVersion`, `SetAuthVerifiedCallback`, `ResetAuthVerifiedCallback`, `fireAuthVerified` (plus the two `sync.Mutex` + `sync.Once` globals) from client's `encoding.go:21-71` into `Xray-core/proxy/vless/encoding/encoding.go`.
2. Add `if responseAddons.AuthVerified { fireAuthVerified() }` immediately after the successful `DecodeHeaderAddons` call in `DecodeResponseHeader` (`Xray-core/proxy/vless/encoding/encoding.go:197-200`).
3. In `Xray-core/proxy/vless/outbound/outbound.go:271-277`, extend the relay-mode block so when `relayClientVersion == ""` the code falls back to `encoding.GetClientVersion()` for normal-mode requests.

No changes needed to `addons.proto`, `addons.pb.go`, `error_response.go`, or the server-side `addons.go` encoder — these are already correct in the unified fork.
