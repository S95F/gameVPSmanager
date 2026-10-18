// `gvm backup` — the nightly, and the two housekeeping kinds beside it.
//
// All four subcommands are the same thing: a job, run through internal/jobs,
// with the running guard armed. Not one line of the freeze, the snapshot, the
// retention or the verification lives here; this file turns a command line into
// a jobs.Env and prints the record that comes back. The reason is section
// 13.4's: a second path that froze a world and thawed it would be a second
// path that can fail between the two, and `backup.sh` already demonstrated
// what that costs.
//
// `verify` and `prune` take an OPTIONAL instance, because their kinds are
// scoped `either` and the same table is `gvm backup verify
// pz-main` and the host-level nightly. A second implementation for the
// host-wide sweep is how the two would come to disagree about what "verified"
// means.
//
// `list` is the exception: it runs no job at all. It reads the index, which is
// a cache rebuilt from the archives, so it answers "what have I got" with
// Postgres down and without decompressing 2.4 GB -- section 13.2's reason for
// the index existing.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

var backupSubs = []struct {
	name    string
	args    string
	summary string
	run     func(context.Context, *Env, []string) error
}{
	{"", "<instance>", "freeze, snapshot, verify, then prune (the nightly)", runBackupNow},
	// `run` is the explicit spelling of the nightly, reserved so a new verb
	// never shadows it: `gvm backup run <instance>` == `gvm backup <instance>`.
	{"run", "<instance>", "the nightly, by hand (same as `gvm backup <instance>`)", runBackupNow},
	{"verify", "[<instance>] [--deep]", "read archives back: gzip, manifest, digests", runBackupVerify},
	{"prune", "[<instance>]", "apply retention; preserve archives are never candidates", runBackupPrune},
	{"list", "[<instance>]", "what this host has archived, from the index", runBackupList},
	{"delete", "<archive> [--dry-run]", "remove one archive (type the instance name); the only way to remove a preserve archive", runBackupDelete},
}

func runBackup(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		backupUsage(env.Stderr)
		return errors.New("name an instance to back up")
	}
	switch args[0] {
	case "-h", "--help", "help":
		backupUsage(env.Stdout)
		return nil
	}
	for _, s := range backupSubs {
		if s.name != "" && s.name == args[0] {
			return s.run(ctx, env, args[1:])
		}
	}
	// No subcommand: `gvm backup pz-main` is the nightly, by hand.
	return runBackupNow(ctx, env, args)
}

func backupUsage(w io.Writer) {
	fmt.Fprint(w, "gvm backup — archive an instance, and keep the archives honest\n\nUsage:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range backupSubs {
		fmt.Fprintf(tw, "  gvm backup %s %s\t%s\n", s.name, s.args, s.summary)
	}
	_ = tw.Flush()
	fmt.Fprint(w, "\n  Retention only ever runs after a new archive has been written AND\n"+
		"  verified, so a failing backup cannot erode the history it failed to add\n"+
		"  to. A `preserve` archive is deleted by no code path at all: `rm` by a\n"+
		"  human is the only way, and `gvm backup list` totals them so the disk\n"+
		"  does not fill by surprise.\n")
}

func runBackupNow(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	fs := flag.NewFlagSet("gvm backup", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm backup <instance>\n\n"+
			"Runs the `backup` job: freeze, snapshot, verify, prune, push.\n"+
			"The server keeps serving throughout; only persistence pauses.\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	pos, err := parseWithInstance(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return err
	}
	return runKind(ctx, env, "backup", lifecycle.JobBackup, name, opt)
}

func runBackupVerify(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	var deep bool
	var only string
	fs := flag.NewFlagSet("gvm backup verify", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm backup verify [<instance>] [--deep]\n\n"+
			"With no instance, verifies every archive on the host -- which is what\n"+
			"gvm-job@_verify.timer runs nightly.\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	fs.BoolVar(&deep, "deep", false,
		"re-hash every member of every archive, not just the newest preserve one (minutes)")
	fs.StringVar(&only, "instance", "", "verify one instance's archives")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneOrNoInstance(pos, only)
	if err != nil {
		fs.Usage()
		return err
	}
	// --deep widens the SHALLOW pass to every member of every archive. The
	// deep pass over the newest preserve archive runs either way: it is the
	// one a restore is most likely to need.
	opt.Engine.DeepVerify = deep
	return runKind(ctx, env, "backup", lifecycle.JobVerify, name, opt)
}

