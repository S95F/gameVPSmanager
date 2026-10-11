package steamcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// D-32's five checks, and the rule that each failure is its OWN error.
//
// Distinct errors are the point: StateFlags 1062 means Steam knows files are
// missing, a short BytesDownloaded means the transfer stopped, a short
// BytesStaged means the bytes arrived and were never committed, zero
// appmanifests means nothing was ever installed here, and two means two
// installs are fighting over one directory. Each one sends the operator
// somewhere different, and a shared "verification failed" sends them nowhere.

const appID = 380870

var allVerifyErrors = []struct {
	name string
	err  error
}{
	{"ErrNoAppManifest", ErrNoAppManifest},
	{"ErrManyAppManifests", ErrManyAppManifests},
	{"ErrStateFlags", ErrStateFlags},
	{"ErrBytesDownloaded", ErrBytesDownloaded},
	{"ErrBytesStaged", ErrBytesStaged},
	{"ErrSentinel", ErrSentinel},
	{"ErrUndeclaredCheck", ErrUndeclaredCheck},
}

func allChecks() spec.Verify {
	return spec.Verify{AppManifest: true, ByteParity: true, SingleACF: true}
}

func pzSentinel() spec.Sentinel {
	return spec.Sentinel{Path: "start-server.sh", Executable: true}
}

// okTree is a healthy Project Zomboid install: one appmanifest with StateFlags
// 4 and matching byte pairs, and an executable sentinel.
func okTree(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		CanonicalACF(appID): &fstest.MapFile{Data: fixture(t, "appmanifest_ok.acf"), Mode: 0o644},
		"start-server.sh":   &fstest.MapFile{Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"ProjectZomboid64":  &fstest.MapFile{Data: []byte("ELF"), Mode: 0o755},
	}
}

func verifyTree(tree fs.FS) (*Report, error) {
	return Verify(VerifyRequest{
		Tree:     tree,
		AppID:    appID,
		Branch:   DefaultBranch,
		Sentinel: pzSentinel(),
		Verify:   allChecks(),
	})
}

// patchACF rewrites one quoted value, so a fixture can isolate one failure
// without a second near-identical file in testdata.
func patchACF(t *testing.T, acf []byte, key, value string) []byte {
	t.Helper()
	re := regexp.MustCompile(`("` + key + `")(\s+)"[^"]*"`)
	out := re.ReplaceAll(acf, []byte(`${1}${2}"`+value+`"`))
	if bytes.Equal(out, acf) {
		t.Fatalf("the fixture has no %q key to patch", key)
	}
	return out
}

func dropACFKey(t *testing.T, acf []byte, key string) []byte {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*"` + key + `"\s+"[^"]*"\s*$\n`)
	out := re.ReplaceAll(acf, nil)
	if bytes.Equal(out, acf) {
		t.Fatalf("the fixture has no %q key to drop", key)
	}
	return out
}

func TestVerifyPassesACleanInstall(t *testing.T) {
	report, err := verifyTree(okTree(t))
	if err != nil {
		t.Fatalf("a healthy install failed verification: %v\nproblems: %v", err, report.Problems)
	}
	if !report.Checked {
		t.Error("Checked is false after a successful verification, which reads as `nobody looked`")
	}
	if report.ACFPath != CanonicalACF(appID) {
		t.Errorf("ACFPath = %q", report.ACFPath)
	}
	if got := report.Version().ID; got != "18452901" {
		t.Errorf("version = %q", got)
	}
	if err := report.Sentinel.OK(); err != nil {
		t.Errorf("sentinel: %v", err)
	}
	if len(report.Problems) != 0 {
		t.Errorf("problems on a clean install: %v", report.Problems)
	}
	if report.Integrity().Failed() {
		t.Error("IntegrityResult.Failed on a clean install")
	}
}

