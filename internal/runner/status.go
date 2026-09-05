package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/S95F/gameVPSmanager/internal/control"
)

// status.json is the DB-free source of truth (section 11.4).
//
// It is rewritten atomically every two seconds and on every transition, and it
// is the file that makes section 1's promise checkable: kill gvmd, kill
// gvm-web, kill Postgres, and `cat /run/gvm/<name>/status.json` still tells an
// operator what the server is doing. Everything in it is derived from state
// this process already holds; nothing in it requires a query, a dial or a
// lock, because a status file that can block is a status file that is missing
// exactly when it is needed.

// StatusProto is the schema version. gvmd refuses a generation it does not
// understand rather than half-reading one.
const StatusProto = 1

// StatusInterval is section 11.4's rewrite cadence.
const StatusInterval = 2 * time.Second

// Status is the whole document.
type Status struct {
	Proto          int    `json:"proto"`
	Instance       string `json:"instance"`
	Game           string `json:"game,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	Unit           string `json:"unit,omitempty"`

	State      State     `json:"state"`
	StateSince time.Time `json:"state_since"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	ReadyAt    time.Time `json:"ready_at,omitzero"`

	Process   ProcessStatus   `json:"process"`
	Console   ConsoleStatus   `json:"console"`
	Control   control.Status  `json:"control"`
	Players   *PlayersStatus  `json:"players,omitempty"`
	Readiness ReadinessStatus `json:"readiness"`
	Shutdown  ShutdownStatus  `json:"shutdown"`
}

// ProcessStatus is the pinned identity.
//
// GamePID and GameStartTicks are the engine under a launcher script and are
// left zero by this build: finding it needs a /proc walk that has not been
// written. Zero rather than a copy of PID, because a dashboard showing the
// shell's pid labelled "game" would send an operator to kill the wrong thing.
type ProcessStatus struct {
	PID            int    `json:"pid"`
	PGID           int    `json:"pgid"`
	StartTicks     uint64 `json:"start_ticks"`
	GamePID        int    `json:"game_pid,omitempty"`
	GameStartTicks uint64 `json:"game_start_ticks,omitempty"`
}

// ConsoleStatus is what a reader needs to attach to the ring or find the log.
//
// Subscribers and Connections are different numbers and both are here: a
// connection that has never sent `tail` is attached but is not following, and
// an operator debugging "the dashboard shows nothing" needs to know which of
// the two is zero.
type ConsoleStatus struct {
	Mode        string `json:"mode"`
	Head        uint64 `json:"head"`
	Oldest      uint64 `json:"oldest"`
	Log         string `json:"log,omitempty"`
	Subscribers int    `json:"subscribers"`
	Connections int    `json:"connections"`
	// BytesIn is everything the game has printed, which is the cheapest
	// possible answer to "is it doing anything at all".
	BytesIn int64 `json:"bytes_in"`
}

// PlayersStatus is the slow RCON poll's last answer.
//
// It is a pointer in Status because absent and zero are different: `null`
// means this game has no way to be asked, and `{"online": 0}` means it was
// asked and the server is empty.
type PlayersStatus struct {
	Online int       `json:"online"`
	Max    int       `json:"max,omitempty"`
	At     time.Time `json:"at,omitzero"`
	Err    string    `json:"error,omitempty"`
}

// ReadinessStatus is D-12's evidence: what is being waited for, how much extra
// time systemd has been asked for, and when the game last said anything.
type ReadinessStatus struct {
	Probe             string    `json:"probe,omitempty"`
	ExtendedUsecTotal int64     `json:"extended_usec_total"`
	LastProgress      time.Time `json:"last_progress,omitzero"`
}

// ShutdownStatus is the ladder, live.
type ShutdownStatus struct {
	InProgress bool `json:"in_progress"`
	Rung       Rung `json:"rung"`
	Degraded   bool `json:"degraded"`
}

// SetConnCounter installs the socket's open-connection count. It is a seam
// rather than a field because the socket is constructed after the runner.
func (r *Runner) SetConnCounter(f func() int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connsFn = f
}

