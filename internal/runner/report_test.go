package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestMillisMarshalsAsMilliseconds(t *testing.T) {
	rep := &Report{Duration: MillisOf(40 * time.Second)}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"duration_ms":40000`) {
		t.Fatalf("json = %s\nwant duration_ms in milliseconds, not nanoseconds", b)
	}
	if got := MillisOf(1500 * time.Millisecond).Duration(); got != 1500*time.Millisecond {
		t.Errorf("round trip = %s", got)
	}
}

func TestRungFor(t *testing.T) {
	for via, want := range map[string]Rung{
		spec.ViaConsole:  RungConsole,
		spec.ViaControl:  RungControl,
		spec.ViaSignal:   RungSignal,
		spec.ViaWaitExit: RungWaitExit,
	} {
		got, err := RungFor(via)
		if err != nil || got != want {
			t.Errorf("RungFor(%q) = %q, %v; want %q", via, got, err, want)
		}
	}
	if _, err := RungFor("telnet"); err == nil {
		t.Error("RungFor accepted a channel that does not exist")
	}
	// Both SIGKILL rungs are degraded and nothing else is: reaching either
	// means the world was ended rather than asked to save.
	for _, r := range []Rung{RungSIGKILL, RungCgroup} {
		if !r.Degrades() {
			t.Errorf("%q must be degraded", r)
		}
	}
	for _, r := range []Rung{RungNone, RungConsole, RungControl, RungSignal, RungWaitExit, RungSIGTERM} {
		if r.Degrades() {
			t.Errorf("%q must not be degraded", r)
		}
	}
}

// TestAppendReportIsOneLinePerRecord. gvmd reads this file by byte offset
// while the runner is still writing it, so a record has to arrive whole.
func TestAppendReportIsOneLinePerRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "shutdown.jsonl")
	for i := range 3 {
		rep := &Report{Instance: "pz-main", Reason: "test", FinalRung: RungConsole, ExitCode: i}
		if err := AppendReport(p, rep); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rep Report
		if err := json.Unmarshal(sc.Bytes(), &rep); err != nil {
			t.Fatalf("line %d is not a whole record: %v", n, err)
		}
		if rep.Proto != ReportProto || rep.ExitCode != n {
			t.Errorf("line %d = %+v", n, rep)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("got %d records, want 3", n)
	}
}

func TestSaveWritesBothPlaces(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "shutdown.jsonl")
	last := filepath.Join(dir, "last-shutdown.json")

	rep := &Report{Instance: "pz-main", FinalRung: RungSIGKILL, Degraded: true, ExitCode: -1}
	if err := rep.Save(jsonl, last); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastShutdown(last)
	if err != nil {
		t.Fatal(err)
	}
	if got.FinalRung != RungSIGKILL || !got.Degraded {
		t.Errorf("last-shutdown = %+v", got)
	}
	if _, err := os.Stat(jsonl); err != nil {
		t.Errorf("the journal is the durable copy and must exist: %v", err)
	}
}

func TestTailFromMarksActorLines(t *testing.T) {
	f := console.NewFake(console.FakeOptions{PartialAfter: -1})
	t.Cleanup(func() { _ = f.Close() })

	f.Emit("game says hello")
	if _, err := f.Send("user:alice", "quit"); err != nil {
		t.Fatal(err)
	}
	f.Note("-- gvm: sigterm")

	got := tailFrom(f.Ring)
	want := []string{"game says hello", "> quit", "-- gvm: sigterm"}
	if len(got) != len(want) {
		t.Fatalf("tail = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tail[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if tailFrom(nil) != nil {
		t.Error("a nil ring has no tail")
	}
}
