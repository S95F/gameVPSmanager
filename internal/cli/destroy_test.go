package cli

import (
	"errors"
	"strings"
	"testing"
)

// rebuild.sh's contract, as a table.
//
// Every row here is a way somebody could arrive at a destructive action
// without having decided to. The check is deliberately unforgiving: no
// trimming, no case folding, no prefix, no "y". The whole value of typing a
// server's name is that it is hard to produce by accident and impossible to
// produce by muscle memory.
func TestTypedConfirmIsExact(t *testing.T) {
	c := TypedConfirm{Subject: "pz-main"}
	for _, tc := range []struct {
		typed string
		ok    bool
		why   string
	}{
		{typed: "pz-main", ok: true, why: "the name, exactly"},
		{typed: "", why: "an empty answer, which is what a bare Enter produces"},
		{typed: "y", why: "the habit a y/n prompt teaches"},
		{typed: "yes", why: "the other habit"},
		{typed: "PZ-MAIN", why: "the right name in the wrong case"},
		{typed: "pz-main ", why: "a trailing space, from a paste"},
		{typed: " pz-main", why: "a leading space"},
		{typed: "pz", why: "a prefix"},
		{typed: "pz-main2", why: "a different server whose name starts the same"},
	} {
		err := c.Check(tc.typed, true)
		if tc.ok && err != nil {
			t.Errorf("%s (%s) was refused: %v", tc.typed, tc.why, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q was accepted (%s)", tc.typed, tc.why)
		}
		if !tc.ok && err != nil && !errors.Is(err, ErrNotConfirmed) {
			t.Errorf("%q: error does not wrap ErrNotConfirmed: %v", tc.typed, err)
		}
	}
}

// The archive acknowledgement is a SEPARATE field, and a caller that supplies
// the name and not the ack is refused. They answer different questions --
// "which server" and "yes, I understand the world is being replaced" -- and
// collapsing them into one would let the first stand in for the second.
func TestTypedConfirmNeedsTheArchiveAcknowledgement(t *testing.T) {
	c := TypedConfirm{Subject: "pz-main", ArchiveAck: true}
	if err := c.Check("pz-main", false); err == nil {
		t.Error("the typed name alone satisfied a confirmation that also wanted the archive ack")
	}
	if err := c.Check("pz-main", true); err != nil {
		t.Errorf("both given and still refused: %v", err)
	}
}

// An empty Subject is a refusal rather than a free pass. A confirmation with
// nothing to compare against is a bug in the caller, and the fail-closed
// reading is the only safe one.
func TestTypedConfirmWithNoSubjectRefuses(t *testing.T) {
	if err := (TypedConfirm{}).Check("", true); err == nil {
		t.Error("a confirmation with no subject accepted an empty answer")
	}
}

// The prompt reads ONE line and does not loop: a prompt that re-asks is a
// prompt an operator answers by holding a key down.
func TestTypedConfirmPromptReadsOneLine(t *testing.T) {
	var out strings.Builder
	ok, err := TypedConfirm{Subject: "pz-main"}.Prompt(strings.NewReader("pz-main\n"), &out)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out.String(), `type "pz-main" to confirm`) {
		t.Errorf("the prompt does not say what to type: %q", out.String())
	}

	out.Reset()
	ok, err = TypedConfirm{Subject: "pz-main"}.Prompt(strings.NewReader("pz-mian\npz-main\n"), &out)
	if ok || err == nil {
		t.Error("a wrong first answer was rescued by a correct second line")
	}
}

// The flag and the prompt go through the same Check, so the scriptable route
// is not weaker than the interactive one.
func TestConfirmFromUsesTheSameCheckForFlagAndPrompt(t *testing.T) {
	env := &Env{Stdin: strings.NewReader(""), Stdout: &strings.Builder{}}
	c := TypedConfirm{Subject: "pz-main", ArchiveAck: true}
	if err := c.confirmFrom(env, "PZ-MAIN"); err == nil {
		t.Error("--confirm accepted the wrong case where the prompt would not")
	}
	if err := c.confirmFrom(env, "pz-main"); err != nil {
		t.Errorf("--confirm with the right name was refused: %v", err)
	}
}

// --archive is not optional and the refusal says why, because the operator's
// next move ("run gvm backup") is not derivable from "missing flag".
func TestDestroyRefusesWithoutAnArchive(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	before := len(h.FS.writes)

	code, _, errb := runWith(t, h, "", "destroy", "valheim-1")
	if code == 0 {
		t.Fatal("destroy ran with no archive")
	}
	if !strings.Contains(errb, "gvm backup") {
		t.Errorf("the refusal does not name what would produce one:\n%s", errb)
	}
	if len(h.FS.writes) != before {
		t.Errorf("a refused destroy wrote %v", h.FS.writes[before:])
	}
}

// An archive gvm cannot find, or cannot verify, authorises nothing -- and the
// instance definition is still there afterwards.
func TestDestroyRefusesAnUnverifiableArchive(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, _, errb := runWith(t, h, "valheim-1\n",
		"destroy", "valheim-1", "--archive", t.TempDir()+"/nope.tar.gz")
	if code == 0 {
		t.Fatal("destroy accepted an archive that is not there")
	}
	if strings.Contains(errb, "removed") {
		t.Errorf("something was removed: %s", errb)
	}
	mustReadInstance(t, h, "valheim-1") // still parseable, so still there
}
