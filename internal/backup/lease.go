package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The freeze lease: D-31, and the reason it is a FILE.
//
// The sequence a backup performs is save-off, snapshot, save-on. The failure
// being guarded against is THE BACKUP PROCESS DYING BETWEEN THOSE TWO
// COMMANDS. What is left behind is not a crashed server -- it is a server that
// is still up, still accepting players, and silently no longer persisting
// anything. Nobody notices. The loss is discovered hours later as "the world
// rolled back to 4am", and by then the only copy of those hours is gone.
//
// A goroutine watchdog cannot help with this, and the reason is not subtle: it
// dies with the process it is supposed to be guarding. Neither can a `defer`.
// Both are inside the thing that failed. So the watchdog is OUT OF PROCESS:
// the job writes a lease file carrying a deadline and the exact thaw commands,
// and the RUNNER -- a different process, with a different lifetime, supervised
// by systemd -- arms a timer on it and executes those commands unconditionally
// when it expires.
//
// Every failure mode is then safe, and this is the table D-31 is arguing for:
//
//	job SIGKILLed        -> the lease remains, the deadline passes, the runner thaws
//	runner SIGKILLed     -> systemd restarts it and Sweep thaws the expired lease
//	both dead            -> the server is dead too, so the freeze is moot
//	box rebooted         -> /run is tmpfs, the lease is gone, the server starts with autosave on
//
// The goroutine version is safe in exactly one of those four.
//
// Two asymmetries in this file are deliberate and both point the same way,
// because the two mistakes are not equally expensive. Thawing a server that
// was not frozen re-sends `save-on` to a server that is already saving: a
// no-op. NOT thawing one that was frozen is the outage above. So:
//
//   - a lease that will not parse is treated as a freeze that IS armed, and
//     thawed from the runner's own manifest rather than ignored;
//   - a zero or absent deadline is treated as already expired, not as
//     "forever".
//
// And one asymmetry points the other way, for the same reason: the lease is
// only CLEARED after a thaw that succeeded, or by its own owner. Clearing a
// lease whose thaw failed would delete the only remaining evidence that
// autosave may still be off.

// LeaseSchema is the schema version of freeze.lease.
//
// A reader refuses a schema it does not know rather than guessing, because
// every field in this document is load-bearing for a decision about whether a
// world is being persisted -- and a deadline computed from a field that moved
// is worse than an honest refusal, which thaws.
const LeaseSchema = 1

// LeaseFileMode is section 5's mode for /run/gvm/<name>/freeze.lease.
//
// 0600, owned by the INSTANCE account rather than by root, which is the one
// combination that works: the file is written by the job (root) and has to be
// read by the runner, which runs as gvm-<name> under User= in the generated
// drop-in. Root-owned 0600 would be a safety net the runner cannot see.
//
// That the instance account can therefore also WRITE the file is not an
// escalation. The only thing a forged lease can make the runner do is re-issue
// this game's own `save-on` -- and the account in question is the game, which
// already owns the world outright.
const LeaseFileMode fs.FileMode = 0o600

// LeaseDirMode is /run/gvm/<name> as the unit declares it
// (RuntimeDirectoryMode=0750). Arm creates it only when it is missing, which
// happens because RuntimeDirectoryPreserve=restart means a clean stop takes
// the directory with it.
const LeaseDirMode fs.FileMode = 0o750

// DefaultLeaseBudget is the deadline a freeze gets when its manifest declares
// no lease_budget.
//
// Finite and short. The budget is the WORST CASE WINDOW during which a dead
// job leaves a running server not persisting, so a generous default is a
// generous outage. Two minutes covers a reflink clone and a flush with room
// to spare; the games whose snapshot is a whole tar declare their own.
const DefaultLeaseBudget = 2 * time.Minute

// MaxLeaseBudget is the longest deadline this package will arm.
//
// A deadline is a safety net, and a net nobody would wait under is not one. An
// hour is already far past any plausible freeze; a manifest asking for more has
// a bug in it, and silently honouring the number would mean an hour-long window
// in which the world is not being saved and nothing says so.
const MaxLeaseBudget = time.Hour

// Lease errors. Separate sentinels because the operator's next move differs:
// an unparseable lease is thawed and reported, while a refusal to arm is a
// backup that must not proceed.
var (
	// ErrLease is a lease document that cannot be read.
	ErrLease = errors.New("backup: the freeze lease cannot be read")
	// ErrArm is a lease this package refuses to write. It is fatal to the
	// backup: a freeze with no out-of-process watchdog behind it is the
	// failure mode of D-31 with the guard removed, and taking the backup
	// anyway would trade a world for a tarball.
	ErrArm = errors.New("backup: refusing to arm a freeze lease")
)

