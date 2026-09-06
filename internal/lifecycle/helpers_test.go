package lifecycle

import (
	"context"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// fakeFS is an in-memory Linux filesystem.
//
// Every path in section 5 is absolute and slash-separated, and development
// happens on Windows, so a test that used t.TempDir() would be testing
// C:\Users\...\etc\gvm and proving nothing about /etc/gvm. This holds the
// literal paths.
type fakeFS struct {
	files map[string]fakeFile
	dirs  map[string]bool
	// errs forces a read error on one path, for the "could not read" half of
	// every rule that has one.
	errs map[string]error
}

type fakeFile struct {
	data []byte
	mode fs.FileMode
	mod  time.Time
}

func newFS() *fakeFS {
	return &fakeFS{files: map[string]fakeFile{}, dirs: map[string]bool{}, errs: map[string]error{}}
}

func (f *fakeFS) put(p, body string) *fakeFS { return f.putAt(p, body, time.Time{}) }

func (f *fakeFS) putAt(p, body string, mod time.Time) *fakeFS {
	f.files[p] = fakeFile{data: []byte(body), mode: 0o644, mod: mod}
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		f.dirs[d] = true
	}
	return f
}

func (f *fakeFS) fail(p string, err error) *fakeFS { f.errs[p] = err; return f }

func (f *fakeFS) Stat(name string) (fs.FileInfo, error) {
	if err := f.errs[name]; err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	if ff, ok := f.files[name]; ok {
		return fakeInfo{name: path.Base(name), size: int64(len(ff.data)), mode: ff.mode, mod: ff.mod}, nil
	}
	if f.dirs[name] {
		return fakeInfo{name: path.Base(name), mode: fs.ModeDir | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	if err := f.errs[name]; err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if ff, ok := f.files[name]; ok {
		return append([]byte(nil), ff.data...), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := f.errs[name]; err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	seen := map[string]fs.DirEntry{}
	prefix := strings.TrimSuffix(name, "/") + "/"
	for p, ff := range f.files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			seen[rest[:i]] = fakeInfo{name: rest[:i], mode: fs.ModeDir | 0o755}
			continue
		}
		seen[rest] = fakeInfo{name: rest, size: int64(len(ff.data)), mode: ff.mode, mod: ff.mod}
	}
	if len(seen) == 0 && !f.dirs[strings.TrimSuffix(name, "/")] {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type fakeInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (i fakeInfo) Name() string               { return i.name }
func (i fakeInfo) Size() int64                { return i.size }
func (i fakeInfo) Mode() fs.FileMode          { return i.mode }
func (i fakeInfo) ModTime() time.Time         { return i.mod }
func (i fakeInfo) IsDir() bool                { return i.mode.IsDir() }
func (i fakeInfo) Sys() any                   { return nil }
func (i fakeInfo) Type() fs.FileMode          { return i.mode.Type() }
func (i fakeInfo) Info() (fs.FileInfo, error) { return i, nil }

// fakeSystemd implements the WHOLE of platform.Systemd, including the five
// mutating methods, and counts every call.
//
// That completeness is the point of it. A fake with only State and ListUnits
// would make TestReconcilerNeverActuates pass because there was nothing to
// call; this one could be started, stopped, restarted, enabled, disabled and
// reloaded, and the assertion is that after a full sweep over a host with
// every kind of drift on it, none of those happened.
type fakeSystemd struct {
	mu    sync.Mutex
	units map[string]platform.UnitState

	Starts   []string
	Stops    []string
	Restarts []string
	Enables  []string
	Disables []string
	Reloads  int
	States   int
	Lists    int
}

func newSystemd() *fakeSystemd {
	return &fakeSystemd{units: map[string]platform.UnitState{}}
}

func (f *fakeSystemd) set(unit string, st platform.UnitState) *fakeSystemd {
	st.Name = unit
	f.units[unit] = st
	return f
}

// Actuations is every call that changed the host, which is the number
// TestReconcilerNeverActuates asserts is zero.
func (f *fakeSystemd) Actuations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Starts) + len(f.Stops) + len(f.Restarts) + len(f.Enables) + len(f.Disables) + f.Reloads
}

func (f *fakeSystemd) State(_ context.Context, unit string) (platform.UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.States++
	if st, ok := f.units[unit]; ok {
		return st, nil
	}
	return platform.UnitState{Name: unit, LoadState: "not-found", ActiveState: "inactive"}, nil
}

func (f *fakeSystemd) ListUnits(_ context.Context, _ string) ([]platform.UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Lists++
	out := make([]platform.UnitState, 0, len(f.units))
	for _, u := range f.units {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeSystemd) Start(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Starts = append(f.Starts, u)
	return nil
}

func (f *fakeSystemd) Stop(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stops = append(f.Stops, u)
	return nil
}

func (f *fakeSystemd) Restart(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Restarts = append(f.Restarts, u)
	return nil
}

func (f *fakeSystemd) Enable(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Enables = append(f.Enables, u)
	return nil
}

func (f *fakeSystemd) Disable(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Disables = append(f.Disables, u)
	return nil
}

func (f *fakeSystemd) DaemonReload(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reloads++
	return nil
}

// fakeHost records hostops requests. It is the manager's privileged surface.
type fakeHost struct {
	reqs []hostops.Request
	// fail returns an error for the first request with this verb.
	fail    hostops.Verb
	failErr error
	// onStop mutates the fake filesystem the way systemd would: a clean stop
	// takes the RuntimeDirectory with it, hold file included.
	onStop func()
}

func (h *fakeHost) Do(_ context.Context, req hostops.Request) (hostops.Result, error) {
	h.reqs = append(h.reqs, req)
	if h.fail != "" && req.Verb == h.fail {
		return hostops.Result{Verb: req.Verb}, h.failErr
	}
	if req.Verb == hostops.VerbUnitStop && h.onStop != nil {
		h.onStop()
	}
	res := hostops.Result{Verb: req.Verb, Changed: true}
	if req.File != "" {
		p, err := hostops.FilePath(req.File, req.Instance, req.Job, req.LegacyUnit)
		if err != nil {
			return hostops.Result{Verb: req.Verb}, err
		}
		res.Path = p
	}
	return res, nil
}

func (h *fakeHost) verbs() []hostops.Verb {
	out := make([]hostops.Verb, 0, len(h.reqs))
	for _, r := range h.reqs {
		out = append(out, r.Verb)
	}
	return out
}

func (h *fakeHost) find(v hostops.Verb) (hostops.Request, bool) {
	for _, r := range h.reqs {
		if r.Verb == v {
			return r, true
		}
	}
	return hostops.Request{}, false
}

// aliveFunc builds an Observer.Alive from a set of live pids.
func aliveFunc(live ...int) func(int) Tri {
	set := map[int]bool{}
	for _, p := range live {
		set[p] = true
	}
	return func(pid int) Tri { return TriOf(set[pid]) }
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
