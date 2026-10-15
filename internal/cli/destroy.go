// `gvm destroy` — the destructive route, and the three gates in front of it.
//
// rebuild.sh made you type the server name before it deleted a world, and that
// discipline is the one thing from the bash suite that must survive contact
// with a web UI where a click is one pixel from a mistake. So the contract is
// server-side and identical for every caller: the typed name is
// checked HERE, by the same TypedConfirm the API edge will use, not by the
// interface that asked the question.
//
// The other two gates are types rather than manners. instance.Destroy takes a
// *archive.Ref and refuses one that has never been verified, and it takes
// []instance.Jailed, which no package outside internal/instance can construct.
// So this file cannot delete anything it has not first archived, checked, and
// proven to be inside the jail -- and it could not be written to do so without
// the compiler stopping it.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

type destroyOptions struct {
	Name string
	// Archive is the tarball that authorises the deletion. It is REQUIRED and
	// there is no flag that waives it: `gvm backup` writes one, and a destroy
	// with nothing to restore from is the failure this whole chain exists to
	// make impossible.
	Archive string
	// Confirm carries the typed name for a non-interactive caller. It is
	// checked by exactly the same function that checks a typed answer.
	Confirm string
	Dry     bool
	Remote  string
}

func parseDestroyArgs(env *Env, args []string) (destroyOptions, error) {
	var opt destroyOptions
	fs := flag.NewFlagSet("gvm destroy", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm destroy <instance> --archive <path>\n\n"+
			"Removes an instance's install and data, then its control-plane files.\n"+
			"The archive is not optional: it is what makes the deletion reversible.\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opt.Archive, "archive", "", "the verified archive that authorises this (required)")
	fs.StringVar(&opt.Confirm, "confirm", "", "type the instance name here instead of at the prompt")
	fs.BoolVar(&opt.Dry, "dry", false, "measure and report what would be removed, and remove nothing")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseWithInstance(fs, args)
	if err != nil {
		return destroyOptions{}, flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return destroyOptions{}, err
	}
	opt.Name = name
	if opt.Archive == "" {
		fs.Usage()
		return destroyOptions{}, errors.New("--archive is required: gvm will not delete a world " +
			"it cannot put back. `gvm backup <instance>` writes one")
	}
	return opt, nil
}

func runDestroy(ctx context.Context, env *Env, args []string) error {
	opt, err := parseDestroyArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "destroy", opt.Remote)
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

	in, _, err := loadInstance(h, opt.Name)
	if err != nil {
		return err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return err
	}

	// A running server is not destroyed. Deleting the files under a live
	// process leaves it running against unlinked inodes, which looks fine
	// until it writes and produces a save nobody can find.
	obs, err := h.observer().Observe(ctx, opt.Name)
	if err != nil {
		return err
	}
	if v := lifecycle.Derive(obs); v.Phase.Serving() {
		return fmt.Errorf("%s is %s. Stop it first: gvm stop %s", opt.Name, v.Phase, opt.Name)
	}

	paths := h.paths(opt.Name)
	targets, err := destroyTargets(in, m, paths.SecretsDir)
	if err != nil {
		return err
	}

	ref, err := verifiedArchive(ctx, opt.Archive, opt.Name, paths.SnapDir)
	if err != nil {
		return err
	}

	// The preflight is the same function Destroy runs, in the form that
	// answers instead of acting -- so the plan and the act cannot drift.
	pre, err := instance.DestroyPreflightWith(ref, targets, h.statfs())
	if err != nil {
		return err
	}
	ageWarn := destroyAgeWarning(ref, pre, h.now())
	printDestroyPreflight(env.Stdout, opt.Name, ref, pre, ageWarn)
	if opt.Dry {
		fmt.Fprintln(env.Stdout, "\n  nothing was removed (--dry).")
		return nil
	}

	// The space arithmetic is CONSULTED and not merely printed. It is the one
	// refusal in this chain that is about reversibility rather than paperwork:
	// a destroy that leaves no room to restore the archive afterwards is one
	// the operator cannot undo, which is precisely what --archive exists to
	// prevent. It is checked after the --dry return, because a dry run's
	// contract is to measure and report, and before the typed confirmation,
	// because nobody should be asked to type an instance name for a destroy
	// that is going to be refused anyway.
	if err := pre.Check(); err != nil {
		return err
	}

	// The age warning again, directly above the prompt: the one place an
	// operator is certain to read it is the line before they type the name.
	if ageWarn != "" {
		fmt.Fprintf(env.Stderr, "\n  WARNING: %s\n", ageWarn)
	}
	if err := (TypedConfirm{Subject: opt.Name, ArchiveAck: true}).confirmFrom(env, opt.Confirm); err != nil {
		return err
	}

	// The archive that authorised this destroy gets a durable preserve pin
	// BEFORE the world is removed, so retention can never reclaim the one copy
	// of what was just destroyed — even after a new instance is created with
	// the same name into the same backup directory. The pin is a root-owned
	// sidecar; Reindex and Prune both honour it (cli-ergonomics-7, rule 5).
	if err := archive.WritePreservePin(ref.Path, archive.PreservePin{
		Reason: "destroy-authorised", PinnedAt: h.now(), Instance: opt.Name,
	}); err != nil {
		return fmt.Errorf("pin the authorising archive before destroying the world: %w", err)
	}
	// Refresh the index so `gvm backup list` shows the pin at once rather than
	// at the next backup. A reindex failure is not fatal: the sidecar is the
	// source of truth and the next Reindex will pick it up regardless.
	_, _ = archive.Reindex(ctx, filepath.Dir(ref.Path), archive.ReindexOptions{Now: h.Now})

	res, err := archive.Destroy(ctx, ref, toArchiveTargets(targets))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "\n  removed %d file(s), %d director(ies), %s\n",
		res.Files, res.Dirs, spec.FormatBytes(res.Bytes))
	fmt.Fprintf(env.Stdout, "  the authorising archive is preserved (pinned: destroy-authorised): %s\n",
		ref.Path)

	// The supervision comes down before the definition does.
	//
	// `gvm apply` ENABLED gvm@<name>.service and enabled-and-started a timer
	// per job. Nothing else in gvm removes either: apply plans only for jobs
	// the spec declares, and there is no sweeper. Left behind, the enablement
	// symlink makes systemd try the instance at every boot -- and the packaged
	// gvm@.service has no ConditionPathExists guard, so a spec that is gone
	// produces a permanently FAILED unit rather than a skipped one -- while
	// each surviving timer fires a nightly job against an instance whose YAML
	// no longer exists.
	//
	// Disable must precede each file removal: DisableUnitFiles reads the
	// [Install] section out of the unit file to find the symlinks, so removing
	// the timer first would strand its timers.target.wants link with nothing
	// left to name it.
	req := func(r hostops.Request) error {
		r.Actor, r.Instance = actorFor(t), opt.Name
		if _, err := h.Ops.Do(ctx, r); err != nil {
			return err
		}
		return nil
	}
	jobs := make([]string, 0, len(in.Spec.Jobs))
	for name := range in.Spec.Jobs {
		jobs = append(jobs, name)
	}
	// Sorted so the audit trail of a destroy is the same every time it is run.
	sort.Strings(jobs)
	// The per-job sequence is reconcile's, not a second copy of it: `gvm apply`
	// takes down the timer of a job the operator deleted with exactly these
	// verbs in exactly this order, and two spellings of "how a timer comes
	// down" would eventually disagree about which of the stop, the disable and
	// the removals may be skipped.
	for _, job := range jobs {
		a := &reconcile.OrphanTimerAction{
			Instance: opt.Name, Job: job, Actor: actorFor(t), Host: h.Ops,
		}
		if err := a.Do(ctx, nil); err != nil {
			return fmt.Errorf("tearing down the %s job: %w", job, err)
		}
	}
	if err := req(hostops.Request{Verb: hostops.VerbUnitDisable, Unit: hostops.UnitInstance}); err != nil {
		return fmt.Errorf("disabling the unit: %w", err)
	}

	// The control-plane files go last and through hostops, so the removal of
	// the definition is an audited privileged verb like every other write to
	// /etc/gvm -- and so an interrupted destroy leaves the YAML behind, which
	// is the recoverable direction: an instance with no files is repairable
	// and files with no instance are orphaned.
	for _, k := range []hostops.FileKind{hostops.FileDropIn, hostops.FileManifestLock} {
		if err := req(hostops.Request{Verb: hostops.VerbFileRemove, File: k}); err != nil {
			return fmt.Errorf("removing the %s: %w", k, err)
		}
	}
	// One reload, after the unit files are gone and before the YAML is, so
	// systemd forgets the deleted drop-in and timers rather than holding them
	// until something else happens to reload.
	if err := req(hostops.Request{Verb: hostops.VerbDaemonReload}); err != nil {
		return fmt.Errorf("reloading systemd: %w", err)
	}
	if err := req(hostops.Request{Verb: hostops.VerbFileRemove, File: hostops.FileInstanceSpec}); err != nil {
		return fmt.Errorf("removing the %s: %w", hostops.FileInstanceSpec, err)
	}
	fmt.Fprintf(env.Stdout, "  the archive is still at %s\n", ref.Path)
	return nil
}

