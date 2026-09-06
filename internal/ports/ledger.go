package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The ledger's on-disk identity.
const (
	// LedgerSchema is the schema this build writes. A reader refuses an
	// unknown one rather than guessing: a ledger it half-understands is a set
	// of reservations it may silently drop, and dropping a reservation is how
	// two concurrent creates end up on the same port.
	LedgerSchema = 1

	// LedgerFile is the ledger's name under /var/lib/gvm.
	LedgerFile = "ports.lock.json"

	// LockFile is the flock file guarding it.
	//
	// It is deliberately NOT the ledger itself. The ledger is replaced by
	// atomic rename, so a lock held on the ledger's own inode would, one write
	// later, be a lock on an unlinked inode that nobody else can ever contend
	// for -- an advisory lock that stops working silently, months after
	// somebody wrote it, which is the exact bug class this file exists to
	// prevent.
	LockFile = ".ports.lock"

	// DefaultTTL bounds how long an unfinished create holds its ports.
	//
	// A reservation with no expiry is a permanent leak: `gvm create` is
	// interrupted, the YAML is never written, and 16261 is unavailable until
	// somebody finds the file and deletes it by hand. Fifteen minutes is
	// comfortably longer than the gap between allocating and writing an
	// instance YAML and comfortably shorter than an operator's patience.
	DefaultTTL = 15 * time.Minute
)

// ErrLedgerSchema and friends. Each is a sentinel because the caller reacts
// differently: a corrupt ledger is a repairable operator problem, a reserved
// endpoint is a retry, and a schema mismatch means the wrong gvm.
var (
	ErrLedgerSchema  = errors.New("ports: ledger schema is not one this build understands")
	ErrLedgerCorrupt = errors.New("ports: ledger is not readable JSON")
	ErrReserved      = errors.New("ports: endpoint is already reserved")
)

// Reservation is one in-flight claim, held between `gvm create` choosing a
// window and the instance YAML that makes it authoritative being written.
type Reservation struct {
	Instance string    `json:"instance"`
	Name     string    `json:"name"`
	Proto    Proto     `json:"proto"`
	Port     int       `json:"port"`
	PID      int       `json:"pid"`
	Acquired time.Time `json:"acquired"`
	Expires  time.Time `json:"expires"`
}

// Endpoint is the endpoint the reservation holds.
func (r Reservation) Endpoint() Endpoint { return Endpoint{Proto: r.Proto, Port: r.Port} }

// Live reports whether the reservation is still in force at now.
func (r Reservation) Live(now time.Time) bool {
	return r.Expires.IsZero() || now.Before(r.Expires)
}

// Claim renders the reservation as an occupancy claim.
func (r Reservation) Claim() Claim {
	return Claim{
		Endpoint: r.Endpoint(),
		Owner:    r.Instance,
		Desc: fmt.Sprintf("an in-flight reservation for %s:%s (pid %d, expires %s)",
			r.Instance, r.Name, r.PID, r.Expires.UTC().Format(time.RFC3339)),
		Source: SourceLedger,
	}
}

// Ledger is the file's contents.
type Ledger struct {
	Schema       int           `json:"schema"`
	Updated      time.Time     `json:"updated"`
	Reservations []Reservation `json:"reservations"`
}

// Live returns the reservations still in force at now, sorted.
func (l Ledger) Live(now time.Time) []Reservation {
	var out []Reservation
	for _, r := range l.Reservations {
		if r.Live(now) {
			out = append(out, r)
		}
	}
	sortReservations(out)
	return out
}

// Claims renders the live reservations for an Occupancy.
func (l Ledger) Claims(now time.Time) []Claim {
	live := l.Live(now)
	out := make([]Claim, 0, len(live))
	for _, r := range live {
		out = append(out, r.Claim())
	}
	return out
}

func sortReservations(rs []Reservation) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Port != rs[j].Port {
			return rs[i].Port < rs[j].Port
		}
		if rs[i].Proto != rs[j].Proto {
			return rs[i].Proto < rs[j].Proto
		}
		if rs[i].Instance != rs[j].Instance {
			return rs[i].Instance < rs[j].Instance
		}
		return rs[i].Name < rs[j].Name
	})
}

// Unlocker releases a lock. It is an interface, not *platform.FileLock, so the
// store's read-modify-write can be exercised on a machine without flock(2).
type Unlocker interface {
	Unlock() error
}

// LockFunc takes an exclusive lock on path.
type LockFunc func(path string) (Unlocker, error)

