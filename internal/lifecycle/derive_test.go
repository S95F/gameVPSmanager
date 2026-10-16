package lifecycle

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/runner"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// base is a healthy, applied, running instance. Every row below is one
// deliberate change to it, so the table reads as "what makes this go wrong"
// rather than as thirty unrelated structs.
func base() Observation {
	return Observation{
		Name:           "pz-main",
		Now:            at("2026-09-04T19:00:00Z"),
		SpecPresent:    true,
		SpecSHA256:     "9f2c1a4e",
		Game:           "projectzomboid",
		Enabled:        true,
		ManagedInstall: true,
		Pin:            Pin{Present: true, OK: true, Revision: 7, SHA256: "1b40c9de"},
		DropInPresent:  true,
		DropIn:         DropInHeader{Generated: true, SpecSHA256: "9f2c1a4e", Game: "projectzomboid", Revision: 7},
		UnitChecked:    true,
		Unit: platform.UnitState{
			Name: "gvm@pz-main.service", LoadState: "loaded",
			ActiveState: "active", SubState: "running", Result: "success", MainPID: 41799,
		},
		Status: &runner.Status{
			State: runner.StateReady, ReadyAt: at("2026-09-04T18:23:58Z"),
			Players: &runner.PlayersStatus{Online: 3, Max: 8},
			Control: control.Status{Kind: spec.ControlSourceRCON, Ready: true},
		},
		StatusModTime: at("2026-09-04T18:59:58Z"),
		Installed:     Yes,
	}
}

type deriveCase struct {
	name  string
	setup func(*Observation)
	phase Phase
	rule  Rule
	// hint is a substring the hint must contain. It is asserted because the
	// hint is the half an operator acts on, and a rule that fired with the
	// wrong next command is worse than one that fired with none.
	hint string
	// reason is a substring the reason must contain, where the wording is
	// load-bearing.
	reason string
}

