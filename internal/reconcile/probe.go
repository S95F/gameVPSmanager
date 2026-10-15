package reconcile

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

// ErrProbe is the sentinel a failed write-probe wraps.
//
// It is distinct from ErrPlan because the operator's next move is different: a
// plan error is a gvm bug, and a probe failure is a sandbox the operator can
// widen or a path they can move -- and the message names both.
var ErrProbe = errors.New("reconcile: the sandbox blocks a write the game needs")

// ProbeBinary is the installed path of gvm, which the transient unit runs.
//
// It is the packaged location rather than os.Executable() on purpose. The
// probe has to run the binary the SERVICE will run -- the one in
// ExecStart=/usr/bin/gvm -- because that is the file the sandbox's
// ProtectSystem= and NoNewPrivileges= will apply to. Probing with a binary
// sitting in an operator's home directory would test a path the unit never
// takes.
const ProbeBinary = "/usr/bin/gvm"

// ProbeFilePrefix is the name `gvm probe-write` creates in each directory.
//
// The pid suffix means two probes running at once (an apply and a doctor) do
// not collide, and the leading dot keeps it out of a `ls` an operator runs
// while wondering what gvm put in their world directory.
const ProbeFilePrefix = ".gvm-probe-"

// ProbeTimeout bounds the transient unit.
//
// It becomes RuntimeMaxSec, so it is enforced by the kernel rather than by
// gvm's own patience: a probe that wedges on an NFS mount must not hold an
// apply open forever, and a `systemd-run --wait` that never returns is exactly
// how that would happen.
const ProbeTimeout = 30 * time.Second

// ProbeSpec is one write-probe: the sandbox to build and the paths to try
// inside it.
type ProbeSpec struct {
	Instance string
	// User and Group are the account the GAME runs as. Probing as root would
	// answer a question nobody asked: root can write through most of these
	// directives, and the failure D-15 exists to catch is a DAC-plus-sandbox
	// interaction that only the real account sees.
	User  string
	Group string

	// Sandbox is the same value the drop-in is rendered from. Sharing it is
	// the property that makes the probe meaningful.
	Sandbox Sandbox

	// Binary defaults to ProbeBinary.
	Binary string
	// Slice defaults to gvm-jobs.slice, so a probe on a busy box competes
	// with the other provisioning work rather than with a live world.
	Slice string
	// Timeout defaults to ProbeTimeout.
	Timeout time.Duration
}

// UnitName is the transient unit's name.
//
// It is derived from the instance name, which is already constrained to
// [a-z0-9-] (D-45), so it needs no escaping and `systemctl status
// gvm-probe-pz-main` works verbatim while the probe is running.
func (p ProbeSpec) UnitName() string { return "gvm-probe-" + p.Instance }

// Args builds the complete systemd-run argv.
//
// It is a pure function returning a []string, and both halves of that matter.
// Pure, so the exact command an apply will run is asserted in a test on the
// development machine, where there is no systemd. A []string, so there is no
// shell: a path containing a quote or a semicolon is one argv element, not a
// second command -- the same reason section 7.7 emits SteamCMD as argv.
func (p ProbeSpec) Args() ([]string, error) {
	if err := instance.ValidateName(p.Instance); err != nil {
		return nil, err
	}
	if p.User == "" {
		return nil, fmt.Errorf("%w: the probe needs the account the game runs as; probing as "+
			"root would answer a question nobody asked", ErrPlan)
	}
	group := p.Group
	if group == "" {
		group = p.User
	}
	if len(p.Sandbox.Paths) == 0 {
		return nil, fmt.Errorf("%w: no paths to probe", ErrPlan)
	}
	rwp, err := joinPaths("ReadWritePaths", p.Sandbox.Paths)
	if err != nil {
		return nil, err
	}
	for _, v := range []string{p.User, group, p.Sandbox.ProtectHome, p.Sandbox.ProtectSystem} {
		if err := checkUnitValue("probe property", v); err != nil {
			return nil, err
		}
	}

	bin := p.Binary
	if bin == "" {
		bin = ProbeBinary
	}
	slice := p.Slice
	if slice == "" {
		slice = instance.JobSliceName
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}

	args := []string{
		"--collect", "--wait", "--pipe", "--quiet",
		"--unit=" + p.UnitName(),
		"--slice=" + slice,
		"--uid=" + p.User,
		"--gid=" + group,
		"--property=ProtectSystem=" + p.Sandbox.ProtectSystem,
		"--property=ProtectHome=" + p.Sandbox.ProtectHome,
		"--property=PrivateTmp=yes",
		"--property=NoNewPrivileges=yes",
		fmt.Sprintf("--property=RuntimeMaxSec=%d", int(timeout/time.Second)),
		"--property=ReadWritePaths=" + rwp,
		bin, "probe-write",
	}
	return append(args, p.Sandbox.Paths...), nil
}

