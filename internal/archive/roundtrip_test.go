package archive

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestArchiveRoundTrip is the whole package in one test: write an archive,
// read its manifest out of the first 8 KB alone, verify every member's digest,
// restore it into a scratch directory, and prove that what came back is
// byte-for-byte what went in, with its modes and mtimes.
//
// It uses two roots, because one root is the case backup.sh already handled and
// two is the case it could not: `tar -C $DATA_DIR` can carry a world or
// /etc/gvm, never both.
func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	etc := t.TempDir()
	dir := t.TempDir()

	writeTree(t, src, pzTree())
	writeTree(t, etc, map[string]file{
		"instances":              {dir: true, perm: 0o750, mtime: fixed.Add(-time.Hour)},
		"instances/pz-main.yaml": {body: "apiVersion: gvm/v1\nkind: Instance\n", perm: 0o640, mtime: fixed.Add(-time.Hour)},
	})

	ref := mustCreate(t, Options{
		Dir:      dir,
		Instance: "pz-main",
		Class:    ClassPreserve,
		Reason:   "adopt-pre-cutover",
		Now:      func() time.Time { return fixed },
		Creator:  Creator{Kind: "root_cli", Reason: "adopt cutover"},
		Subject: Subject{
			Name:                 "pz-main",
			Game:                 "projectzomboid",
			GameManifestSHA256:   "1b40c9de0000000000000000000000000000000000000000000000000000beef",
			GameManifestRevision: 7,
			UnixUser:             "pzserver",
			Paths:                map[string]string{"install": "/opt/pzserver", "data": src},
		},
		Sources: []Source{
			{
				Prefix:  "data/",
				Base:    src,
				Include: []string{"Server", "Saves/Multiplayer/servertest"},
				Exclude: []string{"steamapps/workshop/**"},
			},
			{Prefix: "etcgvm/", Base: etc},
		},
	})

	// -- 1. The 8 KB promise, taken literally. --------------------------------
	f, err := os.Open(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	head, err := PeekManifest(io.LimitReader(f, PeekBytes))
	f.Close()
	if err != nil {
		t.Fatalf("PeekManifest over the first %d bytes: %v", PeekBytes, err)
	}
	if head.Instance.Name != "pz-main" || head.Class != ClassPreserve || head.Reason != "adopt-pre-cutover" {
		t.Fatalf("head manifest = %+v, want the pz-main preserve archive", head.Instance)
	}
	if head.MemberTable == nil || head.MemberTable.Path != MembersMember {
		t.Fatalf("head manifest does not name the member table: %+v", head.MemberTable)
	}
	if len(head.Members) != 0 {
		t.Fatalf("the head manifest carries %d members; the digest table belongs in %s, or an "+
			"8412-file world stops being peekable", len(head.Members), MembersMember)
	}
	wantRoots := map[string]string{"data/": filepath.ToSlash(src), "etcgvm/": filepath.ToSlash(etc)}
	for _, r := range head.Roots {
		if wantRoots[r.Prefix] != r.Base {
			t.Errorf("root %q base = %q, want %q", r.Prefix, r.Base, wantRoots[r.Prefix])
		}
		delete(wantRoots, r.Prefix)
	}
	if len(wantRoots) != 0 {
		t.Errorf("manifest is missing roots %v", wantRoots)
	}

	// The exclusion is the difference between a 2.4 GB nightly and an 8 GB one.
	full, err := ReadArchiveManifest(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range full.Members {
		if strings.Contains(m.Path, "steamapps/workshop") {
			t.Fatalf("%s was archived despite the steamapps/workshop/** exclusion", m.Path)
		}
	}
	if !containsMember(full.Members, "data/Server/servertest.ini") ||
		!containsMember(full.Members, "data/Saves/Multiplayer/servertest/players.db") ||
		!containsMember(full.Members, "etcgvm/instances/pz-main.yaml") {
		t.Fatalf("members are missing something that was included: %v", memberNames(full.Members))
	}
	// The include named Saves/Multiplayer/servertest; its ancestors are stored
	// so their modes survive, which `tar -C` lost.
	if !containsMember(full.Members, "data/Saves/Multiplayer") {
		t.Errorf("the ancestor directory data/Saves/Multiplayer was not stored, so its mode is "+
			"lost on restore: %v", memberNames(full.Members))
	}

	// -- 2. Deep verification. ------------------------------------------------
	if ref.Verified() {
		t.Fatal("Create returned a verified Ref; the bytes have been written but not read back, " +
			"and a Ref that says otherwise would authorize a destroy on an unchecked archive")
	}
	rep := mustVerify(t, ref, ModeDeep)
	if !rep.OK() {
		t.Fatalf("deep verify found problems: %v", rep.Problems)
	}
	if rep.Checked != countFiles(full.Members) {
		t.Errorf("deep verify hashed %d members, the archive holds %d files", rep.Checked, countFiles(full.Members))
	}
	if !ref.Verified() {
		t.Fatal("VerifyRef did not stamp the Ref, so it still cannot authorize a destroy")
	}

	// -- 3. Restore, and compare every member. --------------------------------
	to := t.TempDir()
	rr, err := Restore(context.Background(), RestoreOptions{
		Source: ref.Path,
		To:     to,
		Now:    func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.Legacy {
		t.Error("a gvm archive restored through the legacy path")
	}

	for _, m := range full.Members {
		prefix, rel, ok := splitPrefix(rr.Restored, m.Path)
		if !ok {
			t.Fatalf("%s belongs to no restored root", m.Path)
		}
		out := filepath.Join(rr.Restored[prefix], filepath.FromSlash(rel))
		fi, err := os.Lstat(out)
		if err != nil {
			t.Errorf("%s was not restored: %v", m.Path, err)
			continue
		}
		samePerm(t, m.Path, fi.Mode(), m.Perm())
		if got := fi.ModTime().UTC().Truncate(time.Second); !got.Equal(m.MTime) {
			t.Errorf("%s: mtime %s, want %s", m.Path, got, m.MTime)
		}
		if m.Kind != KindFile {
			continue
		}
		if fi.Size() != m.Size {
			t.Errorf("%s: %d bytes, want %d", m.Path, fi.Size(), m.Size)
		}
		if got := sha256OfFile(t, out); got != m.SHA256 {
			t.Errorf("%s: sha256 %s, want %s", m.Path, got, m.SHA256)
		}
	}

	// Nothing was in the way, so nothing was moved aside.
	if len(rr.Replaced) != 0 {
		t.Errorf("restore into an empty directory moved %v aside", rr.Replaced)
	}
	// And no staging debris is left behind anywhere.
	assertNoDebris(t, to)
}

// TestRestoreOverAliveTreeMovesTheOldContentsAsideRatherThanDeletingThem is the
// boundary between restore and destroy: this package removes a world in exactly
// one function, and that function demands a receipt. Restore is not it.
func TestRestoreOverALiveTreeMovesTheOldContentsAside(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{
		"Server":                {dir: true, perm: 0o755, mtime: fixed},
		"Server/servertest.ini": {body: "new", perm: 0o600, mtime: fixed},
	})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})

	live := t.TempDir()
	writeTree(t, live, map[string]file{
		"Server":                {dir: true, perm: 0o755, mtime: fixed},
		"Server/servertest.ini": {body: "the live world", perm: 0o600, mtime: fixed},
	})

	rr, err := Restore(context.Background(), RestoreOptions{
		Source: ref.Path,
		Dest:   map[string]string{"data/": live},
		Now:    func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rr.Replaced) != 1 {
		t.Fatalf("Replaced = %v, want exactly one holding directory", rr.Replaced)
	}
	old, err := os.ReadFile(filepath.Join(rr.Replaced[0], "Server", "servertest.ini"))
	if err != nil {
		t.Fatalf("the previous contents were not kept: %v", err)
	}
	if string(old) != "the live world" {
		t.Fatalf("held-aside file = %q, want the previous contents", old)
	}
	now, err := os.ReadFile(filepath.Join(live, "Server", "servertest.ini"))
	if err != nil || string(now) != "new" {
		t.Fatalf("restored file = %q, %v; want the archived contents", now, err)
	}
}

func containsMember(ms []Member, path string) bool {
	for _, m := range ms {
		if m.Path == path {
			return true
		}
	}
	return false
}

func memberNames(ms []Member) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Path)
	}
	return out
}

func countFiles(ms []Member) int {
	n := 0
	for _, m := range ms {
		if m.Kind == KindFile {
			n++
		}
	}
	return n
}

// assertNoDebris fails if a staging or holding directory survived.
func assertNoDebris(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, stagingPrefix) {
			t.Errorf("staging directory %s survived the restore", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestManifestIsValidJSONAnOperatorCanRead is the DB-free story: a box with no
// gvm on it must be able to answer "what is this file" with tar and jq.
func TestManifestIsValidJSONAnOperatorCanRead(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "x", perm: 0o644, mtime: fixed}})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "vh-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})

	var raw map[string]any
	f, err := os.Open(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, err := firstMemberBytes(io.LimitReader(f, PeekBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the first member is not readable JSON: %v", err)
	}
	for _, key := range []string{"schema", "created_at", "instance", "class", "reason", "roots"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("manifest has no %q field; an operator reading it with jq needs it", key)
		}
	}
}
