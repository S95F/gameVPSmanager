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

// provenPath is a stand-in for instance.Jailed.
//
// It is a fake on purpose and it does NOT weaken the guarantee: the proving
// happens in internal/instance, whose Jailed has unexported fields and can
// only be produced by Jail.Resolve, and instance's own compile-fail fixtures
// assert that no other package can forge one. What this package owns is the
// second half of the contract -- that Destroy refuses to run without a
// verified receipt and without a valid target -- and testing that needs a
// target this package can construct.
type provenPath struct {
	abs  string
	root string
}

func (p provenPath) Path() string { return p.abs }
func (p provenPath) Root() string { return p.root }
func (p provenPath) Rel() string {
	rel, err := filepath.Rel(p.root, p.abs)
	if err != nil {
		return p.abs
	}
	return rel
}
func (p provenPath) Valid() bool { return p.abs != "" && p.root != "" }

func verifiedRef(path string) *Ref {
	now := time.Now()
	return &Ref{
		Path:       path,
		SHA256:     strings.Repeat("a", 64),
		Instance:   "pz-main",
		Class:      ClassPreserve,
		Reason:     "pre-rebuild",
		CreatedAt:  now,
		VerifiedAt: now,
	}
}

func mustPanicWith(t *testing.T, want string, fn func()) {
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

// TestDestroyRequiresArchiveRef is section 13.1's panic, and the reason it is
// a panic: a caller that reached this point without a verified archive has a
// bug in its ordering, and a returned error is something the next author can
// route around with `if err != nil { log; continue }`. There is no correct
// "continue anyway" here, so the function does not offer one.
func TestDestroyRequiresArchiveRef(t *testing.T) {
	root := t.TempDir()
	target := provenPath{abs: filepath.Join(root, "install"), root: root}
	if err := os.MkdirAll(target.abs, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("nil ref", func(t *testing.T) {
		mustPanicWith(t, "nil archive ref", func() {
			_, _ = Destroy(context.Background(), nil, []Target{target})
		})
	})

	t.Run("written but never verified", func(t *testing.T) {
		ref := verifiedRef("/var/backups/gvm/pz-main/pre-rebuild.tar.gz")
		ref.VerifiedAt = time.Time{}
		mustPanicWith(t, "unverified archive ref", func() {
			_, _ = Destroy(context.Background(), ref, []Target{target})
		})
	})

	t.Run("verified but pathless", func(t *testing.T) {
		ref := verifiedRef("")
		mustPanicWith(t, "unverified archive ref", func() {
			_, _ = Destroy(context.Background(), ref, []Target{target})
		})
	})

	// The path must still be there: a panic that happened after the delete
	// would prove nothing.
	if _, err := os.Stat(target.abs); err != nil {
		t.Fatalf("Destroy removed %s before refusing: %v", target.abs, err)
	}
}

// TestDestroyRejectsAnUnprovenTarget covers the zero value, which is the one
// forgery an interface cannot prevent at compile time.
func TestDestroyRejectsAnUnprovenTarget(t *testing.T) {
	root := t.TempDir()
	good := provenPath{abs: filepath.Join(root, "install"), root: root}
	if err := os.MkdirAll(good.abs, 0o755); err != nil {
		t.Fatal(err)
	}
	mustPanicWith(t, "unproven target at index 1", func() {
		_, _ = Destroy(context.Background(), verifiedRef("/var/backups/gvm/pz-main/x.tar.gz"),
			[]Target{good, provenPath{}})
	})
	if _, err := os.Stat(good.abs); err != nil {
		t.Fatalf("Destroy removed a valid target before refusing the invalid one: %v", err)
	}
}

func TestDestroyRefusesAnEmptySet(t *testing.T) {
	_, err := Destroy(context.Background(), verifiedRef("/var/backups/gvm/pz-main/x.tar.gz"), nil)
	if err == nil {
		t.Fatal("Destroy over an empty set reported success; a destroy that removes nothing " +
			"must not be indistinguishable from one that worked")
	}
}

func TestDestroyRemovesAndMeasures(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]file{
		"install":            {dir: true, perm: 0o755},
		"install/server.jar": {body: "0123456789", perm: 0o644},
		"install/lib":        {dir: true, perm: 0o755},
		"install/lib/a.so":   {body: "abc", perm: 0o644},
		"world":              {dir: true, perm: 0o755},
		"world/keep.bin":     {body: "survivor", perm: 0o644},
	})
	target := provenPath{abs: filepath.Join(root, "install"), root: root}

	res, err := Destroy(context.Background(), verifiedRef("/var/backups/gvm/pz-main/x.tar.gz"), []Target{target})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if res.Files != 2 || res.Bytes != 13 {
		t.Errorf("result = %d files / %d bytes, want 2 / 13; the measurement has to happen "+
			"BEFORE the delete or there is nothing left to measure", res.Files, res.Bytes)
	}
	if res.Archive != "/var/backups/gvm/pz-main/x.tar.gz" {
		t.Errorf("result does not record the archive that authorised it: %q", res.Archive)
	}
	if _, err := os.Stat(target.abs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("install survived the destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "world", "keep.bin")); err != nil {
		t.Errorf("a path outside the destroy set was removed: %v", err)
	}
}

func TestDestroyHonoursACancelledContext(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]file{"install": {dir: true, perm: 0o755}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Destroy(ctx, verifiedRef("/x.tar.gz"), []Target{provenPath{abs: filepath.Join(root, "install"), root: root}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(root, "install")); err != nil {
		t.Fatalf("a cancelled Destroy still deleted: %v", err)
	}
}
