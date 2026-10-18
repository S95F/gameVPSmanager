package cli

import (
	"strings"
	"testing"
)

func TestUserErrorShape(t *testing.T) {
	if got := (&userError{Msg: "a happened"}).Error(); got != "a happened" {
		t.Errorf("no-hint = %q", got)
	}
	if got := (&userError{Msg: "a happened", Hint: "do b"}).Error(); got != "a happened. do b" {
		t.Errorf("with-hint = %q", got)
	}
}

// TestRenderErrorShowsRefOnlyUnderDebug: the Ref is for a developer reading a
// bug report, so it appears only when GVM_DEBUG is set.
func TestRenderErrorShowsRefOnlyUnderDebug(t *testing.T) {
	ue := &userError{Msg: "a happened", Hint: "do b", Ref: "gvm help faq"}

	off := &Env{Getenv: func(string) string { return "" }}
	if got := renderError(off, ue); strings.Contains(got, "gvm help faq") {
		t.Errorf("Ref leaked without GVM_DEBUG: %q", got)
	}
	on := &Env{Getenv: func(k string) string {
		if k == "GVM_DEBUG" {
			return "1"
		}
		return ""
	}}
	if got := renderError(on, ue); !strings.Contains(got, "gvm help faq") {
		t.Errorf("Ref missing under GVM_DEBUG: %q", got)
	}
}

// TestRequiredArgIsUsageErrorWithHint: a missing required flag is a usage error
// (exit 2) and reads "what happened. what to do next".
func TestRequiredArgIsUsageErrorWithHint(t *testing.T) {
	h := newTestHost(t)
	code, _, errb := runWith(t, h, "", "create", "valheim-1")
	if code != exitUsage {
		t.Errorf("missing --game exited %d, want %d (usage)", code, exitUsage)
	}
	if !strings.Contains(errb, "--game is required") || !strings.Contains(errb, "gvm game list") {
		t.Errorf("error did not read 'what happened. what to do next':\n%s", errb)
	}
}
