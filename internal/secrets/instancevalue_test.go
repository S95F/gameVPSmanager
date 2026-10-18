package secrets

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateInstanceValueBansGameHostileChars: a secret that reaches a game's
// argv or config files is refused for any character that would be cut, escaped
// or re-interpreted (rule 10). The refusal names the offset and the reason,
// never the value.
func TestValidateInstanceValueBansGameHostileChars(t *testing.T) {
	for _, bad := range []string{
		`a'b`, `a"b`, `a\b`, `a$b`, "a`b", `a;b`, `a=b`, `a b`, "a\tb",
	} {
		err := ValidateInstanceValue(New(bad))
		if !errors.Is(err, ErrBannedChar) {
			t.Errorf("%q should be a banned-char refusal, got %v", bad, err)
		}
		// The refusal must not echo the value.
		if err != nil && strings.Contains(err.Error(), bad) {
			t.Errorf("refusal echoed the value: %q", err)
		}
	}
}

// TestValidateInstanceValueAcceptsGoodPasswords: a generated-style password
// passes, and the file-format rules (empty, newline) still apply.
func TestValidateInstanceValueAcceptsGoodPasswords(t *testing.T) {
	for _, ok := range []string{"P4ssw0rd", "aB3xY9zQ", "base62ONLY123"} {
		if err := ValidateInstanceValue(New(ok)); err != nil {
			t.Errorf("%q should pass, got %v", ok, err)
		}
	}
	if err := ValidateInstanceValue(New("")); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty should be ErrEmpty, got %v", err)
	}
	if err := ValidateInstanceValue(New("a\nb")); !errors.Is(err, ErrMultiline) {
		t.Errorf("newline should be ErrMultiline, got %v", err)
	}
}

// TestHostSecretsKeepOnlyFileRules: a CurseForge-style key ($2a$10$…) is
// refused by the instance ban but accepted by the host-scope ValidateValue,
// which is the whole reason the two checks are separate.
func TestHostSecretsKeepOnlyFileRules(t *testing.T) {
	key := New("$2a$10$abcdefghijklmnopqrstuv")
	if err := ValidateInstanceValue(key); !errors.Is(err, ErrBannedChar) {
		t.Errorf("the $ in a bcrypt-style key should fail the instance ban, got %v", err)
	}
	if err := ValidateValue(key); err != nil {
		t.Errorf("a host API key should pass the file-format rules, got %v", err)
	}
}
