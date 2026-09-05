package spec

import (
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/rcon"
)

// D-07. The manifest's dialect enum and internal/rcon's constants are two
// declarations of one set, in two packages that must not import each other.
// This file exists to fail the moment they drift: a manifest that names a
// dialect the client does not implement would load, validate, pass review, and
// then behave as whatever rcon's default happens to be — silently, and only
// against a live server.

func TestDialectEnumMirrorsRCON(t *testing.T) {
	want := []rcon.Dialect{
		rcon.DialectSource,
		rcon.DialectMinecraft,
		rcon.DialectRust,
		rcon.DialectSimple,
	}
	got := Dialects()
	if len(got) != len(want) {
		t.Fatalf("spec has %d dialects, internal/rcon has %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Errorf("dialect %d: spec %q, rcon %q", i, got[i], want[i])
		}
	}
}

// valve_empty_response is a terminator strategy, not a dialect, and it is the
// one internal/rcon documents as unsafe on Project Zomboid. It must decode --
// so validation can report it with file:line -- and it must never be valid.
func TestValveEmptyResponseIsNotADialect(t *testing.T) {
	var c ControlSpec
	if err := yaml.Unmarshal([]byte("dialect: valve_empty_response\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v; an unknown dialect must reach validation, not die in the decoder", err)
	}
	if c.Dialect.Valid() {
		t.Fatal("valve_empty_response reports itself as a valid dialect")
	}
	for _, d := range Dialects() {
		if d == "valve_empty_response" {
			t.Fatal("valve_empty_response is in the dialect enum")
		}
	}
}

// The refusal has to happen when the file is READ, not when a command is sent.
// A dialect is only exercised on a live server under load, so a bad one that
// survives `gvm game validate` is discovered by a stop that never completes.
func TestValveEmptyResponseIsRefusedWhenTheManifestIsRead(t *testing.T) {
	raw, err := fs.ReadFile(games.FS, "projectzomboid.yaml")
	if err != nil {
		t.Fatalf("read projectzomboid.yaml: %v", err)
	}
	broken := strings.Replace(string(raw), "dialect: simple", "dialect: valve_empty_response", 1)
	if broken == string(raw) {
		t.Fatal("projectzomboid.yaml no longer contains `dialect: simple`")
	}

	_, diags, err := ValidateBytes("games/projectzomboid.yaml", []byte(broken))
	if err != nil {
		t.Fatalf("the file must still decode so the diagnostic can carry a position: %v", err)
	}
	var found Diag
	for _, d := range diags.Errors() {
		if d.Path == "spec.control.dialect" {
			found = d
		}
	}
	if found.Msg == "" {
		t.Fatalf("no error at spec.control.dialect; diagnostics were:\n%s", diags)
	}
	if found.Line == 0 {
		t.Error("the dialect error carries no line; 8.6 requires the manifest's own position")
	}
	if !strings.Contains(found.Msg, "valve_empty_response") ||
		!strings.Contains(found.Msg, "source|minecraft|rust|simple") {
		t.Errorf("message does not name the offender and the legal set: %q", found.Msg)
	}
}

// Project Zomboid is the one server that has to work on day one, and its
// terminator support is unknown. The empty-RESPONSE_VALUE trick risks hangs and
// truncated replies there, so the manifest asks for the idle-terminated reader.
func TestProjectZomboidAsksForTheSimpleDialect(t *testing.T) {
	m, _ := shippedManifest(t, "projectzomboid")
	if m.Spec.Control == nil {
		t.Fatal("projectzomboid declares no control channel")
	}
	if got := m.Spec.Control.Dialect; got != DialectSimple {
		t.Errorf("dialect %q, want %q", got, DialectSimple)
	}
}

// Every shipped manifest that has a control channel names a dialect the client
// implements. This is the check that catches a NEW manifest naming a plausible
// dialect that only exists in its author's head.
func TestEveryShippedDialectIsImplemented(t *testing.T) {
	for _, id := range shippedGames {
		t.Run(id, func(t *testing.T) {
			m, _ := shippedManifest(t, id)
			if m.Spec.Control == nil {
				return
			}
			// rcon.specFor falls back to DialectSource for anything it does not
			// recognise, so an unimplemented name does not fail — it quietly
			// speaks a different protocol. Membership is the whole check.
			if !m.Spec.Control.Dialect.Valid() {
				t.Fatalf("dialect %q is not one of %v", m.Spec.Control.Dialect, Dialects())
			}
		})
	}
}
