// The CONTROL fixture. It must compile, or the two "must not compile" fixtures
// beside it prove nothing: a typo in an import path fails to build for a
// reason that has nothing to do with archive-before-destroy.
//
// This is what a destructive Action looks like when it is written correctly:
// Do takes the *archive.Ref and hands it to the one primitive that can delete
// anything.
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

func (w wipe) Do(ctx context.Context, ref *archive.Ref) error {
	return reconcile.Remove(ctx, ref, w.targets)
}

var _ reconcile.Action = wipe{}

func main() { _ = wipe{} }
