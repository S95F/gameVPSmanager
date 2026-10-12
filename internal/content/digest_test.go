package content

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func treeFS() fstest.MapFS {
	return fstest.MapFS{
		"item/mods/Thing/mod.info":            &fstest.MapFile{Data: []byte("id=Thing\n")},
		"item/mods/Thing/media/scripts/a.txt": &fstest.MapFile{Data: []byte("alpha")},
		"item/mods/Thing/media/scripts/b.txt": &fstest.MapFile{Data: []byte("beta")},
	}
}

func TestDigestTreeIsDeterministicAndCountsMembers(t *testing.T) {
	ctx := context.Background()
	a, err := DigestTree(ctx, treeFS(), "item")
	if err != nil {
		t.Fatalf("DigestTree: %v", err)
	}
	if a.Members != 3 {
		t.Errorf("members = %d, want 3", a.Members)
	}
	if a.Bytes != int64(len("id=Thing\n")+len("alpha")+len("beta")) {
		t.Errorf("bytes = %d", a.Bytes)
	}
	if got := a.Digest.Algo(); got != AlgoTreeSHA256 {
		t.Errorf("algo = %q; a tree is not a blob the store can verify by hashing", got)
	}
	if _, err := ParseDigest(a.Digest); err != nil {
		t.Errorf("the tree digest does not parse: %v", err)
	}

	b, err := DigestTree(ctx, treeFS(), "item")
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Error("two digests of the same tree differ; a digest that depended on walk order " +
			"would read as tampering on the machine that checks it")
	}
}

// TestDigestTreeIsRootRelative is what lets a digest recorded under one
// workshop root verify under another: the manifest declares two, and the
// server may use either.
func TestDigestTreeIsRootRelative(t *testing.T) {
	ctx := context.Background()
	under := fstest.MapFS{}
	for name, f := range treeFS() {
		under["a/deeper/path/"+name] = f
	}

	here, err := DigestTree(ctx, treeFS(), "item")
	if err != nil {
		t.Fatal(err)
	}
	there, err := DigestTree(ctx, under, "a/deeper/path/item")
	if err != nil {
		t.Fatal(err)
	}
	if here.Digest != there.Digest {
		t.Fatal("the same tree under a different root hashed differently")
	}
}

func TestDigestTreeNoticesEveryKindOfChange(t *testing.T) {
	ctx := context.Background()
	base, err := DigestTree(ctx, treeFS(), "item")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		mut  func(fstest.MapFS)
	}{
		{"content changed", func(m fstest.MapFS) {
			m["item/mods/Thing/media/scripts/a.txt"] = &fstest.MapFile{Data: []byte("ALPHA")}
		}},
		{"file added", func(m fstest.MapFS) {
			m["item/mods/Thing/media/scripts/c.txt"] = &fstest.MapFile{Data: []byte("gamma")}
		}},
		{"file removed", func(m fstest.MapFS) {
			delete(m, "item/mods/Thing/media/scripts/b.txt")
		}},
		{"file renamed", func(m fstest.MapFS) {
			m["item/mods/Thing/media/scripts/z.txt"] = m["item/mods/Thing/media/scripts/a.txt"]
			delete(m, "item/mods/Thing/media/scripts/a.txt")
		}},
		{"symlink added", func(m fstest.MapFS) {
			// Recorded rather than skipped: a symlink is exactly the member
			// worth noticing, because it is how a tree points at bytes
			// outside itself.
			m["item/mods/Thing/link"] = &fstest.MapFile{Mode: fs.ModeSymlink,
				Data: []byte("media/scripts/a.txt")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := treeFS()
			tc.mut(m)
			got, err := DigestTree(ctx, m, "item")
			if err != nil {
				t.Fatalf("DigestTree: %v", err)
			}
			if got.Digest == base.Digest {
				t.Fatal("the digest did not move")
			}
		})
	}
}

// TestDigestTreeCommitsToASymlinkTarget: a type letter alone would let a link
// be repointed at another tree's bytes -- or at /etc/shadow -- without the
// digest moving.
func TestDigestTreeCommitsToASymlinkTarget(t *testing.T) {
	ctx := context.Background()
	with := func(target string) Digest {
		t.Helper()
		m := treeFS()
		m["item/mods/Thing/link"] = &fstest.MapFile{Mode: fs.ModeSymlink, Data: []byte(target)}
		got, err := DigestTree(ctx, m, "item")
		if err != nil {
			t.Fatalf("DigestTree: %v", err)
		}
		return got.Digest
	}
	if with("media/scripts/a.txt") == with("/etc/shadow") {
		t.Fatal("repointing a symlink left the tree digest unchanged")
	}
}

