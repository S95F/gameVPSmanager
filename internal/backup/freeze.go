package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The freeze, and the end of `sleep 15`.
//
// backup.sh flushes like this:
//
//	as_pz "screen ... stuff \"save\\015\"" 2>/dev/null || true
//	sleep 15
//
// Three things are wrong with it and all three are fixed here rather than
// tuned.
//
// The `|| true` means a save that never reached the game is indistinguishable
// from one that did: the backup proceeds either way and the tarball claims to
// be a flushed world. Here a rung that could not be delivered is recorded, and
// the archive is still taken -- a backup of an unflushed world is worth far
// more than no backup -- but it is marked DEGRADED, so "the world rolled back
// fifteen minutes" has an entry in the job log to find rather than a silence.
//
// The `15` is a number somebody chose once. It is simultaneously too long on a
// small world and too short on a large one, and nothing anywhere says which.
// Here the flush waits for the MARKER the manifest declares -- Project
// Zomboid's "(?i)(save complete|saved)" -- and returns as soon as the game
// says it is done. The wait is a budget, not a duration.
//
// And where a game declares no marker (Valheim's `save`, Palworld's `Save`),
// the blind wait is still the only option -- so it is EXPLICIT and RECORDED:
// the result carries the duration waited and the sentence "this game declares
// no save marker, so the flush was given 10s and assumed". A degraded flush
// that is visible is a different thing from one that is assumed.
//
// The rung order is the manifest's: `before` (save-off), `during` (the flush),
// then the snapshot, then `after` (save-on). The lease is armed BEFORE the
// first rung is sent, which is the whole of D-31 -- see lease.go.

// ConsoleSender delivers a `via: console` rung.
//
// It is a local interface rather than control.Sender for the reason
// runner.Ladder declares its own: this package compiles and tests against a
// fake, and a freeze that could only be exercised with a live console socket
// is a freeze that gets exercised in production.
type ConsoleSender interface {
	// Send writes one line and returns the sequence the echo landed at, so a
	// marker search can start after it rather than matching a line that was
	// already in the ring.
	Send(ctx context.Context, actor, line string) (seq uint64, err error)
}

// ControlChannel delivers a `via: control` rung, structurally satisfied by
// control.Channel.
type ControlChannel interface {
	Ready() bool
	Unready() string
	Exec(ctx context.Context, cmd string) (string, error)
}

// LogWatcher is how a rung waits for its marker.
//
// A separate seam from ConsoleSender because the two are genuinely different
// capabilities and half the catalogue has one without the other: a game can
// accept console input while gvm captures no ring to watch (the standalone
// path), and a control-channel rung's marker still appears in the console log.
// A nil LogWatcher degrades every marker to a plain wait, RECORDED as such.
type LogWatcher interface {
	// Cursor is the sequence the next line will have.
	Cursor() uint64
	// Await blocks until a line matching pattern appears at or after since,
	// or until budget elapses, or until ctx is done. A timeout is reported as
	// ErrNoMarker, which the caller degrades on rather than fails on.
	Await(ctx context.Context, pattern string, since uint64, budget time.Duration) error
}

// ErrNoMarker is a marker wait that timed out. It is a degradation and not a
// failure: the save may well have completed and merely said so in a form this
// manifest's regexp does not match, and refusing to archive on that basis
// would turn a cosmetic mismatch into a gap in history.
var ErrNoMarker = errors.New("backup: the save marker did not appear within the flush budget")

// ErrFreeze is a freeze that could not be performed at all.
var ErrFreeze = errors.New("backup: the freeze could not be performed")

// DefaultFlushWait is how long a rung with neither a marker nor a declared
// wait is given.
//
// It is backup.sh's fifteen seconds, kept deliberately: the number was chosen
// against a real Project Zomboid world and there is no better one available
// without a marker. What has changed is that it is now named, recorded in the
// result, and reported as an assumption rather than as a fact.
const DefaultFlushWait = 15 * time.Second

