package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// gameYAML is the smallest file the loader has to accept. Completeness is
// validation's problem; this is about resolution, provenance and refusal.
func gameYAML(id string, rev int, extra ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: gvm/v1\nkind: Game\nmetadata:\n  id: %s\n  name: %s\n  revision: %d\nspec:\n  driverABI: 1\n  runtime:\n    exec: ./start-server.sh\n", id, id, rev)
	for _, e := range extra {
		b.WriteString(e)
	}
	return b.String()
}

func writeGame(t *testing.T, dir, id string, rev int, extra ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".yaml")
	body := gameYAML(id, rev, extra...)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return body
}

// threeLayers builds a catalog whose layers are individually populated by the
// caller, mirroring a real box: embedded floor, packaged directory, /etc
// override.
type threeLayers struct {
	cat      *Catalog
	shareDir string
	etcDir   string
	embedded fstest.MapFS
}

func newThreeLayers(t *testing.T) *threeLayers {
	t.Helper()
	root := t.TempDir()
	l := &threeLayers{
		shareDir: filepath.Join(root, "share"),
		etcDir:   filepath.Join(root, "etc"),
		embedded: fstest.MapFS{},
	}
	if err := os.MkdirAll(l.shareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	l.cat = NewCatalog(
		EmbeddedSource(l.embedded, "embedded games"),
		DirSource(OriginPackage, l.shareDir),
		DirSource(OriginOverride, l.etcDir),
	)
	return l
}

func TestCatalogHighestOriginWins(t *testing.T) {
	tests := []struct {
		name       string
		embedded   int // revision, 0 = layer absent
		shipped    int
		override   int
		wantOrigin Origin
		wantRev    int
	}{
		{"only the floor", 5, 0, 0, OriginEmbedded, 5},
		{"packaged shadows the floor", 5, 7, 0, OriginPackage, 7},
		{"override shadows both", 5, 7, 9, OriginOverride, 9},
		{"override with a lower revision still wins", 5, 7, 2, OriginOverride, 2},
		{"override over the bare floor", 5, 0, 9, OriginOverride, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newThreeLayers(t)
			if tc.embedded > 0 {
				l.embedded["projectzomboid.yaml"] = &fstest.MapFile{
					Data: []byte(gameYAML("projectzomboid", tc.embedded)),
				}
			}
			if tc.shipped > 0 {
				writeGame(t, l.shareDir, "projectzomboid", tc.shipped)
			}
			if tc.override > 0 {
				writeGame(t, l.etcDir, "projectzomboid", tc.override)
			}

			m, diags, err := l.cat.Load("projectzomboid", RootCLIAuthorizer{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected errors: %s", diags)
			}
			if m.Origin != tc.wantOrigin || m.Metadata.Revision != tc.wantRev {
				t.Errorf("winner = %s revision %d, want %s revision %d",
					m.Origin, m.Metadata.Revision, tc.wantOrigin, tc.wantRev)
			}
		})
	}
}

func TestCatalogOrdersSourcesByRankNotByArgumentOrder(t *testing.T) {
	// A caller listing the layers in the wrong order must not be able to invert
	// the shadowing rule.
	root := t.TempDir()
	share := filepath.Join(root, "share")
	etc := filepath.Join(root, "etc")
	writeGame(t, share, "valheim", 3)
	writeGame(t, etc, "valheim", 4)

	cat := NewCatalog(
		DirSource(OriginPackage, share),
		DirSource(OriginOverride, etc),
	)
	m, _, err := cat.Load("valheim", RootCLIAuthorizer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Origin != OriginOverride {
		t.Errorf("origin = %s, want %s", m.Origin, OriginOverride)
	}
}

func TestLoadRecordsProvenanceFromTheFileNotTheContent(t *testing.T) {
	l := newThreeLayers(t)
	body := writeGame(t, l.shareDir, "valheim", 3)

	m, _, err := l.cat.Load("valheim", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); m.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q (the digest of the file's own bytes)", m.SHA256, want)
	}
	if string(m.Raw) != body {
		t.Errorf("Raw is not the file verbatim:\n got %q\nwant %q", m.Raw, body)
	}
	if want := filepath.Join(l.shareDir, "valheim.yaml"); m.SourceRef != want {
		t.Errorf("SourceRef = %q, want %q", m.SourceRef, want)
	}
}

func TestManifestCannotClaimItsOwnProvenance(t *testing.T) {
	// A file that could name its own origin could claim to be the approved
	// override and skip D-19 entirely.
	l := newThreeLayers(t)
	writeGame(t, l.shareDir, "valheim", 3,
		"origin: override\nsourceRef: /etc/gvm/games/valheim.yaml\nsha256: deadbeef\n")

	m, _, err := l.cat.Load("valheim", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Origin != OriginPackage {
		t.Errorf("origin = %q: the file's own `origin:` key was believed", m.Origin)
	}
	if m.SHA256 == "deadbeef" {
		t.Error("sha256 came from the file rather than from its bytes")
	}
}

// recordingAuthorizer captures what it was asked to approve.
type recordingAuthorizer struct {
	id, sha string
	err     error
}

func (a *recordingAuthorizer) ApproveOverride(id, sha string) error {
	a.id, a.sha = id, sha
	return a.err
}

func TestOverrideApprovalIsCheckedAgainstTheExactBytes(t *testing.T) {
	l := newThreeLayers(t)
	body := writeGame(t, l.etcDir, "valheim", 9)

	auth := &recordingAuthorizer{}
	m, _, err := l.cat.Load("valheim", auth)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if auth.id != "valheim" {
		t.Errorf("authorizer saw id %q", auth.id)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); auth.sha != want {
		t.Errorf("authorizer saw sha %q, want %q", auth.sha, want)
	}
	if m.Origin != OriginOverride {
		t.Errorf("origin = %s", m.Origin)
	}
}

func TestUnapprovedOverrideIsSkippedLoudlyNotFatally(t *testing.T) {
	// D-19: a box whose Postgres is down still boots its games off the packaged
	// manifest, and the operator is told which file was ignored.
	tests := []struct {
		name     string
		auth     OverrideAuthorizer
		wantWord string
	}{
		{"no approval recorded", DenyOverrides{}, "not approved"},
		{"no authorizer wired up", nil, "not approved"},
		{"approval store unreachable", &recordingAuthorizer{err: errors.New("dial /run/gvm/admin.sock: connection refused")}, "could not be checked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newThreeLayers(t)
			writeGame(t, l.shareDir, "valheim", 3)
			writeGame(t, l.etcDir, "valheim", 9)

			m, diags, err := l.cat.Load("valheim", tc.auth)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if m.Origin != OriginPackage || m.Metadata.Revision != 3 {
				t.Fatalf("winner = %s revision %d, want the packaged revision 3",
					m.Origin, m.Metadata.Revision)
			}
			if diags.HasErrors() {
				t.Errorf("a skipped override must warn, not error: %s", diags)
			}
			if len(diags) != 1 {
				t.Fatalf("want exactly one warning, got %d: %s", len(diags), diags)
			}
			d := diags[0]
			if d.Sev != SevWarning || !strings.Contains(d.Msg, tc.wantWord) {
				t.Errorf("diag = %q, want a warning containing %q", d.String(), tc.wantWord)
			}
			if !strings.Contains(d.File, "valheim.yaml") {
				t.Errorf("diag names %q, want the skipped override file", d.File)
			}
		})
	}
}

func TestEveryCandidateSkippedIsAnError(t *testing.T) {
	l := newThreeLayers(t)
	writeGame(t, l.etcDir, "valheim", 9)

	_, diags, err := l.cat.Load("valheim", DenyOverrides{})
	if !errors.Is(err, ErrNoUsableManifest) {
		t.Fatalf("err = %v, want ErrNoUsableManifest", err)
	}
	if len(diags) == 0 {
		t.Error("no diagnostic explained why nothing was usable")
	}
}

func TestBrokenOverrideDoesNotFallThroughToThePackagedManifest(t *testing.T) {
	// Silently running different argv because the operator's own edit did not
	// parse is precisely the surprise this design exists to prevent.
	l := newThreeLayers(t)
	writeGame(t, l.shareDir, "valheim", 3)
	if err := os.WriteFile(filepath.Join(l.etcDir, "valheim.yaml"),
		[]byte("apiVersion: gvm/v1\nmetadata:\n  id: valheim\n   revision: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, diags, err := l.cat.Load("valheim", RootCLIAuthorizer{})
	if err == nil {
		t.Fatalf("Load succeeded with %v, want a parse error", m)
	}
	if m != nil {
		t.Errorf("a manifest was returned anyway: %s revision %d", m.Origin, m.Metadata.Revision)
	}
	if !diags.HasErrors() {
		t.Errorf("no error diagnostic: %s", diags)
	}

	// `gvm game which` still has to be able to explain the situation, so the
	// candidate list survives the failure.
	cands, _, err := l.cat.Resolve("valheim", RootCLIAuthorizer{})
	if err == nil {
		t.Fatal("Resolve succeeded")
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want both layers listed", len(cands))
	}
	if cands[0].Err == nil || cands[0].Origin != OriginOverride {
		t.Errorf("candidate[0] = %+v, want the broken override with its error", cands[0])
	}
	if cands[1].Manifest == nil || cands[1].Winner {
		t.Errorf("candidate[1] = %+v, want the packaged file present but not the winner", cands[1])
	}
}

func TestABrokenLayerNobodyConsultedDoesNotPoisonTheWinner(t *testing.T) {
	// A corrupt embedded floor under a good override must not make `gvm apply`
	// refuse: the file is never read at runtime. It is still reachable on the
	// candidate for `gvm doctor` to report.
	l := newThreeLayers(t)
	l.embedded["valheim.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: gvm/v1\n  id: [oops\n")}
	writeGame(t, l.etcDir, "valheim", 9)

	m, diags, err := l.cat.Load("valheim", RootCLIAuthorizer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Origin != OriginOverride {
		t.Errorf("origin = %s", m.Origin)
	}
	if diags.HasErrors() {
		t.Errorf("the broken floor produced errors for a load that never read it: %s", diags)
	}

	cands, _, err := l.cat.Resolve("valheim", RootCLIAuthorizer{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	floor := cands[len(cands)-1]
	if floor.Err == nil || !floor.Diags.HasErrors() {
		t.Errorf("the broken floor is not reportable: %+v", floor)
	}
}

func TestResolveListsTheLosersInRankOrder(t *testing.T) {
	l := newThreeLayers(t)
	l.embedded["projectzomboid.yaml"] = &fstest.MapFile{Data: []byte(gameYAML("projectzomboid", 7))}
	writeGame(t, l.shareDir, "projectzomboid", 7)
	writeGame(t, l.etcDir, "projectzomboid", 9)

	cands, _, err := l.cat.Resolve("projectzomboid", RootCLIAuthorizer{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantOrigins := []Origin{OriginOverride, OriginPackage, OriginEmbedded}
	var got []Origin
	for _, c := range cands {
		got = append(got, c.Origin)
	}
	if !reflect.DeepEqual(got, wantOrigins) {
		t.Fatalf("origins = %v, want %v", got, wantOrigins)
	}
	if !cands[0].Winner || cands[1].Winner || cands[2].Winner {
		t.Errorf("winner flags = %v/%v/%v, want only the override", cands[0].Winner, cands[1].Winner, cands[2].Winner)
	}
	if cands[1].SHA256 != cands[2].SHA256 {
		t.Error("identical packaged and embedded bytes produced different digests")
	}
	if cands[0].Revision != 9 {
		t.Errorf("candidate revision = %d, want 9 for `gvm game which`", cands[0].Revision)
	}
}

func TestNullListElementIsRefusedWithAPosition(t *testing.T) {
	l := newThreeLayers(t)
	writeGame(t, l.shareDir, "valheim", 3, "  ports:\n    - {name: game, default: 2456}\n    -\n    - {name: query, default: 2457}\n")

	m, diags, err := l.cat.Load("valheim", nil)
	if !errors.Is(err, ErrNullListElement) {
		t.Fatalf("err = %v, want ErrNullListElement", err)
	}
	if m != nil {
		t.Fatal("a manifest with a silently dropped port was returned")
	}
	if len(diags) != 1 {
		t.Fatalf("diags = %s, want one", diags)
	}
	d := diags[0]
	if d.Path != "spec.ports[1]" || d.Line == 0 || d.Sev != SevError {
		t.Errorf("diag = %+v, want an error at spec.ports[1] with a line", d)
	}
	if d.Fix == "" {
		t.Error("no fix offered for an empty list element")
	}
}

// hostileFS fails the test if it is ever opened.
type hostileFS struct{ t *testing.T }

func (h hostileFS) Open(name string) (fs.File, error) {
	h.t.Errorf("the filesystem was consulted with %q for an id that is not valid", name)
	return nil, fs.ErrNotExist
}

func TestInvalidGameIDNeverReachesTheFilesystem(t *testing.T) {
	cat := NewCatalog(Source{Origin: OriginPackage, Ref: "hostile", FS: hostileFS{t}})
	for _, id := range []string{
		"", "a", "Projectzomboid", "-pz", "pz_main", "pz.yaml",
		"../../etc/shadow", "..", "pz/../../etc/passwd",
		strings.Repeat("a", 33),
	} {
		t.Run(id, func(t *testing.T) {
			if _, _, err := cat.Load(id, RootCLIAuthorizer{}); !errors.Is(err, ErrBadGameID) {
				t.Errorf("Load(%q) err = %v, want ErrBadGameID", id, err)
			}
		})
	}
}

func TestMissingManifestIsDistinguishableFromABrokenOne(t *testing.T) {
	l := newThreeLayers(t)
	_, _, err := l.cat.Load("valheim", nil)
	if !errors.Is(err, ErrManifestNotFound) {
		t.Errorf("err = %v, want ErrManifestNotFound", err)
	}
}

// errFS is a layer that exists but cannot be read.
type errFS struct{ err error }

func (e errFS) Open(string) (fs.File, error) { return nil, e.err }

func TestUnreadableLayerDegradesToTheNextOne(t *testing.T) {
	root := t.TempDir()
	share := filepath.Join(root, "share")
	writeGame(t, share, "valheim", 3)

	cat := NewCatalog(
		Source{Origin: OriginOverride, Ref: "/etc/gvm/games", FS: errFS{errors.New("permission denied")}},
		DirSource(OriginPackage, share),
	)
	m, diags, err := cat.Load("valheim", RootCLIAuthorizer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Origin != OriginPackage {
		t.Errorf("origin = %s, want the packaged manifest", m.Origin)
	}
	if len(diags) != 1 || diags[0].Sev != SevWarning {
		t.Fatalf("diags = %s, want one warning about the unreadable layer", diags)
	}
}

func TestCatalogListIgnoresEverythingThatIsNotAManifest(t *testing.T) {
	l := newThreeLayers(t)
	l.embedded["valheim.yaml"] = &fstest.MapFile{Data: []byte(gameYAML("valheim", 1))}
	writeGame(t, l.shareDir, "projectzomboid", 7)
	writeGame(t, l.etcDir, "projectzomboid", 9) // same id from two layers, listed once

	for _, name := range []string{
		"README.md",
		"projectzomboid.yaml.swp",
		"Projectzomboid.yaml", // an id that could never be loaded
		".projectzomboid.yaml.tmp1234",
	} {
		if err := os.WriteFile(filepath.Join(l.shareDir, name), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := l.cat.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"projectzomboid", "valheim"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
}

func TestLoadInstanceRefusesADroppedAcquireEntry(t *testing.T) {
	// The same hazard as a dropped port, with a worse symptom: a mod silently
	// leaves the load order and the world loads without it.
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")
	body := `apiVersion: gvm/v1
kind: Instance
metadata:
  name: pz-main
spec:
  game: projectzomboid
  content:
    mods:
      source: steam_workshop
      acquire:
        - {id: "2169435993"}
        -
        - {id: "2200148440"}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in, diags, err := LoadInstance(path)
	if !errors.Is(err, ErrNullListElement) {
		t.Fatalf("err = %v, want ErrNullListElement", err)
	}
	if in != nil {
		t.Fatal("an instance with a dropped mod was returned")
	}
	if len(diags) != 1 || diags[0].Path != "spec.content.mods.acquire[1]" {
		t.Errorf("diags = %s, want one naming the acquire element", diags)
	}
}

func TestLoadInstanceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pz-main.yaml")
	body := `apiVersion: gvm/v1
kind: Instance
metadata:
  name: pz-main
  uid: 019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2
spec:
  game: projectzomboid
  managed_install: false
  ports:
    game: 16261
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in, diags, err := LoadInstance(path)
	if err != nil || diags.HasErrors() {
		t.Fatalf("LoadInstance: %v %s", err, diags)
	}
	if in.Metadata.Name != "pz-main" || in.Spec.Ports["game"] != 16261 {
		t.Errorf("instance = %+v", in)
	}
	if in.Spec.ManagedInstall {
		t.Error("managed_install: false decoded as true")
	}
}
