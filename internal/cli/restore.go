// `gvm restore` — the read-only half, and the contract around the other half.
//
// Section 13.2 gives restore seven ordered steps with no way around step 2:
// verify the source, archive the CURRENT state as class `preserve`, stop
// through the ladder, extract member by member verifying digests as the bytes
// stream, chown, re-read the server-owned config, start. Steps 4 and 5 are
// internal/archive.Restore and are built. The ORDERING -- which is the part
// that keeps a restore from overwriting a live world without a receipt for
// what was there -- is internal/backup's restore path, which this build does
// not have; execute.go's `pending` table names it, and asking jobs.Lookup is
// how an operator gets that sentence rather than a nil dereference.
//
// So what this file does today is everything that does not need the engine,
// and it does it properly:
//
//   - `--list` reads the archive's own manifest. Every archive carries
//     .gvm/manifest.json as its FIRST member precisely so this is one ~8 KB
//     read rather than decompressing 2.4 GB.
//   - the preflight prints what would be restored WHERE, from the manifest's
//     own roots, and which game manifest the archive was taken against --
//     because a 2026 world restored onto a 2027 manifest whose shutdown ladder
//     changed is a decision and not a default.
//   - section 16.4's typed confirmation is assembled and handed to the engine,
//     which checks it before the lock. `--no-pre-archive` is refused without
//     it under any circumstances: skipping step 2 means the live world is
//     overwritten with no copy of what was there, which is the one thing in
//     this subsystem that cannot be undone by any later command.

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

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// restoreOptions are section 13.2's own flags.
type restoreOptions struct {
	// Source is the archive to read.
	Source string
	// To is `--to <dir>`: extract somewhere for inspection, skipping steps
	// 2-7 entirely. It touches no live world, which is why it is the one
	// restore route with nothing to confirm.
	To string
	// NoPreArchive skips step 2. It prints a warning and requires the
	// instance name typed, because what it removes is the only copy of the
	// world it is about to overwrite.
	NoPreArchive bool
	// Paranoid upgrades the source verification from `--spot 24` to `--deep`.
	Paranoid bool
	// PreserveOwnership applies the recorded uid/gid instead of the instance
	// account's.
	PreserveOwnership bool
	// AcceptManifestDrift allows a restore onto a different game manifest
	// than the archive was taken against.
	AcceptManifestDrift bool
	// ForceStop escalates a stop that will not stop, and marks the restore
	// degraded.
	ForceStop bool
}

