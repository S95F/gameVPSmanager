package reconcile

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

func planFor(t *testing.T, name, game string, mut func(*PlanInput)) (*Plan, *fakeHost, *fakeInspect) {
	t.Helper()
	in, raw := loadInstance(t, name)
	m := loadManifest(t, game)
	host := &fakeHost{}
	// The account exists by default, which is the state of every box after
	// the first apply and the only state an ADOPTED instance is ever allowed
	// to be in. The one test that needs it absent builds its own inspector.
	ins := newInspect().account(in.Spec.User)

	pi := PlanInput{
		Instance: in, Manifest: m, SpecBytes: raw,
		Actor: rootActor(), Host: host, Inspect: ins,
		Probe: okProbe, HasSecret: allSecretsPresent,
		Now: renderedAt, Version: goldenVersion,
	}
	if mut != nil {
		mut(&pi)
	}
	p, err := BuildPlan(pi)
	if err != nil {
		t.Fatalf("BuildPlan(%s): %v", name, err)
	}
	return p, host, ins
}

// TestTheProbeIsPlannedBeforeTheDropIn is D-15 as an ordering, asserted on the
// plan rather than on the run.
//
// A test that only checked the executed sequence would pass for a plan that
// happened to be ordered right today. Checking the plan is what makes the
// ordering a property of the thing `--diff` prints, which is the thing an
// operator approves.
func TestTheProbeIsPlannedBeforeTheDropIn(t *testing.T) {
	p, _, _ := planFor(t, "pz-main", "projectzomboid", nil)
	probeAt, dropInAt := -1, -1
	for i, s := range p.Describe() {
		switch s.Kind {
		case KindProbe:
			probeAt = i
		case KindDropIn:
			dropInAt = i
		}
	}
	if probeAt < 0 {
		t.Fatal("the plan contains no write-probe")
	}
	if dropInAt < 0 {
		t.Fatal("the plan writes no drop-in")
	}
	if probeAt > dropInAt {
		t.Errorf("the probe is planned at step %d and the drop-in at %d", probeAt, dropInAt)
	}
}

// TestAPlanThatWritesADropInWithNoProbeIsRefused.
//
// The ordering above is enforced by Validate, not by the order the builder
// happens to append in. That distinction matters: a future step inserted in
// the wrong place has to fail loudly rather than silently skip the check that
// exists to catch a sandbox which blocks the game's own save directory.
func TestAPlanThatWritesADropInWithNoProbeIsRefused(t *testing.T) {
	p, _, _ := planFor(t, "pz-main", "projectzomboid", nil)

	stripped := &Plan{Instance: p.Instance}
	for _, a := range p.Steps {
		if a.Describe().Kind == KindProbe {
			continue
		}
		stripped.Add(a)
	}
	err := stripped.Validate()
	if err == nil {
		t.Fatal("a plan that writes a drop-in with no write-probe validated")
	}
	if !errors.Is(err, ErrPlan) || !strings.Contains(err.Error(), "D-15") {
		t.Errorf("the refusal does not name the decision it enforces: %v", err)
	}

	// And Apply must not run the plan either, even if a caller skipped
	// Validate: the check has to be on the path that changes the host.
	a := &Applier{Host: &fakeHost{}}
	if _, err := a.Apply(context.Background(), stripped, nil); err == nil {
		t.Fatal("Apply ran a plan whose drop-in write had no probe before it")
	}
}

