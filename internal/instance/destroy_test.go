package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// newRef is a verified receipt: an archive that exists and whose contents were
// checked. Anything less is refused by Destroy, which is the point.
func newRef(path string) *archive.Ref {
	now := time.Now()
	return &archive.Ref{
		Path:       path,
		Instance:   "pz-main",
		Class:      archive.ClassPreserve,
		Reason:     "pre-rebuild",
		CreatedAt:  now,
		VerifiedAt: now,
	}
}

// writeRef writes a stand-in tarball and returns a receipt for it, sized from
// what actually landed on disk. The bytes are not a real archive because the
// preflight never decompresses one: it stats it, sizes it and peeks its head.
func writeRef(t *testing.T, path string, size int) *archive.Ref {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := newRef(path)
	ref.Bytes = int64(size)
	return ref
}

// jailedFixture returns a Jailed proven against a real temp tree, which is the
// only way to obtain one -- including from inside this package.
func jailedFixture(t *testing.T) []Jailed {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "install"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := NewJail(JailConfig{Allow: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := j.ResolveAll([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// worldFixture builds a small world under a jail root and returns the jail,
// the root and the resolved destroy set.
func worldFixture(t *testing.T, rel ...string) (root string, paths []Jailed) {
	t.Helper()
	root = t.TempDir()
	if len(rel) == 0 {
		rel = []string{"world"}
	}
	for _, r := range rel {
		dir := filepath.Join(root, r, "Saves")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"map_meta.bin", "players.db"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name+" contents"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	j, err := NewJail(JailConfig{Allow: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err = j.ResolveAll(rel)
	if err != nil {
		t.Fatal(err)
	}
	return root, paths
}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one mentioning %q", want)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic = %q, want it to mention %q", msg, want)
		}
	}()
	fn()
}

// Archive-before-destroy, encoded as a type. A caller with no receipt has
// nothing to pass, and passing nil is a programming error rather than an
// operating condition: it panics, where a returned error is something a future
// caller can route around with an `if err != nil { log }` (section 13.1).
func TestDestroyPanicsWithoutAnArchive(t *testing.T) {
	paths := jailedFixture(t)
	mustPanic(t, "nil archive ref", func() {
		_ = Destroy(context.Background(), nil, paths)
	})
}

func TestDestroyPanicsOnAnUnverifiedArchive(t *testing.T) {
	paths := jailedFixture(t)
	written := newRef("/var/backups/gvm/pz-main/pre-rebuild.tar.gz")
	written.VerifiedAt = time.Time{} // written, never checked
	mustPanic(t, "unverified archive ref", func() {
		_ = Destroy(context.Background(), written, paths)
	})
}

// The zero Jailed is the one value another package CAN construct --
// []Jailed{{}} compiles anywhere -- so it is the one forgery Destroy has to
// reject at run time rather than at compile time.
func TestDestroyPanicsOnAForgedZeroJailed(t *testing.T) {
	good := jailedFixture(t)
	mixed := append([]Jailed{}, good...)
	mixed = append(mixed, Jailed{})
	mustPanic(t, "zero Jailed at index 1", func() {
		_ = Destroy(context.Background(), newRef("/var/backups/gvm/pz-main/x.tar.gz"), mixed)
	})
}

func TestDestroyRefusesAnEmptyDestroySet(t *testing.T) {
	err := Destroy(context.Background(), newRef("/var/backups/gvm/pz-main/x.tar.gz"), nil)
	if !errors.Is(err, ErrJail) {
		t.Fatalf("Destroy with no paths = %v, want a refusal: a destroy that removes nothing "+
			"must not report success", err)
	}
}

func TestDestroyHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Destroy(ctx, newRef("/var/backups/gvm/pz-main/x.tar.gz"), jailedFixture(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy = %v, want context.Canceled", err)
	}
}

// The whole point of step 8: the world goes, and the receipt that authorised
// it does not. An implementation that removed the backup along with the world
// would satisfy every type-level check in this package and still be the
// catastrophe archive-before-destroy exists to prevent.
func TestDestroyRemovesTheWorldAndLeavesTheArchive(t *testing.T) {
	root, paths := worldFixture(t)
	// Deliberately outside the jail root: this is where a real backup lives.
	ref := writeRef(t, filepath.Join(t.TempDir(), "backups", "pre-rebuild.tar.gz"), 4096)

	if err := Destroy(context.Background(), ref, paths); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(paths[0].Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of the destroyed world = %v, want it gone", err)
	}
	// The allow root itself survives: only the named paths are destroyed.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the allow root was removed as well: %v", err)
	}
	fi, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatalf("the archive did not survive the destroy: %v", err)
	}
	if fi.Size() != ref.Bytes {
		t.Fatalf("the archive is %d bytes after the destroy, was %d", fi.Size(), ref.Bytes)
	}
}

func TestDestroyRemovesEveryNamedPath(t *testing.T) {
	_, paths := worldFixture(t, "world", "world_player")
	ref := writeRef(t, filepath.Join(t.TempDir(), "b.tar.gz"), 512)

	if err := Destroy(context.Background(), ref, paths); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, p := range paths {
		if _, err := os.Stat(p.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the destroy", p.Rel())
		}
	}
}

// An archive written inside the tree it is a backup OF satisfies
// archive-before-destroy on paper and violates it in fact. It is not a
// hypothetical: it is what a backup directory configured under the data root
// produces on the first rebuild.
func TestDestroyRefusesAnArchiveInsideTheDestroySet(t *testing.T) {
	root, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(root, "world", "backups", "pre-rebuild.tar.gz"), 2048)

	err := Destroy(context.Background(), ref, paths)
	if !errors.Is(err, ErrArchiveInTarget) {
		t.Fatalf("Destroy = %v, want ErrArchiveInTarget", err)
	}
	if _, statErr := os.Stat(paths[0].Path()); statErr != nil {
		t.Fatalf("the world was removed despite the refusal: %v", statErr)
	}
	if _, statErr := os.Stat(ref.Path); statErr != nil {
		t.Fatalf("the archive was removed despite the refusal: %v", statErr)
	}
}

// Verification happened at some earlier moment. Between then and now,
// retention, an operator's rm, or a half-finished copy can have taken the file
// away -- and a Ref for bytes that are gone is not a receipt for anything.
func TestDestroyRefusesAReceiptForAMissingArchive(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "gone.tar.gz"), 128)
	if err := os.Remove(ref.Path); err != nil {
		t.Fatal(err)
	}

	err := Destroy(context.Background(), ref, paths)
	if !errors.Is(err, ErrArchiveMissing) {
		t.Fatalf("Destroy = %v, want ErrArchiveMissing", err)
	}
	if _, statErr := os.Stat(paths[0].Path()); statErr != nil {
		t.Fatalf("the world was removed despite the refusal: %v", statErr)
	}
}

