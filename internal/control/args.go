package control

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Argument escaping is a real injection surface, and it is not a theoretical
// one (section 11.10).
//
// `broadcast: {control: "servermsg \"{{ .Msg }}\""}` interpolates operator
// input straight into a command sent to a live game server's console. A game
// console has no quoting standard: there is no shell, no escape character
// anyone documented, and no library that knows what Project Zomboid's parser
// does with a backslash. So there is nothing to escape TO. A `"` in a player
// name breaks the command; a `\n` in a Minecraft message is not a rude word in
// the chat, it is `/op attacker` running as the server.
//
// The only safe rule is refusal: a closed character class, the same shape of
// closed surface as the manifest's template grammar, and anything outside it is
// rejected before it reaches a renderer. That costs an operator the ability to
// put a curly quote in a broadcast. It buys the guarantee that no console
// command gvm sends can be extended or terminated by operator input.

// MaxArg is the argument length limit, in CHARACTERS, not bytes -- the same
// unit the regexp below counts in.
//
// 512 is not about buffer safety (the dialects cap request bodies at 1446 or
// 4096 bytes themselves); it is about a broadcast being a broadcast. A 4 KB
// "message" is either an accident or an attempt to find out what the server's
// parser does when it runs out of room.
const MaxArg = 512

// argSafe is section 11.10's regexp, verbatim, and it is the thing that decides.
//
// The scanner below exists only to explain a refusal, because "argument
// contains a character that cannot be safely quoted" tells an operator nothing
// about which character. If the two ever disagree the argument is REFUSED --
// this expression is the authority and the scanner is the commentary.
// args_test.go proves they agree over the table and over 20 000 random inputs,
// so a disagreement is a CI failure rather than a mystery rejection at 2am.
//
// Absent from the class, deliberately: " \ ` $ and every C0/C1 control
// character including \n, \r and NUL.
var argSafe = regexp.MustCompile(`^[\p{L}\p{N} _.,:;!?@#%^&*()\[\]{}+=/<>~'|-]{0,512}$`)

// ErrUnsafeArg is the sentinel every refusal in this file wraps, including the
// length refusal: a call site asking "was this the caller's fault" wants one
// answer, not two.
var ErrUnsafeArg = errArg("control: argument contains a character that cannot be safely quoted")

// errArg exists so ErrUnsafeArg is a plain comparable error value without
// pulling errors.New into a var block that also declares a type.
type errArg string

func (e errArg) Error() string { return string(e) }

// ArgError is one refusal, positioned. Name is the verb operand ("Msg"), empty
// when a bare string was checked.
//
// The offending VALUE is never in the message. It is operator input on its way
// into an audit row and a log line, and a refusal that echoes a NUL or a CR
// into the log has reproduced the injection it just prevented -- one line lower.
type ArgError struct {
	Name   string
	Offset int // byte offset of the offending rune; -1 for a whole-value fault
	Why    string
}

// Error renders the refusal. Name is QUOTED because BindArgs sets it from a
// raw JSON key: console.Request.Validate never inspects those, and this
// message travels through verbRecorder into Runner.Note, which writes it to
// console.log. An unquoted CR there forges a second line in the record of who
// asked for what -- exactly the injection the type comment says a refusal must
// not reproduce, one field over. strconv.Quote also covers NUL and invalid
// UTF-8, so the whole control-character family is closed rather than three
// named members of it.
func (e *ArgError) Error() string {
	where := "argument"
	if e.Name != "" {
		where = "argument " + strconv.Quote(e.Name)
	}
	if e.Offset < 0 {
		return fmt.Sprintf("control: %s %s", where, e.Why)
	}
	return fmt.Sprintf("control: %s %s at byte %d", where, e.Why, e.Offset)
}

// Unwrap makes errors.Is(err, ErrUnsafeArg) true for every refusal, which is
// what the socket layer matches on to answer bad_request rather than internal.
func (e *ArgError) Unwrap() error { return ErrUnsafeArg }

// CheckArg reports whether s may be interpolated into a console command.
// name is used only to make the error readable and may be empty.
func CheckArg(name, s string) error {
	if n := utf8.RuneCountInString(s); n > MaxArg {
		return &ArgError{Name: name, Offset: -1,
			Why: fmt.Sprintf("is %d characters, over the %d limit", n, MaxArg)}
	}
	for i, r := range s {
		if safeRune(r) {
			continue
		}
		return &ArgError{Name: name, Offset: i, Why: describeRune(s, i, r)}
	}
	// Belt and braces, and the braces are the authority: if the scanner and
	// section 11.10's expression ever disagree, the argument does not go out.
	if !argSafe.MatchString(s) {
		return &ArgError{Name: name, Offset: -1,
			Why: "is outside the characters a console command may carry"}
	}
	return nil
}

