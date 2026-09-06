package ports

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// memLock stands in for flock(2), which is Linux-only. It provides the one
// property the ledger depends on -- exclusion between concurrent holders --
// so the read-modify-write ordering is provable on the development machine,
// while the real lock is what enforces it across processes.
type memLock struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	takes int
}

func newMemLock() *memLock { return &memLock{locks: map[string]*sync.Mutex{}} }

func (m *memLock) fn(path string) (Unlocker, error) {
	m.mu.Lock()
	mu, ok := m.locks[path]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[path] = mu
	}
	m.takes++
	m.mu.Unlock()
	mu.Lock()
	return memUnlock{mu}, nil
}

func (m *memLock) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.takes
}

type memUnlock struct{ mu *sync.Mutex }

func (u memUnlock) Unlock() error { u.mu.Unlock(); return nil }

func newStore(t *testing.T) (*Store, *memLock, *time.Time) {
	t.Helper()
	lk := newMemLock()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := &now
	return &Store{
		Dir:  t.TempDir(),
		Lock: lk.fn,
		Now:  func() time.Time { return *clock },
	}, lk, clock
}

var pzReqs = []Requirement{
	{Name: "game", Protos: []Proto{UDP}},
	{Name: "direct", Protos: []Proto{UDP}},
	{Name: "rcon", Protos: []Proto{TCP}},
}

var pzAlloc = Allocation{"game": 16261, "direct": 16262, "rcon": 27015}

var vhReqs = []Requirement{{Name: "game", Protos: []Proto{UDP}}}

func TestAMissingLedgerIsAnEmptyOneAndNotAnError(t *testing.T) {
	// The first create on a fresh host has nothing to read.
	s, _, _ := newStore(t)
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Reservations) != 0 || l.Schema != LedgerSchema {
		t.Fatalf("ledger = %+v", l)
	}
}

func TestReserveRecordsEveryEndpointAndTakesTheLock(t *testing.T) {
	s, lk, _ := newStore(t)
	made, err := s.Reserve("pz-main", pzAlloc, pzReqs)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 3 {
		t.Fatalf("made %d reservations, want 3", len(made))
	}
	if lk.count() == 0 {
		t.Error("Reserve did not take the lock")
	}

	claims, err := s.Claims()
	if err != nil {
		t.Fatal(err)
	}
	occ := emptyHost()
	occ.AddAll(claims)
	for _, ep := range []Endpoint{{UDP, 16261}, {UDP, 16262}, {TCP, 27015}} {
		if occ.Free(ep, "someone-else") {
			t.Errorf("%s is free despite an in-flight reservation", ep)
		}
		if !occ.Free(ep, "pz-main") {
			t.Errorf("%s blocks its own reserver", ep)
		}
	}
}

func TestTheSecondConcurrentCreateIsToldWhoBeatIt(t *testing.T) {
	// This is the whole point of the file. Two runs that read the same
	// occupancy both propose 16261; the loser has to be told, by name.
	s, _, _ := newStore(t)
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	_, err := s.Reserve("pz-eu", pzAlloc, pzReqs)
	if !errors.Is(err, ErrReserved) {
		t.Fatalf("error = %v, want ErrReserved", err)
	}
	var re *ReservedError
	if !errors.As(err, &re) {
		t.Fatalf("error = %T, want *ReservedError", err)
	}
	if re.Holder.Instance != "pz-main" {
		t.Errorf("holder = %+v", re.Holder)
	}
	// Endpoints are walked in sorted name order, so the endpoint named is the
	// same one on every run: a message that changed between attempts would be
	// unsearchable.
	if re.Endpoint != (Endpoint{UDP, 16262}) || re.Holder.Name != "direct" {
		t.Errorf("refused on %s (%s); the walk is not deterministic", re.Endpoint, re.Holder.Name)
	}
	if !strings.Contains(err.Error(), "udp/16262") || !strings.Contains(err.Error(), "pz-main") {
		t.Errorf("message = %q", err)
	}

	// And the loser left nothing behind.
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Reservations {
		if r.Instance != "pz-main" {
			t.Errorf("a refused reservation was written anyway: %+v", r)
		}
	}
}

