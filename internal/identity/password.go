// Package identity manages users, sessions, tokens and TOTP for gvm.
package identity

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// HashPassword returns a bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	if len(password) == 0 {
		return "", errors.New("identity: password must not be empty")
	}
	if len(password) > 72 {
		// bcrypt silently truncates at 72 bytes; reject to avoid confusion.
		return "", fmt.Errorf("identity: password exceeds 72 bytes")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("identity: hash password: %w", err)
	}
	return string(b), nil
}

// CheckPassword returns nil if password matches hash.
func CheckPassword(hash, password string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrWrongPassword
	}
	if err != nil {
		return fmt.Errorf("identity: check password: %w", err)
	}
	return nil
}

var ErrWrongPassword = errors.New("identity: wrong password")
