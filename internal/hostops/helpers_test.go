package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// fakeFS is an in-memory FS that records the ORDER of everything it was asked
// to do.
//
// The order is not incidental: chown before chmod is a rule with a reason
// (a chown clears setgid), and a rule whose violation still produces a working
// directory on the developer's machine is a rule that gets broken. The trace
// is how it gets asserted.
type fakeFS struct {
	files map[string]*fakeEntry
	trace []string

	// failChown makes the ownership step fail, so the tests can prove a
	// half-applied directory is reported rather than swallowed.
	failChown bool
	// noStatfs makes DiskUsage refuse the way platform.Statfs does off Linux.
	noStatfs bool
	usage    platform.DiskUsage
}

type fakeEntry struct {
	data  []byte
	mode  fs.FileMode
	dir   bool
	owner string
	group string
}

func newFakeFS(dirs ...string) *fakeFS {
	f := &fakeFS{files: map[string]*fakeEntry{}}
	f.mkdirs(dirs...)
	return f
}

// mkdirs pre-creates directories the way the .deb and systemd-tmpfiles do,
// without going through the code under test.
func (f *fakeFS) mkdirs(dirs ...string) {
	for _, d := range dirs {
		for p := d; p != "/" && p != "." && p != ""; p = path.Dir(p) {
			if _, ok := f.files[p]; !ok {
				f.files[p] = &fakeEntry{dir: true, mode: 0o755}
			}
		}
	}
}

func (f *fakeFS) op(format string, a ...any) { f.trace = append(f.trace, fmt.Sprintf(format, a...)) }

func (f *fakeFS) Stat(p string) (fs.FileInfo, error) {
	e, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: path.Base(p), e: e}, nil
}

func (f *fakeFS) ReadFile(p string) ([]byte, error) {
	e, ok := f.files[p]
	if !ok || e.dir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	return e.data, nil
}

func (f *fakeFS) WriteAtomic(p string, data []byte, perm fs.FileMode, backup bool) error {
	f.op("write %s %04o backup=%v", p, perm.Perm(), backup)
	if prev, ok := f.files[p]; ok && backup {
		f.files[p+spec.BackupSuffix] = &fakeEntry{data: prev.data, mode: prev.mode}
	}
	f.files[p] = &fakeEntry{data: append([]byte(nil), data...), mode: perm}
	return nil
}

func (f *fakeFS) Remove(p string) error {
	f.op("remove %s", p)
	if _, ok := f.files[p]; !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	delete(f.files, p)
	return nil
}

