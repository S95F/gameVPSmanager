package probe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// KindLogMarker is the manifest's `readiness.probes[].kind` for this probe.
const KindLogMarker = "log_marker"

// LogMarker is the universal readiness probe: wait for a line.
//
// It is the only probe that works during a boot, on a game with no ports open
// yet, with no control channel, on the half of the catalogue that has no RCON
// at all. It is also the only one that can distinguish "loading" from "loaded",
// because the game says so in words.
//
// The pattern is compiled by the constructor and never at match time. Section
// 8.6 check 9 requires it to compile when the MANIFEST is read, so a typo in a
// regex is a validation error at `gvm apply` rather than a runner that starts,
// waits for a marker it can never match, and fails at the hard deadline an hour
// later.
type LogMarker struct {
	ring *console.Ring
	re   *regexp.Regexp

	// since is the first Seq that may satisfy this probe. See After.
	since uint64

	// Timeout bounds the wait. Zero defers to the caller's context.
	Timeout time.Duration
}

var _ Probe = (*LogMarker)(nil)

// After returns the first Seq a line sent right now could occupy.
//
// This one-liner is section 11.6 rule 1, and it is the difference between a
// shutdown that saves and one that does not. `Ring.WaitFor` treats `since` as
// INCLUSIVE, so passing Head() accepts the line already in the ring -- and if
// that line is the `save` an operator typed an hour ago, the wait guarding
// `quit` is satisfied instantly and `quit` lands in the middle of a world
// write. Capture this BEFORE the send, never after.
func After(r *console.Ring) uint64 { return r.Head() + 1 }

// NewLogMarker compiles pattern and returns a probe over r.
//
// since is where matching starts: readiness passes 0 (the whole boot log, since
// the marker may already have scrolled past while the probe list was being
// built), and a shutdown rung passes After(ring) captured before its send.
func NewLogMarker(r *console.Ring, pattern string, since uint64) (*LogMarker, error) {
	if r == nil {
		return nil, errors.New("probe: log_marker needs a ring")
	}
	if pattern == "" {
		return nil, errors.New("probe: log_marker needs a pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("probe: log_marker pattern %q: %w", pattern, err)
	}
	return &LogMarker{ring: r, re: re, since: since}, nil
}

// Kind implements Probe.
func (p *LogMarker) Kind() string { return KindLogMarker }

// Describe implements Probe. The pattern is shown between slashes because it is
// a regex and an operator who sees `SERVER STARTED` in a status line will
// reasonably go looking for that exact string.
func (p *LogMarker) Describe() string { return "a console line matching /" + p.re.String() + "/" }

// Pattern returns the compiled expression, for a caller that needs to match the
// same rule against a line it already has (the fail-marker scanner).
func (p *LogMarker) Pattern() *regexp.Regexp { return p.re }

// Since returns the cursor matching starts from.
func (p *LogMarker) Since() uint64 { return p.since }

// Match reports whether one line satisfies the probe, cursor and source
// included. Run and Wait route through it, so the live path and this one
// cannot drift apart.
//
// Only the GAME's own output counts as evidence. A gvm annotation quoting a
// marker, or an operator broadcasting "world saved, server restarting" into
// the console while the save is still running, would otherwise release `quit`
// onto a world mid-write -- the same failure After() exists to prevent,
// arriving from a different direction. readiness.go applies the identical rule
// to fail markers, and for the same reason.
func (p *LogMarker) Match(l console.Line) bool {
	return l.Seq >= p.since && l.Source == console.SourceGame && p.re.MatchString(l.Text)
}

// Run implements Probe: block until the marker appears.
//
// Unlike the socket probes there is no polling here. Ring.WaitFor subscribes,
// so a marker is seen the moment it is appended rather than up to `every` later
// -- which matters on a boot where the operator is watching a spinner, and
// matters more on a shutdown rung where every second of waiting is a second of
// the stop budget.
func (p *LogMarker) Run(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	if _, err := p.ring.WaitForFunc(ctx, p.since, p.Match); err != nil {
		return fail(KindLogMarker, "/"+p.re.String()+"/", "not seen", err)
	}
	return nil
}

// Wait is Run with the matched line, for a caller that wants the text -- a
// shutdown step recording what the game actually said, or a readiness probe
// whose marker carries a version string.
func (p *LogMarker) Wait(ctx context.Context) (console.Line, error) {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	l, err := p.ring.WaitForFunc(ctx, p.since, p.Match)
	if err != nil {
		return console.Line{}, fail(KindLogMarker, "/"+p.re.String()+"/", "not seen", err)
	}
	return l, nil
}