func TestEachD32FailureIsItsOwnError(t *testing.T) {
	cases := []struct {
		name string
		want error
		tree func(t *testing.T) fstest.MapFS
		// says is a phrase the operator-facing problem has to contain, because
		// each of these means something different to act on.
		says string
	}{
		{
			name: "StateFlags is not 4",
			want: ErrStateFlags,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				tr[CanonicalACF(appID)] = &fstest.MapFile{
					Data: fixture(t, "appmanifest_1062.acf"), Mode: 0o644,
				}
				return tr
			},
			says: "files missing",
		},
		{
			name: "the download is short",
			want: ErrBytesDownloaded,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				tr[CanonicalACF(appID)] = &fstest.MapFile{
					Data: fixture(t, "appmanifest_bytes_mismatch.acf"), Mode: 0o644,
				}
				return tr
			},
			says: "download is short",
		},
		{
			name: "the staging step is short",
			want: ErrBytesStaged,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				tr[CanonicalACF(appID)] = &fstest.MapFile{
					Data: patchACF(t, fixture(t, "appmanifest_ok.acf"), "BytesStaged", "1900000000"),
					Mode: 0o644,
				}
				return tr
			},
			says: "staging step is short",
		},
		{
			name: "no appmanifest at all",
			want: ErrNoAppManifest,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				delete(tr, CanonicalACF(appID))
				return tr
			},
			says: "nothing was ever installed",
		},
		{
			name: "two appmanifests",
			want: ErrManyAppManifests,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				tr["steamapps/common/pz/"+appManifestName(appID)] = &fstest.MapFile{
					Data: fixture(t, "appmanifest_ok.acf"), Mode: 0o644,
				}
				return tr
			},
			says: "sharing one install directory",
		},
		{
			name: "the sentinel is gone",
			want: ErrSentinel,
			tree: func(t *testing.T) fstest.MapFS {
				tr := okTree(t)
				delete(tr, "start-server.sh")
				return tr
			},
			says: "start-server.sh is absent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := verifyTree(tc.tree(t))
			if err == nil {
				t.Fatal("verification passed")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// The distinctness claim, checked rather than asserted: no OTHER
			// failure may be reported for this tree.
			for _, other := range allVerifyErrors {
				if errors.Is(other.err, tc.want) {
					continue
				}
				if errors.Is(err, other.err) {
					t.Errorf("%s is also reported, so the two failures are not distinguishable "+
						"by error: %v", other.name, err)
				}
			}
			if !strings.Contains(strings.Join(report.Problems, " "), tc.says) {
				t.Errorf("the operator-facing problem does not say %q: %v", tc.says, report.Problems)
			}
			if !report.Integrity().Failed() && report.Integrity().Checked {
				t.Error("IntegrityResult reports no problems for a failed verification")
			}
		})
	}
}

// The repair path is the only remedy, and the message has to name it: D-32
// forbids auto-remediation because the only tool that would fix this is
// `validate`, which overwrites the operator's own edits.
func TestTheStateFlagsFailureNamesTheRepairCommand(t *testing.T) {
	tr := okTree(t)
	tr[CanonicalACF(appID)] = &fstest.MapFile{Data: fixture(t, "appmanifest_1062.acf"), Mode: 0o644}
	report, err := verifyTree(tr)
	if err == nil {
		t.Fatal("1062 passed")
	}
	joined := strings.Join(report.Problems, " ")
	if !strings.Contains(joined, "--repair") {
		t.Errorf("the problem does not tell the operator what to run: %v", report.Problems)
	}
}

