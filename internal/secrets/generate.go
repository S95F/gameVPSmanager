package secrets

import (
	"crypto/rand"
	"fmt"
)

// Alphabet is base62: the digits, then the upper and lower case letters.
//
// The characters that are NOT here are the point, and the reason has changed
// since the bash suite wrote the rule down. install.sh refused quotes,
// backslashes, `$` and backticks because it interpolated the value into a
// shell command line; gvm renders argv directly with no shell in the path, so
// that reason is gone. What replaced it is narrower and harder to notice:
// every generated password is also written into a game's own config file, and
// those formats have no escaping at all. Project Zomboid's ini is `key=value`
// with `;` separating list elements, so a `;` or an `=` or a newline in a
// password silently corrupts the line — and the server starts anyway, with a
// different password than the one gvm recorded.
//
// base62 excludes all of those by construction, plus every shell metacharacter,
// plus whitespace, plus the ini comment characters. generate_test.go asserts
// each of those exclusions separately rather than trusting this sentence.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sampleCeiling is the rejection threshold: the largest multiple of the
// alphabet size that fits in a byte.
//
// 256 is not a multiple of 62. 256 = 4*62 + 8, so a naive `b % 62` maps five
// distinct byte values onto each of the first eight characters and only four
// onto the other fifty-four — making "0" through "7" exactly 25% more likely
// than every other character, in every position, forever. That is not a
// theoretical bias: over a 20-character password it is a measurable, exploitable
// skew in the search space, and it is invisible to every test that only checks
// the characters are legal. Bytes at or above this ceiling are thrown away and
// redrawn instead.
const sampleCeiling = 256 - (256 % len(Alphabet))

// MinPasswordLen is the floor GeneratePassword enforces.
//
// It is a guard against a caller's arithmetic, not a policy: 62^8 is about
// 2.2e14, and anything shorter is not a password. The shortest a shipped game
// demands is Valheim's five characters (section 8.6 check 13), so the floor
// never conflicts with a game's own rule.
const MinPasswordLen = 8

// MaxPasswordLen is a sanity ceiling. Nothing needs more, and the only way a
// caller asks for a million-character password is by passing the wrong
// variable.
const MaxPasswordLen = 4096

// DefaultPasswordLen is what `gvm create` and `gvm secret rotate` generate:
// about 119 bits of entropy, short enough to paste into a game's join dialog.
const DefaultPasswordLen = 20

// GeneratePassword returns n characters drawn uniformly from Alphabet.
//
// The error is the caller's length and nothing else. crypto/rand.Read no
// longer reports failure — since Go 1.24 it fills the buffer or crashes the
// program — so there is no partially-random return value to worry about here
// and no "if the entropy source failed" branch that could not be tested.
func GeneratePassword(n int) (Value, error) {
	if n < MinPasswordLen {
		return Value{}, fmt.Errorf("secrets: password length %d is below the %d-character floor", n, MinPasswordLen)
	}
	if n > MaxPasswordLen {
		return Value{}, fmt.Errorf("secrets: password length %d is above the %d-character ceiling", n, MaxPasswordLen)
	}

	out := make([]byte, 0, n)
	// One draw per character plus a margin: at a 3.1% rejection rate the
	// odds of needing a second draw are small, and the loop handles it
	// anyway. Drawing in a batch rather than a byte at a time keeps this off
	// the entropy source n times per password.
	buf := make([]byte, n+n/8+8)
	for len(out) < n {
		rand.Read(buf)
		for _, b := range buf {
			if int(b) >= sampleCeiling {
				continue
			}
			out = append(out, Alphabet[int(b)%len(Alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return New(string(out)), nil
}

// NewMasterKey mints a master key (section 14.7 class B).
func NewMasterKey() MasterKey {
	b := make([]byte, MasterKeySize)
	rand.Read(b)
	// ModeChecked describes a file that was read, and nothing has been read.
	// Saying true here would let a caller that persists this key and passes
	// the struct along believe a permission check happened.
	return MasterKey{Value: New(string(b))}
}
