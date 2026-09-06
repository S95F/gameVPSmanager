package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
)

// Host is the privileged surface, and it is exactly hostops.Do.
//
// One method, for the same reason internal/reconcile declares the same
// interface: there is no second method through which a privileged operation
// could be reached, so the enumerable surface stays enumerable no matter what
// this package grows into.
type Host interface {
	Do(ctx context.Context, req hostops.Request) (hostops.Result, error)
}

// ErrNoLadder is a stop with no way to learn which rung ended it.
var ErrNoLadder = errors.New("lifecycle: a stop needs a way to observe the shutdown ladder")

// Outcome is what the shutdown ladder did (section 11.6).
type Outcome struct {
	// Rung is the ladder step that actually ended the process: "console",
	// "signal", "sigterm", "sigkill". It is the field that turns "the server
	// stopped" into "the server SAVED", and audit.InstanceStop requires it on
	// its success row for exactly that reason.
	Rung string `json:"rung"`
	// Degraded is true when a SIGKILL was needed. Section 11.6: either
	// SIGKILL rung always sets it.
	Degraded bool `json:"degraded"`
	// Took is how long the ladder ran.
	Took time.Duration `json:"took"`
}

// ShutdownFunc runs an instance's shutdown ladder and reports the rung it
// ended on.
//
// It is a seam and not an implementation because the production one is the
// console socket's uid-0 `stop` operation (section 11.4): the runner walks the
// rungs, streams each one, and answers when the game's process group is gone.
// That is a socket client, and putting it here would make every process that
// only wants to READ a phase link the console protocol.
//
// Manager.Stop REFUSES when this is nil, and that refusal is deliberate rather
// than a convenience left unimplemented. audit.InstanceStop requires `rung` on
// its success row; a stop issued without one writes an audit row that says a
// world was stopped and cannot say whether it was saved first, which is
// precisely the record the bash suite already produced and the reason this
// field exists at all.
type ShutdownFunc func(ctx context.Context, name string, budget time.Duration) (Outcome, error)

// Request is one operator action.
type Request struct {
	Verb     Verb
	Instance string
	Actor    audit.Actor
	// Reason is what the operator typed. It reaches the audit row and, for a
	// stop, the hold file -- so the next person to wonder why a server is down
	// finds the answer next to the evidence.
	Reason string
	// Budget bounds the shutdown ladder. Zero uses the instance's own
	// shutdown.budget, which is what the rendered ladder already carries.
	Budget time.Duration
	// Force overrides an overridable refusal. It never overrides one whose
	// Refusal.Force is false.
	Force     bool
	RequestID string
}

// Step is one privileged call the manager made.
type Step struct {
	Verb    hostops.Verb `json:"verb"`
	Path    string       `json:"path,omitempty"`
	Unit    string       `json:"unit,omitempty"`
	Changed bool         `json:"changed"`
}

// Result is what a verb did.
type Result struct {
	Verb     Verb     `json:"verb"`
	Instance string   `json:"instance"`
	Before   Verdict  `json:"before"`
	After    Verdict  `json:"after"`
	Steps    []Step   `json:"steps,omitempty"`
	Shutdown *Outcome `json:"shutdown,omitempty"`
	// Forced records that a refusal was overridden. It is on the result and
	// not only in the log because a `gvm status` taken a minute later should
	// be able to say the instance was started over gvm's objection.
	Forced bool `json:"forced,omitempty"`
	// Changed is whether the host is not what it was.
	Changed bool `json:"changed"`
}

// Manager executes lifecycle verbs.
//
// It is a DIFFERENT TYPE from Reconciler and that separation is the point of
// both. Manager actuates, always on an operator's behalf, always with an
// audit.Actor, always after a precondition check the operator can read and
// override. Reconciler observes and cannot actuate at all. Merging them would
// give the sweep a start method, and something with a start method eventually
// calls it.
type Manager struct {
	// Obs is how the manager learns the state it is about to change. It is
	// the same Observer the reconciler uses, so a refusal and a `gvm status`
	// taken a second earlier cannot disagree about what is true.
	Obs *Observer
	// Host is the privileged surface. Required.
	Host Host
	// Shutdown runs the ladder. Required for VerbStop; see ShutdownFunc.
	Shutdown ShutdownFunc
	// Now is the clock, for the hold file's timestamp.
	Now func() time.Time
}

func (m *Manager) obs() *Observer {
	if m.Obs != nil {
		return m.Obs
	}
	return &Observer{}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Do runs one verb.
//
// The shape is observe, decide, act, observe again. The second observation is
// not decoration: hostops waits for the systemd job to complete before
// returning, so by the time the steps are done there is a real answer to "and
// what is it now", and a caller that had to poll for it would either poll too
// early and report the old phase or sleep and be wrong on a slow box.
func (m *Manager) Do(ctx context.Context, req Request) (*Result, error) {
	d, ok := decls[req.Verb]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownVerb, req.Verb)
	}
	if err := instance.ValidateName(req.Instance); err != nil {
		return nil, err
	}
	if m.Host == nil {
		return nil, errors.New("lifecycle: no privileged surface configured")
	}
	if req.Verb == VerbStop && m.Shutdown == nil {
		return nil, ErrNoLadder
	}

	obs, err := m.obs().Observe(ctx, req.Instance)
	if err != nil {
		return nil, err
	}
	res := &Result{Verb: req.Verb, Instance: req.Instance, Before: Derive(obs)}

	if err := Allowed(req.Verb, obs, res.Before); err != nil {
		var ref *Refusal
		if !req.Force || !errors.As(err, &ref) || !ref.Force {
			return res, err
		}
		res.Forced = true
	}

	if err := m.execute(ctx, d, req, res); err != nil {
		return res, err
	}

	after, err := m.obs().Observe(ctx, req.Instance)
	if err != nil {
		return res, err
	}
	res.After = Derive(after)
	return res, nil
}

