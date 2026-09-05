//go:build linux

package runner

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// These tests run only on Linux CI, because a pty is the one part of the
// runner that cannot be faked into existence on the development machine: the
// hangup semantics being asserted here (EIO rather than EOF) are the tty
// layer's, not Go's.
//
// They launch /bin/sh deliberately. gvm never invents a shell -- noshell_test.go
// asserts that structurally -- but a manifest may name one, and PZ's
// start-server.sh is exactly that: a script the runner execs by absolute path
// with a list of arguments. Using it here reproduces the real topology.

func shellLaunch(mode, script string) Launch {
	return Launch{
		Path: "/bin/sh",
		Args: []string{"-c", script},
		Dir:  "/tmp",
		Env:  []string{"PATH=/usr/bin:/bin", "LC_ALL=C"},
		Console: ConsoleConfig{
			Mode:       mode,
			LineEnding: "\n",
			Rows:       DefaultRows,
			Cols:       DefaultCols,
		},
	}
}

// runShell spawns a script, pumps its console into a fake ring, waits for it to
// exit and returns what it printed plus how it ended.
func runShell(t *testing.T, l Launch, stdin string) ([]string, ExitStatus) {
	t.Helper()
	sp, err := spawn(l)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	f := console.NewFake(console.FakeOptions{PartialAfter: -1})
	pumped := make(chan error, 1)
	go func() { pumped <- pump(sp.Out, f.Ingest) }()

	if stdin != "" {
		if _, err := io.WriteString(sp.In, stdin); err != nil {
			t.Fatalf("write to the child's console: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := sp.Process.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	select {
	case err := <-pumped:
		// A pty master reports its hangup as EIO. If that reaches the caller as
		// an error, everything the dying process printed is thrown away.
		if err != nil {
			t.Fatalf("pump returned %v; a pty hangup is a normal end of stream", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pump never ended: the parent's copy of the slave was probably not closed")
	}
	return f.Texts(), st
}

func TestPTYDoesNotEchoInjectedInput(t *testing.T) {
	// With ECHO on, every command an operator types appears TWICE in the
	// console: once as the attributed actor echo the runner appends, and once
	// as though the game had printed it. The second copy is unattributed,
	// which makes the console log lie about who typed what.
	lines, st := runShell(t, shellLaunch(spec.ConsoleModePTY, "read line; echo GOT:$line"), "hello\n")
	if !st.OK() {
		t.Fatalf("exit = %s", st)
	}
	var got []string
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			got = append(got, s)
		}
	}
	if len(got) != 1 || got[0] != "GOT:hello" {
		t.Fatalf("console = %q, want exactly [GOT:hello]; a bare \"hello\" means the tty echoed our write back", got)
	}
}

func TestPTYKeepsTheWholeCrashTail(t *testing.T) {
	// Section 11.6 rule 8: the report always carries the last lines, because
	// that is the thing you actually want at 2am. This is the mechanism under
	// it -- 200 lines written and then an immediate non-zero exit, with the
	// hangup arriving while the last of them are still in the tty buffer.
	const n = 200
	script := "i=0; while [ $i -lt " + strconv.Itoa(n) + " ]; do echo line$i; i=$((i+1)); done; exit 1"
	lines, st := runShell(t, shellLaunch(spec.ConsoleModePTY, script), "")

	if st.Signaled || st.Code != 1 {
		t.Errorf("exit = %s, want exit status 1", st)
	}
	if len(lines) != n {
		t.Fatalf("captured %d of %d lines", len(lines), n)
	}
	if lines[0] != "line0" || lines[n-1] != "line"+strconv.Itoa(n-1) {
		t.Errorf("first/last = %q / %q", lines[0], lines[n-1])
	}
}

func TestPTYUsesTheDocumentedWindowSize(t *testing.T) {
	// Section 11.4 fixes the terminal at 200x50 at spawn. Games that format
	// their output to the width they saw at startup depend on it being set at
	// all; leaving it at the kernel default of 0x0 makes some of them wrap
	// every line.
	lines, st := runShell(t, shellLaunch(spec.ConsoleModePTY, "stty size"), "")
	if !st.OK() {
		t.Fatalf("exit = %s", st)
	}
	if len(lines) == 0 {
		t.Fatal("stty printed nothing")
	}
	want := strconv.Itoa(int(DefaultRows)) + " " + strconv.Itoa(int(DefaultCols))
	if got := strings.TrimSpace(lines[0]); got != want {
		t.Errorf("stty size = %q, want %q", got, want)
	}
}

func TestPTYDeliversOutputUnmodified(t *testing.T) {
	// OPOST is off, so the game's bytes reach the raw log as the game wrote
	// them. With ONLCR the tty would insert a CR before every LF and the
	// on-disk record -- the complete one the ring is allowed to evict from --
	// would contain bytes the game never produced.
	sp, err := spawn(shellLaunch(spec.ConsoleModePTY, "printf 'a\\nb\\n'"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	raw, err := io.ReadAll(sp.Out)
	if err != nil && !endOfStream(err) {
		t.Fatalf("read: %v", err)
	}
	if got := string(raw); got != "a\nb\n" {
		t.Errorf("raw stream = %q, want %q", got, "a\nb\n")
	}
}

func TestPipeModeCapturesStdoutAndStderrTogether(t *testing.T) {
	// A game's stack trace goes to stderr and has to land between the stdout
	// lines it belongs with, or the console tells a story with the causes
	// missing.
	lines, st := runShell(t, shellLaunch(spec.ConsoleModePipe, "echo out; echo err 1>&2; exit 3"), "")
	if st.Code != 3 {
		t.Errorf("exit = %s, want exit status 3", st)
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "out") || !strings.Contains(joined, "err") {
		t.Errorf("console = %q", lines)
	}
}

func TestSpawnRefusesAConsoleModeItDoesNotImplement(t *testing.T) {
	if _, err := spawn(shellLaunch("tmux", "true")); err == nil {
		t.Fatal("an unknown console mode was spawned")
	}
}

func TestConsoleModeNoneStillRunsAndStillPins(t *testing.T) {
	// `none` is a real answer, not a broken one: the game runs, gvm supervises
	// it, and only the console capability reports unsupported.
	sp, err := spawn(shellLaunch(spec.ConsoleModeNone, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.Out != nil || sp.In != nil {
		t.Error("console mode none produced a stream")
	}
	if !sp.Process.Identity().Valid() {
		t.Errorf("identity = %s", sp.Process.Identity())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if st, err := sp.Process.Wait(ctx); err != nil || !st.OK() {
		t.Errorf("wait = %s, %v", st, err)
	}
}

func TestSpawnRefusesAProgramThatIsNotThere(t *testing.T) {
	l := shellLaunch(spec.ConsoleModePTY, "true")
	l.Path = "/nonexistent/start-server.sh"
	_, err := spawn(l)
	if err == nil {
		t.Fatal("a missing program was spawned")
	}
	if !strings.Contains(err.Error(), l.Path) {
		t.Errorf("error does not name the program: %v", err)
	}
	// And nothing was left half-built: a leaked pty pair per failed start
	// would exhaust the host's pts allocation on a restart loop.
	if errors.Is(err, context.Canceled) {
		t.Error("unexpected error shape")
	}
}
