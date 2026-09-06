package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bigTree is wide enough that a spot check picks a proper subset.
func bigTree(n int) map[string]file {
	tree := map[string]file{"Saves": {dir: true, perm: 0o755, mtime: fixed}}
	for i := range n {
		name := "Saves/map_" + string(rune('a'+i%26)) + "_" + itoa(i) + ".bin"
		tree[name] = file{
			body:  strings.Repeat("c", 1+i*3),
			perm:  0o644,
			mtime: fixed.Add(time.Duration(i) * time.Minute),
		}
	}
	return tree
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func spotArchive(t *testing.T) *Ref {
	t.Helper()
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, bigTree(80))
	return mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})
}

// TestSpotCheckIsASubsetAndIsReproducible: the sample has to be a real subset
// (or it is just a deep verify wearing a different name) and it has to be the
// same subset every time (or "verified yesterday, failed today" becomes a fact
// about the dice rather than about the bytes).
func TestSpotCheckIsASubsetAndIsReproducible(t *testing.T) {
	ref := spotArchive(t)
	man, err := ReadArchiveManifest(ref.Path)
	if err != nil {
		t.Fatal(err)
	}

	first := selectSpot(man.Members, DefaultSpot, ref.SHA256)
	second := selectSpot(man.Members, DefaultSpot, ref.SHA256)
	if len(first) != len(second) {
		t.Fatalf("two selections over the same archive picked %d and %d members", len(first), len(second))
	}
	for k := range first {
		if !second[k] {
			t.Fatalf("the selection is not reproducible: %s was picked once and not the other time", k)
		}
	}
	if len(first) >= countFiles(man.Members) {
		t.Fatalf("the spot check picked %d of %d members, which is a deep verify", len(first), countFiles(man.Members))
	}
	if len(first) != DefaultSpot {
		t.Errorf("the spot check picked %d members, want %d", len(first), DefaultSpot)
	}

	// A different archive digest must produce a different sample, or the seed
	// is not doing anything.
	other := selectSpot(man.Members, DefaultSpot, strings.Repeat("f", 64))
	same := 0
	for k := range first {
		if other[k] {
			same++
		}
	}
	if same == len(first) {
		t.Error("a different archive digest produced an identical sample; the PRNG is not seeded")
	}

	// The extremes are always in, because that is where a truncated or
	// mis-padded write shows up.
	var largest, smallest, newest Member
	for i, m := range man.Members {
		if m.Kind != KindFile {
			continue
		}
		if i == 0 || largest.Path == "" {
			largest, smallest, newest = m, m, m
		}
		if m.Size > largest.Size {
			largest = m
		}
		if m.Size < smallest.Size {
			smallest = m
		}
		if m.MTime.After(newest.MTime) {
			newest = m
		}
	}
	for _, m := range []Member{largest, smallest, newest} {
		if !first[m.Path] {
			t.Errorf("the spot check did not include %s, one of the largest/smallest/newest members", m.Path)
		}
	}
}

func TestSpotCheckExtractsAndComparesRealBytes(t *testing.T) {
	ref := spotArchive(t)
	scratch := t.TempDir()
	rep, err := Verify(context.Background(), ref.Path, VerifyOptions{
		Mode: ModeSpot, Spot: 6, Scratch: scratch, Now: func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a good archive failed a spot check: %v", rep.Problems)
	}
	if rep.Checked != 6 {
		t.Errorf("Checked = %d, want 6", rep.Checked)
	}
	// The scratch tree is cleaned up: it lives under
	// /var/lib/gvm/restore-scratch on a real box and a 2 GB member left there
	// every night is a disk-full incident.
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the spot check left %d entries in the scratch directory", len(entries))
	}
}

