package spec

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrBytes is the sentinel every ParseBytes failure wraps, so callers can
// distinguish "this is not a size" from an I/O or decode failure without
// string-matching the message.
var ErrBytes = errors.New("invalid byte size")

// byteUnit is a suffix and the multiplier it selects. The slice is ordered
// largest-first because FormatBytes takes the first unit that divides evenly.
type byteUnit struct {
	suffix string
	mult   int64
}

// systemd's resource-control settings are base 1024 and understand K, M, G, T
// (and larger, which nothing on a game host has any business using). gvm accepts
// exactly the four it can also emit: every string FormatBytes produces parses
// back, and every string ParseBytes accepts has a canonical form. A unit gvm
// could parse but never write would be a silent asymmetry in `gvm apply --diff`.
var byteUnits = []byteUnit{
	{"T", 1 << 40},
	{"G", 1 << 30},
	{"M", 1 << 20},
	{"K", 1 << 10},
}

// ParseBytes converts a systemd-style size into a byte count.
//
// The suffixes are K, M, G and T, all base 1024, case-insensitive, and each also
// accepted in its "KB"/"KiB" spelling — systemd reads "KB" as 1024 bytes too, so
// gvm must not be the one place on the box where "MB" quietly means 10^6.
//
// A BARE INTEGER IS A LITERAL BYTE COUNT. "8" is eight bytes, not eight of
// anything else; the value an operator meant when they wrote a bare "19" in a
// memory field is unknowable, so gvm does not guess and doctor's
// memory_max >= heap + native_headroom check reports the absurd result rather
// than inventing megabytes.
//
// Fractions ("1.5G") are refused even though systemd accepts them: the rounding
// they imply is invisible in a diff, and "1536M" says the same thing exactly.
// Negative sizes are refused because no field that takes one has a meaning for
// them.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("%w: empty", ErrBytes)
	}

	digits, mult := splitByteSuffix(t)

	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		if strings.Contains(digits, ".") {
			return 0, fmt.Errorf("%w: %q: fractional sizes are not accepted; "+
				"write a whole number of the next smaller unit", ErrBytes, s)
		}
		return 0, fmt.Errorf("%w: %q: want an integer optionally suffixed K, M, G or T "+
			"(base 1024); a bare integer is a byte count", ErrBytes, s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: %q: negative", ErrBytes, s)
	}
	if mult != 1 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%w: %q: overflows int64", ErrBytes, s)
	}
	return n * mult, nil
}

// splitByteSuffix peels a recognised unit off the end of s and returns the
// digits plus the multiplier it selects. Spellings are tried longest-first so
// "GiB" is not read as "G" followed by junk. An unrecognised tail is left on the
// digits so ParseInt reports it, which keeps the error message about the string
// the operator actually wrote.
func splitByteSuffix(s string) (digits string, mult int64) {
	for _, u := range byteUnits {
		for _, spelling := range []string{u.suffix + "iB", u.suffix + "B", u.suffix} {
			if len(s) > len(spelling) && strings.EqualFold(s[len(s)-len(spelling):], spelling) {
				return strings.TrimSpace(s[:len(s)-len(spelling)]), u.mult
			}
		}
	}
	// A trailing bare "B" is systemd's spelling of "bytes".
	if len(s) > 1 && (s[len(s)-1] == 'B' || s[len(s)-1] == 'b') {
		return strings.TrimSpace(s[:len(s)-1]), 1
	}
	return s, 1
}

// FormatBytes renders n with the largest suffix that divides it evenly, so
// `memory_max: 8G` survives a load/save cycle as "8G" instead of turning into
// 8589934592 and making an unchanged drop-in look edited. Values that divide
// evenly by nothing render bare, which ParseBytes reads back as bytes.
//
// FormatBytes(ParseBytes(s)) is not always s — "7168M" and "7G" are the same
// number and both canonicalise to "7G" — but ParseBytes(FormatBytes(n)) == n
// for every n >= 0.
func FormatBytes(n int64) string {
	if n <= 0 {
		return strconv.FormatInt(n, 10)
	}
	for _, u := range byteUnits {
		if n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}
