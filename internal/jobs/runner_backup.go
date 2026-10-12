package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `backup` kind: section 13.2, and the one kind that never stops the
// server.
//
// Two things in the table below are invariants rather than orderings.
//
// The FREEZE has an Undo, so the thaw runs on every path -- an error return, a
// panic, a cancelled context. D-31 explains why that is not enough on its own
// and why the out-of-process lease exists: a goroutine, or a defer, dies with
// its process, and the failure being guarded against is precisely the process
// dying between `save-off` and `save-on`, which leaves a running server that
// has silently stopped persisting. The thaw here is the fast path; the runner's
// lease timer is the one that survives SIGKILL. Both, not either.
//
// PRUNE runs only after a VERIFIED create, and it is not fatal. "A failing
// backup must never erode history" is the invariant backup.sh had by accident
// -- its `tar` precedes its `find | rm` -- made explicit. The seam takes the
// ref precisely so that pruning without one cannot be expressed, and the step
// re-checks it anyway, because this is the one place in the subsystem where a
// mistake deletes the only copy of a world.
//
// The policy is `fail`: a backup that did not happen is a gap in history and
// has to be loud. The server is untouched either way, which is what makes that
// affordable.

// Backups is the backup engine (internal/backup, section 13.2).
//
// Every method is one phase of the table below, and the names match the step
// names so the never-leaves-stopped test can enumerate its injection points
// from the table rather than from a list somebody has to remember to extend.
type Backups interface {
	// Preflight checks free disk against the estimate and refuses before
	// anything is frozen.
	Preflight(ctx context.Context) error
	// Freeze arms /run/gvm/<name>/freeze.lease and runs freeze.before
	// through the console (D-31).
	Freeze(ctx context.Context) error
	// Thaw runs freeze.after and unlinks the lease. It is idempotent: the
	// runner's own lease timer may have got there first.
	Thaw(ctx context.Context) error
	// Snapshot writes the archive -- reflink, btrfs subvolume or a straight
	// tar, whichever the filesystem supports.
	Snapshot(ctx context.Context, class archive.Class, reason string) (*archive.Ref, error)
	// Verify checks the archive it was just handed, and sets ref.VerifiedAt.
	Verify(ctx context.Context, ref *archive.Ref) error
	// Prune applies retention. `after` is the archive that was just written
	// and verified: retention only ever runs after one exists.
	Prune(ctx context.Context, after *archive.Ref) error
	// Push copies off-box. Failing must never fail the local backup.
	Push(ctx context.Context, ref *archive.Ref) error
}

var backupRunner = &Runner{
	Kind:    lifecycle.JobBackup,
	Scope:   ScopeInstance,
	Policy:  PolicyFail,
	Stops:   false,
	Summary: "freeze, snapshot, verify, then prune",
	Steps: func(env *Env) ([]Step, error) {
		b := env.Backups
		if b == nil {
			return nil, fmt.Errorf("%w: a backup job needs a backup engine (Env.Backups)", ErrEnv)
		}
		// The ref lives in the closure, which is why Steps is a function.
		// Passing it through the Run's summary map would make the prune's
		// precondition a type assertion on an `any`, and "only after a
		// verified create" is not a thing to express with a type assertion.
		var ref *archive.Ref

		return []Step{
			{
				Name: "backup.preflight", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error { return b.Preflight(ctx) },
			},
			{
				Name: "backup.freeze", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error { return b.Freeze(ctx) },
				// The thaw is this step's own defer. It runs for every exit
				// path, including a panic in the snapshot.
				Undo: func(ctx context.Context, jr *Run) error { return b.Thaw(ctx) },
			},
			{
				Name: "backup.snapshot", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					// ClassRoutine, always. A `preserve` archive is produced
					// by the thing that is about to be irreversible -- a
					// rebuild, a restore, a cutover -- and never by the
					// nightly, because preserve archives are never pruned by
					// any code path and a nightly that produced them would
					// fill the disk by design (section 13.2).
					r, err := b.Snapshot(ctx, archive.ClassRoutine, "routine")
					if err != nil {
						return err
					}
					if r == nil {
						return errors.New("the backup engine reported success and returned no " +
							"archive; a backup nobody can name is not a backup")
					}
					ref = r
					jr.Set("archive_path", r.Path)
					jr.Set("archive_bytes", r.Bytes)
					jr.Set("archive_sha256", r.SHA256)
					return nil
				},
			},
			{
				Name: "backup.verify", Fatal: true,
				Do: func(ctx context.Context, jr *Run) error {
					if ref == nil {
						return errors.New("nothing to verify")
					}
					if err := b.Verify(ctx, ref); err != nil {
						return err
					}
					if !ref.Verified() {
						// archive.Ref.Verified() is what authorizes a destroy
						// and what authorizes a prune. An engine that
						// returned nil without stamping VerifiedAt would make
						// every later check vacuous, so it is caught here
						// rather than trusted.
						return errors.New("the engine reported the archive verified and left " +
							"VerifiedAt zero; a Ref that has never been verified is not a " +
							"receipt for anything")
					}
					jr.Set("verified", true)
					return nil
				},
			},
			{
				// NOT fatal, and after the verify: a failing prune costs disk
				// space, and a prune that ran before the verify could delete
				// the last good archive on the strength of a truncated new
				// one.
				Name: "backup.prune",
				Do: func(ctx context.Context, jr *Run) error {
					if !ref.Verified() {
						return fmt.Errorf("refusing to prune: retention only ever runs after a " +
							"new archive has been written AND verified, and a failing backup " +
							"must never erode history (section 13.2)")
					}
					return b.Prune(ctx, ref)
				},
			},
			{
				// NOT fatal, deliberately and per section 13.2: an off-box
				// push failing must never fail the local backup, which is the
				// actual protection. `on_failure: fail` in the remote block
				// is how an operator opts into the other behaviour, and it is
				// the remote target's business rather than this table's.
				Name: "backup.push",
				When: func(jr *Run) (bool, string) {
					return ref.Verified(), "there is no verified archive to push"
				},
				Do: func(ctx context.Context, jr *Run) error { return b.Push(ctx, ref) },
			},
		}, nil
	},
}
