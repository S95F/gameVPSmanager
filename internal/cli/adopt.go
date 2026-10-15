package cli

import (
	"context"
	"flag"
	"fmt"
	"path"

	"github.com/S95F/gameVPSmanager/internal/adopt"
	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/version"
)

func runAdopt(ctx context.Context, env *Env, args []string) error {
	return subGroup{Verb: "adopt", Subs: []subcmd{
		{Name: "discover", Summary: "inspect an existing server install", Run: runAdoptDiscover},
		{Name: "cutover", Summary: "switch a legacy service over to gvm",
			Avail: avail.Unbuilt("not in this build"), Run: runAdoptUnbuilt("cutover")},
		{Name: "rollback", Summary: "revert a cutover to the legacy service",
			Avail: avail.Unbuilt("not in this build"), Run: runAdoptUnbuilt("rollback")},
		{Name: "finalize", Summary: "remove the legacy unit after a cutover", Run: runAdoptFinalize},
	}}.dispatch(ctx, env, args)
}

// runAdoptUnbuilt is the Run for a cutover/rollback that this build does not
// ship. --help is still help (exit 0, U7); otherwise it refuses honestly and
// changes nothing — there is no rollback script a cutover never wrote, and the
// live server stays exactly as it was.
func runAdoptUnbuilt(sub string) func(context.Context, *Env, []string) error {
	return func(_ context.Context, env *Env, args []string) error {
		if helpRequested(args) {
			fmt.Fprintf(env.Stdout, "Usage: gvm adopt %s <instance>\n\n"+
				"Not available in this build; nothing on this host would be changed.\n", sub)
			return errHelpShown
		}
		return avail.New(fmt.Sprintf("adopt %s is not in gvm %s; nothing on this host was "+
			"changed — keep running the existing service as before", sub, version.Version))
	}
}

func runAdoptDiscover(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("adopt discover", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	unitName := fs.String("unit", "zomboid", "bare systemd unit name (without .service)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	discEnv := adopt.OSDiscoverEnv("")
	r, err := adopt.DiscoverPZ(discEnv, *unitName)
	if err != nil {
		return fmt.Errorf("adopt discover: %w", err)
	}

	fmt.Fprint(env.Stdout, adopt.FormatDiscoverResult(r))

	// Exit non-zero if there are any errors in the findings.
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return fmt.Errorf("adopt discover: %d finding(s) require attention before cutover",
				countErrors(r.Findings))
		}
	}
	return nil
}

func runAdoptFinalize(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("adopt finalize", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "bypass the 7-day review window (not the target checks)")
	serverName := fs.String("name", "", "server name (required)")
	unitPath := fs.String("unit-path", "", "path to the legacy unit file to remove (required)")
	confirm := fs.String("confirm", "", "the instance name, typed, as the destructive-action contract requires")
	adoptedFrom := fs.String("adopted-from", "", "ignored; adopted_from is read from the instance YAML")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if *serverName == "" || *unitPath == "" {
		return fmt.Errorf("adopt finalize: --name and --unit-path are required")
	}
	if *adoptedFrom != "" {
		fmt.Fprintln(env.Stderr, "adopt finalize: --adopted-from is ignored; the value is read "+
			"from the instance YAML, which the host controls")
	}

	t, err := Resolve(env, "adopt", "")
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	in, _, err := loadInstance(h, *serverName)
	if err != nil {
		return err
	}

	// The legacy unit's name comes from the instance's OWN adopt guard, never
	// from --unit-path. A missing or unit-less guard means finalize cannot tell
	// what it would be removing, so it refuses.
	guardPath := path.Join(path.Dir(instance.DropInPath(*serverName)), "20-adopt.conf")
	guard, _ := h.files().ReadFile(guardPath)
	legacy, haveDropin := adopt.ParseAdoptGuardUnit(guard)

	if err := adopt.CheckFinalize(adopt.FinalizeInputs{
		Name:           *serverName,
		AdoptedFrom:    in.Metadata.AdoptedFrom,
		ManagedInstall: in.Spec.ManagedInstall,
		LegacyUnit:     legacy,
		HaveDropin:     haveDropin,
		UnitPathFlag:   *unitPath,
		Force:          *force,
		Now:            h.now(),
	}); err != nil {
		return err
	}

	// Removing the legacy units is destructive (rule 5), so the instance name
	// is typed first. --yes does not satisfy this.
	if err := (TypedConfirm{Subject: *serverName}).confirmFrom(env, *confirm); err != nil {
		return err
	}

	if err := adopt.ArchiveAndRemoveLegacyUnit(*serverName, *unitPath); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "finalize: %s — %s copied to %s and removed\n",
		*serverName, legacy, adopt.LegacyUnitsArchiveDir(*serverName))
	return nil
}

func countErrors(findings []adopt.DiscoverFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == "error" {
			n++
		}
	}
	return n
}
