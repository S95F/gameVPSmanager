// This fixture MUST NOT COMPILE.
//
// It is the whole of "the reconciler never actuates", stated as a type. A
// sweep that healed by itself would fight the operator who stopped an instance
// to repair a world by hand, would restart a server mid-rebuild because it
// briefly looked down, and would do both at 3am with nobody watching.
//
// TestReconcilerNeverActuates proves no start HAPPENED during one sweep over a
// drifted host. That is a statement about one run, and a future edit could
// falsify it. This is the statement that cannot be falsified by an edit to
// this package's logic: the reconciler's view of systemd is a
// lifecycle.UnitReader, which has State and ListUnits and nothing else, so
// there is no Start to call from anywhere.
//
// If this ever compiles, somebody has widened that seam and the sweep can
// start a server.
package main

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

type reader struct{}

func (reader) State(context.Context, string) (platform.UnitState, error) {
	return platform.UnitState{}, nil
}

func (reader) ListUnits(context.Context, string) ([]platform.UnitState, error) {
	return nil, nil
}

func main() {
	r := &lifecycle.Reconciler{Obs: &lifecycle.Observer{Units: reader{}}}

	// "It looked down, so I started it." This is the line the whole design
	// exists to make impossible to write.
	if err := r.Obs.Units.Start(context.Background(), "gvm@pz-main.service"); err != nil {
		panic(err)
	}
}
