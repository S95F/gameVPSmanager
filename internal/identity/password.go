// Package identity manages users, sessions, tokens and TOTP for gvm.
package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinPasswordRunes is the stated minimum (admin-security-14): 12 characters.
// It is counted in runes, not bytes, so a passphrase of a dozen non-ASCII
// characters is not rejected for being "too short" when it is not.
const MinPasswordRunes = 12

// argon2id parameters, RFC 9106's second recommended option (64 MiB, t=3, p=4).
// They are written into every hash, so a future tuning of these constants does
// not strand the passwords hashed under the old ones: CheckPassword reads the
// parameters out of the stored string rather than assuming these.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB → 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

var (
	// ErrPasswordTooShort is a password below MinPasswordRunes.
	ErrPasswordTooShort = fmt.Errorf("identity: password must be at least %d characters", MinPasswordRunes)
	// ErrWrongPassword is a password that does not match the stored hash.
	ErrWrongPassword = errors.New("identity: wrong password")
	// errBadHash is a stored hash that is not a well-formed argon2id string.
	errBadHash = errors.New("identity: password hash is malformed")
)

// HashPassword hashes a password with argon2id and returns the PHC-format
// string stored for the user.
//
// argon2id replaced bcrypt (admin-security-14): bcrypt silently truncates at 72
// bytes, so a long passphrase became a weaker one with no warning. argon2id has
// no such cap, so there is no maximum here — only the minimum, enforced so that
// every stored hash is of a password that met the policy.
func HashPassword(password string) (string, error) {
	if utf8.RuneCountInString(password) < MinPasswordRunes {
		return "", ErrPasswordTooShort
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword returns nil if password matches the stored argon2id hash. The
// comparison is constant time, and the cost parameters come from the hash
// itself, so a hash written under different parameters still verifies.
func CheckPassword(hash, password string) error {
	salt, want, p, err := decodeArgon2id(hash)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrWrongPassword
	}
	return nil
}

type argon2Params struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeArgon2id parses the PHC string HashPassword produces:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>
func decodeArgon2id(hash string) (salt, key []byte, p argon2Params, err error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, p, errBadHash
	}
	var version int
	if _, e := fmt.Sscanf(parts[2], "v=%d", &version); e != nil || version != argon2.Version {
		return nil, nil, p, errBadHash
	}
	var mem, t, par int
	if _, e := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &par); e != nil {
		return nil, nil, p, errBadHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, p, errBadHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, p, errBadHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, p, errBadHash
	}
	p = argon2Params{memory: uint32(mem), time: uint32(t), threads: uint8(par)}
	return salt, key, p, nil
}
