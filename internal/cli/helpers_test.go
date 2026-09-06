package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/ports"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// testTime is a fixed clock. Every rendered file carries a timestamp, and a
// wall clock would make the "second apply changes nothing" assertion depend on
// how long the first one took.
var testTime = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// memFS is one in-memory filesystem playing both roles the CLI needs: the
// read-only lifecycle.Files an observer sees, and the hostops.FS every
// privileged write lands on.
//
// It is one type rather than two because the property most of these tests are
// about is that a write through hostops becomes visible to the next
// observation. Two independent fakes would let a test pass while the real
// command wrote somewhere nothing ever reads.
type memFS struct {
	files map[string]*memFile
	// writes is every path written, in order, so a test can assert not only
	// what a command produced but that it produced NOTHING ELSE.
	writes []string
	dirs   []string
	chowns []string
}

type memFile struct {
	data  []byte
	mode  fs.FileMode
	dir   bool
	mtime time.Time
}

func newMemFS() *memFS { return &memFS{files: map[string]*memFile{}} }

func clean(p string) string { return path.Clean(strings.ReplaceAll(p, `\`, "/")) }

func (m *memFS) put(p string, data []byte, mode fs.FileMode) {
	m.files[clean(p)] = &memFile{data: data, mode: mode, mtime: testTime}
	m.mkdirAll(path.Dir(clean(p)))
}

// mkdirAll is for SETTING UP a fixture only. hostops itself creates exactly one
// directory per call and refuses a missing parent, and Mkdir below keeps that
// rule so a plan that ensures directories out of order still fails here.
func (m *memFS) mkdirAll(p string) {
	p = clean(p)
	for p != "/" && p != "." {
		if _, ok := m.files[p]; !ok {
			m.files[p] = &memFile{dir: true, mode: 0o755, mtime: testTime}
		}
		p = path.Dir(p)
	}
}

type memInfo struct {
	name string
	f    *memFile
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return int64(len(i.f.data)) }
func (i memInfo) Mode() fs.FileMode  { return i.f.mode }
func (i memInfo) ModTime() time.Time { return i.f.mtime }
func (i memInfo) IsDir() bool        { return i.f.dir }
func (i memInfo) Sys() any           { return nil }

func (m *memFS) Stat(p string) (fs.FileInfo, error) {
	f, ok := m.files[clean(p)]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return memInfo{name: path.Base(clean(p)), f: f}, nil
}

func (m *memFS) ReadFile(p string) ([]byte, error) {
	f, ok := m.files[clean(p)]
	if !ok || f.dir {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), f.data...), nil
}

type memEntry struct{ memInfo }

func (e memEntry) Type() fs.FileMode          { return e.memInfo.Mode().Type() }
func (e memEntry) Info() (fs.FileInfo, error) { return e.memInfo, nil }

func (m *memFS) ReadDir(p string) ([]fs.DirEntry, error) {
	p = clean(p)
	if f, ok := m.files[p]; !ok || !f.dir {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
	}
	var out []fs.DirEntry
	for k, f := range m.files {
		if path.Dir(k) == p && k != p {
			out = append(out, memEntry{memInfo{name: path.Base(k), f: f}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (m *memFS) WriteAtomic(p string, data []byte, perm fs.FileMode, _ bool) error {
	p = clean(p)
	if f, ok := m.files[path.Dir(p)]; !ok || !f.dir {
		return &fs.PathError{Op: "create", Path: p, Err: fs.ErrNotExist}
	}
	m.files[p] = &memFile{data: append([]byte(nil), data...), mode: perm, mtime: testTime}
	m.writes = append(m.writes, p)
	return nil
}

func (m *memFS) Remove(p string) error {
	delete(m.files, clean(p))
	return nil
}

func (m *memFS) Mkdir(p string, perm fs.FileMode) error {
	p = clean(p)
	if f, ok := m.files[path.Dir(p)]; !ok || !f.dir {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrNotExist}
	}
	m.files[p] = &memFile{dir: true, mode: perm, mtime: testTime}
	m.dirs = append(m.dirs, p)
	return nil
}

func (m *memFS) Chmod(p string, perm fs.FileMode) error {
	if f, ok := m.files[clean(p)]; ok {
		f.mode = perm
	}
	return nil
}

func (m *memFS) Chown(p, owner, group string) error {
	m.chowns = append(m.chowns, fmt.Sprintf("%s %s:%s", clean(p), owner, group))
	return nil
}

func (m *memFS) DiskUsage(string) (platform.DiskUsage, error) {
	return platform.DiskUsage{Total: 1 << 40, Free: 1 << 39, Avail: 1 << 39}, nil
}

// fakeUsers never fails and never modifies. Nothing in these tests turns on
// account creation; what turns on it is that `gvm create` does not do any.
type fakeUsers struct{ have map[string]bool }

func (u fakeUsers) Lookup(_ context.Context, name string) (platform.User, error) {
	if u.have[name] {
		return platform.User{Name: name}, nil
	}
	return platform.User{}, hostops.ErrNoSuchAccount
}

func (u fakeUsers) EnsureSystemUser(_ context.Context, name, home string) (platform.User, error) {
	u.have[name] = true
	return platform.User{Name: name, Home: home}, nil
}

func (u fakeUsers) EnsureGroup(context.Context, string) error        { return nil }
func (u fakeUsers) AddToGroup(context.Context, string, string) error { return nil }

// fakeSystemd records what it was asked to do and answers state questions with
// whatever the test set.
type fakeSystemd struct {
	calls  []string
	states map[string]platform.UnitState
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{states: map[string]platform.UnitState{}}
}

func (s *fakeSystemd) State(_ context.Context, unit string) (platform.UnitState, error) {
	if st, ok := s.states[unit]; ok {
		return st, nil
	}
	return platform.UnitState{Name: unit, LoadState: "not-found", ActiveState: "inactive"}, nil
}

func (s *fakeSystemd) ListUnits(_ context.Context, pattern string) ([]platform.UnitState, error) {
	var out []platform.UnitState
	for _, st := range s.states {
		out = append(out, st)
	}
	_ = pattern
	return out, nil
}

func (s *fakeSystemd) Start(_ context.Context, u string) error   { return s.note("start", u) }
func (s *fakeSystemd) Stop(_ context.Context, u string) error    { return s.note("stop", u) }
func (s *fakeSystemd) Restart(_ context.Context, u string) error { return s.note("restart", u) }
func (s *fakeSystemd) Enable(_ context.Context, u string) error  { return s.note("enable", u) }
func (s *fakeSystemd) Disable(_ context.Context, u string) error { return s.note("disable", u) }
func (s *fakeSystemd) DaemonReload(context.Context) error        { return s.note("daemon-reload", "") }

func (s *fakeSystemd) note(verb, unit string) error {
	s.calls = append(s.calls, strings.TrimSpace(verb+" "+unit))
	return nil
}

// noLock is a ports.LockFunc that does not lock.
//
// The real one is platform.Flock, which is Linux-only; the ledger's
// read-modify-write is proven in internal/ports' own tests. What these tests
// need is the ORDERING -- reserve, write the YAML, release -- and that is
// visible without a real flock.
func noLock(string) (ports.Unlocker, error) { return nopUnlock{}, nil }

type nopUnlock struct{}

func (nopUnlock) Unlock() error { return nil }

// testHost builds a HostEnv with everything faked and nothing reaching the
// developer's machine.
type testHost struct {
	*HostEnv
	FS      *memFS
	Systemd *fakeSystemd
	Users   fakeUsers
	Audit   *audit.MemorySink
	LedgerD string
}

func newTestHost(t *testing.T) *testHost {
	t.Helper()
	m := newMemFS()
	// The directories section 5.1's tmpfiles.d creates on a real box. hostops
	// creates one directory per call and refuses a missing parent, so a test
	// that skipped these would fail for the wrong reason.
	for _, d := range []string{
		"/etc/gvm", "/etc/gvm/instances", "/etc/gvm/games", "/etc/gvm/secrets",
		"/etc/gvm/secrets/instances", "/etc/gvm/jobs", "/etc/systemd/system",
		"/var/lib/gvm", "/var/lib/gvm/instances", "/var/lib/gvm/cache",
		"/var/lib/gvm/cache/steam", "/var/lib/gvm/cache/steam/homes",
		"/var/log/gvm", "/var/backups/gvm", "/srv/gvm", "/run/gvm",
	} {
		m.mkdirAll(d)
	}

	sink := audit.NewMemorySink()
	sd := newFakeSystemd()
	users := fakeUsers{have: map[string]bool{}}
	ops, err := hostops.New(hostops.Config{
		Systemd: sd,
		Users:   users,
		FS:      m,
		Audit:   audit.New(sink),
		LoadInstance: func(p string) (*spec.Instance, spec.Diags, error) {
			raw, err := m.ReadFile(p)
			if err != nil {
				return nil, nil, err
			}
			return spec.ParseInstance(p, raw)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	h := &HostEnv{
		Ops:        ops,
		Files:      m,
		Units:      unitsOver{ops: ops},
		Accounts:   func(n string) bool { return users.have[n] },
		Ledger:     &ports.Store{Dir: dir, Lock: noLock, Now: func() time.Time { return testTime }},
		Catalog:    spec.SystemCatalog(games.FS),
		LoadPinned: func(d string) (*spec.Manifest, error) { return pinnedFromFS(m, d) },
		HasSecret:  func(string, string) bool { return false },
		// The real write-probe launches a transient unit through systemd-run.
		// The fake answers "writable" for every path the sandbox names, which
		// is what a healthy host answers; probe_test.go in internal/reconcile
		// owns the failing shapes.
		Probe: func(_ context.Context, p reconcile.ProbeSpec) ([]reconcile.ProbeResult, error) {
			out := make([]reconcile.ProbeResult, 0, len(p.Sandbox.Paths))
			for _, path := range p.Sandbox.Paths {
				out = append(out, reconcile.ProbeResult{Path: path, OK: true})
			}
			return out, nil
		},
		Listeners: func() ([]ports.Listener, error) { return nil, nil },
		Ephemeral: func() (platform.PortRange, error) { return platform.DefaultEphemeral, nil },
		Now:       func() time.Time { return testTime },
	}
	return &testHost{HostEnv: h, FS: m, Systemd: sd, Users: users, Audit: sink, LedgerD: dir}
}

// pinnedFromFS is spec.LoadPinned against the fake filesystem.
func pinnedFromFS(m *memFS, dir string) (*spec.Manifest, error) {
	p := spec.PinPath(dir)
	raw, err := m.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", spec.ErrNoPin, p)
	}
	mani, diags, err := spec.ValidateBytes(p, raw)
	if err != nil {
		return nil, err
	}
	if diags.HasErrors() {
		return nil, errors.New(diags.Errors().String())
	}
	return mani, nil
}

// runWith drives the whole CLI, from argv to exit code, against a fake host.
func runWith(t *testing.T, h *testHost, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := &Env{
		Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Geteuid: func() int { return 0 },
	}
	if h != nil {
		env.Host = h.HostEnv
	}
	return Main(context.Background(), env), out.String(), errb.String()
}

// seedInstance runs `gvm create` and then writes the manifest pin the way an
// apply would, so a test that is about apply does not have to hand-roll a
// fixture that could disagree with what create actually writes.
func seedInstance(t *testing.T, h *testHost, name, game string, extra ...string) {
	t.Helper()
	args := append([]string{"create", name, "--game", game}, extra...)
	code, _, errb := runWith(t, h, "", args...)
	if code != 0 {
		t.Fatalf("gvm create: exit %d: %s", code, errb)
	}
}

func mustReadInstance(t *testing.T, h *testHost, name string) *spec.Instance {
	t.Helper()
	raw, err := h.FS.ReadFile(instance.PathsFor(name).Spec)
	if err != nil {
		t.Fatal(err)
	}
	in, diags, err := spec.ParseInstance("test.yaml", raw)
	if err != nil {
		t.Fatal(err)
	}
	if diags.HasErrors() {
		t.Fatalf("the written instance does not parse cleanly:\n%s", diags.Errors())
	}
	return in
}

// observerFor is what `gvm status` builds, exposed for tests that want to
// assert a phase without going through the printer.
func observerFor(h *testHost) *lifecycle.Observer { return h.HostEnv.observer() }

var _ io.Writer = (*bytes.Buffer)(nil)

// isLinux is `runtime.GOOS == "linux"` behind a name, so a test that asserts
// the off-Linux refusal reads as being about the refusal rather than about a
// build constraint.
func isLinux() bool { return runtime.GOOS == "linux" }