// TestAFailedProbeStopsTheApplyBeforeTheDropInIsWritten.
//
// This is the whole of D-15 in one assertion. The failure it models -- an
// adopted world under /home that ProtectHome hides -- must be discovered while
// nothing has changed. If the drop-in were written first, the discovery would
// happen at the next restart, at 3am, on a server that had been running fine.
func TestAFailedProbeStopsTheApplyBeforeTheDropInIsWritten(t *testing.T) {
	refuse := func(_ context.Context, ps ProbeSpec) ([]ProbeResult, error) {
		out := make([]ProbeResult, 0, len(ps.Sandbox.Paths))
		for _, path := range ps.Sandbox.Paths {
			r := ProbeResult{Path: path, OK: true}
			if underHome(path) {
				r = ProbeResult{Path: path, OK: false, Error: "open .gvm-probe-9: read-only file system"}
			}
			out = append(out, r)
		}
		return out, nil
	}
	p, host, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) { pi.Probe = refuse })

	a := &Applier{Host: host}
	_, err := a.Apply(context.Background(), p, nil)
	if err == nil {
		t.Fatal("an apply whose write-probe refused a path completed")
	}
	if !errors.Is(err, ErrProbe) {
		t.Errorf("the failure is not a probe failure: %v", err)
	}
	if !strings.Contains(err.Error(), "/home/pzserver/Zomboid") {
		t.Errorf("the failure does not name the path that could not be written: %v", err)
	}
	// The message has to say WHY the apply stopped, because the operator's
	// next question is "what did it change before it gave up".
	if !strings.Contains(err.Error(), "the drop-in was not written") {
		t.Errorf("the failure does not say that nothing was changed: %v", err)
	}
	for _, v := range host.verbs() {
		if strings.Contains(v, "file.write dropin") {
			t.Fatalf("the drop-in was written after the probe refused a path: %v", host.verbs())
		}
	}
}

// TestAnUnprobableHostRefusesRatherThanProceeding.
//
// "We could not check" silently becoming "checked" is the precise failure the
// probe exists to prevent, so a host with no systemd-run refuses by default.
// `--diff` opts out explicitly, because a plan is not an apply.
func TestAnUnprobableHostRefusesRatherThanProceeding(t *testing.T) {
	cannot := func(context.Context, ProbeSpec) ([]ProbeResult, error) {
		return nil, platformUnsupported()
	}
	p, host, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) { pi.Probe = cannot })
	if _, err := (&Applier{Host: host}).Apply(context.Background(), p, nil); err == nil {
		t.Fatal("an apply proceeded on a host where the sandbox could not be probed")
	}

	p2, host2, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) {
		pi.Probe = cannot
		pi.AllowUnprobed = true
	})
	if _, err := (&Applier{Host: host2}).Apply(context.Background(), p2, nil); err != nil {
		t.Fatalf("AllowUnprobed did not let the plan through: %v", err)
	}
}

// TestAPartialProbeIsNotApproval.
//
// A probe that answered for four of six paths has proved nothing about the
// other two, and the drop-in is about to declare all six writable.
func TestAPartialProbeIsNotApproval(t *testing.T) {
	partial := func(_ context.Context, ps ProbeSpec) ([]ProbeResult, error) {
		return []ProbeResult{{Path: ps.Sandbox.Paths[0], OK: true}}, nil
	}
	p, host, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) { pi.Probe = partial })
	_, err := (&Applier{Host: host}).Apply(context.Background(), p, nil)
	if err == nil {
		t.Fatal("a probe that reported one of four paths was treated as a pass")
	}
	if !strings.Contains(err.Error(), "unproven") {
		t.Errorf("the refusal does not say the paths are unproven: %v", err)
	}
}

// TestAFirstApplyIssuesTheVerbsInDependencyOrder.
//
// Each ordering below fails loudly if reversed, and the reasons are different:
// hostops creates one directory per call and refuses a missing parent; it
// reads the account for an instance-owned directory out of the instance YAML;
// and it chowns those directories to an account that has to exist.
func TestAFirstApplyIssuesTheVerbsInDependencyOrder(t *testing.T) {
	p, host, _ := planFor(t, "valheim-1", "valheim", nil)
	if _, err := (&Applier{Host: host}).Apply(context.Background(), p, nil); err != nil {
		t.Fatal(err)
	}
	got := host.verbs()
	joined := strings.Join(got, "\n")

	mustPrecede := [][2]string{
		{"dir.ensure spec_dir", "file.write manifest_lock"},
		{"dir.ensure state", "dir.ensure snap"},
		{"dir.ensure state", "dir.ensure steam_scripts"},
		{"file.write instance_spec", "user.ensure"},
		{"user.ensure", "dir.ensure srv"},
		{"user.ensure", "dir.ensure logs"},
		{"file.write dropin", "daemon.reload"},
		{"daemon.reload", "unit.enable"},
	}
	for _, mp := range mustPrecede {
		i, j := indexOf(got, mp[0]), indexOf(got, mp[1])
		if i < 0 {
			t.Errorf("%q was never issued:\n%s", mp[0], joined)
			continue
		}
		if j < 0 {
			t.Errorf("%q was never issued:\n%s", mp[1], joined)
			continue
		}
		if i > j {
			t.Errorf("%q (step %d) must come before %q (step %d):\n%s", mp[0], i, mp[1], j, joined)
		}
	}
}