func runBackupPrune(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	var only string
	fs := flag.NewFlagSet("gvm backup prune", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm backup prune [<instance>]\n\n"+
			"Applies keep/keep_days from the instance's own backup block. Retention\n"+
			"runs only after the newest archive has been read back, so a directory\n"+
			"whose newest archive is damaged prunes nothing and says so.\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	fs.StringVar(&only, "instance", "", "prune one instance's archives")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneOrNoInstance(pos, only)
	if err != nil {
		fs.Usage()
		return err
	}
	return runKind(ctx, env, "backup", lifecycle.JobPrune, name, opt)
}

// oneOrNoInstance accepts the name as a positional OR as --instance, and
// accepts neither.
//
// Both spellings because section 13.2's own walkthrough uses `--instance` and
// every other verb in this CLI takes the name positionally; a command that
// accepted only one of them would be a command an operator types twice.
func oneOrNoInstance(pos []string, flagName string) (string, error) {
	switch {
	case len(pos) > 1:
		return "", usageErr(fmt.Errorf("name one instance, not %d", len(pos)))
	case len(pos) == 1 && flagName != "" && pos[0] != flagName:
		return "", usageErr(fmt.Errorf("two different instances named: %q and --instance %q",
			pos[0], flagName))
	case len(pos) == 1:
		return pos[0], instanceNameOK(pos[0])
	case flagName != "":
		return flagName, instanceNameOK(flagName)
	}
	return "", nil
}

func instanceNameOK(name string) error {
	_, err := oneInstance([]string{name})
	return err
}

// runKind is an operator verb driving one job kind directly.
//
// It does NOT go through jobs.Resolve: the kind is known, and a descriptor
// lookup would make `gvm backup pz-main` fail on a box where `gvm apply` has
// not written /etc/gvm/jobs yet -- which is exactly the box an operator is
// most likely to be taking a backup of by hand.
func runKind(ctx context.Context, env *Env, verb string, kind lifecycle.JobKind,
	name string, opt jobOptions) error {

	action := actionFor(kind)
	return runOneJob(ctx, env, verb, opt, func(*HostEnv) (jobs.Resolved, error) {
		return jobs.Resolved{
			Slug:   jobs.Slug{Instance: name, Job: jobNameFor(action)},
			Kind:   kind,
			Action: action,
			Via:    jobs.ViaJobName,
		}, nil
	})
}

// jobNameFor is the action as a JOB NAME, which is not always the same string.
//
// D-45: a slug is "<instance>_<job>" and neither half may contain '_', so
// there is exactly one reading of exactly one separator. The action
// `content_sync` contains one -- it is an enum value in the ledger, not a job
// name -- and using it as the job half would produce `pz-main_content_sync`,
// which has two legal readings and which instance.JobSlug refuses outright.
//
// So an operator verb spells it with a hyphen, and the ACTION recorded in the
// row keeps the schema's own spelling. The two are different fields for this
// reason, and conflating them would make `gvm content sync` the one verb that
// could not be driven as a job at all.
func jobNameFor(action string) string { return strings.ReplaceAll(action, "_", "-") }

// actionFor is the action string for a kind, which is the same word by
// construction (see jobs.actionKinds): the word in the YAML, the kind in the
// ledger and the name in `systemctl list-timers` are one word, so there is
// nothing to translate when reading a journal.
func actionFor(kind lifecycle.JobKind) string {
	for _, a := range jobs.Actions() {
		if k, _ := jobs.KindForAction(a); k == kind {
			return a
		}
	}
	return string(kind)
}

// ----------------------------------------------------------------------
// gvm backup list

func runBackupList(ctx context.Context, env *Env, args []string) error {
	var only, from string
	var asJSON bool
	var remote string
	fs := flag.NewFlagSet("gvm backup list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm backup list [<instance>]\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&only, "instance", "", "list one instance's archives")
	fs.StringVar(&from, "from", "",
		"a directory to read instead of this instance's own (legacy bash-suite archives too)")
	fs.BoolVar(&asJSON, "json", false, "print the listing as JSON")
	fs.StringVar(&remote, "remote", "", "dashboard URL to run this against instead of the local host")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneOrNoInstance(pos, only)
	if err != nil {
		fs.Usage()
		return err
	}
	t, err := Resolve(env, "backup", remote)
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

	dirs, err := archiveDirs(h, name, from)
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
	if asJSON {
		return writeJSON(env.Stdout, all)
	}
	printArchiveRows(env.Stdout, all)
	return nil
}

// archiveRow is one archive as the index remembers it.
type archiveRow struct {
	Dir        string    `json:"dir"`
	File       string    `json:"file"`
	Instance   string    `json:"instance,omitempty"`
	Class      string    `json:"class,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Bytes      int64     `json:"bytes"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	VerifiedAt time.Time `json:"verified_at,omitzero"`
	Legacy     bool      `json:"legacy,omitempty"`
	Unreadable string    `json:"unreadable,omitempty"`
	PinnedBy   string    `json:"pinned_by,omitempty"`
}

func archiveDirs(h *HostEnv, name, from string) ([]string, error) {
	if from != "" {
		if !filepath.IsAbs(from) {
			return nil, fmt.Errorf("--from %q is not an absolute path", from)
		}
		return []string{from}, nil
	}
	if name != "" {
		return []string{h.backupDir(name)}, nil
	}
	store := &jobs.ArchiveStore{Root: instance.BackupRoot, Files: h.files()}
	names, err := store.Instances(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, h.backupDir(n))
	}
	return out, nil
}

// archiveRows reads one directory's index, falling back to peeking the files.
//
// The fallback is not an optimisation in reverse: a box restored from a USB
// stick, or one whose archives were rsynced from the old server, has tarballs
// and no index at all -- and `gvm restore --list` has to work there, which is
// section 14.6's promise in its most literal form.
func archiveRows(h *HostEnv, dir string) ([]archiveRow, error) {
	ix, err := archive.LoadIndex(dir)
	if err != nil {
		return nil, err
	}
	if len(ix.Entries) == 0 {
		return peekRows(h, dir)
	}
	out := make([]archiveRow, 0, len(ix.Entries))
	for _, e := range ix.Entries {
		out = append(out, archiveRow{
			Dir: dir, File: e.File, Instance: e.Instance, Class: string(e.Class),
			Reason: e.Reason, Bytes: e.Bytes, CreatedAt: e.CreatedAt,
			VerifiedAt: e.VerifiedAt, Legacy: e.Legacy, Unreadable: e.Unreadable,
			PinnedBy: e.PinnedBy,
		})
	}
	return out, nil
}

// peekRows reads each archive's first member, which is the manifest.
//
// Every archive carries .gvm/manifest.json as its FIRST member precisely so
// this costs one ~8 KB read rather than decompressing the whole thing
// .
func peekRows(h *HostEnv, dir string) ([]archiveRow, error) {
	ents, err := h.files().ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []archiveRow
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archive.Ext) {
			continue
		}
		row := archiveRow{Dir: dir, File: e.Name()}
		m, err := archive.PeekManifestFile(filepath.Join(dir, e.Name()))
		switch {
		case err != nil:
			// Reported, never dropped. An archive that cannot be read is the
			// single most important row in this listing.
			row.Unreadable = err.Error()
		default:
			row.Instance, row.Class = m.Instance.Name, string(m.Class)
			row.Reason, row.CreatedAt = m.Reason, m.CreatedAt
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

func printArchiveRows(w io.Writer, rows []archiveRow) {
	if len(rows) == 0 {
		fmt.Fprint(w, "\nno archives\n")
		return
	}
	fmt.Fprint(w, "\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "ARCHIVE\tCLASS\tSIZE\tTAKEN\tVERIFIED\tREASON\n")
	var preserve, total int64
	for _, r := range rows {
		class := r.Class
		if r.Legacy {
			class = "legacy"
		}
		if r.PinnedBy != "" {
			// A durable preserve pin from a sidecar, e.g. the archive that
			// authorised a destroy. Named so the operator knows why it will
			// never be pruned (cli-ergonomics-7).
			class += " (pinned: " + r.PinnedBy + ")"
		}
		verified := "never"
		if !r.VerifiedAt.IsZero() {
			verified = r.VerifiedAt.UTC().Format("2006-01-02")
		}
		taken := ""
		if !r.CreatedAt.IsZero() {
			taken = r.CreatedAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.File, class,
			spec.FormatBytes(r.Bytes), taken, verified, r.Reason)
		total += r.Bytes
		if r.Class == string(archive.ClassPreserve) {
			preserve += r.Bytes
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n  %d archive(s), %s\n", len(rows), spec.FormatBytes(total))
	if preserve > 0 {
		// Section 13.2's reason for printing this at all: preserve archives
		// are pruned by no code path, so the only thing between them and a
		// full disk is somebody noticing.
		fmt.Fprintf(w, "  %s of that is `preserve`, which no code path deletes; `rm` by a "+
			"human is the only way\n", spec.FormatBytes(preserve))
	}
	for _, r := range rows {
		if r.Unreadable != "" {
			fmt.Fprintf(w, "  UNREADABLE %s: %s\n", r.File, r.Unreadable)
		}
	}
}

// ----------------------------------------------------------------------
// gvm backup delete

// runBackupDelete is the one explicit way to remove an archive, preserve
// archives included. Retention never removes a preserve archive and
// archive.DeleteArchive refuses one, so without this verb reclaiming a
// destroy-authorising or pre-rebuild archive means `rm` under /var/backups as
// root -- which bypasses the index, the archive-first guard and any record that
// it happened. This routes that same intent through a typed instance name, a
// guard that will not let the last verified copy of a live world go, and a
// backup.delete audit row (cli-ergonomics-7, offsite-backup-dr-12,
// failure-recovery-12, rule 5).
func runBackupDelete(ctx context.Context, env *Env, args []string) error {
	var confirm, remote string
	var dry bool
	fs := flag.NewFlagSet("gvm backup delete", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm backup delete <archive> [--dry-run]\n\n"+
			"Removes one archive after you type its instance's name. It is the only\n"+
			"gvm path that will remove a `preserve` archive. It refuses the newest\n"+
			"verified archive of an instance that still exists, so a live world\n"+
			"always keeps at least one verified copy.\n\n")
		flagUsage(env.Stderr, fs)
	}
	fs.StringVar(&confirm, "confirm", "", "type the instance name here instead of at the prompt")
	fs.BoolVar(&dry, "dry-run", false, "show what would be removed, and remove nothing")
	fs.StringVar(&remote, "remote", "", "dashboard URL to run this against instead of the local host")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return usageErr(errors.New("name exactly one archive to delete"))
	}

	t, err := Resolve(env, "backup", remote)
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

	// Resolve to the directory and basename, and read that directory's index --
	// the index is what knows the archive's instance, class, pin and verified
	// time, which are the facts every guard below is made of.
	abs, err := filepath.Abs(pos[0])
	if err != nil {
		return err
	}
	dir, file := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))

	e, ix, err := findArchiveEntry(ctx, h, dir, file)
	if err != nil {
		return err
	}
	if e.Instance == "" {
		// A legacy tarball names no instance, so there is no name to type. The
		// typed-name gate is the whole safety of this verb, and a gate with no
		// subject is not one -- so refuse rather than invent a subject.
		return refused(fmt.Errorf("%s carries no gvm manifest, so gvm cannot tell which instance it "+
			"belongs to and there is no name to confirm against; remove it by hand if you mean to", file))
	}
	inst := e.Instance
	exists := instanceExists(h, inst)

	// Archive-first. The newest verified archive of a world that still exists is
	// the copy a restore reaches for; removing it could leave a live instance
	// with no verified copy at all, which is the loss the whole archive chain
	// exists to prevent. An older archive, or any archive of an instance that is
	// gone, is fair game once the name is typed.
	if exists && newestVerified(ix, inst, file) {
		return refused(fmt.Errorf("%s is the newest verified archive of %s, which still exists, so gvm "+
			"will not remove the one copy a restore would use. Take a fresh backup first "+
			"(gvm backup %s), then delete this one, or delete an older archive", file, inst, inst))
	}

	printDeletePreflight(env.Stdout, dir, e, exists, h.now())
	if dry {
		fmt.Fprintln(env.Stdout, "\n  nothing was removed (--dry-run).")
		return nil
	}
	if err := (TypedConfirm{Subject: inst}).confirmFrom(env, confirm); err != nil {
		return err
	}

	full := filepath.Join(dir, file)
	ev := audit.Event{
		Actor: actorFor(t), Action: audit.BackupDelete, Instance: inst,
		Params: audit.Params{"path": full, "sha256": e.SHA256, "bytes": e.Bytes},
	}
	if err := h.audit().Do(ctx, ev, func(context.Context) (audit.Params, error) {
		return nil, archive.ForceDeleteArchive(full)
	}); err != nil {
		return fmt.Errorf("removing %s: %w", file, err)
	}

	// Refresh the index so `gvm backup list` stops showing it at once, and so
	// the "last copy" check below reads the directory as it is now.
	ix2, _ := archive.Reindex(ctx, dir, archive.ReindexOptions{Now: h.Now})

	fmt.Fprintf(env.Stdout, "\n  removed %s (%s)\n", full, spec.FormatBytes(e.Bytes))
	if !exists && ix2 != nil && countInstanceArchives(ix2, inst) == 0 {
		// The one delete that a fresh backup cannot undo: the instance is gone,
		// and this was its last archive. Saying so is the point.
		fmt.Fprintf(env.Stdout, "  this was the last copy of %s's world\n", inst)
	}
	return nil
}

// findArchiveEntry locates the archive's index entry, preferring the existing
// index (no rescan, no write) and rebuilding it from the directory only when the
// cache does not know the file -- a box restored from a USB stick, or one whose
// archives were rsynced in, has tarballs and no index.
func findArchiveEntry(ctx context.Context, h *HostEnv, dir, file string) (archive.Entry, *archive.Index, error) {
	ix, err := archive.LoadIndex(dir)
	if err != nil {
		return archive.Entry{}, nil, fmt.Errorf("reading the archive index in %s: %w", dir, err)
	}
	if e, ok := ix.Find(file); ok {
		return e, ix, nil
	}
	ix, err = archive.Reindex(ctx, dir, archive.ReindexOptions{Now: h.Now})
	if err != nil {
		return archive.Entry{}, nil, fmt.Errorf("reading the archive directory %s: %w", dir, err)
	}
	e, ok := ix.Find(file)
	if !ok {
		return archive.Entry{}, nil, usageErr(fmt.Errorf("%s is not in %s", file, dir))
	}
	return e, ix, nil
}

// instanceExists reports whether an instance's definition is still on the box.
// It stats the YAML rather than parsing it: a spec present but unparsable is an
// instance that still exists, and the archive-first guard must hold for it too.
func instanceExists(h *HostEnv, name string) bool {
	_, err := h.files().Stat(h.paths(name).Spec)
	return err == nil
}

// newestVerified reports whether file is the newest VERIFIED archive of inst in
// ix. Newest is by CreatedAt, and only archives this host has read back
// (VerifiedAt set) count: an archive gvm has never verified is not yet a copy a
// restore can be promised, so protecting it would refuse a delete on the
// strength of bytes nobody has checked.
func newestVerified(ix *archive.Index, inst, file string) bool {
	newest := ""
	var newestAt time.Time
	for _, e := range ix.Entries {
		if e.Instance != inst || e.VerifiedAt.IsZero() {
			continue
		}
		if newest == "" || e.CreatedAt.After(newestAt) {
			newest, newestAt = e.File, e.CreatedAt
		}
	}
	return newest != "" && newest == file
}

func countInstanceArchives(ix *archive.Index, inst string) int {
	n := 0
	for _, e := range ix.Entries {
		if e.Instance == inst {
			n++
		}
	}
	return n
}

func printDeletePreflight(w io.Writer, dir string, e archive.Entry, exists bool, now time.Time) {
	fmt.Fprintf(w, "\ndelete %s\n", filepath.Join(dir, e.File))
	fmt.Fprintf(w, "  instance  %s", e.Instance)
	if !exists {
		fmt.Fprint(w, " (no longer exists)")
	}
	fmt.Fprintln(w)
	class := string(e.Class)
	if e.PinnedBy != "" {
		class += " (pinned: " + e.PinnedBy + ")"
	}
	fmt.Fprintf(w, "  class     %s\n", class)
	if e.Reason != "" {
		fmt.Fprintf(w, "  reason    %s\n", e.Reason)
	}
	if !e.CreatedAt.IsZero() {
		taken := e.CreatedAt.UTC().Format("2006-01-02 15:04")
		if !now.IsZero() {
			days := int(now.Sub(e.CreatedAt).Hours() / 24)
			fmt.Fprintf(w, "  taken     %s (%d days ago)\n", taken, days)
		} else {
			fmt.Fprintf(w, "  taken     %s\n", taken)
		}
	}
	if !e.VerifiedAt.IsZero() {
		fmt.Fprintf(w, "  verified  %s\n", e.VerifiedAt.UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(w, "  size      %s\n", spec.FormatBytes(e.Bytes))
}
