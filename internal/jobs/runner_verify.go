package jobs

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The `verify` kind: archive integrity, nightly, host-level (section 13.2).
//
// It is the only job whose output is purely EVIDENCE. Nothing it does changes
// the host, which is why its policy is `warn` and why it is safe to run it
// over every archive on the box every night.
//
// It exists because backup.sh had no integrity story at all -- it wrote a
// tarball, chmod'ed it and moved on. Per-member sha256 in the archive manifest
// is what makes a check possible, and a check that only ran at restore time
// would discover a truncated archive at the exact moment it was needed. The
// split between shallow and deep is a cost decision: shallow is seconds and
// covers everything, deep is minutes and covers the newest `preserve` archive
// per instance, which is the one a rebuild or a cutover left behind and
// therefore the one a restore is most likely to reach for.
//
// Scope is `either`: this same table is `gvm backup verify pz-main` and the
// host-level `_verify` timer, over one instance or over all of them. A second
// implementation for the host sweep is how the two would come to disagree
// about what "verified" means.
var verifyRunner = &Runner{
	Kind:  lifecycle.JobVerify,
	Scope: ScopeEither,
	// `warn`, section 13.3's eighth row. A verification that could not run is
	// a loss of EVIDENCE and never of availability -- but a verification that
	// FAILED is evidence of its own, and the step below returns it as a
	// failure so the run is degraded and findable.
	Policy:  PolicyWarn,
	Stops:   false,
	Summary: "shallow-verify everything, deep-verify the newest preserve archive",
	Steps: func(env *Env) ([]Step, error) {
		a := env.Archives
		if a == nil {
			return nil, fmt.Errorf("%w: a verify job needs an archive store (Env.Archives)",
				ErrEnv)
		}
		return []Step{
			{
				// Shallow over everything: seconds per archive, so it is
				// affordable nightly over every instance on the box.
				Name: "verify.shallow",
				Do: func(ctx context.Context, jr *Run) error {
					return overInstances(ctx, a, env, jr, "verified_shallow", a.VerifyShallow)
				},
			},
			{
				// Deep over the newest `preserve` per instance: minutes, and
				// the one archive most likely to be needed, because preserve
				// is what a rebuild, a restore and an adopt cutover leave
				// behind. A truncated one of those is discovered here rather
				// than during the restore it was taken for.
				Name: "verify.deep_newest_preserve",
				Do: func(ctx context.Context, jr *Run) error {
					return overInstances(ctx, a, env, jr, "verified_deep",
						a.VerifyNewestPreserve)
				},
			},
		}, nil
	},
}
