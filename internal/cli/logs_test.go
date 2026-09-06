package cli

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/instance"
)

func TestTailLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want []string
	}{
		{name: "fewer lines than asked for", in: "a\nb\n", n: 5, want: []string{"a", "b"}},
		{name: "exactly the tail", in: "a\nb\nc\n", n: 2, want: []string{"b", "c"}},
		{name: "no trailing newline still counts the last line", in: "a\nb", n: 1, want: []string{"b"}},
		{name: "empty file", in: "", n: 3, want: nil},
		{name: "n=0 means all of it", in: "a\nb\n", n: 0, want: []string{"a", "b"}},
		// A blank line in the middle of a console log is a line the game
		// printed, and dropping it would silently reflow a post-mortem.
		{name: "blank lines are lines", in: "a\n\nb\n", n: 3, want: []string{"a", "", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLines(strings.NewReader(tc.in), tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// One corrupt line must not hide the shutdowns that came before it: a torn
// write from a power loss is exactly the event somebody is reading this file
// about.
func TestParseShutdownLogKeepsGoingPastACorruptLine(t *testing.T) {
	in := `{"instance":"pz-main","final_rung":"console","degraded":false}
{ this is not json
{"instance":"pz-main","final_rung":"sigkill","degraded":true}
`
	reps, bad := parseShutdownLog(strings.NewReader(in))
	if len(reps) != 2 {
		t.Fatalf("parsed %d records, want 2", len(reps))
	}
	if len(bad) != 1 || bad[0].line != 2 {
		t.Fatalf("bad lines = %+v, want one at line 2", bad)
	}
	if !reps[1].Degraded || reps[1].FinalRung != "sigkill" {
		t.Errorf("the degraded record did not survive: %+v", reps[1])
	}
}

// `gvm logs` prints the tail and points at the journal for the runner's own
// diagnostics, because the console log deliberately does not go there.
func TestLogsPrintsTheConsoleTailAndNamesTheJournal(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	h.FS.put(instance.PathsFor("valheim-1").ConsoleLog,
		[]byte("one\ntwo\nthree\n"), 0o640)

	code, out, errb := runWith(t, h, "", "logs", "valheim-1", "-n", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if strings.Contains(out, "one\n") {
		t.Errorf("-n 2 printed three lines:\n%s", out)
	}
	for _, want := range []string{"two", "three", "journalctl -u gvm@valheim-1.service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// An instance that has never started has no console log, and saying so is not
// the same as saying the log is empty.
func TestLogsExplainsAnAbsentConsoleLog(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")

	code, _, errb := runWith(t, h, "", "logs", "valheim-1")
	if code == 0 {
		t.Fatal("logs succeeded with no log file")
	}
	if !strings.Contains(errb, "never started") {
		t.Errorf("stderr = %s", errb)
	}
}

func TestLogsShutdownPrintsTheRung(t *testing.T) {
	h := newTestHost(t)
	seedInstance(t, h, "valheim-1", "valheim", "--var", "public_name=Test")
	h.FS.put(instance.PathsFor("valheim-1").ShutdownLog,
		[]byte(`{"instance":"valheim-1","final_rung":"signal","degraded":false,"duration_ms":41000}`+"\n"),
		0o640)

	code, out, errb := runWith(t, h, "", "logs", "valheim-1", "--shutdown")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, "rung=signal") {
		t.Errorf("the rung -- the whole point of the record -- is not printed:\n%s", out)
	}
}
