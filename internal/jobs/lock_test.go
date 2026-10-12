package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// These run on the development machine, which has no flock(2). That is the
// point of the Flock seam: the syscall is one line and the part worth testing
// is everything around it -- the directory, the holder line, the truncation of
// a stale one, and the diagnostic that tells an operator whether to WAIT or to
// CLEAR. All of that is portable, and all of it is exercised here.

func testHolder(at time.Time) lifecycle.Holder {
	return lifecycle.Holder{
		PID:       48812,
		Unit:      "gvm-job@pz-main_nightly.service",
		Kind:      "update",
		StartedAt: at,
	}
}

func tempLock(t *testing.T) string {
	t.Helper()
	// A nested directory, so MkdirAll's own behaviour is covered: the real
	// path is /run/gvm/<name>/job.lock and the per-instance directory may not
	// exist yet when a job runs before the first start.
	return filepath.ToSlash(filepath.Join(t.TempDir(), "gvm", "pz-main", "job.lock"))
}

// TestTheHolderLineNamesPidUnitKindAndSince is D-30's whole reason for
// existing.
//
// "A lock that says only 'held' is useless at 2am; one that names the pid and
// when it was taken tells the operator whether to wait or to clear it." A
// 40-minute workshop download is worth waiting for; a pid from three boots ago
// is not, and those are opposite actions.
func TestTheHolderLineNamesPidUnitKindAndSince(t *testing.T) {
	path := tempLock(t)
	at := time.Date(2026, 9, 4, 4, 0, 3, 0, time.UTC)

	lk, err := Acquire(path, testHolder(at), LockOptions{Flock: newFakeFlock()})
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	got, err := ReadHolder(path)
	if err != nil {
		t.Fatalf("the lock file does not name its holder: %v", err)
	}
	if got.PID != 48812 {
		t.Errorf("pid = %d", got.PID)
	}
	if got.Unit != "gvm-job@pz-main_nightly.service" {
		t.Errorf("unit = %q", got.Unit)
	}
	if got.Kind != "update" {
		t.Errorf("kind = %q", got.Kind)
	}
	if !got.StartedAt.Equal(at) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, at)
	}

	// The rest of the file explains itself, because an operator who finds it
	// with `cat` should not have to come here to learn what it is.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "pid:unit:kind:started_at") {
		t.Errorf("the file does not explain its own format:\n%s", body)
	}
	if !strings.Contains(body, "deleting this file does not release anything") {
		t.Errorf("the file does not warn that deleting it is not a release:\n%s", body)
	}
}

// TestAStartedAtIsSuppliedWhenTheCallerLeftItOut: a holder of unknown age is
// the one thing nobody should have to reason about.
func TestAStartedAtIsSuppliedWhenTheCallerLeftItOut(t *testing.T) {
	at := time.Date(2026, 9, 4, 4, 0, 3, 0, time.UTC)
	lk, err := Acquire(tempLock(t), lifecycle.Holder{PID: 7, Kind: "backup"}, LockOptions{
		Flock: newFakeFlock(),
		Now:   func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	if !lk.Holder().StartedAt.Equal(at) {
		t.Errorf("started_at = %v, want the clock's %v", lk.Holder().StartedAt, at)
	}
}

// TestContentionNamesWhoIsBlocking.
//
// errors.Is(err, ErrLocked) is what a caller tests; the message is what the
// operator reads, and it has to carry the pid, the unit, the kind and how long
// it has been held.
func TestContentionNamesWhoIsBlocking(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()
	at := time.Date(2026, 9, 4, 4, 0, 3, 0, time.UTC)

	first, err := Acquire(path, testHolder(at), LockOptions{Flock: fl})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	_, err = Acquire(path, lifecycle.Holder{PID: 2, Kind: "backup"}, LockOptions{
		Flock: fl,
		Now:   func() time.Time { return at.Add(2 * time.Minute) },
	})
	if err == nil {
		t.Fatal("two jobs took the same lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	var busy *Busy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %T, want *Busy", err)
	}
	if !busy.Known {
		t.Error("the holder was readable and Busy says it was not")
	}
	msg := err.Error()
	for _, want := range []string{
		"48812", "gvm-job@pz-main_nightly.service", "update",
		"2026-09-04T04:00:03Z", "2m0s ago",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the diagnostic does not contain %q:\n%s", want, msg)
		}
	}
}

// TestAnUnnamedHolderSaysSoRatherThanInventingOne.
//
// A job SIGKILLed between taking the lock and writing its line leaves exactly
// this. A diagnostic that invented a pid would be worse than one that admits
// it does not know -- and it tells the operator the right next move anyway.
func TestAnUnnamedHolderSaysSoRatherThanInventingOne(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := fl.TryLock(f); err != nil {
		t.Fatal(err)
	}

	_, err = Acquire(path, testHolder(time.Now()), LockOptions{Flock: fl})
	var busy *Busy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want *Busy", err)
	}
	if busy.Known {
		t.Error("Busy claims to know a holder an empty file cannot name")
	}
	if !strings.Contains(err.Error(), "does not name its holder") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "gvm job clear") {
		t.Errorf("err = %v; it has to say what to do about it", err)
	}
}