// confirmFrom takes the typed value from a flag or from the terminal, and
// sends both through the same Check.
//
// One path, two sources. If the flag were checked with a different comparison
// -- a case-insensitive one, say, because it is "just a flag" -- then the
// scriptable route would be weaker than the interactive one, and the scriptable
// route is the one an attacker gets to use.
func (c TypedConfirm) confirmFrom(env *Env, typed string) error {
	// A typed-name mismatch or a declined prompt is a REFUSAL (exit 5), not a
	// failure: the operator was asked and did not confirm, which a script should
	// tell apart from the action failing (U8). A terminal read error stays a
	// failure (1) — that is the tool breaking, not the operator declining.
	if typed != "" {
		if err := c.Check(typed, true); err != nil {
			return refused(err)
		}
		return nil
	}
	ok, err := c.Prompt(env.Stdin, env.Stdout)
	if err != nil {
		return err
	}
	if !ok {
		return refused(ErrNotConfirmed)
	}
	return nil
}

// verifiedArchive turns a path into a receipt, or refuses.
//
// The Ref is built from the archive's own index entry and then VERIFIED, which
// is the only thing that sets VerifiedAt. A Ref that has never been verified is
// not a receipt for anything -- a truncated tarball is exactly the failure
// archive-before-destroy exists to prevent -- and instance.Destroy panics on
// one rather than returning an error, because reaching it is a programming
// mistake and not an operating condition.
func verifiedArchive(ctx context.Context, path, wantInstance, scratch string) (*archive.Ref, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, file := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))

	ix, err := archive.Reindex(ctx, dir, archive.ReindexOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the archive directory %s: %w", dir, err)
	}
	e, ok := ix.Find(file)
	if !ok {
		return nil, fmt.Errorf("%s is not in %s", file, dir)
	}
	if e.Legacy {
		return nil, fmt.Errorf("%s carries no gvm manifest, so gvm cannot tell what it holds "+
			"or which instance it came from. It will not authorise a destroy", file)
	}
	if e.Instance != wantInstance {
		return nil, fmt.Errorf("%s is an archive of %q, not of %q", file, e.Instance, wantInstance)
	}
	ref := e.Ref(dir)

	// A spot verify rather than deep: deep re-reads every byte, which on a
	// 40 GB world is minutes of the operator staring at nothing, and spot's
	// sample always includes the largest, smallest and newest members, which
	// is where truncation and a mid-backup edit actually show up.
	if _, err := archive.VerifyRef(ctx, ref, archive.VerifyOptions{
		Mode:    archive.ModeSpot,
		Scratch: scratch,
	}); err != nil {
		return nil, fmt.Errorf("%s did not verify, so it does not authorise anything: %w", file, err)
	}
	return ref, nil
}