func TestDestroyRefusesAnArchiveThatChangedSize(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "truncated.tar.gz"), 4096)
	if err := os.WriteFile(ref.Path, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Destroy(context.Background(), ref, paths)
	if !errors.Is(err, ErrArchiveMissing) {
		t.Fatalf("Destroy = %v, want ErrArchiveMissing", err)
	}
	if !strings.Contains(err.Error(), "verified at 4096") {
		t.Errorf("err = %v, want it to name the size it was verified at", err)
	}
	if _, statErr := os.Stat(paths[0].Path()); statErr != nil {
		t.Fatalf("the world was removed despite the refusal: %v", statErr)
	}
}

func TestDestroyRefusesADirectoryAsAnArchive(t *testing.T) {
	_, paths := worldFixture(t)
	dir := filepath.Join(t.TempDir(), "not-a-file.tar.gz")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ref := newRef(dir)

	if err := Destroy(context.Background(), ref, paths); !errors.Is(err, ErrArchiveMissing) {
		t.Fatalf("Destroy = %v, want ErrArchiveMissing", err)
	}
}

// A path outside the jail cannot be turned into a Jailed at all, which is why
// Destroy takes []Jailed and not []string: there is no forged value to reject
// downstream because there is no way to construct one upstream.
func TestAPathOutsideTheJailCannotBeConstructed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := NewJail(JailConfig{Allow: []string{root}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want error
	}{
		{"a traversal out of the root", "../etc", ErrTraversal},
		{"a traversal buried in the middle", "world/../../etc", ErrTraversal},
		{"an absolute path elsewhere", outside, ErrOutside},
		// A prefix match would call this one inside the root. It is a
		// neighbouring instance's entire world.
		{"a sibling sharing the root's name prefix", root + "-old", ErrOutside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := j.Resolve(tc.path)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve(%q) = %v, want %v", tc.path, err, tc.want)
			}
			if got.Valid() {
				t.Fatalf("Resolve(%q) returned a valid Jailed alongside its refusal", tc.path)
			}
			// And the zero value it did return is exactly what Destroy
			// refuses, so a caller that ignored the error still destroys
			// nothing.
			mustPanic(t, "zero Jailed at index 0", func() {
				_ = Destroy(context.Background(),
					newRef(filepath.Join(t.TempDir(), "x.tar.gz")), []Jailed{got})
			})
		})
	}
}

