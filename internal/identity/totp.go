package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // TOTP (RFC 6238) mandates SHA-1
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const (
	totpDigits   = 6
	totpPeriod   = 30 // seconds
	totpWindow   = 1  // accept ±1 window (30s drift tolerance)
	totpKeyBytes = 20 // 160-bit secret for SHA-1 HOTP
)

// NewTOTPSecret generates a random TOTP secret (base32-encoded).
func NewTOTPSecret() (string, error) {
	b := make([]byte, totpKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// TOTPCode returns the current TOTP code for a base32-encoded secret.
func TOTPCode(secret string) (string, error) {
	return totpAt(secret, time.Now())
}

// VerifyTOTP checks a 6-digit code against the secret, accepting ±1 window.
// It returns the step that matched (for last_totp_step replay detection), or
// an error if no window matches.
func VerifyTOTP(secret, code string, lastStep int64) (int64, error) {
	now := time.Now()
	currentStep := now.Unix() / totpPeriod

	for delta := int64(-totpWindow); delta <= totpWindow; delta++ {
		step := currentStep + delta
		if step == lastStep {
			continue // replay prevention
		}
		expected, err := hotpCode(secret, uint64(step))
		if err != nil {
			return 0, err
		}
		if expected == code {
			return step, nil
		}
	}
	return 0, ErrInvalidTOTP
}

func totpAt(secret string, t time.Time) (string, error) {
	step := uint64(t.Unix()) / totpPeriod
	return hotpCode(secret, step)
}

func hotpCode(secret string, counter uint64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("identity: decode totp secret: %w", err)
	}

	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	h := mac.Sum(nil)

	offset := h[len(h)-1] & 0x0f
	code := binary.BigEndian.Uint32(h[offset:offset+4]) & 0x7fffffff
	code %= uint32(math.Pow10(totpDigits))

	return fmt.Sprintf("%0*d", totpDigits, code), nil
}

var ErrInvalidTOTP = fmt.Errorf("identity: invalid or expired TOTP code")