// FlockLocker is the production lock: platform.Flock, which is flock(2) and is
// released by the kernel if the holder dies for any reason.
func FlockLocker(path string) (Unlocker, error) {
	l, err := platform.Flock(path)
	if err != nil {
		// Returned as a bare nil so the interface is nil too. A typed nil
		// pointer in a non-nil interface is the classic way an error path
		// still looks like it acquired something.
		return nil, err
	}
	return l, nil
}

// ReservedError names who holds the endpoint a reservation was refused for.
type ReservedError struct {
	Endpoint Endpoint
	Holder   Reservation
}

func (e *ReservedError) Error() string {
	return fmt.Sprintf("ports: %s is already reserved by %s:%s until %s",
		e.Endpoint, e.Holder.Instance, e.Holder.Name, e.Holder.Expires.UTC().Format(time.RFC3339))
}

func (e *ReservedError) Unwrap() error { return ErrReserved }

// Store is the ledger file plus the lock that serialises writes to it.
//
// Nothing in the ledger is authoritative. /etc/gvm/instances is; the ledger
// only covers the window between an allocation being chosen and the YAML that
// records it being written, which is exactly the window in which two concurrent
// `gvm create` runs would otherwise both pick 16261. It is rebuildable from
// /etc at any time, which is what Sweep does.
type Store struct {
	// Dir holds both the ledger and its lock file.
	Dir string

	// Lock defaults to FlockLocker.
	Lock LockFunc

	// Now defaults to time.Now. Injected so expiry is testable without
	// sleeping through a TTL.
	Now func() time.Time

	// TTL defaults to DefaultTTL.
	TTL time.Duration
}

// Path is the ledger file.
func (s *Store) Path() string { return filepath.Join(s.Dir, LedgerFile) }

// LockPath is the flock file.
func (s *Store) LockPath() string { return filepath.Join(s.Dir, LockFile) }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTTL
}

func (s *Store) lock() (Unlocker, error) {
	fn := s.Lock
	if fn == nil {
		fn = FlockLocker
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("ports: ledger directory %s: %w", s.Dir, err)
	}
	return fn(s.LockPath())
}

// Load reads the ledger without taking the lock.
//
// Reading unlocked is safe because the file is only ever replaced by rename, so
// a reader sees one whole version or another and never a half-written one. It
// is also necessary: building an Occupancy must not block behind a `gvm create`
// that is mid-write, or `gvm doctor` would hang on a busy host.
func (s *Store) Load() (Ledger, error) {
	b, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		// No file is an empty ledger, not an error: the first create on a
		// fresh host has nothing to read.
		return Ledger{Schema: LedgerSchema}, nil
	}
	if err != nil {
		return Ledger{}, fmt.Errorf("ports: read ledger %s: %w", s.Path(), err)
	}
	return decodeLedger(b, s.Path())
}

func decodeLedger(b []byte, path string) (Ledger, error) {
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		// Refused rather than reset. Starting from empty because the file did
		// not parse would hand out every port some other process is holding,
		// which is the failure the ledger exists to prevent -- so a corrupt
		// ledger stops creates until somebody looks at it.
		return Ledger{}, fmt.Errorf("%w: %s: %v", ErrLedgerCorrupt, path, err)
	}
	if l.Schema != LedgerSchema {
		return Ledger{}, fmt.Errorf("%w: %s has schema %d, this build writes %d",
			ErrLedgerSchema, path, l.Schema, LedgerSchema)
	}
	return l, nil
}

// Claims is Load reduced to live occupancy claims.
func (s *Store) Claims() ([]Claim, error) {
	l, err := s.Load()
	if err != nil {
		return nil, err
	}
	return l.Claims(s.now()), nil
}