// TestReleaseTruncatesAndNeverUnlinks.
//
// Unlinking a flocked file is the classic way to end up with two holders: B
// opens the path, A unlinks it and exits, B locks an inode with no name, C
// creates a fresh file at the path and locks that. Both believe they are
// alone. Truncating clears the stale holder line, which is the only thing
// about the file that could mislead anyone.
func TestReleaseTruncatesAndNeverUnlinks(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()

	lk, err := Acquire(path, testHolder(time.Now()), LockOptions{Flock: fl})
	if err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Release unlinked the lock file, which is how two holders happen: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("the file still has %d bytes; a stale holder line outliving the lock is the "+
			"one misleading thing about it", st.Size())
	}
	if _, err := ReadHolder(path); err == nil {
		t.Error("a released lock still names a holder")
	}
	// Idempotent: a deferred Release plus one on the success path is a
	// natural way to write a call site.
	if err := lk.Release(); err != nil {
		t.Errorf("the second Release returned %v", err)
	}
	// And the lock is genuinely free again.
	again, err := Acquire(path, testHolder(time.Now()), LockOptions{Flock: fl})
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = again.Release()
}

// TestAShorterHolderLineLeavesNoTailOfThePreviousOne.
//
// Without the truncate in writeHolder, a short line written over a long one
// leaves the previous holder's tail behind, and ParseHolder reads a first line
// that is half of one pid and half of another.
func TestAShorterHolderLineLeavesNoTailOfThePreviousOne(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()

	long, err := Acquire(path, lifecycle.Holder{
		PID: 123456, Unit: "gvm-job@pz-main_a-very-long-job-name.service",
		Kind: "content_sync", StartedAt: time.Now(),
	}, LockOptions{Flock: fl})
	if err != nil {
		t.Fatal(err)
	}
	if err := long.Release(); err != nil {
		t.Fatal(err)
	}

	short, err := Acquire(path, lifecycle.Holder{PID: 7, Kind: "prune", StartedAt: time.Now()},
		LockOptions{Flock: fl})
	if err != nil {
		t.Fatal(err)
	}
	defer short.Release()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "a-very-long-job-name") {
		t.Errorf("the previous holder's line survived:\n%s", b)
	}
	got, err := ReadHolder(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 7 || got.Kind != "prune" {
		t.Errorf("holder = %+v", got)
	}
}

// TestAFailedHolderWriteReleasesTheLock: the whole value of the file is the
// line, so a lock whose file cannot say who has it is not a lock worth
// holding.
func TestAFailedHolderWriteReleasesTheLock(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()

	// A directory where the file should be: the open succeeds on neither
	// platform, which is the point -- Acquire must not leave a lock behind
	// when it refuses.
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path, testHolder(time.Now()), LockOptions{Flock: fl}); err == nil {
		t.Fatal("Acquire opened a directory as a lock file")
	} else if !errors.Is(err, ErrLock) {
		t.Errorf("err = %v, want ErrLock", err)
	}
	fl.mu.Lock()
	n := len(fl.held)
	fl.mu.Unlock()
	if n != 0 {
		t.Errorf("%d lock(s) left held after a refusal", n)
	}
}

// TestANonContentionFlockFailureIsNotReportedAsContention.
//
// "Held by somebody else" and "the kernel refused" need different answers, and
// off Linux the second one is what the stub returns. An operator told their
// nightly is "held" when the real problem is that there is no flock(2) would
// go looking for a process that does not exist.
func TestANonContentionFlockFailureIsNotReportedAsContention(t *testing.T) {
	fl := newFakeFlock()
	fl.err = platform.Unsupported("flock(2)")

	_, err := Acquire(tempLock(t), testHolder(time.Now()), LockOptions{Flock: fl})
	if err == nil {
		t.Fatal("a refused flock produced a lock")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("err = %v, which reads as contention", err)
	}
	if !errors.Is(err, ErrLock) {
		t.Errorf("err = %v, want ErrLock", err)
	}
}

// TestThePlatformStubRefusesRatherThanPretending.
//
// A lock that silently succeeded off Linux would make the development machine
// the one platform where two SteamCMD runs can write one install tree, and the
// only recovery from that is a restore.
func TestThePlatformStubRefusesRatherThanPretending(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = SystemFlock.TryLock(f)
	if runningOnLinux() {
		if err != nil {
			t.Fatalf("flock(2) on Linux: %v", err)
		}
		if err := SystemFlock.Unlock(f); err != nil {
			t.Errorf("unlock: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("the stub pretended to take a lock")
	}
	var unsup *platform.UnsupportedError
	if !errors.As(err, &unsup) {
		t.Errorf("err = %T (%v), want platform.UnsupportedError", err, err)
	}
}

// TestAnEmptyPathIsRefusedBeforeAnythingIsCreated: a job layer that joined an
// empty instance name into a path would land on a lock two host jobs share,
// and this is the floor under that.
func TestAnEmptyPathIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	if _, err := Acquire("", testHolder(time.Now()), LockOptions{Flock: newFakeFlock()}); err == nil {
		t.Fatal("Acquire took a lock on no path")
	}
}

// TestFlockLockerIsTheProductionLocker, including the nil-interface shape that
// a straight `return Acquire(...)` would have produced.
func TestFlockLockerIsTheProductionLocker(t *testing.T) {
	path := tempLock(t)
	fl := newFakeFlock()
	l := FlockLocker{LockOptions{Flock: fl}}

	u, err := l.Acquire(path, testHolder(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path() != path {
		t.Errorf("Path() = %q, want %q", u.Path(), path)
	}
	if u.Holder().PID != 48812 {
		t.Errorf("Holder() = %+v", u.Holder())
	}

	second, err := l.Acquire(path, testHolder(time.Now()))
	if err == nil {
		t.Fatal("two holders")
	}
	if second != nil {
		t.Errorf("a refused Acquire returned a non-nil Unlocker (%T); a nil *Lock in an "+
			"interface is the one shape `if lock != nil` gets wrong", second)
	}
	if err := u.Release(); err != nil {
		t.Fatal(err)
	}
}

func runningOnLinux() bool { return runtime.GOOS == "linux" }
