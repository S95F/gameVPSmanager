package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// TestTheTypedConfirmationIsRebuildShsPrompt is section 16.4, row by row.
//
// rebuild.sh asks the operator to type the server name and compares it with
// `=`. Everything below is that comparison plus the two things an interface
// adds: automation that has to be able to say "a human already decided", and a
// transport over which such a claim cannot be trusted.
func TestTheTypedConfirmationIsRebuildShsPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *Confirmation
		trig Trigger
		ok   bool
		says string
	}{
		{name: "the name typed", c: &Confirmation{Typed: testInstance}, trig: TriggerCLI, ok: true},
		{
			name: "the name with the shell's whitespace",
			c:    &Confirmation{Typed: "  " + testInstance + "\n"}, trig: TriggerCLI, ok: true,
		},
		{
			// The whole point of typing the name: "y" must not work, and
			// neither must anything else a hand reaches for by reflex.
			name: "y", c: &Confirmation{Typed: "y"}, trig: TriggerCLI,
			says: "\"y\" is not \"" + testInstance + "\"",
		},
		{
			name: "another instance's name",
			c:    &Confirmation{Typed: "pz-test"}, trig: TriggerCLI,
			says: "is not",
		},
		{
			// Case folding would make "PZ-MAIN" acceptable, and an answer a
			// shell's history completion can produce is not a confirmation.
			name: "the name in upper case",
			c:    &Confirmation{Typed: strings.ToUpper(testInstance)}, trig: TriggerCLI,
			says: "is not",
		},
		{name: "nothing", c: &Confirmation{}, trig: TriggerCLI, says: "type the instance name"},
		{name: "no confirmation at all", c: nil, trig: TriggerCLI, says: "was not confirmed"},
		{name: "--yes from a root shell", c: &Confirmation{Automated: true}, trig: TriggerCLI, ok: true},
		{name: "--yes from a timer", c: &Confirmation{Automated: true}, trig: TriggerTimer, ok: true},
		{
			// Section 16.4's own threat model: the attacker can send a form,
			// so a boolean in a body proves nothing.
			name: "--yes over the API", c: &Confirmation{Automated: true}, trig: TriggerAPI,
			says: "section 16.4",
		},
		{
			name: "--yes from the dashboard", c: &Confirmation{Automated: true}, trig: TriggerWeb,
			says: "section 16.4",
		},
		{
			// A typed name is acceptable over any transport: it is the thing
			// section 16.4 asks the execute call to carry.
			name: "the name over the API", c: &Confirmation{Typed: testInstance}, trig: TriggerAPI,
			ok: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Check(testInstance, lifecycle.JobRebuild, tc.trig)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("refused a valid confirmation: %v", err)
			case !tc.ok && err == nil:
				t.Fatal("accepted it")
			case err == nil:
				return
			}
			if !errors.Is(err, ErrNotConfirmed) {
				t.Errorf("err = %v, want ErrNotConfirmed", err)
			}
			if tc.says != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.says)) {
				t.Errorf("err = %q, want it to say %q", err, tc.says)
			}
			// Every refusal states the consequence. A prompt that says only
			// "not confirmed" teaches the operator to type the name without
			// reading, which is the habit the prompt exists to prevent.
			if !strings.Contains(err.Error(), "DELETES") {
				t.Errorf("err = %q; it does not say what a rebuild destroys", err)
			}
		})
	}
}

// TestOnlyDestructiveKindsNeedConfirming, in both directions.
//
// A nightly backup that demanded a typed name would be a nightly nobody
// schedules; a rebuild that did not would be rebuild.sh with the prompt
// deleted.
func TestOnlyDestructiveKindsNeedConfirming(t *testing.T) {
	want := map[lifecycle.JobKind]bool{
		lifecycle.JobRebuild: true,
		lifecycle.JobRestore: true,
	}
	for _, k := range lifecycle.JobKinds() {
		if got := Destructive(k); got != want[k] {
			t.Errorf("Destructive(%s) = %v, want %v", k, got, want[k])
		}
		if Destructive(k) && RiskOf(k) == "" {
			t.Errorf("%s is destructive and says nothing about what it destroys", k)
		}
		if !Destructive(k) && RiskOf(k) != "" {
			t.Errorf("%s is not destructive and carries a risk sentence: %q", k, RiskOf(k))
		}
		// A kind that destroys nothing accepts no confirmation and needs
		// none, whatever the caller supplied.
		if !Destructive(k) {
			if err := (*Confirmation)(nil).Check(testInstance, k, TriggerAPI); err != nil {
				t.Errorf("%s refused to run without a confirmation: %v", k, err)
			}
		}
	}
	if len(DestructiveKinds()) != len(want) {
		t.Errorf("DestructiveKinds() = %v, want %d of them", DestructiveKinds(), len(want))
	}
}

// TestADestructiveKindNeverRunsWithoutAConfirmation is the structural half:
// the check is in the ENGINE, not in the caller.
//
// Section 16.4's sentence is "server-side, applies identically to browser, API
// and CLI". That is only true if the refusal happens somewhere all three go
// through, and the only such place is Execute. The assertion is therefore not
// "Check refuses" -- that is the test above -- but "a job whose confirmation is
// missing does not reach the lock, the lease or the step table".
func TestADestructiveKindNeverRunsWithoutAConfirmation(t *testing.T) {
	for _, kind := range DestructiveKinds() {
		if _, err := Lookup(kind); err != nil {
			// A kind whose runner is still pending cannot be driven here. The
			// confirmation is checked in validate, which runs after Lookup,
			// so there is nothing to assert until the runner lands -- and
			// slug_test.go is what makes sure the gap is tracked.
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			rig := newRig(t, kind, testInstance)
			rig.env.Confirm = nil

			jr, err := Execute(context.Background(), rig.env)
			if err == nil {
				t.Fatal("a destructive job with no confirmation ran")
			}
			if !errors.Is(err, ErrNotConfirmed) {
				t.Errorf("err = %v, want ErrNotConfirmed", err)
			}
			if jr != nil {
				t.Errorf("a refused confirmation produced a run record: %+v", jr)
			}
			if rig.locker.released != 0 || len(rig.locker.paths) != 0 {
				t.Errorf("the job lock was taken for a refused confirmation: %v",
					rig.locker.paths)
			}
			if stops, starts := rig.super.counts(); stops != 0 || starts != 0 {
				t.Errorf("a refused confirmation stopped %d and started %d", stops, starts)
			}
			if rig.host.wrote(hostops.FileLease) != 0 {
				t.Error("a refused confirmation took the lease")
			}
			if !rig.super.isActive() {
				t.Error("a refused confirmation left the server stopped")
			}
		})
	}
}

// TestThePromptSaysWhatItWillDestroy: the sentence an operator agrees to and
// the sentence quoted back at them on a mismatch are the same string.
func TestThePromptSaysWhatItWillDestroy(t *testing.T) {
	c := &Confirmation{}
	p := c.Prompt(testInstance, lifecycle.JobRebuild)
	if !strings.Contains(p, RiskOf(lifecycle.JobRebuild)) {
		t.Errorf("prompt = %q; it does not carry the risk sentence", p)
	}
	if !strings.Contains(p, testInstance) {
		t.Errorf("prompt = %q; it does not name what to type", p)
	}
	// An override narrows it: `gvm restore --to <dir>` touches no live world
	// and has to be able to say so rather than threaten one.
	c.Risk = "this only extracts into a scratch directory"
	if p := c.Prompt(testInstance, lifecycle.JobRestore); strings.Contains(p, "overwrites") {
		t.Errorf("prompt = %q; the caller's own risk sentence was ignored", p)
	}
}
