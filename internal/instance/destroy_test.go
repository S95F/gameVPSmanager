package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
)

// newRef is a verified receipt: an archive that exists and whose contents were
// checked. Anything less is refused by Destroy, which is the point.
func newRef(path string) *archive.Ref {
	now := time.Now()
	return &archive.Ref{
		Path:       path,
		Instance:   "pz-main",
		Class:      archive.ClassPreserve,
		Reason:     "pre-rebuild",
		CreatedAt:  now,
		VerifiedAt: now,
	}
}

// jailedFixture returns a Jailed proven against a real temp tree, which is the
// only way to obtain one -- including from inside this package.
func jailedFixture(t *testing.T) []Jailed {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "install"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := NewJail(JailConfig{Allow: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := j.ResolveAll([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustPanic(t *testing.T, want string, fn func()) {
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

// Archive-before-destroy, encoded as a type. A caller with no receipt has
// nothing to pass, and passing nil is a programming error rather than an
// operating condition: it panics, where a returned error is something a future
// caller can route around with an `if err != nil { log }` (section 13.1).
func TestDestroyPanicsWithoutAnArchive(t *testing.T) {
	paths := jailedFixture(t)
	mustPanic(t, "nil archive ref", func() {
		_ = Destroy(context.Background(), nil, paths)
	})
}

func TestDestroyPanicsOnAnUnverifiedArchive(t *testing.T) {
	paths := jailedFixture(t)
	written := newRef("/var/backups/gvm/pz-main/pre-rebuild.tar.gz")
	written.VerifiedAt = time.Time{} // written, never checked
	mustPanic(t, "unverified archive ref", func() {
		_ = Destroy(context.Background(), written, paths)
	})
}

// The zero Jailed is the one value another package CAN construct --
// []Jailed{{}} compiles anywhere -- so it is the one forgery Destroy has to
// reject at run time rather than at compile time.
func TestDestroyPanicsOnAForgedZeroJailed(t *testing.T) {
	good := jailedFixture(t)
	mixed := append([]Jailed{}, good...)
	mixed = append(mixed, Jailed{})
	mustPanic(t, "zero Jailed at index 1", func() {
		_ = Destroy(context.Background(), newRef("/var/backups/gvm/pz-main/x.tar.gz"), mixed)
	})
}

func TestDestroyRefusesAnEmptyDestroySet(t *testing.T) {
	err := Destroy(context.Background(), newRef("/var/backups/gvm/pz-main/x.tar.gz"), nil)
	if !errors.Is(err, ErrJail) {
		t.Fatalf("Destroy with no paths = %v, want a refusal: a destroy that removes nothing "+
			"must not report success", err)
	}
}

func TestDestroyHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Destroy(ctx, newRef("/var/backups/gvm/pz-main/x.tar.gz"), jailedFixture(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy = %v, want context.Canceled", err)
	}
}

// Until Phase 5 the body is honest about being absent. A stub that returned nil
// would report every rebuild as having deleted a world it never touched.
func TestDestroyIsNotImplementedYet(t *testing.T) {
	err := Destroy(context.Background(), newRef("/var/backups/gvm/pz-main/x.tar.gz"), jailedFixture(t))
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Destroy = %v, want ErrNotImplemented; a stub that returns nil claims to have "+
			"destroyed something", err)
	}
}