func deriveCases() []deriveCase {
	return []deriveCase{
		{
			name:  "no spec at all",
			setup: func(o *Observation) { o.SpecPresent = false },
			phase: PhaseAbsent, rule: RuleNoSpec,
			reason: "/etc/gvm/instances/pz-main.yaml",
		},
		{
			name:  "the spec is there and does not parse",
			setup: func(o *Observation) { o.Game = "" },
			phase: PhaseFailed, rule: RuleSpecUnreadable,
			hint: "gvm apply --diff pz-main",
		},
		{
			// D-17: apt upgrade gvm shipped a manifest with different argv
			// under a live world. The runner exits 64 on this; saying so here
			// is the same refusal one step earlier and with a next move.
			name: "the manifest pin does not match",
			setup: func(o *Observation) {
				o.Pin.OK = false
				o.Pin.Reason = "pinned revision 7 sha256 1b40c9de, got revision 8 sha256 22aa9911"
			},
			phase: PhaseFailed, rule: RulePinMismatch,
			hint:   "not available in this build",
			reason: "revision 8",
		},
		{
			name: "a job died holding the instance",
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobUpdate, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
				o.LeaseState = LeaseStale
			},
			phase: PhaseFailed, rule: RuleLeaseStale,
			hint:   "gvm job clear pz-main",
			reason: "the holder is gone",
		},
		{
			name:  "the lease file cannot be read",
			setup: func(o *Observation) { o.LeaseUnreadable = true },
			phase: PhaseFailed, rule: RuleLeaseUnknown,
			hint: "gvm job clear pz-main",
		},
		{
			name: "an install job holds it",
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobInstall, PID: 900, StartedAt: at("2026-09-04T18:55:00Z")}
				o.LeaseState = LeaseLive
			},
			phase: PhaseInstalling, rule: RuleLeaseInstall,
			reason: "install",
		},
		{
			name: "an update job holds it",
			setup: func(o *Observation) {
				o.Lease = &Lease{
					Kind: JobUpdate, Unit: "gvm-job@pz-main_nightly.service",
					PID: 900, StartedAt: at("2026-09-04T18:55:00Z"), Reason: "steam build 19487321",
				}
				o.LeaseState = LeaseLive
			},
			phase: PhaseUpdating, rule: RuleLeaseUpdate,
			reason: "gvm-job@pz-main_nightly.service",
		},
		{
			// A backup does not stop the server. Reporting a nightly backup
			// as "updating" is how the word stops meaning anything.
			name: "a backup job holds it and the server is still up",
			setup: func(o *Observation) {
				o.Lease = &Lease{Kind: JobBackup, PID: 900, StartedAt: at("2026-09-04T18:59:00Z")}
				o.LeaseState = LeaseLive
			},
			phase: PhaseRunning, rule: RuleReady,
		},
		{
			name:  "content is declared that gvm cannot download",
			setup: func(o *Observation) { o.BlockedMods = true },
			phase: PhaseAwaitingManualFiles, rule: RuleBlockedContent,
			hint: "gvm content blocked pz-main",
		},
		{
			// The definition-of-done row: LoadState=not-found with no
			// applied sha256 is `declared`, not `failed`.
			name: "declared, never applied, and systemd has never heard of the unit",
			setup: func(o *Observation) {
				o.DropInPresent = false
				o.DropIn = DropInHeader{}
				o.Pin = Pin{}
				o.Unit.LoadState = "not-found"
				o.Unit.ActiveState = "inactive"
				o.Status = nil
			},
			phase: PhaseDeclared, rule: RuleNeverApplied,
			hint: "gvm apply pz-main",
		},
		{
			name: "applied, and the pinned manifest is gone",
			setup: func(o *Observation) {
				o.Pin = Pin{}
			},
			phase: PhaseFailed, rule: RulePinMissing,
			hint:   "gvm apply pz-main",
			reason: "manifest.lock.yaml",
		},
		{
			name: "a drop-in gvm did not write",
			setup: func(o *Observation) {
				o.DropIn = DropInHeader{Generated: false}
			},
			phase: PhaseFailed, rule: RuleForeignDropIn,
			hint: "90-local.conf",
		},
		{
			// Not "stopped". The manager did not answer and every statement
			// about running would be an invention.
			name:  "the service manager did not answer",
			setup: func(o *Observation) { o.UnitChecked = false },
			phase: PhaseDegraded, rule: RuleUnitUnknown,
			reason: "unit state is unknown",
		},
		{
			name:  "the unit is masked",
			setup: func(o *Observation) { o.Unit.LoadState = "masked" },
			phase: PhaseFailed, rule: RuleUnitMasked,
			hint: "systemctl unmask gvm@pz-main.service",
		},
		{
			name: "applied, and systemd does not know the unit",
			setup: func(o *Observation) {
				o.Unit.LoadState = "not-found"
				o.Unit.ActiveState = "inactive"
			},
			phase: PhaseFailed, rule: RuleUnitNotFound,
			hint: "gvm apply pz-main",
		},
		{
			name: "activating",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "activating"
				o.Unit.SubState = "start"
				o.Status = &runner.Status{
					State: runner.StateBooting,
					Readiness: runner.ReadinessStatus{
						Probe: "log_marker", LastProgress: at("2026-09-04T18:59:57Z"),
					},
				}
			},
			phase: PhaseStarting, rule: RuleActivating,
			reason: "log_marker",
		},
		{
			name:  "deactivating",
			setup: func(o *Observation) { o.Unit.ActiveState = "deactivating" },
			phase: PhaseStopping, rule: RuleDeactivating,
		},
		{
			name: "the unit failed on the sandbox exit code",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "failed"
				o.Unit.Result = "exit-code"
				o.Unit.ExecMainStatus = runner.ExitConfig
				o.Status = nil
			},
			phase: PhaseFailed, rule: RuleUnitFailed,
			hint:   "gvm apply --diff pz-main",
			reason: "the sandbox blocks a required write",
		},
		{
			// The definition-of-done row: active with no status.json is
			// degraded. systemd says the unit is up; the runner has not said
			// anything at all.
			name:  "active with no status.json",
			setup: func(o *Observation) { o.Status = nil },
			phase: PhaseDegraded, rule: RuleNoStatus,
			reason: "/run/gvm/pz-main/status.json",
		},
		{
			name: "active with a status.json nobody has rewritten",
			setup: func(o *Observation) {
				o.StatusModTime = o.Now.Add(-5 * time.Minute)
			},
			phase: PhaseDegraded, rule: RuleStatusStale,
			reason: "5m0s ago",
		},
		{
			name: "the shutdown ladder is running",
			setup: func(o *Observation) {
				o.Status.Shutdown = runner.ShutdownStatus{InProgress: true, Rung: "console"}
			},
			phase: PhaseStopping, rule: RuleShuttingDown,
			reason: "rung console",
		},
		{
			name:  "booting",
			setup: func(o *Observation) { o.Status.State = runner.StateBooting },
			phase: PhaseStarting, rule: RuleBooting,
		},
		{
			name:  "the unit is active and the runner says failed",
			setup: func(o *Observation) { o.Status.State = runner.StateFailed },
			phase: PhaseDegraded, rule: RuleRunnerFailed,
		},
		{
			name:  "the unit is active and the runner says exited",
			setup: func(o *Observation) { o.Status.State = runner.StateExited },
			phase: PhaseDegraded, rule: RuleRunnerExited,
		},
		{
			name:  "ready",
			setup: func(o *Observation) {},
			phase: PhaseRunning, rule: RuleReady,
			reason: "3 players of 8",
		},
		{
			name: "applied and the install sentinel is not there",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
				o.Installed = No
			},
			phase: PhaseProvisioning, rule: RuleNotInstalled,
			hint: "not available in this build",
		},
		{
			name: "an operator stopped it",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
				o.Hold = true
				o.HoldReason = "2026-09-04T19:00:00Z root_cli disk swap"
			},
			phase: PhaseStopped, rule: RuleHeld,
			reason: "disk swap",
		},
		{
			name: "the unit stopped on a timeout",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Unit.Result = "timeout"
				o.Status = nil
			},
			phase: PhaseFailed, rule: RuleUncleanExit,
			reason: "result=timeout",
		},
		{
			name: "disabled",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Enabled = false
				o.Status = nil
			},
			phase: PhaseStopped, rule: RuleDisabled,
			hint: "sudo gvm apply pz-main",
		},
		{
			name: "inactive, enabled and nothing holding it",
			setup: func(o *Observation) {
				o.Unit.ActiveState = "inactive"
				o.Status = nil
			},
			phase: PhaseStopped, rule: RuleInactive,
			hint: "gvm start pz-main",
		},
		{
			name:  "an active state gvm does not model",
			setup: func(o *Observation) { o.Unit.ActiveState = "refreshing" },
			phase: PhaseDegraded, rule: RuleUnmodelled,
			reason: `"refreshing"`,
		},
	}
}

