// Package secrets holds the three things gvm needs in order to handle a
// credential without ever printing one: a value type whose every rendering
// path is redacted, the /etc/gvm/secrets file store that is the truth for
// every game secret (D-02, D-28), and the generator that mints new ones.
//
// Game secrets are files rather than database columns because they must be
// written in cleartext into the game's own config file for the game to work at
// all (`RCONPassword=` in a Project Zomboid ini). Encrypting them into
// Postgres would defend against nothing the filesystem does not already
// expose, while making a running server depend on a database (D-28). What that
// buys is worth stating: `pg_dump` contains zero game secrets.
package secrets

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is what every rendering path on a Value produces.
//
// A fixed four bullets rather than one per character: a mask that tracks the
// length hands a reader the password's length for free, and length is most of
// what a cracking run needs to know before it starts.
const Redacted = "••••"

// ErrNoUnmarshal is returned by every decoding path on a Value.
//
// This looks pedantic and is not. Without it, a struct carrying a Value that
// is marshalled (yielding "••••") and later unmarshalled comes back holding
// the literal string "••••" as the password — and the next rotation writes
// that into the game's ini and into /etc/gvm/secrets, locking every operator
// out of their own server with a value that looks, in every log and every
// dashboard, exactly like a redaction working correctly.
var ErrNoUnmarshal = errors.New("secrets: a Value cannot be decoded from a document; secrets live in /etc/gvm/secrets (D-02), and a serialized one is a redaction, not a password")

// Value is a secret that cannot be printed by accident.
//
// Every path fmt, encoding/json, gopkg.in/yaml.v3 and log/slog can reach —
// String, GoString, Format, MarshalJSON, MarshalText, MarshalYAML, LogValue —
// renders Redacted. The plaintext leaves through exactly one method, Reveal,
// which is deliberately ugly so that every real call site is one search away.
// reveal_test.go counts them.
//
// The plaintext sits behind a pointer instead of in a string field for a
// reason that is easy to miss and expensive to discover: fmt walks a struct's
// UNEXPORTED fields by reflection and cannot call methods on them, so a Value
// held in an unexported field of some other struct is printed field by field
// with no method in sight. A string field prints the password there. A pointer
// prints an address, because at depth > 0 fmt does not follow pointers.
//
// Value is deliberately not comparable. == would compare box pointers, so two
// Values holding the same password would answer "different" — a silent wrong
// answer in precisely the place the question gets asked (did the rotation
// change anything?). Equal is the answer, and it is constant-time.
type Value struct {
	_ [0]func()
	p *boxed
}

// boxed is the indirection described above. It is a named type rather than a
// *string so that a reflective printer that reaches it reports
// "*secrets.boxed" and an address, which reads as a deliberate wall.
type boxed struct{ s string }

// New wraps a plaintext secret.
func New(s string) Value { return Value{p: &boxed{s: s}} }

// Reveal returns the plaintext. It is the only way out.
//
// Call it as late as possible and never into a variable that outlives the
// statement: the whole guarantee of this type ends at this call, and a
// `pw := v.Reveal()` twenty lines above the use is a plain string that some
// future error message will interpolate.
func (v Value) Reveal() string { return v.plain() }

// plain is the in-package accessor. The store writing a value to its own 0600
// file is not a leak and must not spend one of Reveal's countable call sites:
// if it did, the budget in reveal_test.go would have to grow to accommodate
// the one caller that is definitionally safe.
func (v Value) plain() string {
	if v.p == nil {
		return ""
	}
	return v.p.s
}

// Empty reports whether the secret is unset or the empty string. It is the
// question `when: {secret_set: join_password}` asks, and it can be answered
// without revealing anything.
func (v Value) Empty() bool { return v.plain() == "" }

// Len is the secret's length in bytes.
//
// It leaks the length, deliberately and narrowly: the Valheim validator has to
// refuse a join password under five characters (section 8.6 check 13), and the
// alternative is handing that validator the plaintext.
func (v Value) Len() int { return len(v.plain()) }

// Equal compares two secrets in constant time.
//
// Constant time matters here even though both operands are usually local: this
// is what a rotation check and a "did the operator retype it correctly" check
// will use, and one of those runs against attacker-supplied input the first
// time somebody wires a form to it.
func Equal(a, b Value) bool {
	return subtle.ConstantTimeCompare([]byte(a.plain()), []byte(b.plain())) == 1
}

// String satisfies fmt.Stringer with the redaction. Format takes precedence
// for every fmt verb; this exists for the direct call and for the interface
// assertion a template engine or a logger might make.
func (v Value) String() string { return Redacted }

// GoString redacts %#v as well. A Go-syntax rendering of a password is still a
// password.
func (v Value) GoString() string { return Redacted }

// Format renders Redacted for every verb, ignoring width, precision and flags.
//
// Uniformity is the point. A per-verb rendering — %q quoted, %#v in Go syntax,
// %x hexadecimal — would mean a reviewer has to know which verbs are safe
// before they can approve a log line, and the entire value of this type is
// that no verb needs checking.
func (v Value) Format(f fmt.State, verb rune) {
	io.WriteString(f, Redacted)
}

// LogValue is log/slog's redaction hook. It fires when a Value is logged
// directly; a Value inside a struct is reached instead through MarshalJSON
// (slog.JSONHandler) or Format (slog.TextHandler), which is why all three
// exist rather than just this one.
func (v Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// redactedJSON is Redacted as a JSON string. The bullets need no escaping, so
// the literal is the encoding.
const redactedJSON = `"` + Redacted + `"`

// MarshalJSON redacts. Returning a fresh slice each call keeps a caller that
// mutates the returned buffer — encoding/json does not, but a hand-rolled
// encoder might — from editing every future redaction on the box.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(redactedJSON), nil }

// MarshalText redacts. It is what a URL encoder, a text template and any
// encoding/* package that prefers TextMarshaler will reach for.
func (v Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalYAML redacts. gvm writes no YAML containing a secret (D-02 puts them
// in their own files), but `gvm show --format yaml` renders instance state
// that may carry one, and this is what makes that safe by construction rather
// than by the renderer remembering.
func (v Value) MarshalYAML() (any, error) { return Redacted, nil }

// UnmarshalJSON refuses. See ErrNoUnmarshal.
func (v *Value) UnmarshalJSON([]byte) error { return ErrNoUnmarshal }

// UnmarshalText refuses. See ErrNoUnmarshal.
func (v *Value) UnmarshalText([]byte) error { return ErrNoUnmarshal }
