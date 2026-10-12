package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `prune` kind: retention plus an index rewrite (section 13.3).
//
// It never stops the server and its policy is `warn`, because the cost of a
// prune that did not happen is disk space. The cost of one that happened WRONG
// is a world nobody can restore, which is why every rule about what may be
// deleted lives in internal/archive and not here: Prune() refuses a `preserve`
// archive even with --force (section 13.2), the only way one goes is `rm` by a
// human, and `doctor` reports total preserve bytes so the operator notices
// before the disk fills.
//
// The ordering is retention first, index second. Rewriting the index before
// the deletions would leave it describing files that are about to go; doing it
// after means a crash between the two leaves a stale index, which Reindex
// repairs on the next run. One of those is recoverable and the other is a
// restore list with holes in it.

// Archives is the archive directory's housekeeping (internal/archive).
//
// instanceName is a parameter rather than configuration because both of these
// run host-wide from `_verify` and per-instance from the CLI: an empty name
// means every instance, which is the ScopeEither this file declares.
type Archives interface {
	// Instances lists the instances with an archive directory. It is how the
	// host-level sweep finds its work without a database.
	Instances(ctx context.Context) ([]string, error)
	// Prune applies the retention policy: keep N newest, then keep_days.
	// `preserve` archives are never candidates, by any code path.
	Prune(ctx context.Context, instanceName string) (removed int, err error)
	// Reindex rebuilds the directory's index from the archives in it.
	Reindex(ctx context.Context, instanceName string) error
	// VerifyShallow is gzip CRC, manifest parse, member count and whole-file
	// sha256 -- seconds per archive.
	VerifyShallow(ctx context.Context, instanceName string) (checked int, err error)
	// VerifyNewestPreserve re-hashes every member of the newest `preserve`
	// archive -- minutes, and the one archive a restore is most likely to
	// need.
	VerifyNewestPreserve(ctx context.Context, instanceName string) (checked int, err error)
}

var pruneRunner = &Runner{
	Kind:  lifecycle.JobPrune,
	Scope: ScopeEither,
	// `warn`, section 13.3's seventh row.
	Policy:  PolicyWarn,
	Stops:   false,
	Summary: "apply retention, then rewrite the index",
	Steps: func(env *Env) ([]Step, error) {
		a := env.Archives
		if a == nil {
			return nil, fmt.Errorf("%w: a prune job needs an archive store (Env.Archives)",
				ErrEnv)
		}
		return []Step{
			{
				Name: "prune.retention",
				Do: func(ctx context.Context, jr *Run) error {
					return overInstances(ctx, a, env, jr, "pruned",
						func(ctx context.Context, name string) (int, error) {
							return a.Prune(ctx, name)
						})
				},
			},
			{
				Name: "prune.reindex",
				Do: func(ctx context.Context, jr *Run) error {
					return overInstances(ctx, a, env, jr, "reindexed",
						func(ctx context.Context, name string) (int, error) {
							return 0, a.Reindex(ctx, name)
						})
				},
			},
		}, nil
	},
}

// overInstances applies one operation to this job's scope.
//
// A named instance means that instance; no instance means every one the
// archive store knows about, which is how the host-level `_verify` timer
// covers a box without a database. A failure on one instance is recorded by
// name and does not stop the others: a corrupt archive directory for a
// decommissioned server must not prevent the live one from being verified.
func overInstances(ctx context.Context, a Archives, env *Env, jr *Run, countKey string,
	fn func(ctx context.Context, instanceName string) (int, error)) error {

	names := []string{env.Instance}
	if env.Instance == "" {
		all, err := a.Instances(ctx)
		if err != nil {
			return fmt.Errorf("could not list the instances with archives: %w", err)
		}
		names = all
	}
	var total int
	var failed []string
	var errs []error
	for _, name := range names {
		n, err := fn(ctx, name)
		total += n
		if err != nil {
			failed = append(failed, name)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	jr.Set(countKey, total)
	if len(failed) > 0 {
		jr.Set(countKey+"_failed", failed)
		return fmt.Errorf("%s failed for %s: %w", countKey, strings.Join(failed, " "),
			errors.Join(errs...))
	}
	return nil
}
