# Custom Online Users Metrics Feature — Research

## Overview

Custom `/online` HTTP endpoint in the Xray-core fork that exposes real-time counts of unique online users across VPN inbound configurations. Designed to supply a central admin portal with concurrent user metrics.

## 1. Feature Purpose & Rationale

### Why a Separate `/online` Endpoint?

The standard `/metrics` endpoint exposes detailed per-user traffic counters (`user>>>email>>>traffic>>>uplink/downlink`), which creates problematic cardinality explosion: each user accumulates separate counter entries that cannot be discarded without losing their data. This makes `/metrics` unsuitable for aggregated user-count reporting.

The `/online` endpoint solves this by:
- **Tracking only active connections**: Using TTL-based IP/UUID records that auto-expire after 20 seconds of inactivity
- **Deduplication**: Computing unique users across multiple simultaneous connections
- **No cardinality bloat**: Data is ephemeral and does not accumulate in metric storage

### Deduplication Concept

A single user may connect via multiple concurrent sessions (different IPs). The feature tracks:
- **Per-user online maps** (`user>>>email>>>online`): Count IPs used by each user
- **Global inbound online maps** (`inbound>>>tag>>>online`): Track unique user identities (UUIDs) per inbound, regardless of source IP

The deduplication algorithm (in `/online-users` endpoint) elects the *most recently active* connection mode (direct vs. proxy) for each user across all inbounds, preventing duplicate counting.

---

## 2. HTTP Endpoints

### `/online` Endpoint

**Handler Code** (`app/metrics/metrics.go`, lines 182-201):
```go
http.HandleFunc("/online", func(w http.ResponseWriter, r *http.Request) {
	manager, ok := c.statsManager.(*stats.Manager)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp := map[string]int{}
	manager.VisitOnlineMaps(func(name string, om *stats.OnlineMap) bool {
		if strings.HasPrefix(name, "inbound>>>") {
			// Extract tag from "inbound>>>tag>>>online"
			parts := strings.Split(name, ">>>")
			if len(parts) >= 2 {
				resp[parts[1]] = om.Count()
			}
		}
		return true
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
})
```

**Response Format**:
```json
{
  "inbound_tag_1": 42,
  "inbound_tag_2": 18,
  "proxy_inbound": 15
}
```

**Fields**: Map of inbound tags to current online user counts (deduplicated UUIDs per inbound).

### `/online-users` Endpoint

**Handler Code** (`app/metrics/metrics.go`, lines 202-211):
```go
http.HandleFunc("/online-users", func(w http.ResponseWriter, r *http.Request) {
	manager, ok := c.statsManager.(*stats.Manager)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildOnlineUsersResponse(manager))
})
```

**Response Format**:
```json
{
  "asOfMs": 1713361200000,
  "direct": {
    "user-uuid-1": 1713361199500,
    "user-uuid-2": 1713361198300
  },
  "proxy": {
    "user-uuid-3": 1713361198800
  }
}
```

**Fields**:
- `asOfMs`: Timestamp of response (milliseconds since epoch)
- `direct`: Map of user UUIDs last seen via direct inbound, with their last activity timestamp
- `proxy`: Map of user UUIDs last seen via proxy inbound, with their last activity timestamp

**Deduplication Logic** (`buildOnlineUsersResponse()`, lines 59-112):
1. Iterates all inbound online maps, collecting per-inbound user activity
2. Classifies each inbound as "direct" or "proxy" based on tag name (case-insensitive contains check)
3. Per user, records the most recent activity timestamp
4. Elects the connection mode with the latest timestamp as the user's current mode
5. Returns separate counts for each mode, avoiding double-counting

---

## 3. OnlineMap Implementation & Cleanup

### OnlineMap Structure

(`app/stats/online_map.go`):
```go
type OnlineMap struct {
	ipList        map[string]time.Time  // Maps IP or UUID -> last seen time
	access        sync.RWMutex
	lastCleanup   time.Time
	cleanupPeriod time.Duration         // 10 seconds
}
```

### Count() Method with Stale Data Fix

**Current implementation** (`app/stats/online_map.go`, lines 26-34):
```go
func (c *OnlineMap) Count() int {
	if time.Since(c.lastCleanup) > c.cleanupPeriod {
		c.RemoveExpiredIPs()
		c.lastCleanup = time.Now()
	}
	c.access.RLock()
	defer c.access.RUnlock()
	return len(c.ipList)
}
```

**Critical stale-count fix** (commit `546bf632`): Triggers `RemoveExpiredIPs()` *before* acquiring the read lock and counting. This ensures expired entries are flushed before returning the count, preventing stale-count bugs where a caller receives inflated numbers from entries pending cleanup.

### IpTimeMap() with Cloning

**Implementation** (`app/stats/online_map.go`, lines 81-96):
```go
func (c *OnlineMap) IpTimeMap() map[string]time.Time {
	if time.Since(c.lastCleanup) > c.cleanupPeriod {
		c.RemoveExpiredIPs()
		c.lastCleanup = time.Now()
	}

	c.access.RLock()
	defer c.access.RUnlock()

	cloned := make(map[string]time.Time, len(c.ipList))
	for k, v := range c.ipList {
		cloned[k] = v
	}

	return cloned
}
```