// Via is the channel a thaw step is delivered over. It is the manifest's own
// `via`, narrowed to the two values a freeze rung may use (spec.ViaConsole and
// spec.ViaControl): a freeze cannot be a signal and cannot be a wait_exit.
type Via string

const (
	ViaConsole Via = "console"
	ViaControl Via = "control"
)

// Valid reports whether v is a channel a freeze rung may use.
func (v Via) Valid() bool { return v == ViaConsole || v == ViaControl }

// ThawStep is one command the runner must send if the lease expires.
//
// The commands are copied INTO the lease rather than looked up by the runner,
// so that a thaw does not depend on the runner holding the same manifest
// revision the job froze with. The runner's own manifest is the FALLBACK, for
// the case where the lease itself is unreadable -- see Watcher.Fallback.
type ThawStep struct {
	Via  Via    `json:"via"`
	Send string `json:"send"`
}

// Lease is /run/gvm/<name>/freeze.lease.
//
// The field set is D-31's document verbatim, plus two optional fields that
// make "is the owner still alive" answerable rather than guessed at.
type Lease struct {
	Schema   int    `json:"schema"`
	Instance string `json:"instance"`
	// Reason is the human sentence, "backup:routine" style. It is what a
	// status view prints and what the runner logs beside DEGRADED_FREEZE.
	Reason string `json:"reason,omitempty"`
	// OwnerPID is the process that armed the lease. It lets the runner thaw
	// EARLY when the owner is provably gone, which shrinks the window from
	// the whole budget to one poll interval. It is advisory in the safe
	// direction only: a pid that cannot be inspected, or has been reused, is
	// treated as alive and the deadline still applies.
	OwnerPID int `json:"owner_pid,omitempty"`
	// OwnerStartTicks pins OwnerPID against pid reuse (/proc/<pid>/stat field
	// 22). Zero means "not recorded", which the liveness check reads as
	// "cannot establish" rather than as "gone".
	OwnerStartTicks uint64 `json:"owner_start_ticks,omitempty"`
	// OwnerUnit is the job's systemd unit, so `journalctl -u <unit>` is in
	// the document an operator finds.
	OwnerUnit string     `json:"owner_unit,omitempty"`
	ArmedAt   time.Time  `json:"armed_at"`
	Deadline  time.Time  `json:"deadline"`
	ThawSteps []ThawStep `json:"thaw_steps"`
}

// Expired reports whether the deadline has passed.
//
// A zero deadline reads as expired. That is the fail-safe direction: a
// document whose deadline field is missing or unparsed describes a freeze with
// no watchdog behind it, and the correct response to "I cannot tell when this
// expires" is to thaw now, not to wait forever.
func (l *Lease) Expired(now time.Time) bool {
	if l == nil {
		return false
	}
	if l.Deadline.IsZero() {
		return true
	}
	return !now.Before(l.Deadline)
}

// Remaining is how long is left before the deadline, floored at zero.
func (l *Lease) Remaining(now time.Time) time.Duration {
	if l == nil || l.Deadline.IsZero() {
		return 0
	}
	if d := l.Deadline.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Steps is the thaw to execute: the lease's own, or the fallback when the
// lease declares none.
//
// A lease with no thaw steps is legal and common: `after: none` is how every
// shipped manifest except Minecraft's spells a one-shot flush that has no mode
// to toggle back. For those the lease is cheap insurance and the correct thaw
// is to send nothing. The fallback is therefore only consulted for a lease
// that could not be READ at all, where "declares none" and "says nothing" are
// not distinguishable -- which is Watcher's business, not this method's.
func (l *Lease) Steps() []ThawStep {
	if l == nil {
		return nil
	}
	return l.ThawSteps
}

// Describe is the one line a status view or a log record prints.
func (l *Lease) Describe(now time.Time) string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("freeze armed")
	if l.Reason != "" {
		b.WriteString(" for " + l.Reason)
	}
	if l.OwnerPID > 0 {
		b.WriteString(fmt.Sprintf(" by pid %d", l.OwnerPID))
	}
	if l.OwnerUnit != "" {
		b.WriteString(" (" + l.OwnerUnit + ")")
	}
	switch {
	case l.Deadline.IsZero():
		b.WriteString("; no deadline, which reads as already expired")
	case l.Expired(now):
		b.WriteString("; EXPIRED " + now.Sub(l.Deadline).Round(time.Second).String() + " ago")
	default:
		b.WriteString("; thaws in " + l.Remaining(now).Round(time.Second).String())
	}
	return b.String()
}

