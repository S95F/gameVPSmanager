package spec

import (
	"strings"
	"testing"
)

// NewUID mints the identity an instance keeps forever (D-25). Three things
// have to hold, and each of them fails silently if it does not.
func TestNewUIDIsACanonicalUUIDv7(t *testing.T) {
	seen := map[UID]bool{}
	var prev UID
	for i := 0; i < 2000; i++ {
		u := NewUID()

		// It must survive its own parser, because a uid that decodes as
		// something else is a NEW instance identity, and the failure is
		// invisible until every grant row for the old one disappears.
		got, err := ParseUID(string(u))
		if err != nil {
			t.Fatalf("NewUID produced %q, which ParseUID rejects: %v", u, err)
		}
		if got != u {
			t.Fatalf("ParseUID normalised %q to %q; NewUID must emit the canonical form", u, got)
		}

		// Version 7 and the RFC 4122 variant, in the nibbles that carry them.
		if u[14] != '7' {
			t.Fatalf("%q: version nibble = %q, want '7'", u, u[14])
		}
		if !strings.ContainsRune("89ab", rune(u[19])) {
			t.Fatalf("%q: variant nibble = %q, want one of 8,9,a,b", u, u[19])
		}

		if seen[u] {
			t.Fatalf("NewUID repeated %q within %d calls", u, i)
		}
		seen[u] = true

		// The first six bytes are big-endian milliseconds, so ids minted in
		// order sort in order as strings. Anything keyed by one -- an index,
		// a listing -- is built on that, and it costs nothing to assert.
		if prev != "" && u[:8] < prev[:8] {
			t.Fatalf("%q sorts before %q; the timestamp prefix is not monotonic", u, prev)
		}
		prev = u
	}
}