// TestVerifyCatchesACorruptMember distinguishes the two failure shapes an
// operator cares about: a member whose bytes changed, and a stream that will
// not decompress at all.
func TestVerifyCatchesACorruptMember(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, pzTree())
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src, Include: []string{"Server"}}},
	})

	corrupt := filepath.Join(dir, "corrupt.tar.gz")
	tamper(t, ref.Path, corrupt, "data/Server/servertest.ini", func(b []byte) []byte {
		out := append([]byte(nil), b...)
		out[0] ^= 0xff
		return out
	})

	t.Run("deep finds it", func(t *testing.T) {
		rep, err := Verify(context.Background(), corrupt, VerifyOptions{Mode: ModeDeep, Now: func() time.Time { return fixed }})
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() {
			t.Fatal("a deep verify passed an archive whose contents do not match its manifest")
		}
		if !strings.Contains(rep.Err().Error(), "servertest.ini") {
			t.Errorf("the report does not name the bad member: %v", rep.Err())
		}
		if !errors.Is(rep.Err(), ErrCorrupt) {
			t.Errorf("the report's error does not wrap ErrCorrupt: %v", rep.Err())
		}
	})

	t.Run("shallow does not, and says what it checked", func(t *testing.T) {
		// This is not a gap being papered over: shallow is documented as the
		// cheap mode, and asserting that it does NOT re-hash is what keeps
		// somebody from making it silently expensive later.
		rep, err := Verify(context.Background(), corrupt, VerifyOptions{Mode: ModeShallow, Now: func() time.Time { return fixed }})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Fatalf("shallow reported a problem it cannot have found: %v", rep.Problems)
		}
		if rep.Checked != 0 {
			t.Errorf("shallow hashed %d members; it is meant to cost seconds", rep.Checked)
		}
		if rep.SHA256 == "" {
			t.Error("shallow did not compute the whole-file digest, which is what it reports")
		}
	})

	t.Run("VerifyRef refuses to stamp a corrupt archive", func(t *testing.T) {
		r := &Ref{Path: corrupt, Instance: "pz-main", Class: ClassRoutine, CreatedAt: fixed}
		_, err := VerifyRef(context.Background(), r, VerifyOptions{Mode: ModeDeep, Now: func() time.Time { return fixed }})
		if err == nil {
			t.Fatal("VerifyRef accepted a corrupt archive")
		}
		if r.Verified() {
			t.Fatal("VerifyRef stamped a Ref for an archive that failed. That Ref would then " +
				"authorize a destroy")
		}
	})
}

// TestVerifyCatchesATruncatedFile is the failure archive-before-destroy exists
// to prevent: the bytes stopped arriving and nobody noticed.
func TestVerifyCatchesATruncatedFile(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, bigTree(40))
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})

	b, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}

	// Two shapes, because they fail in two different places: a tail
	// truncation leaves a readable manifest and an unfinished stream, which is
	// what a full disk produces; a head truncation leaves nothing readable at
	// all, which is what an interrupted copy produces.
	cases := map[string][]byte{
		"tail truncated": b[:len(b)*95/100],
		"head truncated": b[:min(400, len(b)/4)],
	}
	for name, body := range cases {
		truncated := filepath.Join(dir, "truncated-"+strings.ReplaceAll(name, " ", "-")+".tar.gz")
		if err := os.WriteFile(truncated, body, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []Mode{ModeShallow, ModeSpot, ModeDeep} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				rep, err := Verify(context.Background(), truncated, VerifyOptions{
					Mode: mode, Scratch: t.TempDir(), Now: func() time.Time { return fixed },
				})
				if err != nil {
					t.Fatal(err)
				}
				if rep.OK() {
					t.Fatalf("%s passed a %s archive", mode, name)
				}
			})
		}
	}
}

// TestVerifyRefRejectsADigestThatMoved catches an archive replaced under gvm's
// feet between the write and the check.
func TestVerifyRefRejectsADigestThatMoved(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})
	ref.SHA256 = strings.Repeat("0", 64)

	_, err := VerifyRef(context.Background(), ref, VerifyOptions{Mode: ModeShallow, Now: func() time.Time { return fixed }})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("VerifyRef = %v, want ErrCorrupt when the file no longer hashes to what was written", err)
	}
	if ref.Verified() {
		t.Fatal("the Ref was stamped anyway")
	}
}