func TestConcurrentReservesLeaveExactlyOneWinner(t *testing.T) {
	s, _, _ := newStore(t)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []string
	)
	for _, name := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, err := s.Reserve(name, pzAlloc, pzReqs); err == nil {
				mu.Lock()
				wins = append(wins, name)
				mu.Unlock()
			}
		}(name)
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("winners = %v, want exactly one", wins)
	}
}

func TestReservingAgainForTheSameInstanceReplacesRatherThanAccumulates(t *testing.T) {
	// A retried create must not fight with itself.
	s, _, _ := newStore(t)
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	moved := Allocation{"game": 16271, "direct": 16272, "rcon": 27016}
	if _, err := s.Reserve("pz-main", moved, pzReqs); err != nil {
		t.Fatalf("a retry must not collide with its own previous attempt: %v", err)
	}
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Reservations) != 3 {
		t.Fatalf("%d reservations, want 3", len(l.Reservations))
	}
	for _, r := range l.Reservations {
		if r.Port == 16261 {
			t.Errorf("the abandoned attempt is still held: %+v", r)
		}
	}
}

func TestReleaseIsWhatTheInstanceYAMLBecomingAuthoritativeLooksLike(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("vh-1", Allocation{"game": 2456}, vhReqs); err != nil {
		t.Fatal(err)
	}
	n, err := s.Release("pz-main")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("released %d, want 3", n)
	}
	l, _ := s.Load()
	if len(l.Reservations) != 1 || l.Reservations[0].Instance != "vh-1" {
		t.Fatalf("remaining = %+v", l.Reservations)
	}
	// Releasing an instance that holds nothing writes nothing.
	if n, err := s.Release("nobody"); err != nil || n != 0 {
		t.Errorf("Release(nobody) = %d, %v", n, err)
	}
}

func TestAnInterruptedCreateStopsHoldingItsPortsWhenTheTTLPasses(t *testing.T) {
	// A reservation with no expiry is a permanent leak: the create is
	// interrupted, the YAML is never written, and 16261 is unavailable until
	// somebody finds the file and deletes it by hand.
	s, _, clock := newStore(t)
	s.TTL = 10 * time.Minute
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	claims, _ := s.Claims()
	if len(claims) != 3 {
		t.Fatalf("claims = %d, want 3 while the reservation is live", len(claims))
	}

	*clock = clock.Add(11 * time.Minute)
	claims, err := s.Claims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %v, want none once the TTL has passed", claims)
	}

	// And the next reserver takes the ports rather than being refused.
	if _, err := s.Reserve("pz-eu", pzAlloc, pzReqs); err != nil {
		t.Fatalf("an expired reservation must not block: %v", err)
	}
}

func TestReserveDropsExpiredEntriesAsItGoes(t *testing.T) {
	// The ledger is rewritten under the lock on every reserve, so the write
	// path is the cheapest place to shed dead entries. Sweep exists for the
	// hosts where nothing is being created at all.
	s, _, clock := newStore(t)
	s.TTL = 10 * time.Minute
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(20 * time.Minute)
	if _, err := s.Reserve("vh-1", Allocation{"game": 2456}, vhReqs); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Load()
	if len(l.Reservations) != 1 || l.Reservations[0].Instance != "vh-1" {
		t.Fatalf("remaining = %+v, want only vh-1", l.Reservations)
	}
}

