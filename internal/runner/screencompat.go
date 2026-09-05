package runner

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// screencompat is the adoption-week bridge, and its most important property is
// that it sunsets (D-11).
//
// `screen -r pzserver` is muscle memory for the operator this product is being
// built for, and taking it away on cutover day is how a migration gets rolled
// back. So `console_compat: screen` keeps it working -- by running `gvm
// console` INSIDE a screen session. The session is a VIEW: it attaches to the
// console socket as the instance user, which section 11.4 gives `tail` and
// `send` and never `verb` or `stop`. Nothing about the supervision path goes
// through screen, which is the whole point of D-11: `screen -X stuff` returns
// immediately and cannot tell you whether the command was delivered.
//
// The 90-day sunset is in the code rather than in a README because a
// transitional mechanism with no expiry date is a permanent one. `gvm doctor`
// warns from T-14 and `gvm apply` refuses to render it past the sunset.

const (
	// ScreenCompatMode is the manifest's `runtime.console_compat` value.
	ScreenCompatMode = "screen"
	// ScreenSunset is how long after first use the bridge stops being
	// renderable.
	ScreenSunset = 90 * 24 * time.Hour
	// ScreenWarnFrom is when doctor starts warning.
	ScreenWarnFrom = 14 * 24 * time.Hour

	// DefaultScreenPath and DefaultGvmPath are absolute because nothing here
	// may resolve a program through the environment: the bridge runs as the
	// instance user, and that user's PATH is not gvm's to trust.
	DefaultScreenPath = "/usr/bin/screen"
	DefaultGvmPath    = "/usr/local/bin/gvm"
)

// ScreenCompat is one instance's bridge.
type ScreenCompat struct {
	// Session is the screen session name -- what the operator types after
	// `screen -r`. It is the instance name by default.
	Session string
	// Socket is the console socket the inner `gvm console` dials.
	Socket string

	ScreenPath string
	GvmPath    string

	// FirstUsed is when this instance first rendered the bridge, which starts
	// the sunset clock. Zero means "starting now".
	FirstUsed time.Time

	// Start is the process seam. nil runs the command for real.
	Start func(argv []string) error
}

// ScreenSessionOK reports whether s is a usable screen session name.
//
// The set is deliberately narrow. The name reaches `screen -dmS <name>` as one
// argv element, so there is no injection here -- but it also reaches the
// operator's own `screen -r <name>`, typed into a shell, and a session name
// with a space or a quote in it is one they cannot type. A control character
// in it would corrupt `screen -ls` output for every session on the box.
func ScreenSessionOK(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("runner: a screen session needs a name")
	case len(s) > 80:
		return fmt.Errorf("runner: screen session name %q is longer than 80 characters", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("runner: screen session name %q contains %q; use letters, digits, '-', '_' and '.'", s, string(r))
		}
	}
	return nil
}

// Argv renders the command that starts the bridge.
//
// It is a LIST, built the same way the game's own launch is built and for the
// same reason: nothing here is ever concatenated into a string, so an instance
// name cannot become a command. `-dmS` starts the session detached, so the
// process returns immediately and the runner is not its supervisor.
func (c ScreenCompat) Argv() ([]string, error) {
	if err := ScreenSessionOK(c.Session); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.Socket) == "" {
		return nil, fmt.Errorf("runner: the screen bridge needs a console socket path")
	}
	screen, err := absProgram("console_compat screen binary", c.ScreenPath, DefaultScreenPath)
	if err != nil {
		return nil, err
	}
	gvm, err := absProgram("console_compat gvm binary", c.GvmPath, DefaultGvmPath)
	if err != nil {
		return nil, err
	}
	return []string{
		screen, "-dmS", c.Session,
		gvm, "console", "--socket", c.Socket, "--no-color",
	}, nil
}

// absProgram resolves a program path, refusing anything that would be looked
// up on PATH.
func absProgram(what, given, fallback string) (string, error) {
	p := strings.TrimSpace(given)
	if p == "" {
		p = fallback
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("runner: %s %q is not an absolute path, so which binary runs would depend on PATH", what, p)
	}
	return p, nil
}

// SunsetAt is when this bridge stops being renderable.
func (c ScreenCompat) SunsetAt() time.Time {
	first := c.FirstUsed
	if first.IsZero() {
		return time.Time{}
	}
	return first.Add(ScreenSunset)
}

