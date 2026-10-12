package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCreateRefusesAnArchiveThatProtectsNothing.
//
// Every one of these would produce a file that satisfies archive-before-destroy
// by existing, while protecting nothing. The type says a Ref means "the data is
// safe"; these are the ways that could be a lie.
func TestCreateRefusesAnArchiveThatProtectsNothing(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	base := Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	}

	cases := map[string]func(o *Options){
		"no instance":              func(o *Options) { o.Instance = "" },
		"no reason":                func(o *Options) { o.Reason = "" },
		"unknown class":            func(o *Options) { o.Class = "maybe" },
		"no sources":               func(o *Options) { o.Sources = nil },
		"relative base":            func(o *Options) { o.Sources[0].Base = "relative/path" },
		"prefix without a slash":   func(o *Options) { o.Sources[0].Prefix = "data" },
		"empty source tree":        func(o *Options) { o.Sources[0].Base = t.TempDir() },
		"missing required include": func(o *Options) { o.Sources[0].Include = []string{"Saves/Multiplayer/servertest"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Sources = append([]Source(nil), base.Sources...)
			mutate(&o)
			ref, err := Create(context.Background(), o)
			if err == nil {
				t.Fatalf("Create succeeded and wrote %s", ref.Path)
			}
			if ref != nil {
				t.Fatal("a failed Create returned a Ref, which would then authorise a prune")
			}
		})
	}
}

// TestCreateRefusesTwoSourcesSharingAPrefix: a member path has to say which
// base it came from, or restore cannot put it back.
func TestCreateRefusesAmbiguousPrefixes(t *testing.T) {
	a, b, dir := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]file{"x": {body: "a", perm: 0o644, mtime: fixed}})
	writeTree(t, b, map[string]file{"y": {body: "b", perm: 0o644, mtime: fixed}})

	for name, sources := range map[string][]Source{
		"duplicate prefix":      {{Prefix: "data/", Base: a}, {Prefix: "data/", Base: b}},
		"unprefixed among many": {{Prefix: "", Base: a}, {Prefix: "etcgvm/", Base: b}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Create(context.Background(), Options{
				Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
				Now: func() time.Time { return fixed }, Sources: sources,
			})
			if err == nil {
				t.Fatal("Create accepted sources whose members cannot be told apart")
			}
		})
	}
}

// TestCreateRefusesASourceFileImpersonatingTheManifest.
//
// A member arriving from a world directory called .gvm/manifest.json would be
// a self-describing archive describing itself falsely, and it would be the
// first member's content that a peek reads.
func TestCreateRefusesASourceFileImpersonatingTheManifest(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{
		".gvm":               {dir: true, perm: 0o755, mtime: fixed},
		".gvm/manifest.json": {body: `{"schema":1,"class":"routine"}`, perm: 0o644, mtime: fixed},
	})
	_, err := Create(context.Background(), Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "", Base: src}},
	})
	if err == nil || !strings.Contains(err.Error(), MetaPrefix) {
		t.Fatalf("Create = %v, want a refusal naming the reserved %s namespace", err, MetaPrefix)
	}
}

// TestOptionalIncludesAreNotesNotErrors, per section 8.2's include_optional.
func TestOptionalIncludesAreNotesNotErrors(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{
		"Server":                {dir: true, perm: 0o755, mtime: fixed},
		"Server/servertest.ini": {body: "x", perm: 0o600, mtime: fixed},
	})
	var notes []string
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:    func() time.Time { return fixed },
		OnNote: func(s string) { notes = append(notes, s) },
		Sources: []Source{{
			Prefix:          "data/",
			Base:            src,
			Include:         []string{"Server"},
			IncludeOptional: []string{"Saves/Multiplayer/servertest_player"},
		}},
	})
	if ref == nil {
		t.Fatal("Create failed over an absent optional include")
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), "servertest_player") {
		t.Errorf("the absent optional include produced no note: %v", notes)
	}
}