func TestSweepDropsExpiredReservations(t *testing.T) {
	// An interrupted create on a host where nothing else is being created has
	// nothing to prune it, so this is the only thing standing between it and a
	// permanently unavailable port.
	s, _, clock := newStore(t)
	s.TTL = 10 * time.Minute
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Sweep(nil); err != nil || n != 0 {
		t.Fatalf("Sweep while live = %d, %v", n, err)
	}
	*clock = clock.Add(20 * time.Minute)
	n, err := s.Sweep(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("swept %d, want the three expired reservations", n)
	}
	l, _ := s.Load()
	if len(l.Reservations) != 0 {
		t.Errorf("remaining = %+v", l.Reservations)
	}
}

func TestSweepDropsReservationsWhoseInstanceIsNowAuthoritative(t *testing.T) {
	// "Rebuildable from /etc at any time": nothing here is authoritative, so
	// discarding a reservation whose instance now has a YAML is always safe.
	s, _, _ := newStore(t)
	if _, err := s.Reserve("vh-1", Allocation{"game": 2456}, vhReqs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("vh-2", Allocation{"game": 2466}, vhReqs); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(func(name string) bool { return name == "vh-1" })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	l, _ := s.Load()
	if len(l.Reservations) != 1 || l.Reservations[0].Instance != "vh-2" {
		t.Fatalf("remaining = %+v", l.Reservations)
	}
	// A nil predicate is what a caller that cannot read /etc passes, and it
	// must never drop a live reservation.
	if n, err := s.Sweep(nil); err != nil || n != 0 {
		t.Errorf("Sweep(nil) dropped %d live reservations (%v)", n, err)
	}
}

func TestACorruptLedgerIsRefusedAndNotReset(t *testing.T) {
	// Starting from empty because the file did not parse would hand out every
	// port some other process is holding, which is the failure the ledger
	// exists to prevent.
	s, _, _ := newStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrLedgerCorrupt) {
		t.Fatalf("error = %v, want ErrLedgerCorrupt", err)
	}
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); !errors.Is(err, ErrLedgerCorrupt) {
		t.Fatalf("Reserve on a corrupt ledger = %v, want a refusal", err)
	}
	// And the corrupt file is still there for somebody to look at.
	b, err := os.ReadFile(s.Path())
	if err != nil || string(b) != "{not json" {
		t.Errorf("the corrupt ledger was overwritten: %q %v", b, err)
	}
}

