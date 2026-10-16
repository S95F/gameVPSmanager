//go:build linux

package hostops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// mode is systemd's job mode. "replace" everywhere: it starts the unit and its
// dependencies, replacing already-queued jobs that conflict.
//
// Not "fail", which would make `gvm apply` lose a race against a timer that
// queued a restart a second earlier and report an error the operator cannot
// act on. Never "isolate", which stops every unit that is not a dependency of
// this one -- on a game host that is the whole box.
const mode = "replace"

// jobResults are systemd's JobRemoved results. Only "done" is success;
// "skipped" is systemd saying the job did not apply to the unit's current
// state, which for a stop of an already-stopped unit is exactly what gvm
// wants and for a start is not.
const (
	jobDone    = "done"
	jobSkipped = "skipped"
)

// dbusSystemd drives systemd over D-Bus.
//
// It connects to systemd's PRIVATE socket (/run/systemd/private) rather than
// the system bus. Section 1 promises that killing the control plane leaves
// every game running and every root CLI verb working, and the system bus is
// part of the control plane in that sense: dbus-daemon can be restarting, or
// wedged, exactly when an operator needs `sudo gvm stop`. The private socket
// is root-only and is served by PID 1 itself, so it is available whenever
// systemd is.
//
// Shelling out to systemctl was the alternative and is worse in the way that
// matters here: systemctl's exit codes conflate "the unit failed" with "the
// unit does not exist", and its output is a moving target across distributions.
type dbusSystemd struct{ conn *sd.Conn }

// newSystemd opens the connection and returns it with its closer.
func newSystemd(ctx context.Context) (platform.Systemd, func() error, error) {
	conn, err := sd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("hostops: connect to systemd: %w; this needs root and a running systemd", err)
	}
	s := &dbusSystemd{conn: conn}
	return s, func() error { conn.Close(); return nil }, nil
}

// job runs a unit job and WAITS for systemd to finish it.
//
// Waiting is the whole point. The D-Bus calls return as soon as the job is
// enqueued, so a caller that did not wait would reload systemd while a restart
// was still in flight, or report "started" for a unit that failed its
// ExecStartPre a moment later. Every ordering bug in a reconciler starts here.
//
// The channel is buffered because go-systemd's own documentation is explicit
// that an unread result blocks the connection's dispatch loop for every other
// job -- an unbuffered channel plus a cancelled context would wedge the whole
// manager connection, not just this call.
func (s *dbusSystemd) job(ctx context.Context, unit string, allowSkipped bool,
	call func(context.Context, string, string, chan<- string) (int, error)) error {

	ch := make(chan string, 1)
	if _, err := call(ctx, unit, mode, ch); err != nil {
		return err
	}
	select {
	case res := <-ch:
		switch {
		case res == jobDone:
			return nil
		case res == jobSkipped && allowSkipped:
			return nil
		}
		return fmt.Errorf("systemd job for %s ended %q", unit, res)
	case <-ctx.Done():
		// The job keeps running; systemd owns it now. Saying so is the honest
		// report, because the caller's next observation may well find the unit
		// up and would otherwise look like a contradiction.
		return fmt.Errorf("%w while waiting for the systemd job on %s, which is still running", ctx.Err(), unit)
	}
}

func (s *dbusSystemd) Start(ctx context.Context, unit string) error {
	return s.job(ctx, unit, false, s.conn.StartUnitContext)
}

// Stop tolerates "skipped": stopping a unit that is already stopped is the
// normal case in a reconciler and is not a failure.
func (s *dbusSystemd) Stop(ctx context.Context, unit string) error {
	return s.job(ctx, unit, true, s.conn.StopUnitContext)
}

func (s *dbusSystemd) Restart(ctx context.Context, unit string) error {
	return s.job(ctx, unit, false, s.conn.RestartUnitContext)
}

