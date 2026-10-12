package jobs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ArchiveStore is the production Archives: the `prune` and `verify` kinds'
// housekeeping over /var/backups/gvm.
//
// Every rule about what may be deleted lives in internal/archive and none of
// them is restated here. What this file contributes is the one thing that
// package deliberately refuses to decide for a standalone prune: WHICH archive
// is the receipt.
//
// archive.Prune requires a fresh, verified Ref and returns an error without
// one, because backup.sh's prune ran on a schedule and would have eaten the
// good archives the first night its tar failed. A nightly `prune` job has no
// fresh archive -- it did not write one. So this adapter produces the receipt
// honestly: it takes the NEWEST routine archive already in the directory and
// VERIFIES it, and prunes only if that verification passes. The consequence is
// exactly the right one: a directory whose newest archive is truncated prunes
// nothing and says so, rather than deleting last week's intact worlds to make
// room for a broken one.
//
// The four archive-package calls are fields. That is rule 5 of this project
// rather than indirection for its own sake: internal/archive reads the real
// filesystem through os, so without the seams the only way to drive "the
// newest archive does not verify, so nothing is pruned" would be to build a
// corrupt 2 GB tarball on a Linux box. With them it is a table test on the
// development machine, and the production wiring is still the package's own
// functions by default.
type ArchiveStore struct {
	// Root is the archive root, /var/backups/gvm. It is only used to
	// enumerate instances; each instance's directory comes from Dir.
	Root string
	// Dir is where one instance's archives live. Nil is PathsFor(n).Backups,
	// which honours the default layout; an instance whose spec sets
	// backup.dir needs this.
	Dir func(instanceName string) string
	// Files reads the root directory. Nil is lifecycle.OSFiles.
	Files lifecycle.Files

	// Policy is the retention to apply, per instance, from its own spec.
	// Nil yields the zero policy, which has no count and no age limit and
	// therefore deletes NOTHING -- the safe direction for a missing
	// configuration, and the reason an absent backup block cannot be read as
	// "keep nothing" (see archive.Policy.Keep).
	Policy func(instanceName string) (archive.Policy, error)

	// Receipt is how the archive that AUTHORISES a prune is read back. Zero
	// is shallow -- gzip CRC, manifest parse, member count and whole-file
	// sha256 -- which is section 13.2's default for a nightly and is seconds
	// per archive. It is deliberately not a spot check: a spot extracts
	// members into scratch space, and the thing being established here is
	// that the newest archive is intact enough to be history, not that it is
	// safe to delete a world with.
	Receipt archive.VerifyOptions
	// Shallow and Deep are the `verify` kind's two modes.
	Shallow archive.VerifyOptions
	Deep    archive.VerifyOptions

	Now   func() time.Time
	Notes func(string)

	// The injected archive-package calls. Nil uses the real one.
	Reindexer func(ctx context.Context, dir string, o archive.ReindexOptions) (*archive.Index, error)
	Verifier  func(ctx context.Context, path string, o archive.VerifyOptions) (*archive.Report, error)
	Receipter func(ctx context.Context, ref *archive.Ref, o archive.VerifyOptions) (*archive.Report, error)
	Retainer  func(ctx context.Context, dir string, p archive.Policy, after *archive.Ref) (*archive.PruneResult, error)
}

// Compile-time proof that this is what the `prune` and `verify` runners ask
// for.
var _ Archives = (*ArchiveStore)(nil)

// ErrNoReceipt is a prune with nothing it may run after.
//
// Its own sentinel because the operator's next move is specific and is not
// "retry": there is an archive directory whose newest routine archive did not
// read back, and the thing to do is look at it.
var ErrNoReceipt = errors.New("jobs: retention has no verified archive to run after")

func (a *ArchiveStore) files() lifecycle.Files {
	if a.Files != nil {
		return a.Files
	}
	return lifecycle.OSFiles{}
}

func (a *ArchiveStore) dir(name string) string {
	if a.Dir != nil {
		return a.Dir(name)
	}
	return instance.PathsFor(name).Backups
}

func (a *ArchiveStore) note(format string, args ...any) {
	if a.Notes != nil {
		a.Notes(fmt.Sprintf(format, args...))
	}
}

