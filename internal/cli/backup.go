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
// scoped `either` (section 13.3) and the same table is `gvm backup verify
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
	{"verify", "[<instance>] [--deep]", "read archives back: gzip, manifest, digests", runBackupVerify},
	{"prune", "[<instance>]", "apply retention; preserve archives are never candidates", runBackupPrune},
	{"list", "[<instance>]", "what this host has archived, from the index", runBackupList},
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
	pos, err := parseInterspersed(fs, args)
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
		return "", fmt.Errorf("name one instance, not %d", len(pos))
	case len(pos) == 1 && flagName != "" && pos[0] != flagName:
		return "", fmt.Errorf("two different instances named: %q and --instance %q",
			pos[0], flagName)
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
		})
	}
	return out, nil
}

// peekRows reads each archive's first member, which is the manifest.
//
// Every archive carries .gvm/manifest.json as its FIRST member precisely so
// this costs one ~8 KB read rather than decompressing the whole thing
// (section 13.1).
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
