package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

func runProbe(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	env := &Env{Args: args, Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	err = runProbeWrite(context.Background(), env, args)
	return out.String(), errb.String(), err
}

// TestAWritableDirectoryIsReportedAsOkAndLeavesNothingBehind.
//
// The probe file must not survive: a dot-file appearing in an operator's save
// directory with no explanation is exactly the sort of thing that gets blamed
// on the game.
func TestAWritableDirectoryIsReportedAsOkAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := runProbe(t, dir)
	if err != nil {
		t.Fatalf("a writable directory failed the probe: %v", err)
	}

	got, perr := reconcile.ParseProbeResults(strings.NewReader(stdout))
	if perr != nil {
		t.Fatalf("the parent could not read this output: %v\n%s", perr, stdout)
	}
	if len(got) != 1 || !got[0].OK || got[0].Path != dir {
		t.Fatalf("results = %+v, want one OK result for %s", got, dir)
	}

	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("the probe left %d file(s) behind: %v", len(ents), ents)
	}
}

// TestAnUnwritableDirectoryIsReportedPerPathAndExits78.
//
// 78 is EX_CONFIG, which the instance template lists in
// RestartPreventExitStatus: a sandbox that blocks a required write is not
// something a restart fixes, and a restart loop would hide it. The same code
// is used here and at boot, so the two are one event with one number.
//
// The unwritable case is modelled with a path that does not exist rather than
// with a mode, because a mode-based test is meaningless on Windows and as root
// -- and this test has to run on the development machine.
func TestAnUnwritableDirectoryIsReportedPerPathAndExits78(t *testing.T) {
	ok := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-directory")

	stdout, stderr, err := runProbe(t, ok, missing)
	if err == nil {
		t.Fatal("a probe against a directory that does not exist succeeded")
	}
	var xe *ExitError
	if !errors.As(err, &xe) || xe.Code != probeWriteFailed {
		t.Fatalf("err = %v, want exit %d", err, probeWriteFailed)
	}

	// BOTH paths are reported, and the good one is reported as good: the
	// parent turns this list into one message naming every refusal, and an
	// operator widens the sandbox once instead of once per apply.
	got, perr := reconcile.ParseProbeResults(strings.NewReader(stdout))
	if perr != nil {
		t.Fatalf("the parent could not read this output: %v\n%s", perr, stdout)
	}
	if len(got) != 2 {
		t.Fatalf("results = %+v, want one per path", got)
	}
	if !got[0].OK {
		t.Errorf("the writable path was reported as failed: %+v", got[0])
	}
	if got[1].OK || got[1].Error == "" {
		t.Errorf("the unwritable path carries no reason: %+v", got[1])
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("the journal copy does not name the path: %q", stderr)
	}
}

// TestTheProbeStopsAtNothingUntilEveryPathIsTried.
//
// One exit code cannot say which of six paths was refused. Reporting all of
// them is why this command speaks NDJSON at all.
func TestTheProbeStopsAtNothingUntilEveryPathIsTried(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "gone")
	good := t.TempDir()
	stdout, _, _ := runProbe(t, bad, bad, good)

	got, err := reconcile.ParseProbeResults(strings.NewReader(stdout))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("results = %+v, want three; the probe stopped early", got)
	}
	if !got[2].OK {
		t.Error("the last path was never tried, or was tried and wrongly failed")
	}
}

// TestALeftoverProbeFileIsAFailureAndNotASilentPass.
//
// O_EXCL. Without it, a leftover from a crashed probe would make the next one
// succeed against a file this run did not create -- which is a pass earned by
// somebody else's work.
func TestALeftoverProbeFileIsAFailureAndNotASilentPass(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, reconcile.ProbeFilePrefix+itoa(os.Getpid()))
	if err := os.WriteFile(name, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeOne(dir, os.Getpid()); err == nil {
		t.Fatal("a probe succeeded against a file it did not create")
	}
	if _, err := os.Stat(name); err != nil {
		t.Error("the refused probe removed a file it did not create")
	}
}

// TestNoDirectoriesIsAUsageErrorRatherThanASilentPass.
//
// A probe of nothing proves nothing, and returning success for it would let a
// caller that built an empty path list conclude the sandbox is fine.
func TestNoDirectoriesIsAUsageErrorRatherThanASilentPass(t *testing.T) {
	if _, _, err := runProbe(t); err == nil {
		t.Fatal("`gvm probe-write` with no paths reported success")
	}
}

// TestTheCommandIsRegisteredAndHidden.
//
// Registered because the transient unit runs it by name; hidden because its
// only caller is another copy of gvm and a human typing it would be doing
// something wrong.
func TestTheCommandIsRegisteredAndHidden(t *testing.T) {
	c, ok := lookup("probe-write")
	if !ok {
		t.Fatal("`gvm probe-write` is not registered; the transient unit would fail with 'unknown command'")
	}
	if !c.Hidden {
		t.Error("`gvm probe-write` is listed in the usage output; nothing a human types belongs there")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
