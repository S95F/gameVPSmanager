package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// This file answers one question: systemd started `gvm job run pz-main_nightly`
// -- what is that?
//
// It has to be answerable with no database, because D-29 makes systemd the
// trigger and Postgres only the ledger: the nightly must fire when gvmd,
// gvm-web and Postgres are all dead. So the slug carries the two NAMES (D-13)
// and a file on disk carries the action. Nothing here reads a row.
//
// The ambiguity that D-45 removes is worth stating in full, because it is the
// reason instance names cannot contain '_'. Given "a_b_c" there are two legal
// readings, and a job layer that picked one would address the wrong instance
// with a verb that stops servers. Neither half may contain the separator, so
// there is exactly one reading of exactly one '_', and a slug with two is
// refused rather than guessed at.

// ErrSlug is a slug gvm will not act on.
var ErrSlug = errors.New("jobs: the job slug does not name a job")

// ErrDescriptor is a job descriptor gvm will not act on.
var ErrDescriptor = errors.New("jobs: the job descriptor does not describe a job")

// Slug is the "<instance>_<job>" systemd passed as %i (D-13).
//
// The instance half is empty for a host-level job, and that is the whole
// representation of "host-level" -- there is no bool. A separate flag would be
// a second source of truth for the same fact, and the two would disagree the
// first time something constructed a Slug by hand.
type Slug struct {
	// Instance is the instance name, or "" for a host-level job.
	Instance string
	// Job is the job name: "nightly", "guard", "verify".
	Job string
}

// hostJobs are the job names that are legal with an empty instance part, and
// the kind each one is.
//
// Closed, and short on purpose. A host-level job acts on every instance on the
// box, so a new one is a decision; an open set would let `gvm job run _rebuild`
// mean something nobody designed. These two are exactly what D-13 names and
// exactly what packaging/systemd ships a unit for.
var hostJobs = map[string]lifecycle.JobKind{
	"guard":  lifecycle.JobGuard,
	"verify": lifecycle.JobVerify,
}

