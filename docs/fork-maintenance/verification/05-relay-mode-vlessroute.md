# Feature 5 — Relay Mode + VlessRoute + RelayUUID: Verification Report

## Executive Summary

**Verdict: PASS.** Every item from the SHOULD-EXIST list is present, correctly wired, and the critical fork-vs-upstream override (`userSentID[8:10]`) is preserved. The relay validator is a faithful `vless.Validator` implementation that never returns nil, and relay UUIDs flow from inbound to outbound without mutation or ProcessUUID zeroing. The `relay` proto field, JSON-config field, Handler field, and runtime override path are all connected end-to-end. Routing commentary in `features/routing/context.go` correctly describes bytes 9-10 (1-indexed equivalent of 8:10). No HIGH or MEDIUM severity issues found.

## Per-Item Evidence Table

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 1 | `validator_relay.go` exists, implements `vless.Validator` | PASS | `validator_relay.go:10` `var _ vless.Validator = (*relayValidator)(nil)`; Get/GetWithMeta/Add/Del/GetByEmail/GetAll/GetCount at lines 16-28 |
| 2 | `GetWithMeta` returns synthetic user for ANY UUID (never nil) | PASS | validator_relay.go:20-22 delegates to `syntheticUser(id)` which unconditionally allocates a `MemoryUser` |
| 3 | `case "relay"` in validator selection switch | PASS | `inbound.go:82-84` |
| 4 | VlessRoute byte range MUST be `userSentID[8:10]` (not upstream 6:8) | PASS | `inbound.go:578` |
| 5 | RelayUUID capture block gated on `isRelay` type-assertion | PASS | `inbound.go:579-587` |
| 6 | RelayClientVersion capture from requestAddons inside same block | PASS | `inbound.go:584-586` |
| 7 | `session.Inbound.RelayUUID []byte` + `RelayClientVersion string` | PASS | `common/session/session.go:52-57` |
| 8 | Outbound proto: `bool relay = 2;` | PASS | `config.proto:13` |
| 9 | Generated pb: `Relay bool` with `GetRelay()` accessor | PASS | `config.pb.go:30` tag `varint,2`; `config.pb.go:70-75` |
| 10 | Handler has `relay bool`, init from `config.GetRelay()` | PASS | `outbound.go:58,85` |
| 11 | Process override: uses RelayUUID when h.relay and len==16 | PASS | `outbound.go:239-252` |
| 12 | Fallback log on missing RelayUUID (no panic) | PASS | `outbound.go:250-252` |
| 13 | Override `requestAddons.ClientVersion` when relayClientVersion present | PASS | `outbound.go:268-270` |
| 14 | `infra/conf/vless.go` VLessOutboundConfig accepts `"relay": true` | PASS | `vless.go:251,379` |
| 15 | `features/routing/context.go` comment reflects bytes 8:10 | PASS | `context.go:44` |

## Detailed Findings

### HIGH-priority checks — all pass

**Byte range override preserved:** `inbound.go:578 inbound.VlessRoute = net.PortFromBytes(userSentID[8:10])`. Zero-indexed slice producing bytes at indices 8 and 9. Upstream's current convention is `userSentID[6:8]`. The override is preserved.

**ProcessUUID safety:** `proxy/vless/validator.go`'s `ProcessUUID()` still zeros bytes 6-7 for auth-storage keying. Because VlessRoute reads bytes 8-9 BEFORE any lookup, ProcessUUID's zeroing cannot destroy the routing metadata.

**UUID forwarding is byte-exact:** `outbound.go:241-248` copies all 16 bytes verbatim. `protocol.NewID(relayID)` receives the raw 16-byte array by value. **No ProcessUUID is invoked on this path** — ProcessUUID only runs inside `vless.MemoryValidator.Get()` for the exit server's own auth lookup.

**Relay flag propagation end-to-end:** JSON `"relay": true` (vless.go:251) → proto `Relay` field (vless.go:379, config.pb.go:30 tag `varint,2`) → Handler init (outbound.go:85) → runtime branch (outbound.go:239). All four links verified.

### MEDIUM-priority checks — all pass

**Fallback log behaviour:** When `h.relay` is true but inbound context is missing or RelayUUID is not 16 bytes, code logs at debug level and falls through. No panic paths. Combined guard `ib != nil && len(ib.RelayUUID) == 16` covers all bad-input cases.

**Relay validator never returns nil:** Both `Get` and `GetWithMeta` call `syntheticUser(id)`. Other Validator methods (`Add`/`Del`/`GetByEmail`/`GetAll`/`GetCount`) are intentional no-ops.

**Validator interface compliance:** The `var _ vless.Validator = (*relayValidator)(nil)` compile-time assertion at validator_relay.go:10 will catch any future upstream change to the Validator interface at build time.

## Risk Assessment

**Relay architecture integrity: GREEN.**

The most dangerous failure mode — silent UUID mutation on the relay hop — is not present. The relay path:
1. Reads raw `userSentID` from the wire (before ProcessUUID).
2. Stores it verbatim into `inbound.RelayUUID` only when the relay validator is active.
3. On the outbound side, reconstructs the exact 16 bytes into a new `MemoryUser` and forwards them.

The byte-range override (8:10) is preserved at the sole site that matters. The `relay` flag is correctly plumbed through JSON → proto → Handler → runtime branch.

**No HIGH or MEDIUM severity issues. Feature 5 is production-ready from a rebase-correctness standpoint.**

Recommended follow-ups (outside audit scope):
- Integration test: relay/exit pair with a UUID that has distinctive bytes 8-9, assert the exit sees the identical 16 bytes.
- Unit test asserting `relayValidator.Get(anyUUID) != nil` to lock the never-nil invariant.