// The refusal that matters when it fires: after this destroy there would be no
// room to put the world back, so the destroy is one the operator could not
// undo. statfs is a parameter rather than a package variable so the arithmetic
// is provable on a box that has no statfs at all.
func TestDestroyRefusesWhenARestoreWouldNotFit(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "big.tar.gz"), 1<<20)

	// A megabyte of archive, a volume with 4 KiB free, and a world whose
	// deletion releases a few hundred bytes: there would be no way back.
	tiny := func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 10 << 30, Free: 4096, Avail: 4096}, nil
	}
	pf, err := destroyPreflight(ref, paths, tiny)
	if err != nil {
		t.Fatalf("destroyPreflight: %v", err)
	}
	// A tarball whose head is unreadable falls back to the COMPRESSED size,
	// which is a floor -- and the caller is told so rather than handed a
	// number that looks equally trustworthy either way.
	if !pf.EstimateIsFloor || pf.Estimate != ref.Bytes {
		t.Errorf("estimate %d (floor=%v), want the compressed size %d marked as a floor",
			pf.Estimate, pf.EstimateIsFloor, ref.Bytes)
	}
	if want := platform.Required(pf.Estimate, RestoreHeadroom); pf.Volumes[0].Required != want {
		t.Errorf("Required = %d, want the estimate with 20%% headroom (%d)",
			pf.Volumes[0].Required, want)
	}

	err = pf.Check()
	if !errors.Is(err, ErrDiskPreflight) {
		t.Fatalf("Check = %v, want ErrDiskPreflight", err)
	}
	if !strings.Contains(err.Error(), "short") {
		t.Errorf("err = %q, want it to say how much it is short by", err)
	}
	if _, statErr := os.Stat(paths[0].Path()); statErr != nil {
		t.Fatalf("the world was removed despite the refusal: %v", statErr)
	}
}

// The good estimate is the UNCOMPRESSED size, and it is readable from the
// first 8 KB of the archive: a 2.4 GB world in a 700 MB tarball would be
// under-estimated by a factor of three if the preflight trusted the file size.
func TestDestroyPreflightReadsTheEstimateFromTheArchiveManifest(t *testing.T) {
	root, paths := worldFixture(t)
	const worldBytes = 3 << 30

	ref, err := archive.Create(context.Background(), archive.Options{
		Dir:      filepath.Join(t.TempDir(), "backups"),
		Instance: "pz-main",
		Class:    archive.ClassPreserve,
		Reason:   "pre-rebuild",
		Sources:  []archive.Source{{Prefix: "data/", Base: root, Include: []string{"world"}}},
		Fingerprint: &archive.Fingerprint{
			Paths: []string{"world"},
			Bytes: worldBytes,
		},
	})
	if err != nil {
		t.Fatalf("archive.Create: %v", err)
	}
	ref.VerifiedAt = time.Now() // Create writes the receipt; VerifyRef stamps it

	roomy := func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 10 << 30, Free: 9 << 30, Avail: 9 << 30}, nil
	}
	pf, err := destroyPreflight(ref, paths, roomy)
	if err != nil {
		t.Fatalf("destroyPreflight: %v", err)
	}
	if pf.EstimateIsFloor {
		t.Error("EstimateIsFloor is true for an archive whose head manifest was readable")
	}
	if pf.Estimate != worldBytes {
		t.Fatalf("estimate = %d, want the manifest's %d", pf.Estimate, worldBytes)
	}
}

func TestDestroyPreflightPassesWhenThereIsRoom(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "small.tar.gz"), 1024)

	roomy := func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{Total: 10 << 30, Free: 9 << 30, Avail: 9 << 30}, nil
	}
	pf, err := destroyPreflight(ref, paths, roomy)
	if err != nil {
		t.Fatalf("destroyPreflight: %v", err)
	}
	if err := pf.Check(); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
	if len(pf.Volumes) != 1 || !pf.Volumes[0].Checked {
		t.Fatalf("volumes = %+v, want one checked volume", pf.Volumes)
	}
	if pf.Files != 2 || pf.Dirs != 2 || pf.Bytes == 0 {
		t.Errorf("measured %d files, %d dirs, %d bytes; want the world it was given",
			pf.Files, pf.Dirs, pf.Bytes)
	}
	if pf.Volumes[0].Freeing != pf.Bytes {
		t.Errorf("Freeing = %d, want the measured %d", pf.Volumes[0].Freeing, pf.Bytes)
	}
}

