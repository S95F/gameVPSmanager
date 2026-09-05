package console

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// specANSI is section 11.3 rule 3's pattern, transcribed literally. It is the
// oracle for the hand-rolled scanner: if the two ever disagree, the scanner is
// wrong, because the spec is the contract.
var specANSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no escapes at all", "SERVER STARTED", "SERVER STARTED"},
		{"empty", "", ""},
		{"sgr colour", "\x1b[31mERROR\x1b[0m: disk full", "ERROR: disk full"},
		{"sgr with several params", "\x1b[1;33;40mwarn\x1b[m", "warn"},
		{"private mode with ?", "\x1b[?25lhidden\x1b[?25h", "hidden"},
		{"erase line, as a progress bar draws", "\x1b[2Kloading 42%", "loading 42%"},
		{"cursor movement", "\x1b[10;20Hat", "at"},
		{"intermediate bytes", "\x1b[0 qtext", "text"},
		{"osc terminated by BEL", "\x1b]0;window title\x07real output", "real output"},
		{"osc terminated by ST", "\x1b]2;title\x1b\\after", "after"},
		{"osc with an empty payload", "\x1b]\x07x", "x"},
		{"several sequences in one line", "\x1b[32m[INFO]\x1b[0m \x1b]0;t\x07done", "[INFO] done"},
		{"tabs and other printables survive", "a\tb  c", "a\tb  c"},
		{"utf-8 survives", "\x1b[36mjoueur éà connecté\x1b[0m", "joueur éà connecté"},

		// Anything the grammar does not cover is evidence, and evidence is
		// kept. Each of these is also checked against the oracle below.
		{"truncated CSI at end of line", "text\x1b[31", "text\x1b[31"},
		{"lone escape", "text\x1bmore", "text\x1bmore"},
		{"unterminated OSC", "\x1b]0;never ends", "\x1b]0;never ends"},
		{"OSC with a bare ESC that is not ST", "\x1b]0;a\x1bZb\x07c", "\x1b]0;a\x1bZb\x07c"},
		{"escape followed by a non-CSI intro", "\x1b(Btext", "\x1b(Btext"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripANSI(tc.in); got != tc.want {
				t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got, want := StripANSI(tc.in), specANSI.ReplaceAllString(tc.in, ""); got != want {
				t.Errorf("scanner %q disagrees with the spec regexp %q", got, want)
			}
		})
	}
}

func TestStripANSIDoesNotAllocateOnACleanLine(t *testing.T) {
	// The hot path is every line of every server's output; a clean line must
	// return the same string, not a copy of it.
	const in = "1699 mods loaded"
	got := StripANSI(in)
	if got != in {
		t.Fatalf("StripANSI mangled a clean line: %q", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = StripANSI(in) }); n != 0 {
		t.Errorf("StripANSI allocated %v times on a clean line", n)
	}
}

// TestStripANSIAgreesWithTheSpecRegexpOnRandomBytes is the anti-drift test:
// the scanner exists for speed, so it has to be provably the same function as
// the pattern section 11.3 writes down.
func TestStripANSIAgreesWithTheSpecRegexpOnRandomBytes(t *testing.T) {
	// A weighted alphabet, so escapes, brackets and terminators show up far
	// more often than uniform random bytes would produce them.
	alphabet := []byte("\x1b\x1b\x1b[]0129;?@m \\\x07\\ABCxyz\t")
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 20000; i++ {
		n := rng.Intn(24)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		in := b.String()
		if got, want := StripANSI(in), specANSI.ReplaceAllString(in, ""); got != want {
			t.Fatalf("StripANSI(%q) = %q, spec regexp = %q", in, got, want)
		}
	}
}

func TestEscLen(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\x1b", 0},
		{"x[0m", 0},
		{"\x1b[0m", 4},
		{"\x1b[m", 3},
		{"\x1b[1;2;3;4;5H", 12},
		{"\x1b]0;t\x07", 6},
		{"\x1b]0;t\x1b\\", 7},
		{"\x1b]0;t", 0},
	}
	for _, tc := range tests {
		if got := escLen(tc.in); got != tc.want {
			t.Errorf("escLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
