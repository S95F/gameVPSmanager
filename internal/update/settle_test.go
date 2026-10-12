package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeWorld is a filesystem whose clock only moves when the settler sleeps.
//
// That coupling is the whole reason these tests can assert on a 45-second
// quiescence window in a microsecond: every Sleep advances the clock by exactly
// the requested duration and then runs an optional mutation, so "the roots
// changed between poll 3 and poll 4" is a statement about the table and not
// about the machine's load.
type fakeWorld struct {
	at    time.Time
	polls int

	// files is the current state, per root.
	files map[string][]Entry
	// present is the set of directories that count as non-empty.
	present map[string]bool

	// onSleep mutates the world between polls. It receives the poll number
	// that just completed.
	onSleep func(w *fakeWorld, poll int)

	// walkErr and emptyErr are the two seam failures.
	walkErr  error
	emptyErr error

	sleeps int
}

func newWorld() *fakeWorld {
	return &fakeWorld{
		at:      time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC),
		files:   map[string][]Entry{},
		present: map[string]bool{},
	}
}

func (w *fakeWorld) now() time.Time { return w.at }

func (w *fakeWorld) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.sleeps++
	w.at = w.at.Add(d)
	if w.onSleep != nil {
		w.onSleep(w, w.sleeps)
	}
	return nil
}

func (w *fakeWorld) walk(root string) ([]Entry, error) {
	if w.walkErr != nil {
		return nil, w.walkErr
	}
	w.polls++
	return w.files[root], nil
}

func (w *fakeWorld) nonEmpty(dir string) (bool, error) {
	if w.emptyErr != nil {
		return false, w.emptyErr
	}
	return w.present[dir], nil
}

func (w *fakeWorld) settler(expect []Expectation, roots ...string) *Settler {
	return &Settler{
		Instance: testInstance,
		Expect:   expect,
		Roots:    roots,
		Poll:     5 * time.Second,
		Quiet:    45 * time.Second,
		Budget:   1800 * time.Second,
		Now:      w.now,
		Sleep:    w.sleep,
		NonEmpty: w.nonEmpty,
		Walk:     w.walk,
	}
}

func expectation(ids ...string) []Expectation {
	out := make([]Expectation, 0, len(ids))
	for _, id := range ids {
		out = append(out, Expectation{ID: id, Dirs: []string{"/root/" + id}})
	}
	return out
}

// TestThePositiveSignalEndsTheWaitImmediately.
//
// This is the half of D-33 that makes a fast box fast. `sleep 180` cost three
// minutes on a night when every item was already on disk; here that night
// costs one poll and no sleep at all.
func TestThePositiveSignalEndsTheWaitImmediately(t *testing.T) {
	w := newWorld()
	w.present["/root/2169435993"] = true
	w.present["/root/2460154811"] = true

	st, err := w.settler(expectation("2169435993", "2460154811"), "/root").Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Positive {
		t.Error("the positive signal was not reported")
	}
	if len(st.Missing) != 0 {
		t.Errorf("missing = %v, want none", st.Missing)
	}
	if w.sleeps != 0 {
		t.Errorf("slept %d time(s) with everything already present; this is the `sleep 180` "+
			"this code exists to delete", w.sleeps)
	}
	if st.Waited != 0 {
		t.Errorf("waited = %v, want 0", st.Waited)
	}
}

// TestASlowDownloadIsWaitedForRatherThanDeclaredStalled.
//
// The other half: 180 seconds was also too SHORT, which is the failure that
// produces a mod reported missing because it was merely late. Here the item
// arrives on the fourth poll and the wait ends then -- not at a fixed number.
func TestASlowDownloadIsWaitedForRatherThanDeclaredStalled(t *testing.T) {
	w := newWorld()
	// The tree grows while the download runs, so quiescence never fires.
	w.onSleep = func(w *fakeWorld, poll int) {
		w.files["/root"] = append(w.files["/root"], Entry{
			Rel: "chunk", Size: int64(poll) * 1024, ModTime: w.at,
		})
		if poll == 3 {
			w.present["/root/2169435993"] = true
		}
	}

	st, err := w.settler(expectation("2169435993"), "/root").Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Positive {
		t.Fatalf("the item arrived and was not reported present: %+v", st)
	}
	if w.sleeps != 3 {
		t.Errorf("slept %d time(s), want 3 (the item arrived during the third)", w.sleeps)
	}
	if want := 15 * time.Second; st.Waited != want {
		t.Errorf("waited = %v, want %v", st.Waited, want)
	}
}

