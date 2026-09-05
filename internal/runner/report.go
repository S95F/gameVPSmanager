package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Millis is a duration that marshals as whole milliseconds.
//
// It exists because the JSON field is named `duration_ms` and a time.Duration
// marshals as NANOseconds, so the obvious struct would publish 40000000000
// under a name that says milliseconds. gvmd ingests these records into a
// column it will later average, and a units bug in a column nobody re-derives
// is a wrong number that outlives the code.
type Millis int64

// MillisOf truncates d to whole milliseconds.
func MillisOf(d time.Duration) Millis { return Millis(d / time.Millisecond) }

// Duration returns the wrapped value.
func (m Millis) Duration() time.Duration { return time.Duration(m) * time.Millisecond }

func (m Millis) String() string { return m.Duration().String() }

// Rung names one attempted step of the shutdown ladder.
//
// The four declared rungs are the manifest's `via:` values; the last three are
// implicit and cannot be written in a manifest (section 11.6 rule 7). The rung
// that ended the process is the single most valuable field in the report: it
// is the difference between "saved and exited at 40s" and "hung and got
// SIGKILLed at 180s", which the screen-based unit could never tell apart
// (section 1.2 bug 1).
type Rung string

const (
	// RungNone means no rung was needed: the game had already exited when the
	// ladder started. Not degraded -- there was nothing left to do wrong.
	RungNone     Rung = "none"
	RungConsole  Rung = "console"
	RungControl  Rung = "control"
	RungSignal   Rung = "signal"
	RungWaitExit Rung = "wait_exit"
	// RungSIGTERM is the first implicit rung: SIGTERM to the process group.
	RungSIGTERM Rung = "sigterm"
	// RungSIGKILL is SIGKILL to the process group, and is ALWAYS degraded.
	RungSIGKILL Rung = "sigkill"
	// RungCgroup is the last implicit rung: SIGKILL to every pid still in the
	// unit's cgroup, for an engine that escaped its group by calling setsid.
	// Section 11.6 rule 7 lists it without naming it; it is degraded too.
	RungCgroup Rung = "sigkill_cgroup"
)

// RungFor maps a manifest `via:` onto the rung it produces.
func RungFor(via string) (Rung, error) {
	switch via {
	case spec.ViaConsole:
		return RungConsole, nil
	case spec.ViaControl:
		return RungControl, nil
	case spec.ViaSignal:
		return RungSignal, nil
	case spec.ViaWaitExit:
		return RungWaitExit, nil
	}
	return "", fmt.Errorf("runner: %q is not a shutdown channel (want %s|%s|%s|%s)",
		via, spec.ViaConsole, spec.ViaControl, spec.ViaSignal, spec.ViaWaitExit)
}

// Degrades reports whether reaching this rung means the world was not stopped
// cleanly. Both SIGKILL rungs do, per section 11.6 rule 7.
func (r Rung) Degrades() bool { return r == RungSIGKILL || r == RungCgroup }

// A declared `via: signal` step is reported as final_rung "signal" whatever
// signal it carried, so a reader cannot answer this question from the rung
// name alone; the ladder decides that one from the signal. See
// degradesBySignal.

// Outcome is what one rung achieved.
//
// `timeout` and `ok` are deliberately different values even though both let the
// ladder advance, because the whole point of `wait_for_log` is that "saved at
// 4s" and "waited the full 30s and gave up" must not look the same in the
// record (section 8, wait_for_log semantics).
type Outcome string

const (
	// OutcomeOK: the rung did what it was asked. For a step with a
	// wait_for_log that means the marker appeared.
	OutcomeOK Outcome = "ok"
	// OutcomeExited: the game exited during this rung. The terminal success.
	OutcomeExited Outcome = "exited"
	// OutcomeTimeout: the wait elapsed without the marker and without an exit.
	// The ladder continues (`on_timeout: continue` is the default) or jumps to
	// the implicit rungs (`escalate`).
	OutcomeTimeout Outcome = "timeout"
	// OutcomeSkipped: the rung could not be attempted at all -- no channel for
	// it, or no budget left.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeFailed: the attempt was made and errored.
	OutcomeFailed Outcome = "failed"
)

