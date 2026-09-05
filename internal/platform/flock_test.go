package platform

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestFileLockPathIsSafeOnTheZeroValue(t *testing.T) {
	// Flock returns a nil lock on failure, and a caller that defers Unlock
	// before checking the error must not panic on the way out.
	var l *FileLock
	if got := l.Path(); got != "" {
		t.Fatalf("Path on a nil lock = %q", got)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("Unlock on a nil lock = %v", err)
	}
}

// The lock is the first of the three defences against two servers opening one
// world (section 11.7), so its behaviour is asserted against a real kernel
// rather than mocked.
func TestFlockIsExclusiveAndReleasable(t *testing.T) {
	if !isLinux() {
		t.Skip("flock is Linux-only; the stub is covered by TestHostOperationsAreUnsupportedOffLinux")
	}
	path := filepath.Join(t.TempDir(), "job.lock")

	first, err := Flock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Path() != path {
		t.Errorf("Path = %q, want %q", first.Path(), path)
	}

	// A second open file description on the same file is exactly what a
	// hand-run `gvm run pz-main` alongside the unit looks like.
	if _, err := Flock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire = %v, want ErrLocked", err)
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// Unlocking twice happens whenever a defer and an explicit release
	// coexist; it must not report a spurious failure.
	if err := first.Unlock(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}

	second, err := Flock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestFlockReportsIOFailuresSeparatelyFromContention(t *testing.T) {
	if !isLinux() {
		t.Skip("flock is Linux-only")
	}
	// A missing directory is a broken host, not a busy one: the caller exits
	// with a different code for each.
	_, err := Flock(filepath.Join(t.TempDir(), "no-such-dir", "job.lock"))
	if err == nil {
		t.Fatal("locking inside a missing directory succeeded")
	}
	if errors.Is(err, ErrLocked) {
		t.Fatalf("an I/O failure was reported as contention: %v", err)
	}
}
