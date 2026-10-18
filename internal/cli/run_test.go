package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/control/stdinctl"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/runner"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The load half of `gvm run` is a pure function of a path set and an
// environment, which is what lets it be asserted here against a temporary
// directory on a machine with no /etc/gvm, no systemd credentials and no pty.

// adoptedPZ is the section 6.1 instance: an adopted Project Zomboid server, the
// one migration this project is not allowed to get wrong. The manifest pin
// deliberately does NOT match the shipped manifest — verifying it is preflight's
// job, and loadRunSpec must not do it, or a mismatch would be reported with the
// wrong exit code and systemd would retry it forever.
const adoptedPZ = `
apiVersion: gvm/v1
kind: Instance
metadata:
  name: pz-main
  uid: 019f2c7a-8b31-7c4e-9d20-3f61a5b7c9e2
  created: 2026-09-04T18:22:11Z
spec:
  game: projectzomboid
  manifest:
    revision: 7
    sha256: 1b40c9de4f7a2c8e5d3b1a09f6e4c2b8d7a5930e1c4f6b8a2d0e3c5f7a9b1d3e
    origin: shipped
  enabled: true
  managed_install: false
  user: pzserver
  paths:
    install: /opt/pzserver
    data: /home/pzserver/Zomboid
    home: /home/pzserver
    work: /var/lib/gvm/cache/steam/homes/pz-main
  ports:
    game: 16261
    direct: 16262
    rcon: 27015
  vars:
    server_name: servertest
    jvm_xmx: 5g
    jvm_xms: 5g
    max_players: "8"
  resources:
    cpu_weight: 100
    memory_high: 7G
    memory_max: 8G
  backup:
    schedule: "0 4 * * *"
`

