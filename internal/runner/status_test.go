package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestStatusIsWrittenOnEveryTransition. status.json is the DB-free source of
// truth: kill gvmd, kill gvm-web, kill Postgres, and `cat` still tells an
// operator what the server is doing.
func TestStatusIsWrittenOnEveryTransition(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	st, err := ReadStatus(h.cfg.Paths.StatusJSON)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateBooting {
		t.Errorf("state = %q, want %q", st.State, StateBooting)
	}
	if st.Instance != "pz-main" || st.Unit != "gvm@pz-main.service" {
		t.Errorf("identity = %+v", st)
	}
	if st.ManifestSHA256 != "1b40c9de" {
		t.Errorf("manifest sha = %q; a status that cannot name the pinned bytes cannot prove them", st.ManifestSHA256)
	}
	if st.Process.PID != h.proc.PID() || st.Process.StartTicks == 0 {
		t.Errorf("process = %+v, want the pinned identity", st.Process)
	}
	if st.Process.GamePID != 0 {
		t.Error("game_pid is not populated yet and must not be filled with the launcher's pid")
	}
	if st.Console.Mode != spec.ConsoleModePTY {
		t.Errorf("console mode = %q", st.Console.Mode)
	}
	// A game with no channel reports kind "none", which is the intended honest
	// answer rather than a gap: an absent block would read as "the runner has
	// not got there yet".
	if st.Control.Kind != spec.ControlNone {
		t.Errorf("control kind = %q, want %q", st.Control.Kind, spec.ControlNone)
	}
	if st.Players != nil {
		t.Error("players must be null until something has actually asked")
	}
	if st.Shutdown.InProgress {
		t.Error("no shutdown is running")
	}
}

func TestStatusTracksReadyAndPlayers(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	h.SetState(StateReady)
	at := h.clock.Now()
	h.SetPlayers(&PlayersStatus{Online: 3, Max: 8, At: at})
	h.WriteStatus()

	st, err := ReadStatus(h.cfg.Paths.StatusJSON)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadyAt.IsZero() || !st.ReadyAt.Equal(h.ReadyAt()) {
		t.Errorf("ready_at = %v, want %v", st.ReadyAt, h.ReadyAt())
	}
	if st.Players == nil || st.Players.Online != 3 || st.Players.Max != 8 {
		t.Errorf("players = %+v", st.Players)
	}

	// Zero and absent are different answers, and a dashboard renders them
	// differently: "0 online" versus "this game cannot be asked".
	h.SetPlayers(&PlayersStatus{Online: 0, At: at})
	h.WriteStatus()
	st, err = ReadStatus(h.cfg.Paths.StatusJSON)
	if err != nil {
		t.Fatal(err)
	}
	if st.Players == nil || st.Players.Online != 0 {
		t.Errorf("players = %+v, want an explicit zero", st.Players)
	}
}

func TestStatusBackendReturnsTheSameDocument(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	raw, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Proto != StatusProto || st.Instance != "pz-main" {
		t.Errorf("status = %+v", st)
	}
}

func TestStatusLoopRewritesOnTheClock(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The console log file already holds a flush timer on this clock, so the
	// wait has to be for one MORE timer rather than for any timer at all.
	before := h.clock.Timers()
	go h.StatusLoop(ctx)

	settle(t, "the status loop to arm its timer", func() bool { return h.clock.Timers() > before })
	h.Note("-- gvm: a line the file has not seen yet")
	want := h.Ring().Head()

	// Advanced inside the poll rather than once, because that is what the loop
	// actually does: it rewrites every StatusInterval, so a write lost to a
	// reader holding the file open (which happens on Windows, where rename
	// cannot replace an open file) is picked up by the next tick instead of
	// failing the run.
	settleEvery(t, "a rewritten status file", 5*time.Millisecond, func() bool {
		if st, err := ReadStatus(h.cfg.Paths.StatusJSON); err == nil && st.Console.Head >= want {
			return true
		}
		if h.clock.Timers() > before {
			h.clock.Advance(StatusInterval)
		}
		return false
	})
}

func TestReadStatusRefusesAnUnknownProto(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/status.json"
	if err := writeFileAtomic(p, []byte(`{"proto":99,"instance":"pz-main"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatus(p); err == nil || !strings.Contains(err.Error(), "proto 99") {
		t.Fatalf("err = %v, want a refusal naming the generation", err)
	}
}

// TestShutdownStatusNarratesTheRung. An operator watching a ninety second save
// wants to know which rung is running, not a spinner.
func TestShutdownStatusNarratesTheRung(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	h.stopStep(string(RungConsole), "save")

	st, err := ReadStatus(h.cfg.Paths.StatusJSON)
	if err != nil {
		t.Fatal(err)
	}
	if st.Shutdown.Rung != RungConsole {
		t.Errorf("rung = %q, want %q", st.Shutdown.Rung, RungConsole)
	}
}

func TestStatusReportsExtendedTime(t *testing.T) {
	h := newHarness(t)
	h.extendedUsec.Add((90 * time.Second).Microseconds())
	st := h.Snapshot()
	if st.Readiness.ExtendedUsecTotal != 90_000_000 {
		t.Errorf("extended = %d, want 90000000: this is the answer to "+
			"'why did systemd wait eleven minutes'", st.Readiness.ExtendedUsecTotal)
	}
}