// Instances lists the instances with an archive directory.
//
// From the filesystem, never from a database: section 14.6 promises `gvm
// restore --list` and the nightly verify work with Postgres down, and the
// host-level `_verify` timer is the job that most needs that to be true. A
// directory whose name is not a legal instance name is SKIPPED with a note
// rather than refused, because /var/backups is a place operators put things
// and one stray directory must not stop the sweep.
func (a *ArchiveStore) Instances(_ context.Context) ([]string, error) {
	root := a.Root
	if root == "" {
		root = instance.BackupRoot
	}
	ents, err := a.files().ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		// A box that has never taken a backup. Not an error: the verify timer
		// fires on a fresh install too, and failing it would train the
		// operator to ignore the one that matters.
		a.note("%s does not exist yet, so there is nothing to verify or prune", root)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: reading %s: %w", root, err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if instance.ValidateName(e.Name()) != nil {
			a.note("%s is not an instance name, so %s was not scanned",
				e.Name(), path.Join(root, e.Name()))
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// Prune applies retention to one instance, after verifying the archive that
// authorises it.
func (a *ArchiveStore) Prune(ctx context.Context, instanceName string) (int, error) {
	dir := a.dir(instanceName)
	ix, ok, err := a.index(ctx, dir)
	if err != nil || !ok {
		return 0, err
	}
	pol := archive.Policy{}
	if a.Policy != nil {
		if pol, err = a.Policy(instanceName); err != nil {
			return 0, fmt.Errorf("jobs: reading %s's retention policy: %w", instanceName, err)
		}
	}
	pol.Now, pol.OnNote = a.Now, a.Notes

	ref, err := a.receipt(ctx, dir, ix)
	if err != nil {
		return 0, err
	}
	res, err := a.retain(ctx, dir, pol, ref)
	if err != nil {
		return 0, err
	}
	if res.PreservedBytes > 0 {
		// Section 13.2: preserve archives are never pruned by any code path,
		// so the only thing that stops them filling a disk is somebody
		// noticing. Saying so on every prune is the cheapest place to notice.
		a.note("%s holds %d preserve archive(s), %s, which no code path deletes: %s",
			dir, len(res.Preserved), spec.FormatBytes(res.PreservedBytes),
			strings.Join(res.Preserved, " "))
	}
	return len(res.Removed), nil
}

// receipt is the newest routine archive, read back.
//
// Newest, because it is the one whose existence justifies letting an older one
// go. Routine, because a preserve archive is not part of the history retention
// manages and using one as the receipt would authorise a prune on the strength
// of a file retention is not allowed to touch.
func (a *ArchiveStore) receipt(ctx context.Context, dir string, ix *archive.Index) (*archive.Ref, error) {
	var newest *archive.Entry
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if e.Legacy || e.Class != archive.ClassRoutine || e.Unreadable != "" {
			continue
		}
		if newest == nil || e.CreatedAt.After(newest.CreatedAt) {
			newest = e
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("%w: %s holds no routine archive, so there is nothing retention "+
			"could have been extended by. A prune that ran anyway would be deleting history on "+
			"the strength of nothing", ErrNoReceipt, dir)
	}
	ref := newest.Ref(dir)
	// VerifiedAt is cleared before the check so that the index's memory of an
	// earlier verification cannot stand in for this one. The index is a cache
	// (archive/index.go says so), and a cache must not be able to vouch for
	// bytes that authorise a deletion.
	ref.VerifiedAt = time.Time{}
	if _, err := a.verifyRef(ctx, ref, a.Receipt); err != nil {
		return nil, fmt.Errorf("%w: %s is the newest archive in %s and it did not read back, so "+
			"nothing was pruned: %w", ErrNoReceipt, newest.File, dir, err)
	}
	if !ref.Verified() {
		return nil, fmt.Errorf("%w: %s verified without being stamped, which means the check "+
			"compared nothing", ErrNoReceipt, newest.File)
	}
	return ref, nil
}

// Reindex rebuilds one directory's index.
func (a *ArchiveStore) Reindex(ctx context.Context, instanceName string) error {
	_, _, err := a.index(ctx, a.dir(instanceName))
	return err
}

// VerifyShallow checks every archive in one directory: gzip CRC, manifest
// parse, member count and whole-file sha256.
func (a *ArchiveStore) VerifyShallow(ctx context.Context, instanceName string) (int, error) {
	o := a.Shallow
	if o.Mode == "" {
		o.Mode = archive.ModeShallow
	}
	return a.verifyAll(ctx, instanceName, o)
}

// VerifyNewestPreserve re-hashes every member of the newest preserve archive.
//
// The newest preserve archive is what a rebuild, a restore or an adopt cutover
// left behind, which makes it the one a restore is most likely to reach for --
// and therefore the one whose truncation must be discovered on a quiet night
// rather than during the restore it was taken for.
func (a *ArchiveStore) VerifyNewestPreserve(ctx context.Context, instanceName string) (int, error) {
	dir := a.dir(instanceName)
	ix, ok, err := a.index(ctx, dir)
	if err != nil || !ok {
		return 0, err
	}
	var newest *archive.Entry
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if e.Class != archive.ClassPreserve || e.Legacy {
			continue
		}
		if newest == nil || e.CreatedAt.After(newest.CreatedAt) {
			newest = e
		}
	}
	if newest == nil {
		return 0, nil
	}
	o := a.Deep
	if o.Mode == "" {
		o.Mode = archive.ModeDeep
	}
	return a.verifyOne(ctx, dir, newest, o)
}