// ScreenSunsetState reports where a bridge is in its life.
//
// It is a pure function over two timestamps so that doctor, apply and the
// runner all answer identically -- a sunset that three components compute
// three ways is a sunset that never actually arrives.
func ScreenSunsetState(now, firstUsed time.Time) (warn, expired bool, remaining time.Duration) {
	if firstUsed.IsZero() {
		return false, false, ScreenSunset
	}
	remaining = firstUsed.Add(ScreenSunset).Sub(now)
	switch {
	case remaining <= 0:
		return true, true, 0
	case remaining <= ScreenWarnFrom:
		return true, false, remaining
	}
	return false, false, remaining
}

// Run starts the bridge.
func (c ScreenCompat) Run() error {
	argv, err := c.Argv()
	if err != nil {
		return err
	}
	start := c.Start
	if start == nil {
		start = startDetached
	}
	return start(argv)
}

// startDetached runs argv and waits for it to return.
//
// `screen -dmS` forks the session and exits immediately, so waiting here waits
// for screen's own setup rather than for the session -- which is what makes an
// error usable: a non-zero status means the session was NOT created, and the
// operator's `screen -r` is going to fail.
//
// The command is an exec.Cmd struct literal with an absolute Path. Nothing in
// this package resolves a program through PATH; noshell_test.go asserts it.
func startDetached(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("runner: nothing to start")
	}
	cmd := &exec.Cmd{Path: argv[0], Args: argv}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

// StartScreenCompat starts the bridge for this instance if the manifest asks
// for one.
//
// It is called after readiness, not at spawn, because the session's whole job
// is to attach to the console socket and the socket does not exist until the
// runner is serving. A failure is a warning, never fatal: the bridge is a
// convenience, and refusing to run a game server because a compatibility shim
// did not start would be the tail wagging the dog.
func (r *Runner) StartScreenCompat(gvmPath string) error {
	compat := strings.TrimSpace(r.cfg.Manifest.Spec.Runtime.ConsoleCompat)
	if compat == "" {
		return nil
	}
	if compat != ScreenCompatMode {
		return fmt.Errorf("runner: spec.runtime.console_compat %q is not %q", compat, ScreenCompatMode)
	}
	c := ScreenCompat{
		Session: r.cfg.Instance,
		Socket:  r.cfg.Paths.ConsoleSock,
		GvmPath: gvmPath,
	}
	// Run inside the exemption window. The session leader and the `gvm
	// console` it hosts land in the unit's OWN cgroup and stay there for the
	// life of the instance, and Empty() is cgroup-wide -- so without this a
	// textbook `quit` would never settle, would walk to the SIGKILL rungs, and
	// would be reported as a degraded shutdown of a world that saved perfectly.
	if err := r.exemptAround(c.Run); err != nil {
		r.log.Warn("console_compat screen", "err", err)
		r.Note("-- gvm: the screen compatibility session did not start: %v", err)
		return err
	}
	r.Note("-- gvm: screen compatibility session %q is up; `screen -r %s` attaches (sunsets in %d days)",
		c.Session, c.Session, int(ScreenSunset/(24*time.Hour)))
	if ids := r.SweepExempt(); len(ids) > 0 {
		// Recorded, because an invisible exemption list is how the next
		// version of this bug hides: a cgroup rung that skipped pids has to
		// say which.
		r.Note("-- gvm: %s is exempt from the cgroup sweep (%s)", plural(len(ids), "process"), joinIdentities(ids))
	}
	return nil
}

// sweepExempter is the optional half of Process: one that can prove which
// processes in the cgroup are gvm's own helpers rather than game survivors.
type sweepExempter interface {
	ExemptAround(fn func() error) error
	SweepExempt() []Identity
}

// exemptAround runs fn with the sweep exemption window open when the platform
// supports one, and plainly when it does not.
func (r *Runner) exemptAround(fn func() error) error {
	if e, ok := r.Process().(sweepExempter); ok {
		return e.ExemptAround(fn)
	}
	return fn()
}

// SweepExempt lists the processes gvm started inside the unit's cgroup that
// the shutdown sweep must not count as game survivors.
func (r *Runner) SweepExempt() []Identity {
	if e, ok := r.Process().(sweepExempter); ok {
		return e.SweepExempt()
	}
	return nil
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ses", n, what)
}

func joinIdentities(ids []Identity) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return strings.Join(parts, ", ")
}
