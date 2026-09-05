package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// canary is deliberately unlike anything else in this file, so that a test
// asserting its absence cannot be satisfied by a coincidence and a failure
// message shows exactly what leaked.
const canary = "hunter2-CANARY-9f3a1c"

// carrier is the shape that matters most: a secret held in an EXPORTED field
// of an ordinary struct that somebody logs or serialises whole.
type carrier struct {
	Instance string
	RCON     Value
}

// hidden is the shape that matters second most, and the one a string field
// would leak. fmt walks unexported fields by reflection and cannot call
// methods on them.
type hidden struct {
	instance string
	rcon     Value
}

func TestEveryFormatVerbRendersExactlyTheRedaction(t *testing.T) {
	v := New(canary)
	// Every verb, including the ones a developer reaches for when a value is
	// not printing the way they expect -- which is the moment a secret gets
	// logged.
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10s", "%-10v", "%.3s", "%08.3v"}
	for _, f := range verbs {
		if got := fmt.Sprintf(f, v); got != Redacted {
			t.Errorf("fmt.Sprintf(%q, value) = %q, want %q", f, got, Redacted)
		}
		if got := fmt.Sprintf(f, &v); got != Redacted {
			t.Errorf("fmt.Sprintf(%q, &value) = %q, want %q", f, got, Redacted)
		}
	}
}

func TestVerbsFmtHandlesBeforeTheFormatterStillRedact(t *testing.T) {
	v := New(canary)
	// %T and %p never reach Format, and %p is the interesting one.
	//
	// fmt intercepts %p before handleMethods, finds a non-pointer operand,
	// and routes to badVerb -- which sets p.erroring, and handleMethods
	// returns immediately while that flag is set, to avoid recursing through
	// a Formatter that is itself the problem. So the redaction does NOT
	// apply: fmt falls back to walking the struct with reflection, exactly as
	// it does for an unexported field.
	//
	// What stands between `%p` -- a plausible typo for %v on something a
	// developer suspects is a pointer -- and the password is therefore the
	// pointer box alone. That is the second reason Value holds its plaintext
	// behind one, and this assertion is the proof.
	if got := fmt.Sprintf("%T", v); got != "secrets.Value" {
		t.Errorf("%%T = %q, want secrets.Value", got)
	}
	got := fmt.Sprintf("%p", v)
	assertNoCanary(t, "%p", got)
	if !strings.Contains(got, "0x") {
		t.Errorf("%%p = %q; fmt's error path prints the struct reflectively, so this "+
			"should show the box's address. If it shows anything else, check what it "+
			"shows instead -- with a plain string field it shows the password.", got)
	}
}

func TestStringerGoStringerAndMarshalersAllRedact(t *testing.T) {
	v := New(canary)

	if got := v.String(); got != Redacted {
		t.Errorf("String() = %q", got)
	}
	if got := v.GoString(); got != Redacted {
		t.Errorf("GoString() = %q", got)
	}
	if got := v.LogValue().String(); got != Redacted {
		t.Errorf("LogValue() = %q", got)
	}

	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var decoded string
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("MarshalJSON produced %q, which is not a JSON string: %v", b, err)
	}
	if decoded != Redacted {
		t.Errorf("MarshalJSON decoded to %q, want %q", decoded, Redacted)
	}

	txt, err := v.MarshalText()
	if err != nil || string(txt) != Redacted {
		t.Errorf("MarshalText() = %q, %v", txt, err)
	}

	y, err := v.MarshalYAML()
	if err != nil || y != any(Redacted) {
		t.Errorf("MarshalYAML() = %v, %v", y, err)
	}
}

func TestSecretNestedInAStructNeverEscapesThroughAnyEncoder(t *testing.T) {
	c := carrier{Instance: "pz-main", RCON: New(canary)}

	j, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoCanary(t, "json.Marshal(struct)", string(j))
	if !strings.Contains(string(j), Redacted) {
		t.Errorf("json.Marshal(struct) = %s, want the redaction in it", j)
	}

	y, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	assertNoCanary(t, "yaml.Marshal(struct)", string(y))

	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		assertNoCanary(t, "fmt.Sprintf("+f+", struct)", fmt.Sprintf(f, c))
	}
}

func TestSecretInAnUnexportedFieldIsNotWalkedIntoPlaintext(t *testing.T) {
	h := hidden{instance: "pz-main", rcon: New(canary)}

	// This is the case the pointer indirection in Value exists for. fmt
	// reaches an unexported field by reflection, which cannot call Format on
	// it, so the field is printed structurally: a string field would print
	// the password here.
	for _, f := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(f, h)
		assertNoCanary(t, "fmt.Sprintf("+f+", struct with unexported secret)", out)
		if !strings.Contains(out, "0x") {
			t.Errorf("fmt.Sprintf(%q, hidden) = %q; the plaintext is supposed to be behind a "+
				"pointer that reflection prints as an address. If Value now holds the "+
				"string directly, this printed something else and the leak is back.", f, out)
		}
	}

	j, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoCanary(t, "json.Marshal(struct with unexported secret)", string(j))
}

func TestSlogRedactsThroughAllThreeHooks(t *testing.T) {
	v := New(canary)
	c := carrier{Instance: "pz-main", RCON: v}
	h := hidden{instance: "pz-main", rcon: v}

	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	}
	for name, mk := range handlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(mk(&buf))
			// Directly: LogValue. In an exported field: MarshalJSON for the
			// JSON handler, Format for the text one. In an unexported field:
			// neither, which is why the pointer matters.
			log.Info("start", "rcon", v)
			log.Info("start", "creds", c)
			log.Info("start", "creds", h)
			log.Info("start", slog.Any("group", []any{v, c, h}))
			assertNoCanary(t, "slog "+name, buf.String())
			if !strings.Contains(buf.String(), Redacted) {
				t.Errorf("slog %s never emitted the redaction: %s", name, buf.String())
			}
		})
	}
}

func TestWrappedErrorsNeverCarryPlaintext(t *testing.T) {
	v := New(canary)

	// The shape that produces a leak in practice: an error built from a
	// secret, wrapped by its caller, printed by the top of the stack.
	inner := fmt.Errorf("rcon auth rejected password %v", v)
	wrapped := fmt.Errorf("instance %s: %w", "pz-main", inner)
	outer := fmt.Errorf("start: %w", wrapped)

	assertNoCanary(t, "wrapped error", outer.Error())
	assertNoCanary(t, "wrapped error %v", fmt.Sprintf("%v", outer))
	assertNoCanary(t, "wrapped error %+v", fmt.Sprintf("%+v", outer))
	if !errors.Is(outer, inner) {
		t.Error("the wrap chain broke, so this test is no longer about a wrapped error")
	}

	// The neighbouring mistake -- fmt.Errorf("boom: %w", v) on the Value
	// itself -- is not tested here because it does not build: `go vet`'s
	// printf check refuses %w on an operand that is not an error, so that one
	// is caught before it can run. Format would redact it anyway.
}

func TestRevealIsTheOnlyWayOut(t *testing.T) {
	v := New(canary)
	if got := v.Reveal(); got != canary {
		t.Errorf("Reveal() = %q, want the plaintext back", got)
	}
	if v.Len() != len(canary) {
		t.Errorf("Len() = %d, want %d", v.Len(), len(canary))
	}
	if v.Empty() {
		t.Error("Empty() on a set secret")
	}
}

func TestZeroValueIsEmptyAndPrintsTheRedaction(t *testing.T) {
	var v Value
	if !v.Empty() {
		t.Error("the zero Value is not Empty; a caller checking `secret_set` would see an unset secret as set")
	}
	if v.Reveal() != "" {
		t.Errorf("Reveal() on the zero Value = %q, want the empty string and no panic", v.Reveal())
	}
	if v.Len() != 0 {
		t.Errorf("Len() = %d", v.Len())
	}
	if got := fmt.Sprintf("%v", v); got != Redacted {
		t.Errorf("the zero Value formats as %q; an unset secret must look exactly like a set one, "+
			"or the format of a log line tells the reader which instances have no password", got)
	}
}

func TestEqualComparesTheSecretAndNotTheBox(t *testing.T) {
	a, b := New(canary), New(canary)
	if !Equal(a, b) {
		t.Error("Equal on two Values holding the same secret returned false")
	}
	if Equal(a, New(canary+"!")) {
		t.Error("Equal on different secrets returned true")
	}
	if !Equal(Value{}, New("")) {
		t.Error("the zero Value and an explicitly empty one differ")
	}
}

func TestValueIsNotComparableWithEquals(t *testing.T) {
	// == would compare the box pointers, so two Values holding the same
	// password would answer "different" -- silently, in a rotation check.
	// Making the type non-comparable turns that into a compile error and
	// leaves Equal as the only answer.
	if reflect.TypeOf(Value{}).Comparable() {
		t.Fatal("Value became comparable: `a == b` now compiles and answers about pointers, not secrets")
	}
}

func TestDecodingIntoAValueIsRefused(t *testing.T) {
	// The disaster this prevents: marshal a struct (redacted), unmarshal it
	// back, and now the password IS the string "••••" -- which then gets
	// written into the game's ini and into /etc/gvm/secrets by the next
	// rotation, locking the operator out with something that looks in every
	// log exactly like redaction working.
	var v Value
	if err := v.UnmarshalJSON([]byte(`"` + Redacted + `"`)); !errors.Is(err, ErrNoUnmarshal) {
		t.Errorf("UnmarshalJSON = %v, want ErrNoUnmarshal", err)
	}
	if err := v.UnmarshalText([]byte(Redacted)); !errors.Is(err, ErrNoUnmarshal) {
		t.Errorf("UnmarshalText = %v, want ErrNoUnmarshal", err)
	}

	var c carrier
	if err := json.Unmarshal([]byte(`{"Instance":"pz-main","RCON":"`+Redacted+`"}`), &c); err == nil {
		t.Error("json.Unmarshal into a struct carrying a Value succeeded")
	}
	if !c.RCON.Empty() {
		t.Error("a refused decode still populated the secret")
	}

	// yaml has no Unmarshaler here and does not need one: decoding a scalar
	// into a struct whose only fields are unexported already fails. The test
	// exists so that a future yaml version quietly succeeding is caught.
	//
	// The keys are lowercase because yaml.v3 matches a field by its lowercased
	// name and silently ignores a key that matches nothing -- so `RCON:` here
	// would prove only that unknown keys are skipped.
	if err := yaml.Unmarshal([]byte("instance: pz-main\nrcon: "+Redacted+"\n"), &c); err == nil {
		t.Error("yaml.Unmarshal into a struct carrying a Value succeeded")
	}
}

func assertNoCanary(t *testing.T, what, out string) {
	t.Helper()
	if strings.Contains(out, canary) {
		t.Errorf("%s leaked the plaintext: %s", what, out)
	}
}
