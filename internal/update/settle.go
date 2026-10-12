package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/jobs"
)

// Phase 8, and the end of `sleep 180` (D-33).
//
// update.sh's line is:
//
//	log "Waiting 180s for workshop downloads before re-checking mod IDs..."
//	sleep 180
//
// 180 seconds is a number chosen once, against one world, on one box. It is
// simultaneously too long on the night nothing changed -- three minutes of a
// nightly spent asleep for no reason -- and too short on the night somebody
// added a 2 GB map, where the job then resolves mod ids from a tree that is
// still being written and reports a mod missing that was merely late.
//
// What replaces it is two signals and a budget:
//
//   - the POSITIVE signal: every expected acquire id has a non-empty directory
//     under a declared root. This is exactly rebuild.sh's NOT_DOWNLOADED loop,
//     inverted, and it is the only thing that actually proves the download
//     finished. When it fires the wait ends immediately, however long the
//     budget was.
//   - QUIESCENCE: nothing under the declared roots has changed size or mtime
//     for `Quiet`, measured after at least one poll. This is the answer for a
//     Steam stall, and it is a reason to report rather than a reason to wait.
//     The ids that never arrived are named, and the job records `degraded`.
//   - the BUDGET: a hard ceiling (1800s for an update, 3600s for a rebuild)
//     so that a root whose mtime is being touched by something else forever
//     cannot hold the instance lock against tomorrow's nightly.
//
// Quiescence without the positive signal is never silently treated as success.
// That is the whole of D-33: the bash suite's advice for a stalled workshop
// item ("delete its folder and restart the server") was a comment in a script,
// and here it becomes a named id in a job summary that `gvm doctor` can act on.

// Settle defaults, in seconds, from section 13.5.
const (
	// DefaultQuiet is the `settle` figure: how long the roots must stop
	// changing before a stall is called a stall.
	DefaultQuiet = 45 * time.Second
	// DefaultPoll is how often the roots are measured. Short enough that the
	// positive signal is noticed promptly, long enough that walking a 60 GB
	// install tree is not the thing generating the disk activity.
	DefaultPoll = 5 * time.Second
	// BudgetUpdate and BudgetRebuild are section 13.5's two hard ceilings.
	// A rebuild's first boot downloads the entire workshop list from nothing,
	// which is the 3600s case Project Zomboid's readiness budget is also set
	// against.
	BudgetUpdate  = 1800 * time.Second
	BudgetRebuild = 3600 * time.Second
)

// ErrSettleBudget is the hard ceiling being reached with ids still missing.
var ErrSettleBudget = errors.New("update: content did not settle inside the budget")

// Expectation is one acquire id and the directories it may legitimately
// arrive in.
//
// Several directories because the roots are a list: Project Zomboid's workshop
// items land under the SteamCMD workshop root for the dedicated server's own
// appid, and an adopted install may have a second root under the account's
// HOME. An id is present when ANY of its candidates exists and is non-empty --
// never when merely one of the roots exists, which is the check that passes on
// an empty download.
type Expectation struct {
	// ID is the acquire id, and it is what gets reported by name. A count
	// would make the operator go looking; an id is something they can paste
	// into the Workshop.
	ID string
	// Dirs are absolute candidate directories.
	Dirs []string
}

// Entry is one file as a poll sees it. Size and ModTime and nothing else:
// hashing content every five seconds over a 60 GB tree would itself be the
// disk activity the quiescence check is looking for.
type Entry struct {
	Rel     string
	Size    int64
	ModTime time.Time
}

