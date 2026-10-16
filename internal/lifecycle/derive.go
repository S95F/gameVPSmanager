package lifecycle

import (
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/runner"
)

// Rule names the row of the phase table that fired.
//
// It is returned alongside the Phase because two rules that both answer
// "failed" are two different bugs with two different fixes, and a test that
// asserted only the phase would pass while the table decided for the wrong
// reason. It is also what a support thread quotes: `gvm status --json` prints
// it, so "phase failed, rule lease-stale" is one grep away from the code that
// produced it.
type Rule string

const (
	RuleNoSpec         Rule = "no-spec"
	RulePinMismatch    Rule = "pin-mismatch"
	RuleSpecUnreadable Rule = "spec-unreadable"
	RuleLeaseStale     Rule = "lease-stale"
	RuleLeaseUnknown   Rule = "lease-unreadable"
	RuleLeaseInstall   Rule = "lease-install"
	RuleLeaseUpdate    Rule = "lease-update"
	RuleBlockedContent Rule = "blocked-content"
	RuleNeverApplied   Rule = "never-applied"
	RulePinMissing     Rule = "pin-missing"
	RuleForeignDropIn  Rule = "foreign-dropin"
	RuleUnitUnknown    Rule = "unit-unknown"
	RuleUnitMasked     Rule = "unit-masked"
	RuleUnitNotFound   Rule = "unit-not-found"
	RuleActivating     Rule = "activating"
	RuleDeactivating   Rule = "deactivating"
	RuleUnitFailed     Rule = "unit-failed"
	RuleNoStatus       Rule = "no-status"
	RuleStatusStale    Rule = "status-stale"
	RuleShuttingDown   Rule = "shutting-down"
	RuleBooting        Rule = "booting"
	RuleRunnerFailed   Rule = "runner-failed"
	RuleRunnerExited   Rule = "runner-exited"
	RuleReady          Rule = "ready"
	RuleNotInstalled   Rule = "not-installed"
	RuleHeld           Rule = "held"
	RuleUncleanExit    Rule = "unclean-exit"
	RuleDisabled       Rule = "disabled"
	RuleInactive       Rule = "inactive"
	RuleUnmodelled     Rule = "unmodelled-active-state"
)

// rules is every rule this table can produce. TestEveryRuleIsReachable walks
// it against what Derive actually returns, so a rule that stopped firing --
// because a row above it started shadowing it -- is caught here rather than by
// an operator who never sees the message again.
var rules = []Rule{
	RuleNoSpec, RulePinMismatch, RuleSpecUnreadable, RuleLeaseStale, RuleLeaseUnknown,
	RuleLeaseInstall, RuleLeaseUpdate, RuleBlockedContent, RuleNeverApplied,
	RulePinMissing, RuleForeignDropIn, RuleUnitUnknown, RuleUnitMasked, RuleUnitNotFound,
	RuleActivating, RuleDeactivating, RuleUnitFailed, RuleNoStatus, RuleStatusStale,
	RuleShuttingDown, RuleBooting, RuleRunnerFailed, RuleRunnerExited, RuleReady,
	RuleNotInstalled, RuleHeld, RuleUncleanExit, RuleDisabled, RuleInactive,
	RuleUnmodelled,
}

// Rules returns every declared rule.
func Rules() []Rule { return append([]Rule(nil), rules...) }

// Verdict is the phase table's answer.
//
// Reason states what was OBSERVED, in the past tense, naming the file or the
// unit field it came from. Hint is the command that changes it, or empty. The
// split exists because an operator reads them differently: the reason is
// evidence they can check, the hint is an action they can take, and a message
// that mixed the two ends up being neither.
type Verdict struct {
	Phase  Phase  `json:"phase"`
	Rule   Rule   `json:"rule"`
	Reason string `json:"reason"`
	Hint   string `json:"hint,omitempty"`
}

