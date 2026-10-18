package identity

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// TestRecoveryCodeIsCrockfordAndNormalises: the shown code is grouped Crockford
// base32, and it verifies no matter how the operator types it back — any case,
// with or without the dashes — because the stored hash is of the normalized
// form.
func TestRecoveryCodeIsCrockfordAndNormalises(t *testing.T) {
	plain, hash, err := NewRecoveryCode()
	if err != nil {
		t.Fatalf("NewRecoveryCode: %v", err)
	}

	canonical := NormalizeRecoveryCode(plain)
	if len(canonical) != 16 {
		t.Fatalf("normalized code is %d chars, want 16: %q from %q", len(canonical), canonical, plain)
	}
	// No ambiguous characters survive normalization.
	for _, r := range canonical {
		if strings.ContainsRune("ILOU", r) {
			t.Errorf("normalized code contains an ambiguous character %q: %q", r, canonical)
		}
	}
	// The stored hash is of the normalized form.
	if sum := sha256.Sum256([]byte(canonical)); string(sum[:]) != string(hash) {
		t.Error("the stored hash is not sha256 of the normalized code")
	}

	// The same code typed lowercase, without dashes, and with stray spaces
	// normalizes identically.
	messy := strings.ToLower(strings.ReplaceAll(plain, "-", " "))
	if NormalizeRecoveryCode(messy) != canonical {
		t.Errorf("a messily-typed code did not normalize to the same value:\n %q\n %q",
			NormalizeRecoveryCode(messy), canonical)
	}

	// Crockford confusables map to their digits: O→0, I→1, L→1, so each "OILO"
	// group becomes "0110".
	if got := NormalizeRecoveryCode("OILO-OILO-OILO-OILO"); got != "0110011001100110" {
		t.Errorf("confusable mapping wrong: got %q", got)
	}
}

// TestNormalizeRejectsWrongLength: a code of the wrong length is "" (not a
// partial match), so a garbled entry fails as wrong rather than coinciding.
func TestNormalizeRejectsWrongLength(t *testing.T) {
	for _, bad := range []string{"", "ABCD", "ABCD-EFGH-JKMN-PQRS-TVWX"} {
		if got := NormalizeRecoveryCode(bad); got != "" {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want empty", bad, got)
		}
	}
}