func runRestore(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	var ro restoreOptions
	var list bool
	var only, from string

	fs := flag.NewFlagSet("gvm restore", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr,
			"Usage: gvm restore <archive> --instance <name>\n"+
				"       gvm restore --list [--instance <name>] [--from <dir>]\n\n"+
				"Overwrites a world with the contents of an archive. The current state is\n"+
				"archived as class `preserve` first, which no retention policy deletes.\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	opt.bindConfirm(fs)
	fs.BoolVar(&list, "list", false,
		"list the archives this host has, with what each one contains; reads nothing else")
	fs.StringVar(&only, "instance", "", "the instance to restore onto")
	fs.StringVar(&from, "from", "",
		"a directory to list instead of the instance's own (legacy bash-suite tarballs too)")
	fs.StringVar(&ro.To, "to", "",
		"extract into this directory instead of over the live world; skips steps 2-7")
	fs.BoolVar(&ro.NoPreArchive, "no-pre-archive", false,
		"do NOT archive the current world first. Requires --confirm <instance>")
	fs.BoolVar(&ro.Paranoid, "paranoid", false,
		"verify the source archive member by member (--deep) instead of by sample (--spot 24)")
	fs.BoolVar(&ro.PreserveOwnership, "preserve-ownership", false,
		"restore the recorded uid/gid rather than the instance account's")
	fs.BoolVar(&ro.AcceptManifestDrift, "accept-manifest-drift", false,
		"restore onto a different game manifest than the archive was taken against")
	fs.BoolVar(&ro.ForceStop, "force-stop", false,
		"escalate a stop that will not stop; marks the restore degraded")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if list {
		if len(pos) > 0 {
			return fmt.Errorf("--list takes no archive; it lists them")
		}
		return runRestoreList(ctx, env, only, from, opt)
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("name one archive to restore, or use --list")
	}
	ro.Source = pos[0]
	if only == "" && ro.To == "" {
		// Without an instance there is nothing to restore ONTO, and guessing
		// one from the archive's own manifest would mean a tarball's contents
		// chose which live world to overwrite.
		return fmt.Errorf("name the instance to restore onto: gvm restore %s --instance <name>\n"+
			"  (or --to <dir> to extract it somewhere for inspection, which touches no world)",
			ro.Source)
	}
	if ro.NoPreArchive && strings.TrimSpace(opt.Confirm) != only {
		// Refused HERE as well as in the engine, and not as belt and braces:
		// the engine's confirmation authorises the restore, and this one
		// authorises removing the only copy of what the restore overwrites.
		// They are different decisions and `--yes` does not satisfy this one.
		return fmt.Errorf("--no-pre-archive overwrites %s's world with no archive of what was "+
			"there, and nothing in gvm can undo that. Type the instance name to use it: "+
			"gvm restore %s --instance %s --no-pre-archive --confirm %s",
			only, ro.Source, only, only)
	}

	opt.Engine.Restore = ro
	return runRestoreOnto(ctx, env, only, opt)
}

// runRestoreOnto runs the preflight this build can perform and then hands the
// job to internal/jobs.
func runRestoreOnto(ctx context.Context, env *Env, name string, opt jobOptions) error {
	t, err := Resolve(env, "restore", opt.Remote)
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
	src, err := absArchive(opt.Engine.Restore.Source)
	if err != nil {
		return err
	}
	m, perr := archive.PeekManifestFile(src)
	if perr != nil {
		// Before anything else: an archive whose head cannot be read is not a
		// restore to attempt and abandon halfway. Step 1 verifies it properly;
		// this refuses the ones that cannot even be identified.
		return fmt.Errorf("%s cannot be read as a gvm archive: %w", src, perr)
	}
	printRestorePreflight(env.Stdout, name, src, m, opt.Engine.Restore)

	if err := restoreDrift(h, name, m, opt.Engine.Restore); err != nil {
		return err
	}
	if opt.Engine.Restore.To != "" {
		// `--to <dir>` is the inspection path. It stops no server and
		// overwrites no world, so there is nothing for the destructive-action
		// contract to protect -- but the extraction itself is still steps 4
		// and 5, and those belong to the engine.
		return fmt.Errorf("%w: extracting into %s is internal/archive.Restore, which this "+
			"build has; what it is missing is the path that drives it",
			restoreRunnerGap(), opt.Engine.Restore.To)
	}
	return runKind(ctx, env, "restore", lifecycle.JobRestore, name, opt)
}

// restoreRunnerGap is internal/jobs' own refusal for the restore kind, so the
// sentence naming the package that will bring it is written in exactly one
// place (execute.go's `pending`) and this file cannot drift from it.
func restoreRunnerGap() error {
	_, err := jobs.Lookup(lifecycle.JobRestore)
	if err == nil {
		return errors.New("gvm: the restore path is built; this message should not appear")
	}
	return err
}

// absArchive refuses a relative archive path.
//
// Not pedantry: the restore runs as a job whose working directory is the
// unit's, so a path relative to the operator's shell would resolve somewhere
// else entirely -- and the thing it would resolve to is an archive nobody
// chose.
func absArchive(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("name an archive")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path; a restore runs as a systemd job "+
			"whose working directory is not your shell's", p)
	}
	return p, nil
}