func TestSentinelCases(t *testing.T) {
	cases := []struct {
		name     string
		sentinel spec.Sentinel
		mutate   func(tr fstest.MapFS)
		wantErr  bool
		says     string
	}{
		{
			name:     "present and executable",
			sentinel: pzSentinel(),
		},
		{
			name:     "present and not executable",
			sentinel: pzSentinel(),
			mutate: func(tr fstest.MapFS) {
				tr["start-server.sh"] = &fstest.MapFile{Data: []byte("#!/bin/sh\n"), Mode: 0o644}
			},
			wantErr: true,
			says:    "not executable",
		},
		{
			name:     "a directory where a file is declared",
			sentinel: spec.Sentinel{Path: "steamapps", Executable: false},
			wantErr:  true,
			says:     "is a directory",
		},
		{
			name:     "under the declared floor",
			sentinel: spec.Sentinel{Path: "start-server.sh", MinBytes: 1 << 20},
			wantErr:  true,
			says:     "partial download",
		},
		{
			name:     "no sentinel declared",
			sentinel: spec.Sentinel{},
			wantErr:  true,
			says:     "declares no install sentinel",
		},
		{
			name:     "a sentinel that escapes the tree",
			sentinel: spec.Sentinel{Path: "../../etc/passwd"},
			wantErr:  true,
			says:     "declares no install sentinel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := okTree(t)
			if tc.mutate != nil {
				tc.mutate(tr)
			}
			report, err := Verify(VerifyRequest{
				Tree: tr, AppID: appID, Sentinel: tc.sentinel, Verify: allChecks(),
			})
			switch {
			case tc.wantErr && !errors.Is(err, ErrSentinel):
				t.Fatalf("err = %v, want ErrSentinel", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("err = %v", err)
			}
			if tc.says != "" && !strings.Contains(strings.Join(report.Problems, " "), tc.says) {
				t.Errorf("problems = %v, want something about %q", report.Problems, tc.says)
			}
		})
	}
}

// A workshop item that happens to carry a copy of this app's manifest must not
// be reported as a second install, and steamcmd's own staging area is
// mid-flight by definition.
func TestVerifySkipsWorkshopAndStagingTrees(t *testing.T) {
	tr := okTree(t)
	tr["steamapps/workshop/content/108600/2822286426/"+appManifestName(appID)] =
		&fstest.MapFile{Data: fixture(t, "appmanifest_1062.acf"), Mode: 0o644}
	tr["steamapps/downloading/"+strings.TrimSuffix(appManifestName(appID), ".acf")+".acf"] =
		&fstest.MapFile{Data: fixture(t, "appmanifest_1062.acf"), Mode: 0o644}

	if _, err := verifyTree(tr); err != nil {
		t.Fatalf("a healthy install with workshop content failed verification: %v", err)
	}
}

// An absent number is not a passing check. This is the shape of bug that makes
// a verification step worse than none: it reports a verdict it never reached.
func TestAMissingBytePairIsAFailureNotAPass(t *testing.T) {
	for _, key := range []string{"BytesToDownload", "BytesDownloaded"} {
		t.Run(key, func(t *testing.T) {
			tr := okTree(t)
			tr[CanonicalACF(appID)] = &fstest.MapFile{
				Data: dropACFKey(t, fixture(t, "appmanifest_ok.acf"), key), Mode: 0o644,
			}
			_, err := verifyTree(tr)
			if !errors.Is(err, ErrBytesDownloaded) {
				t.Fatalf("err = %v, want ErrBytesDownloaded", err)
			}
		})
	}
	t.Run("BytesStaged", func(t *testing.T) {
		tr := okTree(t)
		tr[CanonicalACF(appID)] = &fstest.MapFile{
			Data: dropACFKey(t, fixture(t, "appmanifest_ok.acf"), "BytesStaged"), Mode: 0o644,
		}
		if _, err := verifyTree(tr); !errors.Is(err, ErrBytesStaged) {
			t.Fatalf("err = %v, want ErrBytesStaged", err)
		}
	})
	t.Run("StateFlags", func(t *testing.T) {
		tr := okTree(t)
		tr[CanonicalACF(appID)] = &fstest.MapFile{
			Data: dropACFKey(t, fixture(t, "appmanifest_ok.acf"), "StateFlags"), Mode: 0o644,
		}
		// No appid key would make it a different document; no StateFlags makes
		// it an appmanifest with no verdict in it.
		if _, err := verifyTree(tr); !errors.Is(err, ErrStateFlags) {
			t.Fatalf("err = %v, want ErrStateFlags", err)
		}
	})
}