// Rung is one freeze command, fully rendered.
type Rung struct {
	// Field is the manifest path this rung came from, for error messages:
	// "spec.backup.freeze.during".
	Field      string
	Via        Via
	Send       string
	WaitForLog string
	Wait       time.Duration
	// Tainted says the rendered Send contains a secret, so it must not be
	// logged or recorded. No shipped manifest has one; the flag exists
	// because spec.Renderer reports taint and dropping that report on the
	// floor is how a password reaches a job log.
	Tainted bool
}

// Safe is the text of the rung that is safe to record.
func (r Rung) Safe() string {
	if r.Tainted {
		return "(redacted)"
	}
	return r.Send
}

// FreezePlan is the whole freeze, decided and rendered before anything runs.
//
// Built in the preflight for the same reason the snapshot Plan is: every
// refusal in here -- an unrenderable template, a rung on a channel this game
// does not have -- should happen minutes before a world is frozen rather than
// between save-off and save-on.
type FreezePlan struct {
	// Before are `before` and `during`, in that order. They run between the
	// lease being armed and the snapshot.
	Before []Rung
	// After is the thaw: `after`, or nothing when the manifest says `none`.
	After []Rung
	// Budget is the manifest's lease_budget, before BudgetFor adjusts it for
	// the snapshot method.
	Budget time.Duration
}

// Frozen reports whether this plan actually suspends anything.
//
// A game whose freeze is `{before: none, after: none}` -- Core Keeper, the
// generic Steam manifest -- is not frozen at all, and saying so lets the
// engine skip arming a lease for a freeze that does not exist. Arming one
// anyway would put a watchdog on a world nobody touched, which is harmless but
// would make every `gvm status` show a freeze during every nightly.
func (p FreezePlan) Frozen() bool { return len(p.Before) > 0 || len(p.After) > 0 }

// ThawSteps is the plan's thaw as the lease records it.
func (p FreezePlan) ThawSteps() []ThawStep {
	if len(p.After) == 0 {
		return nil
	}
	out := make([]ThawStep, 0, len(p.After))
	for _, r := range p.After {
		out = append(out, ThawStep{Via: r.Via, Send: r.Send})
	}
	return out
}

// BuildFreeze renders a manifest's freeze for one instance.
func BuildFreeze(m *spec.Manifest, r *spec.Renderer) (FreezePlan, error) {
	if m == nil {
		return FreezePlan{}, errors.New("backup: BuildFreeze needs a manifest")
	}
	f := m.Spec.Backup.Freeze
	var plan FreezePlan
	plan.Budget = f.LeaseBudget.Duration()

	var errs []error
	add := func(dst *[]Rung, field string, st *spec.ShutdownStep) {
		if st == nil {
			return
		}
		rung, err := buildRung(r, field, *st)
		if err != nil {
			errs = append(errs, err)
			return
		}
		*dst = append(*dst, rung)
	}
	add(&plan.Before, "spec.backup.freeze.before", f.Before)
	add(&plan.Before, "spec.backup.freeze.during", f.During)
	add(&plan.After, "spec.backup.freeze.after", f.After)

	if err := errors.Join(errs...); err != nil {
		return FreezePlan{}, err
	}
	// spec's own validation already refuses a `during` with no `after`
	// (validate.go: "every freeze needs its thaw"). This is the same claim
	// one layer down, because by here the manifest may have come from a
	// pinned manifest.lock.yaml written by an older gvm whose validator did
	// not make that check.
	if len(plan.Before) > 1 && len(plan.After) == 0 {
		return FreezePlan{}, fmt.Errorf("%w: this manifest suspends saving with %q and declares "+
			"no thaw; dying between the two leaves a running server that silently stops "+
			"persisting (D-31)", ErrFreeze, plan.Before[0].Safe())
	}
	return plan, nil
}

