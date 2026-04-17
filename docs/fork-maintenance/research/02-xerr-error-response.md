# XERR Custom Error Response Feature — Research

## 1. Feature Purpose

XERR is a custom error response protocol that allows the VLESS server to communicate structured error information to clients before closing the connection. Without this feature, clients connecting with invalid, expired, or revoked UUIDs see only a connection drop with no indication of the underlying cause. XERR solves this UX problem by sending a well-defined error response that clients can parse, interpret, and display to users with specific context (e.g., "Your subscription expired" vs. "Account banned" vs. "Unknown UUID").

The feature is implemented as a layered protocol addition: when the server detects certain error conditions during request validation, it sends a 4-byte "XERR" magic prefix followed by structured error metadata before disconnecting.

---

## 2. Wire Protocol: Exact Byte Format and Magic

**Format (both client and server):**
```
[Magic:4 bytes][Version:1][Severity:1][Code:1][MsgLen:2][Message:variable]
```

**Magic bytes:** The literal ASCII string `"XERR"` (0x58 0x45 0x52 0x52 in hex).

**Breakdown of fixed fields:**
- **Magic (4 bytes):** `"XERR"` — triggers XERR parsing path instead of normal VLESS response processing
- **Version (1 byte):** Currently `0x01` (defined as `ErrorVersion` in `inbound/error_response.go:16`)
- **Severity (1 byte):** One of three levels (see section 5)
- **Code (1 byte):** Error code identifying the specific error type (see section 5)
- **MsgLen (2 bytes):** Big-endian uint16 specifying message length (0–65535 bytes)
- **Message (variable):** UTF-8 text message (max 65535 bytes due to uint16 length field)

**Total fixed overhead:** 9 bytes. Minimum XERR packet = 9 bytes (empty message).

**Backwards compatibility:** Yes, with caveats. Old clients expecting VLESS response version 0x00 as the first byte will see 'X' (0x58) and fail with "unexpected response version" — they will not crash, but will disconnect with an error. New clients that implement XERR detection gracefully degrade if the magic check fails and no error is actually present.

---

## 3. Server Side: `inbound/error_response.go`

**SendErrorResponse function** (`inbound/error_response.go:51–81`):

```go
func SendErrorResponse(conn net.Conn, severity byte, code byte, msg string) error {
	// Limit message length to prevent overflow (max 65535 bytes due to uint16)
	if len(msg) > 65535 {
		msg = msg[:65535]
	}

	// Build the error response
	// [Magic:4][Version:1][Severity:1][Code:1][MsgLen:2][Message:variable]
	buf := make([]byte, 4+1+1+1+2+len(msg))

	// Magic bytes "XERR"
	copy(buf[0:4], ErrorMagic)
	buf[4] = ErrorVersion
	buf[5] = severity
	buf[6] = code
	binary.BigEndian.PutUint16(buf[7:9], uint16(len(msg)))
	copy(buf[9:], msg)

	_, err := conn.Write(buf)
	return err
}
```

**When is it triggered?**

In `inbound/inbound.go:332–351`, XERR is sent when:
1. `DecodeRequestHeader()` returns an error containing the exact string `"invalid request user id"`
2. The configured validator is a `remoteValidator` (not the local memory validator)
3. The raw UUID bytes were successfully extracted and can be looked up in the validator's error cache

**Code flow** (`inbound/inbound.go:332–351`):

```go
if err != nil && strings.Contains(err.Error(), "invalid request user id") {
	if remoteVal, ok := h.validator.(*remoteValidator); ok {
		if len(userSentID) == 16 {
			var id uuid.UUID
			copy(id[:], userSentID)
			code, msg := remoteVal.GetLastError(id)
			// Use default error if tower didn't provide specific details
			if code == 0 {
				code = ErrInvalidUUID
				msg = "Invalid or unknown user ID"
			}
			severity := GetSeverityForCode(code)
			if sendErr := SendErrorResponse(connection, severity, byte(code), msg); sendErr != nil {
				errors.LogWarningInner(ctx, sendErr, "failed to send XERR error response")
			} else {
				errors.LogInfo(ctx, "sent XERR error response: code=", code, " msg=", msg)
			}
		}
	}
}
```

Key observations:
- `DecodeRequestHeader()` returns the UUID as `userSentID` even when validation fails (first return value).
- The inbound handler uses string matching (`strings.Contains`) to detect XERR-triggering errors.
- Falls back to `ErrInvalidUUID` (0x01) with a generic message if the remote validator has no cached error details.

---

## 4. Client Side: `encoding/error_response.go`

**TryParseServerError function** (`encoding/error_response.go:47–94`):

