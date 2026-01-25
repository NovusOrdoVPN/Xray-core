package encoding

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Error response protocol for VLESS (client-side parsing)
// Format: [Magic:4][Version:1][Severity:1][Code:1][MsgLen:2][Message:UTF-8]

const (
	// ErrorMagic is the magic bytes that identify an error response
	ErrorMagic = "XERR"

	// Severity levels
	SeverityCritical = 0x01 // Client should disconnect immediately
	SeverityWarning  = 0x02 // Client may retry or show warning
	SeverityInfo     = 0x03 // Informational only
)

// ServerError represents an error response from the VLESS server.
// It implements the error interface so it can be returned as an error.
type ServerError struct {
	Severity byte
	Code     byte
	Message  string
}

// Error implements the error interface.
// The format "server error [code]: message" is used for log parsing on the client side.
func (e *ServerError) Error() string {
	return fmt.Sprintf("server error [%d]: %s", e.Code, e.Message)
}

// IsCritical returns true if this is a critical error that should cause disconnect.
func (e *ServerError) IsCritical() bool {
	return e.Severity == SeverityCritical
}

// TryParseServerError attempts to parse a server error response.
// It takes the first byte already read (to check for 'X') and the reader for remaining data.
// Returns nil, nil if the data does not start with XERR magic (not an error response).
// Returns the ServerError if successfully parsed.
// Returns nil, error if parsing fails after detecting XERR magic.
func TryParseServerError(reader io.Reader, firstByte byte) (*ServerError, error) {
	// Check if first byte is 'X' (0x58)
	if firstByte != 'X' {
		return nil, nil // Not an error response
	}

	// Read remaining 3 bytes of magic ("ERR")
	magicRest := make([]byte, 3)
	if _, err := io.ReadFull(reader, magicRest); err != nil {
		return nil, nil // Not enough data, probably not XERR
	}

	// Check if magic matches "XERR"
	if string(append([]byte{firstByte}, magicRest...)) != ErrorMagic {
		return nil, nil // Not an error response
	}

	// Parse the rest of the error response:
	// [Version:1][Severity:1][Code:1][MsgLen:2][Message:variable]
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, fmt.Errorf("failed to read error response header: %w", err)
	}

	// version := header[0] // Currently unused, for future compatibility
	severity := header[1]
	code := header[2]
	msgLen := binary.BigEndian.Uint16(header[3:5])

	// Read the message
	msg := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(reader, msg); err != nil {
			return nil, fmt.Errorf("failed to read error message: %w", err)
		}
	}

	return &ServerError{
		Severity: severity,
		Code:     code,
		Message:  string(msg),
	}, nil
}