// StepResult is one rung, attempted, with the evidence.
//
// Every field here answers a question an operator asks at 2am in the order
// they ask it: what did gvm send, when, how long did it take, did the game
// acknowledge it, and was the game still there afterwards.
type StepResult struct {
	// Index is the manifest step's position, or -1 for an implicit rung.
	Index int    `json:"index"`
	Rung  Rung   `json:"rung"`
	Via   string `json:"via,omitempty"`
	// Send is the command as delivered, or spec.Redacted when the template
	// carried a secret.
	Send   string `json:"send,omitempty"`
	Signal string `json:"signal,omitempty"`
	Scope  string `json:"scope,omitempty"`

	// WaitForLog is the marker pattern, and SinceSeq is the ring cursor the
	// match had to beat. SinceSeq is in the record because it is the evidence
	// for section 11.6 rule 1: a match at or below it would have been a stale
	// line from a previous save.
	WaitForLog string `json:"wait_for_log,omitempty"`
	SinceSeq   uint64 `json:"since_seq,omitempty"`
	// MatchedSeq and Matched are the line that satisfied the wait.
	MatchedSeq uint64 `json:"matched_seq,omitempty"`
	Matched    string `json:"matched,omitempty"`
	// EchoSeq is the ring position of a console step's echo, so the report
	// points into the console log at the exact moment the command ran.
	EchoSeq uint64 `json:"echo_seq,omitempty"`
	// Output is a control step's reply.
	Output string `json:"output,omitempty"`

	// Budget is the step's declared `wait`.
	Budget    Millis    `json:"budget_ms,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Duration  Millis    `json:"duration_ms"`

	Outcome Outcome `json:"outcome"`
	// Note explains an outcome that is not self-evident: a tolerated
	// disconnect, a skipped rung, a wait cut short by the budget.
	Note string `json:"note,omitempty"`
	Err  string `json:"error,omitempty"`
	// Alive is whether the game was still there when this rung finished.
	Alive bool `json:"alive"`
}

// ReportProto is the schema version of a shutdown record. gvmd ingests this
// file by byte offset and must be able to refuse a generation it does not
// understand rather than half-parse it.
const ReportProto = 1

// Report is the record of one shutdown, and it is the product feature.
//
// The bash suite's `ExecStop=screen -X stuff 'quit\015'` returned immediately
// and left no evidence at all: a world that saved in 40 s and a JVM that hung
// until systemd's SIGKILL at 180 s produced byte-identical output in
// `systemctl status`. Every field below exists so those two are never confused
// again -- and so a nightly restart that has quietly been SIGKILLing the world
// for three weeks is visible in a query rather than in a corrupt save.
type Report struct {
	Proto    int    `json:"proto"`
	Instance string `json:"instance"`
	Game     string `json:"game,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   Millis    `json:"duration_ms"`

	Steps     []StepResult `json:"steps"`
	FinalRung Rung         `json:"final_rung"`
	Degraded  bool         `json:"degraded"`

	// ExitCode and Signal are how the game ended. Both are carried because
	// "exit_code: -1" tells an operator nothing and "signal: SIGKILL" tells
	// them everything.
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	// Exited is false when the ladder finished with the game still running,
	// which is the case an exit code cannot express.
	Exited bool `json:"exited"`

	// ExtendedUsec is the total EXTEND_TIMEOUT_USEC this runner asked systemd
	// for, across readiness and this shutdown. It is the answer to "why did
	// systemd wait eleven minutes for a unit with TimeoutStopSec=360".
	ExtendedUsec int64 `json:"extended_usec"`

	// ConsoleTail is the last 30 console lines, ALWAYS (section 11.6 rule 8),
	// mirroring the bash suite's `tail -n 30` on early exit. It is the thing
	// you actually want at 2am, and it is in the record because by the time
	// anyone reads this the ring is gone with the process.
	ConsoleTail []string `json:"console_tail"`

	// Standalone marks a report written by `gvm run-shutdown` (section 11.8
	// layer 3), where there is no ring and no socket. Without the flag, an
	// empty console_tail would be indistinguishable from a game that printed
	// nothing on the way out.
	Standalone bool `json:"standalone,omitempty"`

	// Err is a failure of the LADDER itself rather than of a rung -- the
	// runner panicked, or the process could not be adopted.
	Err string `json:"error,omitempty"`
}

// ConsoleTailLines is how many console lines a report carries.
const ConsoleTailLines = 30

// tailFrom renders the last ConsoleTailLines of the ring for the report.
//
// Actor lines are prefixed "> " because a shutdown report whose tail contains
// the word `quit` should say whether the game printed it or somebody typed it.
// Runner annotations already carry their own "-- gvm:" prefix.
func tailFrom(r *console.Ring) []string {
	if r == nil {
		return nil
	}
	lines, _, _ := r.Tail(ConsoleTailLines)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.Source == console.SourceActor {
			out = append(out, "> "+l.Text)
			continue
		}
		out = append(out, l.Text)
	}
	return out
}

// Save writes the report to both places section 11.6 rule 9 names: appended to
// the append-only JSONL that gvmd ingests by byte offset, and replacing
// last-shutdown.json in the runtime directory.
//
// The JSONL goes first. It is the durable one -- gvmd tails it, it survives a
// reboot, and it is the record a post-mortem reads -- while last-shutdown.json
// lives in /run and is gone the moment the box restarts. If only one of them
// can be written, it must be that one.
//
// The runner never writes to Postgres, and that is not an oversight: it has no
// DB grant, and putting a database on the path a world takes to stop means a
// full disk on the database host can prevent a world from being saved.
func (rep *Report) Save(jsonlPath, lastPath string) error {
	var errs []error
	if jsonlPath != "" {
		if err := AppendReport(jsonlPath, rep); err != nil {
			errs = append(errs, err)
		}
	}
	if lastPath != "" {
		if err := WriteLastShutdown(lastPath, rep); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MarshalLine renders the report as one NDJSON line, terminator included.
func (rep *Report) MarshalLine() ([]byte, error) {
	if rep.Proto == 0 {
		rep.Proto = ReportProto
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// AppendReport appends one record to the shutdown journal.
//
// The whole line, terminator included, goes out in ONE Write to a file opened
// O_APPEND, because gvmd reads this file by byte offset while the runner is
// still writing it. A partial line at the end of the file is a record gvmd
// would either drop or, worse, re-read as a different record after the rest
// arrives.
func AppendReport(p string, rep *Report) error {
	line, err := rep.MarshalLine()
	if err != nil {
		return err
	}
	// A missing log directory must not be the reason a shutdown goes
	// unrecorded. MkdirAll is a no-op when it already exists, and when the
	// parent is root-owned it fails -- at which point the open below fails
	// too and reports the real problem.
	_ = os.MkdirAll(filepath.Dir(p), 0o750)

	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	// Synced because the process that wrote it is about to exit and the reason
	// it is exiting may be the same one that is about to take the box down.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteLastShutdown replaces /run/gvm/<name>/last-shutdown.json.
func WriteLastShutdown(p string, rep *Report) error {
	if rep.Proto == 0 {
		rep.Proto = ReportProto
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o640)
}

// ReadLastShutdown decodes last-shutdown.json.
func ReadLastShutdown(p string) (*Report, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("runner: %s: %w", p, err)
	}
	return &rep, nil
}
