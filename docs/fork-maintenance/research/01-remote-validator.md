# Remote Validator + Wildcard Clients + Event Notifications — Research

## Overview

Three interconnected custom features added to the Xray-core VLESS inbound:

1. **Remote Validator**: Defers UUID validation to an external HTTP endpoint ("tower") with intelligent caching
2. **Wildcard Clients**: Allows account flow specifications to accept any compatible flow from the client
3. **Event Notifications**: Tracks online user count via global UUID deduplication metrics (implemented via dispatcher hooks, not HTTP webhooks)

---

## 1. Feature Purpose

### Remote Validator Problem Statement

The standard `MemoryValidator` only checks if a UUID exists in a hardcoded list loaded at startup. This creates friction:
- No dynamic user activation/deactivation without restarting
- No ability to enforce per-user policies (rate limiting, time-based access, device locks)
- No fine-grained error responses (why is this UUID denied?)

**Solution**: Replace memory validation with HTTP-based backend validation while maintaining sub-millisecond latency via intelligent multi-layer caching.

### Wildcard Clients & Events as Dependencies

- **Wildcard clients**: Enables relay/passthrough mode where a single VLESS UUID can accept any flow pattern
- **Online metrics**: Tracks unique connected users across all inbound sessions (not just email-based stats)

---

## 2. External API Contract — Remote Validator HTTP Endpoint

### Request

**Endpoint**: User-provided URL (via config `ValidatorEndpoint`)
**Method**: `POST`
**Content-Type**: `application/json`

**Payload** (JSON):
```json
{
  "uuid": "550e8400-e29b-41d4-a716-446655440000",
  "clientVersion": "1.2.3"
}
```

**Code reference**: `validator_remote.go:210-226`

The HTTP client has a 5-second timeout (`validator_remote.go:61`). The endpoint receives exactly the UUID string and optional client version extracted from VLESS Addons. **No authentication header is sent** — intentional per the code comment at line 228.

### Response

**Status Code**: Must be 2xx (200-299). Non-2xx treated as network error (conservative deny).

**Body** (JSON):
```json
{
  "status": 0,
  "errorCode": 0,
  "errorMessage": "",
  "decisionTtlSec": 21600,
  "heartbeatSec": 1800,
  "ttlSec": 30
}
```

**Field semantics** (`validator_remote.go:532-549`):

| Field | Meaning | Example |
|-------|---------|---------|
| `status` | 0 = allow, 1+ = deny | `0` |
| `errorCode` | Client error code (e.g. 0x01=invalid, 0x10=subscription expired) | `1` |
| `errorMessage` | Human-readable reason for denial | `"Invalid subscription"` |
| `decisionTtlSec` | How long to cache "allow" decision | `21600` (6 hours) |
| `heartbeatSec` | When to re-verify the user is still online (for device locks) | `1800` (30 min) |
| `ttlSec` | How long to cache "deny" decision | `30` (seconds) |

**Clamping rules** (to prevent tower spam):
- `decisionTtlSec`: max 24 hours (prevents stale auth)
- `heartbeatSec`: min 5 seconds (prevents re-check spam)
- `ttlSec` (deny): min 5 seconds, max 5 minutes

### Error Handling

**Tower network errors** (timeout, 4xx/5xx, JSON decode failure):
- Conservative approach: return deny with 10-second cache
- Code: `validator_remote.go:176-178, 204-205`

If the tower is unreachable, the user is denied but can retry after 10 seconds. No fallback to "allow" is made.

---

## 3. Internal Integration — Validator Selection at Init

### Config Loading

**Config struct** (`infra/conf/vless.go:138-150`):
```go
type VLessInboundConfig struct {
    Clients           []json.RawMessage
    Decryption        string
    Fallbacks         []*VLessInboundFallback
    Flow              string
    Validator         string                 // NEW: "default", "remote", or "relay"
    ValidatorEndpoint string                 // NEW: HTTP endpoint for remote validator
    Testseed          []uint32
}
```