// ProbeResult is one path's answer, and it is the NDJSON `gvm probe-write`
// writes on stdout.
//
// It crosses a process boundary as JSON rather than as an exit code because
// one exit code cannot say WHICH of six paths failed, and "the sandbox blocks
// a required write" without a path is a message that sends an operator to read
// the whole unit file.
type ProbeResult struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
	// Error is the syscall failure as the child saw it. It is carried as a
	// string because it crossed a process boundary and there is no errno left
	// to wrap.
	Error string `json:"error,omitempty"`
	// Blocked is the directive gvm believes is responsible. It is filled in
	// by the PARENT, from the sandbox it built, because the child inside the
	// namespace cannot see which directive hid a directory from it.
	Blocked string `json:"blocked,omitempty"`
}

// maxProbeOutput bounds what is read back from the transient unit.
//
// The child writes one short line per path. A megabyte means something else is
// on that pipe, and a reconciler that buffers whatever a subprocess feels like
// producing is a memory bug waiting for a bad day.
const maxProbeOutput = 1 << 20

// ParseProbeResults reads the child's NDJSON.
//
// It takes an io.Reader rather than running anything, so every shape of output
// -- a clean run, a partial line, a line of garbage from a wrapper that
// printed a warning, an empty stream because the unit never started -- is
// tested on the development machine.
//
// A line that is not JSON is an ERROR rather than a skipped line. The output
// of this program is the evidence an apply proceeds on, and quietly ignoring
// the half of it that did not parse is how an apply proceeds on evidence it
// does not have.
func ParseProbeResults(r io.Reader) ([]ProbeResult, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxProbeOutput))
	sc.Buffer(make([]byte, 0, 8<<10), 64<<10)
	var out []ProbeResult
	for line := 1; sc.Scan(); line++ {
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		var res ProbeResult
		if err := json.Unmarshal([]byte(t), &res); err != nil {
			return out, fmt.Errorf("%w: probe output line %d is not JSON: %q", ErrProbe, line, truncate(t, 120))
		}
		if res.Path == "" {
			return out, fmt.Errorf("%w: probe output line %d names no path", ErrProbe, line)
		}
		out = append(out, res)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("%w: reading probe output: %w", ErrProbe, err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// trimStderr keeps systemd-run's own complaint short enough to read.
//
// It lives here rather than beside its only caller in probe_linux.go because it
// is pure string handling, and pure logic behind a //go:build linux tag is
// logic the development machine never type-checks and no test on it can reach.
// Args() and ParseProbeResults() are untagged for exactly this reason -- the
// command this package would run and the output it would read back are both
// asserted by tests that run everywhere -- and this was the odd one out.
func trimStderr(s string) string { return truncate(strings.Join(strings.Fields(s), " "), 400) }

// Blame names the sandbox directive most likely responsible for a refused
// write.
//
// The child cannot answer this: inside the namespace, a directory hidden by
// ProtectHome and a directory that was never there produce the same ENOENT.
// The parent built the sandbox and can say which directive covers the path,
// which turns "permission denied" into "ProtectHome=read-only hides
// /home/pzserver/Zomboid" -- the difference between a five-minute fix and an
// afternoon.
func Blame(sb Sandbox, path string) string {
	switch {
	case underHome(path) && sb.ProtectHome != "no":
		return "ProtectHome=" + sb.ProtectHome
	case !inPaths(sb.Paths, path) && sb.ProtectSystem == "strict":
		return "ProtectSystem=strict with no ReadWritePaths= covering this path"
	case !inPaths(sb.Paths, path):
		return "the path is not in ReadWritePaths="
	}
	// Everything the sandbox governs permits this write, so what refused it
	// is ordinary unix ownership. Saying so is the useful answer: the fix is
	// a chown, not a unit edit.
	return "not a sandbox directive: check the directory's owner and mode"
}

func inPaths(paths []string, p string) bool {
	for _, r := range paths {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

// ProbeError renders every refusal as one message.
//
// All of them, because an operator who has to widen a sandbox wants to widen
// it once. Reporting the first failure per apply turns a three-path problem
// into three applies.
func ProbeError(sb Sandbox, results []ProbeResult) error {
	var bad []string
	for _, r := range results {
		if r.OK {
			continue
		}
		blocked := r.Blocked
		if blocked == "" {
			blocked = Blame(sb, r.Path)
		}
		msg := r.Path + ": " + blocked
		if r.Error != "" {
			msg += " (" + r.Error + ")"
		}
		bad = append(bad, msg)
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  %s\nNothing has been changed. Fix the paths or the sandbox and "+
		"re-run; the drop-in was not written (D-15).", ErrProbe, strings.Join(bad, "\n  "))
}

// MissingProbePaths reports paths the probe did not answer for.
//
// A probe that reported success for four of six paths has not proved anything
// about the other two, and treating silence as approval is exactly the shape
// of failure this check exists to prevent.
func MissingProbePaths(want []string, got []ProbeResult) []string {
	seen := make(map[string]bool, len(got))
	for _, r := range got {
		seen[r.Path] = true
	}
	var out []string
	for _, p := range want {
		if !seen[p] {
			out = append(out, p)
		}
	}
	return out
}
