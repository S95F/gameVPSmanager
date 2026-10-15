// Package avail is gvm's one record of what works in this build.
//
// A command, a subcommand or a job kind is Works, Partial or NotInBuild, and
// ErrUnavailable is the single sentinel every "not in this build" path returns.
// Both live here, in a leaf package that imports nothing of gvm's own, so
// internal/cli, internal/jobs and internal/backup can all speak the same
// vocabulary and wrap the same sentinel without importing one another.
//
// The point of the type is usability rule U1: no list of what works is written
// by hand in more than one place. The registry on cli.Command and on every
// subcommand entry is read by help, by `gvm job list`, by every `next:` line
// and by the README "What works" table, and a single test
// (TestAvailabilityMatchesBehaviour) fails both when a Works row refuses and
// when a NotInBuild row starts working — so a phase that finishes a journey has
// to flip its rows in the same commit the behaviour changes.
package avail

import "errors"

// State is how much of an entry works in this build.
type State uint8

const (
	// Works: the entry does what its summary says.
	Works State = iota
	// Partial: part of the entry works and part refuses. Why names the split
	// ("--list works; restoring does not").
	Partial
	// NotInBuild: the entry is routed, and its engine is not in this release.
	NotInBuild
)

// Availability is one entry's record: whether it works, and if not, why.
//
// The zero value is Works with no reason, which is the common case and the one
// an entry gets by saying nothing — a row only has to be written down when it
// is Partial or NotInBuild.
type Availability struct {
	State State
	// Why is one operator-facing sentence, in plain words. It is required for
	// Partial and NotInBuild and empty for Works. It never names a design
	// decision number or a Go identifier (U6).
	Why string
}

// Available reports whether the entry runs at all — Works or Partial. A
// Partial entry is available because the half that works is reachable; the
// caller decides, per invocation, whether the specific thing it is doing is the
// half that refuses.
func (a Availability) Available() bool { return a.State != NotInBuild }

// Built is the Works record, for a row that works in full.
func Built() Availability { return Availability{State: Works} }

// Partialf is the Partial record with its reason.
func Partialf(why string) Availability { return Availability{State: Partial, Why: why} }

// Unbuilt is the NotInBuild record with its reason.
func Unbuilt(why string) Availability { return Availability{State: NotInBuild, Why: why} }

// ErrUnavailable is the one sentinel every unbuilt path returns.
//
// It is matched with errors.Is, never printed directly: internal/cli's Main
// prints the standard line ("gvm <verb>: not available in this build; nothing
// was changed.") itself and then the specific reason, so the sentinel's own
// text appears nowhere an operator reads. Every "not built" sentinel in gvm —
// ErrNotImplemented, ErrProviderUnbuilt, ErrContentApplyUnbuilt,
// ErrRemoteUnbuilt and jobs.ErrNoRunner — is built on New below, so all of them
// match this one and all of them exit 3.
var ErrUnavailable = errors.New("not available in this build")

// unavailable carries the operator-facing reason while matching ErrUnavailable
// through its Unwrap. Error() returns the reason alone — no boilerplate — so a
// caller may wrap it with context ("%w: this instance declares …") and Main can
// print the result as the reason with no "not available in this build" showing
// up in the middle of the sentence.
type unavailable struct{ reason string }

func (u *unavailable) Error() string { return u.reason }
func (u *unavailable) Unwrap() error { return ErrUnavailable }

// New returns an error whose errors.Is matches ErrUnavailable and whose message
// is exactly reason. Callers that need formatting pass fmt.Sprintf; the base
// sentinels are static strings.
func New(reason string) error { return &unavailable{reason: reason} }