func (v Verdict) String() string {
	s := string(v.Phase)
	if v.Reason != "" {
		s += ": " + v.Reason
	}
	return s
}

// Derive is THE phase table.
//
// It is a pure function: no clock, no filesystem, no systemd, no database, no
// package-level state. Every input is a field of the Observation and every
// output is the Verdict. That is what makes derive_test.go able to enumerate
// the shapes -- including the ones a real host reaches once a year, like a
// lease left by a job the OOM killer took during a reboot.
//
// The rows are ORDERED and the order is the design. Reading down, each row
// answers a question that makes the rows below it meaningless if it fires:
//
//  1. Does gvm know what this instance is? No spec, an unreadable spec, or a
//     manifest pin that does not match are all "gvm cannot say what this
//     server IS", and every statement further down would be about the wrong
//     thing.
//  2. Is somebody else holding it? A live lease means a job owns the install
//     tree right now; a stale one means a job died owning it. Neither is a
//     state systemd can report and both outrank it.
//  3. Has anything been applied? An unapplied spec has no unit to ask about.
//  4. What does systemd say?
//  5. What does the runner say inside that?
//
// Two rules deserve their own note.
//
// A STALE lease is `failed` and gvm NEVER auto-resumes from it. Resuming a job
// whose owner died means a second process starts writing an install tree the
// first may still have open, and the first may not be as dead as the pid table
// suggests. The fix is a person: `gvm job clear` says what the lease was, how
// old it is and what it was doing, and the operator decides.
//
// `active` with no status.json is `degraded` rather than `running`. systemd
// says the unit is up; the unit's main process is the runner, and a runner
// that has not written status.json is a runner that either has not started
// yet or has stopped doing its job. Calling that "running" would put a green
// tick next to a server nobody can see into.
func Derive(o Observation) Verdict {
	if v, ok := deriveIdentity(o); ok {
		return v
	}
	if v, ok := deriveLease(o); ok {
		return v
	}
	if v, ok := deriveApplied(o); ok {
		return v
	}
	return deriveUnit(o)
}

// deriveIdentity answers "does gvm know what this is".
func deriveIdentity(o Observation) (Verdict, bool) {
	if !o.SpecPresent {
		return Verdict{
			Phase: PhaseAbsent, Rule: RuleNoSpec,
			Reason: "no " + specPath(o.Name),
		}, true
	}
	// A spec that is present and did not parse. The observer records the
	// diagnostic; here it is only visible as "present but nothing was read
	// out of it", which is why Game is the field checked -- every valid
	// instance has one and no valid one has it empty.
	if o.Game == "" {
		return Verdict{
			Phase: PhaseFailed, Rule: RuleSpecUnreadable,
			Reason: specPath(o.Name) + " does not parse",
			Hint:   "gvm apply --diff " + o.Name,
		}, true
	}
	if o.Pin.Present && !o.Pin.OK {
		return Verdict{
			Phase: PhaseFailed, Rule: RulePinMismatch,
			Reason: "the pinned manifest does not match what the instance records: " + o.Pin.Reason,
			// Never "re-create the instance", and never a command that moves the
			// pin: if the YAML pin was hand-edited, restoring it is the fix, and
			// moving a live world to a newer manifest revision (which can ship
			// new argv and destroy globs, D-17) is not available in this build.
			Hint: "if the pin was edited by hand, restore it, then: sudo gvm apply " + o.Name +
				" --diff. Moving an instance to a newer manifest revision is not available in this build",
		}, true
	}
	return Verdict{}, false
}

