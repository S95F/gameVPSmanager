// Package platform is the seam between gvm's portable logic and the Linux
// host it manages.
//
// Everything that touches systemd, ufw, unix users, PTYs or process groups
// goes through this interface. Two things fall out of that:
//
//   - The portable half of gvm builds and tests on Windows, which is where
//     development happens. A Linux-only implementation that is not behind
//     this seam breaks `make check` on the developer's own machine.
//   - Host effects are mockable, so lifecycle and reconciliation logic can be
//     tested without root, without systemd, and without a VPS.
//
// The interface is deliberately narrow and verb-shaped. It is not a general
// "run a command" escape hatch: a caller that could ask the platform to run
// arbitrary shell would make the whole seam decorative.
package platform

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotSupported is returned by every stub on a platform that cannot perform
// host operations. Callers check for it with errors.Is and degrade rather
// than crashing, so `gvm game validate` works fine on Windows.
var ErrNotSupported = errors.New("platform: not supported on this operating system")

// UnsupportedError names the operation that is unavailable, wrapping
// ErrNotSupported so both the specific and the general check work.
type UnsupportedError struct {
	Op string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("platform: %s is not supported on this operating system", e.Op)
}

func (e *UnsupportedError) Unwrap() error { return ErrNotSupported }

// Unsupported builds an UnsupportedError for op.
func Unsupported(op string) error { return &UnsupportedError{Op: op} }

// UnitState is a systemd unit's current condition, as reported by the
// manager rather than inferred from a pid file.
type UnitState struct {
	Name        string
	LoadState   string // loaded, not-found, masked
	ActiveState string // active, activating, deactivating, inactive, failed
	SubState    string // running, exited, start-pre, ...
	MainPID     int
	// Result is why the unit last stopped: "success", "exit-code",
	// "timeout", "oom-kill". This is what distinguishes a clean world save
	// from a SIGKILL at TimeoutStopSec.
	Result string
	// ExecMainStatus is the exit code of the main process, which gvm's
	// runner uses as a typed failure channel rather than a boolean.
	ExecMainStatus int
	// NRestarts is systemd's cumulative restart counter for the unit. It is
	// cleared only by an explicit start/stop or `reset-failed`, so a positive
	// value on a unit that is now ready is history, not a live loop — which is
	// why crash-looping is judged from the unit's current substate, not from
	// this number alone.
	NRestarts int
	// StatusText is the unit's last sd_notify STATUS= line. The runner writes
	// the reason a boot died there, so it carries the one sentence an operator
	// needs when ExecMainStatus only gives a number.
	StatusText string
}

// Failed reports whether the unit is in a failed state.
func (u UnitState) Failed() bool { return u.ActiveState == "failed" }

// Running reports whether the unit is fully up.
func (u UnitState) Running() bool { return u.ActiveState == "active" }

// Systemd is the subset of the service manager gvm drives.
//
// Restart exists separately from Stop+Start because a changed OnCalendar on a
// live timer only recomputes its next elapse on restart -- `enable --now` is
// not enough, a lesson the bash nightly.sh documents in a comment.
type Systemd interface {
	State(ctx context.Context, unit string) (UnitState, error)
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	DaemonReload(ctx context.Context) error
	// ListUnits returns units matching a glob, for reconciliation and for
	// discovering what a previous gvm left behind.
	ListUnits(ctx context.Context, pattern string) ([]UnitState, error)
}

// User is a unix account gvm manages or has adopted.
type User struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

// Users creates and inspects the service accounts instances run as.
type Users interface {
	Lookup(ctx context.Context, name string) (User, error)
	// EnsureSystemUser creates a locked system account if it is absent and
	// returns it either way. It never modifies an existing account, because
	// an adopted instance's user predates gvm and may be shared.
	EnsureSystemUser(ctx context.Context, name, home string) (User, error)
	EnsureGroup(ctx context.Context, name string) error
	AddToGroup(ctx context.Context, user, group string) error
}

// Host is the full platform surface.
type Host interface {
	Systemd() Systemd
	Users() Users
	// Hostname is used in the dashboard and in archive names.
	Hostname() (string, error)
}

// New returns the Host implementation for the running operating system, or an
// error wrapping ErrNotSupported.
func New() (Host, error) { return newHost() }
