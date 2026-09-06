package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// worldBefore is a PZ world as it stands the moment before an adopt cutover.
func worldBefore(t *testing.T, base string, at time.Time) {
	t.Helper()
	writeTree(t, base, map[string]file{
		"Saves":                                     {dir: true, perm: 0o755, mtime: at},
		"Saves/Multiplayer":                         {dir: true, perm: 0o755, mtime: at},
		"Saves/Multiplayer/servertest":              {dir: true, perm: 0o755, mtime: at},
		"Saves/Multiplayer/servertest/map_t.bin":    {body: "chunk table", perm: 0o644, mtime: at},
		"Saves/Multiplayer/servertest/map_1_1.bin":  {body: "chunk 1,1", perm: 0o644, mtime: at},
		"Saves/Multiplayer/servertest/map_2_1.bin":  {body: "chunk 2,1", perm: 0o644, mtime: at},
		"Saves/Multiplayer/servertest/players.db":   {body: "players", perm: 0o644, mtime: at},
		"Saves/Multiplayer/servertest_player":       {dir: true, perm: 0o755, mtime: at},
		"Saves/Multiplayer/servertest_player/1.bin": {body: "steve", perm: 0o644, mtime: at},
	})
}

// TestFingerprintSupersetSemantics is D-34.
//
// A digest comparison would be the obvious implementation and it would be
// wrong: the server writes to the world on both a clean stop and a start, so
// the digest changes on every successful cutover and the check would
// false-alarm every single time -- and then be switched off. The superset test
// still catches the only catastrophe that matters.
func TestFingerprintSupersetSemantics(t *testing.T) {
	ctx := context.Background()
	roots := []string{"Saves/Multiplayer/servertest", "Saves/Multiplayer/servertest_player"}

	t.Run("a normal stop and start passes", func(t *testing.T) {
		base := t.TempDir()
		worldBefore(t, base, fixed)
		before, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}

		// What actually happens across a cutover: every existing chunk is
		// rewritten, a couple of new ones appear, and the mtimes move.
		after := fixed.Add(20 * time.Minute)
		writeTree(t, base, map[string]file{
			"Saves/Multiplayer/servertest/map_t.bin":   {body: "chunk table rewritten on stop", perm: 0o644, mtime: after},
			"Saves/Multiplayer/servertest/map_3_1.bin": {body: "a chunk the players just explored", perm: 0o644, mtime: after},
			"Saves/Multiplayer/servertest/players.db":  {body: "players, now with a new row", perm: 0o644, mtime: after},
		})
		post, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}
		if post.Digest == before.Digest {
			t.Fatal("the digests matched, so this fixture is not exercising the case D-34 is " +
				"about; the point is that they never match")
		}
		if err := before.Contains(post); err != nil {
			t.Fatalf("a normal stop/start failed verification: %v", err)
		}
	})

	t.Run("a regenerated empty world fails and names the missing paths", func(t *testing.T) {
		base := t.TempDir()
		worldBefore(t, base, fixed)
		before, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}

		// The cutover used the wrong servername, so PZ generated a fresh world
		// beside the real one. The directory exists, is being written to, and
		// holds nothing that was there before.
		if err := os.RemoveAll(filepath.Join(base, "Saves", "Multiplayer", "servertest")); err != nil {
			t.Fatal(err)
		}
		after := fixed.Add(20 * time.Minute)
		writeTree(t, base, map[string]file{
			"Saves/Multiplayer/servertest":           {dir: true, perm: 0o755, mtime: after},
			"Saves/Multiplayer/servertest/map_t.bin": {body: "a brand new chunk table", perm: 0o644, mtime: after},
		})
		post, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}

		err = before.Contains(post)
		if err == nil {
			t.Fatal("a freshly generated empty world passed verification. This is the exact " +
				"catastrophe D-34 exists to catch")
		}
		var missing *MissingPathsError
		if !errors.As(err, &missing) {
			t.Fatalf("Contains = %v, want a *MissingPathsError", err)
		}
		for _, want := range []string{
			"Saves/Multiplayer/servertest/map_1_1.bin",
			"Saves/Multiplayer/servertest/players.db",
		} {
			if !contains(missing.Missing, want) {
				t.Errorf("Missing does not name %s: %v", want, missing.Missing)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error message does not name %s; an operator's next question is "+
					"always \"which files\":\n%s", want, err)
			}
		}
		// map_t.bin exists in both, so it is not missing -- the check is a set
		// comparison, not a content one.
		if contains(missing.Missing, "Saves/Multiplayer/servertest/map_t.bin") {
			t.Error("map_t.bin was reported missing; it exists in both worlds with different " +
				"contents, and Contains must not be comparing contents")
		}
	})

	t.Run("a server that never wrote fails on the mtime", func(t *testing.T) {
		base := t.TempDir()
		worldBefore(t, base, fixed)
		before, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing changed at all: the unit came up against some other
		// directory, or came up read-only.
		post, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}
		err = before.Contains(post)
		if !errors.Is(err, ErrWorldNotWritten) {
			t.Fatalf("Contains = %v, want ErrWorldNotWritten: the file set is intact but the "+
				"server has not written, which means it is not running against this directory", err)
		}
	})

	t.Run("a summary-only fingerprint refuses rather than passing", func(t *testing.T) {
		base := t.TempDir()
		worldBefore(t, base, fixed)
		before, err := TakeFingerprint(ctx, base, roots)
		if err != nil {
			t.Fatal(err)
		}
		summary := before.summary()
		if len(summary.Entries) != 0 {
			t.Fatal("summary() kept the path set")
		}
		if err := summary.Contains(before); !errors.Is(err, ErrNoEntries) {
			t.Fatalf("Contains on a summary = %v, want ErrNoEntries. A fingerprint read back "+
				"from an archive manifest carries no path set, and answering \"fine\" would be "+
				"a check that always passes", err)
		}
	})
}

// TestFingerprintSummaryIsWhatLandsInTheManifest keeps the head budget honest:
// the path set is what makes an 8412-file world's manifest 1.5 MB.
func TestFingerprintSummaryIsWhatLandsInTheManifest(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	worldBefore(t, src, fixed)
	fp, err := TakeFingerprint(context.Background(), src, []string{"Saves/Multiplayer/servertest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Entries) == 0 {
		t.Fatal("the fingerprint recorded no entries")
	}

	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassPreserve, Reason: "adopt-pre-cutover",
		Now:         func() time.Time { return fixed },
		Fingerprint: fp,
		Sources:     []Source{{Prefix: "data/", Base: src}},
	})
	man, err := PeekManifestFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	if man.WorldFingerprint == nil {
		t.Fatal("the manifest carries no world fingerprint")
	}
	if len(man.WorldFingerprint.Entries) != 0 {
		t.Fatalf("the manifest carries %d fingerprint entries; a real world has 8412 of them and "+
			"they do not fit in the %d-byte peek budget", len(man.WorldFingerprint.Entries), PeekBytes)
	}
	if man.WorldFingerprint.Files != fp.Files || man.WorldFingerprint.Digest != fp.Digest {
		t.Errorf("the summary lost its counts: %+v", man.WorldFingerprint)
	}
	// And the caller's own value is untouched: summary() must not empty it.
	if len(fp.Entries) == 0 {
		t.Fatal("Create emptied the caller's fingerprint, which is the value the post-cutover " +
			"comparison is about to be made against")
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