// buildRung renders one freeze step.
func buildRung(r *spec.Renderer, field string, st spec.ShutdownStep) (Rung, error) {
	via := Via(strings.TrimSpace(st.Via))
	if !via.Valid() {
		return Rung{}, fmt.Errorf("%w: %s.via is %q; a freeze rung is %s or %s -- it cannot be a "+
			"signal, because a signal does not flush a world", ErrFreeze, field, st.Via,
			ViaConsole, ViaControl)
	}
	send := strings.TrimSpace(st.Send)
	if send == "" {
		return Rung{}, fmt.Errorf("%w: %s sends nothing", ErrFreeze, field)
	}
	rendered, taint, err := renderRung(r, field+".send", send)
	if err != nil {
		return Rung{}, err
	}
	if strings.TrimSpace(rendered) == "" {
		// A template that rendered away to nothing. Refused rather than sent:
		// an empty line to a console is a newline, and a newline is not a
		// save.
		return Rung{}, fmt.Errorf("%w: %s rendered %q to nothing", ErrFreeze, field, send)
	}
	return Rung{
		Field:      field,
		Via:        via,
		Send:       rendered,
		WaitForLog: strings.TrimSpace(st.WaitForLog),
		Wait:       st.Wait.Duration(),
		Tainted:    taint != spec.TaintNone,
	}, nil
}

// renderRung expands one template. A nil renderer is accepted only for a
// template with nothing in it to expand, matching the Commander's and the
// ladder's rule: rendering against a zero RenderData would turn a command into
// a shorter, wrong command and report success.
func renderRung(r *spec.Renderer, field, tmpl string) (string, spec.Taint, error) {
	if r == nil {
		if strings.Contains(tmpl, "{{") {
			return "", spec.TaintNone, fmt.Errorf("%w: %s needs a renderer: %q", ErrFreeze, field, tmpl)
		}
		return tmpl, spec.TaintNone, nil
	}
	return r.Render(field, tmpl)
}

// RungResult is what one rung did.
type RungResult struct {
	Field string `json:"field"`
	Via   string `json:"via"`
	Send  string `json:"send,omitempty"`
	// Delivered says the command reached the game.
	Delivered bool `json:"delivered"`
	// MarkerMatched says the manifest's wait_for_log appeared. False with a
	// non-empty Marker means the budget ran out, which is a degradation.
	Marker        string        `json:"marker,omitempty"`
	MarkerMatched bool          `json:"marker_matched,omitempty"`
	Waited        time.Duration `json:"waited"`
	// Assumed says the flush was given a timeout and believed rather than
	// confirmed -- backup.sh's `sleep 15`, now with its name on it.
	Assumed bool `json:"assumed,omitempty"`
	// Note explains any of the above that needs explaining, and goes in the
	// job log verbatim.
	Note string `json:"note,omitempty"`
	Err  string `json:"err,omitempty"`
}

// FlushResult is the whole of what the pre-backup flush achieved.
type FlushResult struct {
	Rungs []RungResult `json:"rungs,omitempty"`
	// Degraded says at least one rung was not confirmed: undelivered, or
	// waited out rather than marked. The archive is still taken; the flag is
	// what makes that a visible decision rather than an assumption.
	Degraded bool `json:"flush_degraded,omitempty"`
	// Why is the operator-facing sentence for Degraded.
	Why string `json:"flush_degraded_why,omitempty"`
	// Elapsed is how long the flush took, which is the leading part of the
	// stall under a direct snapshot.
	Elapsed time.Duration `json:"flush_elapsed"`
}

// Channels is the set of delivery seams a freeze has available.
//
// A nil member is a channel this game does not have, or one gvm is not holding
// right now, and both are ordinary: Project Zomboid's RCON is unavailable for
// the whole first boot, and the standalone shutdown path has a console with no
// ring behind it. Every absence is recorded rather than treated as a failure.
type Channels struct {
	Console ConsoleSender
	Control ControlChannel
	Log     LogWatcher
	// Actor is the name the console echo is attributed to, so an operator
	// reading the console sees who sent `save-off`.
	Actor string
	// Clock is how a plain wait is slept. Injectable so a 120-second
	// Minecraft flush is a test that finishes instantly.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock the elapsed times are measured against.
	Now func() time.Time
}

