package control

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// The six characters section 11.10 names, each with the console command it
// would turn into. These are the cases that justify the whole file: every one
// of them is a working injection against a game server, not a lint rule.
func TestCheckArgRejectsTheNamedOffenders(t *testing.T) {
	for _, tc := range []struct {
		name string
		arg  string
		why  string
	}{
		{"double quote", `alice" ; servermsg "owned`,
			`closes servermsg "..." and starts a second command`},
		{"backslash", `alice\`, `escapes the closing quote of the template`},
		{"backtick", "alice`id`", "a wrapper shell would expand it"},
		{"dollar", `alice$(id)`, "a wrapper shell would expand it"},
		{"line feed", "hello\n/op attacker", "a second command on its own line"},
		{"carriage return", "hello\r/op attacker", "a second command on its own line"},
		{"nul", "alice\x00drop", "truncates the command at whatever reads it next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckArg("Msg", tc.arg)
			if err == nil {
				t.Fatalf("accepted %q, which %s", tc.arg, tc.why)
			}
			if !errors.Is(err, ErrUnsafeArg) {
				t.Errorf("error does not wrap ErrUnsafeArg: %v", err)
			}
			// The refusal must not reproduce the payload: this message ends up
			// in a log line and an audit row.
			if strings.Contains(err.Error(), tc.arg) {
				t.Errorf("the error echoes the rejected value: %q", err)
			}
			if strings.ContainsAny(err.Error(), "\r\n\x00") {
				t.Errorf("the error carries a control character into the log: %q", err)
			}
		})
	}
}

// Every C0 control character, not just the three that have names. A game
// console's line reader is not documented and 0x0b or 0x1c may well terminate
// a command on it.
func TestCheckArgRejectsEveryControlCharacter(t *testing.T) {
	for r := rune(0); r < 0x20; r++ {
		if err := CheckArg("Msg", "ok"+string(r)+"ok"); err == nil {
			t.Errorf("accepted control character %#04x", r)
		}
	}
	for r := rune(0x7f); r <= 0x9f; r++ {
		if err := CheckArg("Msg", "ok"+string(r)+"ok"); err == nil {
			t.Errorf("accepted control character %#04x", r)
		}
	}
}

func TestCheckArgLengthLimit(t *testing.T) {
	if err := CheckArg("Msg", strings.Repeat("a", MaxArg)); err != nil {
		t.Fatalf("%d characters refused: %v", MaxArg, err)
	}
	err := CheckArg("Msg", strings.Repeat("a", MaxArg+1))
	if err == nil {
		t.Fatalf("accepted %d characters", MaxArg+1)
	}
	if !errors.Is(err, ErrUnsafeArg) {
		t.Errorf("the length refusal does not wrap ErrUnsafeArg: %v", err)
	}
	// The limit counts characters, not bytes: a 512-character Cyrillic message
	// is 1024 bytes and must still be accepted, or the rule silently halves
	// for half the world.
	if err := CheckArg("Msg", strings.Repeat("д", MaxArg)); err != nil {
		t.Errorf("%d two-byte characters refused: %v", MaxArg, err)
	}
	if err := CheckArg("Msg", strings.Repeat("д", MaxArg+1)); err == nil {
		t.Errorf("accepted %d characters", MaxArg+1)
	}
}

func TestCheckArgAcceptsRealInput(t *testing.T) {
	for _, s := range []string{
		"",
		"Alice",
		"xX_dark_slayer_Xx",
		"Player.One",
		"alice@example.com",
		"[GVM] alice",
		"Bob (admin)",
		"restart in 5 minutes - please log out",
		"server restarting; save your progress!",
		"Réunion à 20h: le serveur redémarre",
		"перезапуск через 5 минут",
		"サーバーを再起動します",
		"1/2 done ~ 50%",
		"why? because <reasons>",
	} {
		if err := CheckArg("Msg", s); err != nil {
			t.Errorf("refused %q: %v", s, err)
		}
	}
}

// A curly quote, an em dash and an emoji are refused, and that is a product
// decision rather than an oversight: the class is closed, and a character
// nobody has tested against a game's command parser does not get to be the
// first thing an operator discovers about it.
func TestCheckArgRefusesCharactersOutsideTheClosedClass(t *testing.T) {
	for _, s := range []string{"don\u2019t", "wait \u2014 restarting", "restarting \U0001F680"} {
		if err := CheckArg("Msg", s); err == nil {
			t.Errorf("accepted %q, which is outside section 11.10's class", s)
		}
	}
}

// Invalid UTF-8 is refused with a message that says so, because "contains
// \ufffd" would be a lie about what is in the bytes.
func TestCheckArgRefusesInvalidUTF8(t *testing.T) {
	err := CheckArg("Player", "ali\xffce")
	if err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("message does not name the fault: %q", err)
	}
}

// The scanner exists only to explain a refusal; section 11.10's expression is
// what decides. This is the test that fails if they ever drift, which is the
// only way the two-implementation trick is safe.
func TestScannerAgreesWithTheSpecRegexp(t *testing.T) {
	check := func(s string) {
		t.Helper()
		want := argSafe.MatchString(s)
		got := CheckArg("", s) == nil
		if got != want {
			t.Fatalf("disagreement on %q (%x): scanner says %v, section 11.10's regexp says %v",
				s, s, got, want)
		}
	}

	for _, s := range []string{
		"", "a", "Alice", `"`, `\`, "`", "$", "\n", "\r", "\x00", "\x1b[31m",
		"ok\x7f", "\u00a0", "\u2019", "\U0001F680", "ali\xffce", "\xc3",
		"ⅳ is \\p{Nl}", "²", "½", "ß", "ǅ", "日本",
		strings.Repeat("a", MaxArg), strings.Repeat("a", MaxArg+1),
		strings.Repeat("д", MaxArg), strings.Repeat("д", MaxArg+1),
		"_.,:;!?@#%^&*()[]{}+=/<>~'|- ",
	} {
		check(s)
	}

	// Weighted random: mostly-legal strings with the occasional offender, so
	// the interesting cases are boundaries rather than uniform noise.
	alphabet := []rune("abcXYZ019 _.,:;!?@#%^&*()[]{}+=/<>~'|-é日ⅳ²\"\\`$\n\r\x00\x1b\u2019\U0001F680")
	rng := rand.New(rand.NewSource(11_10))
	var b strings.Builder
	for i := 0; i < 20_000; i++ {
		b.Reset()
		n := rng.Intn(24)
		if i%97 == 0 { // occasionally straddle the length limit
			n = MaxArg - 2 + rng.Intn(5)
		}
		for j := 0; j < n; j++ {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		if i%199 == 0 { // and occasionally corrupt the encoding
			s += "\xff"
		}
		check(s)
	}
}

func TestBindArgs(t *testing.T) {
	got, err := BindArgs(map[string]string{
		"Msg": "hello", "player": "alice", "REASON": "griefing", "Duration": "10m",
	})
	if err != nil {
		t.Fatalf("BindArgs: %v", err)
	}
	if got.Msg != "hello" || got.Player != "alice" || got.Reason != "griefing" || got.Duration != "10m" {
		t.Fatalf("bound %+v", got)
	}
}

func TestBindArgsRefusesWhatCannotBeRead(t *testing.T) {
	// An unknown key is refused rather than dropped: the template surface is
	// closed, so `message:` would broadcast an empty string and report success.
	if _, err := BindArgs(map[string]string{"message": "hello"}); err == nil {
		t.Error("accepted an unknown operand")
	} else if !errors.Is(err, ErrUnsafeArg) {
		t.Errorf("unknown operand does not wrap ErrUnsafeArg: %v", err)
	}

	// Two spellings of one operand: which one wins would depend on map
	// iteration order, so neither does.
	if _, err := BindArgs(map[string]string{"Msg": "a", "msg": "b"}); err == nil {
		t.Error("accepted one operand under two spellings")
	}

	if _, err := BindArgs(map[string]string{"Msg": "hi\nthere"}); err == nil {
		t.Error("accepted an operand containing a line feed")
	}
}

// Every operand is checked, not just the first one the map iteration reaches.
// BindArgs is what runs in production -- on the CLI before the socket is
// opened and again in the runner -- so the assertion belongs to it.
func TestBindArgsChecksEveryOperand(t *testing.T) {
	if _, err := BindArgs(map[string]string{"Msg": "fine", "Player": "bad\"name"}); err == nil {
		t.Fatal("an argument containing a double quote was bound")
	}
	if _, err := BindArgs(map[string]string{"Msg": "fine", "Player": "alice"}); err != nil {
		t.Fatalf("clean arguments were refused: %v", err)
	}
	// A lowercase key is the spelling CheckArgs could not see at all: it
	// indexed by the canonical names, so `msg=` checked nothing.
	if _, err := BindArgs(map[string]string{"msg": "bad\"quote"}); err == nil {
		t.Error("a lowercased operand skipped the character check")
	}
}

// The four names are the closed set of section 8.3's verb surface. If a fifth
// is ever added to spec.VerbArgs, this fails and the author has to decide what
// the escaping rule is for it before it can carry operator input.
func TestArgNamesAreTheClosedVerbSurface(t *testing.T) {
	want := "Msg Player Reason Duration"
	if got := strings.Join(ArgNames(), " "); got != want {
		t.Fatalf("ArgNames() = %q, want %q", got, want)
	}
	for _, n := range ArgNames() {
		if !utf8.ValidString(n) || n == "" {
			t.Fatalf("bad operand name %q", n)
		}
	}
}

// The VALUE half of this invariant is already asserted by
// TestCheckArgRejectsTheNamedOffenders. The NAME half was the one side nothing
// checked, and BindArgs is the one construction site whose Name is
// caller-controlled: an unknown JSON key, which console.Request.Validate never
// inspects. The refusal reaches Runner.Note through verbRecorder and lands in
// console.log, so a CR in the key forges a SourceRunner line there.
func TestBindArgsRefusalCannotForgeALogLine(t *testing.T) {
	for _, k := range []string{
		"Msg\n-- gvm: sending save",
		"Msg\r-- gvm: sending save",
		"Msg\x00forged",
		"Msg\xffbad",
	} {
		_, err := BindArgs(map[string]string{k: "hi"})
		if err == nil {
			t.Fatalf("accepted key %q", k)
		}
		if strings.ContainsAny(err.Error(), "\r\n\x00") {
			t.Errorf("the refusal carries a control character into console.log: %q", err.Error())
		}
		if strings.Contains(err.Error(), k) {
			t.Errorf("the refusal echoes the raw key: %q", err.Error())
		}
	}
}