// testPaths builds a path set rooted in a temp dir, so the loader's real
// filenames are exercised without touching /etc.
func testPaths(t *testing.T, name string) instance.Paths {
	t.Helper()
	root := t.TempDir()
	p := instance.Paths{
		Name:         name,
		Spec:         filepath.Join(root, name+".yaml"),
		SpecDir:      filepath.Join(root, name+".d"),
		ManifestLock: filepath.Join(root, name+".d", "manifest.lock.yaml"),
		Run:          filepath.Join(root, "run"),
		BlockedMods:  filepath.Join(root, "blocked.json"),
	}
	if err := os.MkdirAll(p.SpecDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return p
}

// writeInstance lays down an instance file and a pinned manifest, optionally
// with one substitution applied to the manifest bytes.
func writeInstance(t *testing.T, p instance.Paths, instYAML, old, new string) {
	t.Helper()
	if err := os.WriteFile(p.Spec, []byte(instYAML), 0o600); err != nil {
		t.Fatalf("write instance: %v", err)
	}
	raw, err := fs.ReadFile(games.FS, "projectzomboid.yaml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if old != "" {
		edited := strings.Replace(string(raw), old, new, 1)
		if edited == string(raw) {
			t.Fatalf("projectzomboid.yaml no longer contains %q", old)
		}
		raw = []byte(edited)
	}
	if err := os.WriteFile(p.ManifestLock, raw, 0o600); err != nil {
		t.Fatalf("write pin: %v", err)
	}
}

func noEnv(string) string { return "" }

// The credential ids come from the manifest's OWN `.Secret.<name>` references,
// so a secret added to a manifest is loaded without anybody remembering to add
// it to a list. Project Zomboid names three, in two different spellings: the
// template says rcon_password, the credential is `rcon`.
func TestLoadRunSpecDerivesCredentialIDsFromTheManifest(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")

	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	want := []string{"admin", "join", "rcon"}
	if strings.Join(ld.CredIDs, ",") != strings.Join(want, ",") {
		t.Errorf("credential ids = %v, want %v", ld.CredIDs, want)
	}
}

// The values reach the renderer, which is what puts RCONPassword= into the
// game's own ini and the password into the RCON dial.
func TestLoadRunSpecReadsCredentialsAndBindsThemToTheRenderer(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")

	credDir := t.TempDir()
	// A trailing newline is what an editor and `systemd-creds` both leave, and
	// a password with one appended fails authentication with no error anybody
	// can read.
	if err := os.WriteFile(filepath.Join(credDir, "rcon"), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	ld, err := loadRunSpec("pz-main", p, func(k string) string {
		if k == "CREDENTIALS_DIRECTORY" {
			return credDir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	got, taint, err := ld.Renderer.Render("spec.control.password", "{{ .Secret.rcon_password }}")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("rendered password = %q", got)
	}
	if taint == 0 {
		t.Error("a rendered secret is not marked tainted; Launch.Display would print it")
	}
}

// A missing credential is preflight's finding, not the loader's. Failing here
// would report one of three missing secrets per restart, and with the wrong
// exit code.
func TestLoadRunSpecLeavesAMissingCredentialToPreflight(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")

	ld, err := loadRunSpec("pz-main", p, func(string) string { return t.TempDir() })
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	if len(ld.CredIDs) != 3 {
		t.Errorf("the ids were not still reported: %v", ld.CredIDs)
	}
}

// `.Host.RamMB` is an apply-time fact. The runner does not have it, so a
// manifest that reads one at a runtime surface must be refused rather than
// rendered — a silent zero here is a JVM heap of nothing, in argv.
func TestLoadRunSpecRefusesAHostFactItCannotSupply(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, `"-Xmx{{ .Var.jvm_xmx }}"`, `"-Xmx{{ .Host.RamMB }}m"`)

	_, err := loadRunSpec("pz-main", p, noEnv)
	if err == nil {
		t.Fatal("accepted a manifest the runner cannot render")
	}
	if !strings.Contains(err.Error(), ".Host.RamMB") {
		t.Errorf("the refusal does not name the reference: %v", err)
	}
}

// `.Host.CPUs` is answerable here and is answered, so the refusal above is
// about the two facts the runner genuinely lacks and not about `.Host` as a
// whole.
func TestLoadRunSpecAnswersHostCPUs(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, `"-Xmx{{ .Var.jvm_xmx }}"`, `"-threads{{ .Host.CPUs }}"`)

	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	got, _, err := ld.Renderer.Render("spec.runtime.args[0]", "{{ .Host.CPUs }}")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got == "0" || got == "" {
		t.Errorf("CPUs rendered as %q", got)
	}
}

func TestLoadRunSpecReportsAMissingInstanceFile(t *testing.T) {
	p := testPaths(t, "pz-main")
	if _, err := loadRunSpec("pz-main", p, noEnv); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

func TestLoadRunSpecReportsAMissingPin(t *testing.T) {
	p := testPaths(t, "pz-main")
	if err := os.WriteFile(p.Spec, []byte(adoptedPZ), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadRunSpec("pz-main", p, noEnv)
	if !errors.Is(err, spec.ErrNoPin) {
		t.Errorf("err = %v, want spec.ErrNoPin", err)
	}
}

// group defaults to user. An empty Group would render an empty `chown :`
// anywhere a manifest interpolates it.
func TestRenderDataDefaultsGroupToUser(t *testing.T) {
	in := &spec.Instance{}
	in.Spec.User = "pzserver"
	d := renderData("pz-main", in, spec.SecretValues{})
	if d.Group != "pzserver" {
		t.Errorf("group = %q, want the user", d.Group)
	}
}

// A readiness probe runs on the same box as the game. Dialling the public
// address would test the firewall and the NAT instead of the server, and a
// correctly running instance would never reach ready.
func TestPortAddrPrefersLoopback(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	addr := portAddr(ld)

	got, err := addr("game")
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	if got != "127.0.0.1:16261" {
		t.Errorf("game address = %q", got)
	}
	if _, err := addr("nosuchport"); err == nil {
		t.Error("an unallocated port name was accepted")
	}
}

// An unbuilt exit code is a restart policy chosen by accident: 64 must not be
// retried, 78 is the ProtectHome mistake, and anything that is not a preflight
// refusal at all is a runner bug.
func TestNewFailureChoosesTheExitCode(t *testing.T) {
	t.Run("a path error is a sandbox problem", func(t *testing.T) {
		err := newFailure(&fs.PathError{Op: "open", Path: "/var/log/gvm/pz-main", Err: os.ErrPermission})
		if got := runner.ExitCode(err); got != runner.ExitConfig {
			t.Errorf("exit = %d, want %d", got, runner.ExitConfig)
		}
		if !strings.Contains(runner.StatusOf(err), "/var/log/gvm/pz-main") {
			t.Errorf("STATUS= does not name the path: %q", runner.StatusOf(err))
		}
	})
	t.Run("anything else will fail the same way next time", func(t *testing.T) {
		err := newFailure(errors.New("the shutdown ladder names an unknown signal"))
		if got := runner.ExitCode(err); got != runner.ExitUsage {
			t.Errorf("exit = %d, want %d", got, runner.ExitUsage)
		}
	})
	t.Run("a preflight failure keeps its own code", func(t *testing.T) {
		orig := &runner.Failure{Check: "orphan", Code: runner.ExitTempFail, Status: "still alive"}
		if got := runner.ExitCode(newFailure(orig)); got != runner.ExitTempFail {
			t.Errorf("exit = %d, want %d", got, runner.ExitTempFail)
		}
	})
}

// `systemctl status` and a wrapper script must agree about what a SIGKILLed
// server looks like, and 128+signal is the convention both already read.
func TestExitCodeOfUsesTheShellConventionForASignal(t *testing.T) {
	if got := exitCodeOf(runner.ExitStatus{Code: 3}); got != 3 {
		t.Errorf("exit = %d, want 3", got)
	}
	if got := exitCodeOf(runner.ExitStatus{Signaled: true, Signal: platform.SIGKILL}); got != 137 {
		t.Errorf("signalled exit = %d, want 137", got)
	}
}

// The socket needs an error CODE, not a sentence: gvm-web branches between "fix
// your input" and "the runner is broken", and an unsafe argument is the first.
func TestBackendVerbMapsARefusalToBadRequest(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	consoleCh, err := stdinctl.New(stdinctl.Options{
		Sink: control.SenderFunc(func(string, string) (uint64, error) { return 1, nil }),
	})
	if err != nil {
		t.Fatalf("stdinctl: %v", err)
	}
	cmd, err := control.New(control.Options{
		Manifest: ld.Manifest,
		Instance: ld.Instance,
		Renderer: ld.Renderer,
		Console:  consoleCh,
	})
	if err != nil {
		t.Fatalf("control.New: %v", err)
	}
	b := consoleBackend{Runner: newTestRunner(t, ld), cmd: cmd}

	_, err = b.Verb(context.Background(), "root_cli", "broadcast",
		map[string]string{"Msg": "restart\nquit"})
	if err == nil {
		t.Fatal("a newline in a broadcast reached the game console")
	}
	if !errors.Is(err, socket.ErrBadArgument) {
		t.Errorf("err = %v, want it to wrap socket.ErrBadArgument", err)
	}
	// The rejected value must not travel back out: echoing a CR reproduces the
	// injection one line lower in whatever renders the message.
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal carries the rejected bytes: %q", err)
	}
}

func TestParseRunArgs(t *testing.T) {
	env := &Env{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}

	opt, err := parseRunArgs(env, []string{"pz-main"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opt.Name != "pz-main" || opt.GvmPath != runner.DefaultGvmPath {
		t.Errorf("opt = %+v", opt)
	}

	// The operator types the instance first; the flag has to work after it.
	opt, err = parseRunArgs(env, []string{"pz-main", "--gvm-path", "/opt/gvm/bin/gvm"})
	if err != nil {
		t.Fatalf("parse with a trailing flag: %v", err)
	}
	if opt.GvmPath != "/opt/gvm/bin/gvm" {
		t.Errorf("gvm-path = %q", opt.GvmPath)
	}

	for _, args := range [][]string{nil, {"a-name", "b-name"}, {"../etc"}} {
		if _, err := parseRunArgs(env, args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

// newTestRunner builds a runner far enough to own a ring. It never spawns
// anything: the ring, the launch and the ladder are all resolved by New, which
// is everything the recorder below touches.
func newTestRunner(t *testing.T, ld *runSpec) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{
		Instance: "pz-main",
		Game:     ld.Instance.Spec.Game,
		Manifest: ld.Manifest,
		Renderer: ld.Renderer,
		BaseEnv:  []string{},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// A verb NAME arrives over the socket and console.Request.Validate only checks
// that it is non-blank. Writing it raw into the ring would let a caller forge
// lines in the console log — the record of who typed what, which is exactly the
// thing worth forging.
func TestVerbRecorderCannotForgeConsoleLines(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	r := newTestRunner(t, ld)

	rec := verbRecorder{r: r, log: slog.New(slog.DiscardHandler)}
	rec.RecordVerb("root_cli",
		control.Result{Verb: "broadcast\nSERVER STARTED"},
		errors.New("control: no such verb"))

	lines, _, _ := r.Ring().Snapshot(0, 100)
	if len(lines) != 1 {
		t.Fatalf("the refusal produced %d console lines, want 1", len(lines))
	}
	if strings.Contains(lines[0].Text, "\n") {
		t.Errorf("a newline reached the console line: %q", lines[0].Text)
	}
	if !strings.Contains(lines[0].Text, `\n`) {
		t.Errorf("the name was not escaped, so the reader cannot see what was sent: %q", lines[0].Text)
	}
}

// A control verb touches nothing the game prints. Without a line of its own an
// RCON broadcast is invisible in console.log forever.
func TestVerbRecorderEchoesAControlVerbButNotAConsoleOne(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	r := newTestRunner(t, ld)
	rec := verbRecorder{r: r, log: slog.New(slog.DiscardHandler)}

	// Seq 0 with the control channel: nothing else has recorded this.
	rec.RecordVerb("user:alice@example.com",
		control.Result{Verb: "broadcast", Channel: spec.ChannelControl, Command: `servermsg "back in 5"`}, nil)
	// Seq non-zero: Runner.Send already echoed it as SourceActor.
	rec.RecordVerb("user:alice@example.com",
		control.Result{Verb: "save", Channel: spec.ChannelConsole, Command: "save", Seq: 42}, nil)

	lines, _, _ := r.Ring().Snapshot(0, 100)
	if len(lines) != 1 {
		t.Fatalf("ring holds %d lines, want only the control verb", len(lines))
	}
	if lines[0].Source != console.SourceActor || lines[0].Actor != "user:alice@example.com" {
		t.Errorf("the echo is not attributed: %+v", lines[0])
	}
	if lines[0].Text != `servermsg "back in 5"` {
		t.Errorf("text = %q", lines[0].Text)
	}
}

// A secret that reached argv or a verb template comes back already redacted and
// the plaintext was never returned. Nothing here may try to recover it.
func TestVerbRecorderRecordsARedactedCommandAsIs(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	ld, err := loadRunSpec("pz-main", p, noEnv)
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}
	r := newTestRunner(t, ld)
	rec := verbRecorder{r: r, log: slog.New(slog.DiscardHandler)}
	rec.RecordVerb("root_cli", control.Result{
		Verb: "setpassword", Channel: spec.ChannelControl,
		Command: "setpassword " + spec.Redacted, Secret: true,
	}, nil)

	lines, _, _ := r.Ring().Snapshot(0, 100)
	if len(lines) != 1 || !strings.Contains(lines[0].Text, spec.Redacted) {
		t.Fatalf("lines = %+v", lines)
	}
}

// The control gate is open during `stopping` so the ladder's own `via: control`
// rungs can run. That must not also hand an operator's verb to a world that is
// mid-save, which is the transition internal/runner's state graph refuses by
// name.
func TestAVerbIsRefusedWhileTheShutdownLadderIsRunning(t *testing.T) {
	p := testPaths(t, "pz-main")
	writeInstance(t, p, adoptedPZ, "", "")
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "rcon"), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	ld, err := loadRunSpec("pz-main", p, func(k string) string {
		if k == "CREDENTIALS_DIRECTORY" {
			return credDir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadRunSpec: %v", err)
	}

	r := newTestRunner(t, ld)
	cmd, ctrl, err := buildCommander(r, ld, p, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildCommander: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })
	b := consoleBackend{Runner: r, cmd: cmd}

	for _, s := range []runner.State{runner.StateSpawning, runner.StateBooting, runner.StateReady} {
		if !r.SetState(s) {
			t.Fatalf("could not move to %s", s)
		}
	}
	if !r.SetState(runner.StateStopping) {
		t.Fatal("could not move to stopping")
	}
	if !r.ControlGateOpen() {
		t.Fatal("the control gate is shut in `stopping`; this test would pass for the wrong reason")
	}

	_, err = b.Verb(context.Background(), "root_cli", "broadcast", map[string]string{"Msg": "hello"})
	if err == nil {
		t.Fatal("a verb was accepted while the shutdown ladder was running")
	}
	if !errors.Is(err, socket.ErrBadArgument) {
		t.Errorf("err = %v, want it to map onto bad_request", err)
	}
	if !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("err = %v, want it to say the server is shutting down", err)
	}
}

// A stop that arrives while the game is still booting is not a readiness
// failure. Recording it as one puts a probe's name on an operator's decision
// and exits 75 -- which is absent from RestartPreventExitStatus, so systemd
// starts the world back up ten minutes after somebody stopped it.
func TestStopEndedTheBoot(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"the service manager cancelled us", dead, errors.New("readiness: context canceled"), true},
		{"an operator stopped it over the socket", live, runner.ErrStopRequested, true},
		{"wrapped", live, fmt.Errorf("readiness: %w", runner.ErrStopRequested), true},
		{"a real stall", live, runner.ErrStalled, false},
		{"a probe timed out", live, errors.New("readiness: hard_deadline exceeded"), false},
	} {
		if got := stopEndedTheBoot(c.ctx, c.err); got != c.want {
			t.Errorf("%s: stopEndedTheBoot = %t, want %t", c.name, got, c.want)
		}
	}
}