// Settler waits for phase 8.
//
// Every host-touching member is a seam with a default, so the whole decision
// table -- positive, quiescent, over budget, cancelled -- is an ordinary table
// test on the development machine. That is rule 5, and it matters here because
// the failure this code prevents only ever shows up on a box with a real Steam
// stall on it.
type Settler struct {
	// Instance names the instance in the budget message, so the journalctl
	// line it prints is one the operator can paste.
	Instance string
	// Expect is the positive signal's definition. Empty means there is
	// nothing to wait for, which is the honest answer for a game with no
	// content block (Valheim) and returns immediately with Positive true.
	Expect []Expectation
	// Roots are the directories whose size and mtime quiescence is measured
	// over. A root that does not exist yet counts as empty rather than as an
	// error: on a rebuild's first boot none of them exist at the first poll.
	Roots []string

	// Poll, Quiet and Budget default to DefaultPoll, DefaultQuiet and
	// BudgetUpdate.
	Poll   time.Duration
	Quiet  time.Duration
	Budget time.Duration

	// Now and Sleep are the clock. Injected together, because a test that
	// moved one without the other would measure a quiescence window against a
	// clock that never advanced.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// NonEmpty answers the positive signal for one candidate directory. Nil
	// uses os.ReadDir, where a missing directory is "not present" and any
	// other error is returned: a permission error on a workshop root is a
	// configuration fault, not an absent mod.
	NonEmpty func(dir string) (bool, error)
	// Walk lists one root. Nil walks the real filesystem.
	Walk func(root string) ([]Entry, error)

	// Notes receives the observations that belong in the job log rather than
	// in an error: how many polls it took, and which signal ended the wait.
	Notes func(format string, args ...any)
}

// Wait is phase 8.
//
// It returns the Settlement the job layer records. An error is only ever the
// budget or a cancelled context: a stall is a RESULT, not a failure of this
// function, and the difference is what lets the job carry on and start the
// server rather than treating a late mod as a reason to stay down.
func (s *Settler) Wait(ctx context.Context) (jobs.Settlement, error) {
	if s == nil {
		return jobs.Settlement{}, errors.New("update: no settler")
	}
	start := s.now()
	var (
		prev       string
		havePrev   bool
		quietSince time.Time
		polls      int
	)

	for {
		if err := ctx.Err(); err != nil {
			return jobs.Settlement{Waited: s.since(start)}, err
		}
		polls++

		missing, err := s.missing()
		if err != nil {
			return jobs.Settlement{Waited: s.since(start)}, err
		}
		if len(missing) == 0 {
			// The positive signal. It ends the wait whatever the clock says,
			// which is the half of D-33 that makes a fast box fast: a nightly
			// where every item was already on disk costs one poll.
			s.note("content settled: every expected item is present after %d poll(s) in %s",
				polls, s.since(start).Round(time.Second))
			return jobs.Settlement{Positive: true, Waited: s.since(start)}, nil
		}

		sig, err := s.signature()
		if err != nil {
			return jobs.Settlement{Missing: missing, Waited: s.since(start)}, err
		}

		switch {
		case havePrev && sig == prev:
			// Quiescence is only ever measured BETWEEN polls, which is what
			// "after at least one poll" means: a single observation of a tree
			// says nothing about whether it is changing.
			if quietSince.IsZero() {
				quietSince = s.now()
			}
			if quiet := s.now().Sub(quietSince); quiet >= s.quiet() {
				s.note("content stopped changing %s ago with %d item(s) still missing, so this "+
					"is a stall and not a slow download", quiet.Round(time.Second), len(missing))
				return jobs.Settlement{Missing: missing, Waited: s.since(start)}, nil
			}
		default:
			// It moved. The quiescence clock restarts from zero rather than
			// accumulating, because a download that writes one chunk every 40
			// seconds is progress and must not be declared stalled by a
			// window that only ever counted up.
			quietSince = time.Time{}
		}
		prev, havePrev = sig, true

		if waited := s.since(start); waited >= s.budget() {
			return jobs.Settlement{Missing: missing, Waited: waited}, fmt.Errorf(
				"%w: %s elapsed and %d item(s) are still missing (%s). The roots were still "+
					"changing, so this is not a stall gvm can name -- check free disk and "+
					"`journalctl -u gvm@%s`",
				ErrSettleBudget, waited.Round(time.Second), len(missing),
				strings.Join(missing, " "), s.instance())
		}
		if err := s.sleep(ctx, s.poll()); err != nil {
			return jobs.Settlement{Missing: missing, Waited: s.since(start)}, err
		}
	}
}

