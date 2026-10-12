package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// The fakes. Every one of them exists so that a rule this package enforces can
// be asserted on the development machine, which is Windows: the freeze
// ordering, the lease arming, the chown that must be fatal, the thaw that must
// happen before a verify, and the never-leave-a-world-unsaved rule.

// testNow is a fixed instant. Written out rather than time.Now() so a deadline
// in a failure message is the same string every run and a bisect over this
// package does not chase a clock.
var testNow = time.Date(2026, 9, 4, 4, 0, 3, 0, time.UTC)

// fakeClock is a clock the tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	// slept records every duration something asked to wait for, so a test can
	// assert that a 120-second Minecraft flush asked for 120 seconds without
	// taking 120 seconds.
	slept []time.Duration
}

func newClock(at time.Time) *fakeClock { return &fakeClock{t: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Sleep is Channels.Sleep: it advances the clock instead of blocking, which is
// what makes a test of a 120-second flush instant and deterministic.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// fakeFile is one entry in fakeFS.
type fakeFile struct {
	data  []byte
	mode  fs.FileMode
	dir   bool
	owner string
	group string
	mod   time.Time
}

// fakeFS is an in-memory FS with injectable failures.
//
// Paths are normalised to slash form, so a test can assert against the
// slash-separated paths internal/instance builds -- which is the whole reason
// instance.PathsFor uses path.Join and never filepath.Join.
type fakeFS struct {
	mu    sync.Mutex
	files map[string]*fakeFile
	// FailChown, FailWrite and FailRemove make the three interesting failures
	// injectable by path substring.
	FailChown string
	FailWrite string
	// ReadAs is the account ReadFile pretends to be, so the permission half
	// of the lease handshake is assertable off Linux: a 0600 file owned by
	// somebody else is EACCES, which is what the runner gets from a lease
	// published before its chown.
	ReadAs     string
	FailRemove string
	// Avail is what DiskUsage reports. Zero with Statfs unset reports plenty.
	Avail      uint64
	FailStatfs bool
	// Writes records every WriteFile in order, for an ordering assertion.
	Writes []string
	// onWrite fires on every WriteFile, so a test can interleave the lease
	// arming with the console sends and assert which came first -- which is
	// the whole of D-31's ordering claim.
	onWrite func(path string)
	// Chowns records every chown as "path owner:group".
	Chowns []string
	// RemoveAlls records every recursive removal, which is how a test asserts
	// that the snapshot clone was cleaned up without the fake and the real
	// filesystem having to agree about where it lived.
	RemoveAlls []string
	// history keeps every body ever written to a path, so a test can read
	// back a document that has since been removed -- which is the only way to
	// see that a lease's deadline was extended before the lease was cleared.
	history map[string][][]byte
}

func newFS() *fakeFS {
	return &fakeFS{files: map[string]*fakeFile{}, Avail: 1 << 40}
}

func norm(p string) string { return path.Clean(strings.ReplaceAll(p, `\`, "/")) }

func (f *fakeFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[norm(p)]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: path.Base(norm(p)), f: ff}, nil
}

func (f *fakeFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[norm(p)]
	if !ok || ff.dir {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if f.ReadAs != "" && ff.mode.Perm()&0o044 == 0 && ff.owner != f.ReadAs {
		// Not group- or world-readable and owned by somebody else. The real
		// /run/gvm/<name> is 2750 gvm-<n>:gvm-console and the lease is 0600,
		// so this is precisely what the runner sees for a lease it does not
		// own.
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
	}
	return append([]byte(nil), ff.data...), nil
}

func (f *fakeFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	if f.FailWrite != "" && strings.Contains(norm(p), f.FailWrite) {
		return fmt.Errorf("injected write failure for %s", p)
	}
	f.mu.Lock()
	f.files[norm(p)] = &fakeFile{data: append([]byte(nil), data...), mode: perm, mod: testNow}
	f.Writes = append(f.Writes, norm(p))
	if f.history == nil {
		f.history = map[string][][]byte{}
	}
	f.history[norm(p)] = append(f.history[norm(p)], append([]byte(nil), data...))
	hook := f.onWrite
	f.mu.Unlock()
	if hook != nil {
		hook(norm(p))
	}
	return nil
}

// WriteFileOwned publishes in one step, so the file never exists with the
// wrong owner -- which is the invariant the production implementation sets the
// owner on the temporary file for. The failure seams are both honoured, so a
// test that injects a chown failure still asserts that nothing was published.
func (f *fakeFS) WriteFileOwned(p string, data []byte, perm fs.FileMode, owner, group string) error {
	if f.FailWrite != "" && strings.Contains(norm(p), f.FailWrite) {
		return fmt.Errorf("injected write failure for %s", p)
	}
	if owner != "" && f.FailChown != "" && strings.Contains(norm(p), f.FailChown) {
		return fmt.Errorf("injected chown failure for %s", p)
	}
	f.mu.Lock()
	ff := &fakeFile{data: append([]byte(nil), data...), mode: perm, mod: testNow}
	if owner != "" {
		ff.owner, ff.group = owner, group
		f.Chowns = append(f.Chowns, norm(p)+" "+owner+":"+group)
	}
	f.files[norm(p)] = ff
	f.Writes = append(f.Writes, norm(p))
	if f.history == nil {
		f.history = map[string][][]byte{}
	}
	f.history[norm(p)] = append(f.history[norm(p)], append([]byte(nil), data...))
	hook := f.onWrite
	f.mu.Unlock()
	if hook != nil {
		hook(norm(p))
	}
	return nil
}

func (f *fakeFS) Chown(p, owner, group string) error {
	if f.FailChown != "" && strings.Contains(norm(p), f.FailChown) {
		return fmt.Errorf("injected chown failure for %s", p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[norm(p)]
	if !ok {
		return &fs.PathError{Op: "chown", Path: p, Err: fs.ErrNotExist}
	}
	ff.owner, ff.group = owner, group
	f.Chowns = append(f.Chowns, norm(p)+" "+owner+":"+group)
	return nil
}

func (f *fakeFS) Remove(p string) error {
	if f.FailRemove != "" && strings.Contains(norm(p), f.FailRemove) {
		return fmt.Errorf("injected remove failure for %s", p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[norm(p)]; !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	delete(f.files, norm(p))
	return nil
}

func (f *fakeFS) RemoveAll(p string) error {
	if f.FailRemove != "" && strings.Contains(norm(p), f.FailRemove) {
		return fmt.Errorf("injected remove failure for %s", p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RemoveAlls = append(f.RemoveAlls, norm(p))
	pre := norm(p)
	for k := range f.files {
		if k == pre || strings.HasPrefix(k, pre+"/") {
			delete(f.files, k)
		}
	}
	return nil
}

func (f *fakeFS) MkdirAll(p string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := ""
	for _, part := range strings.Split(strings.TrimPrefix(norm(p), "/"), "/") {
		cur += "/" + part
		if _, ok := f.files[cur]; !ok {
			f.files[cur] = &fakeFile{dir: true, mode: perm, mod: testNow}
		}
	}
	return nil
}

func (f *fakeFS) ReadDir(p string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pre := norm(p)
	if ff, ok := f.files[pre]; !ok || !ff.dir {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
	}
	var out []fs.DirEntry
	for k, ff := range f.files {
		if !strings.HasPrefix(k, pre+"/") {
			continue
		}
		rest := strings.TrimPrefix(k, pre+"/")
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, fakeInfo{name: rest, f: ff})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (f *fakeFS) DiskUsage(p string) (platform.DiskUsage, error) {
	if f.FailStatfs {
		return platform.DiskUsage{}, platform.Unsupported("statfs")
	}
	return platform.DiskUsage{Total: f.Avail * 2, Free: f.Avail, Avail: f.Avail}, nil
}

// mode is the recorded mode of one path, for asserting the lease is 0600.
func (f *fakeFS) mode(p string) fs.FileMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[norm(p)]
	if !ok {
		return 0
	}
	return ff.mode
}

func (f *fakeFS) owner(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[norm(p)]
	if !ok {
		return ""
	}
	return ff.owner
}

// wroteBodies is every version of a path that was ever written, in order.
func (f *fakeFS) wroteBodies(p string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.history[norm(p)]
}

// askedToRemoveAll reports whether p was handed to RemoveAll.
func (f *fakeFS) askedToRemoveAll(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.RemoveAlls {
		if got == norm(p) {
			return true
		}
	}
	return false
}

func (f *fakeFS) has(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[norm(p)]
	return ok
}

// fakeInfo is both an fs.FileInfo and an fs.DirEntry, which is all the two
// callers here need.
type fakeInfo struct {
	name string
	f    *fakeFile
}

func (i fakeInfo) Name() string               { return i.name }
func (i fakeInfo) Size() int64                { return int64(len(i.f.data)) }
func (i fakeInfo) Mode() fs.FileMode          { return i.f.mode }
func (i fakeInfo) ModTime() time.Time         { return i.f.mod }
func (i fakeInfo) IsDir() bool                { return i.f.dir }
func (i fakeInfo) Sys() any                   { return nil }
func (i fakeInfo) Type() fs.FileMode          { return i.f.mode.Type() }
func (i fakeInfo) Info() (fs.FileInfo, error) { return i, nil }

// sent is one command a fake channel received.
type sent struct {
	Via  Via
	Line string
}

// fakeConsole is a ConsoleSender that records.
type fakeConsole struct {
	mu sync.Mutex
	// StartSeq is where the echo sequences begin, so a test can put the ring
	// cursor behind or ahead of the echo and assert which one the marker
	// search starts from.
	StartSeq uint64
	seq      uint64
	got      []sent
	Fail     error
}

func (c *fakeConsole) Send(ctx context.Context, actor, line string) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return 0, c.Fail
	}
	if c.seq == 0 {
		c.seq = c.StartSeq
	}
	c.seq++
	c.got = append(c.got, sent{Via: ViaConsole, Line: line})
	return c.seq, nil
}

func (c *fakeConsole) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.got))
	for _, s := range c.got {
		out = append(out, s.Line)
	}
	return out
}

// fakeControl is a ControlChannel that records.
type fakeControl struct {
	mu       sync.Mutex
	got      []sent
	NotReady string
	Fail     error
	// FailOnce clears Fail after the first refusal, which is the transient
	// shape a real control channel has: rconctl dials per Exec, so a momentary
	// RCON drop fails one send and the next one works.
	FailOnce bool
	// IgnoreCtx keeps the pre-existing behaviour for the one test whose claim
	// is that the engine still ASKS on a cancelled context. Everywhere else
	// the fake honours it, because the production channel does:
	// rconctl.Exec does connect(ctx) and then conn.Execute(ctx, cmd).
	IgnoreCtx bool
}

func (c *fakeControl) Ready() bool     { return c.NotReady == "" }
func (c *fakeControl) Unready() string { return c.NotReady }

func (c *fakeControl) Exec(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.IgnoreCtx {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if c.Fail != nil {
		err := c.Fail
		if c.FailOnce {
			c.Fail = nil
		}
		return "", err
	}
	c.got = append(c.got, sent{Via: ViaControl, Line: cmd})
	return "ok", nil
}

func (c *fakeControl) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.got))
	for _, s := range c.got {
		out = append(out, s.Line)
	}
	return out
}

// fakeLog is a LogWatcher whose answer each test chooses.
type fakeLog struct {
	mu sync.Mutex
	// Match decides whether the marker appears. nil matches.
	Match error
	// Cursors and Awaits record what was asked, so the test can assert the
	// cursor was taken BEFORE the send -- the thing that stops a previous
	// autosave's "save complete" from satisfying the wait instantly.
	cursor uint64
	awaits []awaited
}

type awaited struct {
	Pattern string
	Since   uint64
	Budget  time.Duration
}

func (l *fakeLog) Cursor() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cursor
}

func (l *fakeLog) Await(ctx context.Context, pattern string, since uint64, budget time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.awaits = append(l.awaits, awaited{Pattern: pattern, Since: since, Budget: budget})
	return l.Match
}

func (l *fakeLog) asked() []awaited {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]awaited(nil), l.awaits...)
}

// fakeThawer is the RUNNER's side of D-31: a different process, modelled as a
// recorder, which is the only way to assert "a fake runner honouring the lease
// thaws at the deadline" without a VPS.
type fakeThawer struct {
	mu   sync.Mutex
	got  []ThawStep
	Fail error
}

func (t *fakeThawer) Thaw(ctx context.Context, step ThawStep) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Fail != nil {
		return t.Fail
	}
	t.got = append(t.got, step)
	return nil
}

func (t *fakeThawer) steps() []ThawStep {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ThawStep(nil), t.got...)
}

// errInjected is the generic injected failure.
var errInjected = errors.New("injected failure")

// isLinux says which half of the snapshot seam is compiled in.
//
// runtime.GOOS rather than a build-tagged test file, because the two sides
// assert each other's CONTRACT -- the stub refuses, the real one asks the
// kernel -- and a pair of tagged files would let one side's assertion be
// deleted without the other noticing.
func isLinux() bool { return runtime.GOOS == "linux" }

// quietLogger discards the watchdog's output.
//
// The thaw path logs at Error level by design -- DEGRADED_FREEZE is what an
// operator greps for after a rollback -- and a package whose tests exercise
// that path forty times would otherwise print forty alarming lines on a green
// run. The log CONTENT is asserted through Tick.Why, which is the same
// sentence.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// mustArm arms a lease and fails the test if it could not.
func mustArm(t *testing.T, f FS, req ArmRequest) *Lease {
	t.Helper()
	l, _, err := Arm(context.Background(), f, req)
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	return l
}

// armed is a lease with everything filled in, for a test that only cares about
// one field.
func armed(instanceName string, at time.Time, budget time.Duration, steps ...ThawStep) Lease {
	return Lease{
		Instance: instanceName,
		Reason:   "backup:routine",
		OwnerPID: 48812,
		// Pinned, because "provably dead" is only meaningful for a pinned
		// owner: platform.Alive compares /proc field 22 against what it was
		// handed, so a LIVE pid read against a zero pin comes back false.
		OwnerStartTicks: 918_244,
		OwnerUnit:       "gvm-job@" + instanceName + "_nightly.service",
		ArmedAt:         at,
		Deadline:        at.Add(budget),
		ThawSteps:       steps,
	}
}
