package secrets

import (
	"crypto/sha256"
	"encoding/hex"
)

// FingerprintLen is how many hex characters a fingerprint carries: 12, which is
// 48 bits of a SHA-256. Enough that two different secrets practically never
// collide in a listing an operator is scanning, and far too few to turn back
// into a 119-bit password — the whole point of showing a fingerprint instead
// of a length-only line is to let an operator confirm "the value on this box is
// the one I set" and "these two instances share a key" without ever seeing the
// plaintext.
const FingerprintLen = 12

// Fingerprint is a short, stable, one-way label for a secret's value.
//
// It is computed in-package from plain(), NOT through Reveal: a fingerprint is
// derived data that leaves the secret where it is, so spending one of Reveal's
// countable call sites on it would be both wrong and a slow leak of the budget
// that exists to notice real plaintext handling. An empty value fingerprints as
// the empty string, because there is nothing to label and a listing says
// "unset" rather than a hash of nothing.
func Fingerprint(v Value) string {
	p := v.plain()
	if p == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])[:FingerprintLen]
}