**Proto definition** (`proxy/vless/inbound/config.proto:28-30`):
```protobuf
string validator = 8;
string validator_endpoint = 9;
```

### Init-Time Selection

**Location**: `proxy/vless/inbound/inbound.go:45-87`

The `init()` function creates a MemoryValidator for static clients, then wraps it based on the config `Validator` field:

```go
validator := new(vless.MemoryValidator)
for _, user := range c.Clients {
    u, err := user.ToMemoryUser()
    // populate MemoryValidator
}

var selectedValidator vless.Validator = validator
switch strings.ToLower(c.Validator) {
case "", "default":
    // Use memory validator as-is
case "remote":
    if c.ValidatorEndpoint == "" {
        return nil, errors.New("validatorEndpoint is required when validator is remote")
    }
    selectedValidator = newRemoteValidator(validator, c.ValidatorEndpoint)
case "relay":
    selectedValidator = &relayValidator{}
default:
    return nil, errors.New("unknown validator option: ", c.Validator)
}

return New(ctx, c, dc, selectedValidator)
```

**Key insight**: The MemoryValidator is always created as a fallback, but remote validator wraps it and provides tower-based validation.

---

## 4. Caching Strategy

### Cache Structure

**Location**: `validator_remote.go:17-55`

```go
type remoteValidator struct {
    local    *vless.MemoryValidator
    endpoint string
    client   *http.Client
    cache    sync.Map  // map[string]cachedStatus keyed by UUID.String()

    inflightMu sync.Mutex
    inflight   map[string]*inflightCall  // dedup in-flight requests
}

type cachedStatus struct {
    allowed       bool
    decisionUntil time.Time  // long validity window
    nextTouchAt   time.Time  // when to heartbeat
    denyUntil     time.Time  // short negative cache
    errorCode     int
    errorMsg      string
}
```

### Cache Lookup & Invalidation

**Validation flow** (`validator_remote.go:78-136`):

1. **Deny cached**: If `!e.allowed && now.Before(e.denyUntil)` → return `nil` (reject)
2. **Allow cached** (no touch due): If `e.allowed && now.Before(e.decisionUntil) && now.Before(e.nextTouchAt)` → return cached user
3. **Expired or touch due**: Proceed to tower call with deduplication

**Example timeline for allowed user**:
- Tower says: `decisionTtlSec=21600, heartbeatSec=1800` (6h, 30min)
- Cache stores: `decisionUntil = now + 6h, nextTouchAt = now + 30min`
- After 15 minutes: Cache hit, serve without tower call
- After 31 minutes: Touch due → re-check with tower (even within 6h window)
- After 6+ hours: Expire → full re-validation

### Request Deduplication

**Problem**: Multiple concurrent requests for same UUID hit tower simultaneously, wasting resources.

**Solution** (`validator_remote.go:165-207`):

Uses in-flight map with `sync.WaitGroup` to ensure only one tower call per UUID:
```go
if c, ok := r.inflight[uuidStr]; ok {
    r.inflightMu.Unlock()
    c.wg.Wait()  // Wait for first request to complete
    return c.allowed, c.decisionTTL, ...  // Reuse result
}

c := &inflightCall{}
c.wg.Add(1)
r.inflight[uuidStr] = c
r.inflightMu.Unlock()

// Single tower call, result shared across all waiters
```

### Cache Cleanup (Janitor)

**Location**: `validator_remote.go:301-334`

Runs every 5 minutes:
```go
for range t.C {
    r.cache.Range(func(k, v any) bool {
        e := v.(cachedStatus)
        if !e.allowed && now.After(e.denyUntil) {
            r.cache.Delete(k)
        }
        if e.allowed && now.After(e.decisionUntil) {
            r.cache.Delete(k)
        }
        return true
    })
}
```

Prevents unbounded memory growth from arbitrary UUID lookups.

---

## 5. GetWithMeta() Interface Extension

