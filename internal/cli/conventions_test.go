package cli

import (
	"strings"
	"testing"
)

// TestDryRunAliases: --dry and --dry-run parse to the same option as the verb's
// documented spelling. apply's documented spelling is --diff, and both aliases
// fold into it; create's is --dry-run, and --dry folds into it.
func TestDryRunAliases(t *testing.T) {
	env := parseEnv()

	t.Run("create: --dry == --dry-run", func(t *testing.T) {
		base := []string{"pz-main", "--game", "projectzomboid"}
		run, err := parseCreateArgs(env, append(append([]string{}, base...), "--dry-run"))
		if err != nil {
			t.Fatal(err)
		}
		dry, err := parseCreateArgs(env, append(append([]string{}, base...), "--dry"))
		if err != nil {
			t.Fatal(err)
		}
		if !run.DryRun || !dry.DryRun {
			t.Errorf("--dry-run=%v --dry=%v; both should be true", run.DryRun, dry.DryRun)
		}
	})

	t.Run("apply: --diff == --dry-run == --dry", func(t *testing.T) {
		for _, flag := range []string{"--diff", "--dry-run", "--dry"} {
			opt, err := parseApplyArgs(env, "apply", []string{"pz-main", flag})
			if err != nil {
				t.Fatalf("%s: %v", flag, err)
			}
			if !opt.Diff {
				t.Errorf("apply %s did not set Diff", flag)
			}
		}
	})
}

// TestBackupRunAlias: `gvm backup run <instance>` is the explicit spelling of
// `gvm backup <instance>` — it routes to the same nightly, so `run` never gets
// mistaken for an instance name.
func TestBackupRunAlias(t *testing.T) {
	h := newTestHost(t)
	// `backup run --help` must reach runBackupNow's help (exit 0, its usage),
	// which proves `run` routed there rather than being treated as an instance.
	code, out, errb := runWith(t, h, "", "backup", "run", "--help")
	combined := out + errb
	if code != 0 {
		t.Fatalf("`gvm backup run --help` exited %d:\n%s", code, combined)
	}
	if !strings.Contains(combined, "Usage: gvm backup") {
		t.Errorf("`gvm backup run --help` did not reach the nightly's help:\n%s", combined)
	}
}