// Bytes renders the lease as the file's contents.
func (l Lease) Bytes() ([]byte, error) {
	l.Schema = LeaseSchema
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseLease decodes a lease document.
//
// It takes an io.Reader so the whole of it is exercisable without a
// filesystem, which matters here more than almost anywhere: this is the
// document that decides whether a world is being written to disk.
func ParseLease(r io.Reader) (*Lease, error) {
	var l Lease
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLease, err)
	}
	if l.Schema != LeaseSchema {
		return nil, fmt.Errorf("%w: schema %d, this build understands %d",
			ErrLease, l.Schema, LeaseSchema)
	}
	if l.Instance == "" {
		return nil, fmt.Errorf("%w: no instance", ErrLease)
	}
	if l.ArmedAt.IsZero() {
		// Without it there is no way to tell an operator how long autosave may
		// have been off, which is the first question asked after a rollback.
		return nil, fmt.Errorf("%w: no armed_at", ErrLease)
	}
	for i, st := range l.ThawSteps {
		if !st.Via.Valid() {
			return nil, fmt.Errorf("%w: thaw_steps[%d].via is %q, want %s or %s",
				ErrLease, i, st.Via, ViaConsole, ViaControl)
		}
		if strings.TrimSpace(st.Send) == "" {
			return nil, fmt.Errorf("%w: thaw_steps[%d] sends nothing", ErrLease, i)
		}
	}
	return &l, nil
}

// BudgetFor is the deadline a freeze of this shape needs, and the sentence
// that explains it.
//
// It is a function rather than a constant because the two snapshot families
// put wildly different amounts of work inside the freeze (section 13.2). Under
// reflink or btrfs the clone is milliseconds and the archive is taken from the
// clone AFTER the thaw, so the manifest's declared budget stands. Under
// `direct` the whole tar happens between save-off and save-on -- on an 8 GB
// ext4 VPS that is the 74-second stall the dashboard renders in red -- and a
// 120-second lease would thaw in the middle of it, re-enabling autosave under
// a tar that is still reading.
//
// The throughput floor is a floor and not an estimate: pgzip at level 5 on a
// 4-vCPU VPS manages rather more, and the number exists so that a budget
// derived from it is generous rather than tight. The result is still capped,
// and the cap is reported, because a silent hour is not a safety net.
func BudgetFor(declared time.Duration, m Method, bytes int64, floorMBps int) (time.Duration, string) {
	if declared <= 0 {
		declared = DefaultLeaseBudget
	}
	if floorMBps <= 0 {
		floorMBps = DefaultFloorMBps
	}

	budget, why := declared, ""
	if !m.ThawBeforeArchive() && bytes > 0 {
		// The archive is inside the freeze, so the budget has to cover it.
		archiveFor := time.Duration(bytes/int64(floorMBps)/(1<<20)) * time.Second
		if archiveFor > 0 {
			budget = declared + archiveFor
			why = fmt.Sprintf("the %s snapshot archives %d bytes inside the freeze, so the "+
				"declared %s is extended by %s at a %d MB/s floor",
				m, bytes, declared, archiveFor, floorMBps)
		}
	}
	if budget > MaxLeaseBudget {
		why = fmt.Sprintf("%s exceeds the %s ceiling on a freeze lease and was capped; a window "+
			"longer than that in which the world is not being saved is not a safety net",
			budget, MaxLeaseBudget)
		budget = MaxLeaseBudget
	}
	if why == "" {
		// Branched on the METHOD, not left unconditional. The sentence is
		// persisted as job_runs' freeze_lease_why and extendForDirect prints
		// it again on the fallback path, so the one sentence that must never
		// appear is "the direct snapshot thaws before the archive is written":
		// direct is the method for which the freeze IS the archive. A small
		// world reaches here because bytes/floorMBps/MiB truncates to zero
		// seconds below floorMBps MiB -- correct arithmetic, and a budget
		// that needs no extension is not the same claim as a thaw that
		// happens first.
		switch {
		case m.ThawBeforeArchive():
			why = fmt.Sprintf("the %s snapshot thaws before the archive is written, so the "+
				"declared %s covers the whole freeze", m, declared)
		case bytes > 0:
			why = fmt.Sprintf("the %s snapshot archives %d bytes inside the freeze, which is "+
				"under a second at a %d MB/s floor, so the declared %s covers it unextended",
				m, bytes, floorMBps, declared)
		default:
			why = fmt.Sprintf("the %s snapshot archives inside the freeze and the source "+
				"measured as empty, so the declared %s stands unextended", m, declared)
		}
	}
	return budget, why
}

