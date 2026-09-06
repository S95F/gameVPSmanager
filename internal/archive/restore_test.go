package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRestoreAbortsBeforeRename is section 13.2 step 4, and it is the only
// ordering in the restore that has no way around it.
//
// The archive here is structurally perfect -- valid gzip, valid tar, a manifest
// that parses -- and one member's bytes disagree with the digest recorded for
// them. The live tree must be exactly as it was afterwards, because the
// verification happens as the bytes stream into staging and nothing is renamed
// until the whole stream has passed.
func TestRestoreAbortsBeforeRename(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, pzTree())

	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src, Include: []string{"Server", "Saves/Multiplayer/servertest"}}},
	})

	corrupt := filepath.Join(dir, "corrupt.tar.gz")
	tamper(t, ref.Path, corrupt, "data/Saves/Multiplayer/servertest/players.db", func(b []byte) []byte {
		// One byte, in the middle. Not a truncation, not a header change:
		// exactly the silent bit-rot that a whole-file checksum on the
		// COMPRESSED file would still catch but that backup.sh caught not
		// at all.
		out := append([]byte(nil), b...)
		out[len(out)/2] ^= 0x01
		return out
	})

	// A live world, of a shape that would be recognisably clobbered.
	live := t.TempDir()
	writeTree(t, live, map[string]file{
		"Server":                       {dir: true, perm: 0o755, mtime: fixed},
		"Server/servertest.ini":        {body: "THE LIVE CONFIG", perm: 0o600, mtime: fixed},
		"Saves":                        {dir: true, perm: 0o755, mtime: fixed},
		"Saves/Multiplayer":            {dir: true, perm: 0o755, mtime: fixed},
		"Saves/Multiplayer/servertest": {dir: true, perm: 0o755, mtime: fixed},
		"Saves/Multiplayer/servertest/players.db": {body: "THE LIVE PLAYERS", perm: 0o644, mtime: fixed},
	})
	before := snapshotTree(t, live)

	_, err := Restore(context.Background(), RestoreOptions{
		Source: corrupt,
		Dest:   map[string]string{"data/": live},
		Now:    func() time.Time { return fixed },
	})
	if err == nil {
		t.Fatal("Restore from an archive whose contents do not match its manifest reported success")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "players.db") {
		t.Errorf("the error does not name the member that failed:\n%v", err)
	}

	sameTree(t, "the live world after an aborted restore", before, snapshotTree(t, live))
}

// TestRestoreRefusesAMemberThatWouldEscapeItsRoot is zip-slip, closed
// lexically before any filesystem call, because an archive is data from
// outside gvm: it may have been produced by another box or edited by hand.
func TestRestoreRefusesAMemberThatWouldEscapeItsRoot(t *testing.T) {
	for _, tc := range []struct{ name, member string }{
		{"parent traversal", "../../etc/passwd"},
		{"absolute", "/etc/passwd"},
		{"embedded traversal", "Server/../../../etc/shadow"},
		{"backslash", `Server\..\..\etc\passwd`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := safeMemberPath(tc.member); err == nil {
				t.Fatalf("safeMemberPath(%q) accepted a path that leaves its root", tc.member)
			}
		})
	}
	// And the control: an ordinary member is accepted, so the test above is
	// not passing because everything is refused.
	if got, err := safeMemberPath("Saves/Multiplayer/servertest/map_t.bin"); err != nil || got == "" {
		t.Fatalf("safeMemberPath refused an ordinary member: %q, %v", got, err)
	}
}

func TestRestoreRefusesASymlinkPointingOutOfTheTree(t *testing.T) {
	if err := checkLinkTarget("Server/link", "/etc"); !errors.Is(err, ErrUnsafeMember) {
		t.Errorf("an absolute symlink target was accepted: %v", err)
	}
	if err := checkLinkTarget("Server/link", "../../etc"); !errors.Is(err, ErrUnsafeMember) {
		t.Errorf("a traversing symlink target was accepted: %v", err)
	}
	if err := checkLinkTarget("Server/link", "servertest.ini"); err != nil {
		t.Errorf("a sibling symlink target was refused: %v", err)
	}
}

// TestRestoreCallsCheckLinkTarget pins the WIRING, not the function.
//
// The unit assertions above keep passing if checkLinkTarget is never called
// from Restore at all, and refuseThroughLink only fires once a later member
// tries to traverse a declared link. A hostile archive that declares a
// symlink to an absolute path and stops there is caught by nothing else, so
// this drives it through Restore itself.
func TestRestoreCallsCheckLinkTarget(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "abslink.tar.gz")
	writeHostileTar(t, arc, []*tar.Header{
		{Name: "Server/escape", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
	}, nil)

	to := filepath.Join(dir, "dest")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), RestoreOptions{
		Source: arc, To: to, AllowLegacy: true,
		Now: func() time.Time { return fixed },
	})
	if err == nil {
		t.Fatal("Restore accepted an archive declaring a symlink to an absolute path")
	}
	if !errors.Is(err, ErrUnsafeMember) {
		t.Errorf("Restore = %v, want ErrUnsafeMember", err)
	}
}