// destroyTargets builds the proven set of paths this destroy may remove.
//
// A FULL destroy removes the instance: its install tree, its data tree, and
// then the control-plane files that define it. What makes that reversible is
// the verified archive, which is why --archive has no waiver -- not a list of
// files held back inside a tree whose owning instance is about to stop
// existing.
//
// So the manifest's backup.world_paths and backup.destroy_protected are NOT in
// this jail. They are rebuild-time concepts: world_paths is "never in a destroy
// set without --rebuild" (section 13.6 step 8 destroys exactly them and nothing
// else), and destroy_protected is rebuild.sh's "accounts and settings survive
// the rebuild". Handing either list to a full destroy is not conservative, it
// is incoherent: every shipped manifest puts those paths INSIDE install or
// data, so a jail protecting them refuses the very roots it was asked to
// resolve and `gvm destroy` can remove nothing at all. `gvm rebuild` gets the
// narrow jail, through Jail.Rebuild, which is the one door that promotes a
// world path into the allow set.
//
// The secrets directory stays protected. It lives outside both roots, so it
// blocks nothing in the ordinary case; it is here for the case where install or
// data RESOLVES into it through a symlink, where refusing the destroy entirely
// is the right answer.
//
// The secrets directory arrives as a parameter rather than from PathsFor so
// that the jail logic is exercised on the development box: PathsFor's paths are
// Linux constants, and filepath.IsAbs refuses them on Windows, which would make
// this function's only observable failure there a platform artifact rather than
// its behaviour.
func destroyTargets(in *spec.Instance, m *spec.Manifest, secretsDir string) ([]instance.Jailed, error) {
	_ = m // the manifest's protected lists are deliberately not consulted; see above.

	allow := []string{in.Spec.Paths.Install, in.Spec.Paths.Data}
	j, err := instance.NewJail(instance.JailConfig{
		Allow:     allow,
		Protected: []string{secretsDir},
	})
	if err != nil {
		return nil, err
	}
	return j.ResolveAll(allow)
}