// ArmRequest is everything Arm needs. It is a struct because every field is a
// decision somebody made, and a positional call would hide which.
type ArmRequest struct {
	// Path is /run/gvm/<name>/freeze.lease.
	Path string
	// Owner is the account that must be able to READ the lease: the instance
	// user, as the instance YAML names it. It is read from the spec and never
	// derived from the instance name, for section 6.4's reason -- an adopted
	// pz-main runs as pzserver, and chowning to a gvm-pz-main that does not
	// exist fails while chowning to one that does hands the file to the wrong
	// account.
	//
	// Empty means "do not chown", which is correct only when the arming
	// process already IS that account. Arm records a note saying so rather
	// than silently producing a lease the runner cannot read.
	Owner string
	Lease Lease
}

// Arm writes the lease.
//
// It is the FIRST thing a freeze does -- before `save-off` is sent, not after.
// The ordering is the whole point: a lease written afterwards leaves a window
// exactly as wide as the send, and the send is the command that turns
// persistence off.
//
// Every refusal here is fatal to the backup. A freeze whose watchdog could not
// be armed is a freeze with D-31's guard removed, and a tarball is not worth a
// world.
func Arm(ctx context.Context, f FS, req ArmRequest) (*Lease, []string, error) {
	if f == nil {
		return nil, nil, fmt.Errorf("%w: no filesystem", ErrArm)
	}
	if req.Path == "" {
		return nil, nil, fmt.Errorf("%w: no path", ErrArm)
	}
	l := req.Lease
	l.Schema = LeaseSchema
	if l.Instance == "" {
		return nil, nil, fmt.Errorf("%w: no instance", ErrArm)
	}
	if l.ArmedAt.IsZero() {
		return nil, nil, fmt.Errorf("%w: no armed_at", ErrArm)
	}
	if l.Deadline.IsZero() {
		return nil, nil, fmt.Errorf("%w: a lease with no deadline is not a watchdog; it is a "+
			"file", ErrArm)
	}
	if !l.Deadline.After(l.ArmedAt) {
		return nil, nil, fmt.Errorf("%w: the deadline %s is not after armed_at %s",
			ErrArm, l.Deadline.UTC().Format(time.RFC3339), l.ArmedAt.UTC().Format(time.RFC3339))
	}
	if d := l.Deadline.Sub(l.ArmedAt); d > MaxLeaseBudget {
		return nil, nil, fmt.Errorf("%w: a %s deadline is past the %s ceiling", ErrArm, d, MaxLeaseBudget)
	}
	for i, st := range l.ThawSteps {
		if !st.Via.Valid() {
			return nil, nil, fmt.Errorf("%w: thaw_steps[%d].via is %q", ErrArm, i, st.Via)
		}
		if strings.TrimSpace(st.Send) == "" {
			return nil, nil, fmt.Errorf("%w: thaw_steps[%d] sends nothing", ErrArm, i)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrArm, err)
	}

	body, err := l.Bytes()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrArm, err)
	}
	// RuntimeDirectoryPreserve=restart means a clean stop removes
	// /run/gvm/<name>, and the freeze is armed against a RUNNING server -- so
	// the directory is normally there and this is the unusual path. Creating
	// it is still correct: a lease whose directory is gone is not a lease.
	if err := f.MkdirAll(dirOf(req.Path), LeaseDirMode); err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrArm, req.Path, err)
	}
	// One owned publication, not a write followed by a chown. The reader is a
	// different process with a different account: a lease published
	// root-owned 0600 is unreadable by the runner for as long as it takes to
	// hand it over, and the runner reads an unreadable lease as a freeze that
	// IS armed -- so it thaws, and clears the lease that was armed a
	// microsecond ago, leaving the rest of the snapshot with no guard at all.
	// A failed chown therefore publishes nothing rather than publishing
	// something that has to be cleaned up.
	var notes []string
	if req.Owner == "" {
		notes = append(notes, "the freeze lease was armed without an owner, so it is readable "+
			"only by the account that wrote it; the runner honours it only if that is the "+
			"account the game runs as")
	}
	if err := f.WriteFileOwned(req.Path, body, LeaseFileMode, req.Owner, ""); err != nil {
		if req.Owner != "" {
			// The chown half is fatal, and this is the one chown in gvm that
			// is. The runner reads this file as the instance user; if it
			// cannot, the lease exists and does nothing, which is
			// indistinguishable from not having armed one at all except that
			// the backup believes it is protected.
			return nil, notes, fmt.Errorf("%w: %s could not be written for %s, so the runner "+
				"could not read it and the freeze would have had no watchdog: %w",
				ErrArm, req.Path, req.Owner, err)
		}
		return nil, notes, fmt.Errorf("%w: %s: %w", ErrArm, req.Path, err)
	}
	return &l, notes, nil
}

