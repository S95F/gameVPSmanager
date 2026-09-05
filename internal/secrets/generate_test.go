package secrets

import (
	"fmt"
	"strings"
	"testing"
)

// forbidden is the rule the bash suite wrote down, with its reason updated.
//
// install.sh hard-failed on quotes, backslashes, `$` and backticks because it
// interpolated the value into a shell command line. gvm renders argv directly
// with no shell, so that reason is gone -- but a generated password is also
// written into the game's own config file, and those formats have no escaping
// at all. Project Zomboid's ini is `key=value` with `;` between list elements:
// a `;`, an `=` or a newline in a password corrupts the line, and the server
// starts anyway with a password that no longer matches what gvm recorded.
//
// The shell metacharacters stay on the list regardless, because a manifest
// hook or an operator's own script will eventually put one of these values on
// a command line, and the cost of excluding them is 0.03 bits per character.
var forbidden = []struct {
	c   byte
	why string
}{
	{';', "Project Zomboid's ini separates list elements with it"},
	{'=', "every key=value config format gvm writes ends the key at it"},
	{'\n', "a secret file holds exactly one line and an ini key exactly one value"},
	{'\r', "the same, on a file that travelled through Windows"},
	{'"', "quotes an argument in every shell and several config formats"},
	{'\'', "the same"},
	{'\\', "escapes the character after it, in ini files and in shells"},
	{'`', "command substitution"},
	{'$', "variable and command substitution"},
	{'&', "backgrounds a command"},
	{'|', "pipes one"},
	{'<', "redirects"},
	{'>', "redirects"},
	{'*', "globs"},
	{'?', "globs"},
	{'[', "globs"},
	{']', "globs"},
	{'(', "subshell"},
	{')', "subshell"},
	{'{', "brace expansion"},
	{'}', "brace expansion"},
	{'~', "home expansion"},
	{'!', "history expansion in an interactive shell"},
	{'#', "starts a comment in an ini file and in a shell"},
	{';', "again, because it is the one that actually bit"},
	{' ', "splits an argument and is invisible at the end of a value"},
	{'\t', "the same, more so"},
	{'\x00', "truncates a C string, which is what the game parses"},
	{'%', "systemd expands it in a unit file"},
	{':', "separates fields in a LoadCredential= line and in /etc/passwd"},
	{'/', "a path separator, and a value that reaches a filename"},
	{',', "separates elements in several game config formats"},
}

func TestAlphabetExcludesEveryCharacterThatCouldCorruptAConfigOrACommandLine(t *testing.T) {
	for _, f := range forbidden {
		if strings.IndexByte(Alphabet, f.c) >= 0 {
			t.Errorf("Alphabet contains %q: %s", f.c, f.why)
		}
	}
}

func TestAlphabetIsBase62(t *testing.T) {
	if len(Alphabet) != 62 {
		t.Fatalf("Alphabet is %d characters, want 62", len(Alphabet))
	}
	seen := map[byte]bool{}
	for i := range len(Alphabet) {
		c := Alphabet[i]
		if seen[c] {
			t.Errorf("Alphabet repeats %q, which skews every position toward it", c)
		}
		seen[c] = true
		alnum := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !alnum {
			t.Errorf("Alphabet contains the non-alphanumeric %q", c)
		}
	}
}

func TestSampleCeilingIsTheLargestUsableMultiple(t *testing.T) {
	// The rejection threshold is the whole defence against modulo bias, and
	// it is a constant expression, so getting it wrong is silent.
	if sampleCeiling%len(Alphabet) != 0 {
		t.Errorf("sampleCeiling %d is not a multiple of %d; the bias is back", sampleCeiling, len(Alphabet))
	}
	if sampleCeiling > 256 || 256-sampleCeiling >= len(Alphabet) {
		t.Errorf("sampleCeiling %d is not the LARGEST multiple below 256; the generator "+
			"throws away more entropy than it needs to", sampleCeiling)
	}
	if sampleCeiling != 248 {
		t.Errorf("sampleCeiling = %d, want 248 (256 = 4*62 + 8)", sampleCeiling)
	}
}

func TestGeneratePasswordRejectsLengthsOutsideTheBounds(t *testing.T) {
	for _, n := range []int{-1, 0, 1, MinPasswordLen - 1, MaxPasswordLen + 1} {
		if v, err := GeneratePassword(n); err == nil {
			t.Errorf("GeneratePassword(%d) = a %d-character password, want an error", n, v.Len())
		}
	}
	for _, n := range []int{MinPasswordLen, DefaultPasswordLen, MaxPasswordLen} {
		v, err := GeneratePassword(n)
		if err != nil || v.Len() != n {
			t.Errorf("GeneratePassword(%d) = %d characters, %v", n, v.Len(), err)
		}
	}
}