func TestDeriveIsThePhaseTable(t *testing.T) {
	for _, tc := range deriveCases() {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.setup(&o)
			got := Derive(o)
			if got.Phase != tc.phase || got.Rule != tc.rule {
				t.Fatalf("Derive = %s/%s, want %s/%s\n  reason: %s",
					got.Phase, got.Rule, tc.phase, tc.rule, got.Reason)
			}
			if tc.hint != "" && !strings.Contains(got.Hint, tc.hint) {
				t.Errorf("hint %q does not contain %q", got.Hint, tc.hint)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not contain %q", got.Reason, tc.reason)
			}
			if !got.Phase.Valid() {
				t.Errorf("%s is not a declared phase", got.Phase)
			}
		})
	}
}

// TestEveryRuleIsReachable is the closure property, and it catches the failure
// a per-row test cannot: a rule that stopped firing because a row above it
// started shadowing it. The rule is still declared, its message is still in the
// binary, and no operator will ever see it again.
func TestEveryRuleIsReachable(t *testing.T) {
	fired := map[Rule]bool{}
	for _, tc := range deriveCases() {
		o := base()
		tc.setup(&o)
		fired[Derive(o).Rule] = true
	}
	for _, r := range Rules() {
		if !fired[r] {
			t.Errorf("rule %q is declared and no Observation in the table produces it", r)
		}
	}
	for r := range fired {
		if !slices.Contains(Rules(), r) {
			t.Errorf("Derive returned rule %q, which is not declared in rules", r)
		}
	}
}