// TestQuiescenceNamesTheIdsThatNeverArrived.
//
// A Steam stall. Quiescence without the positive signal is never treated as
// success: the ids are reported BY NAME, because a count sends the operator
// looking and a name is something they can paste into the Workshop. The job
// layer turns this into `degraded`.
func TestQuiescenceNamesTheIdsThatNeverArrived(t *testing.T) {
	w := newWorld()
	w.present["/root/2169435993"] = true
	// 2460154811 never arrives and nothing under the roots ever changes.
	w.files["/root"] = []Entry{{Rel: "a", Size: 1, ModTime: w.at}}

	st, err := w.settler(expectation("2169435993", "2460154811"), "/root").Wait(context.Background())
	if err != nil {
		t.Fatalf("a stall is a RESULT, not an error from the wait: %v", err)
	}
	if st.Positive {
		t.Error("a stall was reported as the positive signal")
	}
	if len(st.Missing) != 1 || st.Missing[0] != "2460154811" {
		t.Errorf("missing = %v, want [2460154811]", st.Missing)
	}
	if st.Waited < 45*time.Second {
		t.Errorf("waited = %v; quiescence is a 45s window and cannot be concluded sooner",
			st.Waited)
	}
}

// TestQuiescenceNeedsAtLeastOnePoll.
//
// A single observation of a tree says nothing about whether it is changing. So
// even with the quiescence window set to an instant, the wait cannot end on the
// first look: it has to see the same tree twice.
func TestQuiescenceNeedsAtLeastOnePoll(t *testing.T) {
	w := newWorld()
	s := w.settler(expectation("2460154811"), "/root")
	s.Quiet = time.Nanosecond

	st, err := s.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Positive {
		t.Error("nothing arrived and the positive signal was reported")
	}
	if w.sleeps < 1 {
		t.Error("quiescence was declared from a single observation")
	}
}

// TestProgressRestartsTheQuiescenceClock.
//
// A download that writes one chunk every forty seconds is progress. A window
// that only ever counted up would declare it stalled; this one restarts from
// zero on every observed change, so the wait outlives several quiet-window
// lengths and still ends on the positive signal.
func TestProgressRestartsTheQuiescenceClock(t *testing.T) {
	w := newWorld()
	w.onSleep = func(w *fakeWorld, poll int) {
		// A change every eighth poll: 40s apart with a 5s poll and a 45s
		// window, so quiescence is always one poll short of firing.
		if poll%8 == 0 {
			w.files["/root"] = append(w.files["/root"], Entry{
				Rel: "chunk", Size: int64(poll), ModTime: w.at,
			})
		}
		if poll == 30 {
			w.present["/root/2169435993"] = true
		}
	}
	st, err := w.settler(expectation("2169435993"), "/root").Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Positive {
		t.Fatalf("a slow but live download was declared stalled after %v: %+v", st.Waited, st)
	}
	if w.sleeps != 30 {
		t.Errorf("slept %d time(s), want 30", w.sleeps)
	}
}

// TestTheBudgetIsHardAndNamesWhatIsMissing.
//
// A root something else keeps touching would otherwise hold the per-instance
// job lock against tomorrow's nightly forever. The ceiling is hard, and the
// error carries the ids and the instance so the message is actionable.
func TestTheBudgetIsHardAndNamesWhatIsMissing(t *testing.T) {
	w := newWorld()
	w.onSleep = func(w *fakeWorld, poll int) {
		w.files["/root"] = []Entry{{Rel: "churn", Size: int64(poll), ModTime: w.at}}
	}
	s := w.settler(expectation("2460154811"), "/root")
	s.Budget = 60 * time.Second

	st, err := s.Wait(context.Background())
	if !errors.Is(err, ErrSettleBudget) {
		t.Fatalf("err = %v, want ErrSettleBudget", err)
	}
	if len(st.Missing) != 1 {
		t.Errorf("missing = %v, want one id", st.Missing)
	}
	for _, want := range []string{"2460154811", testInstance} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the budget message does not mention %q: %v", want, err)
		}
	}
	if st.Waited < 60*time.Second {
		t.Errorf("waited = %v, want at least the budget", st.Waited)
	}
}

