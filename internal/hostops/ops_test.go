package hostops

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func rootActor() audit.Actor { return audit.RootCLI() }

// TestSupervisionVerbsDeriveTheUnit is the shape of the whole package in one
// test: the request names an instance and a unit KIND, and hostops decides
// which unit that is.
//
// The alternative -- a unit name in the request -- would make `unit.start` a
// verb that starts anything, and the enum would be documentation rather than a
// boundary.
func TestSupervisionVerbsDeriveTheUnit(t *testing.T) {
	// The params column is per row because each verb's audit action has its
	// own allowlist: instance.stop REQUIRES the rung on its success row, and
	// instance.apply would drop a "reason" it has no column for.
	tests := []struct {
		verb   Verb
		unit   UnitKind
		job    string
		params audit.Params
		want   string
	}{
		{VerbUnitStart, UnitInstance, "", audit.Params{"reason": "test"}, "start gvm@pz-main.service"},
		{VerbUnitStop, UnitInstance, "", audit.Params{"rung": "console_quit"}, "stop gvm@pz-main.service"},
		{VerbUnitRestart, UnitInstance, "", audit.Params{"reason": "test"}, "restart gvm@pz-main.service"},
		{VerbUnitEnable, UnitJobTimer, "nightly", nil, "enable gvm-job@pz-main_nightly.timer"},
		{VerbUnitDisable, UnitJobTimer, "nightly", nil, "disable gvm-job@pz-main_nightly.timer"},
		{VerbUnitStart, UnitJob, "nightly", nil, "start gvm-job@pz-main_nightly.service"},
	}
	for _, tc := range tests {
		sd := newFakeSystemd()
		o, _ := testOps(t, newFakeFS(), sd, nil)
		res, err := o.Do(context.Background(), Request{
			Verb: tc.verb, Actor: rootActor(), Instance: "pz-main", Unit: tc.unit, Job: tc.job,
			Params: tc.params,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if len(sd.calls) != 1 || sd.calls[0] != tc.want {
			t.Errorf("%s asked systemd for %v, want [%s]", tc.verb, sd.calls, tc.want)
		}
		if res.Unit != strings.SplitN(tc.want, " ", 2)[1] {
			t.Errorf("%s reported unit %q", tc.verb, res.Unit)
		}
		if !res.Changed {
			t.Errorf("%s reported no change", tc.verb)
		}
	}
}

func TestUnitKindAndJobNameMustAgree(t *testing.T) {
	sd := newFakeSystemd()
	o, _ := testOps(t, newFakeFS(), sd, nil)
	ctx := context.Background()

	// A job name with the instance unit is ambiguous: the caller either meant
	// the job unit or pasted a field it did not mean. Guessing produces a stop
	// of the wrong thing.
	_, err := o.Do(ctx, Request{Verb: VerbUnitStop, Actor: rootActor(), Instance: "pz-main", Job: "nightly"})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("instance unit plus a job name: got %v, want ErrBadRequest", err)
	}
	_, err = o.Do(ctx, Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main", Unit: UnitJob})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("job unit with no job name: got %v, want ErrBadRequest", err)
	}
	_, err = o.Do(ctx, Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main", Unit: "timer"})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("undeclared unit kind: got %v, want ErrBadRequest", err)
	}
	if len(sd.calls) != 0 {
		t.Errorf("a refused request still reached systemd: %v", sd.calls)
	}
}

// TestAnUndeclaredVerbIsRefusedAndRecorded: the single most interesting thing
// that can happen to admin.sock must not be the one thing with no trace.
func TestAnUndeclaredVerbIsRefusedAndRecorded(t *testing.T) {
	sd := newFakeSystemd()
	o, sink := testOps(t, newFakeFS(), sd, nil)

	_, err := o.Do(context.Background(), Request{
		Verb: "unit.kill", Actor: audit.User("u-1", audit.ViaWeb), Instance: "pz-main",
	})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("got %v, want ErrUnknownVerb", err)
	}
	if len(sd.calls) != 0 {
		t.Fatalf("an undeclared verb reached systemd: %v", sd.calls)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("wrote %d audit records, want 1", len(recs))
	}
	if recs[0].Action != audit.AuthDenied {
		t.Errorf("recorded as %q, want %q", recs[0].Action, audit.AuthDenied)
	}
	if recs[0].Target != "unit.kill" {
		t.Errorf("target is %q; the refused verb is the useful field", recs[0].Target)
	}
	if recs[0].Via != audit.ViaWeb {
		t.Errorf("actor_via is %q; which process asserted the actor is the point of the column", recs[0].Via)
	}
}