// ReadLease reads and parses the lease at path.
//
// A missing lease is (nil, nil): no freeze is armed, which is the normal state
// and not an error. Anything else that goes wrong is reported, because this
// package's one rule is that "I could not tell" means "thaw".
func ReadLease(f FS, path string) (*Lease, error) {
	b, err := f.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrLease, path, err)
	}
	return ParseLease(strings.NewReader(string(b)))
}

// ClearLease removes the lease. It is idempotent: a lease the runner's own
// timer already thawed and cleared is not an error for the job that armed it.
func ClearLease(f FS, path string) error {
	if err := f.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: clearing %s: %w", path, err)
	}
	return nil
}

// Renew rewrites the lease with a later deadline.
//
// It exists for the caller that wants a heartbeat around a long `direct`
// snapshot instead of one wide budget, and it fails safe by construction: if
// the renewing code dies, no later deadline is written and the runner thaws at
// the one already on disk. That is the opposite of a goroutine watchdog, which
// dies holding the only thing keeping the world unprotected.
//
// owner is required for the same reason Arm's is, and forgetting it here would
// be a subtle way to break the whole mechanism: the write replaces the file
// rather than editing it in place, so a renewal with no owner would leave a
// root-owned 0600 lease where a gvm-<name>-owned one had been -- still on
// disk, still saying a freeze is armed, and no longer readable by the runner
// that is supposed to honour it.
func Renew(ctx context.Context, f FS, path string, l *Lease, deadline time.Time, owner string) (*Lease, error) {
	if l == nil {
		return nil, fmt.Errorf("%w: nothing to renew", ErrArm)
	}
	next := *l
	next.Deadline = deadline
	// Through Arm, so a renewal cannot write a document Arm would have
	// refused -- including one whose new deadline is past the ceiling.
	out, _, err := Arm(ctx, f, ArmRequest{Path: path, Owner: owner, Lease: next})
	return out, err
}

// Thawer is the runner's side of D-31: the channels a thaw step is delivered
// over.
//
// It is a seam and not a convenience. The whole point of this design is that
// the thaw happens in a DIFFERENT process from the one that armed the lease,
// and the only way to test "a fake runner honouring the lease thaws at the
// deadline" on the development machine is for that runner's channels to be
// injectable. Nothing here spawns, dials or blocks indefinitely.
type Thawer interface {
	// Thaw delivers one step. It must be safe to call twice: the lease is
	// retried until it succeeds, and `save-on` is idempotent by nature.
	Thaw(ctx context.Context, step ThawStep) error
}

// ThawerFunc adapts a function to Thawer.
type ThawerFunc func(ctx context.Context, step ThawStep) error

// Thaw implements Thawer.
func (f ThawerFunc) Thaw(ctx context.Context, step ThawStep) error { return f(ctx, step) }

// Decision is what one tick of the lease watch concluded.
//
// The tick is a pure-ish function returning one of these rather than a
// goroutine doing things, because the interesting half of this mechanism is
// the DECISION -- expired, owner gone, unreadable -- and a decision buried in a
// select loop with a real timer in it is a decision that gets tested on the
// production box.
type Decision string

const (
	// DecisionNone is no lease on disk: no freeze is armed.
	DecisionNone Decision = "none"
	// DecisionWait is a live lease whose owner may still be working.
	DecisionWait Decision = "wait"
	// DecisionThaw is a lease whose deadline has passed, or whose owner is
	// provably gone.
	DecisionThaw Decision = "thaw"
	// DecisionUnreadable is a lease file that exists and will not parse. It
	// is treated as armed, and thawed from the fallback steps, because a
	// freeze that might be on is a world that might not be saving.
	DecisionUnreadable Decision = "unreadable"
)