// TestACancelledContextEndsTheWait, because the job's own context is what
// systemd's TimeoutStartSec cancels -- and a phase that ignored it would be the
// thing holding the process open past its SIGKILL.
func TestACancelledContextEndsTheWait(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	w.onSleep = func(*fakeWorld, int) { cancel() }

	_, err := w.settler(expectation("2460154811"), "/root").Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestAnUnreadableWorkshopRootIsNotAMissingMod.
//
// Reporting a permission error as "the mod did not arrive" would send the
// operator to the Steam Workshop to look for a file sitting on their own disk.
func TestAnUnreadableWorkshopRootIsNotAMissingMod(t *testing.T) {
	w := newWorld()
	w.emptyErr = errors.New("permission denied")

	_, err := w.settler(expectation("2169435993"), "/root").Wait(context.Background())
	if err == nil {
		t.Fatal("an unreadable candidate directory was reported as an absent mod")
	}
	if !strings.Contains(err.Error(), "2169435993") || !strings.Contains(err.Error(), "/root/") {
		t.Errorf("the error must name the id and the directory: %v", err)
	}
}

// TestNoExpectationsIsImmediatelyPositive -- a game with no content block
// (Valheim). There is nothing to wait for, and waiting 45 seconds to discover
// that would be the blind sleep under a different name.
func TestNoExpectationsIsImmediatelyPositive(t *testing.T) {
	w := newWorld()
	st, err := w.settler(nil, "/root").Wait(context.Background())
	if err != nil || !st.Positive || w.sleeps != 0 {
		t.Fatalf("st = %+v, err = %v, sleeps = %d", st, err, w.sleeps)
	}
}

// TestARootThatAppearsCountsAsAChange.
//
// On a rebuild's first boot none of the content roots exist at the first poll.
// A missing root therefore has to read as empty rather than as an error, and
// its appearance has to read as activity -- otherwise the very first thing the
// download does would be invisible to the quiescence check.
func TestARootThatAppearsCountsAsAChange(t *testing.T) {
	w := newWorld()
	w.onSleep = func(w *fakeWorld, poll int) {
		switch poll {
		case 2:
			w.files["/root"] = []Entry{{Rel: "first", Size: 10, ModTime: w.at}}
		case 4:
			w.present["/root/2169435993"] = true
		}
	}
	s := w.settler(expectation("2169435993"), "/root")
	// A short window, so that an unnoticed change WOULD end the wait early
	// and the test would fail rather than pass slowly.
	s.Quiet = 6 * time.Second

	st, err := s.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Positive {
		t.Fatalf("the wait ended before the root appeared: %+v after %d sleeps", st, w.sleeps)
	}
}

// TestSizeAndMtimeAreWhatIsCompared, and content is not.
//
// Hashing a 60 GB install tree every five seconds would itself be the disk
// activity the quiescence check is looking at. A file whose size and mtime are
// unchanged is "not changing" for this purpose, deliberately.
func TestSizeAndMtimeAreWhatIsCompared(t *testing.T) {
	w := newWorld()
	at := w.at
	w.files["/root"] = []Entry{{Rel: "a", Size: 100, ModTime: at}}
	s := w.settler(expectation("missing"), "/root")
	s.Quiet = 6 * time.Second

	// Mutating only the mtime is a change; the same size and mtime is not.
	w.onSleep = func(w *fakeWorld, poll int) {
		if poll == 1 {
			w.files["/root"] = []Entry{{Rel: "a", Size: 100, ModTime: at.Add(time.Minute)}}
		}
	}
	st, err := s.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Positive {
		t.Fatal("nothing arrived")
	}
	// Poll 1 and 2 differ (the mtime moved), so the window can only start at
	// poll 2 -- which puts the answer later than a settler that ignored mtime.
	if w.sleeps < 2 {
		t.Errorf("slept %d time(s); an mtime-only change was not noticed", w.sleeps)
	}
}
