package steamcmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory SteamCMD: a Runner that reads the runscript gvm
// generated, prints output shaped like the real thing, and materialises the
// files a real install would leave behind.
//
// It exists so the phases built on this driver -- install, the nightly update
// cycle, rebuild, the jobs layer -- are testable on the Windows development
// machine against a tool that only runs on Linux. Two properties make it worth
// more than a stub:
//
//   - it PARSES the runscript, so a script whose force_install_dir lands after
//     the login is answered with Valve's own warning, which classify.go treats
//     as a hard failure. The ordering rule therefore has a test even though no
//     Steam server is involved.
//
//   - it WRITES a consistent appmanifest, so verify.go's D-32 chain runs for
//     real afterwards, and the knobs on FakeApp (StateFlags, ShortBy, OmitACF,
//     ExtraACF) produce exactly the broken installs that chain exists to catch.
//
// The modes it records are served through TreeFS, because Windows has no
// execute bit and the sentinel's +x rule is one of the five D-32 checks.
type Fake struct {
	mu sync.Mutex

	// Apps is the catalogue Steam pretends to publish.
	Apps map[int]*FakeApp
	// Outcomes are scripted results for app_update runs, consumed in order.
	// An empty queue means "install successfully", which is the common case.
	Outcomes []FakeOutcome
	// Now is the clock the generated appmanifest's LastUpdated comes from.
	Now func() time.Time

	runs  []Invocation
	calls []FakeCall
	modes map[string]fs.FileMode
}

// FakeFile is one file an install materialises.
type FakeFile struct {
	Data []byte
	Mode fs.FileMode
}

// FakeApp is one app in the fake catalogue.
type FakeApp struct {
	Name   string
	Branch string // "" means public
	// BuildID is what app_info_print publishes and what the written
	// appmanifest records.
	BuildID string
	// Bytes is the depot's maxsize, which the disk preflight reads.
	Bytes int64
	// Files are written into force_install_dir, keyed by slash-separated
	// relative path.
	Files map[string]FakeFile

	// The broken-install knobs. Each one produces a tree that a careless
	// installer would call good.
	//
	// StateFlags overrides the healthy 4 (1062 is "fully installed + update
	// required + files missing + update started"). ShortBy makes
	// BytesDownloaded fall that many bytes short of BytesToDownload, which is
	// the "steamcmd said Success and the install is half a gigabyte light"
	// case. StagedShortBy does the same to the staging pair. OmitACF writes no
	// appmanifest at all. ExtraACF writes a second copy at that relative path.
	StateFlags    uint64
	ShortBy       int64
	StagedShortBy int64
	OmitACF       bool
	ExtraACF      string

	// NoSubscription makes app_update fail the way an unowned app does.
	NoSubscription bool
}

// FakeOutcome is one scripted app_update result.
type FakeOutcome struct {
	// Stdout is printed verbatim. Empty means the fake's own success output.
	Stdout string
	Exit   Exit
	// Materialize writes the files anyway, which is how "steamcmd exited 0
	// after Error! 0x602" is reproduced: output that says it failed, a tree
	// that looks plausible, and an exit code that agrees with neither.
	Materialize bool
}

// FakeUpdate is one parsed app_update line.
type FakeUpdate struct {
	AppID       int
	Branch      string
	Validate    bool
	HasPassword bool
}

// FakeCall is a parsed runscript, for assertions.
type FakeCall struct {
	Purpose          string
	Platform         string
	InstallDir       string
	Login            string
	Updates          []FakeUpdate
	AppInfo          []int
	ShutdownOnFailed bool
	NoPrompt         bool
	// Order is the command verbs in file order, so a test can assert that
	// force_install_dir preceded login without re-parsing the script.
	Order []string
	// DirAfterLogin is the ordering bug itself.
	DirAfterLogin bool
}

// NewFake returns a fake with an empty catalogue.
func NewFake() *Fake {
	return &Fake{Apps: map[int]*FakeApp{}, modes: map[string]fs.FileMode{}}
}

// Add puts an app in the catalogue.
func (f *Fake) Add(appid int, app FakeApp) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Apps == nil {
		f.Apps = map[int]*FakeApp{}
	}
	cp := app
	f.Apps[appid] = &cp
	return f
}

// SetBuild publishes a new build for an app, which is what makes the next
// check find an update: the tree on disk keeps the build it was installed
// with, and app_info_print starts answering with this one.
func (f *Fake) SetBuild(appid int, buildID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if app := f.Apps[appid]; app != nil {
		app.BuildID = buildID
	}
}

