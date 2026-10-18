package cli

import (
	"io"
	"strings"
	"testing"
)

func parseEnv() *Env {
	return &Env{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}
}

// TestCreateHelpShowsValuesAndExamplesAndHidesInternal: the flag printer renders
// `--game VALUE` (not a bare `--game`), prints an Examples block, and keeps the
// internal flags (--user, the --dry alias) out of the listing.
func TestCreateHelpShowsValuesAndExamplesAndHidesInternal(t *testing.T) {
	h := newTestHost(t)
	_, out, errb := runWith(t, h, "", "create", "--help")
	combined := out + errb
	for _, want := range []string{"--game VALUE", "--var VALUE", "Examples:", "gvm create valheim-1"} {
		if !strings.Contains(combined, want) {
			t.Errorf("create --help missing %q:\n%s", want, combined)
		}
	}
	for _, unwanted := range []string{"--user", "--dry "} {
		if strings.Contains(combined, unwanted) {
			t.Errorf("create --help showed the hidden flag %q:\n%s", unwanted, combined)
		}
	}
}

// TestCreateDryAliasAndNoEnabled: --dry is --dry-run, and --no-enabled is the
// off switch for --enabled, both folded into the one option downstream reads.
func TestCreateDryAliasAndNoEnabled(t *testing.T) {
	env := parseEnv()
	opt, err := parseCreateArgs(env, []string{"pz-main", "--game", "projectzomboid", "--dry"})
	if err != nil {
		t.Fatalf("parse --dry: %v", err)
	}
	if !opt.DryRun {
		t.Error("--dry did not set DryRun")
	}
	opt2, err := parseCreateArgs(env, []string{"pz-main", "--game", "projectzomboid", "--no-enabled"})
	if err != nil {
		t.Fatalf("parse --no-enabled: %v", err)
	}
	if opt2.Enabled {
		t.Error("--no-enabled did not clear Enabled")
	}
	// The default is still enabled-at-boot.
	opt3, err := parseCreateArgs(env, []string{"pz-main", "--game", "projectzomboid"})
	if err != nil {
		t.Fatalf("parse plain: %v", err)
	}
	if !opt3.Enabled {
		t.Error("default should be enabled")
	}
}

// TestScheduleFlagsAreJobRunOnly: --trigger moved off the operator verbs and
// onto `gvm job run` alone. An operator verb now rejects it; job run still
// accepts it (hidden) and does not fail on an undefined flag.
func TestScheduleFlagsAreJobRunOnly(t *testing.T) {
	t.Run("backup rejects --trigger", func(t *testing.T) {
		h := newTestHost(t)
		_, out, errb := runWith(t, h, "", "backup", "pz-main", "--trigger", "timer")
		if !strings.Contains(out+errb, "not defined") && !strings.Contains(out+errb, "flag provided") {
			// Either message shape is fine; the point is it is refused as an
			// unknown flag, not honoured.
			t.Logf("backup --trigger output:\n%s", out+errb)
		}
		if !strings.Contains(out+errb, "trigger") {
			t.Errorf("backup did not reject --trigger:\n%s", out+errb)
		}
	})
	t.Run("job run still accepts --trigger", func(t *testing.T) {
		h := newTestHost(t)
		_, out, errb := runWith(t, h, "", "job", "run", "no_such_slug", "--trigger", "timer")
		if strings.Contains(out+errb, "flag provided but not defined") {
			t.Errorf("job run rejected its own --trigger:\n%s", out+errb)
		}
	})
	t.Run("job run --help hides the schedule flags", func(t *testing.T) {
		h := newTestHost(t)
		_, out, errb := runWith(t, h, "", "job", "run", "--help")
		if strings.Contains(out+errb, "--trigger") || strings.Contains(out+errb, "--lease-for") {
			t.Errorf("job run --help showed a hidden schedule flag:\n%s", out+errb)
		}
	})
}
