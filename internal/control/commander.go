package control

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Commander turns "broadcast, with this message" into bytes on whichever
// channel can carry them, and answers honestly when neither can.
//
// The routing rule is one line of section 11.10 and the rest of this file is
// its consequences: PREFER CONTROL WHEN IT IS READY AND THE VERB HAS A CONTROL
// FORM, OTHERWISE FALL BACK TO THE CONSOLE. Control is preferred because it
// returns a reply -- `players` over RCON is a list, `players` typed into a
// console is a line of output that may or may not arrive and cannot be
// correlated with the command. The console is the fallback because it always
// exists, for the whole life of the process, on every game in the catalogue.
type Commander struct {
	m    *spec.Manifest
	inst *spec.Instance
	rend *spec.Renderer

	console ConsoleChannel
	control Channel

	rec  Recorder
	live func() spec.Live
	now  func() time.Time
}

// ConsoleChannel is the console seen as a command channel: it can say whether
// the game is accepting input and why not (Channel), and it can inject a line
// and report the Seq of the echo (Sender).
//
// Both halves are needed and neither is redundant. Without Channel the
// commander cannot explain a refusal without attempting a write to a dead pty;
// without Sender a console verb has no Seq, and the Seq is the operator's
// pointer into the console log at the moment their command ran.
type ConsoleChannel interface {
	Channel
	Sender
}

// Recorder receives every verb invocation, successful or not.
//
// The Commander deliberately does not write audit rows and does not import
// internal/audit. An audit row needs actor_kind and actor_via, and the only
// component that can know those is the one that authenticated the request;
// what reaches this package is the resolved actor STRING the console log
// records. So the runner implements this and does both jobs at the boundary
// where identity is real.
//
// The other half of the job is the echo. A console verb is already in the ring
// -- Send echoed it as SourceActor -- and res.Seq is non-zero to say so. A
// CONTROL verb touches nothing the game prints, so unless the recorder writes a
// line for it, an RCON broadcast is invisible in the console log forever. That
// is the gap this interface exists to close.
type Recorder interface {
	RecordVerb(actor string, res Result, err error)
}

// RecorderFunc adapts a function to Recorder.
type RecorderFunc func(actor string, res Result, err error)

// RecordVerb implements Recorder.
func (f RecorderFunc) RecordVerb(actor string, res Result, err error) { f(actor, res, err) }

// Result is one verb invocation's outcome.
type Result struct {
	Verb    string
	Channel string // spec.ChannelConsole | spec.ChannelControl
	Command string // as sent, or spec.Redacted when the template carried a secret
	Secret  bool   // Command is redacted; the real one was never returned
	Output  string // control only; a console verb has no reply, ever
	Seq     uint64 // console only: the Seq of the echo in the ring
	At      time.Time
	Elapsed time.Duration
}

var (
	// ErrUnknownVerb means the manifest declares no such verb. It is not
	// "not supported yet": the verb set is closed per game and the dashboard
	// renders from it, so an unknown verb is a caller bug.
	ErrUnknownVerb = errors.New("control: no such verb")

	// ErrReservedVerb refuses `stop` through the verb path. See stopIsReserved.
	ErrReservedVerb = errors.New("control: verb is reserved")

	// ErrNoConsole means the commander was built without a console channel,
	// which only happens for `console.mode: none`.
	ErrNoConsole = errors.New("control: no console channel")
)

// stopIsReserved is why `stop` cannot be invoked as a verb.
//
// `verbs.stop` exists, and it is the string the shutdown ladder sends. Sending
// it through this path would put `quit` on the wire with no save step before
// it, no wait_exit after it, no Report, no ladder and no degraded flag -- the
// exact "screen -X stuff quit and hope" that section 11.1 rejects. Worse, the
// socket authorizes `verb` and `stop` at DIFFERENT tiers (section 11.4), so a
// peer allowed to run verbs and not to stop the server could stop the server by
// naming it as one.
const stopIsReserved = "`stop` is the shutdown ladder's; use the stop op so the world is saved and a report is written"