func (c Channels) actor() string {
	if strings.TrimSpace(c.Actor) == "" {
		return "gvm-backup"
	}
	return c.Actor
}

func (c Channels) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Channels) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunRungs executes a sequence of freeze rungs and reports what happened.
//
// It never returns an error for a rung that failed. That is the central
// decision of this function and it goes the opposite way from the thaw: a
// flush that did not land costs at most the minutes since the last autosave,
// while refusing to archive costs the whole night's history. So every failure
// is recorded, Degraded is set, and the caller carries on.
//
// The thaw uses the same machinery and the caller treats its result
// differently -- see Thaw.
func RunRungs(ctx context.Context, ch Channels, rungs []Rung) FlushResult {
	start := ch.now()
	var out FlushResult
	for _, r := range rungs {
		res := runRung(ctx, ch, r)
		out.Rungs = append(out.Rungs, res)
		if !res.Delivered || res.Assumed || (res.Marker != "" && !res.MarkerMatched) {
			out.Degraded = true
			out.Why = appendNote(out.Why, degradedWhy(r, res))
		}
	}
	out.Elapsed = ch.now().Sub(start)
	return out
}

// degradedWhy is the sentence an operator reads next to a degraded flush.
func degradedWhy(r Rung, res RungResult) string {
	switch {
	case !res.Delivered:
		return fmt.Sprintf("%s (%s %q) was not delivered: %s", r.Field, r.Via, r.Safe(), res.Err)
	case res.Marker != "" && !res.MarkerMatched:
		return fmt.Sprintf("%s waited %s for %q and the marker never appeared, so the flush is "+
			"unconfirmed", r.Field, res.Waited.Round(time.Millisecond), res.Marker)
	default:
		return fmt.Sprintf("%s declares no save marker, so the flush was given %s and assumed",
			r.Field, res.Waited.Round(time.Millisecond))
	}
}

// runRung delivers one rung and waits for whatever it declared.
func runRung(ctx context.Context, ch Channels, r Rung) RungResult {
	res := RungResult{Field: r.Field, Via: string(r.Via), Send: r.Safe(), Marker: r.WaitForLog}

	// The marker cursor is taken BEFORE the send, so a match cannot be a line
	// that was already in the ring. A "save complete" from the previous
	// autosave would otherwise satisfy the wait instantly and the archive
	// would be taken mid-write.
	var since uint64
	if r.WaitForLog != "" && ch.Log != nil {
		since = ch.Log.Cursor()
	}

	switch r.Via {
	case ViaConsole:
		if ch.Console == nil {
			res.Note = "this game captures no console, so the rung could not be attempted"
			res.Err = "no console channel"
			return res
		}
		seq, err := ch.Console.Send(ctx, ch.actor(), r.Send)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		if r.WaitForLog != "" && ch.Log != nil && seq >= since {
			since = seq + 1
		}
	case ViaControl:
		switch {
		case ch.Control == nil:
			res.Note = "this game has no control channel, so the rung could not be attempted"
			res.Err = "no control channel"
			return res
		case !ch.Control.Ready():
			// Not a failure of this package: Project Zomboid's RCON is
			// unavailable for the whole first boot and 7DTD's telnet is not
			// implemented in this build. It is still a flush that did not
			// happen, so it is recorded as degraded.
			res.Note = firstNonEmpty(ch.Control.Unready(), "the control channel is not ready")
			res.Err = res.Note
			return res
		}
		if _, err := ch.Control.Exec(ctx, r.Send); err != nil {
			res.Err = err.Error()
			return res
		}
	default:
		res.Err = fmt.Sprintf("%q is not a freeze channel", r.Via)
		return res
	}
	res.Delivered = true

	budget := r.Wait
	switch {
	case r.WaitForLog != "" && ch.Log != nil:
		if budget <= 0 {
			// A marker with no wait. The marker is the better signal, so it
			// is given the default budget rather than skipped: a rung that
			// declared what to wait for and then did not wait is the one
			// shape of this that nobody meant.
			budget = DefaultFlushWait
			res.Note = appendNote(res.Note, fmt.Sprintf("this rung declares a marker and no "+
				"wait, so the marker was given %s", budget))
		}
		before := ch.now()
		err := ch.Log.Await(ctx, r.WaitForLog, since, budget)
		res.Waited = ch.now().Sub(before)
		switch {
		case err == nil:
			res.MarkerMatched = true
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			res.Err = err.Error()
			res.Note = appendNote(res.Note, "the job's context ended while waiting for the save marker")
		default:
			res.Err = err.Error()
		}
	case r.WaitForLog != "":
		// A marker was declared and there is nothing to watch it in. The
		// standalone path, and `gvm backup` run against an instance whose
		// console ring gvm is not holding.
		if budget <= 0 {
			budget = DefaultFlushWait
		}
		res.Assumed = true
		res.Note = appendNote(res.Note, fmt.Sprintf("there is no console ring on this path, so "+
			"the %q marker could not be watched and the flush was given %s", r.WaitForLog, budget))
		res.Waited = sleepFor(ctx, ch, budget)
	default:
		// backup.sh's case, named. The game declares no marker, so there is
		// nothing better available than a timeout -- and the timeout is in
		// the record, which is the whole difference.
		if budget <= 0 {
			budget = DefaultFlushWait
		}
		res.Assumed = true
		res.Waited = sleepFor(ctx, ch, budget)
	}
	return res
}