**Deduplication mechanism**: Returns a cloned map of all tracked IPs/UUIDs and their last-seen times. The metrics endpoint uses this to aggregate across inbounds and elect the most recent activity per user.

### TTL & Expiration Logic

**TTL**: 20 seconds (hardcoded in `RemoveExpiredIPs()`, line 75)
**Cleanup trigger**: Runs if `time.Since(lastCleanup) > cleanupPeriod` (10 seconds)

```go
func (c *OnlineMap) RemoveExpiredIPs() {
	c.access.Lock()
	defer c.access.Unlock()

	now := time.Now()
	for k, t := range c.ipList {
		diff := now.Sub(t)
		if diff.Seconds() > 20 {
			delete(c.ipList, k)
		}
	}
}
```

---

## 4. The "Stale Count" Fix (Commit 546bf632)

### The Bug

Prior to this fix, `Count()` did not trigger cleanup before returning. The sequence was:
1. Entries added via `AddIP()` and timestamps updated
2. Caller invokes `Count()` → no cleanup occurs
3. Read lock acquired, `len(c.ipList)` returns count including expired entries
4. Admin portal receives inflated concurrency numbers for stale users

### The Fix

Lines 27-30 (`app/stats/online_map.go`): Moved `RemoveExpiredIPs()` *before* the read lock:

```go
if time.Since(c.lastCleanup) > c.cleanupPeriod {
	c.RemoveExpiredIPs()      // Triggers cleanup while holding write lock
	c.lastCleanup = time.Now()
}
c.access.RLock()              // Now acquire read lock
defer c.access.RUnlock()
return len(c.ipList)          // Return only active entries
```

**Impact**: Ensures counts are always fresh, eliminating the race condition where stale entries persisted until the next `AddIP()` call (which also triggers cleanup).

---

## 5. Filtering Per-User Maps from /metrics

### The Challenge

The existing `/metrics` endpoint publishes all counters via `expvar` (lines 124-147 in `app/metrics/metrics.go`):

```go
expvar.Publish("stats", expvar.Func(func() interface{} {
	// ... iterates all counters and builds response ...
	manager.VisitCounters(func(name string, counter feature_stats.Counter) bool {
		nameSplit := strings.Split(name, ">>>")
		typeName, tagOrUser, direction := nameSplit[0], nameSplit[1], nameSplit[3]
		// Adds to resp["user"][email][direction]
	})
}))
```

This includes all `user>>>email>>>traffic>>>*` counters, which creates unbounded cardinality in metrics export.

### The Solution

The architecture relies on:
1. **Separate endpoint** (`/online`) filters by inbound tag prefix only
2. **Online maps** (not traffic counters) are the source of online-user data
3. **Per-user maps** (`user>>>email>>>online`) are registered but *not exported* via `/metrics`
4. The `VisitOnlineMaps()` method (added in `stats.go`) grants metrics handler selective access to only inbound-scoped online maps

---

## 6. Manager Interface Changes

### New Methods in `features/stats/stats.go`

**OnlineMap Interface** (lines 24-36):
```go
type OnlineMap interface {
	Count() int
	AddIP(string)
	List() []string
	IpTimeMap() map[string]time.Time
}
```

**Manager Interface additions** (lines 88-93):
```go
RegisterOnlineMap(string) (OnlineMap, error)
UnregisterOnlineMap(string) error
GetOnlineMap(string) OnlineMap
```

**Helper function** (lines 113-121):
```go
func GetOrRegisterOnlineMap(m Manager, name string) (OnlineMap, error) {
	onlineMap := m.GetOnlineMap(name)
	if onlineMap != nil {
		return onlineMap, nil
	}
	return m.RegisterOnlineMap(name)
}
```

**VisitOnlineMaps method** added to concrete Manager (`app/stats/stats.go`, lines 86-96):
```go
func (m *Manager) VisitOnlineMaps(visitor func(string, *OnlineMap) bool) {
	m.access.RLock()
	defer m.access.RUnlock()

	for name, om := range m.onlineMap {
		if !visitor(name, om) {
			break
		}
	}
}
```

---

## 7. Policy Configuration Integration

### Configuration Schema

**`infra/conf/policy.go`** (lines 7-16):
```go
type Policy struct {
	Handshake         *uint32 `json:"handshake"`
	ConnectionIdle    *uint32 `json:"connIdle"`
	UplinkOnly        *uint32 `json:"uplinkOnly"`
	DownlinkOnly      *uint32 `json:"downlinkOnly"`
	StatsUserUplink   bool    `json:"statsUserUplink"`
	StatsUserDownlink bool    `json:"statsUserDownlink"`
	StatsUserOnline   bool    `json:"statsUserOnline"`   // NEW FLAG
	BufferSize        *int32  `json:"bufferSize"`
}
```

**Config → Policy translation** (`infra/conf/policy.go`, lines 33-39):
```go
p := &policy.Policy{
	Timeout: config,
	Stats: &policy.Policy_Stats{
		UserUplink:   t.StatsUserUplink,
		UserDownlink: t.StatsUserDownlink,
		UserOnline:   t.StatsUserOnline,  // Propagated
	},
}
```

