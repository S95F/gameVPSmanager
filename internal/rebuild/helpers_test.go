package rebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/update"
)

const testInstance = "pz-main"

// injector is the shared failure seam, and it is the mechanism that makes the
// ordering invariant provable rather than reviewed.
//
// Every fallible call in every fake goes through do(name). A clean run records
// the names in order; the enumeration in rebuild_test.go then re-runs the
// rebuild once per recorded name, with an error, a panic and a cancellation at
// that name. A step added to rebuild.go appears in the clean run's list on the
// day it is added and is therefore covered on the same day -- which is the
// only way an ordering invariant survives a year of edits.
type injector struct {
	mu sync.Mutex

	// at is the call to fail, and mode is how.
	at   string
	mode string

	// cancel is invoked for mode "cancel", so that the cancellation arrives
	// the way a real one does: from systemd's TimeoutStartSec, mid-call.
	cancel func()

	calls []string
}

func (i *injector) do(name string) error {
	i.mu.Lock()
	i.calls = append(i.calls, name)
	hit := name == i.at
	mode := i.mode
	cancel := i.cancel
	i.mu.Unlock()

	if !hit {
		return nil
	}
	switch mode {
	case "error":
		return fmt.Errorf("injected failure at %s", name)
	case "panic":
		panic("injected panic at " + name)
	case "cancel":
		if cancel != nil {
			cancel()
		}
		return context.Canceled
	}
	return nil
}

func (i *injector) recorded() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.calls...)
}

func (i *injector) count(name string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := 0
	for _, c := range i.calls {
		if c == name {
			n++
		}
	}
	return n
}

// fakeContent is internal/content: the two lists, the acquisition list, the
// expectation set and the resolver.
type fakeContent struct {
	inj    *injector
	plan   jobs.ContentPlan
	settle update.SettleInput
	res    jobs.Resolution

	writes int
	fetch  []bool
}

func (c *fakeContent) Plan(_ context.Context, fetch bool) (jobs.ContentPlan, error) {
	c.fetch = append(c.fetch, fetch)
	if err := c.inj.do("content.plan"); err != nil {
		return jobs.ContentPlan{}, err
	}
	return c.plan, nil
}

func (c *fakeContent) Write(context.Context) error {
	if err := c.inj.do("content.write"); err != nil {
		return err
	}
	c.writes++
	return nil
}

func (c *fakeContent) Settlement(context.Context) (update.SettleInput, error) {
	if err := c.inj.do("content.settlement"); err != nil {
		return update.SettleInput{}, err
	}
	return c.settle, nil
}

func (c *fakeContent) Resolve(context.Context) (jobs.Resolution, error) {
	if err := c.inj.do("content.resolve"); err != nil {
		return jobs.Resolution{}, err
	}
	return c.res, nil
}

// fakeServer is a fake systemd plus a fake readiness marker.
type fakeServer struct {
	inj *injector
	// startHook lets a test fail the Nth start rather than every one. The
	// rebuild starts the server twice on the happy path -- boot #1 and the
	// new world -- and those two failures have opposite meanings for whether
	// the world survived.
	startHook func() error
	// stopHook is startHook's twin, and it exists for the same reason: the
	// rebuild stops the server twice -- step 3's quiesce and step 4's
	// clean-stop after the download boot -- and the injector fails EVERY
	// occurrence of a name, so it can only ever reach the first one. A test
	// about the second stop has to be able to fail only the second.
	stopHook func() error

	mu      sync.Mutex
	active  bool
	stops   int
	starts  int
	readies int
}

