package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// TestChooseIsTheDecisionTableFromSectionThirteenTwo.
//
// The table is pure, so the whole of it is exercised on the development
// machine -- which matters because the preference order has a subtlety in it:
// btrfs is chosen over reflink when the world paths are subvolume roots,
// because a subvolume snapshot is ONE atomic operation over the tree while a
// per-file clone walk is a sequence of them.
func TestChooseIsTheDecisionTableFromSectionThirteenTwo(t *testing.T) {
	cases := []struct {
		name  string
		cap   Capability
		allow []Method
		want  Method
		why   string
	}{
		{
			name: "btrfs with subvolume roots",
			cap:  Capability{FSType: "btrfs", Btrfs: true, Subvolumes: true, Reflink: true},
			want: MethodBtrfs, why: "atomic snapshot",
		},
		{
			name: "xfs with reflink",
			cap:  Capability{FSType: "xfs", Reflink: true},
			want: MethodReflink, why: "FICLONE",
		},
		{
			name: "btrfs whose world paths are not subvolumes and cannot clone",
			cap:  Capability{FSType: "btrfs", Btrfs: true},
			want: MethodDirect, why: "not subvolume roots",
		},
		{
			name: "plain ext4",
			cap:  Capability{FSType: "ext2/3/4"},
			want: MethodDirect, why: "neither reflink nor btrfs",
		},
		{
			name: "a probe that answered nothing",
			cap:  Capability{},
			want: MethodDirect, why: "unidentified filesystem",
		},
		{
			// An operator who has had a bad experience with their kernel's
			// FICLONE pins the instance to direct. The capability is there and
			// is deliberately not used.
			name:  "reflink available but not permitted",
			cap:   Capability{FSType: "xfs", Reflink: true},
			allow: []Method{MethodDirect},
			want:  MethodDirect, why: "neither reflink nor btrfs",
		},
		{
			// Every allowed method is unavailable and direct is not allowed.
			// Direct happens anyway, with the contradiction named: refusing to
			// back up because a preference could not be honoured would turn a
			// configuration opinion into a gap in history.
			name:  "only reflink allowed and it is unavailable",
			cap:   Capability{FSType: "ext2/3/4"},
			allow: []Method{MethodReflink},
			want:  MethodDirect, why: "a backup not taken is worse than a slow one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Choose(tc.cap, tc.allow)
			if got != tc.want {
				t.Fatalf("Choose = %q, want %q (%s)", got, tc.want, why)
			}
			if !strings.Contains(why, tc.why) {
				t.Errorf("reason %q does not contain %q", why, tc.why)
			}
			if !got.Valid() {
				t.Errorf("Choose returned an invalid method %q", got)
			}
		})
	}
}

// TestTheOrderingRuleIsAPropertyOfTheMethod.
//
// The two families differ in exactly one way and everything else keys off it:
// whether the archive is written from a clone (so after the thaw) or from the
// live tree (so inside the freeze). The lease budget, the stall figure and the
// step order all read this one bool.
func TestTheOrderingRuleIsAPropertyOfTheMethod(t *testing.T) {
	for _, m := range []Method{MethodReflink, MethodBtrfs} {
		if !m.ThawBeforeArchive() {
			t.Errorf("%s archives from a clone, so the thaw comes first", m)
		}
	}
	if MethodDirect.ThawBeforeArchive() {
		t.Error("a direct snapshot has nothing to archive from but the live tree, so the freeze " +
			"covers the whole archive")
	}
	if Method("tar").Valid() {
		t.Error("an unknown method must not validate")
	}
	if len(Methods()) != 3 {
		t.Errorf("Methods() = %v, want three", Methods())
	}
}

// TestTheStallThresholdIsTheOneTheDashboardRenders.
//
// Section 13.2: "the dashboard renders it in red above 30 s -- because on an
// 8 GB ext4 VPS the operator deserves to know the nightly backup pauses their
// world for 74 seconds". The threshold lives with the measurement so a
// template cannot drift from it.
func TestTheStallThresholdIsTheOneTheDashboardRenders(t *testing.T) {
	if StallRed != 30*time.Second {
		t.Fatalf("StallRed = %v, want 30s", StallRed)
	}
	if (SnapshotResult{StallMs: 74210}).Red() != true {
		t.Error("section 13.2's own 74210ms example is not red")
	}
	if (SnapshotResult{StallMs: 74}).Red() {
		t.Error("74ms -- a reflink clone -- was reported as red")
	}
	if (SnapshotResult{StallMs: StallRed.Milliseconds()}).Red() {
		t.Error("exactly the threshold is not ABOVE it")
	}
}