// The 0-or-more-than-one guard is not optional: the later checks are undefined
// without exactly one file, so single_acf:false does not switch it off.
func TestTheAppManifestCountGuardIsNotOptional(t *testing.T) {
	tr := okTree(t)
	tr["steamapps/common/pz/"+appManifestName(appID)] = &fstest.MapFile{
		Data: fixture(t, "appmanifest_ok.acf"), Mode: 0o644,
	}
	_, err := Verify(VerifyRequest{
		Tree: tr, AppID: appID, Sentinel: pzSentinel(),
		Verify: spec.Verify{AppManifest: true, ByteParity: true, SingleACF: false},
	})
	if !errors.Is(err, ErrManyAppManifests) {
		t.Fatalf("err = %v; two appmanifests are a failure whatever single_acf says", err)
	}
}

// A mandatory check the pinned manifest did not declare is RECORDED as not
// declared -- so "passed", "failed" and "not asked" stay three distinguishable
// states -- and it FAILS, because the two surviving checks are exactly the
// "is the directory non-empty" standard D-32 exists to reject.
//
// The distinguishability lives in the Problem sentence and in the distinct
// error, never in a nil: with verify:{} the only remaining evidence is the
// single-ACF guard and the sentinel, and a directory holding 40 GB of a 65 GB
// download satisfies both.
func TestUndeclaredChecksFailClosed(t *testing.T) {
	tr := okTree(t)
	tr[CanonicalACF(appID)] = &fstest.MapFile{Data: fixture(t, "appmanifest_1062.acf"), Mode: 0o644}
	report, err := Verify(VerifyRequest{
		Tree: tr, AppID: appID, Sentinel: pzSentinel(), Verify: spec.Verify{},
	})
	if !errors.Is(err, ErrUndeclaredCheck) {
		t.Fatalf("err = %v, want ErrUndeclaredCheck; an undeclared mandatory check that returns "+
			"nil makes Apply print `installed and verified` for a tree Steam itself calls "+
			"incomplete", err)
	}
	joined := strings.Join(report.Problems, " ")
	for _, want := range []string{
		"StateFlags was not checked", "byte pairs were not checked",
		// The remedy differs from every other D-32 failure: the tree may be
		// fine and it is the PIN that cannot be trusted.
		"gvm apply",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems = %v, missing %q", report.Problems, want)
		}
	}
	// The facts are still worth recording: a failure is not a reason to stop
	// reporting what was read.
	if ir := report.Integrity(); !ir.Checked || !ir.Failed() {
		t.Errorf("integrity = %+v, want Checked and Failed", ir)
	}
}

// And the same pin is refused at the driver's door, before 65 GB is
// downloaded, for the same reason steamSpec already refuses an undeclared
// sentinel.
func TestAPinWithoutTheMandatoryVerifyFlagsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verify spec.Verify
	}{
		{"neither flag", spec.Verify{}},
		{"no appmanifest", spec.Verify{ByteParity: true}},
		{"no byte_parity", spec.Verify{AppManifest: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			h.ctx.Manifest.Spec.Provider.SteamCMD.Verify = tc.verify
			for _, in := range []provider.Intent{
				provider.IntentInstall, provider.IntentUpdate, provider.IntentVerify,
			} {
				_, err := h.driver.Plan(context.Background(), h.ctx, in)
				if !errors.Is(err, ErrManifest) {
					t.Fatalf("Plan(%s) err = %v, want ErrManifest", in, err)
				}
				if !strings.Contains(err.Error(), "gvm apply") {
					t.Errorf("the refusal does not say how to re-validate the pin: %v", err)
				}
			}
			if _, err := h.driver.Inspect(context.Background(), h.ctx); !errors.Is(err, ErrManifest) {
				t.Errorf("Inspect err = %v, want ErrManifest", err)
			}
			if _, err := h.driver.Observe(context.Background(), h.ctx); !errors.Is(err, ErrManifest) {
				t.Errorf("Observe err = %v, want ErrManifest", err)
			}
			if len(h.fake.Runs()) != 0 {
				t.Error("the refusal still ran steamcmd")
			}
		})
	}
}