func TestAnUnknownSchemaIsRefused(t *testing.T) {
	s, _, _ := newStore(t)
	if err := os.WriteFile(s.Path(), []byte(`{"schema":99,"reservations":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := errFrom(s.Load())
	if !errors.Is(err, ErrLedgerSchema) {
		t.Fatalf("error = %v, want ErrLedgerSchema", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("the error does not name the schema it found: %v", err)
	}
}

func errFrom(_ Ledger, err error) error { return err }

func TestTheLockFileIsNotTheLedgerFile(t *testing.T) {
	// The ledger is replaced by atomic rename, so a lock held on its own inode
	// would, one write later, be a lock on an unlinked inode nobody else can
	// contend for -- an advisory lock that stops working silently.
	s, _, _ := newStore(t)
	if s.LockPath() == s.Path() {
		t.Fatal("the lock must not be taken on the file that gets renamed away")
	}
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	// Two writes: if the lock were the ledger, the second would be on a
	// different inode from the first.
	if _, err := s.Reserve("vh-1", Allocation{"game": 2456}, vhReqs); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(s.Path()) != LedgerFile {
		t.Errorf("ledger file = %s", s.Path())
	}
}

func TestTheLedgerIsReadableJSONWithNoNulls(t *testing.T) {
	// An operator reads this file during an incident. A JSON null where an
	// empty array belongs reads as "the writer crashed".
	s, _, _ := newStore(t)
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release("pz-main"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("the ledger contains a null:\n%s", b)
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	if l.Schema != LedgerSchema || l.Updated.IsZero() {
		t.Errorf("ledger = %+v", l)
	}
}

func TestALockFailureStopsTheWriteEntirely(t *testing.T) {
	// A ledger written without the lock is a ledger two creates can interleave
	// in, which is the same as having no ledger.
	s, _, _ := newStore(t)
	want := errors.New("lock is held")
	s.Lock = func(string) (Unlocker, error) { return nil, want }
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); !errors.Is(err, want) {
		t.Fatalf("error = %v, want the lock failure", err)
	}
	if _, err := os.Stat(s.Path()); err == nil {
		t.Error("the ledger was written without the lock")
	}
	if _, err := s.Release("pz-main"); !errors.Is(err, want) {
		t.Errorf("Release ignored the lock failure: %v", err)
	}
}

func TestAReservationWithNoKnownProtocolHoldsBoth(t *testing.T) {
	// Same reasoning as Occupancy.AddInstance: with the protocol unknown,
	// reserving both is the only answer that cannot under-hold.
	s, _, _ := newStore(t)
	made, err := s.Reserve("x", Allocation{"mystery": 30000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 {
		t.Fatalf("made %d reservations, want tcp and udp", len(made))
	}
}

func TestReserveNeedsAnInstanceName(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Reserve("", pzAlloc, pzReqs); err == nil {
		t.Fatal("want a refusal")
	}
}

func TestReservationClaimNamesTheHolderAndTheDeadline(t *testing.T) {
	r := Reservation{
		Instance: "pz-main", Name: "game", Proto: UDP, Port: 16261, PID: 991,
		Expires: time.Date(2026, 3, 1, 12, 15, 0, 0, time.UTC),
	}
	c := r.Claim()
	if c.Source != SourceLedger || c.Owner != "pz-main" || c.Endpoint != (Endpoint{UDP, 16261}) {
		t.Fatalf("claim = %+v", c)
	}
	for _, want := range []string{"in-flight", "pz-main:game", "pid 991", "2026-03-01T12:15:00Z"} {
		if !strings.Contains(c.Desc, want) {
			t.Errorf("desc %q does not contain %q", c.Desc, want)
		}
	}
}

func TestLedgerStringIsSortedAndReadable(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Reserve("pz-main", pzAlloc, pzReqs); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Load()
	lines := strings.Split(l.String(), "\n")
	if len(lines) != 3 {
		t.Fatalf("String() = %q", l.String())
	}
	if !strings.HasPrefix(lines[0], "udp/16261") || !strings.HasPrefix(lines[2], "tcp/27015") {
		t.Errorf("lines are not sorted by port:\n%s", l.String())
	}
	if (Ledger{}).String() != "no reservations" {
		t.Errorf("an empty ledger reads as %q", (Ledger{}).String())
	}
}

func TestFlockLockerReturnsANilInterfaceOnFailure(t *testing.T) {
	// A typed nil pointer inside a non-nil interface is how an error path still
	// looks like it acquired something, and the deferred Unlock then panics.
	if runtime.GOOS == "linux" {
		t.Skip("flock works here; the failure path is what this pins")
	}
	u, err := FlockLocker(filepath.Join(t.TempDir(), "x.lock"))
	if err == nil {
		t.Fatal("want an error off Linux")
	}
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("error = %v", err)
	}
	if u != nil {
		t.Fatalf("Unlocker = %#v, want a nil interface", u)
	}
}

func TestTheDefaultLockIsFlock(t *testing.T) {
	// A Store with no Lock must not silently write unguarded.
	s := &Store{Dir: t.TempDir()}
	if s.TTL != 0 {
		t.Fatal("fixture")
	}
	_, err := s.Reserve("pz-main", pzAlloc, pzReqs)
	if runtime.GOOS == "linux" {
		if err != nil {
			t.Fatalf("flock should work here: %v", err)
		}
		return
	}
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Fatalf("error = %v, want the flock stub's refusal rather than an unguarded write", err)
	}
	if _, err := os.Stat(s.Path()); err == nil {
		t.Error("the ledger was written without a lock")
	}
}
