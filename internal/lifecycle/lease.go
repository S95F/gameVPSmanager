package lifecycle

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

// Lease errors. Each is a sentinel because the operator's next move differs: a
// lease gvm cannot read has to be cleared by hand, while a lease from a
// previous boot clears itself.
var (
	// ErrLease is a lease document that does not parse. It is deliberately
	// NOT treated as "no lease": a file that exists and cannot be read is a
	// job that may still be running, and guessing it is not is how two
	// updaters end up writing the same install tree.
	ErrLease = errors.New("lifecycle: the job lease cannot be read")
	// ErrHolder is a malformed holder line in a lock file.
	ErrHolder = errors.New("lifecycle: the lock file does not name its holder")
)

// LeaseProto is the schema version of lease.json. A document from a future gvm
// is refused rather than half-read, for the same reason the archive manifest
// carries one: a lease whose deadline field moved is a lease whose expiry is
// being computed from the wrong number.
const LeaseProto = 1

// LeaseFileName is the job lease under /var/lib/gvm/instances/<name>/.
const LeaseFileName = "lease.json"

// JobKind is `job_kind_t` from the schema (section 14.4).
//
// The set is closed and lives here rather than in internal/jobs because the
// phase table has to decide what a lease of each kind MEANS, and a kind that
// exists only in the job package is a kind this file would silently treat as
// harmless.
type JobKind string

const (
	JobBackup      JobKind = "backup"
	JobUpdate      JobKind = "update"
	JobRestart     JobKind = "restart"
	JobContentSync JobKind = "content_sync"
	JobRebuild     JobKind = "rebuild"
	JobRestore     JobKind = "restore"
	JobPrune       JobKind = "prune"
	JobVerify      JobKind = "verify"
	JobGuard       JobKind = "guard"
	JobAdopt       JobKind = "adopt"
	JobPromote     JobKind = "promote"
	JobApply       JobKind = "apply"
	JobInstall     JobKind = "install"
)

var jobKinds = []JobKind{
	JobBackup, JobUpdate, JobRestart, JobContentSync, JobRebuild, JobRestore,
	JobPrune, JobVerify, JobGuard, JobAdopt, JobPromote, JobApply, JobInstall,
}

// JobKinds returns every declared kind in schema order.
func JobKinds() []JobKind { return append([]JobKind(nil), jobKinds...) }

func (k JobKind) Valid() bool {
	for _, q := range jobKinds {
		if k == q {
			return true
		}
	}
	return false
}

func (k JobKind) String() string { return string(k) }

// Phase is the phase a LIVE lease of this kind puts the instance in.
//
// Only jobs that rewrite the install tree or the world mask the phase, and the
// second return value says whether this kind is one of them. A live backup or
// verify lease does NOT make an instance "updating": the server is up, players
// are on it, and reporting a nightly backup as an update would train an
// operator to ignore the word. What every kind of lease does do is show up in
// Status.Lease, so "why is it busy" is still answerable.
//
// Note the asymmetry with staleness, which is deliberate. A STALE lease of any
// kind is a failure, backup included -- a backup job that died between
// `save-off` and `save-on` leaves a running server that has silently stopped
// persisting, which section 13.2 calls the worst failure in the system.
func (k JobKind) Phase() (Phase, bool) {
	switch k {
	case JobInstall:
		return PhaseInstalling, true
	case JobUpdate, JobRebuild, JobRestore, JobAdopt, JobPromote, JobContentSync:
		return PhaseUpdating, true
	}
	return "", false
}