func (s *fakeServer) Stop(_ context.Context, _ time.Duration) (lifecycle.Outcome, error) {
	if err := s.inj.do("server.stop"); err != nil {
		return lifecycle.Outcome{}, err
	}
	if s.stopHook != nil {
		if err := s.stopHook(); err != nil {
			return lifecycle.Outcome{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	s.active = false
	return lifecycle.Outcome{Rung: "saved", Took: 14 * time.Second}, nil
}

func (s *fakeServer) Start(context.Context) error {
	if err := s.inj.do("server.start"); err != nil {
		return err
	}
	if s.startHook != nil {
		if err := s.startHook(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	s.active = true
	return nil
}

func (s *fakeServer) WaitReady(_ context.Context, _ time.Duration) error {
	if err := s.inj.do("server.waitready"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readies++
	return nil
}

func (s *fakeServer) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

type fakeTimers struct {
	inj      *injector
	masked   int
	unmasked int
}

// Mask and Unmask consult the context because every real implementation is a
// D-Bus call through hostops -- systemd_linux.go uses the *Context variants
// exclusively and its job() selects on ctx.Done() -- so a cancelled context is
// a FAILED unmask, not a skipped one. Without this the enumeration's `cancel`
// mode walks straight past the one failure that leaves a box with no backups.
func (t *fakeTimers) Mask(ctx context.Context) error {
	if err := t.inj.do("timers.mask"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.masked++
	return nil
}

func (t *fakeTimers) Unmask(ctx context.Context) error {
	if err := t.inj.do("timers.unmask"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.unmasked++
	return nil
}

// fakeArchiver is internal/backup, reduced to the three calls step 7 makes.
//
// skipVerifyStamp is the failure mode that cannot be seen from an error: an
// engine that reports an archive verified and leaves VerifiedAt zero. An
// unverified Ref is not a receipt, and instance.Destroy panics on one -- so the
// rebuild has to catch it first, with a sentence, rather than reaching the
// panic.
type fakeArchiver struct {
	inj             *injector
	noRef           bool
	skipVerifyStamp bool

	// mode, members and checked are the receipt the engine hands back. A
	// shallow mode is the failure that cannot be seen from an error: it
	// stamps VerifiedAt and compares no member CONTENTS, so it is not the
	// receipt section 13.6 step 7 requires before a world is deleted.
	mode     archive.Mode
	members  int
	checked  int
	noReport bool

	// srv is read at call time, not asserted here: the ordering list is a
	// hand-written fixture and can be made to agree with a wrong ordering,
	// whereas "was the world live when its only copy was taken" is the
	// property itself.
	srv *fakeServer
	// liveAtSnapshot records whether the server was still serving when
	// Snapshot was called.
	liveAtSnapshot []bool

	refs []*archive.Ref
}

func (a *fakeArchiver) Preflight(context.Context) error { return a.inj.do("archives.preflight") }

func (a *fakeArchiver) Snapshot(_ context.Context, class archive.Class, reason string) (*archive.Ref, error) {
	if err := a.inj.do("archives.snapshot"); err != nil {
		return nil, err
	}
	if a.srv != nil {
		a.liveAtSnapshot = append(a.liveAtSnapshot, a.srv.isActive())
	}
	if a.noRef {
		return nil, nil
	}
	ref := &archive.Ref{
		Path:      "/var/backups/gvm/pz-main/pz-main-20260904-182211-" + string(class) + ".tar.gz",
		SHA256:    "1b40c9de00000000000000000000000000000000000000000000000000000000",
		Bytes:     2_576_980_377,
		Instance:  testInstance,
		Class:     class,
		Reason:    reason,
		CreatedAt: time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC),
	}
	a.refs = append(a.refs, ref)
	return ref, nil
}

func (a *fakeArchiver) VerifyReport(_ context.Context, ref *archive.Ref) (*archive.Report, error) {
	if err := a.inj.do("archives.verify"); err != nil {
		return nil, err
	}
	if !a.skipVerifyStamp && ref != nil {
		ref.VerifiedAt = time.Date(2026, 9, 4, 18, 31, 2, 0, time.UTC)
	}
	if a.noReport {
		return nil, nil
	}
	rep := &archive.Report{Mode: archive.ModeSpot, Members: 24, Checked: 24}
	if a.mode != "" {
		rep.Mode = a.mode
	}
	if a.members > 0 {
		rep.Members, rep.Checked = a.members, a.checked
	}
	if ref != nil {
		rep.Path, rep.SHA256, rep.Bytes = ref.Path, ref.SHA256, ref.Bytes
	}
	return rep, nil
}

// fakeDestroyer is the ONE thing every ordering assertion in this package
// counts. Zero calls is the promise.
//
// It re-asserts the two properties instance.Destroy enforces, so a rebuild that
// somehow reached it with a nil or unverified Ref fails HERE with a readable
// message rather than in a panic from another package.
type fakeDestroyer struct {
	inj *injector
	// srv is read at call time: an rm -rf of the world paths while the JVM
	// still has them open is the data-loss half of the ordering, and no
	// guard in instance.Destroy checks the run state.
	srv *fakeServer

	mu         sync.Mutex
	calls      int
	liveAtCall []bool
	refs       []*archive.Ref
	targets    [][]string
}

func (d *fakeDestroyer) Destroy(_ context.Context, ref *archive.Ref, worldPaths []string) error {
	if err := d.inj.do("destroyer.destroy"); err != nil {
		return err
	}
	live := false
	if d.srv != nil {
		live = d.srv.isActive()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.liveAtCall = append(d.liveAtCall, live)
	d.calls++
	d.refs = append(d.refs, ref)
	d.targets = append(d.targets, worldPaths)
	switch {
	case ref == nil:
		return errors.New("destroy was called with a nil ref")
	case !ref.Verified():
		return errors.New("destroy was called with an unverified ref")
	}
	return nil
}

func (d *fakeDestroyer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type fakeHooks struct {
	inj     *injector
	applied int
}

func (h *fakeHooks) Apply(context.Context) error {
	if err := h.inj.do("hooks.apply"); err != nil {
		return err
	}
	h.applied++
	return nil
}

// rig is one rebuild's worth of fakes, wired so that a clean run reaches every
// step -- which is what makes the enumeration cover the whole table rather
// than its happy prefix.
type rig struct {
	cfg  Config
	inj  *injector
	cont *fakeContent
	srv  *fakeServer
	tim  *fakeTimers
	arch *fakeArchiver
	des  *fakeDestroyer
	hook *fakeHooks
}

func newRig() *rig {
	inj := &injector{}
	r := &rig{
		inj:  inj,
		cont: &fakeContent{inj: inj},
		srv:  &fakeServer{inj: inj, active: true},
		tim:  &fakeTimers{inj: inj},
		arch: &fakeArchiver{inj: inj},
		des:  &fakeDestroyer{inj: inj},
		hook: &fakeHooks{inj: inj},
	}
	r.arch.srv, r.des.srv = r.srv, r.srv
	r.cont.plan = jobs.ContentPlan{
		Changed: true,
		Added:   []string{"2169435993"},
		Removed: []string{"1299328280"},
	}
	// No expectations: the positive signal is immediate, so step 5 does not
	// need a clock in every test that is not about step 5.
	r.cont.settle = update.SettleInput{Roots: []string{"/srv/gvm/pz-main/workshop"}}
	r.cont.res = jobs.Resolution{Changed: true, After: []string{"Authentic Z", "Brita"}}

	r.cfg = Config{
		Instance:   testInstance,
		Content:    r.cont,
		Server:     r.srv,
		Timers:     r.tim,
		Archives:   r.arch,
		Destroyer:  r.des,
		Hooks:      r.hook,
		WorldPaths: []string{"/srv/gvm/pz-main/data/Saves/Multiplayer/pz-main"},
		Log:        discardLogger(),
		Notes:      func(string, ...any) {},
	}
	return r
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
