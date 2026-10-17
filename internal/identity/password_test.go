package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestArgon2idRoundTrips(t *testing.T) {
	// A passphrase longer than bcrypt's old 72-byte cap, to prove the cap is
	// gone: two passwords that share the first 72 bytes must not both verify.
	const base = "correct horse battery staple correct horse battery staple and a good few extra words"
	pw := base + "ALPHA"
	other := base + "OMEGA"
	if len(base) < 72 {
		t.Fatalf("test fixture is only %d bytes; it must exceed bcrypt's 72-byte cap", len(base))
	}

	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not argon2id: %q", hash)
	}
	if err := CheckPassword(hash, pw); err != nil {
		t.Errorf("the right password did not verify: %v", err)
	}
	if err := CheckPassword(hash, other); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("a password sharing the first 72 bytes verified (the cap is back): %v", err)
	}
}

func TestPasswordMinimumIsEnforced(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("an 11-or-fewer-char password was accepted: %v", err)
	}
	if _, err := HashPassword("twelve chars"); err != nil { // exactly 12
		t.Errorf("a 12-character password was rejected: %v", err)
	}
}

func TestCheckPasswordRejectsAMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "$argon2id$v=19$m=65536,t=3,p=4$onlyonefield",
		"$bcrypt$...", "$argon2id$v=1$m=1,t=1,p=1$c2FsdA$a2V5",
	} {
		if err := CheckPassword(bad, "whatever-password"); err == nil {
			t.Errorf("a malformed hash %q verified a password", bad)
		}
	}
}