// Steam's own marker for "an update was started and did not land".
//
// TargetBuildID is written when an update begins and left equal to buildid when
// one finishes, so an inequality is Steam saying so itself -- and it is read on
// every path, including the doctor run, where there is no WantBuild to compare
// against.
func TestAnOutstandingTargetBuildIsAFailure(t *testing.T) {
	tr := okTree(t)
	tr[CanonicalACF(appID)] = &fstest.MapFile{
		Data: patchACF(t, fixture(t, "appmanifest_ok.acf"), "TargetBuildID", "18461233"),
		Mode: 0o644,
	}
	report, err := verifyTree(tr)
	if !errors.Is(err, ErrWrongBuild) {
		t.Fatalf("err = %v, want ErrWrongBuild", err)
	}
	joined := strings.Join(report.Problems, " ")
	for _, want := range []string{"18452901", "18461233"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the problem does not name build %s: %v", want, report.Problems)
		}
	}
	if got := report.Integrity().Facts["target_buildid"]; got != "18461233" {
		t.Errorf("facts[target_buildid] = %q; the job record should carry what was compared", got)
	}
	// StateFlags 4 and both byte pairs match, so this failure is reported by
	// nothing else: the other D-32 checks all pass on this tree.
	for _, other := range []error{ErrStateFlags, ErrBytesDownloaded, ErrBytesStaged} {
		if errors.Is(err, other) {
			t.Errorf("%v is also reported, so the two failures are not distinguishable", other)
		}
	}
}

func TestTheWantBuildComparison(t *testing.T) {
	cases := []struct {
		name    string
		want    string
		wantErr bool
		says    string
	}{
		{name: "not asserted", want: ""},
		{name: "the build the plan asked for", want: "18452901"},
		{
			name: "a build that never arrived", want: "18461233", wantErr: true,
			says: "exited without reporting an error",
		},
		{
			// Steam published again mid-download. Reported, not failed.
			name: "a build newer than the plan's", want: "18400000", says: "newer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Verify(VerifyRequest{
				Tree: okTree(t), AppID: appID, Branch: DefaultBranch,
				Sentinel: pzSentinel(), Verify: allChecks(), WantBuild: tc.want,
			})
			switch {
			case tc.wantErr && !errors.Is(err, ErrWrongBuild):
				t.Fatalf("err = %v, want ErrWrongBuild", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("err = %v", err)
			}
			if tc.says != "" && !strings.Contains(strings.Join(report.Problems, " "), tc.says) {
				t.Errorf("problems = %v, want something about %q", report.Problems, tc.says)
			}
			if tc.want != "" && report.Integrity().Facts["want_buildid"] != tc.want {
				t.Errorf("facts[want_buildid] = %q, want %q",
					report.Integrity().Facts["want_buildid"], tc.want)
			}
		})
	}
}

func TestIntegrityFactsCarryTheNumbers(t *testing.T) {
	report, err := verifyTree(okTree(t))
	if err != nil {
		t.Fatal(err)
	}
	facts := report.Integrity().Facts
	for key, want := range map[string]string{
		"state_flags":      "4 (fully installed)",
		"buildid":          "18452901",
		"bytes_downloaded": "1417183232/1417183232",
		"bytes_staged":     "3461823232/3461823232",
		"appmanifest":      CanonicalACF(appID),
		"branch":           DefaultBranch,
	} {
		if got := facts[key]; got != want {
			t.Errorf("facts[%q] = %q, want %q", key, got, want)
		}
	}
	// Hand edits are not derivable from a Steam install, and saying so is not
	// the same as reporting none.
	if !strings.Contains(facts["hand_edits"], "validate") {
		t.Errorf("facts[hand_edits] = %q", facts["hand_edits"])
	}
}

