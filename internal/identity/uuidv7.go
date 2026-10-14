package identity

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// NewUUIDv7 generates a UUID version 7 (time-ordered random UUID).
// Format: 48 bits Unix ms | 4 bits ver(7) | 12 bits rand | 2 bits var | 62 bits rand.
func NewUUIDv7() ([16]byte, error) {
	var uuid [16]byte

	now := time.Now().UnixMilli()

	// 48-bit millisecond timestamp in the first 6 bytes.
	binary.BigEndian.PutUint32(uuid[0:], uint32(now>>16))
	binary.BigEndian.PutUint16(uuid[4:], uint16(now))

	// Fill the rest with random bytes.
	if _, err := rand.Read(uuid[6:]); err != nil {
		return uuid, fmt.Errorf("identity: generate uuidv7: %w", err)
	}

	// Version bits: 0111 (v7) in the high nibble of byte 6.
	uuid[6] = (uuid[6] & 0x0f) | 0x70

	// Variant bits: 10xx in the high bits of byte 8.
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return uuid, nil
}

// UUIDv7String returns a UUIDv7 as a lowercase hyphenated string.
func UUIDv7String() (string, error) {
	u, err := NewUUIDv7()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}
