package spec

import "regexp"

// An age X25519 identity (the PRIVATE key) begins with this prefix; the public
// recipient begins with "age1". The off-box backup config wants the recipient,
// and a pasted identity is both wrong and dangerous: it would be embedded in
// every archive's copy of the instance YAML, so the key that decrypts the
// off-box backups would travel inside them (offsite-backup-dr-3).
const AgeSecretKeyPrefix = "AGE-SECRET-KEY-1"

// ageSecretKeyRe matches a whole age identity: the prefix plus its bech32 body
// (bech32 is lowercase letters and digits, but the prefix is upper-case, so the
// match is case-insensitive on the prefix only).
var ageSecretKeyRe = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Za-z]+`)

// ContainsAgeSecretKey reports whether raw holds an age identity. apply and
// create refuse a YAML that does, naming the fix.
func ContainsAgeSecretKey(raw []byte) bool {
	return ageSecretKeyRe.Match(raw)
}

// RedactAgeSecretKeys replaces every age identity in raw with a marker and
// returns the count removed. It is the runtime backstop: an instance YAML
// hand-edited to carry an identity after apply must not embed it in the
// archive, so the backup redacts it and records a warning rather than refusing
// a world a stale credential should not block.
func RedactAgeSecretKeys(raw []byte) ([]byte, int) {
	n := 0
	out := ageSecretKeyRe.ReplaceAllFunc(raw, func([]byte) []byte {
		n++
		return []byte(AgeSecretKeyPrefix + "(redacted)")
	})
	return out, n
}
