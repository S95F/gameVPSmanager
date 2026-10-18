package packaging

import (
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/jobs"
)

// unit is one parsed unit file: section -> directive -> values, in order.
//
// A list of values per key rather than one, because systemd genuinely allows
// repeats and treats some of them additively (ExecStart in a oneshot,
// RestrictAddressFamilies) -- and a parser that kept only the last value would
// quietly agree with a mistake.
type unit map[string]map[string][]string

func (u unit) first(section, key string) string {
	if vs := u[section][key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func (u unit) has(section, key string) bool { return len(u[section][key]) > 0 }

// parseUnit reads one unit file.
//
// Strict about the things systemd is strict about and that an editor gets
// wrong: a directive outside any section is ignored by systemd and would be a
// hardening line that does nothing, and a line that is not Key=Value is a
// syntax error systemd reports at load and nothing else notices.
func parseUnit(t *testing.T, name string) unit {
	t.Helper()
	b, err := FS.ReadFile(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if strings.Contains(string(b), "\r\n") {
		t.Errorf("%s has CRLF line endings; systemd keeps the \\r as part of the value", name)
	}
	out := unit{}
	section := ""
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.Trim(line, "[]")
			if out[section] == nil {
				out[section] = map[string][]string{}
			}
			continue
		}
		if section == "" {
			t.Errorf("%s:%d: %q is outside any section, so systemd ignores it", name, i+1, line)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("%s:%d: %q is not Key=Value", name, i+1, line)
			continue
		}
		k = strings.TrimSpace(k)
		out[section][k] = append(out[section][k], strings.TrimSpace(v))
	}
	return out
}

func unitNames(t *testing.T, suffix string) []string {
	t.Helper()
	ents, err := FS.ReadDir("systemd")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, path.Join("systemd", e.Name()))
		}
	}
	return out
}

// hostJobUnits are the concrete template instances this build ships, and the
// directives each one is ALLOWED to differ from gvm-job@.service on.
//
// Declared, so that a difference is a decision somebody wrote down. The whole
// hazard of a concrete unit shadowing a template is that it inherits nothing:
// a hardening directive left out of one of these is not inherited from the
// template, it is simply absent, and the symptom is a job running with more
// reach than the template says any job gets.
var hostJobUnits = map[string]map[string]string{
	"systemd/gvm-job@_guard.service": {
		// A sweep that fires every five minutes must not be able to block its
		// own next 48 runs.
		"TimeoutStartSec": "600",
	},
	"systemd/gvm-job@_verify.service": {},
}

// hostJobAdditions are directives a concrete unit may add that the template
// does not carry at all.
var hostJobAdditions = map[string]map[string]bool{
	"systemd/gvm-job@_verify.service": {
		// Nightly re-hashing of gigabytes must sit behind everything else on
		// the box, or it is the reason an operator disables the one check
		// that finds a truncated archive before a restore needs it.
		"IOSchedulingClass": true,
		"Nice":              true,
	},
}

// TestTheHostJobUnitsCarryTheTemplatesHardening.
//
// A concrete `gvm-job@_guard.service` takes precedence over the template for
// that instance and inherits nothing from it, so every directive the template
// relies on has to be repeated -- and repetition rots. This is the assertion
// that makes the duplication safe: add a directive to gvm-job@.service and
// forget the two concrete units, and this test names them.
func TestTheHostJobUnitsCarryTheTemplatesHardening(t *testing.T) {
	tmpl := parseUnit(t, "systemd/gvm-job@.service")
	if len(tmpl["Service"]) < 10 {
		t.Fatalf("the template's [Service] has %d directives; this test would prove nothing",
			len(tmpl["Service"]))
	}
	for name, overrides := range hostJobUnits {
		t.Run(path.Base(name), func(t *testing.T) {
			u := parseUnit(t, name)
			for key, want := range tmpl["Service"] {
				got := u["Service"][key]
				if len(got) == 0 {
					t.Errorf("%s does not carry %s=%s; a concrete unit inherits NOTHING from "+
						"the template it shadows", path.Base(name), key, strings.Join(want, " "))
					continue
				}
				if ov, ok := overrides[key]; ok {
					if got[0] != ov {
						t.Errorf("%s has %s=%s; the declared override is %s",
							path.Base(name), key, got[0], ov)
					}
					continue
				}
				if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
					t.Errorf("%s has %s=%s, the template has %s; if that is deliberate, "+
						"declare it in hostJobUnits with a reason",
						path.Base(name), key, strings.Join(got, " "), strings.Join(want, " "))
				}
			}
			for key := range u["Service"] {
				if tmpl.has("Service", key) || hostJobAdditions[name][key] {
					continue
				}
				t.Errorf("%s adds %s, which the template does not carry; declare it in "+
					"hostJobAdditions with a reason", path.Base(name), key)
			}
			// The thing that makes it a gvm job at all.
			if got := u.first("Service", "ExecStart"); !strings.Contains(got, "gvm job run %i") {
				t.Errorf("ExecStart = %q; a host job unit runs `gvm job run %%i`", got)
			}
			if got := u.first("Service", "Slice"); got != "gvm-jobs.slice" {
				t.Errorf("Slice = %q; a job competes with the other provisioning work, not "+
					"with a live world", got)
			}
		})
	}
}