// TestEveryPhaseIsProducible is the same closure from the other side. A phase
// in the schema enum that nothing can produce is either a phase gvm forgot to
// derive or a column value the database will never see.
func TestEveryPhaseIsProducible(t *testing.T) {
	seen := map[Phase]bool{}
	for _, tc := range deriveCases() {
		o := base()
		tc.setup(&o)
		seen[Derive(o).Phase] = true
	}
	for _, p := range Phases() {
		if !seen[p] {
			t.Errorf("phase %q is declared and nothing in the table produces it", p)
		}
	}
}

// TestDeriveIsPure is the property the whole package is arranged around.
//
// Two calls on the same value give the same answer, and the value is not
// touched. If Derive ever grew a clock, a filesystem read or a cache, one of
// these two assertions breaks.
func TestDeriveIsPure(t *testing.T) {
	for _, tc := range deriveCases() {
		o := base()
		tc.setup(&o)
		before := deepCopy(t, o)

		first := Derive(o)
		second := Derive(o)
		if first != second {
			t.Fatalf("%s: Derive is not deterministic: %+v then %+v", tc.name, first, second)
		}
		if !reflect.DeepEqual(before, deepCopy(t, o)) {
			t.Fatalf("%s: Derive mutated its argument", tc.name)
		}
	}
}

func deepCopy(t *testing.T, o Observation) Observation {
	t.Helper()
	out := o
	if o.Status != nil {
		st := *o.Status
		if o.Status.Players != nil {
			p := *o.Status.Players
			st.Players = &p
		}
		out.Status = &st
	}
	if o.Lease != nil {
		l := *o.Lease
		out.Lease = &l
	}
	out.Problems = append([]Problem(nil), o.Problems...)
	return out
}

// TestAStaleLeaseNeverResumesAndNeverClearsItself is the rule spelled out.
//
// Every kind of job, stale, must land on `failed` and must hint at the command
// a PERSON runs. Auto-resuming would start a second writer against an install
// tree the first may still have open; auto-clearing would hide that it ever
// happened. Neither is a decision software gets to make.
func TestAStaleLeaseNeverResumesAndNeverClearsItself(t *testing.T) {
	for _, k := range JobKinds() {
		o := base()
		o.Lease = &Lease{Kind: k, PID: 900, StartedAt: at("2026-09-04T18:00:00Z")}
		o.LeaseState = LeaseStale
		v := Derive(o)
		if v.Phase != PhaseFailed {
			t.Errorf("a stale %s lease derived %s; it must be failed and wait for a person",
				k, v.Phase)
		}
		if v.Rule != RuleLeaseStale {
			t.Errorf("a stale %s lease fired rule %s, want %s", k, v.Rule, RuleLeaseStale)
		}
		if !strings.Contains(v.Hint, "gvm job clear") {
			t.Errorf("a stale %s lease hints %q, which does not name the command that clears it",
				k, v.Hint)
		}
	}
}

// TestALiveLeaseOutranksSystemd guards the ordering.
//
// systemd will happily report a unit as active while a rebuild is halfway
// through replacing the world under it. The lease is the more specific truth
// and it has to win, or `gvm status` says "running, 0 players" during a
// restore.
func TestALiveLeaseOutranksSystemd(t *testing.T) {
	for _, k := range JobKinds() {
		want, masks := k.Phase()
		o := base()
		o.Lease = &Lease{Kind: k, PID: 900, StartedAt: at("2026-09-04T18:55:00Z")}
		o.LeaseState = LeaseLive
		got := Derive(o)
		if masks && got.Phase != want {
			t.Errorf("a live %s lease over an active unit derived %s, want %s", k, got.Phase, want)
		}
		if !masks && got.Phase != PhaseRunning {
			t.Errorf("a live %s lease does not stop the server, so the phase should stay running; got %s",
				k, got.Phase)
		}
	}
}

