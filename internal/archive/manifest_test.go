package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestArchivesAre0600 is D-44: /var/backups/gvm/<name> is 0700 root:root and
// the tarballs in it are 0600, because a compromised mod must not be able to
// read every world the server has ever had. install.sh chowned the backup
// directory to the game user; that is the one thing in it deliberately not
// ported.
func TestArchivesAre0600(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})
	fi, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	samePerm(t, ref.Path, fi.Mode(), platformPerm(ArchiveFileMode))
}

// TestPeekRefusesAManifestFromAFutureGvm.
//
// Half-understanding a manifest is worse than an honest refusal: the whole
// point of the file is that an operator can restore from the tarball with no
// database, no gvm install and no notes.
func TestManifestValidationRefusesWhatItCannotActOn(t *testing.T) {
	cases := map[string]Manifest{
		"future schema":  {Schema: SchemaVersion + 1, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed},
		"no instance":    {Schema: SchemaVersion, Class: ClassRoutine, CreatedAt: fixed},
		"unknown class":  {Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: "maybe", CreatedAt: fixed},
		"zero createdat": {Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine},
		"prefix without a slash": {
			Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed,
			Roots: []Root{{Prefix: "data", Base: "/home/pzserver/Zomboid"}},
		},
		"root without a base": {
			Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed,
			Roots: []Root{{Prefix: "data/"}},
		},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
	valid := Manifest{
		Schema: SchemaVersion, Instance: Subject{Name: "pz-main"}, Class: ClassRoutine, CreatedAt: fixed,
		Roots: []Root{{Prefix: "data/", Base: "/home/pzserver/Zomboid"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate refused a well-formed manifest: %v", err)
	}
}

// TestPeekSaysWhichKindOfMissingItIs. "No manifest at all" and "a manifest
// that does not fit in the peek budget" send the operator in different
// directions, so they are different errors.
func TestPeekSaysWhichKindOfMissingItIs(t *testing.T) {
	legacy := filepath.Join("testdata", "legacy-pz-backup.tar.gz")
	f, err := os.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := PeekManifest(f); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("PeekManifest over a bash-suite tarball = %v, want ErrNoManifest", err)
	}

	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]file{"world.bin": {body: "w", perm: 0o644, mtime: fixed}})
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src}},
	})
	g, err := os.Open(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	// Half the budget: the manifest is intact but the reader was not given
	// enough of the file.
	if _, err := PeekManifest(io.LimitReader(g, 64)); !errors.Is(err, ErrHeadTooLarge) {
		t.Fatalf("a starved peek = %v, want ErrHeadTooLarge rather than \"no manifest\"", err)
	}
}

// TestMemberTableDigestIsChecked: the head manifest is what a peek trusts, and
// it has to be able to say whether the table it later reads is the one it
// describes.
func TestMemberTableDigestIsChecked(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	writeTree(t, src, pzTree())
	ref := mustCreate(t, Options{
		Dir: dir, Instance: "pz-main", Class: ClassRoutine, Reason: "nightly",
		Now:     func() time.Time { return fixed },
		Sources: []Source{{Prefix: "data/", Base: src, Include: []string{"Server"}}},
	})

	swapped := filepath.Join(dir, "swapped.tar.gz")
	tamper(t, ref.Path, swapped, MembersMember, func(b []byte) []byte {
		var mf memberFile
		if err := json.Unmarshal(b, &mf); err != nil {
			t.Fatal(err)
		}
		// A plausible edit: somebody relaxes one member's digest so a corrupt
		// archive verifies.
		for i := range mf.Members {
			if strings.HasSuffix(mf.Members[i].Path, "servertest.ini") {
				mf.Members[i].SHA256 = strings.Repeat("0", 64)
			}
		}
		out, err := json.MarshalIndent(mf, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		return append(out, '\n')
	})

	if _, err := ReadArchiveManifest(swapped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadArchiveManifest over an edited digest table = %v, want ErrCorrupt", err)
	}
	rep, err := Verify(context.Background(), swapped, VerifyOptions{Mode: ModeDeep, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("verify passed an archive whose digest table had been edited")
	}
}

// TestHeadManifestFitsTheBudgetWithARealisticInstance keeps the 8 KB promise
// honest against the fields a real archive carries rather than against an
// empty struct.
func TestHeadManifestFitsTheBudgetWithARealisticInstance(t *testing.T) {
	m := Manifest{
		Schema: SchemaVersion, GVMVersion: "v0.4.1", GVMCommit: "9f2c1a7",
		CreatedAt: fixed,
		CreatedBy: Creator{Kind: "root_cli", Reason: "adopt cutover"},
		Instance: Subject{
			Name: "pz-main", Game: "projectzomboid",
			GameManifestSHA256: strings.Repeat("1b", 32), GameManifestRevision: 7,
			UnixUser: "pzserver",
			Paths:    map[string]string{"install": "/opt/pzserver", "data": "/home/pzserver/Zomboid"},
		},
		Class:    ClassPreserve,
		Reason:   "adopt-pre-cutover",
		SpecYAML: strings.Repeat("apiVersion: gvm/v1\nkind: Instance\nmetadata:\n  name: pz-main\n", 20),
		WorldFingerprint: &Fingerprint{
			Paths:     []string{"Saves/Multiplayer/servertest", "Saves/Multiplayer/servertest_player"},
			Files:     8412,
			Dirs:      191,
			Bytes:     2251799813,
			NewestMod: fixed,
			Digest:    "sha256:" + strings.Repeat("7c", 32),
		},
		Roots: []Root{
			{Prefix: "data/", Base: "/home/pzserver/Zomboid"},
			{Prefix: "etcgvm/", Base: "/etc/gvm"},
		},
		MemberTable:             &MemberTable{Path: MembersMember, Count: 8412, Bytes: 1_600_000, SHA256: strings.Repeat("ab", 32)},
		SecretsIncluded:         true,
		SecretsRedactedFromSpec: []string{"Password", "RCONPassword"},
	}
	b, err := m.marshalHead()
	if err != nil {
		t.Fatalf("a realistic head manifest does not fit the budget: %v", err)
	}
	if len(b) > maxHeadJSON {
		t.Fatalf("head is %d bytes, budget %d", len(b), maxHeadJSON)
	}
	// And it survives the round trip through JSON with every field intact.
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Instance.GameManifestRevision != 7 || back.WorldFingerprint.Files != 8412 ||
		len(back.Roots) != 2 || !back.SecretsIncluded {
		t.Fatalf("the head manifest lost fields on the round trip: %+v", back)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Error("the manifest member does not end in a newline; `tar -xOf ... | jq` wants one")
	}
}