func (f *fakeFS) Mkdir(p string, m fs.FileMode) error {
	f.op("mkdir %s %04o", p, m.Perm())
	if _, ok := f.files[p]; ok {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	f.files[p] = &fakeEntry{dir: true, mode: m}
	return nil
}

func (f *fakeFS) Chmod(p string, m fs.FileMode) error {
	f.op("chmod %s %04o", p, m.Perm())
	e, ok := f.files[p]
	if !ok {
		return &fs.PathError{Op: "chmod", Path: p, Err: fs.ErrNotExist}
	}
	e.mode = m
	return nil
}

func (f *fakeFS) Chown(p, owner, group string) error {
	f.op("chown %s %s:%s", p, owner, group)
	if f.failChown {
		return errors.New("chown refused")
	}
	e, ok := f.files[p]
	if !ok {
		return &fs.PathError{Op: "chown", Path: p, Err: fs.ErrNotExist}
	}
	e.owner, e.group = owner, group
	return nil
}

func (f *fakeFS) DiskUsage(p string) (platform.DiskUsage, error) {
	if f.noStatfs {
		return platform.DiskUsage{}, platform.Unsupported("statfs")
	}
	if _, ok := f.files[p]; !ok {
		return platform.DiskUsage{}, &fs.PathError{Op: "statfs", Path: p, Err: fs.ErrNotExist}
	}
	return f.usage, nil
}

// wrote reports the recorded content and mode of a path.
func (f *fakeFS) wrote(t *testing.T, p string) *fakeEntry {
	t.Helper()
	e, ok := f.files[p]
	if !ok {
		t.Fatalf("nothing at %s; the fake holds:\n\t%s", p, strings.Join(f.paths(), "\n\t"))
	}
	return e
}

func (f *fakeFS) paths() []string {
	out := make([]string, 0, len(f.files))
	for p := range f.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

type fakeInfo struct {
	name string
	e    *fakeEntry
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return int64(len(i.e.data)) }
func (i fakeInfo) Mode() fs.FileMode {
	if i.e.dir {
		return i.e.mode | fs.ModeDir
	}
	return i.e.mode
}
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.e.dir }
func (i fakeInfo) Sys() any           { return nil }

// fakeSystemd records what the manager was asked to do and answers from a
// small table. It is not a systemd simulator: what these tests assert is which
// UNIT hostops derived, which is the half a real systemd could not tell them.
type fakeSystemd struct {
	calls  []string
	states map[string]platform.UnitState
	units  []platform.UnitState
	err    error
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{states: map[string]platform.UnitState{}}
}

func (s *fakeSystemd) record(op, unit string) error {
	s.calls = append(s.calls, op+" "+unit)
	return s.err
}

func (s *fakeSystemd) Start(_ context.Context, u string) error   { return s.record("start", u) }
func (s *fakeSystemd) Stop(_ context.Context, u string) error    { return s.record("stop", u) }
func (s *fakeSystemd) Restart(_ context.Context, u string) error { return s.record("restart", u) }
func (s *fakeSystemd) Enable(_ context.Context, u string) error  { return s.record("enable", u) }
func (s *fakeSystemd) Disable(_ context.Context, u string) error { return s.record("disable", u) }
func (s *fakeSystemd) DaemonReload(context.Context) error        { return s.record("daemon-reload", "") }

func (s *fakeSystemd) State(_ context.Context, u string) (platform.UnitState, error) {
	if err := s.record("state", u); err != nil {
		return platform.UnitState{}, err
	}
	st, ok := s.states[u]
	if !ok {
		return platform.UnitState{Name: u, LoadState: "not-found"}, nil
	}
	return st, nil
}

func (s *fakeSystemd) ListUnits(_ context.Context, pattern string) ([]platform.UnitState, error) {
	if err := s.record("list", pattern); err != nil {
		return nil, err
	}
	return s.units, nil
}

// testOps builds an Ops over the fakes, with an audit sink the caller can read.
func testOps(t *testing.T, f FS, sd platform.Systemd, users platform.Users) (*Ops, *audit.MemorySink) {
	t.Helper()
	sink := audit.NewMemorySink()
	o, err := New(Config{
		Systemd: sd,
		Users:   users,
		FS:      f,
		// The audit system's own complaints go nowhere in tests. They are
		// real signal in production, and here they would be duplicated noise:
		// what a test asserts is the CONTENT of the record, which is a
		// stronger statement than the log line about it.
		Audit:        audit.New(sink).WithLogger(slog.New(slog.DiscardHandler)),
		Log:          slog.New(slog.DiscardHandler),
		LoadInstance: loadFixedInstance("gvm-pz-main"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return o, sink
}

// loadFixedInstance is the LoadInstance seam, answering with one account.
func loadFixedInstance(user string) func(string) (*spec.Instance, spec.Diags, error) {
	return func(string) (*spec.Instance, spec.Diags, error) {
		return &spec.Instance{Spec: spec.InstanceSpec{User: user}}, nil, nil
	}
}

// controlPlaneDirs is what the .deb and systemd-tmpfiles leave behind. Every
// test that writes starts from this and nothing more, which is how a kind that
// silently depends on a directory nobody creates gets caught.
func controlPlaneDirs() []string {
	return []string{
		"/etc/gvm/instances",
		"/etc/gvm/secrets/instances",
		"/etc/systemd/system",
		"/var/lib/gvm/instances",
		"/var/lib/gvm/cache/steam/homes",
		"/var/log/gvm",
		"/var/backups/gvm",
		"/srv/gvm",
	}
}