// Reserve records an allocation for instance, refusing if any endpoint has been
// taken since the caller built its occupancy.
//
// The re-read inside the lock is the point of the whole file. Two `gvm create`
// runs that both read the same occupancy will both propose 16261; the second
// one to reach this function sees the first one's reservation and is told, by
// name, who beat it.
//
// Reserving again for the same instance replaces that instance's previous
// reservations rather than accumulating them, so a retried create does not
// fight with itself.
func (s *Store) Reserve(instance string, alloc Allocation, reqs []Requirement) ([]Reservation, error) {
	// The name is not validated here beyond being present: instance.ValidateName
	// owns that rule, and importing internal/instance for it would drag the
	// destroy jail and internal/archive into the allocator's dependency graph
	// for one string check.
	if instance == "" {
		return nil, fmt.Errorf("ports: a reservation needs an instance name")
	}

	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock.Unlock()

	l, err := s.Load()
	if err != nil {
		return nil, err
	}
	now := s.now()

	byName := make(map[string][]Proto, len(reqs))
	for _, r := range reqs {
		byName[r.Name] = r.Protos
	}

	held := map[Endpoint]Reservation{}
	kept := make([]Reservation, 0, len(l.Reservations))
	for _, r := range l.Reservations {
		if !r.Live(now) || r.Instance == instance {
			continue
		}
		kept = append(kept, r)
		held[r.Endpoint()] = r
	}

	var made []Reservation
	for _, name := range alloc.Names() {
		protos := byName[name]
		if len(protos) == 0 {
			// Same reasoning as Occupancy.AddInstance: with the protocol
			// unknown, reserving both is the only answer that cannot under-hold.
			protos = []Proto{TCP, UDP}
		}
		for _, pr := range protos {
			ep := Endpoint{Proto: pr, Port: alloc[name]}
			if h, taken := held[ep]; taken {
				return nil, &ReservedError{Endpoint: ep, Holder: h}
			}
			made = append(made, Reservation{
				Instance: instance,
				Name:     name,
				Proto:    pr,
				Port:     alloc[name],
				PID:      os.Getpid(),
				Acquired: now,
				Expires:  now.Add(s.ttl()),
			})
		}
	}

	l.Reservations = append(kept, made...)
	if err := s.write(l, now); err != nil {
		return nil, err
	}
	return made, nil
}

// Release drops every reservation belonging to instance. It is what `gvm
// create` calls once the instance YAML is on disk and authoritative, and what a
// failed create calls on the way out.
func (s *Store) Release(instance string) (int, error) {
	return s.filter(func(r Reservation) bool { return r.Instance != instance })
}

// Sweep drops expired reservations and any whose instance has since become
// authoritative.
//
// settled answers "does /etc/gvm/instances have a YAML for this name". That
// predicate is what "rebuildable from /etc at any time" means in practice:
// nothing in this file is authoritative, so discarding a reservation whose
// instance now has a YAML is always safe, and discarding an expired one is the
// only thing standing between an interrupted create and a permanently
// unavailable port.
func (s *Store) Sweep(settled func(instance string) bool) (int, error) {
	now := s.now()
	return s.filter(func(r Reservation) bool {
		if !r.Live(now) {
			return false
		}
		return settled == nil || !settled(r.Instance)
	})
}

// filter rewrites the ledger under the lock, keeping the reservations keep
// accepts, and returns how many were dropped.
func (s *Store) filter(keep func(Reservation) bool) (int, error) {
	unlock, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer unlock.Unlock()

	l, err := s.Load()
	if err != nil {
		return 0, err
	}
	out := make([]Reservation, 0, len(l.Reservations))
	for _, r := range l.Reservations {
		if keep(r) {
			out = append(out, r)
		}
	}
	dropped := len(l.Reservations) - len(out)
	if dropped == 0 {
		return 0, nil
	}
	l.Reservations = out
	if err := s.write(l, s.now()); err != nil {
		return 0, err
	}
	return dropped, nil
}

// write replaces the ledger atomically. The caller holds the lock.
func (s *Store) write(l Ledger, now time.Time) error {
	l.Schema = LedgerSchema
	l.Updated = now
	sortReservations(l.Reservations)
	if l.Reservations == nil {
		// An explicit empty array rather than JSON null, so an operator
		// reading the file sees "nothing reserved" instead of wondering
		// whether the writer crashed.
		l.Reservations = []Reservation{}
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("ports: encode ledger: %w", err)
	}
	b = append(b, '\n')
	// 0644: the ledger names instances and port numbers, all of which are in
	// the world-readable instance YAMLs anyway, and `gvm doctor` run as an
	// operator has to be able to read it.
	if err := spec.WriteFileAtomicNoBackup(s.Path(), b, 0o644); err != nil {
		return fmt.Errorf("ports: write ledger: %w", err)
	}
	return nil
}

// String renders a ledger for `gvm ports --reservations`.
func (l Ledger) String() string {
	if len(l.Reservations) == 0 {
		return "no reservations"
	}
	var b strings.Builder
	rs := append([]Reservation{}, l.Reservations...)
	sortReservations(rs)
	for i, r := range rs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s %s:%s pid %d expires %s",
			r.Endpoint(), r.Instance, r.Name, r.PID, r.Expires.UTC().Format(time.RFC3339))
	}
	return b.String()
}