// Seed writes what an install of appid would have left in installDir, with no
// run. It is how a later phase stands up an instance that is ALREADY
// installed -- the starting state of every nightly -- without pretending the
// install happened in that test.
func (f *Fake) Seed(installDir string, appid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	app := f.Apps[appid]
	if app == nil {
		return fmt.Errorf("steamcmd fake: app %d is not in the catalogue", appid)
	}
	return f.materialize(installDir, appid, app)
}

// Runs returns every invocation the fake was handed.
func (f *Fake) Runs() []Invocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Invocation(nil), f.runs...)
}

// Calls returns every runscript the fake parsed.
func (f *Fake) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, inv Invocation) (Exit, error) {
	if err := ctx.Err(); err != nil {
		return Exit{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs = append(f.runs, inv)
	if !inv.RunAs.Valid() {
		// D-16: the real sandbox refuses this, so the fake does too. A test
		// that forgot the identity would otherwise pass here and fail on the
		// box.
		return Exit{}, fmt.Errorf("steamcmd fake: no run-as identity; third-party code never "+
			"runs as root (D-16): %v", inv.RunAs)
	}
	call, err := ParseRunscript(inv.Script)
	if err != nil {
		return Exit{}, err
	}
	call.Purpose = inv.Purpose
	f.calls = append(f.calls, call)

	out := inv.Stdout
	if out == nil {
		out = discard{}
	}
	write := func(s string) { _, _ = out.Write([]byte(s)) }

	write(fakePreamble(call.Login))
	if call.DirAfterLogin {
		// Valve's own warning, and the one the classifier calls a gvm bug.
		write("Please use force_install_dir before logon!\n")
		return Exit{Code: 1}, nil
	}

	if len(call.AppInfo) > 0 {
		for _, id := range call.AppInfo {
			app := f.Apps[id]
			if app == nil {
				write(fmt.Sprintf("No app info for AppID %d found, requesting...\n", id))
				continue
			}
			write(f.appInfoBlock(id, app))
		}
		write("\n")
		return Exit{}, nil
	}

	var outcome *FakeOutcome
	if len(f.Outcomes) > 0 {
		o := f.Outcomes[0]
		f.Outcomes = f.Outcomes[1:]
		outcome = &o
	}

	for _, up := range call.Updates {
		app := f.Apps[up.AppID]
		if app == nil {
			write(fmt.Sprintf("ERROR! Failed to install app '%d' (No subscription)\n", up.AppID))
			return Exit{Code: 1}, nil
		}
		if app.NoSubscription {
			write(fmt.Sprintf("ERROR! Failed to install app '%d' (No subscription)\n", up.AppID))
			return Exit{Code: 1}, nil
		}
		if outcome != nil {
			write(outcome.Stdout)
			if outcome.Materialize {
				if err := f.materialize(call.InstallDir, up.AppID, app); err != nil {
					return Exit{}, err
				}
			}
			continue
		}
		write(fakeProgress(app.Bytes))
		if err := f.materialize(call.InstallDir, up.AppID, app); err != nil {
			return Exit{}, err
		}
		write(fmt.Sprintf("Success! App '%d' fully installed.\n", up.AppID))
	}
	if outcome != nil {
		return outcome.Exit, nil
	}
	return Exit{}, nil
}

// materialize writes what a real install would leave on disk.
func (f *Fake) materialize(installDir string, appid int, app *FakeApp) error {
	if installDir == "" {
		return errors.New("steamcmd fake: app_update with no force_install_dir")
	}
	for rel, file := range app.Files {
		if err := f.writeFile(installDir, rel, file.Data, file.Mode); err != nil {
			return err
		}
	}
	if app.OmitACF {
		return nil
	}
	acf := f.renderACF(appid, app)
	if err := f.writeFile(installDir, CanonicalACF(appid), []byte(acf), 0o644); err != nil {
		return err
	}
	if app.ExtraACF != "" {
		if err := f.writeFile(installDir, app.ExtraACF, []byte(acf), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) writeFile(root, rel string, data []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	full := filepath.Join(filepath.FromSlash(root), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, data, mode.Perm()); err != nil {
		return err
	}
	if f.modes == nil {
		f.modes = map[string]fs.FileMode{}
	}
	// Recorded because Windows reports 0666 for every regular file, and the
	// sentinel's execute bit is one of the five checks D-32 requires.
	f.modes[filepath.Clean(full)] = mode
	return nil
}

func (f *Fake) renderACF(appid int, app *FakeApp) string {
	state := app.StateFlags
	if state == 0 {
		state = StateInstalled
	}
	size := app.Bytes
	if size <= 0 {
		size = 1 << 20
	}
	now := time.Unix(1762000000, 0).UTC()
	if f.Now != nil {
		now = f.Now()
	}
	var b strings.Builder
	b.WriteString("\"AppState\"\n{\n")
	kv := func(k, v string) { fmt.Fprintf(&b, "\t%q\t\t%q\n", k, v) }
	kv("appid", strconv.Itoa(appid))
	kv("Universe", "1")
	kv("name", app.Name)
	kv("StateFlags", strconv.FormatUint(state, 10))
	kv("installdir", app.Name)
	kv("LastUpdated", strconv.FormatInt(now.Unix(), 10))
	kv("SizeOnDisk", strconv.FormatInt(size, 10))
	kv("buildid", app.BuildID)
	kv("BytesToDownload", strconv.FormatInt(size, 10))
	kv("BytesDownloaded", strconv.FormatInt(size-app.ShortBy, 10))
	kv("BytesToStage", strconv.FormatInt(size, 10))
	kv("BytesStaged", strconv.FormatInt(size-app.StagedShortBy, 10))
	kv("TargetBuildID", app.BuildID)
	b.WriteString("}\n")
	return b.String()
}

func (f *Fake) appInfoBlock(appid int, app *FakeApp) string {
	branch := app.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	size := app.Bytes
	if size <= 0 {
		size = 1 << 20
	}
	var b strings.Builder
	fmt.Fprintf(&b, "AppID : %d, change number : 25881818/0, last change : Tue Sep  9 18:00:00 2025\n",
		appid)
	fmt.Fprintf(&b, "%q\n{\n", strconv.Itoa(appid))
	b.WriteString("\t\"common\"\n\t{\n")
	fmt.Fprintf(&b, "\t\t\"name\"\t\t%q\n", app.Name)
	b.WriteString("\t\t\"type\"\t\t\"Tool\"\n\t}\n")
	b.WriteString("\t\"depots\"\n\t{\n")
	fmt.Fprintf(&b, "\t\t%q\n\t\t{\n", strconv.Itoa(appid+1))
	b.WriteString("\t\t\t\"config\"\n\t\t\t{\n\t\t\t\t\"oslist\"\t\t\"linux\"\n\t\t\t}\n")
	fmt.Fprintf(&b, "\t\t\t\"maxsize\"\t\t%q\n\t\t}\n", strconv.FormatInt(size, 10))
	b.WriteString("\t\t\"branches\"\n\t\t{\n")
	fmt.Fprintf(&b, "\t\t\t%q\n\t\t\t{\n", branch)
	fmt.Fprintf(&b, "\t\t\t\t\"buildid\"\t\t%q\n", app.BuildID)
	b.WriteString("\t\t\t\t\"timeupdated\"\t\t\"1757461234\"\n\t\t\t}\n\t\t}\n\t}\n}\n")
	return b.String()
}

func fakePreamble(login string) string {
	return "Redirecting stderr to '/var/lib/gvm/cache/steam/homes/x/.local/share/Steam/logs/stderr.txt'\n" +
		"Steam Console Client (c) Valve Corporation - version 1757457223\n" +
		"-- type 'quit' to exit --\n" +
		"Loading Steam API...OK\n" +
		"Connecting " + login + " to Steam Public..." + "OK\n" +
		"Waiting for client config...OK\n" +
		"Waiting for user info...OK\n"
}

func fakeProgress(total int64) string {
	if total <= 0 {
		total = 1 << 20
	}
	var b strings.Builder
	b.WriteString(" Update state (0x3) reconfiguring, progress: 0.00 (0 / 0)\n")
	for _, pct := range []int{12, 64, 97} {
		done := total * int64(pct) / 100
		fmt.Fprintf(&b, " Update state (0x61) downloading, progress: %d.00 (%d / %d)\n",
			pct, done, total)
	}
	fmt.Fprintf(&b, " Update state (0x101) committing, progress: 99.00 (%d / %d)\n", total, total)
	return b.String()
}

// TreeFS is the install tree as the driver should read it, with the modes the
// fake declared. Pass it to WithTreeFS.
func (f *Fake) TreeFS(root string) fs.FS {
	f.mu.Lock()
	defer f.mu.Unlock()
	modes := make(map[string]fs.FileMode, len(f.modes))
	for k, v := range f.modes {
		modes[k] = v
	}
	return ModeFS{Base: os.DirFS(root), Root: filepath.Clean(root), Modes: modes}
}

// ModeFS is an fs.FS that reports modes a map says it should.
//
// Windows reports 0666 for every regular file, so os.DirFS cannot answer "is
// the sentinel executable" there -- and that question is one of D-32's five
// checks. Rather than skip the rule on the development machine or weaken it to
// suit the platform, the fake records the mode it wrote and serves it back.
type ModeFS struct {
	Base  fs.FS
	Root  string
	Modes map[string]fs.FileMode
}

func (m ModeFS) Open(name string) (fs.File, error) { return m.Base.Open(name) }

func (m ModeFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(m.Base, name) }

func (m ModeFS) ReadFile(name string) ([]byte, error) { return fs.ReadFile(m.Base, name) }

func (m ModeFS) Stat(name string) (fs.FileInfo, error) {
	fi, err := fs.Stat(m.Base, name)
	if err != nil {
		return nil, err
	}
	mode, ok := m.Modes[filepath.Join(m.Root, filepath.FromSlash(path.Clean(name)))]
	if !ok {
		return fi, nil
	}
	return modeInfo{FileInfo: fi, mode: mode}, nil
}

type modeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (m modeInfo) Mode() fs.FileMode {
	if m.FileInfo.IsDir() {
		return m.mode | fs.ModeDir
	}
	return m.mode
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ParseRunscript reads a generated runscript back into its commands.
//
// The fake uses it to behave like steamcmd, and tests use it to assert what gvm
// asked for without matching strings against a file format. It is deliberately
// strict about the two things that are silently wrong rather than loudly wrong:
// `-validate` (which steamcmd ignores, so a corrupt install looks validated)
// and a force_install_dir that comes after the login.
func ParseRunscript(script []byte) (FakeCall, error) {
	var call FakeCall
	loggedIn := false
	for n, raw := range strings.Split(string(script), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		verb := fields[0]
		call.Order = append(call.Order, verb)
		switch verb {
		case "@ShutdownOnFailedCommand":
			call.ShutdownOnFailed = len(fields) > 1 && fields[1] == "1"
		case "@NoPromptForPassword":
			call.NoPrompt = len(fields) > 1 && fields[1] == "1"
		case "@sSteamCmdForcePlatformType":
			if len(fields) > 1 {
				call.Platform = fields[1]
			}
		case "force_install_dir":
			if len(fields) < 2 {
				return call, fmt.Errorf("steamcmd fake: line %d: force_install_dir with no path", n+1)
			}
			call.InstallDir = fields[1]
			if loggedIn {
				call.DirAfterLogin = true
			}
		case "login":
			if len(fields) < 2 {
				return call, fmt.Errorf("steamcmd fake: line %d: login with no account", n+1)
			}
			call.Login = fields[1]
			loggedIn = true
		case "app_update":
			up, err := parseFakeUpdate(fields, n+1)
			if err != nil {
				return call, err
			}
			call.Updates = append(call.Updates, up)
		case "app_info_print":
			if len(fields) < 2 {
				return call, fmt.Errorf("steamcmd fake: line %d: app_info_print with no app id", n+1)
			}
			id, err := strconv.Atoi(fields[1])
			if err != nil {
				return call, fmt.Errorf("steamcmd fake: line %d: %q is not an app id", n+1, fields[1])
			}
			call.AppInfo = append(call.AppInfo, id)
		case "app_info_request", "app_info_update", "quit":
		default:
			return call, fmt.Errorf("steamcmd fake: line %d: %q is not a command this fake knows",
				n+1, verb)
		}
	}
	if !call.ShutdownOnFailed {
		// Without it a failed app_update falls through to quit and steamcmd can
		// exit 0 -- the trap the whole package is built around. A script
		// missing it is a gvm bug, so the fake refuses rather than papering
		// over it.
		return call, errors.New("steamcmd fake: the script has no @ShutdownOnFailedCommand 1, so " +
			"a failed app_update would fall through to quit and could exit 0")
	}
	return call, nil
}

func parseFakeUpdate(fields []string, line int) (FakeUpdate, error) {
	if len(fields) < 2 {
		return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: app_update with no app id", line)
	}
	id, err := strconv.Atoi(fields[1])
	if err != nil {
		return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: %q is not an app id", line, fields[1])
	}
	up := FakeUpdate{AppID: id}
	for i := 2; i < len(fields); i++ {
		switch fields[i] {
		case "validate":
			up.Validate = true
		case "-validate":
			return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: `-validate` is parsed as an "+
				"unknown option and does NO validation; the keyword is positional", line)
		case "-beta":
			if i+1 >= len(fields) {
				return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: -beta with no branch", line)
			}
			i++
			up.Branch = fields[i]
		case "-betapassword":
			if i+1 >= len(fields) {
				return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: -betapassword with no value", line)
			}
			i++
			up.HasPassword = true
		default:
			return FakeUpdate{}, fmt.Errorf("steamcmd fake: line %d: %q is not an app_update "+
				"argument", line, fields[i])
		}
	}
	return up, nil
}

// SortedAppIDs is the catalogue's app ids, for stable test output.
func (f *Fake) SortedAppIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.Apps))
	for id := range f.Apps {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

var _ Runner = (*Fake)(nil)