// deriveLease answers "is somebody else holding it".
func deriveLease(o Observation) (Verdict, bool) {
	if o.LeaseUnreadable {
		return Verdict{
			Phase: PhaseFailed, Rule: RuleLeaseUnknown,
			Reason: LeasePath(o.Name) + " exists and cannot be read, so gvm cannot tell " +
				"whether a job is running",
			Hint: "gvm job clear " + o.Name,
		}, true
	}
	switch o.LeaseState {
	case LeaseStale:
		return Verdict{
			Phase: PhaseFailed, Rule: RuleLeaseStale,
			Reason: "a " + string(o.Lease.Kind) + " job took this instance " +
				ago(o.Now, o.Lease.StartedAt) + " and did not release it; the holder is gone",
			Hint: "gvm job clear " + o.Name,
		}, true
	case LeaseLive:
		ph, masks := o.Lease.Kind.Phase()
		if !masks {
			// A backup, prune or verify does not stop the server, so the
			// phase stays whatever systemd and the runner say. The lease is
			// still reported (Status carries it) -- "why is it busy" has an
			// answer even when the phase does not change.
			return Verdict{}, false
		}
		rule := RuleLeaseUpdate
		if ph == PhaseInstalling {
			rule = RuleLeaseInstall
		}
		return Verdict{
			Phase: ph, Rule: rule,
			Reason: o.Lease.Describe(o.Now),
		}, true
	}
	return Verdict{}, false
}

// deriveApplied answers "has anything been written to the host for this".
func deriveApplied(o Observation) (Verdict, bool) {
	if o.BlockedMods {
		// Deliberately above the unit rules: the server may well be running,
		// but the content intent cannot be satisfied without a human and
		// reporting `running` would bury that until the next sync fails.
		return Verdict{
			Phase: PhaseAwaitingManualFiles, Rule: RuleBlockedContent,
			Reason: "content is declared that gvm cannot download; the files must be fetched by hand",
			Hint:   "gvm content blocked " + o.Name,
		}, true
	}
	if o.DropInPresent && !o.DropIn.Generated {
		return Verdict{
			Phase: PhaseFailed, Rule: RuleForeignDropIn,
			Reason: dropInPath(o.Name) + " was not written by gvm; it is not being overwritten",
			// 90-local.conf is the supported place for an operator's own
			// directives, and section 5.3 promises gvm never touches it.
			Hint: "move your directives into 90-local.conf, then: gvm apply " + o.Name,
		}, true
	}
	if o.DropIn.SpecSHA256 == "" {
		return Verdict{
			Phase: PhaseDeclared, Rule: RuleNeverApplied,
			Reason: "the spec exists and no apply has converged it",
			Hint:   "gvm apply " + o.Name,
		}, true
	}
	if !o.Pin.Present {
		// Applied, so a manifest was pinned at the time; the pin file is gone
		// now. The runner refuses to start on that and exits 64, so saying so
		// here is strictly earlier than finding out from a restart loop.
		return Verdict{
			Phase: PhaseFailed, Rule: RulePinMissing,
			Reason: "applied, but " + manifestLockPath(o.Name) + " is missing",
			Hint:   "gvm apply " + o.Name,
		}, true
	}
	return Verdict{}, false
}

// deriveUnit answers "what is it doing", from systemd and then from the runner.
func deriveUnit(o Observation) Verdict {
	if !o.UnitChecked {
		// Not "stopped". The service manager did not answer, and every
		// statement about running or not running below this line would be an
		// invention. Degraded is the phase for "it may well be up and gvm
		// cannot account for it", which is exactly this.
		return Verdict{
			Phase: PhaseDegraded, Rule: RuleUnitUnknown,
			Reason: "the service manager did not answer; the unit state is unknown",
			Hint:   "systemctl status " + unitName(o.Name),
		}
	}
	switch o.Unit.LoadState {
	case "masked":
		return Verdict{
			Phase: PhaseFailed, Rule: RuleUnitMasked,
			Reason: unitName(o.Name) + " is masked; systemd will refuse every start",
			Hint:   "systemctl unmask " + unitName(o.Name),
		}
	case "not-found", "error", "bad-setting":
		return Verdict{
			Phase: PhaseFailed, Rule: RuleUnitNotFound,
			Reason: "systemd reports " + unitName(o.Name) + " as " + o.Unit.LoadState +
				", but this instance has been applied",
			Hint: "gvm apply " + o.Name,
		}
	}

	switch o.Unit.ActiveState {
	case "activating":
		return Verdict{
			Phase: PhaseStarting, Rule: RuleActivating,
			Reason: startingReason(o),
		}
	case "deactivating":
		return Verdict{
			Phase: PhaseStopping, Rule: RuleDeactivating,
			Reason: "the shutdown ladder is running" + rungSuffix(o),
		}
	case "failed":
		return unitFailed(o)
	case "active", "reloading":
		return deriveRunning(o)
	case "inactive":
		return deriveInactive(o)
	}
	return Verdict{
		Phase: PhaseDegraded, Rule: RuleUnmodelled,
		Reason: fmt.Sprintf("systemd reports an active state gvm does not model: %q", o.Unit.ActiveState),
		Hint:   "systemctl status " + unitName(o.Name),
	}
}