func TestVerifyTreeShallowChecksPresenceAndNonEmptiness(t *testing.T) {
	ctx := context.Background()

	// Present and non-empty: the positive acquisition signal.
	if _, err := VerifyTree(ctx, treeFS(), "item", "", false); err != nil {
		t.Errorf("a downloaded item failed the shallow check: %v", err)
	}

	// Absent entirely.
	_, err := VerifyTree(ctx, treeFS(), "9999", "", false)
	if !errors.Is(err, ErrUnaccounted) {
		t.Errorf("an absent item = %v, want the unaccounted refusal", err)
	}

	// Present and empty: a download that started and did not finish. NOT the
	// same as absent, and treating it as acquired is how a mod silently never
	// loads.
	empty := fstest.MapFS{"item/keep": &fstest.MapFile{Mode: fs.ModeDir}}
	if _, err := VerifyTree(ctx, empty, "item", "", false); !errors.Is(err, ErrUnaccounted) {
		t.Errorf("an empty item directory passed verification: %v", err)
	}
}

func TestVerifyTreeDeepCatchesAMismatch(t *testing.T) {
	ctx := context.Background()
	recorded, err := DigestTree(ctx, treeFS(), "item")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := VerifyTree(ctx, treeFS(), "item", recorded.Digest, true); err != nil {
		t.Errorf("an unchanged tree failed deep verification: %v", err)
	}

	changed := treeFS()
	changed["item/mods/Thing/media/scripts/a.txt"] = &fstest.MapFile{Data: []byte("substituted")}
	_, err = VerifyTree(ctx, changed, "item", recorded.Digest, true)
	if !errors.Is(err, ErrDigest) {
		t.Fatalf("a substituted file passed deep verification: %v", err)
	}
	// Both digests in the message: "mismatch" alone sends the operator to a
	// shell to work out which one gvm thought it had.
	if !strings.Contains(err.Error(), recorded.Digest.Short()) {
		t.Errorf("the error does not name the recorded digest: %v", err)
	}

	// No recorded digest with deep true is not a failure: there is nothing to
	// compare against, so the computed digest comes back for recording.
	got, err := VerifyTree(ctx, treeFS(), "item", "", true)
	if err != nil {
		t.Fatalf("deep verify with nothing recorded: %v", err)
	}
	if got.Digest != recorded.Digest {
		t.Error("the returned digest is not the computed one")
	}
}

func TestTreePresentTreatsAMissingRootAsNormal(t *testing.T) {
	present, err := TreePresent(treeFS(), "9999")
	if err != nil {
		t.Fatalf("a missing root is a normal answer, not an error: %v", err)
	}
	if present {
		t.Error("TreePresent invented a directory")
	}
	// A file where a directory was expected is not a tree.
	present, err = TreePresent(treeFS(), "item/mods/Thing/mod.info")
	if err != nil || present {
		t.Errorf("a regular file reported as a tree: %v, %v", present, err)
	}
	if _, err := TreePresent(nil, "item"); err == nil {
		t.Error("TreePresent with no filesystem returned an answer")
	}
}

func TestDigestTreeRefusesAnUnreadableSubtree(t *testing.T) {
	// A digest taken over the part that happened to be readable is a digest
	// of something else, and it would verify against nothing.
	m := treeFS()
	m["item/mods/Broken/mod.info"] = &fstest.MapFile{Data: []byte("id=Broken\n")}
	if _, err := DigestTree(context.Background(), errorFS{m, "item/mods/Broken/mod.info"},
		"item"); err == nil {
		t.Fatal("DigestTree ignored an unreadable member")
	}
}

// errorFS fails to open one named path, which is what a permissions problem or
// a disappearing file looks like mid-walk.
type errorFS struct {
	fstest.MapFS
	fail string
}

func (e errorFS) Open(name string) (fs.File, error) {
	if name == e.fail {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return e.MapFS.Open(name)
}

func TestCleanFSPathNormalisesBothSeparators(t *testing.T) {
	// A rule that behaves differently on the Windows development machine than
	// on the Debian host is not a rule that has been tested.
	for _, tc := range []struct{ in, want string }{
		{"", "."},
		{"/", "."},
		{"item", "item"},
		{"/item/", "item"},
		{`item\mods`, "item/mods"},
		{"./item", "item"},
		{"  item  ", "item"},
	} {
		if got := cleanFSPath(tc.in); got != tc.want {
			t.Errorf("cleanFSPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
