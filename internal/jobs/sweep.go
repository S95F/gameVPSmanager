package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// Sweep is the production Sweeper: section 13.4's recovery sweep, assembled
// from two things that already exist.
//
// The enumeration is lifecycle.Reconciler.Sweep and nothing else, and that is
// the whole design of this file. The reconciler already derives section 13.4's
// predicate -- spec.enabled && not held && the unit is not up -- as
// FindingNotRunning, over every instance in /etc/gvm/instances, with no
// database and no systemd write anywhere in its type. Re-deriving it here
// would produce a second opinion about which servers should be running, and
// the two would disagree the first night one of them grew a term: `gvm doctor`
// would report a box as healthy while the guard timer started something, or
// the reverse.
//
// So Desired is a FILTER over a report, which also satisfies the contract that
// matters: it cannot actuate, because the only thing it holds that could is
// used exclusively by Recover. TestTheGuardSweepCannotActuateWhileEnumerating
// is that sentence as a test, and it is the same property
// TestReconcilerNeverActuates asserts one layer down.
//
// Recover is the Supervisor's Start, which is internal/lifecycle's manager --
// so a recovery goes through the same precondition table, writes the same
// audit row and clears the same hold as an operator's `gvm start`. A guard
// that started units by a private path would be a guard whose starts were
// invisible in the one place an operator looks.
// Enumerator is the sweep's whole view of the host, and it has one method
// that returns a report.
//
// It exists as an interface for the reason every seam in this package does:
// the never-actuating property has to be checkable on the development
// machine, and a fake returning a crafted SweepReport is how "Desired started
// nothing" becomes a test rather than a claim. *lifecycle.Reconciler is the
// production implementation and the assertion below says so, so the interface
// cannot drift away from the thing it describes.
type Enumerator interface {
	Sweep(ctx context.Context) (lifecycle.SweepReport, error)
}

var _ Enumerator = (*lifecycle.Reconciler)(nil)

type Sweep struct {
	// Rec is the enumeration. Required.
	Rec Enumerator
	// Super is how one instance is started. Required for Recover and unused
	// by Desired.
	Super Supervisor
	// Files reads the degraded breadcrumb, so a recovery's reason can name
	// the job that left the server down -- which is section 13.4's own
	// sentence, "recovered after <unit> ended without restoring service".
	//
	// Optional. Without it, and on the SIGKILL path where no breadcrumb was
	// ever written, the reason falls back to the finding's own detail, which
	// says what was observed rather than who caused it. Both are honest; one
	// is more useful, and neither may be invented.
	Files lifecycle.Files
	// Paths locates the breadcrumb. Nil is instance.PathsFor. It is a seam
	// for the reason every other path seam in gvm is one: /run/gvm/<n> is an
	// absolute Linux path, and a Windows test that let it through would read
	// C:\run.
	Paths func(name string) instance.Paths
	// Host removes a dead holder's lease.json before the start it blocks.
	// Through hostops rather than a bare unlink for the same reason
	// `gvm job clear` goes that way: freeing an install tree for a second
	// writer is an audited act, not a side effect.
	//
	// Optional. Desired never touches it, so `gvm doctor` can hold a Sweep to
	// answer "what would the guard do" with no privileged surface at all --
	// and then Recover refuses a lease-clearing recovery rather than starting
	// into a refusal it cannot lift.
	Host  Host
	Actor audit.Actor
}

// Compile-time proof that this is what the `guard` runner asks for.
var _ Sweeper = (*Sweep)(nil)

// Desired is section 13.4's predicate, read off one sweep. It starts nothing.
func (s *Sweep) Desired(ctx context.Context) ([]Recovery, error) {
	if s == nil || s.Rec == nil {
		return nil, errors.New("jobs: the guard sweep has no reconciler, so it cannot tell " +
			"which instances should be running")
	}
	rep, err := s.Rec.Sweep(ctx)
	if err != nil {
		// An enumeration that failed is not a box with nothing to recover.
		// The runner's step is Fatal for this reason: carrying on with an
		// empty list would report `ok` for a run that looked at nothing.
		return nil, fmt.Errorf("jobs: the guard sweep could not enumerate the instances: %w", err)
	}
	var out []Recovery
	for _, ir := range rep.Instances {
		switch {
		case hasFinding(ir.Findings, lifecycle.FindingNotRunning):
			// The ordinary case: nothing holds this instance and the
			// reconciler already said so.
			out = append(out, Recovery{
				Instance: ir.Name,
				Reason: s.reason(ir.Name,
					findingDetail(ir.Findings, lifecycle.FindingNotRunning)),
			})
		case deadHolder(ir.Observation):
			// A job was SIGKILLed holding this instance, which is the ONLY
			// failure gvm-guard.timer exists for -- section 13.4 introduces
			// it with "a `defer` does not survive `SIGKILL` of the job
			// process (OOM killer, `systemctl kill`, power loss mid-update)".
			//
			// The reconciler suppresses FindingNotRunning for any lease at
			// all, because "a job is working on it" is the better SENTENCE.
			// That is right for a finding and wrong for an action: a lease
			// whose holder is provably gone is proof that nobody is working
			// on it, and leaving a world down because a file exists inverts
			// update.sh's own contract -- "an outdated server beats one left
			// stopped". Both callers share section 13.4's terms through
			// Observation.DesiredRunningAndDown, so the lease is the only
			// term they differ on and there is still one predicate.
			out = append(out, Recovery{
				Instance:        ir.Name,
				Reason:          s.deadHolderReason(ir.Name, ir.Observation),
				ClearStaleLease: true,
			})
		}
	}
	return out, nil
}