// TestEveryTimerTriggersAUnitThisBuildShips.
//
// A timer whose Unit= names a service that is not installed is the least
// observable failure in this subsystem: `systemctl list-timers` shows it
// happily, and the only symptom is that nothing ever happens.
func TestEveryTimerTriggersAUnitThisBuildShips(t *testing.T) {
	timers := unitNames(t, ".timer")
	if len(timers) == 0 {
		t.Fatal("no timers ship here, so this test proves nothing")
	}
	for _, name := range timers {
		u := parseUnit(t, name)
		target := u.first("Timer", "Unit")
		if target == "" {
			t.Errorf("%s names no Unit=", path.Base(name))
			continue
		}
		if _, err := FS.ReadFile(path.Join("systemd", target)); err == nil {
			continue
		}
		// Not a concrete unit, so it must be a template instance whose
		// template ships: gvm-job@<instance>.service -> gvm-job@.service.
		stem, inst, ok := templateOf(target)
		if !ok {
			t.Errorf("%s triggers %s, which this build does not ship and is not a template "+
				"instance", path.Base(name), target)
			continue
		}
		if _, err := FS.ReadFile(path.Join("systemd", stem)); err != nil {
			t.Errorf("%s triggers %s and neither it nor its template %s ships here",
				path.Base(name), target, stem)
			continue
		}
		if _, err := jobs.ParseSlug(inst); err != nil {
			// A slug the job layer will not act on is a timer that fails every
			// time it fires. It is checked here because the string in the
			// unit file and the parser in internal/jobs are the two halves of
			// D-13, and nothing else compares them.
			t.Errorf("%s triggers %s, whose %%i is %q: %v", path.Base(name), target, inst, err)
		}
	}
}

// templateOf splits "gvm-job@_guard.service" into "gvm-job@.service" and
// "_guard".
func templateOf(unitName string) (template, instance string, ok bool) {
	at := strings.Index(unitName, "@")
	dot := strings.LastIndex(unitName, ".")
	if at < 0 || dot < at {
		return "", "", false
	}
	return unitName[:at+1] + unitName[dot:], unitName[at+1 : dot], true
}