### Why Not Just Get(id)?

**Original interface** only had `Get(id uuid.UUID)`. Problem: client version from Addons isn't available when `Get()` is called during early handshake parsing.

### New Interface

**Location**: `proxy/vless/validator.go:12-22`

```go
type Validator interface {
    Get(id uuid.UUID) *protocol.MemoryUser
    // GetWithMeta validates the UUID and passes client metadata (e.g. app version)
    // to the validation backend. Falls back to Get() for validators that don't need metadata.
    GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser
    Add(u *protocol.MemoryUser) error
    // ... other methods
}
```

### Implementation

**MemoryValidator** (backward compat):
```go
func (v *MemoryValidator) GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser {
    return v.Get(id)
}
```

**RemoteValidator** (new):
```go
func (r *remoteValidator) GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser {
    // Passes clientVersion to tower in HTTP request payload
    allowed, ... := r.checkRemoteDedup(key, clientVersion)
}
```

### Call Site

**Location**: `proxy/vless/encoding/encoding.go:104-105`

Decodes addons **before** calling validator, ensuring clientVersion is available:
```go
requestAddons, err := DecodeHeaderAddons(&buffer, reader)
// ... error check ...
if request.User = validator.GetWithMeta(id, requestAddons.GetClientVersion()); request.User == nil {
    return id[:], nil, nil, false, errors.New("invalid request user id")
}
```

**Use case**: Tower can enforce minimum client version or deny old versions.

---

## 6. Wildcard Clients

### The Change

**Location**: `proxy/vless/inbound/inbound.go:606`

**Original**:
```go
if account.Flow == requestAddons.Flow {
    // Allow only exact flow match
}
```

**Modified**:
```go
if account.Flow == "" || account.Flow == requestAddons.Flow {
    // Allow if account has NO restriction (wildcard) OR exact match
}
```

### Rationale

**Use case**: Relay/passthrough mode. Server accepts any flow from client and relays it downstream, allowing flexible client-side flow choice.

**Behavior matrix**:
| account.Flow | request.Flow | Result |
|---|---|---|
| "" | "xtls" | Allow (wildcard) |
| "xtls" | "xtls" | Allow (exact) |
| "xtls" | "" | Deny |
| "" | "" | Allow |

---

## 7. Event Notifications — Online User Metrics

**Not** HTTP webhooks. Instead: global online user count via metrics.

### Where It Hooks In

**Location**: `app/dispatcher/default.go:16-24, 73-78, 123-128`

Two dispatcher methods register metrics:
1. `getLink()` — when allocating traffic handlers
2. `WrapLink()` — when transferring traffic

### Helper Function

```go
func userOnlineIdentity(user *protocol.MemoryUser) string {
    if user == nil {
        return ""
    }
    if account, ok := user.Account.(*vless.MemoryAccount); ok && account.ID != nil {
        return account.ID.String()  // UUID for remote validator
    }
    return user.Email  // Email for memory validator
}
```

### Metrics Registration

```go
if p.Stats.UserOnline {
    // Per-user online map (tracks IPs per user)
    if len(user.Email) > 0 {
        name := "user>>>" + user.Email + ">>>online"
        if om, _ := stats.GetOrRegisterOnlineMap(d.stats, name); om != nil {
            sessionInbounds := session.InboundFromContext(ctx)
            userIP := sessionInbounds.Source.Address.String()
            om.AddIP(userIP)
        }
    }

    // Global inbound online map (tracks unique UUIDs across all connections)
    if sessionInbound != nil && sessionInbound.Tag != "" && onlineIdentity != "" {
        globalName := "inbound>>>" + sessionInbound.Tag + ">>>online"
        if gom, _ := stats.GetOrRegisterOnlineMap(d.stats, globalName); gom != nil {
            gom.AddIP(onlineIdentity)  // Deduped by UUID
        }
    }
}
```