// deadHolder is the sweep's own lease clause: section 13.4's terms, plus
// evidence that whoever held this instance is gone.
//
// LeaseUnreadable counts, following Classify's asymmetry in the other
// direction: a lease gvm cannot parse is not evidence that a holder is alive,
// and the removal Recover performs is audited and reversible while an
// overnight outage is not. A LIVE lease never counts -- that is a job
// mid-update, and starting the server underneath it is the harm the lease
// exists to prevent.
func deadHolder(o lifecycle.Observation) bool {
	if !o.DesiredRunningAndDown() {
		return false
	}
	return o.LeaseState == lifecycle.LeaseStale || o.LeaseUnreadable
}

func hasFinding(found []lifecycle.Finding, k lifecycle.FindingKind) bool {
	for _, f := range found {
		if f.Kind == k {
			return true
		}
	}
	return false
}

func findingDetail(found []lifecycle.Finding, k lifecycle.FindingKind) string {
	for _, f := range found {
		if f.Kind == k {
			return f.Detail
		}
	}
	return ""
}

// deadHolderReason names what died, because on the SIGKILL path there is no
// breadcrumb and this sentence is all the operator gets.
//
// It goes in the job_runs row, so somebody who finds a recovered instance can
// go straight to `journalctl -u <the unit named here>` instead of guessing
// which of the night's jobs stopped their server.
func (s *Sweep) deadHolderReason(name string, o lifecycle.Observation) string {
	var b strings.Builder
	b.WriteString("recovered " + name + ": it should be running and is not, and ")
	switch {
	case o.LeaseUnreadable:
		b.WriteString("its job lease cannot be read, so no holder can be shown to be alive")
	case o.Lease == nil:
		b.WriteString("a job lease is present and its holder is gone")
	default:
		b.WriteString("a job died holding it")
		if k := string(o.Lease.Kind); k != "" {
			b.WriteString(" (" + k)
			if o.Lease.PID > 0 {
				b.WriteString(fmt.Sprintf(", pid %d", o.Lease.PID))
			}
			if o.Lease.Unit != "" {
				b.WriteString(", " + o.Lease.Unit)
			}
			if age := o.Lease.Age(o.Now); age > 0 {
				b.WriteString(", held " + age.Round(time.Second).String())
			}
			b.WriteString(")")
		}
	}
	if crumb := s.breadcrumb(name); crumb != "" {
		b.WriteString("; " + crumb)
	}
	return sanitise(b.String())
}

// reason is the sentence recorded on the `guard` run.
//
// The breadcrumb is preferred because it is the only artefact that knows WHICH
// job ended without restoring service: the guard's own Restore wrote it as it
// failed, naming the kind and the slug. The fallback says what was observed
// instead of guessing at a cause -- a server SIGKILLed mid-update left no
// marker at all, and "recovered after gvm-job@pz-main_nightly.service ended"
// would be a sentence gvm invented.
func (s *Sweep) reason(name, detail string) string {
	if crumb := s.breadcrumb(name); crumb != "" {
		return "recovered after " + crumb
	}
	if detail == "" {
		detail = "enabled, not held, and the unit is not running"
	}
	return "recovered an instance that should be running and was not (" + detail + ")"
}

// breadcrumb reads the first line of /run/gvm/<name>/degraded.
//
// One line, because that is the record: FormatDegraded writes
// "<ts> <kind> (<slug>) left this instance stopped" first and the reason chain
// after it, and the reason chain belongs in the journal rather than in a
// job_runs summary field.
func (s *Sweep) breadcrumb(name string) string {
	if s.Files == nil {
		return ""
	}
	p := instance.PathsFor
	if s.Paths != nil {
		p = s.Paths
	}
	b, err := s.Files.ReadFile(p(name).Degraded)
	if err != nil || len(b) == 0 {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return sanitise(line)
}

// Recover starts one instance through internal/lifecycle.
func (s *Sweep) Recover(ctx context.Context, r Recovery) error {
	if s == nil || s.Super == nil {
		return errors.New("jobs: the guard sweep has no supervisor, so it cannot start anything")
	}
	if err := instance.ValidateName(r.Instance); err != nil {
		return err
	}
	if r.ClearStaleLease {
		// Second gate. lifecycle.Allowed refuses a start outright on a stale
		// or unreadable lease -- "a job died holding this instance and the
		// install tree may be half-written" -- and the sweep's supervisor is
		// wired unforced on purpose. Removing the lease lifts exactly that
		// refusal and leaves every other one working, where Force would also
		// paper over an unreadable lease and over any refusal later added to
		// that table.
		//
		// A removal that fails is NOT followed by a start: the start could
		// only be refused, and an `unrecovered` row carrying this error is a
		// DEGRADED guard run an operator can act on, where a silent refusal
		// looks like a guard that did nothing.
		if s.Host == nil {
			return fmt.Errorf("jobs: %s: a dead job's lease is still on disk and this sweep "+
				"has no privileged surface to remove it with, so the start would be refused; "+
				"gvm job clear %s", r.Instance, r.Instance)
		}
		if _, err := s.Host.Do(ctx, hostops.Request{
			Verb: hostops.VerbFileRemove, Actor: s.Actor, Instance: r.Instance,
			File: hostops.FileLease,
		}); err != nil {
			return fmt.Errorf("jobs: %s: could not remove the dead job's lease, so the start "+
				"would be refused: %w", r.Instance, err)
		}
	}
	return s.Super.Start(ctx, r.Instance)
}
