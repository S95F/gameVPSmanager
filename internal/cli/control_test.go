package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestParseVerbArgs(t *testing.T) {
	t.Run("keys are the closed operand set", func(t *testing.T) {
		got, err := parseVerbArgs([]string{"Msg=restart in 5", "Player=alice"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got["Msg"] != "restart in 5" || got["Player"] != "alice" {
			t.Errorf("args = %v", got)
		}
	})

	// A broadcast containing an equals sign is ordinary text. Splitting on the
	// last '=' would silently truncate the message an operator sent to a full
	// server.
	t.Run("only the first equals separates", func(t *testing.T) {
		got, err := parseVerbArgs([]string{"Msg=ratio=2:1 tonight"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got["Msg"] != "ratio=2:1 tonight" {
			t.Errorf("Msg = %q", got["Msg"])
		}
	})

	for _, tc := range []struct{ name, arg, want string }{
		{"not a pair", "broadcast", "Key=value"},
		{"unknown operand", "Message=hi", "Message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseVerbArgs([]string{tc.arg})
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// Which spelling won would otherwise depend on map iteration order, and a
	// broadcast that sends one of two messages at random is worse than a
	// refusal.
	t.Run("one operand under two spellings is refused", func(t *testing.T) {
		if _, err := parseVerbArgs([]string{"Msg=a", "msg=b"}); err == nil {
			t.Fatal("accepted the same operand twice")
		}
	})
}

// The three-state capability table of section 8.5 must print as three different
// words. "supported: true, available: false" is a shape an operator has to
// decode; "not yet" plus the manifest's own sentence is one they can act on.
func TestPrintCapsDistinguishesNotYetFromNo(t *testing.T) {
	var out bytes.Buffer
	printCaps(&out, []spec.CapState{
		{Cap: spec.CapConsoleWrite, Supported: true, Available: true, Channel: "console"},
		{Cap: spec.CapControl, Supported: true, Channel: "control",
			Reason: "RCON does not bind until Project Zomboid finishes booting"},
		{Cap: spec.CapEULA},
	})
	got := out.String()
	for _, want := range []string{
		"console.write", "yes",
		"not yet", "RCON does not bind until Project Zomboid finishes booting",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table never says %q:\n%s", want, got)
		}
	}
	// The manifest's sentence is rendered verbatim; rewording it here would be
	// the CLI inventing product copy.
	if strings.Contains(got, "not available") {
		t.Errorf("the reason was reworded:\n%s", got)
	}
}

func TestPrintControlStatusIsReadableForAChannelThatIsDown(t *testing.T) {
	var out bytes.Buffer
	printControlStatus(&out, "pz-main", "booting", control.Status{
		Kind:                spec.ControlSourceRCON,
		Dialect:             "simple",
		Ready:               false,
		LastError:           "dial tcp 127.0.0.1:27015: connection refused",
		ConsecutiveFailures: 3,
	})
	got := out.String()
	for _, want := range []string{"source_rcon", "simple", "connection refused", "gvm control caps pz-main"} {
		if !strings.Contains(got, want) {
			t.Errorf("status never says %q:\n%s", want, got)
		}
	}
}

// A control channel that has never succeeded must not print a zero timestamp:
// "0001-01-01" reads as a clock problem, and an operator will chase it.
func TestPrintControlStatusOmitsANeverTimestamp(t *testing.T) {
	var out bytes.Buffer
	printControlStatus(&out, "valheim-1", "ready", control.Status{Kind: spec.ControlNone})
	if strings.Contains(out.String(), "0001") {
		t.Errorf("a zero last_ok was printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "none") {
		t.Errorf("a console-only game did not report kind none:\n%s", out.String())
	}
}

func TestPrintControlStatusKeepsALastOKItHas(t *testing.T) {
	var out bytes.Buffer
	printControlStatus(&out, "pz-main", "ready", control.Status{
		Kind: spec.ControlSourceRCON, Ready: true,
		LastOK: time.Date(2026, 9, 4, 19, 2, 3, 0, time.UTC),
	})
	if !strings.Contains(out.String(), "2026-09-04 19:02:03Z") {
		t.Errorf("last_ok missing:\n%s", out.String())
	}
}

// `gvm control probe` is documented in section 11.9 and an operator will type
// it. Saying "not built, and here is why" is what keeps them from concluding
// the dialect in the manifest was verified.
func TestControlProbeIsHonestlyUnbuilt(t *testing.T) {
	env := &Env{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}
	err := runControlProbe(context.Background(), env, []string{"pz-main"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "mirror probe") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// The unbuilt sentence now carries the detail a half-built subcommand supplies,
// while a bare stub still reads the way every script and every phase check
// expects.
func TestNotImplementedDetailReachesStderr(t *testing.T) {
	code, _, errb := run(t, "control", "probe", "pz-main")
	if code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if !strings.Contains(errb, "not implemented yet") {
		t.Errorf("stderr = %q", errb)
	}
	if !strings.Contains(errb, "mirror probe") {
		t.Errorf("the detail was swallowed: %q", errb)
	}
}

func TestControlUsageListsEverySubcommand(t *testing.T) {
	var out bytes.Buffer
	controlUsage(&out)
	for _, s := range controlSubs {
		if !strings.Contains(out.String(), "gvm control "+s.name) {
			t.Errorf("usage omits %q:\n%s", s.name, out.String())
		}
	}
}

func TestControlRejectsAnUnknownSubcommand(t *testing.T) {
	code, _, errb := run(t, "control", "frobnicate")
	if code == 0 {
		t.Error("exit 0 for an unknown subcommand")
	}
	if !strings.Contains(errb, "unknown subcommand") {
		t.Errorf("stderr = %q", errb)
	}
}