// TestVerifyRefRefusesAnArchiveThatVouchesForNothing is the receipt rule.
//
// VerifyRef is the only thing that stamps VerifiedAt, and a stamped Ref is what
// archive.Destroy, reconcile.Remove and retention.Prune all accept as authority
// to delete a world. An archive with no manifest -- a bash-suite tarball, or a
// well-formed tarball with no members at all -- has had none of its contents
// compared against anything, so it must not be able to mint that authority.
// Verify stays permissive on the same input, because `gvm backup verify` was
// asked a question about a file it is not going to destroy anything with.
func TestVerifyRefRefusesAnArchiveThatVouchesForNothing(t *testing.T) {
	t.Run("a legacy bash-suite tarball", func(t *testing.T) {
		legacy := filepath.Join("testdata", "legacy-pz-backup.tar.gz")
		ref := &Ref{Path: legacy, Class: ClassRoutine}

		_, err := VerifyRef(context.Background(), ref, VerifyOptions{
			Mode: ModeSpot, Scratch: t.TempDir(), Now: func() time.Time { return fixed },
		})
		if !errors.Is(err, ErrNoManifest) {
			t.Fatalf("VerifyRef(legacy) = %v, want ErrNoManifest: no member content was checked, "+
				"so this cannot authorise a destroy", err)
		}
		if ref.Verified() {
			t.Fatal("a tarball with no digest table was stamped as a destroy receipt")
		}

		// The permissive half: the same file still reports as legacy-not-corrupt.
		rep, err := Verify(context.Background(), legacy, VerifyOptions{Mode: ModeShallow, Now: func() time.Time { return fixed }})
		if err != nil {
			t.Fatalf("Verify(legacy) = %v, want a report; refusing to look at it is not the answer", err)
		}
		if !rep.Legacy || !rep.OK() {
			t.Fatalf("Verify(legacy): Legacy=%v OK=%v, want a clean legacy report", rep.Legacy, rep.OK())
		}
	})

	t.Run("a well-formed tarball with no members", func(t *testing.T) {
		// Exactly what Create refuses to write: "a backup that archives nothing
		// must fail loudly rather than leave a valid, empty tarball behind".
		// Nothing stops one arriving from somewhere else.
		path := filepath.Join(t.TempDir(), "pz-main-20260904-182211-routine.tar.gz")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(f)
		tw := tar.NewWriter(zw)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		ref := &Ref{Path: path, Class: ClassRoutine}
		_, err = VerifyRef(context.Background(), ref, VerifyOptions{
			Mode: ModeSpot, Scratch: t.TempDir(), Now: func() time.Time { return fixed },
		})
		if err == nil {
			t.Fatal("VerifyRef stamped a receipt for a tarball containing nothing at all")
		}
		if ref.Verified() {
			t.Fatal("the empty tarball's Ref was stamped, and would now satisfy Destroy's guard")
		}
	})
}

// TestSelectSpotTerminatesWithDuplicateMemberPaths.
//
// The draw loop bounds itself with `len(pick) < len(files)`, but pick is keyed
// by Member.Path while files counts ENTRIES -- so a digest table naming one
// path twice makes the ceiling unreachable and the loop spins forever at 100%
// CPU. selectSpot takes no context and has no iteration cap, so a deadlined
// verify cannot break out of it, and `gvm destroy --archive` runs ModeSpot over
// whatever path the operator passes.
//
// Nothing upstream rejects the duplicate: ReadArchiveManifest checks the table's
// digest and its count but not path uniqueness, and Manifest.Validate rejects a
// duplicate ROOT prefix while saying nothing about members.
func TestSelectSpotTerminatesWithDuplicateMemberPaths(t *testing.T) {
	members := []Member{
		{Path: "a", Kind: KindFile, Size: 1},
		{Path: "a", Kind: KindFile, Size: 2},
		{Path: "b", Kind: KindFile, Size: 3},
	}
	sha := strings.Repeat("ab", 32)

	done := make(chan map[string]bool, 1)
	go func() { done <- selectSpot(members, DefaultSpot, sha) }()
	select {
	case pick := <-done:
		if len(pick) != 2 {
			t.Errorf("pick = %v, want the two distinct paths", pick)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selectSpot did not return: the draw is bounded by the number of table ENTRIES " +
			"while the picked set is keyed by PATH, so a duplicated path makes the bound unreachable")
	}
}

// TestReadArchiveManifestRejectsADuplicateMemberPath closes the same hole at
// the door, where it also stops Verify's `want` map silently keeping only the
// last of the pair: a table naming one path twice cannot describe a tar stream
// unambiguously.
func TestReadArchiveManifestRejectsADuplicateMemberPath(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{
		"one.bin": {body: "1", perm: 0o644, mtime: fixed},
		"two.bin": {body: "22", perm: 0o644, mtime: fixed},
	})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})

	// Rewrite the member table so one path appears twice, and re-stamp the head
	// manifest's digest for it -- otherwise the existing digest gate refuses
	// first and this asserts nothing about uniqueness.
	dup := filepath.Join(dir, "dup.tar.gz")
	var table []byte
	tamper(t, ref.Path, dup, MembersMember, func(b []byte) []byte {
		var mf memberFile
		if err := json.Unmarshal(b, &mf); err != nil {
			t.Fatal(err)
		}
		mf.Members[1].Path = mf.Members[0].Path
		out, err := json.MarshalIndent(mf, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, '\n')
		table = out
		return out
	})
	final := filepath.Join(dir, "dup2.tar.gz")
	tamper(t, dup, final, ManifestMember, func(b []byte) []byte {
		var man Manifest
		if err := json.Unmarshal(b, &man); err != nil {
			t.Fatal(err)
		}
		man.MemberTable.SHA256 = hexSHA256(table)
		man.MemberTable.Bytes = int64(len(table))
		out, err := json.Marshal(&man)
		if err != nil {
			t.Fatal(err)
		}
		return append(out, '\n')
	})

	if _, err := ReadArchiveManifest(final); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadArchiveManifest = %v, want ErrCorrupt for a table naming one member twice", err)
	}
}
