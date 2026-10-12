package update

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// newRepair is a pipeline whose manifest declares validate_on: repair, with a
// shared ordering recorder across the archive and the installer.
func newRepair(t *testing.T, mutate func(*Config, *fakeInstaller, *fakeArchives)) (
	*Pipeline, *fakeInstaller, *fakeArchives, *recorder,
) {
	t.Helper()
	rec := &recorder{}
	inst := &fakeInstaller{
		rec: rec, local: "18452901", remote: "18452901", repairWrites: true,
		insp: &provider.Inspection{
			Present:   true,
			HandEdits: []string{"start-server.sh", "ProjectZomboid64.json"},
			Unmanaged: []string{"mods-local/README.txt"},
			Integrity: provider.IntegrityResult{Checked: true},
		},
	}
	arch := &fakeArchives{rec: rec}
	cfg := Config{
		Instance:       testInstance,
		ManagedInstall: true,
		Installer:      inst,
		Content:        &fakeContent{},
		Archives:       arch,
		Disk:           roomyDisk,
		Log:            discardLogger(),
		Notes:          func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&cfg, inst, arch)
	}
	return New(cfg), inst, arch, rec
}

// TestARepairMustBeAskedFor.
//
// `validate` is not something a code path gets to decide would help. update.sh
// ran it every night; D-32 removed it from the nightly, and phase 6's response
// to a failed verification is NEEDS_REPAIR plus a message -- never an
// escalation to this.
func TestARepairMustBeAskedFor(t *testing.T) {
	p, inst, arch, _ := newRepair(t, nil)
	rep, err := p.Repair(context.Background(), RepairRequest{})
	if !errors.Is(err, ErrRepairNotRequested) {
		t.Fatalf("err = %v, want ErrRepairNotRequested", err)
	}
	if !strings.Contains(err.Error(), "gvm update pz-main --repair") {
		t.Errorf("the refusal must name the command: %v", err)
	}
	if rep == nil {
		t.Fatal("no report")
	}
	if len(inst.planned()) != 0 || len(arch.refs) != 0 {
		t.Error("an unrequested repair planned or archived something")
	}
}

// TestARepairRefusesAnAdoptedInstall, because those files predate gvm and may
// be the operator's own.
func TestARepairRefusesAnAdoptedInstall(t *testing.T) {
	p, inst, arch, _ := newRepair(t, func(c *Config, _ *fakeInstaller, _ *fakeArchives) {
		c.ManagedInstall = false
	})
	_, err := p.Repair(context.Background(), RepairRequest{Confirmed: true})
	if !errors.Is(err, ErrAdopted) {
		t.Fatalf("err = %v, want ErrAdopted", err)
	}
	if len(inst.planned()) != 0 || len(arch.refs) != 0 {
		t.Error("an adopted repair planned or archived something")
	}
}

// TestARepairNamesTheFilesItIsAboutToRisk.
//
// Point 3 of the repair contract, and the reason the inspection happens before
// the archive and before the plan: the list is the only part of a repair an
// operator can act on, and a dry run has to be able to produce it having
// touched nothing.
func TestARepairNamesTheFilesItIsAboutToRisk(t *testing.T) {
	p, inst, arch, _ := newRepair(t, nil)
	rep, err := p.Repair(context.Background(), RepairRequest{Confirmed: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AtRiskKnown {
		t.Error("the provider reported hand edits and the report says nobody looked")
	}
	want := map[string]bool{
		"start-server.sh": true, "ProjectZomboid64.json": true,
		"mods-local/README.txt": true,
	}
	if len(rep.AtRisk) != len(want) {
		t.Fatalf("at risk = %v, want %d files", rep.AtRisk, len(want))
	}
	for _, f := range rep.AtRisk {
		if !want[f] {
			t.Errorf("unexpected file %q", f)
		}
	}
	// A dry run touches nothing at all -- not even the archive.
	if len(arch.refs) != 0 {
		t.Errorf("a dry run took %d archive(s)", len(arch.refs))
	}
	if len(inst.applied()) != 0 {
		t.Error("a dry run applied something")
	}
	if !rep.DryRun {
		t.Error("the report does not say it was a dry run")
	}
}

// TestNobodyLookedIsNotTheSameAsNothingIsAtRisk.
//
// A provider that keeps no record of what it wrote reports no hand edits, and
// an empty list from such a provider must not read as "safe". On this path the
// difference decides whether an operator proceeds.
func TestNobodyLookedIsNotTheSameAsNothingIsAtRisk(t *testing.T) {
	p, _, _, _ := newRepair(t, func(_ *Config, i *fakeInstaller, _ *fakeArchives) {
		i.insp = &provider.Inspection{
			Present:   true,
			Integrity: provider.IntegrityResult{Checked: false, Reason: "no record is kept"},
		}
	})
	rep, err := p.Repair(context.Background(), RepairRequest{Confirmed: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.AtRiskKnown {
		t.Error("a provider that performs no integrity check was reported as having looked")
	}
	if len(rep.Steps) == 0 || !strings.Contains(strings.Join(rep.Steps, " "), "keeps no record") {
		t.Errorf("the report does not say that nobody looked: %v", rep.Steps)
	}
}

// TestThePreserveArchiveIsWrittenAndVerifiedBeforeValidateRuns.
//
// The ordering invariant, borrowed from rebuild.sh and applied to the one other
// place in gvm that destroys something an operator may care about. The recorder
// is shared between the two fakes precisely so this is an assertion about
// ORDER rather than about two independent counters.
func TestThePreserveArchiveIsWrittenAndVerifiedBeforeValidateRuns(t *testing.T) {
	p, _, arch, rec := newRepair(t, nil)
	rep, err := p.Repair(context.Background(), RepairRequest{Confirmed: true, Reason: "pre-repair"})
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	events := rec.all()
	snap := rec.index("snapshot:preserve")
	ver := rec.index("verify")
	app := rec.index("apply:repair")
	switch {
	case snap < 0:
		t.Fatalf("no preserve archive was taken: %v", events)
	case ver < 0:
		t.Fatalf("the archive was never verified: %v", events)
	case app < 0:
		t.Fatalf("the repair was never applied: %v", events)
	case !(snap < ver && ver < app):
		t.Fatalf("order = %v; the archive must be written AND verified before `validate` "+
			"overwrites anything", events)
	}
	if len(arch.refs) != 1 || arch.refs[0].Class != archive.ClassPreserve {
		t.Errorf("the repair archive is not class=preserve: %+v", arch.refs)
	}
	if !rep.Verified {
		t.Error("the post-repair D-32 check did not pass")
	}
	if rep.Archive == nil || !rep.Archive.Verified() {
		t.Error("the report carries no verified archive")
	}
}

// TestAFailedArchiveAbortsBeforeAnythingIsOverwritten -- all three ways it can
// fail, and in every one of them `validate` never runs.
func TestAFailedArchiveAbortsBeforeAnythingIsOverwritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeArchives)
		want  error
	}{
		{"the write failed", func(a *fakeArchives) {
			a.snapErr = errors.New("no space left on device")
		}, nil},
		{"the engine returned no ref", func(a *fakeArchives) { a.noRef = true }, ErrConfig},
		{"verification failed", func(a *fakeArchives) {
			a.verifyErr = errors.New("member digest mismatch")
		}, nil},
		{"verified and VerifiedAt is zero", func(a *fakeArchives) {
			a.skipVerifyStamp = true
		}, ErrConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, inst, _, _ := newRepair(t, func(_ *Config, _ *fakeInstaller, a *fakeArchives) {
				tc.setup(a)
			})
			_, err := p.Repair(context.Background(), RepairRequest{Confirmed: true})
			if err == nil {
				t.Fatal("the repair proceeded without a receipt")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "untouched") {
				t.Errorf("the refusal does not say the install is untouched: %v", err)
			}
			for _, in := range inst.applied() {
				if in == provider.IntentRepair {
					t.Error("`validate` ran after a failed archive")
				}
			}
		})
	}
}

