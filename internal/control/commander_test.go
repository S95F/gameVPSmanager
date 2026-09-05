package control

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// ---------- fakes ----------

// fakeControl is a control channel whose readiness and answers are set by the
// test. It carries a lastErr so the "a failed channel replaces the manifest
// sentence" rule can be exercised without an RCON server.
type fakeControl struct {
	mu      sync.Mutex
	ready   bool
	reason  string
	lastErr string
	out     string
	err     error
	calls   []string
}

func (f *fakeControl) Kind() Kind { return spec.ControlSourceRCON }

func (f *fakeControl) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeControl) Unready() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready {
		return ""
	}
	return f.reason
}

func (f *fakeControl) Exec(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	return f.out, f.err
}

func (f *fakeControl) Close() error { return nil }

func (f *fakeControl) Snapshot() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Status{Kind: spec.ControlSourceRCON, Ready: f.ready, LastError: f.lastErr}
}

func (f *fakeControl) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeConsole is console.Fake seen as a ConsoleChannel. It uses the real
// ingest and the real ring, so a console verb in these tests goes through the
// same splitter and the same echo the runner ships.
type fakeConsole struct {
	*console.Fake
	ready bool
}

func (f *fakeConsole) Kind() Kind  { return spec.ControlNone }
func (f *fakeConsole) Ready() bool { return f.ready }

func (f *fakeConsole) Unready() string {
	if f.ready {
		return ""
	}
	return "the server is stopped"
}

func (f *fakeConsole) Exec(_ context.Context, cmd string) (string, error) {
	_, err := f.Send("runner", cmd)
	return "", err
}

func newFakeConsole(ready bool) *fakeConsole {
	return &fakeConsole{Fake: console.NewFake(console.FakeOptions{Instance: "pz-main"}), ready: ready}
}

// ---------- helpers ----------

func shipped(t *testing.T, id string) *spec.Manifest {
	t.Helper()
	cat := spec.NewCatalog(spec.EmbeddedSource(games.FS, "embedded games"))
	m, diags, err := cat.Load(id, spec.DenyOverrides{})
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	if diags.HasErrors() {
		t.Fatalf("load %s: %s", id, diags)
	}
	return m
}

type harness struct {
	*Commander
	control *fakeControl
	console *fakeConsole
	rec     []Result
	recErrs []error
}

func newHarness(t *testing.T, id string, controlReady, consoleReady bool, state string) *harness {
	t.Helper()
	m := shipped(t, id)

	h := &harness{console: newFakeConsole(consoleReady)}
	if m.Spec.Control != nil {
		h.control = &fakeControl{ready: controlReady, reason: m.Spec.Control.NotReadyReason}
	}

	var ctl Channel
	if h.control != nil {
		ctl = h.control
	}
	c, err := New(Options{
		Manifest: m,
		Renderer: spec.NewRenderer(spec.RenderData{Name: "pz-main", Game: id}),
		Console:  h.console,
		Control:  ctl,
		Live:     func() spec.Live { return spec.Live{State: state} },
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Recorder: RecorderFunc(func(_ string, r Result, err error) {
			h.rec = append(h.rec, r)
			h.recErrs = append(h.recErrs, err)
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Commander = c
	return h
}

func capOf(t *testing.T, caps []spec.CapState, c spec.Capability) spec.CapState {
	t.Helper()
	s, ok := spec.FindCap(caps, c)
	if !ok {
		t.Fatalf("no %s capability in %v", c, caps)
	}
	return s
}

// ---------- the three requirements with teeth ----------

// Valheim has no query channel and never will. The button is OMITTED, not
// greyed: a greyed control promises it will work later.
func TestValheimHasNoPlayerList(t *testing.T) {
	h := newHarness(t, "valheim", false, true, spec.StateReady)

	players := capOf(t, h.Capabilities(), spec.CapPlayers)
	if players.Supported {
		t.Fatalf("players reports Supported on Valheim: %+v", players)
	}
	if players.Reason == "" {
		t.Error("Supported == false with no reason; the dashboard has nothing to explain the omission with")
	}

	// And the verb genuinely cannot be invoked, rather than merely being
	// hidden in the UI.
	_, err := h.Do(context.Background(), "root_cli", "players", nil)
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("Do(players) on Valheim: %v, want ErrUnknownVerb", err)
	}
	if !Rejected(err) {
		t.Error("an unknown verb is not classified as the caller's mistake")
	}

	// Valheim's one verb still works over the console, which is the whole
	// point of console-as-control-plane.
	res, err := h.Do(context.Background(), "root_cli", "save", nil)
	if err != nil {
		t.Fatalf("Do(save) on Valheim: %v", err)
	}
	if res.Channel != spec.ChannelConsole || res.Command != "save" {
		t.Fatalf("save routed as %+v", res)
	}
}

// Project Zomboid's 40-60 minute first boot is the case section 8.5 exists for:
// the button is present, disabled, and the tooltip is the manifest's own
// sentence -- verbatim, not paraphrased by Go code.
func TestProjectZomboidBeforeReadyIsSupportedButUnavailable(t *testing.T) {
	m := shipped(t, "projectzomboid")
	want := m.Spec.Control.NotReadyReason
	if want == "" {
		t.Fatal("projectzomboid.yaml declares no not_ready_reason")
	}

	h := newHarness(t, "projectzomboid", false, true, spec.StateActivating)
	caps := h.Capabilities()

	for _, c := range []spec.Capability{spec.CapControl, spec.CapPlayers, spec.CapBroadcast, spec.CapKick} {
		s := capOf(t, caps, c)
		if !s.Supported {
			t.Errorf("%s: Supported == false; PZ does have RCON, it is just not up yet", c)
		}
		if s.Available {
			t.Errorf("%s: Available == true before the gate opens", c)
		}
		if s.Reason != want {
			t.Errorf("%s: reason %q, want the manifest's %q verbatim", c, s.Reason, want)
		}
	}

	// The console is still writable during boot, which is the channel an
	// operator actually has for that hour.
	if w := capOf(t, caps, spec.CapConsoleWrite); !w.Available {
		t.Errorf("console.write unavailable while activating: %+v", w)
	}
}

// Routing, both ways, on the one verb PZ declares on both channels.
func TestRoutingPrefersControlWhenReadyAndFallsBackToConsole(t *testing.T) {
	t.Run("control ready", func(t *testing.T) {
		h := newHarness(t, "projectzomboid", true, true, spec.StateReady)
		h.control.out = "World saved."

		res, err := h.Do(context.Background(), "user:alice@example.com", "save", nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if res.Channel != spec.ChannelControl {
			t.Fatalf("routed to %q, want control", res.Channel)
		}
		if res.Output != "World saved." {
			t.Errorf("output %q; the reply is the reason control is preferred", res.Output)
		}
		if got := h.control.sent(); len(got) != 1 || got[0] != "save" {
			t.Fatalf("control saw %q", got)
		}
		if got := h.console.SentLines(); len(got) != 0 {
			t.Fatalf("console also saw %q; the command was sent twice", got)
		}
	})

	t.Run("control not ready", func(t *testing.T) {
		h := newHarness(t, "projectzomboid", false, true, spec.StateActivating)

		res, err := h.Do(context.Background(), "user:alice@example.com", "save", nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if res.Channel != spec.ChannelConsole {
			t.Fatalf("routed to %q, want console", res.Channel)
		}
		if res.Output != "" {
			t.Errorf("console verb reported output %q; the console never replies", res.Output)
		}
		if res.Seq == 0 {
			t.Error("no Seq: the console echo is the record of who typed what (section 11.4)")
		}
		sent := h.console.Sent()
		if len(sent) != 1 || sent[0].Line != "save" {
			t.Fatalf("console saw %+v", sent)
		}
		if sent[0].Actor != "user:alice@example.com" {
			t.Errorf("echo attributed to %q", sent[0].Actor)
		}
		if len(h.control.sent()) != 0 {
			t.Error("dialled the control channel while it reported not ready")
		}
	})
}

// ---------- injection, at the routing layer ----------

func TestBroadcastInterpolatesTheOperand(t *testing.T) {
	h := newHarness(t, "projectzomboid", true, true, spec.StateReady)

	res, err := h.Do(context.Background(), "root_cli", "broadcast",
		map[string]string{"Msg": "restart in 5 minutes"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if want := `servermsg "restart in 5 minutes"`; res.Command != want {
		t.Fatalf("command %q, want %q", res.Command, want)
	}
}

// The refusal happens before anything is sent, on either channel. A verb that
// half-ran would be worse than one that did not run.
func TestUnsafeArgumentReachesNoChannel(t *testing.T) {
	for _, arg := range []string{`alice" ; servermsg "owned`, "line one\nquit", "nul\x00"} {
		h := newHarness(t, "projectzomboid", true, true, spec.StateReady)
		_, err := h.Do(context.Background(), "root_cli", "broadcast", map[string]string{"Msg": arg})
		if !errors.Is(err, ErrUnsafeArg) {
			t.Fatalf("Msg=%q: err %v, want ErrUnsafeArg", arg, err)
		}
		if got := h.control.sent(); len(got) != 0 {
			t.Fatalf("Msg=%q reached the control channel as %q", arg, got)
		}
		if got := h.console.SentLines(); len(got) != 0 {
			t.Fatalf("Msg=%q reached the console as %q", arg, got)
		}
	}
}

// The argument check runs before routing, so a refusal cannot be used to
// discover whether RCON is up.
func TestUnsafeArgumentIsRefusedIdenticallyOnEitherChannel(t *testing.T) {
	up := newHarness(t, "projectzomboid", true, true, spec.StateReady)
	_, upErr := up.Do(context.Background(), "root_cli", "broadcast", map[string]string{"Msg": "x\"y"})

	down := newHarness(t, "projectzomboid", false, true, spec.StateActivating)
	_, downErr := down.Do(context.Background(), "root_cli", "broadcast", map[string]string{"Msg": "x\"y"})

	if upErr == nil || downErr == nil {
		t.Fatalf("accepted a double quote: up=%v down=%v", upErr, downErr)
	}
	if upErr.Error() != downErr.Error() {
		t.Errorf("the refusal differs by channel state:\n  ready:     %v\n  not ready: %v", upErr, downErr)
	}
}

// ---------- stop ----------

// `stop` through the verb path would send `quit` with no save before it, no
// wait_exit after it and no report -- and it would do so at the `verb`
// authorization tier, which section 11.4 sets lower than `stop`.
func TestStopIsNotAVerb(t *testing.T) {
	h := newHarness(t, "projectzomboid", true, true, spec.StateReady)
	_, err := h.Do(context.Background(), "user:alice@example.com", "stop", nil)
	if !errors.Is(err, ErrReservedVerb) {
		t.Fatalf("Do(stop): %v, want ErrReservedVerb", err)
	}
	if len(h.control.sent()) != 0 || len(h.console.SentLines()) != 0 {
		t.Fatal("`quit` was sent outside the shutdown ladder")
	}
	if !strings.Contains(err.Error(), "stop op") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// ---------- unavailable ----------

func TestNeitherChannelAvailable(t *testing.T) {
	h := newHarness(t, "projectzomboid", false, false, spec.StateInactive)

	_, err := h.Do(context.Background(), "root_cli", "save", nil)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Do on a stopped server: %v, want ErrNotReady", err)
	}
	// A verb with both forms on a stopped server is a stopped server, not an
	// RCON problem: the sentence has to be the one the operator can act on.
	if !strings.Contains(err.Error(), "the server is stopped") {
		t.Errorf("reason %q does not mention the stopped server", err)
	}
	if !Rejected(err) {
		t.Error("ErrNotReady is not classified as the caller's mistake")
	}
}

func TestControlOnlyVerbSaysSoWhenControlIsDown(t *testing.T) {
	h := newHarness(t, "projectzomboid", false, true, spec.StateActivating)

	_, err := h.Do(context.Background(), "root_cli", "players", nil)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Do(players): %v, want ErrNotReady", err)
	}
	want := shipped(t, "projectzomboid").Spec.Control.NotReadyReason
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not carry the manifest sentence %q", err, want)
	}
}

// ---------- the live sentence beats the manifest one ----------

// "RCON does not bind until Project Zomboid finishes booting" is the right
// tooltip for the first forty minutes and a lie an hour later when the truth is
// a rejected password.
func TestAFailedChannelReplacesTheManifestReason(t *testing.T) {
	h := newHarness(t, "projectzomboid", false, true, spec.StateReady)
	live := "rcon: authentication failed; not retrying for 4m58s"
	h.control.mu.Lock()
	h.control.reason, h.control.lastErr = live, "rcon: authentication failed"
	h.control.mu.Unlock()

	s := capOf(t, h.Capabilities(), spec.CapControl)
	if s.Reason != live {
		t.Fatalf("reason %q, want the channel's live sentence %q", s.Reason, live)
	}
	if !s.Supported || s.Available {
		t.Fatalf("control state %+v", s)
	}
}

// ---------- the recorder ----------

func TestRecorderSeesSuccessAndFailure(t *testing.T) {
	h := newHarness(t, "projectzomboid", true, true, spec.StateReady)
	h.control.out = "1 player"

	if _, err := h.Do(context.Background(), "token:ab12", "players", nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := h.Do(context.Background(), "token:ab12", "nosuchverb", nil); err == nil {
		t.Fatal("unknown verb accepted")
	}
	if len(h.rec) != 2 {
		t.Fatalf("recorder saw %d invocations, want 2", len(h.rec))
	}
	if h.rec[0].Verb != "players" || h.rec[0].Channel != spec.ChannelControl || h.recErrs[0] != nil {
		t.Errorf("first record %+v (err %v)", h.rec[0], h.recErrs[0])
	}
	if h.recErrs[1] == nil {
		t.Error("the failed invocation was recorded as a success")
	}
	// A control verb touches nothing the game prints, so this record is the
	// only trace it ever happened.
	if h.rec[0].Command != "players" {
		t.Errorf("record does not carry the command: %+v", h.rec[0])
	}
}

// ---------- construction ----------

func TestNilControlBecomesAnExplainedRefusalRatherThanAPanic(t *testing.T) {
	c, err := New(Options{
		Manifest: shipped(t, "valheim"),
		Console:  newFakeConsole(true),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Control() == nil {
		t.Fatal("Control() is nil; every routing decision would need a nil check")
	}
	if c.Control().Ready() {
		t.Error("a game with no control channel reports one that is ready")
	}
	if r := c.Control().Unready(); !strings.Contains(r, "console") {
		t.Errorf("reason %q does not tell the operator where to type instead", r)
	}
}

func TestLiveStateIsResolvedFromTheChannelWhenUnset(t *testing.T) {
	h := newHarness(t, "projectzomboid", true, true, "")
	if got := h.Live().State; got != spec.StateReady {
		t.Errorf("state %q with a writable console, want %q", got, spec.StateReady)
	}
	if !h.Live().ControlReady {
		t.Error("ControlReady is false while the channel reports ready")
	}

	down := newHarness(t, "projectzomboid", false, false, "")
	if got := down.Live().State; got != spec.StateInactive {
		t.Errorf("state %q with a dead console, want %q", got, spec.StateInactive)
	}
}
