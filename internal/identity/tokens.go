package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	tokenRandBytes    = 32 // 256-bit raw token
	recoveryCodeBytes = 16 // 128-bit recovery code
	sessionIDBytes    = 32 // 256-bit session ID
)

// TokenParts holds the plaintext token (shown once) and the SHA-256 hash
// (stored). The prefix is the first 8 characters of the plaintext, used
// to identify the token in listings without revealing it.
type TokenParts struct {
	Plaintext string // shown once to the user
	SHA256    []byte // stored in api_tokens.token_sha256
	Prefix    string // first 8 chars of plaintext
}

// NewAPIToken generates a new API token.
func NewAPIToken() (*TokenParts, error) {
	b := make([]byte, tokenRandBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("identity: generate api token: %w", err)
	}
	plain := "gvmt_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	return &TokenParts{
		Plaintext: plain,
		SHA256:    sum[:],
		Prefix:    plain[:8],
	}, nil
}

// HashToken returns the SHA-256 of a plaintext token for lookup.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// NewSessionID generates a random session ID.
func NewSessionID() (string, error) {
	b := make([]byte, sessionIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewRecoveryCode generates a single-use recovery code (base64url).
func NewRecoveryCode() (plaintext string, sha256hash []byte, err error) {
	b := make([]byte, recoveryCodeBytes)
	if _, err = rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("identity: generate recovery code: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], nil
}

// GenerateRecoveryCodes returns n recovery codes.
func GenerateRecoveryCodes(n int) ([]string, [][]byte, error) {
	plains := make([]string, n)
	hashes := make([][]byte, n)
	for i := range plains {
		p, h, err := NewRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		plains[i] = p
		hashes[i] = h
	}
	return plains, hashes, nil
}