// TestLegacyArchive is the bash-suite tarball: no .gvm/manifest.json, no
// per-member digests, no roots.
//
// The fixture in testdata was produced by testdata/genlegacy/main.go using the
// standard library, deliberately NOT by this package's writer, so the test
// proves gvm can read a foreign archive rather than that it agrees with
// itself.
func TestLegacyArchive(t *testing.T) {
	legacy := filepath.Join("testdata", "legacy-pz-backup.tar.gz")

	t.Run("it is refused by default", func(t *testing.T) {
		_, err := Restore(context.Background(), RestoreOptions{
			Source: legacy,
			To:     t.TempDir(),
			Now:    func() time.Time { return fixed },
		})
		if !errors.Is(err, ErrNoManifest) {
			t.Fatalf("Restore = %v, want ErrNoManifest: reduced verification must be asked for, "+
				"not arrived at silently", err)
		}
	})

	t.Run("it restores with a clear warning", func(t *testing.T) {
		to := t.TempDir()
		rep, err := Restore(context.Background(), RestoreOptions{
			Source:      legacy,
			To:          to,
			AllowLegacy: true,
			Now:         func() time.Time { return fixed },
		})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if !rep.Legacy {
			t.Fatal("the report does not mark this restore as legacy")
		}
		if len(rep.Warnings) == 0 {
			t.Fatal("a reduced-verification restore produced no warning at all")
		}
		warning := strings.Join(rep.Warnings, "\n")
		for _, want := range []string{ManifestMember, "digest"} {
			if !strings.Contains(warning, want) {
				t.Errorf("the warning does not mention %q, so an operator cannot tell what was "+
					"NOT checked:\n%s", want, warning)
			}
		}

		// The contents are the bash suite's: `tar -C $DATA_DIR Server
		// Saves/Multiplayer/servertest`, with no intermediate Saves/ or
		// Saves/Multiplayer/ member of its own.
		for path, want := range map[string]string{
			"Server/servertest.ini":                   "PublicName=Steve's Server\nRCONPassword=REDACTED\nMods=\n",
			"Saves/Multiplayer/servertest/map_t.bin":  "\x00\x01map chunk table\x00",
			"Saves/Multiplayer/servertest/players.db": "SQLite format 3\x00players",
		} {
			got, rerr := os.ReadFile(filepath.Join(to, filepath.FromSlash(path)))
			if rerr != nil {
				t.Errorf("%s was not restored: %v", path, rerr)
				continue
			}
			if string(got) != want {
				t.Errorf("%s = %q, want %q", path, got, want)
			}
		}
		// mtimes still come across: they are in the tar headers even when
		// nothing else is.
		fi, err := os.Stat(filepath.Join(to, "Server", "servertest.ini"))
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 9, 4, 18, 23, 11, 0, time.UTC)
		if got := fi.ModTime().UTC().Truncate(time.Second); !got.Equal(want) {
			t.Errorf("mtime = %s, want %s", got, want)
		}
		assertNoDebris(t, to)
	})

	t.Run("it refuses to guess where to put itself", func(t *testing.T) {
		_, err := Restore(context.Background(), RestoreOptions{
			Source:      legacy,
			AllowLegacy: true,
			Now:         func() time.Time { return fixed },
		})
		if err == nil {
			t.Fatal("Restore invented a destination for an archive that records no roots. " +
				"Guessing $DATA_DIR from a filename is exactly the adoption failure this " +
				"project exists to avoid")
		}
	})

	t.Run("verify reports it as legacy rather than corrupt", func(t *testing.T) {
		rep, err := Verify(context.Background(), legacy, VerifyOptions{Mode: ModeShallow, Now: func() time.Time { return fixed }})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !rep.Legacy {
			t.Fatal("the report does not mark the archive as legacy")
		}
		if !rep.OK() {
			t.Fatalf("a readable bash-suite tarball was reported as having problems: %v. It is "+
				"not corrupt, it is old", rep.Problems)
		}
		if rep.Members == 0 {
			t.Error("no members were counted, so the gzip stream was not walked at all")
		}
		if len(rep.Warnings) == 0 {
			t.Error("verify produced no warning about the absent integrity story")
		}
	})
}