// Lease is /var/lib/gvm/instances/<name>/lease.json: the job that currently
// owns an instance (section 5.3).
//
// It is a file rather than a row because section 1 promises the root CLI keeps
// working with Postgres down, and "is an update running" is the first question
// `gvm start` has to answer. instance_status.lease_job_id mirrors it.
//
// It is under /var/lib/gvm rather than /run for one reason: /run is tmpfs, and
// a lease that vanished on reboot could not tell the difference between "the
// job finished" and "the box died mid-update". BootID is what makes the reboot
// case decidable instead.
type Lease struct {
	Proto    int     `json:"proto"`
	Instance string  `json:"instance"`
	Kind     JobKind `json:"kind"`
	// JobID is the job_runs row id when there is one. Zero is normal: a root
	// CLI job with Postgres down still takes a lease.
	JobID int64 `json:"job_id,omitempty"`
	// Unit is the transient or timer-launched unit that holds it, e.g.
	// gvm-job@pz-main_nightly.service.
	Unit string `json:"unit,omitempty"`
	PID  int    `json:"pid,omitempty"`
	// BootID is /proc/sys/kernel/random/boot_id at acquisition. A lease from
	// another boot cannot still be running, which is the same reasoning
	// section 14.4 applies to zombie job_runs rows.
	BootID    string    `json:"boot_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Deadline is when the lease self-expires. It is optional and it is a
	// BACKSTOP, not the primary signal: a job that is genuinely still running
	// past its deadline is still running, so Classify only uses the deadline
	// when liveness cannot be established.
	Deadline time.Time `json:"deadline,omitzero"`
	// Reason is what to tell an operator who finds it. It is written by the
	// job, not derived, because "nightly update, phase 2 of 7" is knowledge
	// the job has and this package does not.
	Reason string `json:"reason,omitempty"`
}

// LeasePath is where an instance's lease lives. Section 5 is transcribed in
// internal/instance and nowhere else, so this is a lookup rather than a join.
func LeasePath(name string) string { return instance.PathsFor(name).Lease }

// ParseLease decodes a lease document.
//
// It takes an io.Reader so the whole of it is testable without a filesystem,
// which matters more here than almost anywhere else: this is the document that
// decides whether `gvm update` refuses to run.
func ParseLease(r io.Reader) (*Lease, error) {
	var l Lease
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLease, err)
	}
	if l.Proto != LeaseProto {
		return nil, fmt.Errorf("%w: proto %d, this build understands %d",
			ErrLease, l.Proto, LeaseProto)
	}
	if !l.Kind.Valid() {
		return nil, fmt.Errorf("%w: %q is not a job kind", ErrLease, l.Kind)
	}
	if l.StartedAt.IsZero() {
		// Without it there is no way to tell an operator how long they have
		// been blocked, and "cleared a lease of unknown age" is not a thing
		// anybody should have to do.
		return nil, fmt.Errorf("%w: no started_at", ErrLease)
	}
	return &l, nil
}

// Bytes renders the lease as the file's contents.
func (l Lease) Bytes() ([]byte, error) {
	l.Proto = LeaseProto
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// LeaseState is what an observed lease is worth.
type LeaseState string

const (
	// LeaseNone is no lease at all.
	LeaseNone LeaseState = "none"
	// LeaseLive means a job holds this instance right now.
	LeaseLive LeaseState = "live"
	// LeaseStale means the holder is provably gone and the lease was never
	// released. Nothing auto-resumes from it and nothing auto-clears it.
	LeaseStale LeaseState = "stale"
)

// Classify decides whether a lease is live or stale.
//
// The ordering is the whole point and it is asymmetric on purpose. Evidence
// that the holder is GONE is required to call a lease stale; absence of
// evidence leaves it live. Getting that backwards means a second updater
// starts while the first is still writing the install tree, which is
// unrecoverable without a restore. Getting it "wrong" in the safe direction
// means an operator runs `gvm job clear`, reads what it says, and continues.
//
//   - A different boot id is proof: the process cannot have survived the
//     reboot, and /var/lib/gvm did.
//   - A dead pid is proof, when the caller could look. Tri exists so that
//     "could not look" (no /proc, wrong platform, permission) is not spelled
//     the same way as "looked, and it is gone".
//   - The deadline is a backstop for the case where neither of the above was
//     available. A lease whose holder is alive is never stale, deadline or
//     no deadline: killing a running update is worse than waiting for one.
func (l *Lease) Classify(now time.Time, bootID string, alive Tri) LeaseState {
	if l == nil {
		return LeaseNone
	}
	if bootID != "" && l.BootID != "" && bootID != l.BootID {
		return LeaseStale
	}
	switch alive {
	case No:
		return LeaseStale
	case Yes:
		return LeaseLive
	}
	if !l.Deadline.IsZero() && !now.IsZero() && now.After(l.Deadline) {
		return LeaseStale
	}
	return LeaseLive
}

// Age is how long the lease has been held.
func (l *Lease) Age(now time.Time) time.Duration {
	if l == nil || now.IsZero() || l.StartedAt.IsZero() {
		return 0
	}
	return now.Sub(l.StartedAt)
}

// Describe is the one line a status view prints for a lease.
func (l *Lease) Describe(now time.Time) string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(l.Kind))
	if l.Unit != "" {
		b.WriteString(" (" + l.Unit + ")")
	} else if l.PID > 0 {
		b.WriteString(" (pid " + strconv.Itoa(l.PID) + ")")
	}
	if d := l.Age(now); d > 0 {
		b.WriteString(" held for " + d.Round(time.Second).String())
	}
	if l.Reason != "" {
		b.WriteString(": " + l.Reason)
	}
	return b.String()
}

// Holder is the first line of a LOCK file -- /run/gvm/<name>/job.lock and
// /var/lib/gvm/locks/steamcmd.lock (D-30).
//
// It is a different thing from a Lease and both exist. The lock is the flock
// that serialises access; the line inside it is only so that `doctor` can name
// who is blocking instead of printing "held". The lease is the durable record
// of which job owns an INSTANCE, and it survives the process that took it,
// which is precisely what a flock does not do.
type Holder struct {
	PID       int
	Unit      string
	Kind      string
	StartedAt time.Time
}

// Line renders the holder line. The format is section D-30's:
// pid:unit:kind:started_at, with the timestamp last because RFC3339 contains
// colons of its own and anything before it would need escaping.
func (h Holder) Line() string {
	return strconv.Itoa(h.PID) + ":" + h.Unit + ":" + h.Kind + ":" +
		h.StartedAt.UTC().Format(time.RFC3339)
}

// ParseHolder reads the first line of a lock file.
func ParseHolder(r io.Reader) (Holder, error) {
	br := bufio.NewReader(io.LimitReader(r, 4<<10))
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return Holder{}, fmt.Errorf("%w: %v", ErrHolder, err)
	}
	line = strings.TrimRight(line, "\r\n")

	// Four fields, split from the LEFT, so the timestamp keeps its own colons.
	parts := strings.SplitN(line, ":", 4)
	if len(parts) != 4 {
		return Holder{}, fmt.Errorf("%w: %q has %d fields, want pid:unit:kind:started_at",
			ErrHolder, line, len(parts))
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return Holder{}, fmt.Errorf("%w: %q is not a pid", ErrHolder, parts[0])
	}
	at, err := time.Parse(time.RFC3339, parts[3])
	if err != nil {
		return Holder{}, fmt.Errorf("%w: %q is not an RFC3339 time", ErrHolder, parts[3])
	}
	return Holder{PID: pid, Unit: parts[1], Kind: parts[2], StartedAt: at}, nil
}