// Enable installs the unit's [Install] symlinks.
//
// runtime=false writes under /etc so the enablement survives a reboot;
// force=true replaces a symlink that already points somewhere else, which is
// what an adoption leaves behind.
//
// Enabling does NOT reload the manager. That is VerbDaemonReload's job, and
// keeping them apart is what lets one apply write three files and reload once.
func (s *dbusSystemd) Enable(ctx context.Context, unit string) error {
	_, _, err := s.conn.EnableUnitFilesContext(ctx, []string{unit}, false, true)
	return err
}

func (s *dbusSystemd) Disable(ctx context.Context, unit string) error {
	_, err := s.conn.DisableUnitFilesContext(ctx, []string{unit}, false)
	return err
}

func (s *dbusSystemd) DaemonReload(ctx context.Context) error {
	return s.conn.ReloadContext(ctx)
}

// State reads one unit's condition from the manager.
//
// It asks for the unit's properties rather than filtering ListUnits, because
// ListUnits only returns units the manager has LOADED: a `gvm@pz-main.service`
// that has never been started since boot is simply absent from it, and
// "absent" would then be indistinguishable from "not installed". The property
// read loads the unit and answers either way.
func (s *dbusSystemd) State(ctx context.Context, unit string) (platform.UnitState, error) {
	props, err := s.conn.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return platform.UnitState{}, err
	}
	st := platform.UnitState{
		Name:        unit,
		LoadState:   propString(props, "LoadState"),
		ActiveState: propString(props, "ActiveState"),
		SubState:    propString(props, "SubState"),
	}
	// Result, MainPID and ExecMainStatus live on the Service interface, not on
	// Unit, and a timer has none of them. A missing property is not an error:
	// asking a .timer for its exit code is a legitimate question with no
	// answer.
	if svc, err := s.conn.GetUnitTypePropertiesContext(ctx, unit, "Service"); err == nil {
		st.Result = propString(svc, "Result")
		st.MainPID = int(propUint(svc, "MainPID"))
		st.ExecMainStatus = int(propInt(svc, "ExecMainStatus"))
		st.NRestarts = int(propUint(svc, "NRestarts"))
		st.StatusText = propString(svc, "StatusText")
	} else if !isNoSuchInterface(err) {
		return st, err
	}
	return st, nil
}

// ListUnits returns the loaded units matching a glob.
//
// The pattern comes from hostops' own scope table, never from a caller.
func (s *dbusSystemd) ListUnits(ctx context.Context, pattern string) ([]platform.UnitState, error) {
	us, err := s.conn.ListUnitsByPatternsContext(ctx, nil, []string{pattern})
	if err != nil {
		return nil, err
	}
	out := make([]platform.UnitState, 0, len(us))
	for _, u := range us {
		out = append(out, platform.UnitState{
			Name:        u.Name,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
		})
	}
	return out, nil
}

// isNoSuchInterface recognises "this unit type has no Service interface".
//
// D-Bus reports it as a named error rather than a typed one, so matching the
// name is the only option. Getting it wrong in the tolerant direction would
// hide a real failure, so the check is on the exact interface name systemd
// returns and nothing looser.
func isNoSuchInterface(err error) bool {
	var derr interface{ Error() string }
	if !errors.As(err, &derr) {
		return false
	}
	msg := derr.Error()
	return strings.Contains(msg, "org.freedesktop.DBus.Error.UnknownInterface") ||
		strings.Contains(msg, "Unknown interface")
}

// propString, propUint and propInt read one property out of the map D-Bus
// hands back. A property that is missing or of an unexpected type yields the
// zero value: the alternative is failing a whole status read because one
// optional field was not what this build expected, which turns a cosmetic
// systemd change into an outage of `gvm status`.
func propString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func propUint(m map[string]any, key string) uint64 {
	switch v := m[key].(type) {
	case uint32:
		return uint64(v)
	case uint64:
		return v
	}
	return 0
}

func propInt(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int32:
		return int64(v)
	case int64:
		return v
	case uint32:
		return int64(v)
	}
	return 0
}