// sleepFor waits and reports how long it actually waited, so a cancelled job's
// record says four seconds rather than the fifteen it asked for.
func sleepFor(ctx context.Context, ch Channels, d time.Duration) time.Duration {
	before := ch.now()
	_ = ch.sleep(ctx, d)
	return ch.now().Sub(before)
}

// ThawResult is what lifting the freeze achieved.
type ThawResult struct {
	Rungs []RungResult `json:"rungs,omitempty"`
	// Thawed is true only when every rung was delivered. It is what decides
	// whether the lease may be cleared, so it is strict where FlushResult is
	// forgiving: an undelivered `save-on` is the failure D-31 exists for.
	Thawed bool `json:"thawed"`
	// Why explains a false Thawed.
	Why string `json:"thaw_why,omitempty"`
	// LeaseCleared says the lease file was removed, which happens only on a
	// complete thaw. A lease left behind is the only remaining evidence that
	// a world may not be saving, and lifecycle's freeze_armed finding is what
	// surfaces it.
	LeaseCleared bool `json:"lease_cleared"`
}

// Thaw lifts the freeze.
//
// It is idempotent, which matters more here than anywhere else in the package:
// the runner's lease timer may have got there first, the engine's step Undo
// runs it on every exit path including a panic, and `save-on` sent twice to a
// server that is already saving is a no-op. The one thing it must never do is
// report success for a command it could not deliver.
//
// Unlike the flush, delivery failure here is NOT absorbed. The caller fails
// the backup on it, because a server left with saving disabled is worse than
// a missing tarball -- and because the lease, which stays armed, is what will
// eventually fix it.
func Thaw(ctx context.Context, ch Channels, rungs []Rung) ThawResult {
	out := ThawResult{Thawed: true}
	for _, r := range rungs {
		res := runRung(ctx, ch, r)
		out.Rungs = append(out.Rungs, res)
		if !res.Delivered {
			out.Thawed = false
			out.Why = appendNote(out.Why, fmt.Sprintf("%s (%s %q) was not delivered: %s; the "+
				"freeze lease stays armed and the runner will re-send it at the deadline (D-31)",
				r.Field, r.Via, r.Safe(), res.Err))
		}
	}
	return out
}

// firstNonEmpty is the first value with something in it.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
