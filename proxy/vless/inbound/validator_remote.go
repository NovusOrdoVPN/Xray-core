package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/proxy/vless"
)

// remoteValidator wraps the in-memory validator and adds remote UUID verification with caching.
type remoteValidator struct {
	local    *vless.MemoryValidator
	endpoint string
	client   *http.Client

	cache sync.Map // map[string]cachedStatus keyed by normalized UUID string

	// in-flight dedup per key
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

type inflightCall struct {
	wg      sync.WaitGroup
	allowed bool
	// cache timing outputs
	decisionTTL time.Duration
	heartbeat   time.Duration
	denyTTL     time.Duration
	err         error
	// error details from tower (when denied)
	errorCode int
	errorMsg  string
}

type cachedStatus struct {
	allowed bool
	reason  string

	// If allowed:
	decisionUntil time.Time // long validity window (e.g. up to 6h, bounded by exp)
	nextTouchAt   time.Time // when to touch tower again to keep 1h device lock alive

	// If denied:
	denyUntil time.Time // short negative cache (e.g. 30s)
	errorCode int       // error code from tower (for client error response)
	errorMsg  string    // error message from tower (for client error response)
}

func newRemoteValidator(local *vless.MemoryValidator, endpoint string) vless.Validator {
	rv := &remoteValidator{
		local:    local,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 5 * time.Second},
		inflight: make(map[string]*inflightCall),
	}

	// Prevent unbounded growth: periodically remove irrelevant/expired cache entries.
	rv.startJanitor(5 * time.Minute)

	return rv
}

func (r *remoteValidator) Add(u *protocol.MemoryUser) error { return r.local.Add(u) }
func (r *remoteValidator) Del(email string) error           { return r.local.Del(email) }

func (r *remoteValidator) Get(id uuid.UUID) *protocol.MemoryUser {
	return r.GetWithMeta(id, "")
}

func (r *remoteValidator) GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser {
	// IMPORTANT: keep UUID intact (exactly what client uses/sends in VLESS).
	key := id.String()

	now := time.Now()

	// Fast path: cache hit
	if v, ok := r.cache.Load(key); ok {
		e, ok := v.(cachedStatus)
		if !ok {
			// defensive: corrupted value -> drop
			r.cache.Delete(key)
		} else {
			// Deny cached
			if !e.allowed && now.Before(e.denyUntil) {
				return nil
			}

			// Allow cached and no touch due
			if e.allowed && now.Before(e.decisionUntil) && now.Before(e.nextTouchAt) {
				if user := r.local.Get(id); user != nil {
					return user
				}
				return r.syntheticUser(id)
			}
			// else: expired decision OR touch due -> re-check (dedup)
		}
	}

	allowed, decisionTTL, heartbeat, denyTTL, errorCode, errorMsg := r.checkRemoteDedup(key, clientVersion)

	// Update cache
	if !allowed {
		r.cache.Store(key, cachedStatus{
			allowed:   false,
			reason:    "denied",
			denyUntil: now.Add(denyTTL),
			errorCode: errorCode,
			errorMsg:  errorMsg,
		})
		return nil
	}

	// Allowed:
	// decisionUntil = now + decisionTTL
	// nextTouchAt = now + heartbeat
	r.cache.Store(key, cachedStatus{
		allowed:       true,
		reason:        "ok",
		decisionUntil: now.Add(decisionTTL),
		nextTouchAt:   now.Add(heartbeat),
		denyUntil:     time.Time{},
	})

	if user := r.local.Get(id); user != nil {
		return user
	}
	return r.syntheticUser(id)
}

func (r *remoteValidator) GetByEmail(email string) *protocol.MemoryUser {
	return r.local.GetByEmail(email)
}
func (r *remoteValidator) GetAll() []*protocol.MemoryUser { return r.local.GetAll() }
func (r *remoteValidator) GetCount() int64                { return r.local.GetCount() }

// GetLastError returns the error code and message for a denied UUID from cache.
// Returns (0, "") if the UUID is not in cache or was not denied.
func (r *remoteValidator) GetLastError(id uuid.UUID) (code int, msg string) {
	key := id.String()
	if v, ok := r.cache.Load(key); ok {
		if e, ok := v.(cachedStatus); ok && !e.allowed {
			return e.errorCode, e.errorMsg
		}
	}
	return 0, ""
}

func (r *remoteValidator) syntheticUser(id uuid.UUID) *protocol.MemoryUser {
	return &protocol.MemoryUser{
		Email: id.String() + "@remote",
		Account: &vless.MemoryAccount{
			ID: protocol.NewID(id),
		},
	}
}

