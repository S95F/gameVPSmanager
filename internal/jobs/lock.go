package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The job lock, and why its contents matter as much as its existence.
//
// A flock is the lock (D-30: not a Postgres advisory lock, because making the
// database the primary lock reintroduces the dependency the design exists to
// delete). But a lock that only says "held" is useless at 2am. The operator
// standing in front of a server that will not start needs to know whether to
// WAIT or to CLEAR, and those are opposite actions: a 40-minute workshop
// download is worth waiting for, and a pid from three boots ago is not. So the
// first line of the file names the holder -- pid:unit:kind:started_at -- and
// `doctor` prints it instead of printing "held".
//
// The format is lifecycle.Holder's, parsed and rendered there, because the
// steamcmd lock and the per-instance job lock are the same format and a second
// renderer is a second format.

// ErrLocked is what a contended acquisition wraps, so a caller can test for
// "somebody else has it" without unwrapping a *Busy.
var ErrLocked = errors.New("jobs: the job lock is held")

// ErrLock is a lock gvm could not use for a reason that is not contention: an
// unwritable directory, a path that is a directory, a platform with no flock.
var ErrLock = errors.New("jobs: the job lock cannot be taken")

// lockDirMode is 0750 root:root, section 5.3's mode for everything gvm's own
// processes use and nothing else reads.
const lockDirMode fs.FileMode = 0o750

// lockFileMode keeps the holder line readable by root only. The pid and unit
// in it are not secret, but the file lives beside the steamcmd lock and a
// uniform mode there is one fewer thing for doctor to have an opinion about.
const lockFileMode fs.FileMode = 0o640

// Flock is the advisory-lock primitive, as a seam.
//
// Linux has one; Windows, which is where this code is developed, does not have
// this one. The seam is here rather than a //go:build split of the whole file
// because the INTERESTING half of taking a lock is everything around the
// syscall -- creating the directory, writing the holder line, truncating a
// stale one, producing a diagnostic that names who is blocking -- and all of
// that is ordinary portable code that deserves to be tested on the machine it
// is written on.
//
// lock_linux.go supplies the real one. lock_stub.go supplies a refusal, not a
// no-op: a lock that silently succeeds is worse than no lock, because the
// second updater it admits is the one that corrupts an install tree.
type Flock interface {
	// TryLock takes an exclusive lock on an open file and does not block. It
	// returns ErrLocked when another holder has it.
	TryLock(f *os.File) error
	// Unlock releases it. Closing the descriptor releases it too; this exists
	// so Release can be explicit about the order of its three steps.
	Unlock(f *os.File) error
}

// SystemFlock is the platform's advisory lock.
var SystemFlock Flock = sysFlock{}

// Busy is a lock somebody else holds, and who.
//
// Known is separate from Holder because "held, by a process we can name" and
// "held, and the file does not say by whom" are different operator situations.
// The second one happens: a job that was SIGKILLed between taking the lock and
// writing its holder line leaves exactly that, and a diagnostic that invented
// a pid would be worse than one that admits it does not know.
type Busy struct {
	Path   string
	Holder lifecycle.Holder
	Known  bool
	// Now is the clock the age in the message is computed against.
	Now time.Time
}

func (b *Busy) Error() string {
	var sb strings.Builder
	sb.WriteString("jobs: ")
	sb.WriteString(b.Path)
	sb.WriteString(" is held")
	if !b.Known {
		sb.WriteString(" but does not name its holder (a job was killed between taking the " +
			"lock and writing the line); if nothing is running: gvm job clear")
		return sb.String()
	}
	sb.WriteString(" by pid ")
	sb.WriteString(fmt.Sprint(b.Holder.PID))
	if b.Holder.Unit != "" {
		sb.WriteString(" (")
		sb.WriteString(b.Holder.Unit)
		if b.Holder.Kind != "" {
			sb.WriteString(", ")
			sb.WriteString(b.Holder.Kind)
		}
		sb.WriteString(")")
	} else if b.Holder.Kind != "" {
		sb.WriteString(" (")
		sb.WriteString(b.Holder.Kind)
		sb.WriteString(")")
	}
	if !b.Holder.StartedAt.IsZero() {
		sb.WriteString(" since ")
		sb.WriteString(b.Holder.StartedAt.UTC().Format(time.RFC3339))
		if !b.Now.IsZero() {
			sb.WriteString(" (")
			sb.WriteString(b.Now.Sub(b.Holder.StartedAt).Round(time.Second).String())
			sb.WriteString(" ago)")
		}
	}
	return sb.String()
}

// Is makes errors.Is(err, ErrLocked) true for a *Busy.
func (b *Busy) Is(target error) bool { return target == ErrLocked }

// LockOptions are the seams Acquire needs. Every field has a working default.
type LockOptions struct {
	// Flock is the lock primitive. Nil is SystemFlock.
	Flock Flock
	// Now is the clock, for the holder line and the age in a Busy message.
	Now func() time.Time
}

func (o LockOptions) flock() Flock {
	if o.Flock != nil {
		return o.Flock
	}
	return SystemFlock
}

