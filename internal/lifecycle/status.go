package lifecycle

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status is one instance, flattened for a human or for `--json`.
//
// It is a projection of Observation and Verdict rather than a second source of
// truth: every field here is copied out of one of them, and nothing is
// computed that Derive did not already decide. That is why `gvm status` and
// `gvm list` cannot disagree with `gvm doctor` about a phase -- there is one
// table, and this is a view of its answer.
//
// The JSON field names are the ones section 14.4 uses for the mirrored
// columns, so a dashboard reading this and a dashboard reading instance_status
// see the same keys.
type Status struct {
	Name  string `json:"instance"`
	Game  string `json:"game,omitempty"`
	Phase Phase  `json:"phase"`
	Rule  Rule   `json:"phase_rule"`
	// Reason and Hint are the two halves of the sentence an operator reads.
	Reason string `json:"phase_reason,omitempty"`
	Hint   string `json:"hint,omitempty"`

	Enabled bool `json:"enabled"`
	Held    bool `json:"held"`
	Ready   bool `json:"ready"`

	Unit        string `json:"unit"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`
	UnitResult  string `json:"unit_result,omitempty"`
	// UnitKnown is false when the service manager did not answer. A consumer
	// that printed ActiveState without checking this would render an empty
	// string as "not running".
	UnitKnown bool `json:"unit_known"`

	MainPID   int       `json:"main_pid,omitempty"`
	GamePID   int       `json:"game_pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	ReadyAt   time.Time `json:"ready_at,omitzero"`

	Players    *int `json:"players_online,omitempty"`
	PlayersMax int  `json:"players_max,omitempty"`

	ControlKind  string `json:"control_kind,omitempty"`
	ControlReady bool   `json:"control_ready"`

	// Lease is the job holding the instance, if any, as one line.
	Lease      string     `json:"lease,omitempty"`
	LeaseKind  JobKind    `json:"lease_kind,omitempty"`
	LeaseState LeaseState `json:"lease_state,omitempty"`

	// Drift is section 20's INST-005: the spec has been edited and not
	// applied. It is a first-class field because it is the single most common
	// reason a box does not behave the way the YAML says it should.
	Drift       bool   `json:"drift"`
	SpecSHA256  string `json:"spec_sha256,omitempty"`
	AppliedSHA  string `json:"applied_sha256,omitempty"`
	ManifestRev int    `json:"manifest_revision,omitempty"`
	PinOK       bool   `json:"pin_ok"`

	Findings []Finding `json:"findings,omitempty"`
	Problems []Problem `json:"problems,omitempty"`

	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// NewStatus projects an observation and its verdict.
func NewStatus(o Observation, v Verdict) Status {
	s := Status{
		Name:        o.Name,
		Game:        o.Game,
		Phase:       v.Phase,
		Rule:        v.Rule,
		Reason:      v.Reason,
		Hint:        v.Hint,
		Enabled:     o.Enabled,
		Held:        o.Hold,
		Unit:        unitName(o.Name),
		UnitKnown:   o.UnitChecked,
		LeaseState:  o.LeaseState,
		SpecSHA256:  o.SpecSHA256,
		AppliedSHA:  o.DropIn.SpecSHA256,
		ManifestRev: o.Pin.Revision,
		PinOK:       o.Pin.OK,
		Findings:    Findings(o, v),
		Problems:    o.Problems,
		ObservedAt:  o.Now,
	}
	if o.UnitChecked {
		s.ActiveState = o.Unit.ActiveState
		s.SubState = o.Unit.SubState
		s.UnitResult = o.Unit.Result
		s.MainPID = o.Unit.MainPID
	}
	// Drift is only meaningful once something has been applied. An unapplied
	// spec is not "drifted", it is "not applied", and conflating them makes
	// the drift count on a fresh box equal to the number of instances.
	if o.DropIn.SpecSHA256 != "" && o.SpecSHA256 != "" {
		s.Drift = !strings.EqualFold(o.DropIn.SpecSHA256, o.SpecSHA256)
	}
	if st := o.Status; st != nil {
		s.Ready = v.Phase == PhaseRunning
		s.StartedAt = st.StartedAt
		s.ReadyAt = st.ReadyAt
		s.GamePID = st.Process.GamePID
		if s.MainPID == 0 {
			s.MainPID = st.Process.PID
		}
		s.ControlKind = string(st.Control.Kind)
		s.ControlReady = st.Control.Ready
		if p := st.Players; p != nil {
			n := p.Online
			s.Players = &n
			s.PlayersMax = p.Max
		}
	}
	if o.Lease != nil {
		s.Lease = o.Lease.Describe(o.Now)
		s.LeaseKind = o.Lease.Kind
	}
	return s
}

// Line is one row of `gvm list`.
//
// Fixed-width so a column of them aligns, and the phase comes before the
// detail because the phase is what the eye scans for.
func (s Status) Line() string {
	var b strings.Builder
	b.WriteString(pad(s.Name, 20))
	b.WriteString(" ")
	b.WriteString(pad(string(s.Phase), 22))
	b.WriteString(" ")
	b.WriteString(pad(s.Game, 16))
	b.WriteString(" ")
	b.WriteString(s.short())
	return strings.TrimRight(b.String(), " ")
}

// short is the rightmost column: the one fact that matters most for this
// phase. A running server's is its player count; a failed one's is why.
func (s Status) short() string {
	switch s.Phase {
	case PhaseRunning:
		var parts []string
		if s.Players != nil {
			p := strconv.Itoa(*s.Players) + " players"
			if s.PlayersMax > 0 {
				p = strconv.Itoa(*s.Players) + "/" + strconv.Itoa(s.PlayersMax) + " players"
			}
			parts = append(parts, p)
		}
		if s.MainPID > 0 {
			parts = append(parts, "pid "+strconv.Itoa(s.MainPID))
		}
		if s.Drift {
			// Deliberately visible on a HEALTHY row. A drifted instance that
			// is running fine is the one an operator will not otherwise
			// notice until a restart applies a config they forgot they wrote.
			parts = append(parts, "spec edited, not applied")
		}
		if len(parts) == 0 {
			return s.Reason
		}
		return strings.Join(parts, ", ")
	default:
		return s.Reason
	}
}

// Report renders the multi-line `gvm status <name>` block.
func (s Status) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", s.Name, s.Phase)
	if s.Reason != "" {
		fmt.Fprintf(&b, "  %s\n", s.Reason)
	}
	if s.Hint != "" {
		fmt.Fprintf(&b, "  -> %s\n", s.Hint)
	}
	fmt.Fprintf(&b, "  unit    %s", s.Unit)
	if s.UnitKnown {
		fmt.Fprintf(&b, "  %s (%s)", s.ActiveState, s.SubState)
	} else {
		// Never blank and never "inactive": the manager did not answer, and
		// saying so is the difference between a stopped server and an
		// unreachable one.
		b.WriteString("  not checked on this host")
	}
	b.WriteString("\n")
	if s.MainPID > 0 {
		fmt.Fprintf(&b, "  pid     %d", s.MainPID)
		if s.GamePID > 0 && s.GamePID != s.MainPID {
			fmt.Fprintf(&b, " (game %d)", s.GamePID)
		}
		b.WriteString("\n")
	}
	if !s.ReadyAt.IsZero() {
		fmt.Fprintf(&b, "  ready   %s\n", s.ReadyAt.UTC().Format(time.RFC3339))
	}
	if s.Players != nil {
		fmt.Fprintf(&b, "  players %d", *s.Players)
		if s.PlayersMax > 0 {
			fmt.Fprintf(&b, " of %d", s.PlayersMax)
		}
		b.WriteString("\n")
	}
	if s.Lease != "" {
		fmt.Fprintf(&b, "  lease   %s [%s]\n", s.Lease, s.LeaseState)
	}
	if s.Drift {
		fmt.Fprintf(&b, "  drift   spec %s != applied %s\n", short(s.SpecSHA256), short(s.AppliedSHA))
	}
	for _, f := range s.Findings {
		if f.Kind == FindingPhase {
			// Already printed as the reason line; repeating it verbatim
			// under a different heading reads like two separate problems.
			continue
		}
		fmt.Fprintf(&b, "  %-7s %s\n", severityMark(f.Severity), f.Detail)
		if f.Hint != "" {
			fmt.Fprintf(&b, "          -> %s\n", f.Hint)
		}
	}
	return b.String()
}

// severityMark is section 20's severity split: problems, warnings, notes.
// Keeping the notes visually distinct is what stops the problem count from
// being permanently non-zero and therefore ignored.
func severityMark(s Severity) string {
	switch s {
	case SevOK:
		return "ok"
	case SevProblem:
		return "problem"
	}
	return "note"
}

// Summary is the one line that follows a `gvm list`.
//
// Busy and Stopped are split because "it is in the middle of something" and "it
// is deliberately off" are different answers to "why is it not running": a box
// with three servers starting is not a box with three servers an operator
// turned off, and a summary that collapsed the two would read the same for both.
type Summary struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Busy     int `json:"busy"`
	Stopped  int `json:"stopped"`
	Problems int `json:"problems"`
}

// Summarise counts a set of statuses into the four buckets `gvm list` reports.
// Every status lands in exactly one: running, needs-a-person (SevProblem),
// in-flight (Transitional), or otherwise not running (stopped, declared).
func Summarise(ss []Status) Summary {
	var s Summary
	s.Total = len(ss)
	for _, st := range ss {
		switch {
		case st.Phase == PhaseRunning:
			s.Running++
		case st.Phase.Severity() == SevProblem:
			s.Problems++
		case st.Phase.Transitional():
			s.Busy++
		default:
			s.Stopped++
		}
	}
	return s
}

func (s Summary) String() string {
	return fmt.Sprintf("%d running, %d busy, %d stopped, %d needing attention",
		s.Running, s.Busy, s.Stopped, s.Problems)
}

// Statuses projects a whole sweep, sorted by name.
func Statuses(r SweepReport) []Status {
	out := make([]Status, 0, len(r.Instances))
	for _, ir := range r.Instances {
		out = append(out, NewStatus(ir.Observation, ir.Verdict))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
