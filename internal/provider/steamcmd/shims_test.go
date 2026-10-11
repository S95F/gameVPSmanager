package steamcmd

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// memShimFS is an in-memory ShimFS.
//
// It exists because os.Symlink on the Windows development machine needs a
// privilege gvm must not teach anyone to grant, and the three cases that matter
// -- the link is already right, the link points somewhere else, a real file is
// in the way -- are decisions, not syscalls.
type memShimFS struct {
	mu      sync.Mutex
	entries map[string]*memEntry
}

type memEntry struct {
	dir  bool
	link string
	data []byte
	mode fs.FileMode
}

func newMemShimFS() *memShimFS {
	return &memShimFS{entries: map[string]*memEntry{}}
}

func (m *memShimFS) key(p string) string { return path.Clean(strings.ReplaceAll(p, `\`, "/")) }

func (m *memShimFS) MkdirAll(p string, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(p)
	for _, el := range ancestors(k) {
		if e, ok := m.entries[el]; ok {
			if !e.dir && e.link == "" {
				return &fs.PathError{Op: "mkdir", Path: el, Err: fs.ErrExist}
			}
			continue
		}
		m.entries[el] = &memEntry{dir: true, mode: perm | fs.ModeDir}
	}
	return nil
}

func ancestors(k string) []string {
	var out []string
	parts := strings.Split(strings.TrimPrefix(k, "/"), "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		cur += "/" + p
		out = append(out, cur)
	}
	return out
}

func (m *memShimFS) Lstat(p string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[m.key(p)]
	if !ok {
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
	}
	return memShimInfo{name: path.Base(m.key(p)), e: e}, nil
}

func (m *memShimFS) Stat(p string) (fs.FileInfo, error) {
	m.mu.Lock()
	k := m.key(p)
	e, ok := m.entries[k]
	for i := 0; ok && e.link != "" && i < 8; i++ {
		k = m.key(e.link)
		e, ok = m.entries[k]
	}
	m.mu.Unlock()
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return memShimInfo{name: path.Base(k), e: e}, nil
}

func (m *memShimFS) Readlink(p string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[m.key(p)]
	if !ok || e.link == "" {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: fs.ErrInvalid}
	}
	return e.link, nil
}

func (m *memShimFS) Symlink(target, link string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(link)
	if _, ok := m.entries[k]; ok {
		return &fs.PathError{Op: "symlink", Path: link, Err: fs.ErrExist}
	}
	m.entries[k] = &memEntry{link: target, mode: fs.ModeSymlink | 0o777}
	return nil
}

func (m *memShimFS) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(p)
	if _, ok := m.entries[k]; !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	delete(m.entries, k)
	return nil
}

func (m *memShimFS) ReadFile(p string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[m.key(p)]
	if !ok || e.dir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	return e.data, nil
}

func (m *memShimFS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[m.key(p)] = &memEntry{data: data, mode: perm}
	return nil
}

type memShimInfo struct {
	name string
	e    *memEntry
}

func (i memShimInfo) Name() string       { return i.name }
func (i memShimInfo) Size() int64        { return int64(len(i.e.data)) }
func (i memShimInfo) Mode() fs.FileMode  { return i.e.mode }
func (i memShimInfo) ModTime() time.Time { return time.Time{} }
func (i memShimInfo) IsDir() bool        { return i.e.dir }
func (i memShimInfo) Sys() any           { return nil }

var _ ShimFS = (*memShimFS)(nil)

func shimInput() ShimInput {
	return ShimInput{
		Home:        "/srv/gvm/valheim-1",
		Work:        "/var/lib/gvm/cache/steam/homes/valheim-1",
		SteamCMDDir: "/opt/gvm/steamcmd",
		Arch:        "native",
	}
}

func planPaths(t *testing.T, in ShimInput) []string {
	t.Helper()
	shims, err := HomeShims(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range shims {
		if s.Why == "" {
			t.Errorf("%s has no recorded reason, and a symlink nobody explained is one a future "+
				"cleanup deletes", s)
		}
		out = append(out, s.String())
	}
	return out
}

func TestHomeShimsPlan(t *testing.T) {
	got := planPaths(t, shimInput())
	want := []string{
		// The game's HOME: the layout, then the sdk the engine dlopens from.
		"directory /srv/gvm/valheim-1/.local/share/Steam",
		"directory /srv/gvm/valheim-1/.steam",
		"symlink /srv/gvm/valheim-1/.steam/root -> /srv/gvm/valheim-1/.local/share/Steam",
		"symlink /srv/gvm/valheim-1/.steam/steam -> /srv/gvm/valheim-1/.local/share/Steam",
		// SteamCMD's own HOME, which is a different directory per instance so
		// two steamcmd cannot corrupt each other's state (D-30).
		"directory /var/lib/gvm/cache/steam/homes/valheim-1/.local/share/Steam",
		"directory /var/lib/gvm/cache/steam/homes/valheim-1/.steam",
		"symlink /var/lib/gvm/cache/steam/homes/valheim-1/.steam/root -> " +
			"/var/lib/gvm/cache/steam/homes/valheim-1/.local/share/Steam",
		"symlink /var/lib/gvm/cache/steam/homes/valheim-1/.steam/steam -> " +
			"/var/lib/gvm/cache/steam/homes/valheim-1/.local/share/Steam",
		"directory /srv/gvm/valheim-1/.steam/sdk64",
		"symlink /srv/gvm/valheim-1/.steam/sdk64/steamclient.so -> " +
			"/opt/gvm/steamcmd/linux64/steamclient.so",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The sdk link belongs in the GAME's HOME, because that is where the server
// dlopens it from. A copy under SteamCMD's HOME would help nothing.
func TestOnlyTheGameHomeGetsTheSDKLink(t *testing.T) {
	for _, p := range planPaths(t, shimInput()) {
		if strings.Contains(p, "cache/steam/homes") && strings.Contains(p, "sdk") {
			t.Errorf("an sdk shim landed in SteamCMD's HOME: %s", p)
		}
	}
}

func TestSDKBitsFollowsTheEngineArch(t *testing.T) {
	cases := map[string][]int{
		"":       {64},
		"native": {64},
		"i386":   {32},
		"x86":    {32},
		"any":    {64, 32},
	}
	for arch, want := range cases {
		if got := SDKBits(arch); !slices.Equal(got, want) {
			t.Errorf("SDKBits(%q) = %v, want %v", arch, got, want)
		}
	}

	in := shimInput()
	in.Arch = "i386"
	joined := strings.Join(planPaths(t, in), "\n")
	if !strings.Contains(joined, "sdk32/steamclient.so -> /opt/gvm/steamcmd/linux32/steamclient.so") {
		t.Errorf("a 32-bit engine got no sdk32 link:\n%s", joined)
	}
	if strings.Contains(joined, "sdk64") {
		t.Error("a 32-bit engine got an sdk64 link it will never look at")
	}
}

func TestHomeShimsRefusals(t *testing.T) {
	for _, in := range []ShimInput{
		{SteamCMDDir: "/opt/gvm/steamcmd"},
		{Home: "srv/gvm/x", SteamCMDDir: "/opt/gvm/steamcmd"},
	} {
		if _, err := HomeShims(in); !errors.Is(err, ErrShim) {
			t.Errorf("HomeShims(%+v) err = %v, want ErrShim", in, err)
		}
	}
	// No steamcmd directory means no sdk link, and the layout is still worth
	// having: steamcmd writes to both HOMEs regardless.
	in := shimInput()
	in.SteamCMDDir = ""
	for _, p := range planPaths(t, in) {
		if strings.Contains(p, "sdk") {
			t.Errorf("an sdk shim was planned with no steamcmd directory: %s", p)
		}
	}
}

func seededFS(t *testing.T) *memShimFS {
	t.Helper()
	m := newMemShimFS()
	for _, bits := range []string{"32", "64"} {
		if err := m.WriteFile("/opt/gvm/steamcmd/linux"+bits+"/steamclient.so",
			[]byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestApplyShimsIsIdempotent(t *testing.T) {
	m := seededFS(t)
	shims, err := HomeShims(shimInput())
	if err != nil {
		t.Fatal(err)
	}

	first, err := ApplyShims(shims, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Created) != len(shims) {
		t.Fatalf("created %d of %d shims: %v (problems %v)",
			len(first.Created), len(shims), first.Created, first.Problems)
	}
	if len(first.Problems) != 0 {
		t.Errorf("problems = %v", first.Problems)
	}

	second, err := ApplyShims(shims, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Created) != 0 {
		t.Errorf("a second apply created %v", second.Created)
	}
	if len(second.Already) != len(shims) {
		t.Errorf("a second apply reported %d of %d already correct", len(second.Already), len(shims))
	}

	if target, err := m.Readlink("/srv/gvm/valheim-1/.steam/sdk64/steamclient.so"); err != nil ||
		target != "/opt/gvm/steamcmd/linux64/steamclient.so" {
		t.Errorf("the sdk link points at %q (%v)", target, err)
	}
}

// steamcmd moved, or the link is from an older layout. A link gvm owns that
// points at a path which no longer exists is the same failure as no link.
func TestApplyShimsRepointsAStaleLink(t *testing.T) {
	m := seededFS(t)
	if err := m.MkdirAll("/srv/gvm/valheim-1/.steam/sdk64", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Symlink("/opt/steamcmd-old/linux64/steamclient.so",
		"/srv/gvm/valheim-1/.steam/sdk64/steamclient.so"); err != nil {
		t.Fatal(err)
	}

	shims, _ := HomeShims(shimInput())
	res, err := ApplyShims(shims, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Errorf("problems = %v", res.Problems)
	}
	target, err := m.Readlink("/srv/gvm/valheim-1/.steam/sdk64/steamclient.so")
	if err != nil || target != "/opt/gvm/steamcmd/linux64/steamclient.so" {
		t.Errorf("the stale link survived: %q (%v)", target, err)
	}
}

// A real steamclient.so an operator copied in by hand works perfectly, and
// replacing a file gvm did not create is not this code's call.
func TestApplyShimsWillNotReplaceARealFile(t *testing.T) {
	m := seededFS(t)
	if err := m.MkdirAll("/srv/gvm/valheim-1/.steam/sdk64", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("/srv/gvm/valheim-1/.steam/sdk64/steamclient.so",
		[]byte("a real copy"), 0o755); err != nil {
		t.Fatal(err)
	}

	shims, _ := HomeShims(shimInput())
	res, err := ApplyShims(shims, m)
	if err != nil {
		t.Fatalf("a shim gvm declines to force is a problem, not an error: %v", err)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "will not replace") {
		t.Fatalf("problems = %v", res.Problems)
	}
	if got, _ := m.ReadFile("/srv/gvm/valheim-1/.steam/sdk64/steamclient.so"); string(got) != "a real copy" {
		t.Errorf("the operator's file was overwritten: %q", got)
	}
}

// A link to a target that is not there produces exactly the dlopen failure the
// shim exists to fix, so it is worse than no link: the real problem (steamcmd
// is not where gvm thinks) would be one directory away and invisible.
func TestApplyShimsReportsAMissingTarget(t *testing.T) {
	m := newMemShimFS() // no steamclient.so anywhere
	shims, _ := HomeShims(shimInput())
	res, err := ApplyShims(shims, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "steamclient.so is not there") {
		t.Fatalf("problems = %v", res.Problems)
	}
	if _, err := m.Lstat("/srv/gvm/valheim-1/.steam/sdk64/steamclient.so"); err == nil {
		t.Error("a dangling link was created")
	}
}

func TestApplyShimsRefusesARelativePath(t *testing.T) {
	_, err := ApplyShims([]Shim{{Kind: ShimDir, Path: "relative/dir"}}, newMemShimFS())
	if !errors.Is(err, ErrShim) {
		t.Fatalf("err = %v, want ErrShim", err)
	}
}

func TestAppIDFileShim(t *testing.T) {
	in := shimInput()
	in.RuntimeAppID = 892970
	in.AppIDFileRel = "steam_appid.txt"

	rel, content, ok := AppIDShim(in)
	if !ok || rel != "steam_appid.txt" || string(content) != "892970\n" {
		t.Fatalf("AppIDShim = %q, %q, %t", rel, content, ok)
	}

	// No runtime appid means no shim: the install appid is what the server
	// announces, and a steam_appid.txt saying so would be noise.
	in.RuntimeAppID = 0
	if _, _, ok := AppIDShim(in); ok {
		t.Error("a shim was planned with no runtime appid")
	}

	// And the file shim applies idempotently.
	m := newMemShimFS()
	shim := Shim{Kind: ShimFile, Path: "/opt/valheim/steam_appid.txt",
		Content: []byte("892970\n"), Why: "the server announces the game appid"}
	first, err := ApplyShims([]Shim{shim}, m)
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("first apply: %v %+v", err, first)
	}
	second, err := ApplyShims([]Shim{shim}, m)
	if err != nil || len(second.Already) != 1 {
		t.Fatalf("second apply: %v %+v", err, second)
	}
}

func TestRuntimeEnvAndSteamCMDEnv(t *testing.T) {
	in := shimInput()
	in.RuntimeAppID = 892970

	game := RuntimeEnv(in)
	if game["HOME"] != in.Home {
		t.Errorf("HOME = %q", game["HOME"])
	}
	if game["SteamAppId"] != "892970" {
		t.Errorf("SteamAppId = %q; Valheim does not start without it", game["SteamAppId"])
	}

	// SteamCMD's HOME is the per-instance one, which is the whole of D-30: one
	// variable moves ~/.steam and ~/.local/share/Steam together.
	steam := SteamCMDEnv(in)
	if steam["HOME"] != in.Work {
		t.Errorf("steamcmd HOME = %q, want the per-instance work dir %q", steam["HOME"], in.Work)
	}
	if steam["LC_ALL"] != "C.UTF-8" || steam["LANG"] != "C.UTF-8" {
		t.Errorf("steamcmd env = %v; steamcmd misbehaves without a UTF-8 locale", steam)
	}

	// A game with no steamcmd HOME falls back to its own, rather than
	// inheriting whatever HOME the sandbox had.
	in.Work = ""
	if got := SteamCMDEnv(in)["HOME"]; got != in.Home {
		t.Errorf("fallback HOME = %q", got)
	}
}

func TestRuntimeAppIDReadsTheManifest(t *testing.T) {
	m := &spec.Manifest{}
	if _, ok := RuntimeAppID(m); ok {
		t.Error("a manifest with no runtime env reported a runtime appid")
	}

	m.Spec.Runtime.Env = map[string]string{"SteamAppId": "892970"}
	if id, ok := RuntimeAppID(m); !ok || id != 892970 {
		t.Errorf("RuntimeAppID = %d, %t", id, ok)
	}

	// A templated value belongs to the renderer, not to this: reporting it as
	// absent is honest, guessing at it is not.
	m.Spec.Runtime.Env["SteamAppId"] = "{{ .Var.appid }}"
	if _, ok := RuntimeAppID(m); ok {
		t.Error("a template was read as an appid")
	}
	if _, ok := RuntimeAppID(nil); ok {
		t.Error("a nil manifest reported an appid")
	}
}