// HostJobs returns the host-level job names, sorted.
func HostJobs() []string {
	out := make([]string, 0, len(hostJobs))
	for n := range hostJobs {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// actionKinds maps a job ACTION to the kind that executes it.
//
// Two lists exist and they are not the same list, which is worth being
// explicit about. spec.JobActions() is what an instance YAML may SCHEDULE --
// five actions, validated at load. This is what the job layer can EXECUTE,
// which is a superset: `prune` and `verify` are reachable from the CLI and
// from the host-level timer without appearing in any instance's jobs block,
// and `guard` is the recovery sweep itself. The direction of the inclusion is
// asserted by slug_test.go, because the dangerous drift is the other one -- a
// validator accepting an action nothing can run produces a timer that fires
// every night and fails every night.
//
// The strings are identical to the kind strings, and that is not laziness: the
// action an operator writes in YAML, the kind in the ledger's enum and the
// word in `systemctl list-timers` are all the same word, so there is nothing
// to translate when reading a journal.
var actionKinds = map[string]lifecycle.JobKind{
	"backup":       lifecycle.JobBackup,
	"update":       lifecycle.JobUpdate,
	"restart":      lifecycle.JobRestart,
	"content_sync": lifecycle.JobContentSync,
	"rebuild":      lifecycle.JobRebuild,
	"restore":      lifecycle.JobRestore,
	"prune":        lifecycle.JobPrune,
	"verify":       lifecycle.JobVerify,
	"guard":        lifecycle.JobGuard,
}

// Actions returns the job actions the job layer can execute, sorted.
func Actions() []string {
	out := make([]string, 0, len(actionKinds))
	for a := range actionKinds {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// KindForAction maps an action to its kind.
func KindForAction(action string) (lifecycle.JobKind, bool) {
	k, ok := actionKinds[action]
	return k, ok
}

// ParseSlug splits what systemd handed `gvm job run %i`.
//
// The split itself is instance.SplitJobSlug, which is where D-13's charset
// reasoning already lives; this adds the one rule that belongs to the job
// layer rather than to naming -- an empty instance part is only legal for a
// job name that is actually host-level. Without that, `_nightly` would parse,
// resolve to no instance, and then be handed to a backup runner that would
// derive paths from an empty name.
func ParseSlug(s string) (Slug, error) {
	inst, job, err := instance.SplitJobSlug(s)
	if err != nil {
		return Slug{}, fmt.Errorf("%w: %w", ErrSlug, err)
	}
	if inst == "" {
		if _, ok := hostJobs[job]; !ok {
			return Slug{}, fmt.Errorf("%w: %q has an empty instance part, which is how a "+
				"HOST-level job is spelt, but %q is not one of them (%s) (D-13)",
				ErrSlug, s, job, strings.Join(HostJobs(), ", "))
		}
	}
	return Slug{Instance: inst, Job: job}, nil
}

// Host reports whether this is a host-level job.
func (s Slug) Host() bool { return s.Instance == "" }

// String renders the slug, which is also the systemd template parameter.
func (s Slug) String() string { return s.Instance + "_" + s.Job }

// Unit is the job's service unit.
func (s Slug) Unit() string { return instance.JobUnit(s.Instance, s.Job) }

// Timer is the timer that triggers it.
func (s Slug) Timer() string { return instance.JobTimer(s.Instance, s.Job) }

// DescriptorPath is where the slug's descriptor lives.
func (s Slug) DescriptorPath() string { return instance.JobDescPath(s.Instance, s.Job) }

// LockPath is the flock this job takes: one job at a time per instance (D-30),
// or per host job for `_guard` and `_verify`. Both come from the host-wide
// locks directory, so there is no asymmetry left between the two cases.
//
// It is deliberately NOT /run/gvm/<name>/job.lock, which is Paths.RunnerLock,
// and the reasons are worth keeping because the earlier version of this
// comment argued for that path:
//
//   - The RUNNER holds it, for the whole life of the unit (section 11.7 lock
//     1, taken in runner.Preflight). A job that took it could never acquire
//     on a live instance, and a job that did acquire while the instance was
//     down would make its own startStep's `gvm run` exit 75 in preflight --
//     and the release is a defer registered before the guard's Restore, so
//     the guard re-issues the same start into the same lock and the world
//     stays down.
//   - /run/gvm/<name> is gvm@.service's RuntimeDirectory=, with
//     RuntimeDirectoryPreserve=restart, so systemd REMOVES it when the unit
//     goes inactive -- unlinking a flocked file under its holder, after which
//     Acquire's MkdirAll plus O_CREATE hands a second job a fresh inode at
//     the same path and both lock. That is the two-holders hazard
//     Lock.Release and platform.FileLock.Unlock both already refuse to
//     create.
//
// Losing tmpfs costs nothing: no flock survives a process death or a reboot
// wherever the file lives, and a stale holder LINE cannot be misread as
// contention, because a *Busy is only produced after a real TryLock failure
// and writeHolder truncates.
func (s Slug) LockPath() string {
	if s.Host() {
		return instance.HostJobLockPath(s.Job)
	}
	return instance.InstanceJobLockPath(s.Instance)
}

// Descriptor is /etc/gvm/jobs/<instance>_<job>.json (D-13).
//
// It is written by `gvm apply` (internal/reconcile.JobDescriptor renders it)
// and read here. The shape is deliberately identical to the writer's; the
// alternative -- importing internal/reconcile for the struct -- would put the
// whole apply planner, and its archive and probe dependencies, into the
// dependency graph of a job that only wanted to know one word.
type Descriptor struct {
	Instance string `json:"instance"`
	Job      string `json:"job"`
	Action   string `json:"action"`
}

// ParseDescriptor reads a descriptor document.
func ParseDescriptor(r io.Reader) (Descriptor, error) {
	var d Descriptor
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrDescriptor, err)
	}
	if d.Action == "" {
		// The writer defaults a missing action to the job's own name, so a
		// descriptor with no action is one that was hand-written or produced
		// by a different program. Refusing is cheap and the fallback in
		// Resolve covers the legitimate case.
		return Descriptor{}, fmt.Errorf("%w: no action", ErrDescriptor)
	}
	if _, ok := actionKinds[d.Action]; !ok {
		return Descriptor{}, fmt.Errorf("%w: %q is not a job action (want %s)",
			ErrDescriptor, d.Action, strings.Join(Actions(), ", "))
	}
	return d, nil
}

// Kind is the job kind this descriptor's action maps to.
func (d Descriptor) Kind() (lifecycle.JobKind, bool) { return KindForAction(d.Action) }

// DescFS is how Resolve reads the descriptor.
//
// One method, because that is the whole of this package's business with
// /etc/gvm/jobs: a seam this small costs nothing and makes every branch below
// -- a missing file, a corrupt file, a file that disagrees with its own name --
// a test on the development machine rather than a fixture tree.
type DescFS interface {
	ReadFile(name string) ([]byte, error)
}

// OSDescFS reads the real /etc/gvm/jobs.
var OSDescFS DescFS = osDescFS{}

type osDescFS struct{}

func (osDescFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Via says where a resolution's action came from.
type Via string

const (
	// ViaHostJob is a host-level slug, whose kind is implied by its name.
	ViaHostJob Via = "host job"
	// ViaDescriptor is /etc/gvm/jobs/<slug>.json, the normal case.
	ViaDescriptor Via = "descriptor"
	// ViaJobName is the fallback: the job name is itself an action.
	//
	// It exists because `gvm backup pz-main` has to work on a box where
	// `gvm apply` has not run yet -- during an adoption, or after someone
	// deleted /etc/gvm/jobs. A job called "backup" doing a backup is not a
	// guess; a job called "nightly" has no such reading, and Resolve says so
	// with the path of the file it wanted.
	ViaJobName Via = "job name"
)

// Resolved is what systemd's %i turned out to mean.
type Resolved struct {
	Slug   Slug
	Kind   lifecycle.JobKind
	Action string
	Via    Via
}

// Resolve turns a slug into an instance, a job and a kind.
//
// The order is: host jobs first (they have no descriptor and never need one),
// then the descriptor, then the job name. The descriptor is AUTHORITATIVE when
// it parses, and it is cross-checked against the slug: a descriptor whose
// instance or job disagrees with the file it was read from is refused rather
// than believed, because the only ways to produce one are a hand edit and a
// copy between boxes -- and the consequence of believing it is a job that
// stops the wrong world.
//
// A descriptor that EXISTS and does not parse is a refusal, never a fallback.
// Falling through to the job name there would turn a corrupt file into a
// silently different action, which is the one outcome worse than not running.
func Resolve(fsys DescFS, slug string) (Resolved, error) {
	s, err := ParseSlug(slug)
	if err != nil {
		return Resolved{}, err
	}
	if s.Host() {
		// Checked by ParseSlug, so this cannot fail; the lookup is repeated
		// rather than threaded through so that the two files cannot drift.
		return Resolved{Slug: s, Kind: hostJobs[s.Job], Action: s.Job, Via: ViaHostJob}, nil
	}
	if fsys == nil {
		fsys = OSDescFS
	}
	path := s.DescriptorPath()
	b, readErr := fsys.ReadFile(path)
	switch {
	case readErr == nil:
		d, err := ParseDescriptor(strings.NewReader(string(b)))
		if err != nil {
			return Resolved{}, fmt.Errorf("%s: %w", path, err)
		}
		if d.Instance != s.Instance || d.Job != s.Job {
			return Resolved{}, fmt.Errorf("%w: %s describes %s_%s; a descriptor that disagrees "+
				"with its own filename was hand-edited or copied from another box, and "+
				"believing it would run this job against the wrong instance",
				ErrDescriptor, path, d.Instance, d.Job)
		}
		k, _ := d.Kind()
		return Resolved{Slug: s, Kind: k, Action: d.Action, Via: ViaDescriptor}, nil

	case errors.Is(readErr, os.ErrNotExist):
		k, ok := actionKinds[s.Job]
		if !ok {
			return Resolved{}, fmt.Errorf("%w: there is no %s, and %q is not itself a job "+
				"action (%s) -- run `gvm apply %s` to write the descriptor",
				ErrSlug, path, s.Job, strings.Join(Actions(), ", "), s.Instance)
		}
		return Resolved{Slug: s, Kind: k, Action: s.Job, Via: ViaJobName}, nil

	default:
		return Resolved{}, fmt.Errorf("%w: %s: %v", ErrSlug, path, readErr)
	}
}