// Tick is what one pass of the watch did.
type Tick struct {
	Decision Decision
	Lease    *Lease
	// Why is the sentence that goes in the log beside DEGRADED_FREEZE.
	Why string
	// Steps are the thaw steps that were executed, in order.
	Steps []ThawStep
	// Thawed is true when every step succeeded (including the vacuous case of
	// a lease with no steps), which is also the only case that clears the
	// lease.
	Thawed bool
	// Cleared says the lease file was removed.
	Cleared bool
	// Err is the first thaw failure. The lease is deliberately left in place
	// when this is non-nil.
	Err error
}

// Watcher is the runner's lease honour loop, as a value with no goroutine in
// it.
//
// The runner calls Sweep once on start and Step on a timer. Watch is the
// fifteen-line loop around Step for a caller that wants one.
type Watcher struct {
	// Path is /run/gvm/<name>/freeze.lease.
	Path string
	FS   FS
	// Thaw delivers the steps. A nil Thawer makes every expiry a recorded
	// failure rather than a silent success: a runner that cannot reach its
	// own game must not report a world as thawed.
	Thaw Thawer
	// Fallback is this game's freeze.after from the runner's OWN manifest,
	// used only for a lease that could not be parsed. The runner holds the
	// manifest already -- it rendered the shutdown ladder from it -- so the
	// unreadable case still has a correct thaw available rather than only a
	// log line.
	Fallback []ThawStep
	// Alive answers "is the lease's owner still running". Unknown is the
	// honest answer on a platform with no /proc and for a pid the caller may
	// not inspect, and it is read as "assume alive", so only the deadline
	// thaws. Nil behaves as Unknown.
	Alive func(pid int, startTicks uint64) lifecycle.Tri
	// Now is the clock.
	Now func() time.Time
	Log *slog.Logger
	// Interval is how often Watch polls. Zero is DefaultWatchInterval.
	//
	// It is a poll and not inotify on purpose: the expiry that matters is a
	// TIMER, not a file event -- the lease is written once and then nothing
	// happens to it -- so a watch that only woke on inotify would never fire
	// for the case the whole design exists to cover.
	Interval time.Duration
}

// DefaultWatchInterval bounds how long after a deadline the thaw happens.
//
// Five seconds, because the deadline is already the budget: the lease says
// "thaw at 04:02:03" and an operator reading the log wants the thaw to be at
// 04:02:03, not at 04:03:03. It costs one stat of a tmpfs file.
const DefaultWatchInterval = 5 * time.Second

func (w *Watcher) now() time.Time {
	if w != nil && w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) log() *slog.Logger {
	if w != nil && w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Watcher) alive(l *Lease) lifecycle.Tri {
	if w == nil || w.Alive == nil || l == nil || l.OwnerPID <= 0 {
		return lifecycle.Unknown
	}
	if l.OwnerStartTicks == 0 {
		// An UNPINNED lease, and the zero contract on OwnerStartTicks is
		// decided HERE so that no future wiring can get it wrong. Without
		// field 22 there is no way to tell a dead owner from a
		// recorded-nothing one -- and platform.Alive compares st.StartTicks
		// against what it was handed, so a LIVE pid read against a zero pin
		// comes back false, i.e. "provably dead". Honouring that would send
		// save-on in the middle of somebody's snapshot and clear the lease
		// that was the only evidence, which is the one asymmetry this file
		// refuses. Unpinned falls back to the deadline.
		return lifecycle.Unknown
	}
	return w.Alive(l.OwnerPID, l.OwnerStartTicks)
}

// Sweep is the pass the runner makes on its own start.
//
// It is one Step and nothing more, and that is the point: the case it exists
// for is a runner that was SIGKILLed while a backup held a freeze. systemd
// restarts it, RuntimeDirectoryPreserve=restart has kept /run/gvm/<name>, and
// the lease is still sitting there with nobody watching it. A runner that
// armed its timer only for leases it saw APPEAR would never look at that one.
//
// It deliberately does not thaw a lease that is still live with its owner
// possibly working: that would be a save-on in the middle of somebody's
// snapshot. Only an expired lease, or one whose owner is provably gone, is
// acted on -- which is exactly Step's rule.
func (w *Watcher) Sweep(ctx context.Context) Tick { return w.Step(ctx) }