func TestVerifyWithNoTreeIsAFailureThatSaysSo(t *testing.T) {
	report, err := Verify(VerifyRequest{AppID: appID, Sentinel: pzSentinel(), Verify: allChecks()})
	if !errors.Is(err, ErrTreeUnreadable) {
		t.Fatalf("err = %v, want ErrTreeUnreadable", err)
	}
	if report.Integrity().Checked {
		t.Error("Checked is true although nothing was read")
	}
	if report.Integrity().Reason == "" {
		t.Error("Checked:false with no Reason is exactly the ambiguity the seam forbids")
	}
}

func TestACopiedAppManifestIsRefused(t *testing.T) {
	tr := okTree(t)
	tr[CanonicalACF(appID)] = &fstest.MapFile{
		Data: patchACF(t, fixture(t, "appmanifest_ok.acf"), "appid", "896660"), Mode: 0o644,
	}
	report, err := verifyTree(tr)
	if !errors.Is(err, ErrNoAppManifest) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(report.Problems, " "), "copied here") {
		t.Errorf("problems = %v", report.Problems)
	}
}

// A forged appmanifest must not reach the sentence the operator reads.
//
// The install tree is owned by the instance account BY DESIGN (hostops gives
// DirInstall OwnedByInstance, and ARCHITECTURE 18.1 lists "its install tree" as
// what a compromised workshop item yields), so these bytes are third-party
// input on the one path whose whole premise is that what the tool says about
// itself is not evidence. The lexer additionally materialises Valve's \n, \r
// and \t escapes into real control bytes, bounded only by a 1 MB token, so
// "18452901\r\n[ OK ] ... \x1b[2K" is a value a mod can write and gvm would
// print. CR, LF, NUL and ANSI in an operator-facing line is the exact class
// socket.SanitizeActor, lifecycle.sanitiseReason, platform.sanitizeStatus,
// control/args.go, jobs.checkUnitValue and runscript.checkToken all already
// refuse, each for this reason.
func hostileTree(t *testing.T) fstest.MapFS {
	t.Helper()
	tr := okTree(t)
	tr[CanonicalACF(appID)] = &fstest.MapFile{
		Data: fixture(t, "appmanifest_hostile.acf"), Mode: 0o644,
	}
	return tr
}

// controlBytes reports the offending bytes in s, which is what an assertion
// message has to name to be actionable.
func controlBytes(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f {
			out = append(out, fmt.Sprintf("%#x at %d", b, i))
		}
	}
	return out
}

func TestAHostileAppManifestNeverReachesTheOperatorsLine(t *testing.T) {
	report, err := verifyTree(hostileTree(t))

	// (b) first: a buildid that is not a build id means something other than
	// Steam wrote this file, which is precisely what D-32 exists to surface.
	if !errors.Is(err, ErrBuildID) {
		t.Fatalf("err = %v, want ErrBuildID", err)
	}
	if !report.Integrity().Failed() {
		t.Error("the forgery is not reported as a failure")
	}

	// (a) nothing carrying a control byte escapes.
	surfaces := map[string]string{
		"Version().ID":    report.Version().ID,
		"Version().Label": report.Version().Label,
		"verifiedSummary": verifiedSummary(report),
		"Err()":           err.Error(),
	}
	// Per element, not joined: the separator is a control byte and a joined
	// string would fail this assertion for the test's own reason.
	for i, s := range verifySteps(report) {
		surfaces[fmt.Sprintf("verifySteps[%d]", i)] = s
	}
	for i, s := range report.Problems {
		surfaces[fmt.Sprintf("Problems[%d]", i)] = s
	}
	for key, value := range report.Integrity().Facts {
		surfaces["Facts["+key+"]"] = value
	}
	for name, s := range surfaces {
		if bad := controlBytes(s); len(bad) > 0 {
			t.Errorf("%s carries control bytes %v: %q", name, bad, s)
		}
	}
	// And the raw value is not smuggled through as a long run of plausible
	// text either: the label is bounded.
	if n := len(report.Version().Label); n > 160 {
		t.Errorf("the label is %d bytes; a 1 MB token would scroll the real verdict out of a "+
			"terminal", n)
	}
	// The fixed sentence, never the bytes.
	if !strings.Contains(strings.Join(report.Problems, " "), "not a plausible build id") {
		t.Errorf("problems = %v", report.Problems)
	}
	if strings.Contains(strings.Join(report.Problems, " "), "18452901") {
		t.Error("the refused value is quoted back into the problem, which is what the fixed " +
			"sentence exists to avoid")
	}
}