// verifyAll runs one mode over every archive in one instance's directory.
//
// A failure on one archive is collected and the scan continues. The whole
// point of a nightly verification is the SHAPE of the damage: stopping at the
// first bad archive would report one problem on a box that had four.
func (a *ArchiveStore) verifyAll(ctx context.Context, instanceName string,
	o archive.VerifyOptions) (int, error) {

	dir := a.dir(instanceName)
	ix, ok, err := a.index(ctx, dir)
	if err != nil || !ok {
		return 0, err
	}
	var checked int
	var errs []error
	for i := range ix.Entries {
		e := &ix.Entries[i]
		n, verr := a.verifyOne(ctx, dir, e, o)
		checked += n
		if verr != nil {
			errs = append(errs, verr)
		}
	}
	return checked, errors.Join(errs...)
}

func (a *ArchiveStore) verifyOne(ctx context.Context, dir string, e *archive.Entry,
	o archive.VerifyOptions) (int, error) {

	if o.Now == nil {
		o.Now = a.Now
	}
	// Through Entry.Ref rather than joining here, so there is exactly one
	// derivation of "where does this index entry's file live" in gvm.
	p := e.Ref(dir).Path
	rep, err := a.verify(ctx, p, o)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", e.File, err)
	}
	if err := rep.Err(); err != nil {
		return 1, fmt.Errorf("%s: %w", e.File, err)
	}
	if rep.Legacy {
		// Reported and counted: a bash-suite tarball with no manifest is old
		// rather than broken, and leaving it out of the count would make the
		// nightly's "verified 7 archives" quietly mean "verified 7 of 9".
		a.note("%s carries no manifest, so only its gzip stream was checked", e.File)
	}
	return 1, nil
}

// index reindexes a directory, reporting absence as "nothing here".
//
// The bool is not redundant with the error: an instance whose archive
// directory has never been created is the normal state between `gvm create`
// and the first backup, and a nightly verify that failed on it would be a
// nightly that fails on every new instance for a day.
func (a *ArchiveStore) index(ctx context.Context, dir string) (*archive.Index, bool, error) {
	if _, err := a.files().Stat(dir); errors.Is(err, fs.ErrNotExist) {
		a.note("%s does not exist, so there is nothing to scan", dir)
		return nil, false, nil
	}
	ix, err := a.reindex(ctx, dir, archive.ReindexOptions{Now: a.Now, OnNote: a.Notes})
	if err != nil {
		return nil, false, fmt.Errorf("jobs: reindexing %s: %w", dir, err)
	}
	return ix, true, nil
}

func (a *ArchiveStore) reindex(ctx context.Context, dir string, o archive.ReindexOptions) (*archive.Index, error) {
	if a.Reindexer != nil {
		return a.Reindexer(ctx, dir, o)
	}
	return archive.Reindex(ctx, dir, o)
}

func (a *ArchiveStore) verify(ctx context.Context, p string, o archive.VerifyOptions) (*archive.Report, error) {
	if a.Verifier != nil {
		return a.Verifier(ctx, p, o)
	}
	return archive.Verify(ctx, p, o)
}

func (a *ArchiveStore) verifyRef(ctx context.Context, ref *archive.Ref, o archive.VerifyOptions) (*archive.Report, error) {
	if o.Now == nil {
		o.Now = a.Now
	}
	if a.Receipter != nil {
		return a.Receipter(ctx, ref, o)
	}
	return archive.VerifyRef(ctx, ref, o)
}

func (a *ArchiveStore) retain(ctx context.Context, dir string, p archive.Policy,
	after *archive.Ref) (*archive.PruneResult, error) {

	if a.Retainer != nil {
		return a.Retainer(ctx, dir, p, after)
	}
	return archive.Prune(ctx, dir, p, after)
}