func (o LockOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Locker is how a job takes its lock, as a seam.
//
// It exists one level above Flock and for a different reason. Flock is there
// because the development machine has no flock(2); this is there because the
// production lock PATHS are /run/gvm/<name>/job.lock and
// /var/lib/gvm/locks/job__guard.lock, and a test that exercised the engine
// would create those directories on whatever machine it ran on. A fake Locker
// records the path it was handed -- so the derivation is still asserted -- and
// locks nothing.
type Locker interface {
	Acquire(path string, h lifecycle.Holder) (Unlocker, error)
}

// Unlocker is a held lock.
type Unlocker interface {
	// Release drops it, and is idempotent.
	Release() error
	// Holder is who it says holds it.
	Holder() lifecycle.Holder
	// Path is the lock file.
	Path() string
}

// FlockLocker is the production Locker: a real file, a real flock.
type FlockLocker struct{ LockOptions }

// Acquire takes the lock.
func (l FlockLocker) Acquire(path string, h lifecycle.Holder) (Unlocker, error) {
	// Written out rather than returned straight through: a nil *Lock returned
	// into an Unlocker interface is a non-nil interface holding a nil
	// pointer, which is the one shape a caller's `if lock != nil` gets wrong.
	lk, err := Acquire(path, h, l.LockOptions)
	if err != nil {
		return nil, err
	}
	return lk, nil
}

// Lock is a held job lock.
type Lock struct {
	path   string
	f      *os.File
	flock  Flock
	holder lifecycle.Holder
	closed bool
}

// Acquire takes the lock at path and writes h as its holder line.
//
// The order is: lock, then write. A holder line written before the lock was
// taken would overwrite the line of the process that actually holds it, which
// turns the one diagnostic that matters into a lie.
func Acquire(path string, h lifecycle.Holder, o LockOptions) (*Lock, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: no path", ErrLock)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, lockDirMode); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrLock, dir, err)
	}
	// O_RDWR and not O_WRONLY: the holder line has to be readable through the
	// same descriptor so that a successful Acquire can be asserted without
	// reopening the file, and O_EXCL is deliberately absent -- the lock file
	// is a rendezvous point that outlives every holder, not a marker.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrLock, path, err)
	}
	fl := o.flock()
	if err := fl.TryLock(f); err != nil {
		// Read the holder through a SEPARATE descriptor rather than through
		// the one just opened: flock is advisory and does not block reads, so
		// this works while the other process holds it, and keeping the two
		// uses apart means a failed read cannot disturb the descriptor being
		// closed on the next line.
		f.Close()
		if !errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s: %w", ErrLock, path, err)
		}
		busy := &Busy{Path: path, Now: o.now()}
		if got, rerr := ReadHolder(path); rerr == nil {
			busy.Holder, busy.Known = got, true
		}
		return nil, busy
	}

	l := &Lock{path: path, f: f, flock: fl, holder: h}
	if h.StartedAt.IsZero() {
		l.holder.StartedAt = o.now()
	}
	if err := l.writeHolder(); err != nil {
		// Unwind rather than carry on with a lock whose file does not say who
		// has it. The whole value of the file is the line.
		_ = l.Release()
		return nil, err
	}
	return l, nil
}

// writeHolder replaces the file's contents with the holder line.
//
// Truncate first, then write from offset zero: without the truncate, a shorter
// line written over a longer one leaves the tail of the previous holder behind,
// and ParseHolder reads a first line that is half of one pid and half of
// another.
func (l *Lock) writeHolder() error {
	body := l.holder.Line() + "\n" +
		"# gvm job lock. The line above is pid:unit:kind:started_at (D-30).\n" +
		"# It is advisory (flock); deleting this file does not release anything.\n"
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrLock, l.path, err)
	}
	if _, err := l.f.WriteAt([]byte(body), 0); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrLock, l.path, err)
	}
	// Synced because the holder line exists for the case where this process
	// dies: a line still in the page cache when the box loses power is a line
	// the operator does not get.
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrLock, l.path, err)
	}
	return nil
}

// Holder is who this lock says holds it.
func (l *Lock) Holder() lifecycle.Holder { return l.holder }

// Path is the lock file.
func (l *Lock) Path() string { return l.path }

// Release drops the lock. It is idempotent.
//
// The file is TRUNCATED and never unlinked, and that is not tidiness -- it is
// correctness. Unlinking a flocked file is the classic way to end up with two
// holders: B opens the path, A unlinks it and exits, B locks an inode with no
// name, C creates a fresh file at the path and locks that. Both believe they
// are alone. Truncating clears the stale holder line, which is the only thing
// about the file that could mislead anyone.
func (l *Lock) Release() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	var errs []error
	if err := l.f.Truncate(0); err != nil {
		errs = append(errs, err)
	}
	if err := l.flock.Unlock(l.f); err != nil {
		errs = append(errs, err)
	}
	if err := l.f.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: releasing %s: %w", ErrLock, l.path, errors.Join(errs...))
}

// ReadHolder reads a lock file's holder line without taking the lock.
//
// This is what `gvm doctor` and every "who is blocking me" message call. It
// does not lock, by design: asking takes no lock, so asking can never be the
// thing that blocks.
func ReadHolder(path string) (lifecycle.Holder, error) {
	f, err := os.Open(path)
	if err != nil {
		return lifecycle.Holder{}, err
	}
	defer f.Close()
	return lifecycle.ParseHolder(f)
}