// restoreDrift is section 13.2's manifest check, performed before anything.
//
// A world taken against one game manifest and restored onto another is a
// decision: the shutdown ladder, the config keys and the world paths can all
// have moved between revisions, and gvm would be applying the new ones to the
// old world. So it is refused by default and the flag that allows it is named.
func restoreDrift(h *HostEnv, name string, m *archive.Manifest, ro restoreOptions) error {
	if ro.AcceptManifestDrift || name == "" {
		return nil
	}
	in, _, err := loadInstance(h, name)
	if err != nil {
		return err
	}
	want := in.Spec.Manifest.SHA256
	got := m.Instance.GameManifestSHA256
	switch {
	case want == "" || got == "":
		// One side cannot say. Reported rather than refused: an adopted
		// instance's first archive predates the pin, and refusing would make
		// the one restore an operator most needs impossible.
		return nil
	case want == got:
		return nil
	}
	return fmt.Errorf("%s was taken against game manifest %s and %s is pinned to %s.\n"+
		"  The shutdown ladder, the config keys and the world paths can all have moved "+
		"between revisions, so gvm would be applying the new ones to the old world.\n"+
		"  `gvm game which %s` shows the pinned manifest; --accept-manifest-drift proceeds anyway",
		m.Instance.Name, shortSHA(got), name, shortSHA(want), in.Spec.Game)
}

func printRestorePreflight(w io.Writer, name, src string, m *archive.Manifest, ro restoreOptions) {
	fmt.Fprintf(w, "\nrestore %s\n", src)
	fmt.Fprintf(w, "  archive   %s, class %s", m.Instance.Name, m.Class)
	if m.Reason != "" {
		fmt.Fprintf(w, ", reason %q", m.Reason)
	}
	fmt.Fprintf(w, "\n  taken     %s by gvm %s\n",
		m.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"), m.GVMVersion)
	fmt.Fprintf(w, "  game      %s", m.Instance.Game)
	if m.Instance.GameManifestRevision > 0 {
		fmt.Fprintf(w, " rev %d", m.Instance.GameManifestRevision)
	}
	fmt.Fprint(w, "\n")
	if name != "" {
		fmt.Fprintf(w, "  onto      %s\n", name)
	}

	// The roots are what makes the archive self-describing: each member-path
	// prefix maps back to the base directory it was taken from, so a restore
	// knows where the bytes go without a database.
	roots := append([]archive.Root(nil), m.Roots...)
	sort.Slice(roots, func(i, j int) bool { return roots[i].Prefix < roots[j].Prefix })
	for _, r := range roots {
		dest := r.Base
		if ro.To != "" {
			dest = filepath.Join(ro.To, r.Prefix)
		}
		fmt.Fprintf(w, "  %-9s %s/  ->  %s\n", r.Prefix, r.Prefix, dest)
	}
	if f := m.WorldFingerprint; f != nil {
		fmt.Fprintf(w, "  world     %s across %d file(s), newest %s\n",
			spec.FormatBytes(f.Bytes), f.Files, f.NewestMod.UTC().Format("2006-01-02 15:04"))
	}

	mode := "spot 24"
	if ro.Paranoid {
		mode = "deep (every member re-hashed)"
	}
	fmt.Fprintf(w, "  verify    %s before anything is written\n", mode)
	switch {
	case ro.To != "":
		fmt.Fprint(w, "  step 2    not applicable: --to extracts for inspection and touches "+
			"no world\n")
	case ro.NoPreArchive:
		fmt.Fprintf(w, "\n  WARNING: --no-pre-archive. %s's current world will be overwritten "+
			"with NO archive of what is there now, and nothing in gvm can undo that.\n", name)
	default:
		fmt.Fprint(w, "  step 2    the current world is archived as `preserve`, "+
			"reason pre-restore, first\n")
	}
}

// ----------------------------------------------------------------------
// gvm restore --list

func runRestoreList(ctx context.Context, env *Env, only, from string, opt jobOptions) error {
	t, err := Resolve(env, "restore", opt.Remote)
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
	dirs, err := archiveDirs(h, only, from)
	if err != nil {
		return err
	}
	var all []archiveRow
	for _, d := range dirs {
		rows, err := archiveRows(h, d)
		if err != nil {
			return err
		}
		all = append(all, rows...)
	}
	if opt.JSON {
		return writeJSON(env.Stdout, all)
	}
	printArchiveRows(env.Stdout, all)
	fmt.Fprint(env.Stdout, "\n  gvm restore <archive> --instance <name> puts one back. "+
		"The current world is archived first.\n")
	return nil
}
