# AuthVerified and ClientVersion Feature — Research

## 1. Feature Purpose

### AuthVerified Field
The `AuthVerified` boolean field serves as a server→client signal confirming successful authentication. Its purpose is critical to distinguishing between:
- **Pre-auth protocol handshake**: The initial version/UUID exchange before validation
- **Authenticated state**: The server's confirmation that the remote validator has approved the client's UUID

In the VLESS fork with remote validator support, the validator is asynchronous—it may defer decisions or require network round-trips. The `AuthVerified` flag ensures the client knows the server has completed validation before the client commits to processing any data payloads. This is essential when using a remote tower/validator backend (`proxy/vless/inbound/validator_remote.go`), where authentication decisions are made via HTTP to an external service.

### ClientVersion Field
The `ClientVersion` string field transmits the client application's version to the server, enabling:
- **Version enforcement**: Tower can reject outdated or incompatible clients
- **Feature gating**: The server can conditionally enable/disable features based on client app version
- **Analytics**: Tower backend can track version distribution and enforce upgrade policies

This field flows from client to server in the request addons, and the server passes it to the remote validator (`checkRemote` function) so that tower/authentication backend can make decisions informed by the client's app version.

---

## 2. Proto Schema Definition

**File:** `proxy/vless/encoding/addons.proto`

```protobuf
message Addons {
  string Flow = 1;
  bytes Seed = 2;
  bool AuthVerified = 3;
  string ClientVersion = 4;
}
```

**Field Definitions:**
- **AuthVerified** (field number 3, type: `varint`/bool)
  - Protobuf type code: `0x08` (wire type 0 = varint)
  - Default: `false`
  - Direction: Server→Client (in response addons)

- **ClientVersion** (field number 4, type: `bytes`/string)
  - Protobuf type code: `0x22` (wire type 2 = length-delimited)
  - Default: empty string `""`
  - Direction: Client→Server (in request addons)

---

## 3. Wire Format & Backward Compatibility

Protobuf 3 is inherently backward-compatible via field numbering and wire-type semantics:

**Old Client (no ClientVersion) → New Server:**
- Client sends addons without field 4
- Server's `requestAddons.GetClientVersion()` returns empty string `""` (proto3 default)
- Remote validator receives `clientVersion=""` in the HTTP POST payload
- Tower can handle empty/missing version (e.g., treat as "legacy" or deny)

**New Client (with ClientVersion) → Old Server:**
- Client sends addons with field 4 (ClientVersion string)
- Old server code (pre-fork) calls `validator.Get(id)` without metadata
- If old server's code doesn't unmarshal addons, it won't see ClientVersion at all

**New Client (AuthVerified=true) → Old Client (decoding response):**
- Server sends response addons with field 3 (AuthVerified = true)
- Old client unmarshals the addons protobuf
- Since field 3 is optional, old client's `GetAuthVerified()` returns `false` (default)

**Key insight:** Protobuf's field-based design means old and new versions coexist peacefully. However, **semantic compatibility depends on the application layer**.

---

## 4. Server-Side: Setting AuthVerified

**File:** `proxy/vless/inbound/inbound.go`, lines 597-600

```go
responseAddons := &encoding.Addons{
    // Flow: requestAddons.Flow,
    AuthVerified: true,
}
```

**Context:** Set unconditionally to `true` after the request header has been decoded. Placement after account access, before processing the request body, signals: "UUID is valid, authentication is verified, safe to process data".

The comment `// Flow: requestAddons.Flow,` being commented out suggests the server intentionally does NOT echo back the client's requested flow — only confirms auth success.

---

## 5. Server-Side: Reading ClientVersion

**File:** `proxy/vless/inbound/validator_remote.go`, lines 107-136

```go
allowed, decisionTTL, heartbeat, denyTTL, errorCode, errorMsg := r.checkRemoteDedup(key, clientVersion)
```

The `clientVersion` is extracted from requestAddons in `encoding.go:99`:
```go
if request.User = validator.GetWithMeta(id, requestAddons.GetClientVersion()); request.User == nil {
    return id[:], nil, nil, false, errors.New("invalid request user id")
}
```

**Where it flows:**
1. **`checkRemoteDedup(uuidStr, clientVersion)`** — Deduplicates concurrent tower requests for the same UUID
2. **`checkRemote(uuidStr, clientVersion, ...)`** — Builds HTTP JSON payload:
   ```go
   payload := map[string]string{"uuid": uuidStr}
   if clientVersion != "" {
       payload["clientVersion"] = clientVersion
   }
   body, err := json.Marshal(payload)
   ```
   Posts to tower endpoint with `{"uuid": "...", "clientVersion": "..."}` (only if non-empty).

**Key insight:** ClientVersion is NOT stored locally. It is extracted from request addons, passed to remote validator for auth decisions, and optionally passed to relay outbound.

---

## 6. Client-Side: Setting ClientVersion

**File:** `proxy/vless/outbound/outbound.go`

**Relay mode usage (lines ~515-525):**
```go
user := rec.User
var relayClientVersion string
if h.relay {
    ib := session.InboundFromContext(ctx)
    if ib != nil && len(ib.RelayUUID) == 16 {
        var relayID [16]byte
        copy(relayID[:], ib.RelayUUID)
        account := rec.User.Account.(*vless.MemoryAccount)
        user = &protocol.MemoryUser{
            Account: &vless.MemoryAccount{ID: protocol.NewID(relayID), Flow: account.Flow},
            Level:   rec.User.Level,
        }
        relayClientVersion = ib.RelayClientVersion
    } else {
        errors.LogDebug(ctx, "relay outbound: no RelayUUID in inbound context — falling back to configured UUID (possible misconfig or non-VLESS inbound)")
    }
}
```