// Step is one pass: read, decide, and thaw if the decision says to.
func (w *Watcher) Step(ctx context.Context) Tick {
	now := w.now()
	l, err := ReadLease(w.FS, w.Path)
	switch {
	case err != nil:
		// A file that exists and will not parse. Thaw from the fallback: the
		// alternative is to leave a possibly-frozen world unsaved because a
		// JSON document was truncated.
		t := Tick{
			Decision: DecisionUnreadable,
			Why: fmt.Sprintf("%s exists and cannot be read (%v), so a freeze may be armed; "+
				"thawing from this game's own manifest", w.Path, err),
		}
		return w.thaw(ctx, t, w.Fallback)
	case l == nil:
		return Tick{Decision: DecisionNone}
	}

	switch alive := w.alive(l); {
	case alive == lifecycle.No:
		// The owner is provably gone, so nobody is going to send `save-on`.
		// Thawing now rather than at the deadline shrinks the window the world
		// spends unsaved from the whole budget to one poll interval.
		t := Tick{
			Decision: DecisionThaw,
			Lease:    l,
			Why: fmt.Sprintf("the freeze's owner (pid %d) is gone and the lease is still armed; "+
				"thawing %s before its %s deadline", l.OwnerPID, l.Describe(now),
				l.Remaining(now).Round(time.Second)),
		}
		return w.thaw(ctx, t, l.Steps())
	case l.Expired(now):
		t := Tick{
			Decision: DecisionThaw,
			Lease:    l,
			Why:      fmt.Sprintf("the freeze lease expired: %s", l.Describe(now)),
		}
		return w.thaw(ctx, t, l.Steps())
	default:
		return Tick{Decision: DecisionWait, Lease: l, Why: l.Describe(now)}
	}
}

// thaw executes the steps and decides whether the lease may be cleared.
func (w *Watcher) thaw(ctx context.Context, t Tick, steps []ThawStep) Tick {
	// DEGRADED_FREEZE, spelled as D-31 spells it, because this is the line an
	// operator greps for after a rollback. It is logged before the steps run:
	// a thaw that then wedges must still have said that it was needed.
	w.log().Error("DEGRADED_FREEZE", "lease", w.Path, "decision", string(t.Decision), "why", t.Why)

	if len(steps) == 0 {
		// `after: none`, which is how every shipped manifest except
		// Minecraft's spells a one-shot flush with no mode to toggle back.
		// There is nothing to send, so the thaw is complete and the lease has
		// served its purpose.
		t.Thawed = true
		t.Cleared = ClearLease(w.FS, w.Path) == nil
		return t
	}
	if w.Thaw == nil {
		t.Err = errors.New("backup: this runner has no channel to thaw through, so the freeze " +
			"could not be lifted; autosave may still be off")
		w.log().Error("DEGRADED_FREEZE thaw impossible", "lease", w.Path, "err", t.Err)
		return t
	}
	for _, st := range steps {
		if err := w.Thaw.Thaw(ctx, st); err != nil {
			t.Err = fmt.Errorf("backup: thaw %s %q: %w", st.Via, st.Send, err)
			w.log().Error("DEGRADED_FREEZE thaw failed", "lease", w.Path,
				"via", string(st.Via), "send", st.Send, "err", err)
			// The lease stays. It is the only remaining record that this
			// world may not be saving, and lifecycle's freeze_armed finding
			// is what surfaces it to `gvm doctor` and to the dashboard.
			return t
		}
		t.Steps = append(t.Steps, st)
	}
	t.Thawed = true
	t.Cleared = ClearLease(w.FS, w.Path) == nil
	w.log().Warn("freeze thawed by the lease watchdog", "lease", w.Path, "steps", len(t.Steps))
	return t
}

// Watch polls until ctx is done, and is the whole of what the runner has to
// wire up.
//
// It reports every tick to on, when on is non-nil, so the runner can put the
// thaw in its own console ring -- "the backup's freeze expired and I have
// re-enabled saving" is a line the operator watching the console deserves.
func (w *Watcher) Watch(ctx context.Context, on func(Tick)) {
	iv := w.Interval
	if iv <= 0 {
		iv = DefaultWatchInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		tick := w.Step(ctx)
		if on != nil && tick.Decision != DecisionNone {
			on(tick)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
