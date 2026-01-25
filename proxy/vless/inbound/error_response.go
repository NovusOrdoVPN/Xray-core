package inbound

import (
	"encoding/binary"
	"net"
)

// Error response protocol for VLESS
// Format: [Magic:4][Version:1][Severity:1][Code:1][MsgLen:2][Message:UTF-8]

const (
	// ErrorMagic is the magic bytes that identify an error response
	ErrorMagic = "XERR"

	// ErrorVersion is the current version of the error response protocol
	ErrorVersion = 0x01

	// Severity levels
	SeverityCritical = 0x01 // Client should disconnect immediately
	SeverityWarning  = 0x02 // Client may retry or show warning
	SeverityInfo     = 0x03 // Informational only

	// Error codes (must match tower API)
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

// GetSeverityForCode returns the appropriate severity level for an error code
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

// SendErrorResponse sends an XERR error response to the client before closing the connection.
// This allows the client to understand why the connection was rejected.
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

	// Version
	buf[4] = ErrorVersion

	// Severity
	buf[5] = severity

	// Error code
	buf[6] = code

	// Message length (big-endian)
	binary.BigEndian.PutUint16(buf[7:9], uint16(len(msg)))

	// Message content
	copy(buf[9:], msg)

	_, err := conn.Write(buf)
	return err
}
