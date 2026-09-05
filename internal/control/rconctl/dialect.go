package rconctl

import (
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/rcon"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// D-07, at the one place the two enums meet.
//
// spec.Dialect and rcon.Dialect are two declarations of one set in two packages
// that must not import each other: the manifest owns the name an operator
// types, the client owns the behaviour. This table is the only conversion, and
// it is an explicit map with an error rather than the obvious
// `rcon.Dialect(string(d))`.
//
// The conversion by cast is what makes this worth a file. rcon.specFor falls
// back to DialectSource for any name it does not recognise -- so a typo'd or
// future dialect would not fail; it would silently speak Valve's protocol to a
// server that does not implement it, and the symptom is a `save` whose reply
// never terminates during a stop. A map lookup that returns an error turns that
// into a refusal at construction time, before a world is at stake.
var toRCON = map[spec.Dialect]rcon.Dialect{
	spec.DialectSource:    rcon.DialectSource,
	spec.DialectMinecraft: rcon.DialectMinecraft,
	spec.DialectRust:      rcon.DialectRust,
	spec.DialectSimple:    rcon.DialectSimple,
}

// fromRCON is built from toRCON rather than written out, so the two directions
// cannot disagree: a hand-written inverse is a table that looks correct and is
// wrong in exactly one row.
var fromRCON = func() map[rcon.Dialect]spec.Dialect {
	m := make(map[rcon.Dialect]spec.Dialect, len(toRCON))
	for s, r := range toRCON {
		m[r] = s
	}
	return m
}()

// ErrUnknownDialect names a dialect this build does not implement.
type ErrUnknownDialect struct{ Name string }

func (e *ErrUnknownDialect) Error() string {
	return fmt.Sprintf("rconctl: %q is not a dialect (want %s)", e.Name, strings.Join(specNames(), "|"))
}

func specNames() []string {
	out := make([]string, 0, len(spec.Dialects()))
	for _, d := range spec.Dialects() {
		out = append(out, string(d))
	}
	return out
}

// Dialect converts a manifest dialect to the client's. An empty dialect is
// DialectSource, matching rcon.Options' own zero value: a manifest that omits
// the field gets the same behaviour as one that spells out the default.
func Dialect(d spec.Dialect) (rcon.Dialect, error) {
	if d == "" {
		return rcon.DialectSource, nil
	}
	r, ok := toRCON[d]
	if !ok {
		return "", &ErrUnknownDialect{Name: string(d)}
	}
	return r, nil
}

// SpecDialect converts back, for a status block or a probe report that has a
// client and needs the name an operator would recognise from the manifest.
func SpecDialect(d rcon.Dialect) (spec.Dialect, error) {
	s, ok := fromRCON[d]
	if !ok {
		return "", &ErrUnknownDialect{Name: string(d)}
	}
	return s, nil
}
