package reconcile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// renderedAt is the fixed clock every golden file is rendered against.
//
// It is the timestamp section 7.4's example carries, so the golden files and
// the spec's own example are readable side by side.
var renderedAt = time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)

// goldenVersion pins the version string in the header. A golden that moved
// every time the build's git describe changed would be a golden nobody keeps.
const goldenVersion = "0.1.0"

// loadManifest reads a manifest from the shipped set, or from this package's
// testdata for the one game gvm does not ship.
func loadManifest(t *testing.T, id string) *spec.Manifest {
	t.Helper()
	var raw []byte
	var ref string
	if b, err := fs.ReadFile(games.FS, id+".yaml"); err == nil {
		raw, ref = b, "games/"+id+".yaml"
	} else {
		p := filepath.Join("testdata", "games", id+".yaml")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("no manifest for %q in the shipped set or in testdata: %v", id, err)
		}
		raw, ref = b, p
	}
	m, diags, err := spec.ValidateBytes(ref, raw)
	if err != nil {
		t.Fatalf("%s: %v\n%s", ref, err, diags)
	}
	if diags.HasErrors() {
		t.Fatalf("%s does not validate:\n%s", ref, diags)
	}
	return m
}

// loadInstance reads one of this package's instance fixtures.
func loadInstance(t *testing.T, name string) (*spec.Instance, []byte) {
	t.Helper()
	p := filepath.Join("testdata", "instances", name+".yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	in, diags, err := spec.LoadInstance(p)
	if err != nil {
		t.Fatalf("%s: %v\n%s", p, err, diags)
	}
	if diags.HasErrors() {
		t.Fatalf("%s does not load cleanly:\n%s", p, diags)
	}
	return in, raw
}

// allSecretsPresent is the HasSecret seam for a box where `gvm create` has
// already generated the credentials, which is every box after the first apply.
func allSecretsPresent(string) bool { return true }

// fakeHost records every hostops request instead of performing one.
//
// It is a recorder rather than a mock with expectations: what these tests
// assert is the SEQUENCE of privileged verbs an apply issues, and a sequence
// is easier to read as a list than as a series of assertions scattered through
// a test.
type fakeHost struct {
	reqs []hostops.Request
	// fail, when set, is the verb that returns an error, so a test can stop an
	// apply at a chosen point and see what did and did not run after it.
	fail hostops.Verb
	err  error
}

func (f *fakeHost) Do(_ context.Context, req hostops.Request) (hostops.Result, error) {
	f.reqs = append(f.reqs, req)
	if f.fail != "" && req.Verb == f.fail {
		if f.err != nil {
			return hostops.Result{Verb: req.Verb}, f.err
		}
		return hostops.Result{Verb: req.Verb}, errors.New("fake host refused " + string(req.Verb))
	}
	return hostops.Result{Verb: req.Verb, Changed: true}, nil
}

// verbs returns the recorded verbs in order, for a readable assertion.
func (f *fakeHost) verbs() []string {
	out := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		v := string(r.Verb)
		switch {
		case r.File != "":
			v += " " + string(r.File)
		case r.Dir != "":
			v += " " + string(r.Dir)
		case r.Unit != hostops.UnitInstance:
			v += " " + string(r.Unit)
		}
		if r.Job != "" {
			v += "/" + r.Job
		}
		out = append(out, v)
	}
	return out
}

// fakeInspect is the planner's read-only view of a host, in memory.
type fakeInspect struct {
	files    fstest.MapFS
	dirs     map[string]bool
	accounts map[string]bool
}

func newInspect() *fakeInspect {
	return &fakeInspect{
		files:    fstest.MapFS{},
		dirs:     map[string]bool{},
		accounts: map[string]bool{},
	}
}

func (f *fakeInspect) put(path string, body []byte) *fakeInspect {
	f.files[key(path)] = &fstest.MapFile{Data: body, Mode: 0o644}
	return f
}

func (f *fakeInspect) mkdir(path string) *fakeInspect { f.dirs[path] = true; return f }

func (f *fakeInspect) account(name string) *fakeInspect { f.accounts[name] = true; return f }

func (f *fakeInspect) Stat(path string) (fs.FileInfo, error) {
	if f.dirs[path] {
		return dirInfo(path), nil
	}
	return f.files.Stat(key(path))
}

func (f *fakeInspect) ReadFile(path string) ([]byte, error) { return f.files.ReadFile(key(path)) }

// ReadDir answers from the same MapFS the writes land in, so a job descriptor
// a plan wrote is a job descriptor the next plan's orphan sweep can see.
func (f *fakeInspect) ReadDir(path string) ([]fs.DirEntry, error) { return f.files.ReadDir(key(path)) }

func (f *fakeInspect) AccountExists(name string) bool { return f.accounts[name] }

// key strips the leading slash: fstest.MapFS keys are io/fs paths, which are
// never rooted.
func key(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

type fakeDirInfo struct{ name string }

func dirInfo(p string) fs.FileInfo { return fakeDirInfo{name: p} }

func (d fakeDirInfo) Name() string       { return d.name }
func (d fakeDirInfo) Size() int64        { return 0 }
func (d fakeDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o750 }
func (d fakeDirInfo) ModTime() time.Time { return renderedAt }
func (d fakeDirInfo) IsDir() bool        { return true }
func (d fakeDirInfo) Sys() any           { return nil }

func rootActor() audit.Actor { return audit.RootCLI() }

// okProbe is a write-probe that says every path is writable.
func okProbe(_ context.Context, p ProbeSpec) ([]ProbeResult, error) {
	out := make([]ProbeResult, 0, len(p.Sandbox.Paths))
	for _, path := range p.Sandbox.Paths {
		out = append(out, ProbeResult{Path: path, OK: true})
	}
	return out, nil
}
