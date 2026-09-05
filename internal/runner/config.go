package runner

import (
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Console defaults from sections 11.3 and 11.4. The terminal size is fixed at
// spawn and never renegotiated: the pty is the GAME's terminal, shared by every
// viewer, so a per-client TIOCSWINSZ would resize a window somebody else is
// reading.
const (
	DefaultRows uint16 = 50
	DefaultCols uint16 = 200
)

// Config is everything the runner needs to supervise one instance.
//
// It is a struct rather than a pile of arguments because the runner is
// constructed in exactly two places -- `gvm run` and the tests -- and both
// want to see the whole surface at once, including the seams (Spawn, Clock,
// BaseEnv) that let the second one work on a machine with no /proc.
type Config struct {
	Instance string
	Game     string
	// Build identifies the gvm binary in the socket's hello frame, so a
	// dashboard showing a stale console can say which runner it is talking to.
	Build string
	// Unit is the systemd unit name, recorded in runner.json for the operator
	// who has a pid and needs to know what to stop.
	Unit string

	Paths    instance.Paths
	Manifest *spec.Manifest
	Renderer *spec.Renderer

	// BaseEnv is the environment the child inherits before the manifest's own
	// env is layered on. nil means os.Environ(), which under systemd already
	// carries the unit's rendered Environment= lines.
	BaseEnv []string

	// Clock drives the console's timers. nil is the system clock.
	Clock console.Clock

	// Logger is the runner's own diagnostics channel, which goes to the
	// journal. The GAME's console never goes there: journald drops more than
	// 10 000 messages per 30 s per service and would put holes in the exact
	// post-mortem an operator is reading.
	Logger *slog.Logger

	// Notify reports state to systemd. nil is a disabled notifier, which is
	// what running `gvm run` from a shell gets.
	Notify *platform.Notifier

	// Spawn is the process seam. nil means the platform spawner, which exists
	// on Linux only.
	Spawn SpawnFunc
}

// ConsoleConfig is the manifest's console block, resolved and defaulted.
type ConsoleConfig struct {
	Mode       string
	StripANSI  bool
	LineEnding string // the real bytes, not the manifest's name for them
	RingBytes  int
	Rows, Cols uint16
	EchoInput  bool
}

// Capturing reports whether this mode gives the runner a stream to pump. Mode
// `none` is a real answer for a game that cannot be run on anything but its
// own terminal; it makes the console capability report unsupported rather than
// blank.
func (c ConsoleConfig) Capturing() bool {
	return c.Mode == spec.ConsoleModePTY || c.Mode == spec.ConsoleModePipe
}

// consoleConfig resolves the manifest's console block.
func consoleConfig(cs spec.ConsoleSpec) (ConsoleConfig, error) {
	c := ConsoleConfig{
		Mode:      cs.Mode,
		StripANSI: cs.StripANSI,
		RingBytes: cs.RingBytes,
		Rows:      cs.Rows,
		Cols:      cs.Cols,
		EchoInput: cs.EchoInput,
	}
	switch cs.Mode {
	case spec.ConsoleModePTY, spec.ConsoleModePipe, spec.ConsoleModeNone:
	default:
		return ConsoleConfig{}, fmt.Errorf("runner: spec.runtime.console.mode %q is not %s|%s|%s",
			cs.Mode, spec.ConsoleModePTY, spec.ConsoleModePipe, spec.ConsoleModeNone)
	}
	le, err := lineEnding(cs.LineEnding)
	if err != nil {
		return ConsoleConfig{}, err
	}
	c.LineEnding = le
	if c.Rows == 0 {
		c.Rows = DefaultRows
	}
	if c.Cols == 0 {
		c.Cols = DefaultCols
	}
	return c, nil
}

// lineEnding maps the manifest's name for a line terminator onto the bytes
// written after an injected command.
//
// An unknown name is refused rather than defaulted. A game whose console needs
// CRLF and silently gets LF accepts nothing an operator types, and the symptom
// -- commands vanishing with no error -- is indistinguishable from the server
// ignoring them.
func lineEnding(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "lf":
		return "\n", nil
	case "cr":
		return "\r", nil
	case "crlf":
		return "\r\n", nil
	}
	return "", fmt.Errorf("runner: spec.runtime.console.line_ending %q is not lf|cr|crlf", name)
}

// Launch is a fully resolved command: an argv LIST, an env list and a working
// directory. There is no command string anywhere in it, and that is the point.
//
// Every value in Args came out of a manifest template with operator-supplied
// variables in it. If any of them were ever concatenated into one string and
// handed to a shell, `server_name: "; rm -rf /"` would be a remote code
// execution with a YAML file as the payload. noshell_test.go asserts the
// absence of that path structurally.
type Launch struct {
	Path string
	Args []string // argv[1:]; Path is argv[0]
	Dir  string
	Env  []string // KEY=VALUE, sorted

	Console ConsoleConfig

	// SecretArgs indexes Args whose value came from a tainted template, and
	// SecretEnv names the env keys likewise. PZ's install-phase
	// `-adminpassword <secret>` is argv, so an unredacted "-- gvm: exec ..."
	// annotation would put the admin password in the console log, the ring,
	// and every dashboard attached to it.
	SecretArgs []int
	SecretEnv  []string
}

// Argv is the child's argument vector: the resolved path as argv[0], then the
// rendered arguments, each one its own element.
//
// It exists as a function so that the shape of what reaches execve is asserted
// on every platform, including the one where the spawn itself cannot run. A
// game server's argv is built here and nowhere else.
func (l Launch) Argv() []string {
	return append([]string{l.Path}, l.Args...)
}