### Propagation in Dispatcher

**Usage in `app/dispatcher/default.go`** (lines 195-213, 246-264):

The dispatcher checks the policy flag when wrapping inbound and outbound links:

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
			gom.AddIP(onlineIdentity)
		}
	}
}
```

### User Identity Resolution

**`userOnlineIdentity()` function** (`app/dispatcher/default.go`, lines 31-39):
```go
func userOnlineIdentity(user *protocol.MemoryUser) string {
	if user == nil {
		return ""
	}
	if account, ok := user.Account.(*vless.MemoryAccount); ok && account.ID != nil {
		return account.ID.String()
	}
	return user.Email
}
```

**Precedence**: VLESS protocol ID (UUID) is preferred for deduplication; falls back to email if no UUID.

---

## 8. Re-Implementation Considerations for Upstream

### Breaking Changes & Conflicts

1. **`app/stats/online_map.go`**: Exists in upstream but may lack the stale-count fix. Add:
   - TTL constants (20 seconds) and cleanup period (10 seconds) 
   - Count() with cleanup-before-read-lock fix

2. **`app/stats/stats.go`**: Verify `onlineMap` field in Manager struct. Add `VisitOnlineMaps()` method. Confirm upstream's Manager structure hasn't undergone major refactoring.

3. **`features/stats/stats.go`**: Check if upstream has OnlineMap interface and Manager methods. May already exist.

4. **`app/metrics/metrics.go`**: Add new HTTP handlers for `/online` and `/online-users`, plus `buildOnlineUsersResponse()` function. Check if metrics.go has been refactored and if HTTP routes can still be added via http.HandleFunc.

5. **`infra/conf/policy.go`**: Verify `StatsUserOnline` field — likely already exists in upstream.

6. **`app/dispatcher/default.go`**: Add `userOnlineIdentity()` function and online tracking logic. This is the riskiest area: dispatcher code changes frequently. Careful merging required.

### Verification Commands

```bash
git show upstream/main:app/stats/online_map.go | head -30
git show upstream/main:app/stats/stats.go | grep -A 20 "type Manager"
git show upstream/main:features/stats/stats.go | grep -A 10 "type OnlineMap"
git show upstream/main:app/dispatcher/default.go | grep -c "userOnlineIdentity"
```

---

## 9. External Contract (Admin Portal Integration)

### Endpoint Guarantees

The admin portal consumes these endpoints. Preserve:

#### `/online` Endpoint
- **URL path**: `/online` (exactly)
- **Method**: GET
- **Content-Type**: `application/json`
- **Response structure**: `{ "inbound_tag": count, ... }` where `count` is an integer
- **Semantics**: Count per inbound, representing unique users (UUIDs) actively connected
- **No authentication**: Handler checks statsManager but doesn't verify credentials

#### `/online-users` Endpoint
- **URL path**: `/online-users` (exactly)
- **Method**: GET
- **Content-Type**: `application/json`
- **Response structure**:
  ```json
  {
    "asOfMs": <int64>,
    "direct": { "user-id": <int64>, ... },
    "proxy": { "user-id": <int64>, ... }
  }
  ```
- **Field names**: Must be exactly `asOfMs`, `direct`, `proxy` (case-sensitive)
- **Value semantics**: `asOfMs` is Unix milliseconds; user IDs and last-seen timestamps are milliseconds
- **Semantics**: Deduplicated users across all inbounds, with preferred connection mode elected per user

### Breaking Changes

Changing any of the above would break portal display. Treat as stable contract.

---

## 10. Summary of Key Design Decisions

| Aspect | Decision | Rationale |
|--------|----------|-----------|
| **Deduplication** | Track UUID (VLESS ID) as primary identity, fallback to email | Handles multi-connection users (same UUID from different IPs) |
| **Per-user maps** | Created but not exported via /metrics | Prevents cardinality explosion; online tracking is ephemeral |
| **TTL** | 20 seconds | Balance between fresh data and false disconnections due to latency |
| **Cleanup trigger** | Every 10 seconds (or on Count/IpTimeMap call) | Lightweight background cleanup without dedicated threads |
| **Mode classification** | Hardcoded "direct" vs. "proxy" via tag name contains check | Simple; allows admin portal to route users across gateway types |
| **Stale fix** | Cleanup before read lock in Count() | Guarantees fresh counts; prevents race conditions in concurrent read scenarios |
| **Identity precedence** | VLESS ID > Email | VLESS UUID is stable; email may change or not be set |

---

## Files Modified

- `app/metrics/metrics.go` — HTTP handlers for `/online` and `/online-users`
- `app/stats/online_map.go` — OnlineMap implementation with cleanup and deduplication
- `app/stats/stats.go` — Manager.VisitOnlineMaps() method and onlineMap field
- `features/stats/stats.go` — OnlineMap interface and Manager methods (interface/contract)
- `infra/conf/policy.go` — StatsUserOnline config flag
- `app/dispatcher/default.go` — userOnlineIdentity() and online tracking integration