func TestRequestValidation(t *testing.T) {
	o, _ := testOps(t, newFakeFS(), newFakeSystemd(), nil)
	ctx := context.Background()

	bad := []struct {
		what string
		req  Request
	}{
		{"no instance", Request{Verb: VerbUnitStart, Actor: rootActor()}},
		{"invalid instance name", Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "../etc"}},
		{"instance on a host-wide verb", Request{Verb: VerbUnitList, Actor: rootActor(), Instance: "pz-main", Scope: ScopeInstances}},
		{"invalid job name", Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main", Unit: UnitJob, Job: "night ly"}},
	}
	for _, tc := range bad {
		if _, err := o.Do(ctx, tc.req); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", tc.what, err)
		}
	}
}

// TestObservingVerbsWriteNoAuditRecord is the deliberate asymmetry.
//
// audit_log is append-only by grant, so noise written into it cannot be
// removed. A dashboard polling unit state every two seconds would bury the
// rows that matter.
func TestObservingVerbsWriteNoAuditRecord(t *testing.T) {
	sd := newFakeSystemd()
	sd.states["gvm@pz-main.service"] = platform.UnitState{
		Name: "gvm@pz-main.service", ActiveState: "active", SubState: "running", MainPID: 4242,
	}
	sd.units = []platform.UnitState{{Name: "gvm@pz-main.service"}}
	f := newFakeFS(controlPlaneDirs()...)
	f.usage = platform.DiskUsage{Total: 100, Free: 50, Avail: 40}
	o, sink := testOps(t, f, sd, nil)
	ctx := context.Background()

	res, err := o.Do(ctx, Request{Verb: VerbUnitState, Actor: rootActor(), Instance: "pz-main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.State.MainPID != 4242 {
		t.Errorf("state came back as %+v", res.State)
	}

	res, err = o.Do(ctx, Request{Verb: VerbUnitList, Actor: rootActor(), Scope: ScopeInstances})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Units) != 1 {
		t.Errorf("listed %d units", len(res.Units))
	}
	if sd.calls[len(sd.calls)-1] != "list gvm@*.service" {
		t.Errorf("scope %q resolved to %q", ScopeInstances, sd.calls[len(sd.calls)-1])
	}

	f.mkdirs("/var/backups/gvm/pz-main")
	res, err = o.Do(ctx, Request{Verb: VerbDiskUsage, Actor: rootActor(), Instance: "pz-main", Dir: DirBackups})
	if err != nil {
		t.Fatal(err)
	}
	if res.Disk.Avail != 40 || res.Path != "/var/backups/gvm/pz-main" {
		t.Errorf("disk.usage answered %+v for %s", res.Disk, res.Path)
	}

	if n := sink.Len(); n != 0 {
		t.Fatalf("three observations wrote %d audit records", n)
	}
}

func TestUnitListRefusesAnUndeclaredScope(t *testing.T) {
	sd := newFakeSystemd()
	o, _ := testOps(t, newFakeFS(), sd, nil)
	// The empty scope is the zero value, and it must not silently mean "*":
	// the whole host's unit table is not gvm's to hand out.
	if _, err := o.Do(context.Background(), Request{Verb: VerbUnitList, Actor: rootActor()}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("got %v, want ErrBadRequest", err)
	}
	if len(sd.calls) != 0 {
		t.Errorf("a refused list still reached systemd: %v", sd.calls)
	}
}