// TestTheExitCodeTableHasAHintForEveryRestartPreventedCode.
//
// Section 7.3 lists 64, 65, 66 and 78 in RestartPreventExitStatus. Those are
// exactly the codes where systemd will NOT retry, so the unit sits there until
// somebody does something -- which makes them exactly the codes where a hint
// is worth having, and where a generic "check the logs" wastes the operator's
// next five minutes.
func TestTheExitCodeTableHasAHintForEveryRestartPreventedCode(t *testing.T) {
	want := map[int]string{
		runner.ExitUsage:    "gvm apply --diff pz-main",
		runner.ExitDataErr:  "gvm doctor pz-main",
		exitPortUnavailable: "gvm ports pz-main",
		runner.ExitConfig:   "gvm apply --diff pz-main",
		runner.ExitBootLoop: "gvm logs pz-main --shutdown",
	}
	for code, hint := range want {
		o := base()
		o.Unit.ActiveState = "failed"
		o.Unit.Result = "exit-code"
		o.Unit.ExecMainStatus = code
		o.Status = nil
		v := Derive(o)
		if v.Hint != hint {
			t.Errorf("exit %d hints %q, want %q", code, v.Hint, hint)
		}
		if m := exitMeaning(code); m == "unclassified" {
			t.Errorf("exit %d has no sentence in the section 7.8 table", code)
		}
		if !strings.Contains(v.Reason, exitMeaning(code)) {
			t.Errorf("exit %d reason %q does not explain the code", code, v.Reason)
		}
	}
}

// TestExitMeaningClassifiesEveryRoleCode: every exit code in the section 7.8
// contract has a sentence, and the boot codes 69/79 are present (Phase 27).
func TestExitMeaningClassifiesEveryRoleCode(t *testing.T) {
	for _, code := range []int{
		runner.ExitUsage, runner.ExitDataErr, exitPortUnavailable,
		runner.ExitBootDied, runner.ExitSoftware, runner.ExitTempFail,
		runner.ExitConfig, runner.ExitBootLoop,
	} {
		if m := exitMeaning(code); m == "unclassified" {
			t.Errorf("exit %d has no sentence in the section 7.8 table", code)
		}
	}
	// 75's sentence now admits the readiness-failure cause, not only the orphan.
	if m := exitMeaning(runner.ExitTempFail); !strings.Contains(m, "readiness") {
		t.Errorf("exit 75 meaning %q should mention readiness failure", m)
	}
}

// TestAZeroObservationDoesNotPanic. Derive is called on whatever the observer
// produced, including on a host where nothing could be read at all, and a
// phase table that panicked there would take `gvm status` down with it.
func TestAZeroObservationDoesNotPanic(t *testing.T) {
	v := Derive(Observation{Name: "pz-main"})
	if v.Phase != PhaseAbsent {
		t.Fatalf("the zero observation derived %s, want %s", v.Phase, PhaseAbsent)
	}
}

// TestTheStatusFreshnessRuleNeedsAClock. A caller that supplied no clock must
// not have a phase derived from a comparison against the zero time, which
// would make every instance permanently degraded.
func TestTheStatusFreshnessRuleNeedsAClock(t *testing.T) {
	o := base()
	o.Now = time.Time{}
	if v := Derive(o); v.Rule != RuleReady {
		t.Fatalf("with no clock the freshness rule fired anyway: %s/%s", v.Phase, v.Rule)
	}
	o = base()
	o.StatusModTime = time.Time{}
	if v := Derive(o); v.Rule != RuleReady {
		t.Fatalf("with no status mtime the freshness rule fired anyway: %s/%s", v.Phase, v.Rule)
	}
}
