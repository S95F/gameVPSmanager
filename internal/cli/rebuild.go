// `gvm rebuild` — rebuild.sh's ordering, with its prompt kept and its three
// lines of tar replaced by one `gvm restore`.
//
// The command is thin because the ordering is the safety property and it lives
// in exactly one place (internal/rebuild.Run): content resolved, downloaded and
// VERIFIED before anything is deleted; the archive taken after that
// verification and before the deletion; every early exit leaving the world
// untouched and SAYING SO. Expressing any of that a second time here would
// create two orderings that have to agree forever, and the failure of that
// agreement is a deleted world.
//
// It runs as a JOB rather than in-process, which is the one visible difference
// from the script. A rebuild stops the server (step 3) and starts it again
// (step 10), and a rebuild that died between them would leave a world down
// with nobody watching -- so it goes through internal/jobs, where the running
// guard is armed before the first fallible call and gvm-guard.timer is the
// belt to that braces.
//
// THE PROMPT APPLIES TO `--keep-world` TOO, and that is deliberate. The job
// kind is `rebuild`, section 16.4's contract is per-kind and enforced in the
// engine, and `--keep-world` is one keystroke from the invocation that erases
// everybody's characters. `--yes` is how automation says it meant it.

package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

func runRebuild(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	fs := flag.NewFlagSet("gvm rebuild", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr,
			"Usage: gvm rebuild <instance> [--keep-world] [--dry] [--no-fetch]\n\n"+
				"Generates a NEW world with the current content list. The old world is\n"+
				"archived as class `preserve` -- which no retention policy ever deletes --\n"+
				"and only then deleted. Every exit before that archive verifies leaves the\n"+
				"world exactly as it was, and says so.\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	opt.bindConfirm(fs)
	fs.BoolVar(&opt.Engine.Rebuild.KeepWorld, "keep-world", false,
		"apply the content list and keep the world: exits after the resolve, "+
			"never reaching the archive or the destroy")
	fs.BoolVar(&opt.Engine.Rebuild.Dry, "dry", false,
		"stop after the content plan having touched nothing, not even the acquisition list")
	fs.BoolVar(&opt.Engine.Rebuild.NoFetch, "no-fetch", false,
		"use the acquisition list as it stands, for the day Steam is rate-limiting")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return err
	}

	// The typed name reaches the engine as the Confirmation and is checked
	// there, before the lock. Copying the flags into rebuild.Options as well
	// is not a second check: internal/rebuild's own step 2 calls the same
	// jobs.Confirmation, so the two cannot disagree, and the inner one is what
	// keeps `rebuild.Run` honest for a caller that is not this CLI.
	opt.Engine.Rebuild.Typed = opt.Confirm
	opt.Engine.Rebuild.Yes = opt.Yes

	return runKind(ctx, env, "rebuild", lifecycle.JobRebuild, name, opt)
}