// TestManifestDriftIsRefusedUntilItIsAccepted: restoring a 2026 world onto a
// 2027 manifest whose shutdown ladder changed is a decision, not a default.
func TestManifestDriftIsRefusedUntilItIsAccepted(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Subject: Subject{Name: "pz-main", GameManifestSHA256: "aaaa", GameManifestRevision: 7},
		Sources: []Source{{Prefix: "data/", Base: src}},
	})

	_, err := Restore(context.Background(), RestoreOptions{
		Source:                   ref.Path,
		To:                       t.TempDir(),
		ExpectGameManifestSHA256: "bbbb",
		Now:                      func() time.Time { return fixed },
	})
	if !errors.Is(err, ErrManifestDrift) {
		t.Fatalf("Restore = %v, want ErrManifestDrift", err)
	}

	if _, err := Restore(context.Background(), RestoreOptions{
		Source:                   ref.Path,
		To:                       t.TempDir(),
		ExpectGameManifestSHA256: "bbbb",
		AcceptManifestDrift:      true,
		Now:                      func() time.Time { return fixed },
	}); err != nil {
		t.Fatalf("Restore with --accept-manifest-drift = %v, want it to proceed", err)
	}
}

// TestRestoreRefusesTwoRootsLandingInOneDirectory.
//
// Each root promotes its own staged tree, so a shared destination means the
// second promotion moves the first's freshly restored members into a holding
// directory: a restore that reports success having half worked.
func TestRestoreRefusesTwoRootsLandingInOneDirectory(t *testing.T) {
	a, b, dir := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]file{"world.bin": {body: "a", perm: 0o644, mtime: fixed}})
	writeTree(t, b, map[string]file{"gvm.yaml": {body: "b", perm: 0o644, mtime: fixed}})

	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now: func() time.Time { return fixed },
		Sources: []Source{
			{Prefix: "data/", Base: a},
			{Prefix: "etcgvm/", Base: b},
		},
	})

	shared := t.TempDir()
	_, err := Restore(context.Background(), RestoreOptions{
		Source: ref.Path,
		Dest:   map[string]string{"data/": shared, "etcgvm/": shared},
		Now:    func() time.Time { return fixed },
	})
	if err == nil {
		t.Fatal("Restore accepted two roots restoring into one directory")
	}
	if !strings.Contains(err.Error(), shared) {
		t.Errorf("the refusal does not name the shared destination: %v", err)
	}
}

// writeHostileTar builds a tarball by hand, with no manifest, so a test can
// hand Restore member shapes this package's writer would never produce.
func writeHostileTar(t *testing.T, path string, hdrs []*tar.Header, bodies map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for _, h := range hdrs {
		if body, ok := bodies[h.Name]; ok {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body, ok := bodies[h.Name]; ok {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreRefusesASymlinkCHAINThatEscapesTheStage is zip-slip's second act,
// done properly.
//
// checkLinkTarget judges each link on its own with path.Join, which collapses
// ".." LEXICALLY -- so "d1/s" -> "../d0/s/.." joins to "d0" and looks like a
// perfectly ordinary sibling reference. The kernel does not collapse anything:
// it walks d1 -> ".." -> stage -> d0 -> s, and s is itself a symlink to "..",
// so the walk leaves the stage. Each link climbs one level, and a chain of them
// climbs as far as it likes. Then a regular member with no ".." in its own path
// -- so safeMemberPath is satisfied -- is written THROUGH the chain, landing
// outside the staging directory as root, before its digest is ever compared and
// where the deferred cleanup cannot reach it.
func TestRestoreRefusesASymlinkCHAINThatEscapesTheStage(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "hostile.tar.gz")

	// Four hops is more than the two levels between the staging directory and
	// the destination's parent, so the escaped member lands well outside.
	const hops = 4
	var hdrs []*tar.Header
	hdrs = append(hdrs, &tar.Header{
		Name: "d0/s", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o777,
	})
	for i := 1; i < hops; i++ {
		hdrs = append(hdrs, &tar.Header{
			Name:     fmt.Sprintf("d%d/s", i),
			Typeflag: tar.TypeSymlink,
			Linkname: fmt.Sprintf("../d%d/s/..", i-1),
			Mode:     0o777,
		})
	}
	escaped := fmt.Sprintf("d%d/s/pwned.txt", hops-1)
	hdrs = append(hdrs, &tar.Header{Name: escaped, Typeflag: tar.TypeReg, Mode: 0o644})
	writeHostileTar(t, arc, hdrs, map[string]string{escaped: "owned"})

	to := filepath.Join(dir, "dest")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, dir)

	_, err := Restore(context.Background(), RestoreOptions{
		Source: arc, To: to, AllowLegacy: true,
		Now: func() time.Time { return fixed },
	})
	if err == nil {
		t.Fatal("Restore extracted a member through a chain of symlinks that leaves the staging tree")
	}
	if !errors.Is(err, ErrUnsafeMember) {
		t.Errorf("Restore = %v, want ErrUnsafeMember", err)
	}

	// The decisive assertion: nothing was written outside the staging tree.
	// The stage is removed by the deferred cleanup, so anything that escaped it
	// is still on disk and shows up here as an addition.
	for p := range snapshotTree(t, dir) {
		if _, existed := before[p]; !existed && strings.Contains(p, "pwned") {
			t.Fatalf("%s was written outside the staging directory and survived the aborted "+
				"restore; a restore that fails must leave the tree byte-identical", p)
		}
	}
}