// TestSweepSnapDirOnlyRemovesCloneDirectoriesThisPackageWrote.
//
// The startup half of "a defer plus a startup sweep". The defer covers an
// error and a panic; it does not cover SIGKILL, and a 2.4 GB clone left behind
// once a week fills a VPS in a month.
//
// What it must never do is remove something it did not write. A sweep that
// cleared everything under its directory would be one `snap_dir` typo away
// from deleting a world, so the name has to match the pattern AND parse as a
// timestamp -- exactly the rule retention applies to an unmanaged tarball.
func TestSweepSnapDirOnlyRemovesCloneDirectoriesThisPackageWrote(t *testing.T) {
	f := newFS()
	const dir = "/var/lib/gvm/instances/pz-main/snap"
	old := ClonePathFor(dir, "pz-main", testNow.Add(-4*time.Hour))
	fresh := ClonePathFor(dir, "pz-main", testNow.Add(-time.Minute))
	for _, p := range []string{
		old,
		fresh,
		dir + "/pz-main-not-a-timestamp.snap",  // our prefix, a name we did not write
		dir + "/other-20260904-040003.snap",    // another instance's
		dir + "/pz-main-20260904-040003.other", // not a clone
	} {
		if err := f.MkdirAll(p, SnapDirMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.WriteFile(dir+"/stray.txt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepSnapDir(f, dir, "pz-main", testNow, DefaultSnapGrace)
	if err != nil {
		t.Fatalf("SweepSnapDir: %v", err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed %v, want only the four-hour-old clone %s", removed, old)
	}
	if !f.has(fresh) {
		t.Error("a clone newer than the grace period was removed; another job may be holding it")
	}
	for _, keep := range []string{
		dir + "/pz-main-not-a-timestamp.snap",
		dir + "/other-20260904-040003.snap",
		dir + "/pz-main-20260904-040003.other",
		dir + "/stray.txt",
	} {
		if !f.has(keep) {
			t.Errorf("%s was removed and this package did not write it", keep)
		}
	}
}

// TestSweepSnapDirIsSilentWhenThereIsNoSnapDirectory.
//
// The normal state: the directory is created by the first clone and removed
// with the instance, so a host that has only ever taken direct snapshots has
// none. A sweep that errored on it would make every preflight on ext4 log a
// failure.
func TestSweepSnapDirIsSilentWhenThereIsNoSnapDirectory(t *testing.T) {
	removed, err := SweepSnapDir(newFS(), "/var/lib/gvm/instances/pz-main/snap", "pz-main",
		testNow, DefaultSnapGrace)
	if err != nil || len(removed) != 0 {
		t.Fatalf("SweepSnapDir = (%v, %v), want nothing", removed, err)
	}
}

// TestClonePathCarriesTheInstanceAndTheStamp: the sweep ages a leftover clone
// from its NAME rather than from a stat, and two runs must not collide on a
// directory one of them is about to delete.
func TestClonePathCarriesTheInstanceAndTheStamp(t *testing.T) {
	a := ClonePathFor("/snap", "pz-main", testNow)
	b := ClonePathFor("/snap", "pz-main", testNow.Add(time.Second))
	if a == b {
		t.Fatalf("two runs one second apart produced the same clone path %s", a)
	}
	if !strings.HasPrefix(a, "/snap/pz-main-") || !strings.HasSuffix(a, ".snap") {
		t.Errorf("clone path %q does not match the pattern the sweep recognises", a)
	}
}

// TestProbeAndCloneAreHonestOffLinux.
//
// The stub refuses rather than returning an empty Capability and no error, and
// that distinction is load-bearing: an engine that could not tell "this box has
// ext4" from "nobody asked" would hide a real probe failure on a real VPS
// behind the same silence. The engine's handling of the refusal -- record it,
// choose direct, carry on -- is what the Windows tests exercise, and it is the
// path most VPS images take anyway.
func TestProbeAndCloneAreHonestOffLinux(t *testing.T) {
	if isLinux() {
		t.Skip("on Linux the probe asks the kernel; this is the stub's contract")
	}
	capab, err := Probe([]string{"/srv/gvm/pz-main/data"}, "/var/lib/gvm/instances/pz-main/snap")
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Fatalf("Probe error = %v, want platform.ErrNotSupported", err)
	}
	if capab.Reflink || capab.Btrfs || capab.Subvolumes {
		t.Errorf("the stub claimed a capability: %+v", capab)
	}
	if capab.Note == "" {
		t.Error("the stub gave no reason, so nothing could be recorded in the job log")
	}
	// And the refused probe still produces a usable decision.
	if m, _ := Choose(capab, nil); m != MethodDirect {
		t.Errorf("Choose after a refused probe = %q, want %q", m, MethodDirect)
	}

	if _, err := Clone(context.Background(), MethodReflink, "/a", "/b"); !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("Clone error = %v, want platform.ErrNotSupported; a clone that fell back to a "+
			"byte copy would report a millisecond stall for a whole-archive freeze", err)
	}
}

// TestCloneRefusesAMethodThatIsNotOne: direct has nothing to clone, and a
// Clone(direct) is a caller that lost track of its own decision.
func TestCloneRefusesAMethodThatIsNotOne(t *testing.T) {
	if !isLinux() {
		t.Skip("off Linux every Clone is a refusal; the method check is the Linux half")
	}
	if _, err := Clone(context.Background(), MethodDirect, "/a", "/b"); !errors.Is(err, ErrNoClone) {
		t.Errorf("Clone(direct) error = %v, want ErrNoClone", err)
	}
}
