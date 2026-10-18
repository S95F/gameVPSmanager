package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Policy is retention, and it has exactly two classes (section 13.2).
type Policy struct {
	// Keep is how many routine archives survive regardless of age. Zero means
	// "no count limit", and so does an absent backup block in the instance
	// spec -- the two are indistinguishable here, which is why zero cannot be
	// allowed to mean "keep nothing". A policy that keeps nothing is always a
	// typo; --force is how an operator says it on purpose.
	Keep int
	// KeepDays removes routine archives older than D days, but only ones the
	// Keep window has already let go. Zero means "no age limit".
	KeepDays int
	// Force drops the Keep and KeepDays windows and removes every routine
	// archive except the one just written. It does NOT touch preserve: that
	// is the promise of section 13.2, and --force is precisely the moment
	// somebody would otherwise talk themselves into it.
	Force bool
	// DryRun computes the plan and deletes nothing.
	DryRun bool
	Now    func() time.Time
	OnNote func(string)
}

// PruneResult is what retention did, or would have done.
type PruneResult struct {
	Dir string `json:"dir"`
	// Removed and Kept are basenames, newest first.
	Removed []string `json:"removed,omitempty"`
	Kept    []string `json:"kept,omitempty"`
	// Preserved lists the class-preserve archives that were skipped, and
	// PreservedBytes their total. doctor reports it so an operator notices
	// before the disk fills, which is the price of "never pruned, by any code
	// path".
	Preserved      []string `json:"preserved,omitempty"`
	PreservedBytes int64    `json:"preserved_bytes"`
	// Unmanaged lists archives gvm did not write -- bash-suite tarballs,
	// anything hand-copied in. They are never pruned.
	Unmanaged  []string `json:"unmanaged,omitempty"`
	FreedBytes int64    `json:"freed_bytes"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// Prune enforces retention, and only ever after a successful create.
//
// after is the archive that was just written and verified, and it is required.
// backup.sh pruned on a schedule with the prune after the tar, which gave it
// this invariant by accident; the moment a backup failed under that shape --
// disk full, world locked, steamcmd holding a file -- the prune would still
// have run and eaten the good archives, leaving nothing. Making the fresh
// receipt an argument means a failing create returns nil, the caller has
// nothing to pass, and history cannot erode.
//
// It is an error rather than a panic, unlike Destroy: nothing is lost by
// refusing to prune, so the caller can log it and carry on, whereas a destroy
// that got this far has already been told it may delete a world.
func Prune(ctx context.Context, dir string, p Policy, after *Ref) (*PruneResult, error) {
	now := clockOf(p.Now)
	res := &PruneResult{Dir: dir, DryRun: p.DryRun}

	if after == nil {
		return res, fmt.Errorf("%w: no archive was written, so there is nothing to prune after; "+
			"a failing backup must never erode the history it failed to add to", ErrNoFreshArchive)
	}
	if !after.Verified() {
		return res, fmt.Errorf("%w: %s was written but never read back. Retention runs after a "+
			"new archive has been written AND verified, because deleting yesterday's world to "+
			"make room for a truncated one is the failure this ordering exists to prevent",
			ErrNoFreshArchive, after.Path)
	}
	fresh := filepath.Base(after.Path)
	if _, err := os.Stat(after.Path); err != nil {
		return res, fmt.Errorf("%w: %s does not exist: %v", ErrNoFreshArchive, after.Path, err)
	}
	if filepath.Clean(filepath.Dir(after.Path)) != filepath.Clean(dir) {
		return res, fmt.Errorf("%w: %s is not in %s, so it is not the archive this directory's "+
			"history was just extended by", ErrNoFreshArchive, after.Path, dir)
	}

	ix, err := Reindex(ctx, dir, ReindexOptions{Now: p.Now, OnNote: p.OnNote})
	if err != nil {
		return res, err
	}

	var routine []Entry
	for _, e := range ix.Entries {
		switch {
		case e.File == fresh:
			// The archive that authorised this prune is never a candidate for
			// it, whatever the policy says.
			res.Kept = append(res.Kept, e.File)
		case e.Class == ClassPreserve:
			res.Preserved = append(res.Preserved, e.File)
			res.PreservedBytes += e.Bytes
		case e.Instance != "" && after.Instance != "" && e.Instance != after.Instance:
			// A shared backup.dir can hold several instances' archives. Retention
			// for one instance must never reclaim another's: the fresh archive
			// names whose history this prune is extending, and an archive from a
			// different instance is simply not this run's to delete
			// (offsite-backup-dr-2).
			res.Unmanaged = append(res.Unmanaged, e.File)
		case e.Legacy || e.Class != ClassRoutine:
			// Not ours. backup.sh's tarballs, a file somebody scp'd in, an
			// archive whose class did not parse. Deleting somebody else's
			// backups on a schedule is not a feature, and an unrecognised
			// class must never default to routine -- that is the one
			// direction in which a mistake is unrecoverable.
			res.Unmanaged = append(res.Unmanaged, e.File)
		default:
			routine = append(routine, e)
		}
	}

	// Newest first. Reindex already sorts this way; sorting again makes the
	// rule independent of that.
	sort.Slice(routine, func(i, j int) bool { return routine[i].CreatedAt.After(routine[j].CreatedAt) })

	cutoff := time.Time{}
	if p.KeepDays > 0 {
		cutoff = now().UTC().AddDate(0, 0, -p.KeepDays)
	}

	// The fresh archive occupies a slot of the keep window when it is itself
	// routine, so `keep: 1` with a nightly just written means exactly that
	// nightly -- which is what an operator reading "keep: 1" expects. A fresh
	// PRESERVE archive consumes no slot, because preserve archives are not
	// what keep:N is counting.
	reserved := 0
	if after.Class == ClassRoutine {
		reserved = 1
	}

	var doomed []Entry
	for i, e := range routine {
		switch {
		case p.Force:
			doomed = append(doomed, e)
		case p.Keep <= 0 && p.KeepDays <= 0:
			// Neither window was declared, which the fields above document as
			// "no count limit" and "no age limit" -- so nothing has been
			// crossed and nothing goes. Falling through to default here would
			// make a zero-valued Policy byte-for-byte identical to --force,
			// and a zero-valued Policy is exactly what an instance spec with
			// no backup block produces: spec.BackupPolicy's fields are plain
			// ints, there is no loader default, and ValidateInstance says
			// nothing about them, so absent and explicit are indistinguishable
			// by the time they arrive here. Only an explicit limit may doom an
			// archive, because this is the one direction in which a mistake is
			// unrecoverable.
			res.Kept = append(res.Kept, e.File)
		case p.Keep > 0 && i+reserved < p.Keep:
			res.Kept = append(res.Kept, e.File)
		case p.KeepDays > 0 && !e.CreatedAt.Before(cutoff):
			res.Kept = append(res.Kept, e.File)
		default:
			doomed = append(doomed, e)
		}
	}

	for _, e := range doomed {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if p.DryRun {
			res.Removed = append(res.Removed, e.File)
			res.FreedBytes += e.Bytes
			continue
		}
		full := filepath.Join(dir, e.File)
		if err := os.Remove(full); err != nil {
			if p.OnNote != nil {
				p.OnNote(fmt.Sprintf("could not prune %s: %v", e.File, err))
			}
			res.Kept = append(res.Kept, e.File)
			continue
		}
		res.Removed = append(res.Removed, e.File)
		res.FreedBytes += e.Bytes
	}

	if !p.DryRun && len(res.Removed) > 0 {
		if _, err := Reindex(ctx, dir, ReindexOptions{Now: p.Now, OnNote: p.OnNote}); err != nil {
			return res, err
		}
	}
	sort.Strings(res.Removed)
	sort.Strings(res.Kept)
	sort.Strings(res.Preserved)
	sort.Strings(res.Unmanaged)
	return res, nil
}

// PreserveBytes totals the class-preserve archives in a directory.
//
// doctor reports it (section 13.2): "never pruned by any code path" is only a
// safe promise if somebody is told how much disk it is costing.
func PreserveBytes(ctx context.Context, dir string) (int64, []string, error) {
	ix, err := Reindex(ctx, dir, ReindexOptions{})
	if err != nil {
		return 0, nil, err
	}
	var total int64
	var files []string
	for _, e := range ix.Entries {
		if e.Class == ClassPreserve {
			total += e.Bytes
			files = append(files, e.File)
		}
	}
	return total, files, nil
}

// ErrPreserveIsNeverPruned is returned by DeleteArchive when asked to remove a
// preserve archive.
//
// The only way to remove one is a human with rm. That is the whole point: it
// is rebuild.sh's "kept until you delete it -- not pruned by backup retention"
// promoted from a log line into something no code path can talk itself past.
var ErrPreserveIsNeverPruned = errors.New("archive: a preserve archive is never deleted by gvm")

// ErrClassUnknown means gvm cannot establish what an archive is.
//
// It is distinct from ErrPreserveIsNeverPruned because a future `gvm backup rm`
// with explicit operator intent has to be able to tell "I am forbidden" from "I
// do not know what this is" -- and because Prune already leaves this shape in
// Unmanaged rather than reclaiming it.
var ErrClassUnknown = errors.New("archive: cannot establish this archive's class")

// DeleteArchive removes a single archive by path, refusing preserve.
//
// It exists so that every deletion of an archive in gvm goes through one place
// that knows the rule, rather than through whichever os.Remove was nearest.
//
// When the head manifest cannot be read, the FILENAME decides. That is the only
// class signal that survives a truncation or a shredded gzip, gvm wrote it
// itself through Filename(), and it can only err toward refusing -- which is
// the direction this package takes everywhere else: the index reports an
// unreadable archive rather than dropping it from a listing, and Prune routes
// an unrecognised class to Unmanaged because that is the one direction in which
// a mistake is unrecoverable. Consulting only a manifest that PARSES would make
// every peek failure a silent deletion of exactly the archive class the spec
// says no code path may reclaim.
func DeleteArchive(path string) error {
	man, err := PeekManifestFile(path)
	switch {
	case err == nil:
		if man.Class == ClassPreserve {
			return fmt.Errorf("%w: %s was taken %s (%s). Remove it by hand if you mean to",
				ErrPreserveIsNeverPruned, path, man.CreatedAt.Format(time.RFC3339), man.Reason)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Deletion stays idempotent, and the caller gets the natural fs error
		// rather than a class refusal for something already done.
	default:
		_, _, class, ok := ParseFilename(filepath.Base(path))
		switch {
		case !ok:
			return fmt.Errorf("%w: %s could not be read (%v) and its name is not one gvm wrote, "+
				"so there is nothing left to say what it is. Remove it by hand if you mean to",
				ErrClassUnknown, path, err)
		case class == ClassPreserve:
			return fmt.Errorf("%w: %s is named as a preserve archive and could not be read (%v), "+
				"which is itself worth looking at. Remove it by hand if you mean to",
				ErrPreserveIsNeverPruned, path, err)
		}
	}
	return os.Remove(path)
}

// ForceDeleteArchive removes an archive and its preserve sidecar regardless of
// class. It is the one deliberate, operator-gated bypass of the never-prune
// rule DeleteArchive enforces: `gvm backup delete` reaches it only after the
// operator has typed the instance name, so the invariant "a preserve archive is
// removed only by explicit human action" is kept intact — the human is typing a
// name to gvm instead of reaching for rm, which is strictly safer because gvm
// can still refuse to remove the last verified copy of a live world.
//
// It is idempotent: a missing archive and a missing sidecar are both success,
// so a delete interrupted after the tarball went but before the pin did
// completes cleanly on a second run rather than erroring on the half it already
// finished.
func ForceDeleteArchive(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(PreserveSidecarPath(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