// Display renders the launch for a human: the console annotation, the journal
// line, the shutdown report.
//
// It is NOT a command line. Secrets are replaced by spec.Redacted, so pasting
// it into a shell would start a server with the literal string "••••••" as its
// admin password -- which is the correct outcome for a string that exists to be
// read, not run.
func (l Launch) Display() string {
	secret := make(map[int]struct{}, len(l.SecretArgs))
	for _, i := range l.SecretArgs {
		secret[i] = struct{}{}
	}
	parts := make([]string, 0, len(l.Args)+1)
	parts = append(parts, quoteArg(l.Path))
	for i, a := range l.Args {
		if _, ok := secret[i]; ok {
			a = spec.Redacted
		}
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg makes an argv element readable without pretending to be shell
// quoting: an element with whitespace or a quote in it is shown with Go's
// %q so the reader can see where it starts and ends.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\r\"'\\") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// BuildLaunch resolves a manifest's runtime block into an executable command.
//
// base is the environment the child inherits; the caller passes os.Environ()
// in production and a fixed list in tests, because an env built from the
// developer's own machine is not a fixture.
func BuildLaunch(m *spec.Manifest, r *spec.Renderer, base []string) (Launch, error) {
	if m == nil || r == nil {
		return Launch{}, fmt.Errorf("runner: BuildLaunch needs a manifest and a renderer")
	}
	rt := m.Spec.Runtime

	// A wrapper prepends another program's argv to the game's. Refusing is the
	// honest answer for a build that does not implement it: silently dropping
	// `xvfb` would start a headless engine with no X server and produce a
	// failure whose cause is three layers away from its symptom.
	if rt.Wrapper != nil {
		return Launch{}, fmt.Errorf("runner: spec.runtime.wrapper (kind %q) is not implemented in this build", rt.Wrapper.Kind)
	}

	cc, err := consoleConfig(rt.Console)
	if err != nil {
		return Launch{}, err
	}

	dir, _, err := r.Render("spec.runtime.cwd", rt.Cwd)
	if err != nil {
		return Launch{}, err
	}
	dir = strings.TrimSpace(dir)
	if dir != "" && !path.IsAbs(dir) {
		return Launch{}, fmt.Errorf("runner: spec.runtime.cwd rendered to %q, which is not an absolute path", dir)
	}

	execPath, _, err := r.Render("spec.runtime.exec", rt.Exec)
	if err != nil {
		return Launch{}, err
	}
	full, err := resolveExec(execPath, dir)
	if err != nil {
		return Launch{}, err
	}

	l := Launch{Path: full, Dir: dir, Console: cc}
	data := r.Data()
	for i, a := range rt.Args {
		field := fmt.Sprintf("spec.runtime.args[%d]", i)
		ok, err := a.When.Eval(&data)
		if err != nil {
			return Launch{}, fmt.Errorf("%s.when: %w", field, err)
		}
		if !ok {
			continue
		}
		// The two spellings are exclusive, and an empty expansion is refused
		// for the same reason spec's decoder refuses a null argv element: a
		// dropped `-servername` does not fail, it generates a fresh world.
		values := a.Values
		switch {
		case values != nil && a.Value != "":
			return Launch{}, fmt.Errorf("%s sets both `value` and `values`; which one wins would depend on this function", field)
		case values != nil && len(values) == 0:
			return Launch{}, fmt.Errorf("%s has an empty `values` list, so it expands to no argument at all; delete it or write the value", field)
		case values == nil:
			values = []string{a.Value}
		}
		for _, v := range values {
			got, taint, err := r.Render(field, v)
			if err != nil {
				return Launch{}, err
			}
			if strings.ContainsRune(got, 0) {
				// execve would truncate the argument at the NUL and the game
				// would receive a shorter value than the manifest wrote.
				return Launch{}, fmt.Errorf("%s: rendered value contains a NUL byte", field)
			}
			if taint.Has(spec.TaintSecret) {
				l.SecretArgs = append(l.SecretArgs, len(l.Args))
			}
			l.Args = append(l.Args, got)
		}
	}

	env := make(map[string]string, len(rt.Env))
	for k, v := range rt.Env {
		got, taint, err := r.Render("spec.runtime.env."+k, v)
		if err != nil {
			return Launch{}, err
		}
		if err := checkEnvPair(k, got); err != nil {
			return Launch{}, err
		}
		env[k] = got
		if taint.Has(spec.TaintSecret) {
			l.SecretEnv = append(l.SecretEnv, k)
		}
	}
	sort.Strings(l.SecretEnv)
	l.Env = mergeEnv(base, env, runnerOnlyEnv)
	return l, nil
}

// resolveExec turns the manifest's exec into an absolute path.
//
// Three refusals, and each one closes a way for the launch to become something
// other than "run this file":
//
//   - A bare name ("java") would be looked up on PATH, so which binary starts
//     the world would depend on an environment variable.
//   - A relative path is joined to the rendered cwd here rather than left for
//     os/exec, whose resolution of a relative Path against Dir has changed
//     between Go releases; a launch that silently changes meaning on a
//     toolchain upgrade is not a launch gvm can pin.
//   - A NUL byte would truncate the path at exec(2).
func resolveExec(execPath, dir string) (string, error) {
	execPath = strings.TrimSpace(execPath)
	switch {
	case execPath == "":
		return "", fmt.Errorf("runner: spec.runtime.exec is empty")
	case strings.ContainsRune(execPath, 0):
		return "", fmt.Errorf("runner: spec.runtime.exec contains a NUL byte")
	case !strings.Contains(execPath, "/"):
		return "", fmt.Errorf("runner: spec.runtime.exec %q has no path separator, so it would be looked up on PATH; "+
			"write ./%s or an absolute path", execPath, execPath)
	}
	if path.IsAbs(execPath) {
		return path.Clean(execPath), nil
	}
	if dir == "" {
		return "", fmt.Errorf("runner: spec.runtime.exec %q is relative but spec.runtime.cwd is empty", execPath)
	}
	return path.Join(dir, execPath), nil
}

// runnerOnlyEnv are variables the manager set FOR THE RUNNER, which must not
// be inherited by the game.
//
// NOTIFY_SOCKET is the dangerous one: a child that inherits it can send
// READY=1 for a unit that is still booting, or STOPPING=1 for one that is
// fine, and systemd will believe it. Games do not do this on purpose, but a
// Java wrapper that re-execs `systemd-notify` for its own supervision has no
// idea it is speaking for somebody else's unit.
//
// CREDENTIALS_DIRECTORY is defence in depth rather than a boundary: the game
// runs as the same uid that may read those files, so hiding the path raises
// the bar for a mod that greps its environment and nothing more.
var runnerOnlyEnv = []string{
	"NOTIFY_SOCKET",
	"LISTEN_FDS",
	"LISTEN_PID",
	"LISTEN_FDNAMES",
	"CREDENTIALS_DIRECTORY",
}

// mergeEnv layers over on top of base, drops the named keys and returns a
// sorted KEY=VALUE list.
//
// Sorted, so that two runs of the same instance produce byte-identical
// environments and a diff of two launches shows what actually changed rather
// than map iteration order.
func mergeEnv(base []string, over map[string]string, drop []string) []string {
	skip := make(map[string]struct{}, len(drop))
	for _, k := range drop {
		skip[k] = struct{}{}
	}
	merged := make(map[string]string, len(base)+len(over))
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		// An entry with no '=' is not an assignment, and one with an empty key
		// is Windows' "=C:" cwd bookkeeping. Neither is inheritable.
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if _, bad := skip[k]; bad {
			continue
		}
		merged[k] = kv[i+1:]
	}
	for k, v := range over {
		if _, bad := skip[k]; bad {
			continue
		}
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// checkEnvPair refuses an environment entry execve cannot carry intact.
func checkEnvPair(k, v string) error {
	switch {
	case k == "":
		return fmt.Errorf("runner: spec.runtime.env has an empty key")
	case strings.ContainsAny(k, "=\x00"):
		return fmt.Errorf("runner: spec.runtime.env key %q contains %q or a NUL byte", k, "=")
	case strings.ContainsRune(v, 0):
		return fmt.Errorf("runner: spec.runtime.env.%s contains a NUL byte", k)
	}
	return nil
}