// TestMutatingVerbsAreAuditedAsAStartedAndATerminalRow follows the two-row
// shape section 14.8 requires: the `started` row is committed before the work,
// so an operation that crashes the daemon still leaves a trace of having begun.
func TestMutatingVerbsAreAuditedAsAStartedAndATerminalRow(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	o, sink := testOps(t, f, newFakeSystemd(), nil)

	res, err := o.Do(context.Background(), Request{
		Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main",
		File: FileDropIn, Body: []byte("[Service]\nUser=pzserver\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Path != "/etc/systemd/system/gvm@pz-main.service.d/10-gvm.conf" {
		t.Fatalf("result: %+v", res)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("wrote %d records, want a started and a terminal row", len(recs))
	}
	if recs[0].Outcome != audit.Started || recs[1].Outcome != audit.OK {
		t.Fatalf("outcomes are %q then %q", recs[0].Outcome, recs[1].Outcome)
	}
	if recs[1].Action != audit.InstanceApply || recs[1].InstanceName != "pz-main" {
		t.Errorf("terminal row is %q for %q", recs[1].Action, recs[1].InstanceName)
	}
	if recs[1].ParentSpoolID != recs[0].SpoolID {
		t.Error("the terminal row does not point at its started row")
	}
	// The path is in params.actions, which is what audit.InstanceApply
	// allowlists. A row that says an apply happened without saying what it
	// touched is the row the legacy suite already produced.
	actions := fmt.Sprint(recs[1].Params["actions"])
	if !strings.Contains(actions, res.Path) {
		t.Errorf("params.actions is %q and does not name %s", actions, res.Path)
	}
	if _, dropped := recs[1].Params[audit.KeyUnallowlisted]; dropped {
		t.Errorf("hostops passed a param the action does not allow: %v", recs[1].Params)
	}
}

// TestAFailedVerbIsRecordedAsFailed: a refusal is exactly as interesting as a
// success, and more so when it is the one that left an apply half-done.
func TestAFailedVerbIsRecordedAsFailed(t *testing.T) {
	sd := newFakeSystemd()
	sd.err = errors.New("Unit gvm@pz-main.service not found")
	o, sink := testOps(t, newFakeFS(), sd, nil)

	_, err := o.Do(context.Background(), Request{
		Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main",
	})
	if err == nil {
		t.Fatal("a systemd failure was not reported")
	}
	if !strings.Contains(err.Error(), "gvm@pz-main.service") {
		t.Errorf("the error does not name the unit: %v", err)
	}
	recs := sink.Records()
	if len(recs) != 2 || recs[1].Outcome != audit.Failed {
		t.Fatalf("terminal outcome is %q", recs[len(recs)-1].Outcome)
	}
	if recs[1].Error == "" {
		t.Error("the failure row carries no error text")
	}
}

// TestCallerParamsSurviveTheAllowlist proves the reason Request.Params exists:
// hostops cannot know which rung the shutdown ladder ended on, and
// instance.stop requires it on the success row.
func TestCallerParamsSurviveTheAllowlist(t *testing.T) {
	o, sink := testOps(t, newFakeFS(), newFakeSystemd(), nil)
	_, err := o.Do(context.Background(), Request{
		Verb: VerbUnitStop, Actor: rootActor(), Instance: "pz-main",
		Params: audit.Params{"rung": "console_quit", "reason": "nightly restart", "password": "hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, terminal := sink.Records()[0], sink.Records()[1]
	if terminal.Params["rung"] != "console_quit" {
		t.Errorf("params.rung is %v; a stop that does not say how it ended is the record "+
			"the bash suite already produced", terminal.Params["rung"])
	}
	for _, rec := range []audit.Record{started, terminal} {
		if _, kept := rec.Params["password"]; kept {
			t.Errorf("an unallowlisted param reached the %s row", rec.Outcome)
		}
	}
	// The drop is recorded on the row where it happened. The terminal row is
	// built from the started row's ALREADY-FILTERED params, so by then there
	// is nothing left to drop -- which is why this assertion is on the first
	// row and not the last.
	if _, noted := started.Params[audit.KeyUnallowlisted]; !noted {
		t.Error("the dropped param was not recorded as dropped; section 14.8 wants it visible")
	}
}

// TestDirEnsureUsesTheAccountFromTheInstanceFile is the adopted-instance case.
//
// pzserver predates gvm. Deriving "gvm-pz-main" from the instance name would
// either fail to resolve or -- worse -- resolve to a managed account and take
// a live world's directories away from the process using them.
func TestDirEnsureUsesTheAccountFromTheInstanceFile(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	sink := audit.NewMemorySink()
	o, err := New(Config{
		FS: f, Audit: audit.New(sink), LoadInstance: loadFixedInstance("pzserver"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Do(context.Background(), Request{
		Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirLogs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/var/log/gvm/pz-main" || !res.Changed {
		t.Fatalf("result: %+v", res)
	}
	if got := f.wrote(t, res.Path).owner; got != "pzserver" {
		t.Errorf("owner is %q, want the account from the instance file", got)
	}

	// A kind that is root's is not chowned at all: no lookup, no dependency
	// on an account existing yet.
	f.trace = nil
	if _, err := o.Do(context.Background(), Request{
		Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirBackups,
	}); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.trace {
		if strings.HasPrefix(op, "chown ") {
			t.Errorf("a root-owned directory was chowned: %s", op)
		}
	}
}

func TestDirEnsureRefusesAnInstanceFileWithNoUser(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	sink := audit.NewMemorySink()
	o, err := New(Config{FS: f, Audit: audit.New(sink), LoadInstance: loadFixedInstance("")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Do(context.Background(), Request{
		Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirSrv,
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("got %v, want ErrBadRequest", err)
	}
}

func TestUnknownKindsAreRefused(t *testing.T) {
	o, _ := testOps(t, newFakeFS(controlPlaneDirs()...), newFakeSystemd(), nil)
	ctx := context.Background()
	for _, req := range []Request{
		{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: "sudoers"},
		{Verb: VerbFileRemove, Actor: rootActor(), Instance: "pz-main", File: ""},
		{Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: "root"},
		{Verb: VerbDiskUsage, Actor: rootActor(), Instance: "pz-main", Dir: ""},
	} {
		if _, err := o.Do(ctx, req); !errors.Is(err, ErrUnknownKind) {
			t.Errorf("%s %q/%q: got %v, want ErrUnknownKind", req.Verb, req.File, req.Dir, err)
		}
	}
}

func TestFileVerbsRefuseMismatchedAddressing(t *testing.T) {
	o, _ := testOps(t, newFakeFS(controlPlaneDirs()...), newFakeSystemd(), nil)
	ctx := context.Background()
	bad := []struct {
		what string
		req  Request
	}{
		{"per-job kind with no job", Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileJobTimer}},
		{"non-job kind with a job", Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileDropIn, Job: "nightly"}},
		{"legacy kind with no legacy unit", Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileLegacyGuard}},
		{"non-legacy kind with a legacy unit", Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileDropIn, LegacyUnit: "zomboid.service"}},
	}
	for _, tc := range bad {
		if _, err := o.Do(ctx, tc.req); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", tc.what, err)
		}
	}
}

func TestFileRemoveIsIdempotent(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	o, _ := testOps(t, f, newFakeSystemd(), nil)
	ctx := context.Background()
	req := Request{Verb: VerbFileRemove, Actor: rootActor(), Instance: "pz-main",
		File: FileJobTimer, Job: "nightly"}

	// Removing a timer for a job the operator deleted has to be re-runnable:
	// a reconciler that fails on its own successful previous run is one nobody
	// runs twice.
	res, err := o.Do(ctx, req)
	if err != nil {
		t.Fatalf("removing an absent file: %v", err)
	}
	if res.Changed {
		t.Error("removing an absent file reported a change")
	}

	if _, err := o.Do(ctx, Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main",
		File: FileJobTimer, Job: "nightly", Body: []byte("[Timer]\n")}); err != nil {
		t.Fatal(err)
	}
	res, err = o.Do(ctx, req)
	if err != nil || !res.Changed {
		t.Fatalf("removing a present file: changed=%v err=%v", res.Changed, err)
	}
	if _, still := f.files[res.Path]; still {
		t.Errorf("%s is still there", res.Path)
	}
}

// fakeUsers records what the account seam was asked for, and starts from a
// fixed table of accounts that already exist.
type fakeUsers struct {
	existing map[string]platform.User
	created  []string
}

func (u *fakeUsers) Lookup(_ context.Context, name string) (platform.User, error) {
	if got, ok := u.existing[name]; ok {
		return got, nil
	}
	return platform.User{}, fmt.Errorf("%w: %s", ErrNoSuchAccount, name)
}

func (u *fakeUsers) EnsureSystemUser(_ context.Context, name, home string) (platform.User, error) {
	if got, ok := u.existing[name]; ok {
		return got, nil
	}
	u.created = append(u.created, name+" home="+home)
	got := platform.User{Name: name, UID: 900, GID: 900, Home: home}
	u.existing[name] = got
	return got, nil
}

func (u *fakeUsers) EnsureGroup(context.Context, string) error        { return nil }
func (u *fakeUsers) AddToGroup(context.Context, string, string) error { return nil }

func TestUserEnsureReportsWhetherItCreatedAnything(t *testing.T) {
	users := &fakeUsers{existing: map[string]platform.User{}}
	o, _ := testOps(t, newFakeFS(), newFakeSystemd(), users)
	ctx := context.Background()
	req := Request{Verb: VerbUserEnsure, Actor: rootActor(), Instance: "pz-main"}

	res, err := o.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.User.Name != "gvm-pz-main" {
		t.Fatalf("first ensure: %+v", res)
	}
	if len(users.created) != 1 || !strings.Contains(users.created[0], "home=/srv/gvm/pz-main") {
		t.Errorf("created %v; section 6.4 gives the account --home-dir /srv/gvm/<name>", users.created)
	}

	res, err = o.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("a second ensure claimed it changed an account that already existed")
	}
	if len(users.created) != 1 {
		t.Errorf("a second ensure created another account: %v", users.created)
	}
}

func TestNewRefusesWithoutAnAuditLogger(t *testing.T) {
	if _, err := New(Config{FS: newFakeFS()}); !errors.Is(err, ErrNoAudit) {
		t.Fatalf("got %v, want ErrNoAudit", err)
	}
	if _, err := NewLocal(context.Background(), nil); !errors.Is(err, ErrNoAudit) {
		t.Fatalf("NewLocal: got %v, want ErrNoAudit", err)
	}
}

// TestSeamsRefuseRatherThanPretend: an Ops with no systemd must not report a
// clean start against a host it never touched.
func TestSeamsRefuseRatherThanPretend(t *testing.T) {
	o, _ := testOps(t, newFakeFS(), nil, nil)
	ctx := context.Background()
	if _, err := o.Do(ctx, Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main"}); !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("unit.start with no systemd: got %v", err)
	}
	if _, err := o.Do(ctx, Request{Verb: VerbUserEnsure, Actor: rootActor(), Instance: "pz-main"}); !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("user.ensure with no account seam: got %v", err)
	}
}

// TestNewLocalIsAvailableOnlyOnLinux keeps the stub honest. Off Linux it must
// refuse, and it must refuse with the sentinel every caller degrades on.
func TestNewLocalRefusesOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("NewLocal needs root and a running systemd; this is the cross-platform half")
	}
	if _, err := NewLocal(context.Background(), audit.New(audit.NewMemorySink())); !errors.Is(err, platform.ErrNotSupported) {
		t.Fatalf("got %v, want platform.ErrNotSupported", err)
	}
}

func TestSummaryListsEveryVerb(t *testing.T) {
	out := Summary()
	for _, v := range Verbs() {
		if !strings.Contains(out, string(v)) {
			t.Errorf("%q is missing from the capability listing", v)
		}
	}
}

// loadInstanceSignatureMatchesSpec keeps the seam and the production function
// in step: a Config whose LoadInstance no longer accepts spec.LoadInstance
// would compile here and fail in New's default.
var _ = func() func(string) (*spec.Instance, spec.Diags, error) { return spec.LoadInstance }

// TestAHalfAppliedDirectoryIsReported: a mkdir that succeeded and a chown that
// did not leaves a directory the instance user cannot write. Reporting the
// change and the error together is what lets an apply say which step it got to.
func TestAHalfAppliedDirectoryIsReported(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	f.failChown = true
	o, _ := testOps(t, f, newFakeSystemd(), nil)

	res, err := o.Do(context.Background(), Request{
		Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirSrv,
	})
	if err == nil {
		t.Fatal("a failed chown was reported as success")
	}
	if !res.Changed {
		t.Error("the directory was created and the result says nothing changed")
	}
	if res.Path != "/srv/gvm/pz-main" {
		t.Errorf("the failure does not name the directory: %+v", res)
	}
}

// TestDiskUsageSaysWhenItCouldNotMeasure.
//
// platform.Statfs is Linux-only, so on the development machine the answer is
// ErrNotSupported rather than a zero DiskUsage. Returning the zero value would
// make every preflight refuse for lack of space on the one platform where the
// preflight is meaningless -- and, worse, an "unchecked" volume reported as
// "0 bytes free" reads like a measurement.
func TestDiskUsageSaysWhenItCouldNotMeasure(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	f.mkdirs("/var/backups/gvm/pz-main")
	f.noStatfs = true
	o, _ := testOps(t, f, newFakeSystemd(), nil)

	res, err := o.Do(context.Background(), Request{
		Verb: VerbDiskUsage, Actor: rootActor(), Instance: "pz-main", Dir: DirBackups,
	})
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Fatalf("got %v, want platform.ErrNotSupported", err)
	}
	if res.Disk != (platform.DiskUsage{}) {
		t.Errorf("a refusal came back with numbers in it: %+v", res.Disk)
	}
	if res.Path == "" {
		t.Error("the refusal does not say which path could not be measured")
	}
}

// TestAFieldTheVerbIgnoresIsRefused.
//
// Each of these would otherwise succeed while doing less than the caller
// asked. That is the failure mode the Args declaration exists to convert into
// an error: silence, not a wrong answer.
func TestAFieldTheVerbIgnoresIsRefused(t *testing.T) {
	f := newFakeFS(controlPlaneDirs()...)
	sd := newFakeSystemd()
	o, _ := testOps(t, f, sd, nil)
	ctx := context.Background()

	bad := []struct {
		what string
		req  Request
	}{
		{"a body nothing writes",
			Request{Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirState, Body: []byte("x")}},
		{"a file kind on a reload",
			Request{Verb: VerbDaemonReload, Actor: rootActor(), Instance: "pz-main", File: FileDropIn}},
		{"a scope on a start",
			Request{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main", Scope: ScopeInstances}},
		{"a dir kind on a file write",
			Request{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileDropIn, Dir: DirState, Body: []byte("x")}},
		{"a unit kind on user.ensure",
			Request{Verb: VerbUserEnsure, Actor: rootActor(), Instance: "pz-main", Unit: UnitJob, Job: "nightly"}},
		{"a legacy unit on a dir",
			Request{Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirState, LegacyUnit: "zomboid.service"}},
	}
	for _, tc := range bad {
		if _, err := o.Do(ctx, tc.req); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", tc.what, err)
		}
	}
	if len(sd.calls) != 0 {
		t.Errorf("a refused request still reached systemd: %v", sd.calls)
	}
	if len(f.trace) != 0 {
		t.Errorf("a refused request still touched the filesystem: %v", f.trace)
	}
}

// TestTheLegacyGuardWritesBesideTheLegacyUnit is the adoption reciprocal from
// section 7.4: systemd itself refuses to run the legacy unit and the gvm unit
// against the same world, and that only works if the drop-in lands in the
// legacy unit's own directory.
func TestTheLegacyGuardWritesBesideTheLegacyUnit(t *testing.T) {
	f := newFakeFS("/etc/systemd/system")
	o, _ := testOps(t, f, newFakeSystemd(), nil)

	res, err := o.Do(context.Background(), Request{
		Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main",
		File: FileLegacyGuard, LegacyUnit: "zomboid.service",
		Body: []byte("[Unit]\nConflicts=gvm@pz-main.service\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/etc/systemd/system/zomboid.service.d/20-gvm-adopt.conf" {
		t.Fatalf("wrote %s", res.Path)
	}

	// An operator-typed unit name is the only string in a Request that reaches
	// a path, and a traversal in it would be a root-owned drop-in wherever the
	// caller pointed.
	_, err = o.Do(context.Background(), Request{
		Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main",
		File: FileLegacyGuard, LegacyUnit: "../../../etc/cron.d/x.service", Body: []byte("x"),
	})
	if !errors.Is(err, ErrLegacyUnit) {
		t.Fatalf("got %v, want ErrLegacyUnit", err)
	}
}

// TestJobDescriptorWriteCreatesItsDirectory: /etc/gvm/jobs is not in
// systemd-tmpfiles' list, so nothing else creates it, and a missing one would
// turn every timer write into a failure the operator cannot act on.
func TestJobDescriptorWriteCreatesItsDirectory(t *testing.T) {
	f := newFakeFS("/etc/gvm/instances") // /etc/gvm exists; /etc/gvm/jobs does not
	o, _ := testOps(t, f, newFakeSystemd(), nil)

	res, err := o.Do(context.Background(), Request{
		Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main",
		File: FileJobDesc, Job: "nightly",
		Body: []byte(`{"instance":"pz-main","job":"nightly","action":"update"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/etc/gvm/jobs/pz-main_nightly.json" {
		t.Fatalf("wrote %s", res.Path)
	}
	dir := f.wrote(t, "/etc/gvm/jobs")
	if !dir.dir || dir.mode.Perm() != 0o750 || dir.group != WebGroup {
		t.Errorf("/etc/gvm/jobs is %04o group %q; the rest of /etc/gvm is 0750 root:gvm",
			dir.mode.Perm(), dir.group)
	}
}
