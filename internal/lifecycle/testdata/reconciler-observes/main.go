// The CONTROL fixture. It must compile, or the "must not compile" fixture
// beside it proves nothing: a typo in an import path fails to build for a
// reason that has nothing to do with the reconciler's surface.
//
// This is what reaching into a Reconciler's view of systemd looks like when it
// is written correctly. Everything a sweep is allowed to do to a unit is here:
// read its state, and list the units the manager knows about.
package main

import (
	"context"
	"fmt"

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

var _ lifecycle.UnitReader = reader{}

func main() {
	r := &lifecycle.Reconciler{Obs: &lifecycle.Observer{Units: reader{}}}

	st, err := r.Obs.Units.State(context.Background(), "gvm@pz-main.service")
	if err != nil {
		panic(err)
	}
	units, err := r.Obs.Units.ListUnits(context.Background(), "gvm@*.service")
	if err != nil {
		panic(err)
	}
	fmt.Println(st.ActiveState, len(units))
}