// toArchiveTargets adapts the proven paths to archive.Destroy's interface.
//
// It is a conversion and not a re-derivation: instance.Jailed already satisfies
// archive.Target with value receivers, and the loop exists only because Go has
// no covariance on slices. Nothing here can widen what was proven.
func toArchiveTargets(js []instance.Jailed) []archive.Target {
	out := make([]archive.Target, 0, len(js))
	for _, j := range js {
		out = append(out, j)
	}
	return out
}

// destroyAgeWarning returns the "this archive predates recent play" line, or ""
// when the archive is at least as new as everything in the destroy set. It
// names the age in days so the operator sees how much would be lost, not just a
// timestamp they have to subtract in their head (cli-ergonomics-7).
func destroyAgeWarning(ref *archive.Ref, pre *instance.Preflight, now time.Time) string {
	if ref == nil || pre == nil || pre.NewestMTime.IsZero() {
		return ""
	}
	if !ref.CreatedAt.Before(pre.NewestMTime) {
		return ""
	}
	days := int(now.Sub(ref.CreatedAt).Hours() / 24)
	cls := string(ref.Class)
	if ref.Reason != "" {
		cls += " (reason " + ref.Reason + ")"
	}
	return fmt.Sprintf("taken %s (%d days ago), class %s — %d days of play are not in it",
		ref.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), days, cls, days)
}

func printDestroyPreflight(w io.Writer, name string, ref *archive.Ref, pre *instance.Preflight, ageWarn string) {
	fmt.Fprintf(w, "\ndestroy %s\n", name)
	fmt.Fprintf(w, "  archive   %s\n", ref.Path)
	// CreatedAt and Class, not VerifiedAt: when the archive was taken and what
	// it is are what an operator needs to judge whether losing everything newer
	// than it is acceptable. "verified just now" was reassuring about the wrong
	// thing (cli-ergonomics-7).
	created := ref.CreatedAt.UTC().Format("2006-01-02 15:04:05Z")
	if ref.Reason != "" {
		fmt.Fprintf(w, "            %s, taken %s, class %s (reason %s)\n",
			spec.FormatBytes(ref.Bytes), created, ref.Class, ref.Reason)
	} else {
		fmt.Fprintf(w, "            %s, taken %s, class %s\n",
			spec.FormatBytes(ref.Bytes), created, ref.Class)
	}
	if ageWarn != "" {
		fmt.Fprintf(w, "  WARNING   %s\n", ageWarn)
	}
	fmt.Fprintf(w, "  removes   %d file(s), %d director(ies), %s\n",
		pre.Files, pre.Dirs, spec.FormatBytes(pre.Bytes))
	for _, t := range pre.Paths {
		fmt.Fprintf(w, "            %s\n", t)
	}
	est := spec.FormatBytes(pre.Estimate)
	if pre.EstimateIsFloor {
		est += " (a FLOOR: the archive records no uncompressed size, so this is the " +
			"compressed one and a restore will need more)"
	}
	fmt.Fprintf(w, "  restore   would need about %s\n", est)
	for _, v := range pre.Volumes {
		if !v.Checked {
			fmt.Fprintf(w, "  volume    %s: NOT CHECKED on this platform\n", v.Root)
			continue
		}
		fmt.Fprintf(w, "  volume    %s: %s free, %s after this destroy, %s needed to restore\n",
			v.Root, spec.FormatBytes(v.Avail), spec.FormatBytes(v.Avail+v.Freeing),
			spec.FormatBytes(v.Required))
	}
}