// TestAnAdoptedInstanceGetsNoUserEnsure.
//
// Section 6.4: for an adopted instance gvm creates no user and chowns nothing.
// The account is the identity a live world's files already belong to, and
// "ensure" meaning "make it match my idea of it" is how an adoption breaks a
// server that was working.
func TestAnAdoptedInstanceGetsNoUserEnsure(t *testing.T) {
	p, host, _ := planFor(t, "pz-main", "projectzomboid", nil)
	if _, err := (&Applier{Host: host}).Apply(context.Background(), p, nil); err != nil {
		t.Fatal(err)
	}
	for _, v := range host.verbs() {
		if strings.HasPrefix(v, "user.ensure") {
			t.Fatalf("gvm tried to create an adopted instance's account: %v", host.verbs())
		}
	}
	var found bool
	for _, s := range p.Describe() {
		if s.Kind == KindUser {
			found = true
			if !strings.Contains(s.Note, "not managed by gvm") {
				t.Errorf("the plan does not say the account is not gvm's: %q", s.Note)
			}
		}
	}
	if !found {
		t.Error("the plan says nothing about the account at all; an operator cannot tell gvm considered it")
	}
}

// TestAnAdoptedInstanceWhoseAccountIsAbsentIsRefused.
//
// Inventing the account would produce a server that starts and cannot read its
// own save, because the files on disk belong to a uid that no longer maps to
// the running process.
func TestAnAdoptedInstanceWhoseAccountIsAbsentIsRefused(t *testing.T) {
	p, host, _ := planFor(t, "pz-main", "projectzomboid", func(pi *PlanInput) {
		pi.Inspect = newInspect() // a host that has never heard of pzserver
	})
	_, err := (&Applier{Host: host}).Apply(context.Background(), p, nil)
	if err == nil {
		t.Fatal("an apply proceeded for an adopted instance whose account does not exist")
	}
	if !strings.Contains(err.Error(), "pzserver") {
		t.Errorf("the refusal does not name the account: %v", err)
	}
}

