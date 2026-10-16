package cli

import (
	"strings"
	"testing"
)

// readVerbs is the authoritative list of read verbs and whether each offers
// --json. It is the walk TestReadVerbsAcceptJSON performs: a read verb a
// reviewer adds has to be classified here, and a `json` one that forgets the
// flag turns the build red.
//
// It is a declared list rather than a reflective walk of the dispatch table
// because "is this a read verb" is a judgement the table does not record, and
// the flag sets are built inside each Run and are not introspectable from here.
// The classification is the contract; the test enforces the `json` half of it.
var readVerbs = []struct {
	// args invokes the verb in a way that gets past flag parsing. The verb may
	// then fail for want of an instance, a database or a Linux host — the test
	// only cares that --json was not rejected as an unknown flag.
	args   []string
	json   bool   // true: must accept --json
	reason string // for an exempt verb, why it has no --json
}{
	{args: []string{"status", "some-instance"}, json: true},
	{args: []string{"list"}, json: true},
	{args: []string{"ports"}, json: true},
	{args: []string{"game", "list"}, json: true},
	{args: []string{"game", "which", "projectzomboid"}, json: true},
	{args: []string{"db", "status"}, json: true},
	{args: []string{"db", "version"}, json: true},
	{args: []string{"backup", "list"}, json: true},
	{args: []string{"job", "list"}, json: true},

	// Exempt, with the reason. logs is a raw byte stream; doctor and the
	// control channel own their output shapes in their own phases (24, later),
	// and adopt discover / restore --list are partial verbs whose JSON lands
	// with the migration and restore work.
	{args: []string{"logs", "x"}, json: false, reason: "a raw console byte stream, not a record"},
	{args: []string{"doctor"}, json: false, reason: "doctor's --json lands with Phase 24"},
}

func TestReadVerbsAcceptJSON(t *testing.T) {
	for _, v := range readVerbs {
		name := strings.Join(v.args, "-")
		t.Run(name, func(t *testing.T) {
			if !v.json {
				if v.reason == "" {
					t.Errorf("%s is exempt from --json but records no reason", name)
				}
				return
			}
			h := newTestHost(t)
			_, out, errb := runWith(t, h, "", append(append([]string{}, v.args...), "--json")...)
			if strings.Contains(out+errb, "flag provided but not defined") {
				t.Errorf("`gvm %s --json` rejected --json as an unknown flag:\n%s",
					strings.Join(v.args, " "), out+errb)
			}
		})
	}
}