**Enables**: Query `inbound>>>vless-inbound>>>online` to get count of unique UUIDs connected to this inbound (works for relay mode where email is unavailable).

---

## 8. Config Schema

**JSON example**:
```json
{
  "inbounds": [
    {
      "protocol": "vless",
      "settings": {
        "clients": [
          {
            "id": "550e8400-e29b-41d4-a716-446655440000",
            "flow": ""
          }
        ],
        "validator": "remote",
        "validatorEndpoint": "http://tower.example.com/api/validate-uuid",
        "decryption": "none"
      }
    }
  ]
}
```

**Fields**:
- `validator` (string): `"default"` (memory), `"remote"` (HTTP), or `"relay"` (passthrough). Default: `"default"`
- `validatorEndpoint` (string): Required if `validator="remote"`
- `clients[].flow` (string): Empty = wildcard, non-empty = exact flow match required

---

## 9. Edge Cases & Subtleties

### UNCLEAR: Synthetic User Creation

Remote validator creates synthetic users for tower-approved UUIDs not in `clients[]`:

```go
func (r *remoteValidator) syntheticUser(id uuid.UUID) *protocol.MemoryUser {
    return &protocol.MemoryUser{
        Email: id.String() + "@remote",
        Account: &vless.MemoryAccount{
            ID: protocol.NewID(id),
        },
    }
}
```

**Implication**: Tower becomes the single source of truth, but users have no per-client policies unless pre-configured.

### POTENTIAL ISSUE: Error Code Propagation

**Location**: `inbound.go:332-352`

When validation fails, tries to send custom XERR error response using cached error code from tower. **Issue**: If tower times out, client gets generic `ErrInvalidUUID` (code 0x01) instead of understanding the actual problem (network timeout).

Code: `validator_remote.go:435-443`
```go
func (r *remoteValidator) GetLastError(id uuid.UUID) (code int, msg string) {
    key := id.String()
    if v, ok := r.cache.Load(key); ok {
        if e, ok := v.(cachedStatus); ok && !e.allowed {
            return e.errorCode, e.errorMsg
        }
    }
    return 0, ""
}
```

If tower error (not cached), returns `(0, "")` and defaults to `ErrInvalidUUID`.

### HTTP Timeout Behavior

5-second timeout per tower call. On timeout: deny with 10-second retry cache. No exponential backoff or queueing.

---

## 10. Re-implementation Checklist

### Must Apply

1. **Validator interface** (`proxy/vless/validator.go`): Add `GetWithMeta()` method
2. **Remote validator** (`proxy/vless/inbound/validator_remote.go`): Full new file (337 lines)
3. **Config fields** (`proxy/vless/inbound/config.proto`): Add `validator` and `validator_endpoint`
4. **Config parsing** (`infra/conf/vless.go`): Pass new fields through
5. **Init logic** (`proxy/vless/inbound/inbound.go:69-83`): Validator selection switch
6. **Wildcard flow** (`proxy/vless/inbound/inbound.go:606`): One-line change
7. **GetWithMeta call** (`proxy/vless/encoding/encoding.go:105`): Use new interface
8. **Error response** (`proxy/vless/inbound/error_response.go`): XERR protocol + error codes (covered in XERR research)

### Likely to Conflict

- **Dispatcher metrics** (`app/dispatcher/default.go`): Upstream may refactor dispatcher significantly. Test thoroughly.

### Upstream Compatibility Risks

- **MemoryValidator structure**: Assumes `*vless.MemoryAccount` type. Verify stability.
- **Dispatcher interface**: Changes to link allocation/wrapping flow. Check for method signature changes.
- **Config proto**: New fields must not conflict with reserved numbers.

---

## Summary

This feature group enables **dynamic, tower-backed UUID validation with intelligent caching**, supporting **relay passthrough mode** with **per-inbound online user metrics**. The implementation is clean and modular, with the remote validator wrapping the memory validator as a decorator. The main re-implementation risk is the dispatcher metrics change; the rest is relatively isolated.
