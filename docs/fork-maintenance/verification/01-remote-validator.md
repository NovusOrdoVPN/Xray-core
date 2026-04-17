# Feature 1 — Remote Validator + Wildcard + Events: Verification Report

## Executive Summary

**Status: HAS ISSUES**

One **behavioral regression** was found in `app/dispatcher/default.go`: the outer guard of both online-tracking blocks still requires `len(user.Email) > 0`, which silently disables the "global inbound online map" for users with no email — e.g. `relayValidator` synthetic users (`proxy/vless/inbound/validator_relay.go:30-35`, which returns `MemoryUser` with `Email` unset). The backup patch deliberately restructured that guard so `user != nil` was the only outer check, and email was re-tested only on the per-metric paths that actually need email. That refactor was not applied during the rebase.

Everything else on the Feature-1 SHOULD-EXIST list is present. `userOnlineIdentity`, `Validator.GetWithMeta` interface extension, `MemoryValidator.GetWithMeta` fallback, the full 337-line `remoteValidator` implementation, `VLessInboundConfig.Validator`/`ValidatorEndpoint` JSON fields, `VLessOutboundConfig.Relay` field, proto/pb.go additions, and build-time assignments are all intact.

## Per-Item Verification Table (abbreviated)

| # | SHOULD-EXIST Item | Status | Evidence |
|---|---|---|---|
| 1 | `Validator` interface has `GetWithMeta(id, clientVersion)` | PRESENT | `validator.go:14-16` |
| 2 | `MemoryValidator.GetWithMeta` delegates to `Get` | PRESENT | `validator.go:64-67` |
| 3 | `validator_remote.go` 337 lines identical to backup | PRESENT | Byte-identical |
| 4-20 | All remoteValidator internals (cache, inflight, janitor, clamps, etc.) | PRESENT | `validator_remote.go` |
| 21-25 | Proto fields + JSON config fields | PRESENT | `config.proto`, `config.pb.go`, `infra/conf/vless.go` |
| 26 | `userOnlineIdentity` helper | PRESENT | `default.go:28-39` |
| **27** | **`getLink()` outer guard should be `if user != nil`** | **DIVERGENT (regression)** | `default.go:175` still `if user != nil && len(user.Email) > 0` |
| **28** | `getLink()` global-inbound-online unreachable for empty-email | DIVERGENT | Call at `default.go:199` guarded away |
| **29** | **`WrapLink()` same refactor** | **DIVERGENT (regression)** | `default.go:216` same issue |
| **30** | `WrapLink()` global-inbound-online unreachable | DIVERGENT | Unreachable under relay validator |

## Detailed Finding A — Online-tracking outer guard regression

**Severity: BEHAVIORAL** (silently disables `inbound>>>TAG>>>online` metric for `validator: "relay"` inbounds).

**Backup** (from patches/01-remote-validator.patch):
```go
if user != nil {
    onlineIdentity := userOnlineIdentity(user)
    p := d.policy.ForLevel(user.Level)
    if len(user.Email) > 0 && p.Stats.UserUplink { ... }
    if len(user.Email) > 0 && p.Stats.UserDownlink { ... }
    if p.Stats.UserOnline {
        if len(user.Email) > 0 {
            name := "user>>>" + user.Email + ">>>online"
            // per-user map AddIP
        }
        if sessionInbound != nil && sessionInbound.Tag != "" && onlineIdentity != "" {
            globalName := "inbound>>>" + sessionInbound.Tag + ">>>online"
            // global map AddIP(onlineIdentity)
        }
    }
}
```

**Current** — `app/dispatcher/default.go:175-202`:
```go
if user != nil && len(user.Email) > 0 {    // <-- email gate blocks relay users
    p := d.policy.ForLevel(user.Level)
    if p.Stats.UserUplink { ... }
    if p.Stats.UserDownlink { ... }
    if p.Stats.UserOnline {
        trackOnlineIP(ctx, d.stats, user.Email, sessionInbound.Source.Address.String())
        if sessionInbound != nil {
            trackInboundOnline(ctx, d.stats, sessionInbound.Tag, userOnlineIdentity(user))
        }
    }
}
```

`trackInboundOnline` itself correctly short-circuits when `identity == ""`, but the outer email-gated guard blocks the whole block before that helper is reached. For `relayValidator.syntheticUser` users (no email), neither `trackOnlineIP` nor `trackInboundOnline` ever fires.

**Why it matters:** The admin portal reads `inbound>>>TAG>>>online`. For inbounds configured with `validator: "relay"`, the count will always be zero. `remoteValidator` users are unaffected because `syntheticUser` sets `Email = id.String() + "@remote"`.

**Fix:**
```go
if user != nil {
    p := d.policy.ForLevel(user.Level)
    if len(user.Email) > 0 && p.Stats.UserUplink { ... }
    if len(user.Email) > 0 && p.Stats.UserDownlink { ... }
    if p.Stats.UserOnline {
        if len(user.Email) > 0 {
            trackOnlineIP(ctx, d.stats, user.Email, sessionInbound.Source.Address.String())
        }
        if sessionInbound != nil {
            trackInboundOnline(ctx, d.stats, sessionInbound.Tag, userOnlineIdentity(user))
        }
    }
}
```
Apply the same change to `WrapLink` (lines 216-239).

## Finding B — `infra/conf/vless.go` assignment position (COSMETIC)

Backup assigned `config.Validator`/`ValidatorEndpoint` right after `config.Decryption = c.Decryption`. Current assigns them just before `return config, nil`. No behavioral difference.

## Finding C — `context.AfterFunc(..., RemoveIP)` is new (IMPROVEMENT)

`trackOnlineIP` and `trackInboundOnline` register `context.AfterFunc` callbacks that call `om.RemoveIP(...)` when the session's context is cancelled. The backup only called `AddIP`. This is required to match upstream v26.4.15's refcount-based `OnlineMap` semantics. Intentional.

## Recommendation

Fix Finding A before merging. It is a 2-line structural change per call site (remove `&& len(user.Email) > 0` from the outer guard; add `if len(user.Email) > 0 {` around the `trackOnlineIP` call). Without it, the UUID-based identity fallback for email-less users — which is the whole point of `userOnlineIdentity` — is defeated at both places where it is invoked.