// missing is rebuild.sh's NOT_DOWNLOADED loop.
//
// An id is present when any of its candidate directories exists and holds at
// least one entry. "The root exists" is deliberately not the test: SteamCMD
// creates the workshop content directory before it has downloaded anything
// into it, so a check at that level passes on an empty tree -- which is the
// difference between a server that boots with 32 mods and one that boots with
// none and no error.
func (s *Settler) missing() ([]string, error) {
	var out []string
	for _, e := range s.Expect {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		found := false
		for _, dir := range e.Dirs {
			ok, err := s.nonEmpty(dir)
			if err != nil {
				return nil, fmt.Errorf("update: checking whether %s arrived in %s: %w",
					id, dir, err)
			}
			if ok {
				found = true
				break
			}
		}
		if !found {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// signature is one poll's view of every root, as a single comparable string.
//
// A digest rather than a retained file list, so a 40,000-file workshop tree
// costs one hash to compare instead of 40,000 map lookups, and so two polls
// cannot differ because of walk order. Size and mtime only -- see Entry.
func (s *Settler) signature() (string, error) {
	h := sha256.New()
	for _, root := range s.Roots {
		entries, err := s.walk(root)
		if err != nil {
			return "", err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
		fmt.Fprintf(h, "root %s %d\n", filepath.ToSlash(root), len(entries))
		for _, e := range entries {
			h.Write([]byte(filepath.ToSlash(e.Rel)))
			h.Write([]byte{0})
			h.Write([]byte(strconv.FormatInt(e.Size, 10)))
			h.Write([]byte{0})
			h.Write([]byte(strconv.FormatInt(e.ModTime.UnixNano(), 10)))
			h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Settler) nonEmpty(dir string) (bool, error) {
	if s.NonEmpty != nil {
		return s.NonEmpty(dir)
	}
	return DirNonEmpty(dir)
}

func (s *Settler) walk(root string) ([]Entry, error) {
	if s.Walk != nil {
		return s.Walk(root)
	}
	return WalkRoot(root)
}

// DirNonEmpty is the default NonEmpty: present and holding at least one entry.
//
// A missing directory is (false, nil) because that is the ordinary state of a
// workshop item that has not downloaded yet. Everything else is returned: a
// root gvm cannot read is a host misconfiguration, and reporting it as "the
// mod did not arrive" would send the operator to the Workshop to look for a
// file that is sitting on their own disk.
func DirNonEmpty(dir string) (bool, error) {
	if strings.TrimSpace(dir) == "" {
		return false, nil
	}
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil {
		// io.EOF is how Readdirnames reports an empty directory, which is a
		// fact and not a failure; ErrNotExist is the directory having been
		// removed between the Open and the read.
		if errors.Is(err, io.EOF) || errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return len(names) > 0, nil
}

// WalkRoot is the default Walk: every regular file under root.
//
// A missing root is an empty list, for the same reason DirNonEmpty tolerates
// one: on a rebuild's first boot none of the roots exist at the first poll, and
// their appearance is itself the change the quiescence check should see.
// Unreadable subtrees are skipped rather than failed, because a single
// permission oddity inside a 40,000-file workshop tree must not turn the whole
// wait into an error -- it would be measured identically on every poll anyway,
// which quiescence reads correctly as "not changing".
func WalkRoot(root string) ([]Entry, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	var out []Entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		out = append(out, Entry{Rel: rel, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (s *Settler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Settler) since(start time.Time) time.Duration {
	d := s.now().Sub(start)
	if d < 0 {
		return 0
	}
	return d
}

func (s *Settler) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Settler) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return DefaultPoll
}

func (s *Settler) quiet() time.Duration {
	if s.Quiet > 0 {
		return s.Quiet
	}
	return DefaultQuiet
}

func (s *Settler) budget() time.Duration {
	if s.Budget > 0 {
		return s.Budget
	}
	return BudgetUpdate
}

func (s *Settler) instance() string {
	if n := strings.TrimSpace(s.Instance); n != "" {
		return n
	}
	return "<instance>"
}

func (s *Settler) note(format string, args ...any) {
	if s.Notes != nil {
		s.Notes(format, args...)
	}
}