// execute performs the verb's steps in order.
//
// The hold ordering is the only thing here that is not obvious, and it comes
// from a systemd detail with teeth. The packaged gvm@.service declares
// RuntimeDirectoryPreserve=restart, so /run/gvm/<name> -- and everything in
// it, the hold included -- is REMOVED by a clean stop and kept across a
// restart. A hold written only before the stop would therefore be deleted by
// the stop it exists to survive, and `gvm-guard.timer` would bring the
// instance back within five minutes.
//
// So a stop writes it twice: before, so that a crash or a power loss in the
// middle still leaves the marker; and after, because the successful path
// deletes it. The second write recreates the directory (the file kind declares
// a ParentMode for exactly this), and `gvm run-prepare` restores its setgid
// ownership as ExecStartPre before the next start puts anything else in it.
func (m *Manager) execute(ctx context.Context, d Decl, req Request, res *Result) error {
	if d.Hold == HoldClear {
		// Cleared FIRST. If the start then fails, the guard timer picks the
		// instance up within five minutes; a hold left behind by a failed
		// start would suppress exactly that recovery.
		if err := m.step(ctx, res, hostops.Request{
			Verb: hostops.VerbFileRemove, Actor: req.Actor, Instance: req.Instance,
			File: hostops.FileHold, RequestID: req.RequestID,
		}); err != nil {
			return err
		}
	}

	var params audit.Params
	if d.Hold == HoldSet {
		body := FormatHold(m.now(), req.Actor, req.Reason)
		if err := m.step(ctx, res, hostops.Request{
			Verb: hostops.VerbFileWrite, Actor: req.Actor, Instance: req.Instance,
			File: hostops.FileHold, Body: body, RequestID: req.RequestID,
		}); err != nil {
			return err
		}
		out, err := m.Shutdown(ctx, req.Instance, req.Budget)
		if err != nil {
			return err
		}
		res.Shutdown = &out
		// The allowlist for instance.stop is reason, budget_sec, rung,
		// degraded, and rung is REQUIRED on the success row. Assembling it
		// here rather than inside hostops is the split working as intended:
		// hostops knows how to stop a unit and has no way to know how the
		// world was saved.
		params = audit.Params{"rung": out.Rung, "degraded": out.Degraded}
		if req.Reason != "" {
			params["reason"] = req.Reason
		}
		if req.Budget > 0 {
			params["budget_sec"] = int(req.Budget.Seconds())
		}
	} else if req.Reason != "" {
		params = audit.Params{"reason": req.Reason}
	}

	if err := m.step(ctx, res, hostops.Request{
		Verb: d.Host, Actor: req.Actor, Instance: req.Instance,
		Params: params, RequestID: req.RequestID,
	}); err != nil {
		return err
	}

	if d.Hold == HoldSet {
		// The stop took the runtime directory with it. Write the marker again.
		if err := m.step(ctx, res, hostops.Request{
			Verb: hostops.VerbFileWrite, Actor: req.Actor, Instance: req.Instance,
			File: hostops.FileHold, Body: FormatHold(m.now(), req.Actor, req.Reason),
			RequestID: req.RequestID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) step(ctx context.Context, res *Result, req hostops.Request) error {
	out, err := m.Host.Do(ctx, req)
	res.Steps = append(res.Steps, Step{
		Verb: req.Verb, Path: out.Path, Unit: out.Unit, Changed: out.Changed,
	})
	if out.Changed {
		res.Changed = true
	}
	return err
}

// FormatHold renders /run/gvm/<name>/hold.
//
// One line, human first: an operator who finds it with `cat` gets the whole
// story, and section 13.4 asks for the actor and the reason specifically. The
// reason is operator-typed and reaches a file, so every control character is
// stripped -- not because the file is parsed by anything that would be fooled,
// but because a newline in it would make the second line look like a second
// record to whatever eventually does parse it.
func FormatHold(at time.Time, actor audit.Actor, reason string) []byte {
	who := string(actor.Kind)
	if actor.UserID != "" {
		who += ":" + actor.UserID
	}
	line := at.UTC().Format(time.RFC3339) + " " + who
	if r := sanitiseReason(reason); r != "" {
		line += " " + r
	}
	return []byte(line + "\n")
}

func sanitiseReason(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Dropped rather than escaped: an escape sequence in a file an
			// operator will `cat` is a terminal control sequence in their
			// terminal.
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
