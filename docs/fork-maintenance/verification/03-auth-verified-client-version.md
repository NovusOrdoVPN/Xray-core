# Feature 3 — AuthVerified + ClientVersion: Verification Report

## Executive Summary

**Status: HAS ISSUES (one critical latent bug)**

Eight of nine expected changes are correctly re-applied on `sync-upstream-v26.4.15`. However, the reverse-diff audit uncovered a **missing change in `proxy/vless/encoding/addons.go`** that silently breaks the server→client `AuthVerified` signal on the wire. The field is set in the struct literal but `EncodeHeaderAddons` never actually serialises it (a zero-length addons byte is written because `Flow` is empty). This is a regression vs. the backup branch.

## Per-Item Audit Table

| # | Expected | Status | Evidence |
|---|----------|--------|----------|
| 1 | `AuthVerified=3; ClientVersion=4;` in addons.proto | PASS | `proxy/vless/encoding/addons.proto:12-13` |
| 2 | Regenerated pb.go with `GetAuthVerified`/`GetClientVersion` | PASS | `proxy/vless/encoding/addons.pb.go:30-31, 78-90` |
| 3 | DecodeHeaderAddons BEFORE validator call | PASS | `proxy/vless/encoding/encoding.go:97-108` |
| 4 | Validator uses `GetWithMeta(id, requestAddons.GetClientVersion())` | PASS | `encoding.go:106` |
| 5 | Failure return yields `id[:]` (not nil) | PASS | `encoding.go:107` |
| 6 | Error text exactly `"invalid request user id"` | PASS | `encoding.go:107` matched by `inbound/inbound.go:332` |
| 7 | Server sets `AuthVerified: true` in responseAddons literal | PARTIAL (see F1) | `inbound/inbound.go:595-598` — set but not encoded |
| 8 | `requestAddons.ClientVersion = relayClientVersion` guarded by non-empty check | PASS | `outbound/outbound.go:268-270` |
| 9 | `addons.go` encoder triggers protobuf when `AuthVerified \|\| Flow==XRV` | **FAIL** | `addons.go:17-34` still uses the old switch |

## Critical Checks

**C1 — Decode-order change present?** Yes. `encoding.go:97-108`.

**C2 — `GetWithMeta` receives the right clientVersion?** Yes, directly from the just-parsed `requestAddons` at `encoding.go:106`. Interface satisfied by `MemoryValidator.GetWithMeta` at `vless/validator.go:64-66` (stubs to `Get(id)`), `remoteValidator.GetWithMeta` at `inbound/validator_remote.go:78` (live path), and `relayValidator.GetWithMeta`.

**C3 — Failure returns `id[:]`?** Yes. `encoding.go:107`.

**C4 — Error string exact match?** Yes. `"invalid request user id"` matched by XERR hook.

**C5 — `AuthVerified: true` in responseAddons literal?** Yes at `inbound/inbound.go:595-598`. But see F1 — it never reaches the wire.

**C6 — Outbound guarded by non-empty check?** Yes. `outbound.go:265-270`.

## Detailed Findings

### F1 — CRITICAL (latent): `EncodeHeaderAddons` does not serialise `AuthVerified`

**File:** `proxy/vless/encoding/addons.go:17-34`

**Observed:**
```go
func EncodeHeaderAddons(buffer *buf.Buffer, addons *Addons) error {
    switch addons.Flow {
    case vless.XRV:
        // marshal protobuf ...
    default:
        if err := buffer.WriteByte(0); err != nil { ... }  // zero-length addons
    }
    return nil
}
```

**Expected (per backup patch hunk 1):**
```go
needsProtobuf := addons.Flow == vless.XRV || addons.AuthVerified
if needsProtobuf {
    // marshal protobuf ...
} else {
    buffer.WriteByte(0)
}
```

**Impact:** The server constructs `responseAddons = &Addons{AuthVerified: true}` with empty `Flow`. With the current encoder the `switch` hits `default` and writes a single `0x00` length byte — `AuthVerified` is dropped on the wire. Any future client that reads `responseAddons.GetAuthVerified()` will always see `false`. Today no client consumes the flag (research doc section 8), so runtime behaviour is not broken, but this is exactly the server→client signal the field was added for; it will silently fail the day a client starts gating traffic on server-confirmed auth.

**Fix:** Re-apply the four-line hunk from `patches/03-auth-verified-client-version.patch:5-24` to `proxy/vless/encoding/addons.go`.

## Risk Assessment

- **Decode-order change (flagged as highest-risk):** CORRECT.
- **`id[:]` passthrough for XERR:** CORRECT. XERR pipeline intact end-to-end.
- **Proto wire compatibility:** SAFE. Field numbers do not collide.
- **`AuthVerified` encoding bug (F1):** LATENT. No active consumer in the Swift client today, but must be fixed before any client-side `GetAuthVerified()` check ships.
- **Relay propagation:** CORRECT.

## Recommendation

Re-apply the `proxy/vless/encoding/addons.go` hunk (`needsProtobuf := addons.Flow == vless.XRV || addons.AuthVerified`). After that single fix Feature 3 passes cleanly.
