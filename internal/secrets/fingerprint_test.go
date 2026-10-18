package secrets

import (
	"strings"
	"testing"
)

func TestFingerprintIsStableAndFixedWidth(t *testing.T) {
	v := New("Xk4mQ9pLzR2vT7wB")
	a, b := Fingerprint(v), Fingerprint(New("Xk4mQ9pLzR2vT7wB"))
	if a != b {
		t.Errorf("Fingerprint of the same value differs: %q vs %q", a, b)
	}
	if len(a) != FingerprintLen {
		t.Errorf("Fingerprint is %d chars, want %d", len(a), FingerprintLen)
	}
}

func TestFingerprintDiffersForDifferentValues(t *testing.T) {
	if Fingerprint(New("one")) == Fingerprint(New("two")) {
		t.Error("two different secrets share a fingerprint")
	}
}

func TestFingerprintOfEmptyIsEmpty(t *testing.T) {
	if fp := Fingerprint(Value{}); fp != "" {
		t.Errorf("Fingerprint of an unset value = %q, want empty", fp)
	}
}

// TestFingerprintDoesNotContainTheValue: a fingerprint is a label, not a
// leak. A short password must not appear verbatim in its own fingerprint.
func TestFingerprintDoesNotContainTheValue(t *testing.T) {
	const pw = "abcdef12"
	if strings.Contains(Fingerprint(New(pw)), pw) {
		t.Error("the fingerprint contains the plaintext")
	}
}