// Deduplicate tower calls per uuid key.
func (r *remoteValidator) checkRemoteDedup(uuidStr string, clientVersion string) (allowed bool, decisionTTL, heartbeat, denyTTL time.Duration, errorCode int, errorMsg string) {
	// defaults (safe and low load)
	defaultDecision := 6 * time.Hour
	defaultHeartbeat := 30 * time.Minute
	defaultDeny := 30 * time.Second

	r.inflightMu.Lock()
	if c, ok := r.inflight[uuidStr]; ok {
		r.inflightMu.Unlock()
		c.wg.Wait()
		if c.err != nil {
			// On tower error: conservative deny (you can choose allow-if-previously-allowed via cache logic)
			return false, 0, 0, 10 * time.Second, 0, ""
		}
		return c.allowed, c.decisionTTL, c.heartbeat, c.denyTTL, c.errorCode, c.errorMsg
	}

	c := &inflightCall{}
	c.wg.Add(1)
	r.inflight[uuidStr] = c
	r.inflightMu.Unlock()

	defer func() {
		r.inflightMu.Lock()
		delete(r.inflight, uuidStr)
		r.inflightMu.Unlock()
		c.wg.Done()
	}()

	a, dTTL, hb, dny, errCode, errMsg, err := r.checkRemote(uuidStr, clientVersion, defaultDecision, defaultHeartbeat, defaultDeny)
	c.allowed = a
	c.decisionTTL = dTTL
	c.heartbeat = hb
	c.denyTTL = dny
	c.errorCode = errCode
	c.errorMsg = errMsg
	c.err = err

	if err != nil {
		return false, 0, 0, 10 * time.Second, 0, ""
	}
	return a, dTTL, hb, dny, errCode, errMsg
}

func (r *remoteValidator) checkRemote(uuidStr string, clientVersion string, defDecision, defHeartbeat, defDeny time.Duration) (allowed bool, decisionTTL, heartbeat, denyTTL time.Duration, errorCode int, errorMsg string, err error) {
	payload := map[string]string{"uuid": uuidStr}
	if clientVersion != "" {
		payload["clientVersion"] = clientVersion
	}
	body, err := json.Marshal(payload)
	if err != nil {
		errors.LogInfo(context.Background(), "remote validator marshal error: ", err)
		return false, 0, 0, 0, 0, "", err
	}

	req, err := http.NewRequest(http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		errors.LogInfo(context.Background(), "remote validator request build error: ", err)
		return false, 0, 0, 0, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	// Auth intentionally removed: do NOT send x-tower-key.

	resp, err := r.client.Do(req)
	if err != nil {
		errors.LogInfo(context.Background(), "remote validator http error: ", err)
		return false, 0, 0, 0, 0, "", err
	}
	defer resp.Body.Close()

	// Tower returns 200 even for deny (status=1). Treat non-2xx as error.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errors.LogInfo(context.Background(), "remote validator bad status: ", resp.StatusCode)
		return false, 0, 0, 0, 0, "", errors.New("tower bad status")
	}

	var result struct {
		Status         int    `json:"status"`       // 0 = allow
		ErrorCode      int    `json:"errorCode"`    // error code for client (when denied)
		ErrorMessage   string `json:"errorMessage"` // error message for client (when denied)
		DecisionTTLSec int    `json:"decisionTtlSec"`
		HeartbeatSec   int    `json:"heartbeatSec"`
		TTLSec         int    `json:"ttlSec"` // deny cache ttl
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		errors.LogInfo(context.Background(), "remote validator decode error: ", err)
		return false, 0, 0, 0, 0, "", err
	}

	// Use tower TTLs as-is; only apply safety clamps to avoid spamming tower too quickly.
	const (
		minHeartbeat = 5 * time.Second
		minDeny      = 5 * time.Second
		maxDecision  = 24 * time.Hour
		maxDeny      = 5 * time.Minute
	)

	// decisionTTL
	if result.DecisionTTLSec > 0 {
		decisionTTL = time.Duration(result.DecisionTTLSec) * time.Second
	} else {
		decisionTTL = defDecision
	}
	if decisionTTL > maxDecision {
		decisionTTL = maxDecision
	}

	// heartbeat
	if result.HeartbeatSec > 0 {
		heartbeat = time.Duration(result.HeartbeatSec) * time.Second
	} else {
		heartbeat = defHeartbeat
	}
	if heartbeat < minHeartbeat {
		heartbeat = minHeartbeat
	}

	// denyTTL
	if result.TTLSec > 0 {
		denyTTL = time.Duration(result.TTLSec) * time.Second
	} else {
		denyTTL = defDeny
	}
	if denyTTL < minDeny {
		denyTTL = minDeny
	}
	if denyTTL > maxDeny {
		denyTTL = maxDeny
	}

	allowed = (result.Status == 0)
	return allowed, decisionTTL, heartbeat, denyTTL, result.ErrorCode, result.ErrorMessage, nil
}

func (r *remoteValidator) startJanitor(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()

		for range t.C {
			now := time.Now()

			r.cache.Range(func(k, v any) bool {
				e, ok := v.(cachedStatus)
				if !ok {
					r.cache.Delete(k)
					return true
				}

				if !e.allowed {
					// Deny entry is only relevant until denyUntil.
					if !e.denyUntil.IsZero() && now.After(e.denyUntil) {
						r.cache.Delete(k)
					}
					return true
				}

				// Allow entry is only relevant until decisionUntil.
				if e.decisionUntil.IsZero() || now.After(e.decisionUntil) {
					r.cache.Delete(k)
					return true
				}

				return true
			})
		}
	}()
}

// Ensure remoteValidator implements Validator and the custom MetaValidator
// interface (so the type assertion in encoding.go succeeds).
var (
	_ vless.Validator     = (*remoteValidator)(nil)
	_ vless.MetaValidator = (*remoteValidator)(nil)
)