// deriveRunning is the unit being up, cross-examined against the runner.
func deriveRunning(o Observation) Verdict {
	if o.Status == nil {
		return Verdict{
			Phase: PhaseDegraded, Rule: RuleNoStatus,
			Reason: unitName(o.Name) + " is active and the runner has written no " +
				statusPath(o.Name),
			Hint: "gvm logs " + o.Name,
		}
	}
	// A status file that stopped being rewritten is worse than none: it says
	// "ready" forever. The runner rewrites it every 2 s, so this only fires
	// on a runner that is alive and no longer doing its job.
	if !o.Now.IsZero() && !o.StatusModTime.IsZero() &&
		o.Now.Sub(o.StatusModTime) > StatusStaleAfter {
		return Verdict{
			Phase: PhaseDegraded, Rule: RuleStatusStale,
			Reason: statusPath(o.Name) + " was last written " + ago(o.Now, o.StatusModTime) +
				"; the runner rewrites it every " + runner.StatusInterval.String(),
			Hint: "gvm logs " + o.Name,
		}
	}

	st := o.Status
	if st.Shutdown.InProgress || st.State == runner.StateStopping {
		return Verdict{
			Phase: PhaseStopping, Rule: RuleShuttingDown,
			Reason: "the shutdown ladder is running" + rungSuffix(o),
		}
	}
	switch st.State {
	case runner.StatePreflight, runner.StateSpawning, runner.StateBooting:
		return Verdict{
			Phase: PhaseStarting, Rule: RuleBooting,
			Reason: startingReason(o),
		}
	case runner.StateFailed:
		return Verdict{
			Phase: PhaseDegraded, Rule: RuleRunnerFailed,
			Reason: "the unit is active and the runner reports state=failed",
			Hint:   "gvm logs " + o.Name,
		}
	case runner.StateExited:
		// systemd has not caught up with a process that is already gone. It
		// resolves itself in seconds; reporting it as running would not.
		return Verdict{
			Phase: PhaseDegraded, Rule: RuleRunnerExited,
			Reason: "the unit is active and the runner reports state=exited",
			Hint:   "gvm logs " + o.Name,
		}
	}
	return Verdict{
		Phase: PhaseRunning, Rule: RuleReady,
		Reason: readyReason(o),
	}
}

