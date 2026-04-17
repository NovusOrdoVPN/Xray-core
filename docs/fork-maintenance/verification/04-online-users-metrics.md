# Feature 4 — Online Users Metrics: Verification Report

## Executive Summary

The `/online` and `/online-users` HTTP endpoints have been correctly re-implemented on top of upstream v26.4.15's refcount-based `OnlineMap`. All admin portal JSON contract fields (`asOfMs`, `direct`, `proxy`, inbound-tag keys, int64 ms values) are preserved. The refcount-vs-TTL semantic gap is real but behaviorally invisible to the admin portal provided the dispatcher always pairs `AddIP` with a matching `RemoveIP` on context cancellation — which it does via `context.AfterFunc`. Upstream's `ForEach` returns Unix **seconds**, and `buildOnlineUsersResponse` correctly multiplies by 1000 to produce milliseconds.

**Verdict:** PASS with one LOW-severity semantic caveat.

## Per-Item Verification Table

| # | Should Exist | Present? | Evidence |
|---|---|---|---|
| 1 | `/online` handler returns `{tag: count}` | YES | `app/metrics/metrics.go:174-186` |
| 2 | `/online-users` handler returns `asOfMs/direct/proxy` | YES | `app/metrics/metrics.go:187-190` |
| 3 | `buildOnlineUsersResponse` dedup by latest activity | YES | `app/metrics/metrics.go:49-103` |
| 4 | `classifyOnlineMode(tag)` — "proxy" if contains "proxy" | YES | `app/metrics/metrics.go:42-47` |
| 5 | `inboundTagFromOnlineMapName` extracts tag, not full key | YES | `app/metrics/metrics.go:34-40` |
| 6 | Unix-seconds → ms conversion (×1000) | YES | `app/metrics/metrics.go:67` |
| 7 | `userOnlineIdentity` returns VLESS UUID or email | YES | `app/dispatcher/default.go:31-39` |
| 8 | `trackInboundOnline` called from `getLink` | YES | `app/dispatcher/default.go:199` |
| 9 | `trackInboundOnline` called from `WrapLink` | YES | `app/dispatcher/default.go:236` |
| 10 | Per-user email gate + per-inbound UUID gate | YES | `app/dispatcher/default.go:175,196-200,255` |
| 11 | JSON field names `asOfMs`, `direct`, `proxy` preserved | YES | `app/metrics/metrics.go:23-27` |
| 12 | Paired `RemoveIP` on ctx done (refcount decrement) | YES | `app/dispatcher/default.go:248,261` |

## Detailed Findings

### 1. `/online` tag extraction — correct
Handler iterates via `VisitOnlineMaps`, filters `inbound>>>` prefix, calls `inboundTagFromOnlineMapName(name)` which splits on `">>>"` and returns `parts[1]`:
```go
// app/metrics/metrics.go:178-180
if tag := inboundTagFromOnlineMapName(name); tag != "" {
    resp[tag] = om.Count()
}
```
Key is the tag, not the full `inbound>>>tag>>>online` string — matches admin contract.

### 2. Timestamp conversion — CORRECT (critical check)
Upstream `OnlineMap.ForEach` emits `lastSeen int64` in Unix **seconds** (`app/stats/online_map.go:40`: `now := time.Now().Unix()`). Handler converts:
```go
// app/metrics/metrics.go:66-67
om.ForEach(func(userID string, lastSeen int64) bool {
    lastSeenMs := lastSeen * 1000
```
`asOfMs` uses `time.Now().UnixMilli()` (`app/metrics/metrics.go:90`). Admin portal contract for int64 ms values preserved.

### 3. Deduplication most-recent-wins — PRESERVED
Two-phase algorithm at `app/metrics/metrics.go:77-100`: first records per-mode maxima, then elects the mode with greatest `lastSeenMs` per user. Matches backup semantics.

### 4. Tag classification — PRESERVED
```go
// app/metrics/metrics.go:42-47
if strings.Contains(strings.ToLower(tag), "proxy") { return "proxy" }
return "direct"
```
Case-insensitive contains check identical to backup intent.

### 5. `userOnlineIdentity` — CORRECT
```go
// app/dispatcher/default.go:31-39
if account, ok := user.Account.(*vless.MemoryAccount); ok && account.ID != nil {
    return account.ID.String()
}
return user.Email
```
VLESS UUID preferred, email fallback, nil-safety on both `user` and `account.ID`.

### 6. Dispatcher hooks — BOTH call sites present
- `getLink`: `app/dispatcher/default.go:196-201` inside `if p.Stats.UserOnline`
- `WrapLink`: `app/dispatcher/default.go:233-238`, same pattern
Both call `trackOnlineIP` (per-user email map) and `trackInboundOnline` (per-inbound UUID map).

### 7. Gating logic
Outer gate `if user != nil && len(user.Email) > 0` (lines 175, 216) blocks tracking when email is missing — meaning a synthetic remote-validator user with UUID-only will NOT be tracked in the inbound online map. This matches backup behaviour (same gate existed). Inner guard `if inboundTag == "" || identity == ""` at line 255 is an additional safety net. `trackInboundOnline` correctly uses the UUID (via `userOnlineIdentity(user)`) as the per-inbound key.

### 8. RemoveIP pairing — CRITICAL for refcount semantics
`app/dispatcher/default.go:248` and `:261` register `context.AfterFunc(ctx, func() { om.RemoveIP(...) })`. When the connection's context is cancelled, refcount decrements. This is the mechanism that replaces the old TTL-based cleanup.

## Reverse-Diff Audit

Audited `app/metrics/metrics.go`, `app/dispatcher/default.go`, with cross-reference to `features/stats/stats.go`, `app/stats/stats.go`, `app/stats/online_map.go`:
- `inboundTagFromOnlineMapName`, `classifyOnlineMode`, `buildOnlineUsersResponse`, `onlineUsersResponse` struct, `onlineUserPresence` struct, `/online` + `/online-users` handlers, `expvar.Publish("online", ...)` — all present.
- `userOnlineIdentity`, `trackOnlineIP`, `trackInboundOnline`, dual call sites in `getLink`/`WrapLink`, `proxy/vless` import — all present.

No missing logic detected.

## Risk Assessment

### HIGH — none
### MEDIUM — none

### LOW — Refcount vs TTL semantic gap (behaviorally minor)
Upstream's OnlineMap is refcount-based: entries disappear the instant the last connection closes. Backup's used a 20-second TTL grace. Implications:
1. **Admin portal counts drop faster** after disconnects. Users who reconnect every <20s previously appeared "continuously online"; they now flicker. This is more accurate but may surface as chart noise.
2. **`lastSeen` reflects "most recent AddIP"**, i.e. timestamp of most recent new connection. For the admin portal's "online or not" semantics this is sufficient; any downstream code relying on per-packet freshness would be affected (none found in current scope).
3. **No stale-count fix needed** — upstream's `Count()` uses `atomic.Int64`, so the old `546bf632` bug cannot recur.

### INFO — `expvar.Publish("online", ...)` added
New expvar handler at `app/metrics/metrics.go:164-173` exposes online counts under `/debug/vars`. Not in backup; additive, no contract breakage.