```go
func TryParseServerError(reader io.Reader, firstByte byte) (*ServerError, error) {
	if firstByte != 'X' {
		return nil, nil // Not an error response
	}

	magicRest := make([]byte, 3)
	if _, err := io.ReadFull(reader, magicRest); err != nil {
		return nil, nil // Not enough data, probably not XERR
	}

	if string(append([]byte{firstByte}, magicRest...)) != ErrorMagic {
		return nil, nil // Not an error response
	}

	// Parse: [Version:1][Severity:1][Code:1][MsgLen:2][Message:variable]
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, fmt.Errorf("failed to read error response header: %w", err)
	}

	severity := header[1]
	code := header[2]
	msgLen := binary.BigEndian.Uint16(header[3:5])

	msg := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(reader, msg); err != nil {
			return nil, fmt.Errorf("failed to read error message: %w", err)
		}
	}

	return &ServerError{Severity: severity, Code: code, Message: string(msg)}, nil
}
```

**Graceful fallback:** If `firstByte != 'X'`, or magic bytes don't match, or the magic-suffix read fails with EOF, the function returns `(nil, nil)` — telling the caller "this is not XERR; continue normally." Only after the full magic is confirmed does it return actual errors.

**Used in DecodeResponseHeader** (`encoding/encoding.go:166–178`):

```go
if firstByte == 'X' {
	serverErr, parseErr := TryParseServerError(reader, firstByte)
	if parseErr != nil {
		return nil, errors.New("failed to parse server error response").Base(parseErr)
	}
	if serverErr != nil {
		return nil, serverErr  // ServerError implements error interface
	}
	// If nil, nil - not actually XERR; fall through to version check
}

if firstByte != request.Version {
	return nil, errors.New("unexpected response version. Expecting ", int(request.Version), " but actually ", int(firstByte))
}
```

---

## 5. Error Codes and Severity Levels

**Severity levels** (`inbound/error_response.go:18–21`):

```go
const (
	SeverityCritical = 0x01 // Client should disconnect immediately
	SeverityWarning  = 0x02 // Client may retry or show warning
	SeverityInfo     = 0x03 // Informational only
)
```

**Error codes** (`inbound/error_response.go:24–33`):

```go
const (
	ErrInvalidUUID      = 0x01 // Invalid or unknown UUID
	ErrExpiredUUID      = 0x02 // UUID has expired
	ErrRevokedUUID      = 0x03 // UUID has been revoked/banned
	ErrRateLimited      = 0x04 // Too many requests
	ErrServerBusy       = 0x05 // Server is busy
	ErrSubscriptionExp  = 0x10 // Subscription expired
	ErrAccountSuspended = 0x11 // Account suspended
	ErrServerMessage    = 0xFE // Server info message
	ErrCustom           = 0xFF // Custom error (see message)
)
```

**Severity mapping** (`inbound/error_response.go:36–47`):

```go
func GetSeverityForCode(code int) byte {
	switch code {
	case ErrInvalidUUID, ErrExpiredUUID, ErrRevokedUUID, ErrSubscriptionExp, ErrAccountSuspended:
		return SeverityCritical
	case ErrRateLimited, ErrServerBusy:
		return SeverityWarning
	case ErrServerMessage:
		return SeverityInfo
	default:
		return SeverityCritical // Default to critical for unknown codes
	}
}
```

Semantics: Critical = permanent denial; Warning = transient issue; Info = informational.

---

## 6. Integration with Remote Validator

The remote validator maintains an in-memory cache of UUID decisions. When a UUID is denied, the cache stores error details.

**GetLastError method** (`validator_remote.go:146–153`):

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

The cache stores `errorCode` and `errorMsg` from the tower's denial response. The inbound handler retrieves these via `GetLastError()` to populate XERR responses.

---

## 7. Encoding.go Changes

The `DecodeResponseHeader` function was modified to check for 'X' (0x58) **before** the normal version check (lines 166–178 in current `encoding.go`). This is safe for upstream integration because:
- The check is explicit to 'X' only
- Future version bytes won't be 'X'
- The fallthrough behavior is correct if the magic doesn't match

---

## 8. Edge Cases

1. **Incomplete magic (EOF on suffix read):** Returns `(nil, nil)` → falls through to version check → diagnostic error.
2. **Truncated header/message after magic confirmed:** Returns actual error (e.g., "failed to read error response header").
3. **Spurious 'X' that's not XERR:** Returns `(nil, nil)` → version check fails with "unexpected response version. Expecting 0 but actually 88".
4. **Empty message:** Handled correctly; `msgLen=0` → no message bytes read → valid ServerError with empty string.
5. **Unknown severity/code bytes:** Not validated on client; allows forward compatibility.

---

## 9. Summary for Re-implementation

**Files to create:**
- `proxy/vless/encoding/error_response.go` (88 lines) — client-side parsing
- `proxy/vless/inbound/error_response.go` (81 lines) — server-side sending

**Files to modify:**
- `proxy/vless/encoding/encoding.go` — add 'X' check in `DecodeResponseHeader` before version check (lines 166–178)
- `proxy/vless/inbound/inbound.go` — add XERR trigger after `DecodeRequestHeader()` fails (lines 332–351)
- Ensure `validator_remote.go` has `GetLastError()` method

**Test for:** Backwards compatibility with old clients, edge cases with partial/malformed XERR packets, fallback when remote validator is not used.
