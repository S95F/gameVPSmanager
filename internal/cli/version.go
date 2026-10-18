// `gvm version` — what this binary is, and the formats it speaks.
//
// `--json` exists for the upgrade story (gvm-self-upgrade-uninstall): before an
// operator replaces the binary on a box with live worlds, the honest question
// is not "what tag is this" but "will the running runners still understand what
// the new binary writes". So the JSON carries the on-disk and on-socket format
// versions next to the build metadata — the same constants TestFrozenFormats
// pins and the store schema the daemon requires — so a check can compare two
// builds' formats, not just their tags.

package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/backup"
	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/runner"
	"github.com/S95F/gameVPSmanager/internal/store"
	"github.com/S95F/gameVPSmanager/internal/version"
)

func runVersion(_ context.Context, env *Env, args []string) error {
	var asJSON bool
	fs := flag.NewFlagSet("gvm version", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm version [--json]\n\n"+
			"Prints the build. --json adds the on-disk and on-socket format versions,\n"+
			"so an upgrade check can tell whether a new binary's formats still match a\n"+
			"running one's.\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&asJSON, "json", false, "print the build and the format versions as JSON")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}

	if !asJSON {
		fmt.Fprintln(env.Stdout, version.String())
		return nil
	}

	info := version.Info()
	payload := map[string]any{
		"version":   info["version"],
		"commit":    info["commit"],
		"goVersion": info["goVersion"],
		"os":        info["os"],
		"arch":      info["arch"],
		// The frozen formats (U17), by the name an operator or a skew check reads
		// them as. These are the versions a running runner must still accept
		// after the binary under it is replaced.
		"formats": map[string]int{
			"console_proto":  console.Proto,
			"lease_proto":    lifecycle.LeaseProto,
			"freeze_lease":   backup.LeaseSchema,
			"status_proto":   runner.StatusProto,
			"runner_file":    runner.RunnerFileProto,
			"shutdown_plan":  runner.PlanProto,
			"archive_schema": archive.SchemaVersion,
			"store_schema":   store.SchemaVersion,
		},
	}
	if info["dirty"] == "true" {
		payload["dirty"] = true
	}
	return writeJSON(env.Stdout, payload)
}
