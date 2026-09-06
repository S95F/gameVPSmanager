// This fixture MUST NOT compile.
//
// It has the right signature and IGNORES the parameter, which is the case a
// signature alone cannot prevent -- Go does not complain about an unused
// argument. What stops it is that the only way to delete anything from inside
// a plan is reconcile.Remove, and Remove takes the Ref. A Do that ignored its
// Ref has nothing to pass, so the call below does not compile.
package main

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

type wipe struct{ targets []instance.Jailed }

func (w wipe) Describe() reconcile.Step {
	return reconcile.Step{Kind: reconcile.KindRemove, Change: reconcile.Delete, Object: "the old world"}
}

func (w wipe) Destructive() bool { return true }

func (w wipe) Do(ctx context.Context, _ *archive.Ref) error {
	// The Ref was ignored, so there is nothing to hand Remove.
	return reconcile.Remove(ctx, w.targets)
}

var _ reconcile.Action = wipe{}

func main() { _ = wipe{} }