// TestExcludePatterns pins the two forms the shipped manifests use.
func TestExcludePatterns(t *testing.T) {
	cases := []struct {
		patterns []string
		rel      string
		want     bool
	}{
		{[]string{"steamapps/workshop/**"}, "steamapps/workshop/content/108600/x/mod.info", true},
		{[]string{"steamapps/workshop/**"}, "steamapps/workshop", false},
		{[]string{"steamapps/workshop/**"}, "steamapps/common/x", false},
		{[]string{"steamapps/workshop"}, "steamapps/workshop", true},
		{[]string{"steamapps/workshop"}, "steamapps/workshop/content/x", true},
		{[]string{"*.log"}, "server.log", true},
		{[]string{"*.log"}, "logs/server.log", false},
		{[]string{"logs/*.log"}, "logs/server.log", true},
		{nil, "Server/servertest.ini", false},

		// The "**/" PREFIX is the third form, and Project Zomboid's manifest
		// declares it: `**/*.lock`. path.Match's "*" does not cross a
		// separator, so without the depth walk this pattern would match a
		// lock file one directory down and miss the one in
		// Saves/Multiplayer/<world>/ -- which is where a world actually is.
		{[]string{"**/*.lock"}, "x.lock", true},
		{[]string{"**/*.lock"}, "Saves/x.lock", true},
		{[]string{"**/*.lock"}, "Saves/Multiplayer/gvm/map_t.bin.lock", true},
		{[]string{"**/*.lock"}, "Saves/Multiplayer/gvm/map_t.bin", false},
		{[]string{"**/cache"}, "a/b/cache/thing", true},
		{[]string{"**/cache/**"}, "a/b/cache/thing", true},
		{[]string{"**/cache/**"}, "a/b/cache", false},
		// A bare "**" means everything, and the shipped manifests never write
		// one. Pinned here because the reading is not obvious from the code:
		// it falls through to the ancestor match, where "**" matches any
		// single top-level directory. An operator who writes it has emptied
		// their own archive, and spec's validator is where that should be
		// caught rather than reinterpreted here.
		{[]string{"**/"}, "Server/servertest.ini", true},
	}
	for _, tc := range cases {
		if got := excluded(tc.patterns, tc.rel); got != tc.want {
			t.Errorf("excluded(%v, %q) = %v, want %v", tc.patterns, tc.rel, got, tc.want)
		}
	}
}

// TestCreateDetectsAFileChangingUnderIt is the one race in a two-pass write,
// and the freeze lease (D-31) is what normally prevents it. Detecting it turns
// a silently corrupt archive into a named error.
func TestCreateDetectsAFileChangingUnderIt(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]file{
		"a.bin": {body: strings.Repeat("a", 4096), perm: 0o644, mtime: fixed},
		"b.bin": {body: strings.Repeat("b", 4096), perm: 0o644, mtime: fixed},
	})

	// The control: a staging entry whose digest matches what is on disk goes
	// through, so the assertions below are not passing because writeMember
	// rejects everything.
	good, err := stageOne(context.Background(), "data/b.bin", filepath.Join(src, "b.bin"), lstat(t, filepath.Join(src, "b.bin")), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMember(tar.NewWriter(io.Discard), *good); err != nil {
		t.Fatalf("writeMember over an unchanged file = %v", err)
	}

	// The same file, with the digest the scan pass would have recorded a
	// moment before somebody edited it. The length is unchanged, so the size
	// check cannot be what catches this -- it has to be the re-hash.
	edited := *good
	edited.SHA256 = strings.Repeat("0", 64)
	err = writeMember(tar.NewWriter(io.Discard), edited)
	if !errors.Is(err, ErrDrifted) {
		t.Fatalf("writeMember with a stale digest = %v, want ErrDrifted; without this check a "+
			"file edited mid-backup produces an archive that verifies against the digest of "+
			"contents it does not hold", err)
	}
	if !strings.Contains(err.Error(), "b.bin") {
		t.Errorf("the error does not name the file that moved: %v", err)
	}

	// And a file that shrank between the two passes.
	shrunk := *good
	shrunk.Size = good.Size + 1024
	err = writeMember(tar.NewWriter(io.Discard), shrunk)
	if !errors.Is(err, ErrDrifted) {
		t.Fatalf("writeMember over a file that shrank = %v, want ErrDrifted", err)
	}
}

func lstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestCreateRejectsAHeadThatWouldNotBePeekable keeps the 8 KB promise a
// property of the writer rather than a hope of the reader.
func TestCreateRejectsAHeadThatWouldNotBePeekable(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})

	_, err := Create(context.Background(), Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now: func() time.Time { return fixed },
		// A spec YAML larger than the head budget. On a real instance this
		// cannot happen; the check exists so that if it ever does, it breaks
		// at create rather than at 2am during a DB-free restore.
		SpecYAML: strings.Repeat("# padding\n", 2000),
		Sources:  []Source{{Prefix: "data/", Base: src}},
	})
	if !errors.Is(err, ErrHeadTooLarge) {
		t.Fatalf("Create = %v, want ErrHeadTooLarge", err)
	}
	// And nothing was left behind.
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused Create left %d files in the backup directory", len(entries))
	}
}

// TestFilenameRoundTrip: retention depends on being able to tell an archive
// gvm wrote from one it merely found.
func TestFilenameRoundTrip(t *testing.T) {
	name := Filename("pz-main", ClassPreserve, fixed)
	if name != "pz-main-20260904-182211-preserve.tar.gz" {
		t.Fatalf("Filename = %q", name)
	}
	inst, at, class, ok := ParseFilename(name)
	if !ok || inst != "pz-main" || class != ClassPreserve || !at.Equal(fixed) {
		t.Fatalf("ParseFilename(%q) = %q, %s, %q, %v", name, inst, at, class, ok)
	}
	// An instance name with a hyphen in it, which D-45 permits.
	long := Filename("mc-survival-2", ClassRoutine, fixed)
	inst, _, class, ok = ParseFilename(long)
	if !ok || inst != "mc-survival-2" || class != ClassRoutine {
		t.Fatalf("ParseFilename(%q) = %q, %q, %v", long, inst, class, ok)
	}
	for _, foreign := range []string{
		"pz-servertest-20260101-030000.tar.gz", // backup.sh's shape
		"notes.txt",
		"pz-main-20260904-182211-maybe.tar.gz",
		"archives.index.json",
	} {
		if _, _, _, ok := ParseFilename(foreign); ok {
			t.Errorf("ParseFilename(%q) claimed gvm wrote it; retention would then prune it", foreign)
		}
	}
}

// TestCreateWillNotOverwriteAnExistingArchive.
//
// Filename() has one-second resolution and carries no nonce, so two archives of
// the same instance and class in the same second name the same file -- and
// os.Rename replaces silently on both Linux and Windows. Every other place in
// this package that renames onto something that might exist handles it (see
// promote in restore.go, which moves the old entry aside first); this was the
// one path where a good archive could be unlinked with nothing recorded.
//
// The realistic trigger is not a fast loop: it is a job that injects a frozen
// job-start clock as Options.Now, which is how every test in this package uses
// that seam.
func TestCreateWillNotOverwriteAnExistingArchive(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "the first world", perm: 0o644, mtime: fixed}})

	opts := func() Options {
		return Options{
			Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
			Now:     func() time.Time { return fixed },
			Sources: []Source{{Prefix: "data/", Base: src}},
		}
	}
	first := mustCreate(t, opts())
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}

	// The world moves on, and a second archive is taken in the same second.
	writeTree(t, src, map[string]file{"world.bin": {body: "a different world", perm: 0o644, mtime: fixed}})
	second, err := Create(context.Background(), opts())
	if !errors.Is(err, ErrArchiveExists) {
		t.Fatalf("Create = (%v, %v), want ErrArchiveExists: the second archive names the same file "+
			"as the first, and replacing it silently unlinks a good backup", second, err)
	}
	if second != nil {
		t.Error("a refused Create returned a Ref, which is the value that would then authorise a prune")
	}

	after, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("the first archive is gone: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the first archive's bytes were replaced; no code path in this package may make " +
			"an archive disappear")
	}
	if _, err := VerifyRef(context.Background(), first, VerifyOptions{
		Mode: ModeDeep, Scratch: t.TempDir(), Now: func() time.Time { return fixed },
	}); err != nil {
		t.Fatalf("the surviving archive no longer verifies: %v", err)
	}

	// And no temp file was left behind to be counted by retention.
	for _, name := range archivesIn(t, dir) {
		if strings.HasPrefix(name, ".") {
			t.Errorf("a temp file survived the refusal: %s", name)
		}
	}
}