// TestTheGuardTimerIsSection134sFigures.
//
// OnBootSec=3min and OnUnitActiveSec=5min are not taste: the five minutes is
// the number section 13.4 promises an operator as the worst-case gap between a
// SIGKILLed job and their server coming back, and it is quoted in the guard's
// own log line and in internal/jobs' comments. If this timer drifts, those
// sentences become wrong.
func TestTheGuardTimerIsSection134sFigures(t *testing.T) {
	u := parseUnit(t, "systemd/gvm-guard.timer")
	for _, tc := range []struct{ key, want string }{
		{"Unit", "gvm-job@_guard.service"},
		{"OnBootSec", "3min"},
		{"OnUnitActiveSec", "5min"},
		// A missed sweep is not work to replay: the next one re-derives
		// everything, and OnBootSec covers the reboot.
		{"Persistent", "false"},
	} {
		if got := u.first("Timer", tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
	if u.has("Timer", "OnCalendar") {
		t.Error("the guard timer uses OnCalendar; what matters is the interval since the last " +
			"SWEEP, not a wall-clock grid")
	}
	if !u.has("Install", "WantedBy") {
		t.Error("the guard timer has no [Install] WantedBy, so `systemctl enable` has nothing " +
			"to link and the recovery path is never armed")
	}
}

// TestTheVerifyTimerCatchesUpAMissedNight.
//
// Persistent=true, which is the opposite of the restart timer's answer and for
// the backup timer's reason: a missed restart is nothing, and a missed
// verification is a night in which the check that would have found a truncated
// archive did not run.
func TestTheVerifyTimerCatchesUpAMissedNight(t *testing.T) {
	u := parseUnit(t, "systemd/gvm-job@_verify.timer")
	if got := u.first("Timer", "Unit"); got != "gvm-job@_verify.service" {
		t.Errorf("Unit = %q", got)
	}
	if got := u.first("Timer", "Persistent"); got != "true" {
		t.Errorf("Persistent = %q, want true: a missed verification is a gap in evidence", got)
	}
	cal := u.first("Timer", "OnCalendar")
	if cal == "" {
		t.Fatal("the verify timer names no OnCalendar")
	}
	if !strings.Contains(cal, "UTC") {
		t.Errorf("OnCalendar = %q; a HOST timer states its timezone, because an hour that "+
			"shifted twice a year for daylight saving would drift into a backup window "+
			"nobody moved", cal)
	}
	if !u.has("Install", "WantedBy") {
		t.Error("the verify timer has no [Install] WantedBy")
	}
}

// TestEveryHostJobUnitNamesAJobTheJobLayerCanRun, in both directions.
//
// jobs.HostJobs() is a CLOSED set -- a host-level job acts on every instance
// on the box, so adding one is a decision -- and packaging is the other half
// of that decision. A unit for a host job the layer does not know fails every
// time it fires; a host job with no unit never fires at all, which is worse,
// because `systemctl list-timers` shows nothing to notice.
func TestEveryHostJobUnitNamesAJobTheJobLayerCanRun(t *testing.T) {
	shipped := map[string]bool{}
	for _, name := range unitNames(t, ".service") {
		_, inst, ok := templateOf(path.Base(name))
		if !ok || !strings.HasPrefix(inst, "_") {
			continue
		}
		shipped[strings.TrimPrefix(inst, "_")] = true
		res, err := jobs.Resolve(nil, inst)
		if err != nil {
			t.Errorf("%s: %v", path.Base(name), err)
			continue
		}
		if _, err := jobs.Lookup(res.Kind); err != nil {
			t.Errorf("%s resolves to the %s kind, which this build cannot run: %v",
				path.Base(name), res.Kind, err)
		}
	}
	for _, j := range jobs.HostJobs() {
		if !shipped[j] {
			t.Errorf("the job layer runs the host-level job %q and packaging ships no unit "+
				"for it, so it never fires and nothing says so", j)
		}
	}
}

// TestControlPlaneUnitsCarryNoControlPlaneDeps is Phase 48 item 8's portable
// half, on top of ci/control-plane-dead.sh's game-unit checks: the one rule
// says kill gvmd, kill the dashboard, kill Postgres and everything comes
// back — which requires that none of the three control-plane units drags
// another up or down with it. A `Requires=postgresql.service` on gvmd would
// take the verb gateway down with a database restart; an `After=` would stall
// the dashboard's degraded page behind a database that is never coming up.
// (The VM kill test — stop all three, assert the game's pid and starttime are
// unchanged — needs the fresh-Debian runner and remains with it.)
func TestControlPlaneUnitsCarryNoControlPlaneDeps(t *testing.T) {
	units := []string{
		"systemd/gvmd.service",
		"systemd/gvm-web.service",
		"systemd/gvm-db-migrate.service",
	}
	banned := []string{"postgresql", "gvmd.service", "gvm-web.service", "gvm@", "gvm-job@"}
	keys := []string{"Requires=", "Wants=", "BindsTo=", "After=", "Before=", "PartOf="}
	for _, name := range units {
		b, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, key := range keys {
				if !strings.HasPrefix(trimmed, key) {
					continue
				}
				for _, target := range banned {
					if strings.Contains(trimmed, target) {
						t.Errorf("%s: %q couples the control plane together; rule 1 forbids it", name, trimmed)
					}
				}
			}
		}
	}
}

// TestEveryShippedFileIsEmbedded guards against the one mistake that is
// invisible: adding a unit file and not noticing that //go:embed did not take
// it (a name beginning with '_' or '.' is excluded from a directory embed).
func TestEveryShippedFileIsEmbedded(t *testing.T) {
	var n int
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Four units plus a slice pair plus the tmpfiles fragment, at the time of
	// writing. The floor is deliberately loose: what it catches is an embed
	// that silently took nothing.
	if n < 7 {
		t.Errorf("%d files embedded; packaging/ has more than that on disk, so a //go:embed "+
			"pattern is not matching", n)
	}
}

// TestTheDeferredChainFitsInsideTimeoutStopSec.
//
// The invariant this project already enforces for the shutdown ladder
// (internal/spec/manifests_test.go: "budget does not cover the ... ladder;
// TimeoutStopSec would SIGKILL mid-save") applied to the other place a
// deadline races systemd's patience.
//
// A cancelled job runs four deferred legs in sequence, each with its own
// detached budget: the step unwind, the lease release, the running guard's
// restore and the terminal audit row. They used to budget up to twenty-one
// minutes between them -- the restore and the unwind both took
// DefaultRestoreBudget at ten minutes each -- against a TimeoutStopSec of 600
// with SendSIGKILL=yes. So SIGKILL could land inside the guard's own
// Super.Start, before the degraded breadcrumb was written and before the
// terminal job_runs row, which is the evidence an operator has nothing else to
// read.
//
// The restore leg is where the time really goes and the arithmetic is not
// theoretical: gvm@.service is Type=notify, so its start job does not complete
// until READY=1, and its TimeoutStartSec is raised from the manifest's
// readiness.hard_deadline -- 3600s for Project Zomboid. hostops' job() waits
// for JobRemoved because "that is the whole point", so the guard blocks until
// its own deadline cuts it off.
func TestTheDeferredChainFitsInsideTimeoutStopSec(t *testing.T) {
	units := []string{
		"systemd/gvm-job@.service",
		"systemd/gvm-job@_guard.service",
		"systemd/gvm-job@_verify.service",
	}
	for _, name := range units {
		t.Run(path.Base(name), func(t *testing.T) {
			u := parseUnit(t, name)
			raw := u.first("Service", "TimeoutStopSec")
			if raw == "" {
				t.Fatal("no TimeoutStopSec; SendSIGKILL would then use the manager default")
			}
			secs, err := strconv.Atoi(raw)
			if err != nil {
				t.Fatalf("TimeoutStopSec = %q, which this test cannot compare", raw)
			}
			stop := time.Duration(secs) * time.Second
			if jobs.DeferredChainBudget >= stop {
				t.Errorf("the deferred chain may spend %v and %s allows %v before SIGKILL; a "+
					"job killed mid-restore writes no breadcrumb and no terminal row",
					jobs.DeferredChainBudget, path.Base(name), stop)
			}
			// And some head-room, not a photo finish: the legs are budgets,
			// not measurements, and the four of them plus process teardown
			// have to fit with room for a loaded box.
			if margin := stop - jobs.DeferredChainBudget; margin < time.Minute {
				t.Errorf("only %v of head-room under %s's TimeoutStopSec", margin,
					path.Base(name))
			}
		})
	}
}

// TestTheChainBudgetIsTheSumOfItsLegs, so the constant cannot drift away from
// the four budgets it is meant to bound.
func TestTheChainBudgetIsTheSumOfItsLegs(t *testing.T) {
	sum := jobs.UnwindBudget + jobs.ClearLeaseBudget + jobs.DefaultRestoreBudget +
		jobs.FinishBudget
	if sum != jobs.DeferredChainBudget {
		t.Errorf("the legs add up to %v and DeferredChainBudget is %v; the assertion against "+
			"TimeoutStopSec is only meaningful if they agree", sum, jobs.DeferredChainBudget)
	}
}