// samples is the property test's sample count. Each sample is a whole password,
// so the character-level assertions below run over samples*DefaultPasswordLen
// draws.
const samples = 100_000

func TestGeneratedPasswordsAreTheRightLengthInTheRightAlphabetAndUniform(t *testing.T) {
	inAlphabet := [256]bool{}
	for i := range len(Alphabet) {
		inAlphabet[Alphabet[i]] = true
	}

	counts := make([]float64, 256)
	seen := make(map[string]bool, samples)
	total := 0
	for i := range samples {
		v, err := GeneratePassword(DefaultPasswordLen)
		if err != nil {
			t.Fatalf("GeneratePassword: %v", err)
		}
		pw := v.Reveal()
		if len(pw) != DefaultPasswordLen {
			t.Fatalf("sample %d is %d characters, want exactly %d", i, len(pw), DefaultPasswordLen)
		}
		for j := range len(pw) {
			c := pw[j]
			if !inAlphabet[c] {
				t.Fatalf("sample %d position %d is %q, which is not in the alphabet", i, j, c)
			}
			counts[c]++
			total++
		}
		if seen[pw] {
			t.Fatalf("sample %d repeats an earlier password; at 62^%d that is not luck",
				i, DefaultPasswordLen)
		}
		seen[pw] = true
	}

	chi := chiSquare(counts, float64(total))
	if chi > chiSquareLimit {
		t.Errorf("character distribution over %d draws has chi-square %.1f, above the %.0f "+
			"limit for %d degrees of freedom; the generator is skewed",
			total, chi, chiSquareLimit, len(Alphabet)-1)
	}
	t.Logf("%d draws, chi-square %.1f (limit %.0f, expectation %d)", total, chi, chiSquareLimit, len(Alphabet)-1)
}

func TestNaiveModuloWouldFailTheVeryCheckThatJustPassed(t *testing.T) {
	// Without this, the test above proves only that the code under test is
	// not catastrophically broken -- it never shows the limit can detect the
	// specific bug the rejection sampling exists to prevent.
	//
	// A naive `b % 62` over a uniform byte maps five byte values onto each of
	// the first eight characters and four onto the other fifty-four, because
	// 256 = 4*62 + 8. The first eight characters of the alphabet therefore
	// come up 25% more often than the rest, in every position.
	total := float64(samples * DefaultPasswordLen)
	counts := make([]float64, 256)
	for b := range 256 {
		counts[Alphabet[b%len(Alphabet)]] += total / 256
	}

	// The shape is right: "0" through "7" get five byte values, the rest four.
	if counts['0'] != total*5/256 || counts['8'] != total*4/256 {
		t.Fatalf("the biased model is wrong: '0' gets %.0f and '8' gets %.0f of %.0f",
			counts['0'], counts['8'], total)
	}
	chi := chiSquare(counts, total)
	if chi <= chiSquareLimit {
		t.Errorf("modulo bias over %.0f draws scores chi-square %.1f, which is inside the "+
			"%.0f limit -- so the uniformity test above cannot detect it and is decoration",
			total, chi, chiSquareLimit)
	}
	t.Logf("modulo bias would score %.0f against a limit of %.0f", chi, chiSquareLimit)
}

func TestAGeneratedPasswordIsStillARedactedValue(t *testing.T) {
	v, err := GeneratePassword(DefaultPasswordLen)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v %s %q", v, v, v); got != Redacted+" "+Redacted+" "+Redacted {
		t.Errorf("a freshly generated password formats as %q", got)
	}
	if strings.Contains(fmt.Sprintf("%v", v), v.Reveal()) {
		t.Error("the generator returned something that prints itself")
	}
}

// chiSquareLimit is the acceptance threshold for 61 degrees of freedom.
//
// The statistic's mean is 61 with a standard deviation of about 11, and the
// Wilson-Hilferty approximation puts the one-in-a-million point near 129. 140
// is past that, so a correct generator fails this test roughly never, while
// the modulo bias next door scores in the thousands -- the gap is four orders
// of magnitude, not a judgement call.
const chiSquareLimit = 140.0

func chiSquare(counts []float64, total float64) float64 {
	expected := total / float64(len(Alphabet))
	var chi float64
	for i := range len(Alphabet) {
		d := counts[Alphabet[i]] - expected
		chi += d * d / expected
	}
	return chi
}