// A refusal based on the ABSENCE of information would fire on the development
// box and never on the target, which is the wrong way round for a safety
// check. An unanswerable volume is reported, not failed.
func TestDestroyPreflightTreatsAnUnsupportedStatfsAsUnchecked(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "small.tar.gz"), 1024)

	unsupported := func(string) (platform.DiskUsage, error) {
		return platform.DiskUsage{}, platform.Unsupported("statfs")
	}
	pf, err := destroyPreflight(ref, paths, unsupported)
	if err != nil {
		t.Fatalf("destroyPreflight: %v", err)
	}
	if pf.Volumes[0].Checked {
		t.Error("the volume is marked checked after an unsupported statfs")
	}
	if err := pf.Check(); err != nil {
		t.Fatalf("Check = %v; an unanswerable volume must not refuse the destroy", err)
	}
}

// A statfs that fails for a real reason is different: the filesystem answered
// and said something is wrong, and proceeding would be destroying on a volume
// nobody can describe.
func TestDestroyPreflightPropagatesARealStatfsFailure(t *testing.T) {
	_, paths := worldFixture(t)
	ref := writeRef(t, filepath.Join(t.TempDir(), "small.tar.gz"), 1024)

	boom := errors.New("statfs: input/output error")
	failing := func(string) (platform.DiskUsage, error) { return platform.DiskUsage{}, boom }
	if _, err := destroyPreflight(ref, paths, failing); !errors.Is(err, boom) {
		t.Fatalf("destroyPreflight = %v, want the underlying statfs error", err)
	}
}

// The query form answers instead of destroying, so it refuses with an error
// naming what was missing: a dry run asking "would this work" must not be able
// to crash the process by asking badly.
func TestDestroyPreflightReturnsErrorsWhereDestroyPanics(t *testing.T) {
	_, paths := worldFixture(t)
	unverified := writeRef(t, filepath.Join(t.TempDir(), "u.tar.gz"), 64)
	unverified.VerifiedAt = time.Time{}

	for _, tc := range []struct {
		name  string
		ref   *archive.Ref
		paths []Jailed
		want  error
		says  string
	}{
		{"a nil ref", nil, paths, ErrArchiveMissing, "no archive ref"},
		{"a ref with no path", &archive.Ref{VerifiedAt: time.Now()}, paths, ErrArchiveMissing, "names no path"},
		{"an unverified ref", unverified, paths, ErrArchiveMissing, "never verified"},
		{"an empty destroy set", newRef("x"), nil, ErrJail, "no paths"},
		{"a forged zero Jailed", newRef("x"), []Jailed{{}}, ErrJail, "zero Jailed at index 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DestroyPreflight(tc.ref, tc.paths)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DestroyPreflight = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %q, want it to name %q", err, tc.says)
			}
		})
	}
}

// Charging every volume the whole archive would refuse a perfectly restorable
// two-root destroy, and charging none of them would approve one that cannot
// fit. The share is what the volume would actually have to hold.
func TestApportion(t *testing.T) {
	for _, tc := range []struct {
		total, part, whole, want int64
	}{
		{100, 50, 100, 50},
		{100, 100, 100, 100},
		{100, 0, 100, 0},
		{100, 1, 3, 34}, // rounded up: a preflight must not under-ask
		{0, 5, 10, 0},
		{100, 5, 0, 100},     // an unmeasurable whole charges the full estimate
		{100, 200, 100, 100}, // a part larger than the whole is not a discount
		{1 << 40, 1 << 39, 1 << 40, 1 << 39},
	} {
		if got := apportion(tc.total, tc.part, tc.whole); got != tc.want {
			t.Errorf("apportion(%d, %d, %d) = %d, want %d",
				tc.total, tc.part, tc.whole, got, tc.want)
		}
	}
}

func TestVolumeCheckFits(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    VolumeCheck
		want bool
	}{
		{"unchecked always fits", VolumeCheck{Required: 1 << 40}, true},
		{"free space alone is enough", VolumeCheck{Checked: true, Avail: 100, Required: 100}, true},
		{"the destroy is what makes it fit", VolumeCheck{Checked: true, Avail: 10, Freeing: 95, Required: 100}, true},
		{"one byte short", VolumeCheck{Checked: true, Avail: 10, Freeing: 89, Required: 100}, false},
	} {
		if got := tc.v.Fits(); got != tc.want {
			t.Errorf("%s: Fits() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An operator deciding whether to free space needs "2.4 GiB", not 2251799813
// with the digits to count.
func TestBytesHuman(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{2251799813, "2.1 GiB"},
		{5 << 40, "5.0 TiB"},
	} {
		if got := bytesHuman(tc.in); got != tc.want {
			t.Errorf("bytesHuman(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
