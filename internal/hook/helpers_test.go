package hook

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// env is one instance's worth of real directories plus a Ctx pointing at them.
//
// Real directories and not a fake filesystem: everything these hooks do is file
// arithmetic -- which path, which mode, whether the target already exists --
// and a fake would be asserting that the fake agrees with itself. The two
// things a Windows box cannot do are chown and symlink, and both are behind
// their own seam (WithChown here, steamcmd.ShimFS for links) so the logic they
// guard is still exercised.
type env struct {
	t    *testing.T
	root string
	// chowns records every ownership change the FS attempted, so "the file the
	// server has to rewrite ends up owned by the server" is assertable without
	// root.
	chowns []string
	ctx    *Ctx
	active bool
}

func newEnv(t *testing.T, manifestFile string, vars spec.VarValues) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root}

	dirs := map[string]string{}
	for _, name := range []string{"install", "data", "home", "work"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		dirs[name] = p
	}

	paths, err := provider.NewPaths(spec.Paths{
		Install: dirs["install"], Data: dirs["data"],
		Home: dirs["home"], Work: dirs["work"],
	})
	if err != nil {
		t.Fatalf("provider.NewPaths: %v", err)
	}

	owner, err := provider.NewUnixIdent("gvm-test", "gvm-test", 998, 998)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewFS(paths, owner, WithChown(func(p string, uid, gid int) error {
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = p
		}
		e.chowns = append(e.chowns, fmt.Sprintf("%s %d:%d", filepath.ToSlash(rel), uid, gid))
		return nil
	}))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}

	m := loadManifest(t, manifestFile)
	if vars == nil {
		vars = spec.VarValues{}
	}
	render := spec.NewRenderer(spec.RenderData{
		Name:    "test-1",
		Game:    m.Metadata.ID,
		Install: dirs["install"], Data: dirs["data"],
		Home: dirs["home"], Work: dirs["work"],
		User: "gvm-test", Group: "gvm-test",
		Var:    vars,
		Port:   spec.PortValues{},
		Secret: spec.SecretValues{},
	})

	e.ctx = &Ctx{
		Name:        "test-1",
		Manifest:    m,
		Vars:        vars,
		Ports:       spec.PortValues{},
		Paths:       paths,
		Render:      render,
		Fs:          fsys,
		Links:       newFakeShimFS(),
		SteamCMDDir: "/opt/gvm/steamcmd",
	}
	cfg, err := NewConfigAccess(ConfigAccess{
		Manifest: m, Render: render, Fs: fsys,
		Active: func() (bool, error) { return e.active, nil },
	})
	if err != nil {
		t.Fatalf("NewConfigAccess: %v", err)
	}
	e.ctx.Config = cfg
	return e
}

func loadManifest(t *testing.T, file string) *spec.Manifest {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	m, diags, err := spec.ValidateBytes(file, raw)
	if err != nil || diags.HasErrors() {
		t.Fatalf("%s does not validate: %v\n%s", file, err, diags)
	}
	return m
}

// write puts a file into one of the instance's trees, creating parents.
func (e *env) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// mkdir creates a directory with no file in it, which is the one state write
// cannot produce and several of the resolver's guards turn on.
func (e *env) mkdir(rel string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.root, filepath.FromSlash(rel)), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// read returns a file's contents, or fails.
func (e *env) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(rel)))
	return err == nil
}

// run executes one hook by name in the phase it declares.
func (e *env) run(name string) error {
	e.t.Helper()
	h, err := Get(name)
	if err != nil {
		e.t.Fatal(err)
	}
	e.ctx.Phase = h.Phases()[0]
	e.ctx.notes, e.ctx.problems, e.ctx.boot = nil, nil, nil
	return h.Run(e.t.Context(), e.ctx)
}

func (e *env) notes() []string    { return e.ctx.notes }
func (e *env) problems() []string { return e.ctx.problems }

// fakeShimFS is an in-memory filesystem for the symlink half.
//
// os.Symlink on Windows needs a privilege gvm must not teach anyone to grant,
// so the three cases that matter -- already correct, pointing somewhere else,
// and occupied by a real file -- are exercised here instead.
type fakeShimFS struct {
	dirs  map[string]bool
	links map[string]string
	files map[string][]byte
}

func newFakeShimFS() *fakeShimFS {
	return &fakeShimFS{
		dirs:  map[string]bool{},
		links: map[string]string{},
		files: map[string][]byte{},
	}
}

func (f *fakeShimFS) MkdirAll(p string, _ fs.FileMode) error {
	f.dirs[p] = true
	return nil
}

func (f *fakeShimFS) Lstat(p string) (fs.FileInfo, error) {
	switch {
	case f.links[p] != "":
		return fakeInfo{name: p, mode: fs.ModeSymlink}, nil
	case f.dirs[p]:
		return fakeInfo{name: p, mode: fs.ModeDir}, nil
	case f.files[p] != nil:
		return fakeInfo{name: p}, nil
	}
	return nil, fs.ErrNotExist
}

// Stat follows a link, which is what makes "the target is not there" a case the
// apply path can report rather than a dangling link it creates anyway.
func (f *fakeShimFS) Stat(p string) (fs.FileInfo, error) {
	if t := f.links[p]; t != "" {
		return f.Stat(t)
	}
	return f.Lstat(p)
}

func (f *fakeShimFS) Readlink(p string) (string, error) {
	if t := f.links[p]; t != "" {
		return t, nil
	}
	return "", fs.ErrInvalid
}

func (f *fakeShimFS) Symlink(target, link string) error {
	f.links[link] = target
	return nil
}

func (f *fakeShimFS) Remove(p string) error {
	delete(f.links, p)
	delete(f.dirs, p)
	delete(f.files, p)
	return nil
}

func (f *fakeShimFS) ReadFile(p string) ([]byte, error) {
	if b, ok := f.files[p]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeShimFS) WriteFile(p string, data []byte, _ fs.FileMode) error {
	f.files[p] = append([]byte(nil), data...)
	return nil
}

type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }
