# VLESS Relay Mode + VlessRoute + RelayUUID Passthrough — Research

## 1. Feature Purpose and Architecture

The relay mode enables a **two-tier forwarding architecture** that replaces HAProxy in the Yandex Cloud relay fleet. The architecture flows as:

```
Client (with UUID X)
    |
    | (VLESS protocol)
Relay Server (no auth, country-based routing decision)
    |
    | (forwards UUID X to exit server)
Exit Server (VLESS auth verifies UUID X)
    |
    | (actual user's traffic)
Destination
```

**Why relay mode?**
- **Authentication separation**: The relay doesn't perform auth; the exit server does. The relay only routes based on country (extracted from the UUID).
- **Stateless relay**: No user database needed on relay servers, only routing rules.
- **Port-based routing**: The routing decision is embedded in the VLESS UUID itself—specifically in bytes 8-9 (group 4), which encode a port number representing the exit server cluster.

This replaces HAProxy's complexity with native VLESS protocol support.

## 2. relayValidator: Passthrough Without Authentication

**File:** `proxy/vless/inbound/validator_relay.go` (lines 1-35, entirely new)

```go
// relayValidator accepts any UUID without authentication.
// Used on relay servers where auth is deferred to the exit node.
type relayValidator struct{}

func (v *relayValidator) Get(id uuid.UUID) *protocol.MemoryUser {
    return v.syntheticUser(id)
}

func (v *relayValidator) GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser {
    return v.syntheticUser(id)
}

func (v *relayValidator) syntheticUser(id uuid.UUID) *protocol.MemoryUser {
    return &protocol.MemoryUser{
        Account: &vless.MemoryAccount{ID: protocol.NewID(id)},
        Level:   0,
    }
}
```

**How it bypasses auth:**
- Both `Get()` and `GetWithMeta()` return a synthetic user for ANY UUID—no database lookup, no rejection.
- The synthetic user wraps the UUID as-is (no zeroing bytes 6-7 like `ProcessUUID()` does).
- All other methods (`Add`, `Del`, `GetByEmail`, `GetAll`, `GetCount`) are stubs returning nil or zero values—relay servers don't manage users.

**Why this works:**
- The VLESS protocol's DecodeRequestHeader calls `validator.Get()` or `validator.GetWithMeta()` to validate.
- For relay mode, we skip that validation entirely by returning a valid user for any UUID.
- The relay then passes the original UUID to the exit server, which performs real validation.

## 3. VlessRoute Byte Range: The Critical Conflict

**Fork implementation** (`proxy/vless/inbound/inbound.go` line 580):
```go
inbound.VlessRoute = net.PortFromBytes(userSentID[8:10])
```
Uses **bytes 8-9** (group 4, the rightmost two bytes of the UUID).

**Upstream** (latest in `upstream/main`):
```go
inbound.VlessRoute = net.PortFromBytes(userSentID[6:8])
```
Uses **bytes 6-7**.

**Why the fork chose bytes 8-9:**
- **Avoids ProcessUUID() conflict**: `ProcessUUID()` in `proxy/vless/validator.go` lines 24-27 zeros bytes 6-7:
  ```go
  func ProcessUUID(id [16]byte) [16]byte {
      id[6] = 0
      id[7] = 0
      return id
  }
  ```
  If the fork used bytes 6-7, ProcessUUID would destroy the routing information before the UUID reached the exit server.

- **Relay UUID passthrough**: The relay captures the raw UUID (including bytes 6-7 intact) and forwards it to the exit. By using bytes 8-9, the routing decision is preserved even if the exit server applies ProcessUUID.

- **Exit server authentication**: If the exit server uses a configured UUID (not from relay), it can apply ProcessUUID without affecting the routing decision. If it receives a relay UUID, bytes 8-9 still encode the routing destination.

## 4. RelayUUID Passthrough

**Capture in inbound** (`proxy/vless/inbound/inbound.go` lines 581-585):
```go
if _, isRelay := h.validator.(*relayValidator); isRelay {
    if len(userSentID) == 16 {
        inbound.RelayUUID = make([]byte, 16)
        copy(inbound.RelayUUID, userSentID)
    }
    if requestAddons != nil {
        inbound.RelayClientVersion = requestAddons.GetClientVersion()
    }
}
```

**Use in outbound** (`proxy/vless/outbound/outbound.go` lines 227-244):
```go
// In relay mode, use the original client UUID and ClientVersion instead of configured ones.
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

request := &protocol.RequestHeader{
    Version: encoding.Version,
    User:    user,  // uses relayID, not configured account UUID
    Command: command,
    Address: target.Address,
    Port:    target.Port,
}
```

**Data flow:**
1. Client sends UUID X to relay (e.g., `12345678-1234-1234-8910-111213141516`).
2. Relay's relayValidator accepts it (no auth check).
3. Relay stores X in `inbound.RelayUUID`.
4. Relay outbound checks `h.relay == true` and constructs a new MemoryUser with ID = X.
5. Relay sends the request to exit server with UUID = X.
6. Exit server's validator (e.g., MemoryValidator) validates X against its own database.