// SetPlayers records the player-count poll's answer. A nil argument clears it
// back to "never asked".
func (r *Runner) SetPlayers(p *PlayersStatus) {
	r.mu.Lock()
	r.players = p
	r.mu.Unlock()
}

// setProbe records which readiness probe is currently being waited for.
func (r *Runner) setProbe(desc string) {
	r.mu.Lock()
	r.lastProbe = desc
	r.mu.Unlock()
	r.NotifyStatus(desc)
	r.WriteStatus()
}

// Snapshot builds the status document.
func (r *Runner) Snapshot() Status {
	r.mu.Lock()
	st := Status{
		Proto:      StatusProto,
		Instance:   r.cfg.Instance,
		Game:       r.cfg.Game,
		Unit:       r.cfg.Unit,
		State:      r.state,
		StateSince: r.stateSince,
		StartedAt:  r.startedAt,
		ReadyAt:    r.readyAt,
		Process: ProcessStatus{
			PID: r.identity.PID, PGID: r.identity.PGID, StartTicks: r.identity.StartTicks,
		},
		Players: r.players,
		Readiness: ReadinessStatus{
			Probe:             r.lastProbe,
			ExtendedUsecTotal: r.extendedUsec.Load(),
		},
		Shutdown: ShutdownStatus{
			InProgress: r.stopping.Load(),
			Rung:       r.rung,
			Degraded:   r.degraded,
		},
	}
	ctrl, conns := r.control, r.connsFn
	r.mu.Unlock()

	if r.cfg.Manifest != nil {
		st.ManifestSHA256 = r.cfg.Manifest.SHA256
	}
	// Describe answers for a nil channel too, reporting kind "none". An absent
	// control block would read as "the runner has not got there yet", which is
	// a different and much more alarming thing than "this game has no RCON".
	st.Control = control.Describe(ctrl)

	st.Console = ConsoleStatus{
		Mode:        r.launch.Console.Mode,
		Head:        r.ring.Head(),
		Oldest:      r.ring.Oldest(),
		Subscribers: r.ring.Subscribers(),
		BytesIn:     r.ingest.BytesIn(),
	}
	if r.logFile != nil {
		st.Console.Log = r.logFile.Path()
	}
	if conns != nil {
		st.Console.Connections = conns()
	}
	if lb := r.ingest.LastByte(); !lb.IsZero() {
		st.Readiness.LastProgress = lb
	}
	return st
}

// Status implements the console socket's Backend: this instance's status.json
// as bytes, exactly as it is on disk.
func (r *Runner) Status() (json.RawMessage, error) {
	return json.Marshal(r.Snapshot())
}

// WriteStatus rewrites status.json now.
//
// Failures are logged and swallowed. status.json is a mirror, and a runner
// that refused to keep supervising a live game because /run was momentarily
// unwritable would trade a stale status file for a stopped server.
func (r *Runner) WriteStatus() {
	p := r.cfg.Paths.StatusJSON
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(r.Snapshot(), "", "  ")
	if err != nil {
		r.log.Error("status.json", "err", err)
		return
	}
	if err := writeFileAtomic(p, append(b, '\n'), 0o640); err != nil {
		r.log.Error("status.json", "path", p, "err", err)
	}
}

// StatusLoop rewrites status.json every StatusInterval until ctx ends.
//
// It runs on the injected clock so a test drives it without sleeping, and it
// deliberately does no work of its own beyond the snapshot: every value it
// publishes is already computed, so a slow control channel or a stalled game
// cannot make the status file late.
func (r *Runner) StatusLoop(ctx context.Context) {
	defer r.recoverInto("status writer")
	for {
		if err := sleepOn(ctx, r.clock, StatusInterval); err != nil {
			return
		}
		r.WriteStatus()
	}
}

// ReadStatus decodes a status.json written by a runner, for gvmd and doctor.
func ReadStatus(p string) (Status, error) {
	var st Status
	b, err := os.ReadFile(p)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("runner: %s: %w", p, err)
	}
	if st.Proto != StatusProto {
		return st, fmt.Errorf("runner: %s is proto %d, this build speaks %d", p, st.Proto, StatusProto)
	}
	return st, nil
}