// Options configures a Commander. Manifest and Console are required.
type Options struct {
	Manifest *spec.Manifest
	Instance *spec.Instance

	// Renderer binds this instance's values. Required whenever any verb
	// template interpolates anything; a nil renderer is accepted only so a
	// test with literal verbs stays short.
	Renderer *spec.Renderer

	// Console is the always-present channel. A game with `console.mode: none`
	// passes nil and every console verb reports ErrNoConsole.
	Console ConsoleChannel

	// Control is the accelerator. nil is normalised to None(game), so the
	// routing code never nil-checks and a Valheim commander answers
	// "Valheim has no remote control channel" rather than panicking.
	Control Channel

	// Recorder is optional. Without one, control verbs leave no trace in the
	// console log -- acceptable in a test, never in the runner.
	Recorder Recorder

	// Live supplies the parts of the capability input this package cannot see:
	// the instance state, the blocked-content count, EULA acceptance. The
	// control half is always overwritten from the channel itself, so the
	// button and the router cannot disagree.
	Live func() spec.Live

	Clock func() time.Time
}

// New returns a Commander.
func New(o Options) (*Commander, error) {
	if o.Manifest == nil {
		return nil, errors.New("control: commander needs a manifest")
	}
	c := &Commander{
		m:       o.Manifest,
		inst:    o.Instance,
		rend:    o.Renderer,
		console: o.Console,
		control: o.Control,
		rec:     o.Recorder,
		live:    o.Live,
		now:     o.Clock,
	}
	if c.control == nil {
		c.control = None(gameName(o.Manifest))
	}
	if c.live == nil {
		c.live = func() spec.Live { return spec.Live{} }
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

func gameName(m *spec.Manifest) string {
	if m == nil {
		return ""
	}
	if m.Metadata.Name != "" {
		return m.Metadata.Name
	}
	return m.Metadata.ID
}

// Control returns the control channel, never nil. The shutdown ladder's
// `via: control` steps run through it directly: a ladder rung is not a verb and
// must not be rewritten by verb routing halfway down a stop.
func (c *Commander) Control() Channel { return c.control }

// Console returns the console channel, which may be nil for `mode: none`.
func (c *Commander) Console() ConsoleChannel { return c.console }

// Renderer returns the bound renderer, for callers that render manifest fields
// outside the verb surface (the shutdown ladder's `send:` strings).
func (c *Commander) Renderer() *spec.Renderer { return c.rend }

// Verbs lists the declared verbs in a stable order.
func (c *Commander) Verbs() []string {
	out := make([]string, 0, len(c.m.Spec.Verbs))
	for name := range c.m.Spec.Verbs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Live is the capability input, with the control half filled in from the
// channel rather than from whatever the caller last observed.
//
// An unset State is resolved from the console channel instead of defaulting to
// a constant: defaulting to "ready" would render every button enabled on a
// stopped server, and defaulting to "inactive" would grey out the console on a
// running one. The channel knows.
func (c *Commander) Live() spec.Live {
	l := c.live()
	l.ControlReady = c.control.Ready()
	if l.State == "" {
		l.State = spec.StateInactive
		if c.console != nil && c.console.Ready() {
			l.State = spec.StateReady
		}
	}
	return l
}

// Capabilities is what the dashboard renders (section 8.5).
//
// The derivation lives in internal/spec and is not duplicated here. What this
// adds is the one thing spec cannot know: a channel that has actually FAILED
// replaces the manifest's not_ready_reason with its own sentence. "RCON does
// not bind until Project Zomboid finishes booting" is the right tooltip for the
// first forty minutes and a lie an hour later when the real answer is a wrong
// password. Until something fails, the manifest's sentence is passed through
// verbatim.
func (c *Commander) Capabilities() []spec.CapState {
	caps := spec.Capabilities(c.m, c.inst, c.Live())

	if Describe(c.control).LastError == "" {
		return caps
	}
	reason := c.control.Unready()
	if reason == "" {
		return caps
	}
	for i := range caps {
		if s := &caps[i]; s.Supported && !s.Available && s.Channel == spec.ChannelControl {
			s.Reason = reason
		}
	}
	return caps
}

// Do runs one verb.
//
// actor is the resolved attribution string ("user:alice@example.com",
// "root_cli", "unix:pzserver"); it is recorded, never parsed for authorization.
// Authorization happened at the socket, at the tier boundary, before this call.
func (c *Commander) Do(ctx context.Context, actor, verb string, args map[string]string) (Result, error) {
	start := c.now()
	res := Result{Verb: verb, At: start}

	out, err := c.do(ctx, actor, verb, args, &res)
	res.Output = out
	res.Elapsed = c.now().Sub(start)
	if c.rec != nil {
		c.rec.RecordVerb(actor, res, err)
	}
	return res, err
}

func (c *Commander) do(ctx context.Context, actor, verb string, args map[string]string, res *Result) (string, error) {
	name := strings.TrimSpace(verb)
	if name == "" {
		return "", fmt.Errorf("%w: %q", ErrUnknownVerb, verb)
	}
	if name == "stop" {
		return "", fmt.Errorf("%w: %s", ErrReservedVerb, stopIsReserved)
	}
	v, ok := c.m.Spec.Verbs[name]
	if !ok {
		return "", fmt.Errorf("%w: %s does not declare %q", ErrUnknownVerb, gameName(c.m), name)
	}

	// Arguments are checked BEFORE the channel is chosen, so an injection
	// attempt is refused identically whether or not RCON happens to be up.
	// Routing that depended on the argument would make the refusal a probe.
	bound, err := BindArgs(args)
	if err != nil {
		return "", err
	}

	channel, tmpl, err := c.route(name, v)
	if err != nil {
		return "", err
	}
	res.Channel = channel

	cmd, taint, err := c.render("spec.verbs."+name+"."+channel, tmpl, bound)
	if err != nil {
		return "", err
	}

	// The rendered command gets the same one-command-per-line rule the socket
	// applies to a raw `send`. Arguments were already refused for CR, LF and
	// NUL, but a var or a secret VALUE could carry one, and the value came from
	// a file rather than from this call.
	if err := console.ValidateSendLine(cmd); err != nil {
		return "", fmt.Errorf("%w: rendered %s command for %q: %w", ErrUnsafeArg, channel, name, err)
	}

	res.Command = cmd
	if taint.Has(spec.TaintSecret) {
		res.Secret = true
		res.Command = spec.Redacted
	}

	if channel == spec.ChannelControl {
		return c.control.Exec(ctx, cmd)
	}
	seq, err := c.console.Send(actor, cmd)
	res.Seq = seq
	return "", err
}

// route picks the channel for one verb, or explains why there is none.
func (c *Commander) route(name string, v spec.Verb) (channel, tmpl string, err error) {
	hasConsole := v.Console != "" && c.console != nil

	if v.Control != "" && c.control.Ready() {
		return spec.ChannelControl, v.Control, nil
	}
	if hasConsole && c.console.Ready() {
		return spec.ChannelConsole, v.Console, nil
	}

	// Nothing can carry it. Which sentence to hand the operator depends on
	// which channel they were entitled to expect, and the order matters: a verb
	// with both forms on a stopped server is a stopped server, not an RCON
	// problem.
	switch {
	case hasConsole:
		return "", "", fmt.Errorf("%w: %s over the console: %s", ErrNotReady, name, reasonOr(c.console, "the server is not accepting console input"))
	case v.Control != "":
		return "", "", fmt.Errorf("%w: %s over the control channel: %s", ErrNotReady, name, reasonOr(c.control, "the control channel is not up"))
	case v.Console != "":
		return "", "", fmt.Errorf("%w: %s is a console verb and %s captures no console",
			ErrNoConsole, name, gameName(c.m))
	default:
		return "", "", fmt.Errorf("%w: %s declares neither a console nor a control form", ErrUnknownVerb, name)
	}
}

func reasonOr(ch Channel, fallback string) string {
	if ch == nil {
		return fallback
	}
	if r := ch.Unready(); r != "" {
		return r
	}
	return fallback
}

// render expands one verb template against this invocation's operands.
//
// A nil renderer is allowed only for a template with nothing to expand. The
// alternative -- rendering against a zero RenderData -- would turn
// `kickuser "{{ .Player }}"` into `kickuser ""` and kick nobody while
// reporting success.
func (c *Commander) render(field, tmpl string, args spec.VerbArgs) (string, spec.Taint, error) {
	if c.rend == nil {
		if strings.Contains(tmpl, "{{") {
			return "", spec.TaintNone, fmt.Errorf("control: %s needs a renderer: %q", field, tmpl)
		}
		return tmpl, spec.TaintNone, nil
	}
	return c.rend.WithVerbArgs(args).Render(field, tmpl)
}

// Rejected reports whether err is the caller's mistake rather than the game's
// or the host's.
//
// The socket maps this onto its bad_request event code, which is what lets
// gvm-web tell a user "that is not a verb" instead of "internal error" -- and,
// more importantly, tells it not to retry.
func Rejected(err error) bool {
	for _, sentinel := range []error{ErrUnsafeArg, ErrUnknownVerb, ErrReservedVerb, ErrNotReady, ErrNoChannel, ErrNoConsole} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