// TestARepairPlanThatWritesNothingIsRefused.
//
// A manifest whose validate_on omits `repair` has declared that this game is
// not repaired that way. Applying the plan anyway would be a full re-download
// reported as a repair, and the honest answer names the missing declaration.
func TestARepairPlanThatWritesNothingIsRefused(t *testing.T) {
	p, inst, _, _ := newRepair(t, func(_ *Config, i *fakeInstaller, _ *fakeArchives) {
		i.repairWrites = false
	})
	_, err := p.Repair(context.Background(), RepairRequest{Confirmed: true})
	if !errors.Is(err, ErrRepairNoValidate) {
		t.Fatalf("err = %v, want ErrRepairNoValidate", err)
	}
	for _, in := range inst.applied() {
		if in == provider.IntentRepair {
			t.Error("a plan that writes nothing was applied anyway")
		}
	}
}

// TestARepairWithNoArchiveEngineIsRefusedRatherThanWarned.
//
// Unlike the pre-update backup -- update.sh's `|| warn "Backup failed -
// continuing anyway."` -- there is no "continuing anyway" here. The thing about
// to happen is an overwrite of files the operator may have edited, and the
// archive is the only way back.
func TestARepairWithNoArchiveEngineIsRefusedRatherThanWarned(t *testing.T) {
	p, inst, _, _ := newRepair(t, func(c *Config, _ *fakeInstaller, _ *fakeArchives) {
		c.Archives = nil
	})
	_, err := p.Repair(context.Background(), RepairRequest{Confirmed: true})
	if !errors.Is(err, ErrRepairNoArchive) {
		t.Fatalf("err = %v, want ErrRepairNoArchive", err)
	}
	if len(inst.applied()) != 0 {
		t.Error("a repair with no archive engine applied something")
	}
}

// TestRepairIsTheOnlyFileThatNamesTheRepairIntent.
//
// "`gvm update <n> --repair` is the only path that emits `validate`" is a claim
// about the whole package, so it is asserted over the whole package: the repair
// intent -- the only intent a steamcmd runscript will carry `validate` for --
// may be named in repair.go and nowhere else.
//
// It parses the files rather than grepping them, so a mention inside a comment
// or a string does not trip it and a mention in real code cannot hide behind
// one. The companion assertion is the other half: if repair.go stops naming it,
// this test is passing for the wrong reason.
func TestRepairIsTheOnlyFileThatNamesTheRepairIntent(t *testing.T) {
	const allowed = "repair.go"
	names := map[string]bool{"IntentRepair": true, "IntentInstall": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sawInRepair := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !names[sel.Sel.Name] {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "provider" {
				return true
			}
			if name == allowed {
				if sel.Sel.Name == "IntentRepair" {
					sawInRepair = true
				}
				return true
			}
			t.Errorf("%s names provider.%s; `validate` has exactly one path in gvm and it "+
				"is %s", name, sel.Sel.Name, allowed)
			return true
		})
	}
	if !sawInRepair {
		t.Errorf("%s no longer names provider.IntentRepair, so this test proves nothing", allowed)
	}
}
