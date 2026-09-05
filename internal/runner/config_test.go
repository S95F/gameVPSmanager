package runner

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestBuildLaunchResolvesArgvAsAList(t *testing.T) {
	l, err := BuildLaunch(testManifest(), testRenderer(), nil)
	if err != nil {
		t.Fatalf("BuildLaunch: %v", err)
	}
	if want := "/opt/pzserver/start-server.sh"; l.Path != want {
		t.Errorf("Path = %q, want %q", l.Path, want)
	}
	if l.Dir != "/opt/pzserver" {
		t.Errorf("Dir = %q", l.Dir)
	}
	// "My Server" stays ONE argv element. A launch that split on whitespace
	// would hand the game "-servername My" and generate a fresh world named
	// "My" beside the real one.
	want := []string{"-servername", "My Server"}
	if !reflect.DeepEqual(l.Args, want) {
		t.Errorf("Args = %q, want %q", l.Args, want)
	}
}

func TestBuildLaunchAppliesWhenGuardsPerPhase(t *testing.T) {
	// PZ passes -adminpassword only on the first boot, because that is when
	// the admin account is created. A guard that leaked into every restart
	// would put the password in every ps listing forever.
	normal, err := BuildLaunch(testManifest(), testRenderer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(normal.Args, "-adminpassword") {
		t.Errorf("a phase:install argument survived a normal start: %q", normal.Args)
	}

	install, err := BuildLaunch(testManifest(), testRenderer().WithPhase(spec.PhaseInstall), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-servername", "My Server", "-adminpassword", "hunter2"}
	if !reflect.DeepEqual(install.Args, want) {
		t.Errorf("install argv = %q, want %q", install.Args, want)
	}
}

func TestBuildLaunchMarksSecretArgsAndDisplayRedactsThem(t *testing.T) {
	l, err := BuildLaunch(testManifest(), testRenderer().WithPhase(spec.PhaseInstall), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.SecretArgs, []int{3}) {
		t.Fatalf("SecretArgs = %v, want [3]", l.SecretArgs)
	}
	// The runner annotates the console with the command it is about to run,
	// and that annotation goes into the ring, the log file and every dashboard
	// attached to it.
	got := l.Display()
	if strings.Contains(got, "hunter2") {
		t.Errorf("Display leaked the secret: %s", got)
	}
	if !strings.Contains(got, spec.Redacted) {
		t.Errorf("Display did not redact: %s", got)
	}
	// The non-secret operand is still readable, and still visibly one element.
	if !strings.Contains(got, `"My Server"`) {
		t.Errorf("Display lost the argument boundary: %s", got)
	}
}

func TestBuildLaunchExpandsAValuesListInPlace(t *testing.T) {
	m := testManifest()
	m.Spec.Runtime.Args = spec.Args{
		{Values: []string{"-password", "{{ .Secret.admin }}"}, When: &spec.When{SecretSet: "admin"}},
		{Value: "-crossplay", When: &spec.When{VarTrue: "crossplay"}},
	}
	r := spec.NewRenderer(spec.RenderData{
		Install: "/opt/game",
		Secret:  spec.SecretValues{"admin": "hunter2"},
		Var:     spec.VarValues{"crossplay": "false"},
	})
	l, err := BuildLaunch(m, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-password", "hunter2"}; !reflect.DeepEqual(l.Args, want) {
		t.Errorf("Args = %q, want %q", l.Args, want)
	}
	// The secret is the SECOND element of the expanded pair; an index computed
	// before expansion would redact the flag and print the password.
	if !reflect.DeepEqual(l.SecretArgs, []int{1}) {
		t.Errorf("SecretArgs = %v, want [1]", l.SecretArgs)
	}
}

func TestBuildLaunchRefusesAnArgumentThatExpandsToNothing(t *testing.T) {
	// A dropped argument does not fail: `-servername` going missing generates
	// a fresh world beside the real one, and the server starts normally. That
	// is why spec's decoder refuses a null argv element and why this refuses
	// the two ways the same thing happens after decoding.
	empty := testManifest()
	empty.Spec.Runtime.Args = spec.Args{{Values: []string{}}}
	if _, err := BuildLaunch(empty, testRenderer(), nil); err == nil {
		t.Error("an empty values list was accepted")
	}

	both := testManifest()
	both.Spec.Runtime.Args = spec.Args{{Value: "-a", Values: []string{"-b"}}}
	if _, err := BuildLaunch(both, testRenderer(), nil); err == nil {
		t.Error("an argument spelled two ways was accepted")
	}
}

func TestResolveExecRefusesWhatWouldBecomeALookup(t *testing.T) {
	cases := []struct {
		name, exec, dir, want string
		wantErr               bool
	}{
		{name: "relative to cwd", exec: "./start-server.sh", dir: "/opt/pzserver", want: "/opt/pzserver/start-server.sh"},
		{name: "already absolute", exec: "/usr/bin/game", dir: "/opt/pzserver", want: "/usr/bin/game"},
		// A bare name is resolved through PATH, so which binary starts the
		// world would depend on an environment variable.
		{name: "bare name", exec: "java", dir: "/opt/pzserver", wantErr: true},
		{name: "relative with no cwd", exec: "./start.sh", wantErr: true},
		{name: "empty", exec: "", dir: "/opt", wantErr: true},
		{name: "nul byte", exec: "/opt/start\x00.sh", dir: "/opt", wantErr: true},
	}
	for _, c := range cases {
		got, err := resolveExec(c.exec, c.dir)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: resolveExec(%q, %q) = %q, want an error", c.name, c.exec, c.dir, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: resolveExec = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildLaunchRefusesARelativeWorkingDirectory(t *testing.T) {
	// A relative cwd means the game's working directory depends on where
	// systemd happened to put the runner, and the world files are written
	// relative to it.
	m := testManifest()
	m.Spec.Runtime.Cwd = "pzserver"
	if _, err := BuildLaunch(m, testRenderer(), nil); err == nil {
		t.Fatal("a relative cwd was accepted")
	}
}

func TestBuildLaunchRefusesAWrapperItCannotHonour(t *testing.T) {
	// Silently dropping `xvfb` would start a headless engine with no X server,
	// and the failure would surface three layers from its cause.
	m := testManifest()
	m.Spec.Runtime.Wrapper = &spec.Wrapper{Kind: "xvfb"}
	if _, err := BuildLaunch(m, testRenderer(), nil); err == nil || !strings.Contains(err.Error(), "wrapper") {
		t.Fatalf("wrapper error = %v", err)
	}
}

func TestBuildLaunchRefusesANulInAnArgument(t *testing.T) {
	// execve truncates at the NUL, so the game would receive a shorter value
	// than the manifest wrote and nothing would report it.
	m := testManifest()
	m.Spec.Runtime.Args = spec.Args{{Value: "{{ .Var.server_name }}"}}
	r := spec.NewRenderer(spec.RenderData{Install: "/opt/game", Var: spec.VarValues{"server_name": "a\x00b"}})
	if _, err := BuildLaunch(m, r, nil); err == nil {
		t.Fatal("a NUL in an argument was accepted")
	}
}

func TestBuildLaunchRefusesAnUnknownConsoleMode(t *testing.T) {
	m := testManifest()
	m.Spec.Runtime.Console.Mode = "tmux"
	if _, err := BuildLaunch(m, testRenderer(), nil); err == nil {
		t.Fatal("console mode tmux was accepted")
	}
}

func TestMergeEnvDropsTheVariablesThatSpeakForTheUnit(t *testing.T) {
	// NOTIFY_SOCKET is the dangerous one: a child that inherits it can send
	// READY=1 for a unit that is still booting, and systemd believes it.
	base := []string{
		"PATH=/usr/bin",
		"NOTIFY_SOCKET=/run/systemd/notify",
		"LISTEN_FDS=1",
		"LISTEN_PID=42",
		"LISTEN_FDNAMES=console",
		"CREDENTIALS_DIRECTORY=/run/credentials/gvm@pz-main.service",
		"INVOCATION_ID=b3f1",
	}
	got := mergeEnv(base, nil, runnerOnlyEnv)
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(runnerOnlyEnv, k) {
			t.Errorf("%s reached the child", k)
		}
	}
	// INVOCATION_ID stays: it is inherited by every unit's children normally,
	// and removing it would make gvm's environment differ from a plain unit's
	// for no security gain.
	if !slices.Contains(got, "INVOCATION_ID=b3f1") || !slices.Contains(got, "PATH=/usr/bin") {
		t.Errorf("env = %q", got)
	}
}

func TestMergeEnvOverridesAndSorts(t *testing.T) {
	// Sorted output means two runs of the same instance produce byte-identical
	// environments, so a diff of two launches shows what changed rather than
	// map iteration order.
	got := mergeEnv(
		[]string{"HOME=/root", "ZZZ=1", "=C:=C:\\", "malformed"},
		map[string]string{"HOME": "/home/pzserver", "AAA": "2"},
		nil,
	)
	want := []string{"AAA=2", "HOME=/home/pzserver", "ZZZ=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %q, want %q", got, want)
	}
}

func TestLineEndingRefusesAnUnknownName(t *testing.T) {
	// A game whose console needs CRLF and silently gets LF accepts nothing an
	// operator types, and the symptom is indistinguishable from the server
	// ignoring the command.
	cases := map[string]string{"": "\n", "lf": "\n", "LF": "\n", "cr": "\r", "crlf": "\r\n"}
	for in, want := range cases {
		got, err := lineEnding(in)
		if err != nil || got != want {
			t.Errorf("lineEnding(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := lineEnding("nl"); err == nil {
		t.Error("lineEnding(nl) was accepted")
	}
}

func TestConsoleConfigTakesTheDocumentedTerminalSize(t *testing.T) {
	// Section 11.4 fixes the pty at 200x50 at spawn, because it is the game's
	// terminal and every viewer shares it.
	cc, err := consoleConfig(spec.ConsoleSpec{Mode: spec.ConsoleModePTY})
	if err != nil {
		t.Fatal(err)
	}
	if cc.Rows != DefaultRows || cc.Cols != DefaultCols {
		t.Errorf("size = %dx%d, want %dx%d", cc.Cols, cc.Rows, DefaultCols, DefaultRows)
	}
	if !cc.Capturing() {
		t.Error("pty mode is not capturing")
	}
	none, err := consoleConfig(spec.ConsoleSpec{Mode: spec.ConsoleModeNone})
	if err != nil {
		t.Fatal(err)
	}
	if none.Capturing() {
		t.Error("mode none reported as capturing")
	}
}