**Building request addons:**
```go
requestAddons := &encoding.Addons{
    Flow: account.Flow,
}
if relayClientVersion != "" {
    requestAddons.ClientVersion = relayClientVersion
}
```

**Important:** In the fork's current implementation:
- **Regular (non-relay) outbound clients do NOT set ClientVersion** in the request addons
- Only **relay mode** clients propagate the original ClientVersion from inbound context
- This aligns with the "relay proxy" feature: forward the original client's version string upstream

---

## 7. CRITICAL: Decoding Order Change in encoding.go

**File:** `proxy/vless/encoding/encoding.go`, lines 93-107

**FORK VERSION (new order):**
```go
if isfb {
    first.Advance(17)
}

// Decode addons BEFORE validator.Get() so ClientVersion is available
// for the very first tower validation call.
requestAddons, err := DecodeHeaderAddons(&buffer, reader)
if err != nil {
    return nil, nil, nil, false, errors.New("failed to decode request header addons").Base(err)
}

// Validate UUID with tower — clientVersion from addons is now available.
if request.User = validator.GetWithMeta(id, requestAddons.GetClientVersion()); request.User == nil {
    return id[:], nil, nil, false, errors.New("invalid request user id")
}
```

**UPSTREAM ORIGINAL (pre-fork):**
```go
if request.User = validator.Get(id); request.User == nil {
    return nil, nil, nil, isfb, errors.New("invalid request user id")
}

if isfb {
    first.Advance(17)
}

requestAddons, err := DecodeHeaderAddons(&buffer, reader)
if err != nil {
    return nil, nil, nil, false, errors.New("failed to decode request header addons").Base(err)
}
```

**Why the Order Matters (Critical):**

1. **Upstream (validator.Get FIRST):**
   - Call validator WITHOUT knowing the client's version
   - Then decode addons
   - **Problem for remote validator:** Tower makes decisions blind to ClientVersion; feature gating and version enforcement impossible

2. **Fork (DecodeHeaderAddons FIRST):**
   - Parse addons (including ClientVersion) immediately after UUID
   - Call `validator.GetWithMeta(id, clientVersion)` with full context

**Byte-level order in wire format:**
```
VLESS Request Header Structure:
┌─────┬──────────┬──────────┬──────────┬─────────┬─────────┐
│ Ver │    ID    │  Addons  │ Command  │ Address │  Port   │
│ (1) │   (16)   │  (var)   │   (1)    │  (var)  │  (2)    │
└─────┴──────────┴──────────┴──────────┴─────────┴─────────┘
```

**Impact on re-implementation:**
- The order is **NOT optional** — it's a semantic requirement for tower integration
- Upstream's current code likely still has addons decoded AFTER validator.Get()
- When porting to newer upstream, this order change MUST be applied BEFORE any validator call
- Check for recent VLESS changes; upstream may have refactored encoding.go significantly

---

## 8. AuthVerified Usage on Client-Side

**Finding:** No client-side code currently consumes or validates the `AuthVerified` field in response addons.

The field is decoded by `DecodeResponseHeader()` but the client code never accesses `responseAddons.GetAuthVerified()`. The field is **present for future use**:
- Clients simply ignore it (proto3 default behavior)
- No current security depends on it yet

---

## 9. Re-Implementation Notes & Upstream Compatibility

### A. Proto Regeneration (Build Step)

**Critical:** `addons.pb.go` is generated. Re-implementation checklist:
1. Modify `addons.proto` (add field 3 and 4)
2. Run protoc:
   ```bash
   protoc --go_out=. proxy/vless/encoding/addons.proto
   ```
3. **DO NOT manually edit addons.pb.go**

### B. Upstream's encoding.go Changes

Likely conflict areas:
1. **DecodeHeaderAddons triggering condition** in `addons.go:11-12`:
   - Fork: `needsProtobuf := addons.Flow == vless.XRV || addons.AuthVerified`
   - Upstream may have different XRV flow handling
2. **Validator interface changes:**
   - Fork adds `GetWithMeta(id, clientVersion)` to Validator interface
   - MemoryValidator stubs it as: `return v.Get(id)` (ignores metadata)
3. **Response header construction:**
   - Fork: `responseAddons.AuthVerified = true` is hardcoded after auth

### C. Session Context for Relay

The fork adds `RelayClientVersion string` field to `common/session/session.go` (part of Inbound struct). Used to propagate the client version through relay outbound.

---

## 10. Summary Table

| Aspect | Field | Direction | Type | Default | Usage |
|--------|-------|-----------|------|---------|-------|
| **Proto Field 3** | AuthVerified | Server→Client | bool (varint) | false | Server signals auth verified after remote validator approves UUID |
| **Proto Field 4** | ClientVersion | Client→Server | string (bytes) | "" | Client reports app version; server passes to tower for version enforcement & analytics |
| **Encoding Trigger** | AuthVerified in EncodeHeaderAddons | Response | – | Triggers protobuf encoding if `AuthVerified \|\| Flow==XRV` | Ensures auth signal sent even if no Flow |
| **Decoding Order** | DecodeRequestHeader | Request | – | – | **CRITICAL:** addons decoded BEFORE validator.GetWithMeta() |
| **Remote Validator** | GetWithMeta(id, clientVersion) | Internal | – | – | Tower receives ClientVersion in HTTP POST payload |
| **Relay Outbound** | RelayClientVersion | Context | string | "" | Passes original client's version upstream if relay mode enabled |
| **Client-side Handling** | AuthVerified in response | Response | – | Ignored | Present for future use |