## 5. RelayClientVersion Passthrough

**Capture in inbound** (`proxy/vless/inbound/inbound.go` line 587):
```go
if requestAddons != nil {
    inbound.RelayClientVersion = requestAddons.GetClientVersion()
}
```

**Use in outbound** (`proxy/vless/outbound/outbound.go` lines 259-261):
```go
if relayClientVersion != "" {
    requestAddons.ClientVersion = relayClientVersion
}
```

The client's app version (from VLESS protocol addons) is preserved and forwarded to the exit server. This allows the exit server to track which client versions are in use, even through the relay.

## 6. Session.Inbound Extensions

**File:** `common/session/session.go` (lines 49-56)

```go
// VlessRoute is the user-sent VLESS UUID's 9th<<8 | 10th bytes (group 4).
VlessRoute net.Port
// RelayUUID holds the raw 16-byte UUID sent by the client.
// Set by VLESS inbound for relay outbound passthrough.
RelayUUID []byte
// RelayClientVersion holds the client's version string from VLESS addons.
// Set by VLESS inbound for relay outbound passthrough.
RelayClientVersion string
```

**Why on Inbound (not Outbound):**
- The inbound connection receives the client's request and extracts the UUID and client version.
- The outbound handler (which runs later in the same connection's context) reads from `session.InboundFromContext(ctx)` to get the original UUID.
- Storing on Inbound means the metadata flows naturally with the session context from handler to handler.

## 7. Routing Context Integration

**File:** `features/routing/context.go` (line 44)

```go
// GetVlessRoute returns the user-sent VLESS UUID's 9th<<8 | 10th bytes (group 4), if exists.
GetVlessRoute() net.Port
```

**Router usage** (`app/router/condition.go`):
```go
case MatcherAsType_VlessRoute:
    return v.port.Contains(ctx.GetVlessRoute())
```

**How it works:**
1. After the VLESS inbound decodes the UUID, `inbound.VlessRoute` is set from bytes 8-9.
2. The routing engine queries `ctx.GetVlessRoute()` to match routing rules.
3. Rules can specify port ranges for routing (e.g., "route port 8001-8010 to Exit1, 8011-8020 to Exit2").
4. The relay's configuration then forwards different port ranges to different exit servers based on geography/load.

## 8. Outbound Config: Relay Mode Flag

**File:** `proxy/vless/outbound/config.proto` (line 13)

```proto
message Config {
  xray.common.protocol.ServerEndpoint vnext = 1;
  bool relay = 2;
}
```

**File:** `proxy/vless/outbound/outbound.go` (lines 55, 84)

```go
type Handler struct {
    // ...
    relay bool
}

// In New():
handler := &Handler{
    server:        server,
    policyManager: v.GetFeature(policy.ManagerType()).(policy.Manager),
    cone:          ctx.Value("cone").(bool),
    relay:         config.GetRelay(),  // read from proto config
}
```

**What it means:**
- If `relay: true` in the outbound config, the handler enters relay mode.
- In relay mode, the handler overrides the configured exit server UUID with `inbound.RelayUUID`.
- Without this flag, the outbound uses its configured UUID (normal operation).

## 9. Upstream Conflict Analysis

**Current situation:**
- Fork uses `userSentID[8:10]` for VlessRoute (bytes 8-9).
- Upstream uses `userSentID[6:8]` for VlessRoute (bytes 6-7).
- Fork's ProcessUUID has NOT been modified; it still zeros bytes 6-7.

**If we adopt upstream's byte convention (6-7):**
1. VlessRoute would encode routing info in bytes 6-7.
2. But ProcessUUID() zeros bytes 6-7 for authentication storage.
3. Result: **The relay would lose routing info** when the exit server applies ProcessUUID.

**If we keep fork's convention (8-9):**
1. VlessRoute encodes routing info in bytes 8-9.
2. ProcessUUID() still zeros bytes 6-7 (no conflict).
3. Result: **Routing info survives ProcessUUID**, and the exit server auth still works.

**Recommendation:**
- **Keep bytes 8-9** for VlessRoute in the rebase.
- Upstream has moved to bytes 6-7, so we must override it for relay mode to function.
- Document this in comments: "bytes 8-9 chosen to preserve routing through ProcessUUID zeroing in exit server authentication."

**IMPORTANT note about upstream's design:** Upstream zeros bytes 6-7 in `ProcessUUID` specifically BECAUSE they use those bytes for routing and want the routing bits to be invisible to the auth lookup. In their design, the same bytes serve both purposes — routing is extracted BEFORE ProcessUUID, and auth happens AFTER ProcessUUID. This is elegant.

However, our relay architecture needs the ORIGINAL UUID (all 16 bytes intact) to be forwarded to the exit server for its own auth. If we used bytes 6-7 like upstream, the relay's routing decision would still work, BUT the exit server would see the UUID with bytes 6-7 zeroed (because ProcessUUID runs before lookup) which matters if the tower auth stores the original bytes.

Actually wait — this needs careful thought. Tower auth receives the UUID as a STRING, not raw bytes. It's stored in tower's database by some key. If tower stored the zeroed version, it'd match. If tower stores the original non-zeroed UUID, our bytes 8-9 approach is needed.

**Decision point for implementation:** Check what exactly tower validates against — if tower's database stores UUIDs with bytes 6-7 already zeroed, we could adopt upstream's 6-7 convention. If tower stores original UUIDs, we must keep 8-9.

## 10. Edge Cases

### Case 1: VlessRoute is Zero (No Routing)
If the relay client sends a UUID with bytes 8-9 = 0x0000:
- `GetVlessRoute()` returns port 0.
- Routing rules won't match (assuming no rule for port 0).
- **Behavior**: Falls back to default route or rejection.
- **Fix**: Document that relay UUIDs should always encode a valid exit server port.

### Case 2: Non-relay Validator Sees the UUID
If a non-relay validator (e.g., MemoryValidator) is used and the client sends bytes 8-9 ≠ 0:
- The validator applies ProcessUUID (zeros bytes 6-7).
- Bytes 8-9 remain intact.
- **Behavior**: Normal auth works; routing info is preserved in inbound.VlessRoute.
- **No conflict**: Routing is independent of auth.

### Case 3: Tower Auth and Exact Bytes
Assume "tower auth" means a remote validator that requires exact UUID matches:
- If the exit server uses relayValidator, tower validation is skipped (relay mode).
- If the exit server uses a normal validator + tower, it validates the relayed UUID (bytes 8-9 intact).
- **No conflict**: The tower auth backend can store UUIDs with non-zero bytes 8-9.

### Case 4: Exit Server Receives Relay UUID via VLESS Protocol
The relay sends:
```
UUID = [relayed bytes 0-7] [bytes 8-9 from relay decision] [relayed bytes 10-15]
```
The exit server's VLESS decoder extracts this UUID and calls `validator.Get(UUID)`.
- If exit uses MemoryValidator: Applies ProcessUUID (zeros 6-7), then looks up the key.
- If exit uses remote validator: Sends UUID (with bytes 8-9 intact) to tower backend.
- **Flow**: The relay's routing decision (bytes 8-9) reaches the exit server's validator—correct behavior.

### Case 5: Session Context Leak
If a relay outbound is misconfigured and doesn't set `relay: true`:
- The relay override won't execute.
- The outbound will use its configured UUID, not the relayed one.
- The exit server won't see the client's UUID.
- **Mitigation**: The debug log "relay outbound: no RelayUUID" should catch this.

## 11. Re-implementation Notes for Rebase

### Highest-Risk Areas:
1. **Byte range conflict with upstream**: Upstream uses bytes 6-7. You must override to bytes 8-9 if you want ProcessUUID-safe routing. Decision: **keep bytes 8-9**.

2. **relayValidator interface compliance**: The relayValidator implements vless.Validator. If upstream's Validator interface has changed (new methods?), update relayValidator to match.

3. **Session.Inbound struct**: Check if upstream has added other fields to Inbound. Merge carefully.

4. **Config.proto field numbering**: The proto uses field 2 for `relay`. Verify upstream doesn't claim field 2 for something else.

5. **ProcessUUID stability**: Ensure ProcessUUID(id [16]byte) still zeros bytes 6-7 in upstream. If upstream changed ProcessUUID, re-examine the relay mode logic.

### Testing Strategy:
- **Unit test**: Create a relay inbound/outbound pair, send a UUID with known bytes 8-9, verify it reaches the exit.
- **Integration test**: Relay → Exit → Backend, verify routing works.
- **Tower auth test**: Ensure remote validator on exit server sees the relayed UUID with bytes 8-9 intact.

### Code Review Checklist:
- [ ] relayValidator compiles and implements vless.Validator fully.
- [ ] inbound.VlessRoute is set from bytes 8-9 (override upstream's 6-7).
- [ ] RelayUUID is 16 bytes and copied fully (no truncation).
- [ ] Outbound relay mode checks `h.relay` before overriding User.
- [ ] ProcessUUID still zeros bytes 6-7 (no upstream change).
- [ ] Router's MatcherAsType_VlessRoute uses ctx.GetVlessRoute() (unchanged).
- [ ] Proto: relay field is field 2, not claimed by upstream.
- [ ] Session.Inbound.VlessRoute comment matches byte range.

### Migration Notes:
- The patch is **self-contained**: all changes are in VLESS inbound/outbound and session.
- No changes to core routing or validator architecture, only new relayValidator.
- If upstream Validator interface changed, apply interface updates to relayValidator.
- Upstream moved VlessRoute to bytes 6-7; fork needs bytes 8-9 for ProcessUUID safety.

---

## Summary

This relay mode implementation is **architecturally sound** but depends critically on **byte range 8-9** for VlessRoute to coexist with ProcessUUID's byte zeroing (6-7). The feature is self-contained in VLESS inbound/outbound with minimal session impact. The **highest re-implementation risk** is that upstream has moved VlessRoute to bytes 6-7; we must override this to preserve our architecture. All other aspects (relayValidator, session fields, outbound config) are straightforward to integrate.