// deriveInactive is the unit being down, and why that is or is not expected.
func deriveInactive(o Observation) Verdict {
	// Ordered before the hold: an instance with no game files has a more
	// useful thing to be told than "somebody stopped it".
	if o.Installed == No {
		return Verdict{
			Phase: PhaseProvisioning, Rule: RuleNotInstalled,
			Reason: "applied, and the provider's install sentinel is not on disk",
			// Not `gvm install`: installing game files is not available in this
			// build, so pointing back at it would send the operator to a refusal.
			Hint: "game files are not installed, and installing them is not available in this build",
		}
	}
	if o.Hold {
		return Verdict{
			Phase: PhaseStopped, Rule: RuleHeld,
			Reason: holdReason(o),
			// The hold is why the guard timer is leaving it alone. A reboot
			// clears it; the durable stop is enabled: false, not a start.
			Hint: "a reboot clears it; for a durable stop set enabled: false in " +
				specPath(o.Name) + " and run sudo gvm apply " + o.Name,
		}
	}
	// systemd keeps Result from the last run. "success" is a clean exit; a
	// timeout or an oom-kill means the shutdown ladder did not finish and the
	// world may not have been saved.
	if r := o.Unit.Result; r != "" && r != "success" {
		return Verdict{
			Phase: PhaseFailed, Rule: RuleUncleanExit,
			Reason: "the unit stopped with result=" + r + exitSuffix(o),
			Hint:   exitHint(o),
		}
	}
	if !o.Enabled {
		return Verdict{
			Phase: PhaseStopped, Rule: RuleDisabled,
			Reason: "enabled: false in " + specPath(o.Name),
			Hint:   "set enabled: true in " + specPath(o.Name) + ", then: sudo gvm apply " + o.Name,
		}
	}
	return Verdict{
		Phase: PhaseStopped, Rule: RuleInactive,
		Reason: "the unit is inactive and nothing is holding it",
		Hint:   "gvm start " + o.Name,
	}
}

// unitFailed turns systemd's failure into the exit-code table's sentence.
func unitFailed(o Observation) Verdict {
	return Verdict{
		Phase: PhaseFailed, Rule: RuleUnitFailed,
		Reason: "systemd reports " + unitName(o.Name) + " failed" + resultSuffix(o) + exitSuffix(o),
		Hint:   exitHint(o),
	}
}

// exitHint maps section 7.8's exit codes to the command that addresses them.
//
// The four codes RestartPreventExitStatus lists are the ones where a restart
// cannot help, so they are the ones where a hint is worth having: systemd will
// not retry and the unit sits there until a person does something.
func exitHint(o Observation) string {
	switch o.Unit.ExecMainStatus {
	case runner.ExitUsage:
		// 64: the spec is invalid or the manifest pin does not match.
		return "gvm apply --diff " + o.Name
	case runner.ExitDataErr:
		// 65: preflight found something absent -- a directory, a credential,
		// the executable.
		return "gvm doctor " + o.Name
	case exitPortUnavailable:
		return "gvm ports " + o.Name
	case runner.ExitConfig:
		// 78: the sandbox blocks a write the game needs. `apply` re-runs the
		// write-probe, which is what names the path (D-15).
		return "gvm apply --diff " + o.Name
	case runner.ExitTempFail:
		// 75: an orphan from the previous run, or a readiness failure. It
		// clears itself, so the hint is to look rather than to act.
		return "gvm status " + o.Name
	case runner.ExitBootLoop:
		// 79: gave up after three failed boots. The shutdown record of the last
		// attempt holds the STATUS line that says why.
		return "gvm logs " + o.Name + " --shutdown"
	}
	return "gvm logs " + o.Name
}

// exitPortUnavailable is section 7.8's code 66. It has no name in
// internal/runner because the runner never emits it -- the GAME fails to bind
// and the runner reports its exit -- but the phase table still has to
// recognise it, and a bare 66 at the call site would be a number nobody could
// look up.
const exitPortUnavailable = 66

func startingReason(o Observation) string {
	if o.Status != nil && o.Status.Readiness.Probe != "" {
		s := "waiting for readiness probe " + o.Status.Readiness.Probe
		if !o.Status.Readiness.LastProgress.IsZero() && !o.Now.IsZero() {
			s += ", last output " + ago(o.Now, o.Status.Readiness.LastProgress)
		}
		return s
	}
	return "the unit is starting"
}

