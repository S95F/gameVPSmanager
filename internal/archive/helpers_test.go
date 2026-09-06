package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixed is the clock every test archives against, so filenames, manifests and
// retention windows are all decided by arithmetic rather than by how long the
// test suite happened to take.
var fixed = time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)

// clockFrom returns a clock that advances by step on every call, which is how
// a test produces several archives with distinct names and a known order
// without sleeping.
func clockFrom(start time.Time, step time.Duration) func() time.Time {
	n := 0
	return func() time.Time {
		t := start.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// file describes one entry in a source tree fixture.
type file struct {
	body  string
	perm  fs.FileMode
	mtime time.Time
	dir   bool
}

// writeTree materialises a fixture tree and returns it.
func writeTree(t *testing.T, base string, tree map[string]file) {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// Directories first, so a file's parent exists and so a parent's mtime is
	// not bumped by a child written afterwards.
	for _, pass := range []bool{true, false} {
		for name, f := range tree {
			if f.dir != pass {
				continue
			}
			full := filepath.Join(base, filepath.FromSlash(name))
			if f.dir {
				if err := os.MkdirAll(full, orDefault(f.perm, 0o755)); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(f.body), orDefault(f.perm, 0o644)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// mtimes last and deepest-first, so nothing that follows disturbs them.
	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}
	// Longest path first is deepest first for a tree.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if len(names[j]) > len(names[i]) {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		f := tree[name]
		if f.mtime.IsZero() {
			continue
		}
		full := filepath.Join(base, filepath.FromSlash(name))
		if err := os.Chtimes(full, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func orDefault(p, def fs.FileMode) fs.FileMode {
	if p == 0 {
		return def
	}
	return p
}

// pzTree is a miniature Project Zomboid data directory: the two things
// backup.sh took, plus one thing it should not have taken.
func pzTree() map[string]file {
	base := fixed.Add(-2 * time.Hour)
	return map[string]file{
		"Server":                                   {dir: true, perm: 0o755, mtime: base},
		"Server/servertest.ini":                    {body: "PublicName=Steve's Server\nRCONPassword=x\n", perm: 0o600, mtime: base.Add(time.Minute)},
		"Server/servertest_SandboxVars.lua":        {body: "SandboxVars = {\n\tZombies = 3,\n}\n", perm: 0o644, mtime: base.Add(2 * time.Minute)},
		"Saves":                                    {dir: true, perm: 0o755, mtime: base},
		"Saves/Multiplayer":                        {dir: true, perm: 0o755, mtime: base},
		"Saves/Multiplayer/servertest":             {dir: true, perm: 0o750, mtime: base.Add(3 * time.Minute)},
		"Saves/Multiplayer/servertest/map_t.bin":   {body: "map chunk table", perm: 0o644, mtime: base.Add(4 * time.Minute)},
		"Saves/Multiplayer/servertest/players.db":  {body: "SQLite format 3\x00players", perm: 0o644, mtime: base.Add(5 * time.Minute)},
		"Saves/Multiplayer/servertest/map_1_1.bin": {body: strings.Repeat("chunk", 400), perm: 0o644, mtime: base.Add(6 * time.Minute)},
		// The 8 GB-versus-2.4 GB exclusion of section 13.2.
		"steamapps":          {dir: true, perm: 0o755, mtime: base},
		"steamapps/workshop": {dir: true, perm: 0o755, mtime: base},
		"steamapps/workshop/content/108600/2169435993/mod.info": {body: "name=Brita", perm: 0o644, mtime: base},
	}
}

// mustCreate archives a tree and fails the test if it cannot.
func mustCreate(t *testing.T, o Options) *Ref {
	t.Helper()
	ref, err := Create(context.Background(), o)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return ref
}

// mustVerify verifies an archive deeply and fails on any problem.
func mustVerify(t *testing.T, ref *Ref, mode Mode) *Report {
	t.Helper()
	rep, err := VerifyRef(context.Background(), ref, VerifyOptions{Mode: mode, Scratch: t.TempDir(), Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("VerifyRef(%s): %v", mode, err)
	}
	return rep
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// samePerm compares two permission sets the way the running platform can
// express them.
//
// Windows has no unix mode: os.Chmod there only moves the read-only bit, and
// os.Stat reports 0666 or 0444 whatever was asked for. Both the recorded mode
// and the restored one come from the same platform, so an exact comparison is
// still the right assertion; this exists to make the failure message say which
// platform's rules were in play rather than leaving a reader on Linux confused
// by a Windows CI log.
// platformPerm maps an intended unix mode onto what the running platform can
// actually store.
//
// Windows keeps only the read-only bit: os.Chmod there turns any write bit
// into 0666 and its absence into 0444, and os.Stat reports that back.
// Asserting 0600 on the development machine would be asserting something the
// filesystem cannot represent; the real 0600 is asserted by the Linux CI run
// of the same test.
func platformPerm(mode fs.FileMode) fs.FileMode {
	if runtime.GOOS != "windows" {
		return mode
	}
	if mode&0o200 != 0 {
		return 0o666
	}
	return 0o444
}

func samePerm(t *testing.T, what string, got, want fs.FileMode) {
	t.Helper()
	if got.Perm() == want.Perm() {
		return
	}
	t.Errorf("%s: mode %04o, want %04o (on %s)", what, got.Perm(), want.Perm(), runtime.GOOS)
}

// firstMemberBytes returns the body of the archive's first tar member,
// reading only what the caller's reader allows. It is deliberately written
// with the standard library rather than by calling into this package, so the
// test proves the FORMAT is readable and not merely that our reader agrees
// with our writer.
func firstMemberBytes(r io.Reader) ([]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil {
		return nil, err
	}
	if hdr.Name != ManifestMember {
		return nil, fmt.Errorf("first member is %q, want %q", hdr.Name, ManifestMember)
	}
	return io.ReadAll(tr)
}

// tamper rewrites an archive, changing one member's bytes while leaving the
// manifest and its digest table exactly as they were.
//
// A flipped byte in the compressed stream would fail the gzip CRC, which is a
// different and easier failure. This produces the one that matters: a
// structurally perfect archive whose contents do not match what it says about
// itself.
func tamper(t *testing.T, src, dst, member string, mutate func([]byte) []byte) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == member {
			body = mutate(body)
			hdr.Size = int64(len(body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree records every path under root with its contents, so a test can
// assert that a failed operation changed nothing at all.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			out[filepath.ToSlash(rel)+"/"] = ""
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		got, ok := after[k]
		if !ok {
			t.Errorf("%s: %s disappeared", what, k)
			continue
		}
		if got != v {
			t.Errorf("%s: %s changed from %q to %q", what, k, v, got)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s: %s appeared", what, k)
		}
	}
}