// safeRune mirrors the character class of argSafe.
func safeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true // the overwhelmingly common case, before any table
	case r == ' ':
		return true
	case strings.ContainsRune(`_.,:;!?@#%^&*()[]{}+=/<>~'|-`, r):
		return true
	case r < utf8.RuneSelf:
		// Every remaining ASCII byte -- ", \, `, $, and the control
		// characters -- is refused. Listing the survivors rather than the
		// offenders is what makes a new offender impossible to forget.
		return false
	}
	// \p{L}|\p{N} for the non-ASCII range. unicode.In with the L and N tables
	// rather than IsLetter/IsDigit: \p{N} covers Nl and No (Roman numerals,
	// superscript digits) which IsDigit excludes, and that mismatch would
	// surface only as a player name refused for one locale and no other.
	return unicode.In(r, unicode.L, unicode.N)
}

// describeRune names the offender without quoting the value.
func describeRune(s string, i int, r rune) string {
	if r == utf8.RuneError {
		if _, size := utf8.DecodeRuneInString(s[i:]); size <= 1 {
			return "is not valid UTF-8"
		}
	}
	if name, ok := namedOffenders[r]; ok {
		return "contains " + name
	}
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return fmt.Sprintf("contains the control character %#04x", r)
	}
	return fmt.Sprintf("contains %q, which is not one of the characters a console command may carry", r)
}

// namedOffenders spells out the six characters section 11.10 rejects by name,
// with the consequence rather than the category. An operator who reads "a line
// feed is a second command the server will run" fixes their input; one who
// reads "invalid character" opens a ticket.
var namedOffenders = map[rune]string{
	'"':    `a double quote, which would close the quoting around it`,
	'\\':   `a backslash, which no game console documents an escaping rule for`,
	'`':    "a backtick, which a wrapper shell would expand",
	'$':    "a dollar sign, which a wrapper shell would expand",
	'\n':   "a line feed, which the console would run as a second command",
	'\r':   "a carriage return, which the console would run as a second command",
	'\x00': "a NUL, which truncates the command at whatever reads it next",
}

// ArgNames returns the four operand names, in the order a diagnostic lists
// them. They are spec.VerbArgs' fields: the template surface is closed
// (render_guard.go's verbVars), so this is the complete set an operator can
// supply and there is nowhere for a fifth to come from.
func ArgNames() []string { return []string{"Msg", "Player", "Reason", "Duration"} }

// BindArgs turns the socket's `args` map into the renderer's operands.
//
// Names are matched case-insensitively because the map arrives as JSON from
// gvm-web and a JSON client that lowercases its keys should not silently
// broadcast an empty message. Two spellings of one name is an error rather than
// a last-one-wins, since which one won would depend on Go's map iteration order
// -- a broadcast that says different things on different days.
//
// An unknown key is an error too. The template surface is closed, so an
// unknown key cannot possibly be read; accepting it silently would let an
// operator type `message:` instead of `Msg:` and watch the server broadcast
// nothing at all, with a success reported.
func BindArgs(args map[string]string) (spec.VerbArgs, error) {
	var out spec.VerbArgs
	seen := make(map[string]string, len(args))
	for k, v := range args {
		canon, ok := canonArg(k)
		if !ok {
			return spec.VerbArgs{}, &ArgError{Name: k, Offset: -1,
				Why: "is not a verb operand (want " + strings.Join(ArgNames(), ", ") + ")"}
		}
		if prev, dup := seen[canon]; dup {
			return spec.VerbArgs{}, &ArgError{Name: canon, Offset: -1,
				Why: fmt.Sprintf("was given twice, as %q and %q", prev, k)}
		}
		seen[canon] = k
		if err := CheckArg(canon, v); err != nil {
			return spec.VerbArgs{}, err
		}
		switch canon {
		case "Msg":
			out.Msg = v
		case "Player":
			out.Player = v
		case "Reason":
			out.Reason = v
		case "Duration":
			out.Duration = v
		}
	}
	return out, nil
}

func canonArg(k string) (string, bool) {
	for _, n := range ArgNames() {
		if strings.EqualFold(k, n) {
			return n, true
		}
	}
	return "", false
}