func readyReason(o Observation) string {
	var b strings.Builder
	b.WriteString("ready")
	if o.Status != nil {
		if !o.Status.ReadyAt.IsZero() && !o.Now.IsZero() {
			b.WriteString(" since " + ago(o.Now, o.Status.ReadyAt))
		}
		if p := o.Status.Players; p != nil {
			b.WriteString(fmt.Sprintf(", %d player", p.Online))
			if p.Online != 1 {
				b.WriteString("s")
			}
			if p.Max > 0 {
				b.WriteString(fmt.Sprintf(" of %d", p.Max))
			}
		}
	}
	return b.String()
}

func rungSuffix(o Observation) string {
	if o.Status != nil && o.Status.Shutdown.Rung != "" {
		return " (rung " + string(o.Status.Shutdown.Rung) + ")"
	}
	return ""
}

func resultSuffix(o Observation) string {
	if o.Unit.Result != "" && o.Unit.Result != "success" {
		return " with result=" + o.Unit.Result
	}
	return ""
}

func exitSuffix(o Observation) string {
	if o.Unit.ExecMainStatus == 0 {
		return ""
	}
	return fmt.Sprintf(" (exit %d: %s)", o.Unit.ExecMainStatus, exitMeaning(o.Unit.ExecMainStatus))
}

// exitMeaning is section 7.8's table.
func exitMeaning(code int) string {
	switch code {
	case runner.ExitUsage:
		return "spec invalid, or manifest pin mismatch"
	case runner.ExitDataErr:
		return "preflight failed"
	case exitPortUnavailable:
		return "port unavailable at bind time"
	case runner.ExitBootDied:
		return "the game exited before it was ready"
	case runner.ExitSoftware:
		return "the runner panicked"
	case runner.ExitTempFail:
		return "readiness failed (timeout, stall or marker), or an orphan from a previous run"
	case runner.ExitConfig:
		return "the sandbox blocks a required write"
	case runner.ExitBootLoop:
		return "the game failed to reach ready 3 times in a row; gvm stopped retrying"
	}
	return "unclassified"
}

// RestartPreventExitStatus is the exit codes where a restart cannot help, so
// systemd must not retry — the Go twin of gvm@.service's RestartPreventExitStatus=
// directive. A packaging test asserts the two agree. Phase 28's NeedsOperator
// and the role-exit test read it, so the set lives in one place.
//
// 69 (boot death) is NOT here: a single transient death should retry. 79 (boot
// loop) IS: three failures in a row need a human.
var RestartPreventExitStatus = []int{
	runner.ExitUsage,    // 64
	runner.ExitDataErr,  // 65
	exitPortUnavailable, // 66
	runner.ExitConfig,   // 78
	runner.ExitBootLoop, // 79
}

func holdReason(o Observation) string {
	s := "held: an operator stopped it"
	if o.HoldReason != "" {
		s += " (" + o.HoldReason + ")"
	}
	// The lifetime is worth stating: /run is tmpfs, so this is not a durable
	// stop and a reboot brings the instance back, exactly as systemd's own
	// stopped-but-enabled semantics do.
	return s + "; a reboot clears it, `gvm disable` does not"
}

// ago renders an interval the way an operator reads one. A zero or future
// timestamp produces "just now" rather than a negative duration, because a
// clock skew on a VPS is common and "-3m0s ago" reads as a bug in gvm.
func ago(now, then time.Time) string {
	if now.IsZero() || then.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(then)
	if d < time.Second {
		return "just now"
	}
	return d.Round(time.Second).String() + " ago"
}

// The paths and the unit name come from internal/instance, which is where
// section 5 is transcribed. Naming them again here as literals is how a
// directory ends up spelled two ways, and a phase table that told an operator
// to look at a file gvm does not write would be worse than one that said
// nothing.
func specPath(name string) string         { return instance.PathsFor(name).Spec }
func manifestLockPath(name string) string { return instance.PathsFor(name).ManifestLock }
func dropInPath(name string) string       { return instance.DropInPath(name) }
func statusPath(name string) string       { return instance.PathsFor(name).StatusJSON }
func unitName(name string) string         { return instance.UnitName(name) }
