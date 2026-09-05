package spec

import (
	"errors"
	"math"
	"testing"
)

func TestParseBytes(t *testing.T) {
	const (
		ki = int64(1) << 10
		mi = int64(1) << 20
		gi = int64(1) << 30
		ti = int64(1) << 40
	)

	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"bare integer is a literal byte count", "2097152", 2 * mi},
		{"a small bare integer is not megabytes", "19", 19},
		{"zero", "0", 0},
		{"gibibyte", "8G", 8 * gi},
		{"mebibyte", "7168M", 7 * gi},
		{"kibibyte", "100K", 100 * ki},
		{"tebibyte", "2T", 2 * ti},
		{"lowercase suffix, as jvm_xmx writes it", "5g", 5 * gi},
		{"KiB spelling", "8GiB", 8 * gi},
		{"KB spelling is still base 1024", "8GB", 8 * gi},
		{"bare B means bytes", "4096B", 4096},
		{"surrounding whitespace", "  512M  ", 512 * mi},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBytes(tc.in)
			if err != nil {
				t.Fatalf("ParseBytes(%q) = error %v, want %d", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseBytesRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"unknown suffix", "8Z"},
		{"suffix with no number", "G"},
		{"words", "eight gigs"},
		{"negative", "-1G"},
		{"fraction, because the rounding would be invisible in a diff", "1.5G"},
		{"overflow", "9223372036854775807G"},
		{"hex", "0x10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBytes(tc.in)
			if err == nil {
				t.Fatalf("ParseBytes(%q) = %d, want an error", tc.in, got)
			}
			if !errors.Is(err, ErrBytes) {
				t.Errorf("ParseBytes(%q) error %v does not wrap ErrBytes", tc.in, err)
			}
		})
	}
}

func TestFormatBytesPicksTheLargestEvenSuffix(t *testing.T) {
	const (
		ki = int64(1) << 10
		mi = int64(1) << 20
		gi = int64(1) << 30
		ti = int64(1) << 40
	)

	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{8 * gi, "8G"},
		{7 * gi, "7G"}, // 7168M is the same number; both canonicalise here
		{2 * mi, "2M"},
		{1536 * mi, "1536M"}, // 1.5G does not divide evenly by G
		{3 * ki, "3K"},
		{5 * ti, "5T"},
		{1023, "1023"}, // divides evenly by nothing; bare bytes read back as bytes
		{1, "1"},
	}

	for _, tc := range tests {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A drop-in that gvm rewrote must not differ from the one it read merely because
// the number took a trip through int64.
func TestBytesRoundTripThroughFormat(t *testing.T) {
	values := []int64{
		0, 1, 999, 1023, 1024, 2097152,
		5 << 30, 7 << 30, 8 << 30, 1536 << 20, 512 << 20, 3 << 40,
		math.MaxInt64 &^ (1<<20 - 1),
	}
	for _, n := range values {
		s := FormatBytes(n)
		got, err := ParseBytes(s)
		if err != nil {
			t.Fatalf("ParseBytes(FormatBytes(%d)) = ParseBytes(%q) error %v", n, s, err)
		}
		if got != n {
			t.Errorf("ParseBytes(FormatBytes(%d)) = %d via %q", n, got, s)
		}
	}
}

// The two spellings of seven gibibytes are the same number, which is what lets
// `gvm doctor`'s memory_max >= heap + native_headroom check compare a manifest
// against an instance without caring which unit either used.
func TestParseBytesUnitsAgree(t *testing.T) {
	pairs := [][2]string{
		{"7G", "7168M"},
		{"8G", "8589934592"},
		{"512M", "524288K"},
		{"1T", "1024G"},
	}
	for _, p := range pairs {
		a, err := ParseBytes(p[0])
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", p[0], err)
		}
		b, err := ParseBytes(p[1])
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", p[1], err)
		}
		if a != b {
			t.Errorf("ParseBytes(%q)=%d != ParseBytes(%q)=%d", p[0], a, p[1], b)
		}
	}
}
