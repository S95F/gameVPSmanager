// This fixture MUST NOT compile.
//
// Its Do drops the *archive.Ref from the signature, which is the shape a
// destructive action would have if archive-before-destroy were a convention
// instead of a type. reconcile.Action requires the parameter, so the
// assignment below is a compile error and this file is the assertion that it
// still is.
package main

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

type wipe struct{}

func (w wipe) Describe() reconcile.Step {
	return reconcile.Step{Kind: reconcile.KindRemove, Change: reconcile.Delete, Object: "the old world"}
}

func (w wipe) Destructive() bool { return true }

// No *archive.Ref. There is nothing to authorise the deletion with, and there
// is no way to get one from here.
func (w wipe) Do(ctx context.Context) error { return nil }

var _ reconcile.Action = wipe{}

func main() { _ = wipe{} }