// TestASecondApplyChangesNothingAndDoesNotReload.
//
// The most common apply is one that changes nothing, and it has to say so. A
// reload issued for a box where no unit file moved is a reload that makes the
// next real one indistinguishable from noise in the journal.
func TestASecondApplyChangesNothingAndDoesNotReload(t *testing.T) {
	// First apply against an empty host, to learn what it would write.
	first, _, _ := planFor(t, "valheim-1", "valheim", nil)

	// Now build a host that already has all of it.
	ins := newInspect().account("gvm-valheim-1")
	_ = ins
	for _, a := range first.Steps {
		switch v := a.(type) {
		case *FileAction:
			ins.put(v.Path, v.Body)
		case *DirAction:
			ins.mkdir(v.Path)
		case *TimerAction:
			ins.put(timerPathOf(v), v.Body)
		}
	}

	p, host, _ := planFor(t, "valheim-1", "valheim", func(pi *PlanInput) {
		pi.Inspect = ins
		// A day later, so the `# rendered` line differs and the comparison
		// has to be the one that ignores it.
		pi.Now = renderedAt.AddDate(0, 0, 1)
	})
	rep, err := (&Applier{Host: host, Inspect: ins}).Apply(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Describe() {
		switch s.Kind {
		case KindDropIn, KindFile, KindDir:
			if s.Change != Unchanged {
				t.Errorf("%s %s reported %q on a host that already matches", s.Kind, s.Object, s.Change)
			}
		}
	}
	if rep.Changed > rep.Unchanged {
		t.Errorf("a no-op apply reported %d changed and %d unchanged", rep.Changed, rep.Unchanged)
	}
	// The bytes handed to the host must be the bytes already there, or the
	// converged apply rewrites the drop-in and every timer -- a new mtime, a
	// NeedDaemonReload systemd had no reason for, and an audit row recording a
	// write for a step just described as Unchanged. Asserted against the host
	// rather than the description, because the description is the half that was
	// already right.
	// Every step's Body is what Do hands the host verbatim, and hostops decides
	// whether to touch the disk with an exact bytes.Equal. So on a converged
	// host each Body must equal what is already there -- otherwise the apply
	// rewrites the drop-in and every timer it owns: a new mtime, a
	// NeedDaemonReload systemd had no reason for, and an audit row recording a
	// write for a step just described as Unchanged. Asserted against the bytes
	// rather than against the description, because the description is the half
	// that was already right.
	for _, a := range p.Steps {
		var path string
		var body []byte
		switch v := a.(type) {
		case *FileAction:
			path, body = v.Path, v.Body
		case *TimerAction:
			path, body = timerPathOf(v), v.Body
		default:
			continue
		}
		cur, rerr := ins.ReadFile(path)
		if rerr != nil {
			t.Errorf("a converged apply plans a write to %s, which is not on the host: %v", path, rerr)
			continue
		}
		if !bytes.Equal(cur, body) {
			t.Errorf("a converged apply would write %s with bytes that differ from what is on "+
				"disk; the planner's equality and hostops' equality have to be the same decision", path)
		}
	}
	if reload := findReload(p); reload == nil {
		t.Fatal("the plan has no reload step at all")
	} else if reload.Needed {
		t.Error("the apply's daemon-reload is marked needed on a host where no unit file changed")
	}
}

// TestApplyVerifiesWhatItWrote.
//
// hostops skips a write whose bytes already match, an atomic rename can lose a
// race with an operator editing the same file, and a drop-in that is not what
// gvm believes it is governs a live server. Reading it back costs one open.
func TestApplyVerifiesWhatItWrote(t *testing.T) {
	p, host, _ := planFor(t, "valheim-1", "valheim", nil)

	// A host that accepts every write and then reports something else on
	// disk, which is what a second writer looks like from here.
	liar := newInspect().account("gvm-valheim-1")
	for _, a := range p.Steps {
		if fa, ok := a.(*FileAction); ok {
			liar.put(fa.Path, []byte("somebody else wrote this\n"))
		}
	}
	_, err := (&Applier{Host: host, Inspect: liar}).Apply(context.Background(), p, nil)
	if err == nil {
		t.Fatal("apply reported success for files that are not what it wrote")
	}
	if !strings.Contains(err.Error(), "something else is writing it") {
		t.Errorf("the failure does not say what happened: %v", err)
	}
}

// destroyStep is a destructive action that records whether it ran, so the
// tests below can prove it did NOT.
type destroyStep struct {
	ran bool
	ref *archive.Ref
}

func (d *destroyStep) Describe() Step {
	return Step{Kind: KindRemove, Change: Delete, Instance: "pz-main", Object: "the old world"}
}
func (d *destroyStep) Destructive() bool { return true }
func (d *destroyStep) Do(_ context.Context, ref *archive.Ref) error {
	d.ran, d.ref = true, ref
	return nil
}

// TestADestructivePlanNeedsAVerifiedArchiveAndAConfirmation.
//
// Both are checked BEFORE the first step runs, because the point of a plan is
// that everything which can refuse has refused before anything happens.
func TestADestructivePlanNeedsAVerifiedArchiveAndAConfirmation(t *testing.T) {
	newPlan := func(step *destroyStep) *Plan {
		p := &Plan{Instance: "pz-main"}
		p.Add(step)
		return p
	}
	yes := func(context.Context, *Plan) (bool, error) { return true, nil }
	no := func(context.Context, *Plan) (bool, error) { return false, nil }

	verified := &archive.Ref{
		Path:   "/var/backups/gvm/pz-main/pz-main-20260904-182211-preserve.tar.gz",
		SHA256: "d0", Instance: "pz-main", Class: archive.ClassPreserve,
		CreatedAt: renderedAt, VerifiedAt: renderedAt.Add(time.Minute),
	}
	unverified := *verified
	unverified.VerifiedAt = time.Time{}

	cases := []struct {
		name    string
		ref     *archive.Ref
		confirm func(context.Context, *Plan) (bool, error)
		wantErr error
	}{
		{"no archive at all", nil, yes, ErrNoArchive},
		{"an unverified archive", &unverified, yes, ErrNoArchive},
		{"no confirmation wired", verified, nil, ErrNotConfirmed},
		{"the operator said no", verified, no, ErrNotConfirmed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step := &destroyStep{}
			_, err := (&Applier{Host: &fakeHost{}, Confirm: c.confirm}).
				Apply(context.Background(), newPlan(step), c.ref)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if step.ran {
				t.Error("the destructive step ran anyway")
			}
		})
	}

	// And the happy path hands the SAME ref through to the action, because a
	// Do that received a different one would be authorised by an archive
	// nobody checked.
	step := &destroyStep{}
	if _, err := (&Applier{Host: &fakeHost{}, Confirm: yes}).
		Apply(context.Background(), newPlan(step), verified); err != nil {
		t.Fatal(err)
	}
	if !step.ran {
		t.Fatal("a confirmed destructive step with a verified archive did not run")
	}
	if step.ref != verified {
		t.Error("the action was handed a different archive than the one Apply checked")
	}
}