// (c) the three fixtures that are not forged parse exactly as they did.
func TestValidationDoesNotTouchAHealthyAppManifest(t *testing.T) {
	for name, want := range map[string]struct{ build, target, label string }{
		"appmanifest_ok.acf": {
			"18452901", "18452901", "Project Zomboid Dedicated Server",
		},
		"appmanifest_1062.acf": {
			"18452901", "18461233", "Project Zomboid Dedicated Server",
		},
		"appmanifest_bytes_mismatch.acf": {
			"18461233", "18461233", "Project Zomboid Dedicated Server",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := parseFixture(t, name)
			if m.BuildID != want.build || m.TargetBuildID != want.target || m.Name != want.label {
				t.Errorf("buildid %q / target %q / name %q, want %q / %q / %q",
					m.BuildID, m.TargetBuildID, m.Name, want.build, want.target, want.label)
			}
			if m.BuildIDSuspect {
				t.Error("a Steam-written appmanifest was refused")
			}
		})
	}
}

// The same forgery through the driver: Observe, Inspect and the verify intent's
// Apply all compose an operator-facing sentence from these fields.
func TestTheDriverNeverPrintsAForgedAppManifest(t *testing.T) {
	h := newHarness(t, true)
	if err := h.fake.Seed(h.installDir, pzApp); err != nil {
		t.Fatal(err)
	}
	acf := filepath.Join(h.installDir, filepath.FromSlash(CanonicalACF(pzApp)))
	if err := os.WriteFile(acf, fixture(t, "appmanifest_hostile.acf"), 0o644); err != nil {
		t.Fatal(err)
	}

	obs, err := h.driver.Observe(context.Background(), h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Healthy {
		t.Error("a forged appmanifest was observed as healthy")
	}
	ins, err := h.driver.Inspect(context.Background(), h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ins.Integrity.Failed() {
		t.Error("Inspect did not report the forgery")
	}
	// applyVerify's note is the whole output of `gvm doctor`'s verify step, and
	// it is the branch a forgery survives unaltered: nothing runs app_update to
	// overwrite it.
	applied, err := h.apply(h.plan(provider.IntentVerify))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applied.Note, "FAILED verification") {
		t.Errorf("note = %q", applied.Note)
	}
	surfaces := map[string]string{
		"Observe.Detail":  obs.Detail,
		"Observe.Version": obs.Version.ID + "|" + obs.Version.Label,
		"Inspect.Version": ins.Version.ID + "|" + ins.Version.Label,
		"Applied.Note":    applied.Note,
		"Applied.Version": applied.Version.ID + "|" + applied.Version.Label,
	}
	// And the plan refuses outright. The local buildid flows into CheckBuild
	// and out through Summary into Plan.Reason, which is what an operator reads
	// before authorizing a download; an empty string there would answer
	// BuildNotInstalled and turn the forgery into a plan to reinstall over a
	// live world.
	_, planErr := h.driver.Plan(context.Background(), h.ctx, provider.IntentUpdate)
	if !errors.Is(planErr, ErrBuildID) {
		t.Fatalf("Plan err = %v, want ErrBuildID", planErr)
	}
	surfaces["Plan err"] = planErr.Error()

	for i, s := range ins.Integrity.Problems {
		surfaces[fmt.Sprintf("Inspect.Problems[%d]", i)] = s
	}
	for i, s := range applied.Steps {
		surfaces[fmt.Sprintf("Applied.Steps[%d]", i)] = s
	}
	for key, value := range ins.Integrity.Facts {
		surfaces["Inspect.Facts["+key+"]"] = value
	}
	for name, s := range surfaces {
		if bad := controlBytes(s); len(bad) > 0 {
			t.Errorf("%s carries control bytes %v: %q", name, bad, s)
		}
	}
}
