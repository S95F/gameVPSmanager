package rconctl

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/rcon"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// D-07. internal/spec's dialect_sync_test asserts the two ENUMS name the same
// set; this asserts the CONVERSION between them is total in both directions.
// Both are needed: a set that matches and a table that is missing a row gives a
// manifest that validates and a client that refuses to start.
func TestEveryRCONDialectRoundTrips(t *testing.T) {
	// Written out rather than derived, for the same reason spec's test writes
	// them out: a list built from the table under test cannot catch the table
	// being wrong.
	all := []rcon.Dialect{
		rcon.DialectSource,
		rcon.DialectMinecraft,
		rcon.DialectRust,
		rcon.DialectSimple,
	}
	if len(all) != len(spec.Dialects()) {
		t.Fatalf("this test knows %d dialects, spec declares %d", len(all), len(spec.Dialects()))
	}
	if len(toRCON) != len(all) {
		t.Fatalf("the conversion table has %d rows for %d dialects", len(toRCON), len(all))
	}

	for _, d := range all {
		s, err := SpecDialect(d)
		if err != nil {
			t.Errorf("%s has no manifest spelling: %v", d, err)
			continue
		}
		back, err := Dialect(s)
		if err != nil {
			t.Errorf("%s -> %s -> error: %v", d, s, err)
			continue
		}
		if back != d {
			t.Errorf("%s round-tripped to %s", d, back)
		}
	}
}

func TestEverySpecDialectConverts(t *testing.T) {
	for _, s := range spec.Dialects() {
		d, err := Dialect(s)
		if err != nil {
			t.Errorf("manifest dialect %q does not convert: %v", s, err)
			continue
		}
		if string(d) != string(s) {
			t.Errorf("%q converts to %q; the two spellings are meant to be the same word", s, d)
		}
	}
}

// The reason this file exists rather than a cast. rcon.specFor falls back to
// DialectSource for anything it does not recognise, so a cast would turn an
// unknown name into "speak Valve's protocol" -- and the symptom is a `save`
// during a stop whose reply never terminates.
func TestAnUnknownDialectIsRefusedRatherThanDefaulted(t *testing.T) {
	for _, name := range []string{"valve_empty_response", "srcds", "SIMPLE", "telnet"} {
		got, err := Dialect(spec.Dialect(name))
		if err == nil {
			t.Errorf("%q converted to %q instead of failing", name, got)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name the offender: %v", err)
		}
		if !strings.Contains(err.Error(), "simple") {
			t.Errorf("error does not list the legal set: %v", err)
		}
	}
}

// An omitted dialect is Source, matching rcon.Options' own zero value: a
// manifest that leaves the field out must behave identically to one that spells
// out the default, or the default becomes a second, undocumented dialect.
func TestEmptyDialectIsSource(t *testing.T) {
	d, err := Dialect("")
	if err != nil {
		t.Fatalf("empty dialect: %v", err)
	}
	if d != rcon.DialectSource {
		t.Fatalf("empty dialect is %q, want %q", d, rcon.DialectSource)
	}
}

// Project Zomboid is the game that has to work on day one and its terminator
// support is unknown, so the client it gets must be the idle-terminated one.
func TestProjectZomboidGetsTheSimpleDialect(t *testing.T) {
	d, err := Dialect(spec.DialectSimple)
	if err != nil {
		t.Fatal(err)
	}
	if d != rcon.DialectSimple {
		t.Fatalf("manifest `simple` builds a %q client", d)
	}
}
