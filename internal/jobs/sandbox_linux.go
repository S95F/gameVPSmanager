//go:build linux

package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// The absolute paths systemd ships its tools at.
//
// Absolute and stat-resolved rather than looked up through PATH: this runs as
// root, and an inherited PATH deciding which program starts a transient unit
// with --uid= is a decision nobody made. internal/reconcile resolves
// systemd-run the same way, for the same reason.
var (
	systemdRunPaths = []string{"/usr/bin/systemd-run", "/bin/systemd-run"}
	systemctlPaths  = []string{"/usr/bin/systemctl", "/bin/systemctl"}
)

// graceOverBudget is how long gvm waits past RuntimeMaxSec before giving up on
// systemd-run itself.
//
// One rung above the kernel's deadline, so the kill that normally happens is
// systemd's and this one only covers systemd-run hanging. Ordering them the
// other way would mean gvm killed the job first and RuntimeMaxSec was never
// tested by anything.
const graceOverBudget = 15 * time.Second

// RunSandboxed launches cmd as a transient systemd SERVICE and waits for it.
//
// Never a scope. A scope runs the command in the CALLER's cgroup and unit
// context: it takes no meaningful hardening properties, inherits root's
// environment and capabilities, and dies with whoever started it. SteamCMD
// self-updates and then executes Valve-supplied binaries, so running it in a
// scope would give it strictly less confinement than the game it installs --
// the regression D-16 exists to name. A transient service also survives
// `systemctl restart gvmd`, because it is not a child of any daemon.
//
// out receives the child's stdout AND stderr, interleaved in arrival order. The
// same writer is given to both streams, which os/exec documents as serialising
// the writes, so a streaming consumer (the SteamCMD classifier is one) sees the
// output as it is produced rather than at the end. That matters: an early "No
// subscription" in a four-hour download has to be found while the download is
// still running.
//
// The returned error is an *ExitError when the child ran and failed, and
// something else when the SANDBOX failed -- systemd-run missing, the unit
// refused, the name already taken. Callers that classify program output depend
// on telling those apart, because there is nothing to classify in the second
// case.
func RunSandboxed(ctx context.Context, s Sandbox, cmd []string, out io.Writer) error {
	args, err := s.Args(cmd)
	if err != nil {
		return err
	}
	bin, err := findTool(systemdRunPaths)
	if err != nil {
		return fmt.Errorf("%w: %w", platform.Unsupported("transient units (systemd-run)"), err)
	}

	budget := s.Budget()
	ctx, cancel := context.WithTimeout(ctx, budget+graceOverBudget)
	defer cancel()

	c := exec.CommandContext(ctx, bin, args...)
	// An empty environment, not the caller's. systemd-run passes nothing of
	// its own environment to the unit, but it reads plenty for itself
	// (XDG_RUNTIME_DIR, DBUS_SESSION_BUS_ADDRESS), and a job that behaved
	// differently depending on whether root's shell or a timer started it is a
	// bug nobody can reproduce.
	c.Env = []string{}
	// Nothing on stdin. steamcmd prompts for a Steam Guard code when its
	// cached refresh token has expired; with no stdin that is an immediate EOF
	// and a classifiable failure instead of a unit sitting at a prompt until
	// RuntimeMaxSec.
	c.Stdin = nil

	// A copy of the tail, for the ExitError's Detail. The job log already has
	// everything; this is so the error a caller returns up the stack says
	// something, rather than "exit status 1" with the explanation three screens
	// away in another stream.
	tail := &tailBuffer{max: 4 << 10}
	var w io.Writer = tail
	if out != nil {
		w = io.MultiWriter(out, tail)
	}
	c.Stdout, c.Stderr = w, w

	started := time.Now()
	runErr := c.Run()
	elapsed := time.Since(started)
	if runErr == nil {
		return nil
	}

	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		// Whether RuntimeMaxSec fired is decided by the clock rather than by
		// the exit status, because systemd-run's status for a signal-killed
		// service has not been stable across the systemd versions this project
		// supports (249 on buster-era boxes through 257). The clock is
		// unambiguous: a run that came back at its budget was killed at its
		// budget.
		//
		// This is also why nothing in gvm treats a steamcmd exit code as
		// evidence (section 13.5). The verdict on what happened comes from the
		// output, three independent times; this status only says that
		// something stopped.
		return &ExitError{
			Unit:     s.UnitName(),
			Code:     ee.ExitCode(),
			Signaled: ee.ProcessState != nil && ee.ExitCode() == -1,
			Timeout:  elapsed >= budget,
			Detail:   trimDiagnostic(tail.String()),
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// systemd-run itself did not return within RuntimeMaxSec + grace. The
		// unit may still be running, so say so: the operator's next move is
		// `systemctl status`, not a retry.
		return fmt.Errorf("jobs: systemd-run for %s did not return within %s; the unit may "+
			"still be running (`systemctl status %s`): %w",
			s.UnitName(), budget+graceOverBudget, s.UnitName(), runErr)
	}
	return fmt.Errorf("jobs: starting %s: %w: %s", s.UnitName(), runErr,
		trimDiagnostic(tail.String()))
}

// ShowUnit reads properties back off a unit with `systemctl show`.
//
// This is the other half of the D-16 assertion and it is deliberately a
// separate call: building the right argv proves nothing about a host whose
// systemd silently ignored a directive, and the symptom of that -- a SteamCMD
// with more reach than the game it installs -- looks exactly like success.
//
// It is called while the unit is RUNNING. Args passes --collect, so the unit is
// unloaded as soon as it goes inactive and there is nothing left to ask
// afterwards; that is the correct trade, because a fixed unit name whose failed
// instance lingered would make every subsequent nightly fail with "unit already
// exists".
func ShowUnit(ctx context.Context, unit string, props []string) (map[string]string, error) {
	bin, err := findTool(systemctlPaths)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", platform.Unsupported("systemctl show"), err)
	}
	args := make([]string, 0, len(props)+2)
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	args = append(args, "show", unit)

	c := exec.CommandContext(ctx, bin, args...)
	c.Env = []string{}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	runErr := c.Run()

	// Parsed whether or not systemctl succeeded: `systemctl show` of a unit
	// that has already gone away exits non-zero while still printing the
	// properties it knows, and discarding those would throw away the only
	// evidence this call exists to collect.
	got, parseErr := ParseShow(&stdout)
	if len(got) > 0 {
		return got, nil
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if runErr != nil {
		return nil, fmt.Errorf("jobs: systemctl show %s: %w: %s", unit, runErr,
			trimDiagnostic(stderr.String()))
	}
	return nil, fmt.Errorf("%w: `systemctl show %s` reported nothing, so no property is "+
		"verified", ErrProperties, unit)
}

// VerifyUnit reads a running unit's properties and reports every one that did
// not land.
//
// The verdict itself (CheckShow) is pure and tested on the development machine;
// this is only the part that needs a systemd to ask.
func VerifyUnit(ctx context.Context, s Sandbox) error {
	got, err := ShowUnit(ctx, s.UnitName(), s.ShowProperties())
	if err != nil {
		return err
	}
	return s.PropertiesError(s.CheckShow(got))
}

func findTool(candidates []string) (string, error) {
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("none of %v is present", candidates)
}

// tailBuffer keeps the LAST max bytes written to it.
//
// The tail rather than the head: a failure's explanation is at the end of the
// output, and a four-hour download's head is a progress line repeated ten
// thousand times. It never grows past 2*max, so a job that produces gigabytes
// cannot turn its own error message into a memory bug.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if len(t.buf) > t.max {
		return string(t.buf[len(t.buf)-t.max:])
	}
	return string(t.buf)
}