// TestRemoveRefusesAnUnverifiedRef even when called directly, because an
// Action's Do is reachable without going through Apply.
func TestRemoveRefusesAnUnverifiedRef(t *testing.T) {
	err := Remove(context.Background(), &archive.Ref{Path: "/tmp/x.tar.gz"}, nil)
	if !errors.Is(err, ErrNoArchive) {
		t.Fatalf("err = %v, want ErrNoArchive", err)
	}
	if err := Remove(context.Background(), nil, nil); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("err = %v, want ErrNoArchive", err)
	}
}

// TestAPlanClaimingToRemoveWithoutBeingDestructiveIsRefused.
//
// Validate reads the plan for deletions so that a caller CAN reason about one.
// A step that removes data while reporting Destructive()==false would run with
// no archive and no confirmation, which is the exact shape of the accident the
// archive rule exists to prevent.
func TestAPlanClaimingToRemoveWithoutBeingDestructiveIsRefused(t *testing.T) {
	p := &Plan{Instance: "pz-main"}
	p.Add(&brokenAction{step: Step{Kind: KindRemove, Change: Delete, Instance: "pz-main", Object: "world"}})
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "Destructive()==false") {
		t.Fatalf("err = %v, want a refusal naming the mismatch", err)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if strings.HasPrefix(s, want) {
			return i
		}
	}
	return -1
}

func findReload(p *Plan) *ReloadAction {
	for _, a := range p.Steps {
		if r, ok := a.(*ReloadAction); ok {
			return r
		}
	}
	return nil
}

// platformUnsupported is what a host with no systemd-run answers with. It is
// the real sentinel rather than a lookalike, because ProbeAction distinguishes
// "could not ask" from "asked and was refused" by errors.Is on exactly it.
func platformUnsupported() error { return platform.Unsupported("the write-probe (systemd-run)") }

// timerPathOf is where a planned timer would be written.
func timerPathOf(a *TimerAction) string { return instance.JobTimerPath(a.Instance, a.Job) }
