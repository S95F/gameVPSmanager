package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestNoopIsAnExplainedRefusal(t *testing.T) {
	n := None("Valheim")
	if n.Ready() {
		t.Fatal("a game with no control channel reports one that is ready")
	}
	if !strings.Contains(n.Unready(), "Valheim") {
		t.Errorf("reason %q does not name the game", n.Unready())
	}
	if n.Kind() != spec.ControlNone {
		t.Errorf("kind %q", n.Kind())
	}

	_, err := n.Exec(context.Background(), "players")
	if !errors.Is(err, ErrNoChannel) {
		t.Fatalf("Exec: %v, want ErrNoChannel", err)
	}
	// A caller that skipped Ready still gets something an operator can read.
	if !strings.Contains(err.Error(), "Valheim") {
		t.Errorf("Exec error %q drops the reason", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// ErrNoChannel and ErrNotReady are deliberately different sentinels: one
// resolves itself in forty minutes and the other never does, and a caller
// deciding whether to retry needs to tell them apart.
func TestNoChannelIsNotNotReady(t *testing.T) {
	if errors.Is(ErrNoChannel, ErrNotReady) || errors.Is(ErrNotReady, ErrNoChannel) {
		t.Fatal("the permanent and temporary refusals are the same error")
	}
}

func TestUnsupportedKindKeepsItsSentence(t *testing.T) {
	n := Unsupported(spec.ControlTelnet, "telnet control ships in gvm v1.1; use the console meanwhile")
	if n.Kind() != spec.ControlTelnet {
		t.Errorf("kind %q, want telnet: status.json must report what the manifest ASKED for", n.Kind())
	}
	if !strings.Contains(n.Unready(), "v1.1") {
		t.Errorf("reason %q does not name the release", n.Unready())
	}
}

// A nil channel is reported as kind "none", not omitted: a missing control
// block in status.json reads as "the runner has not got that far", which is a
// much more alarming thing than "this game has no RCON".
func TestDescribeHandlesANilChannel(t *testing.T) {
	s := Describe(nil)
	if s.Kind != spec.ControlNone {
		t.Fatalf("Describe(nil) = %+v", s)
	}
	if s.Ready {
		t.Error("a nil channel is reported ready")
	}
}

func TestDescribeUsesReporterWhenAvailable(t *testing.T) {
	want := Status{
		Kind: spec.ControlSourceRCON, Dialect: "simple", Ready: true,
		LastOK: time.Unix(1_700_000_000, 0).UTC(), ConsecutiveFailures: 0,
	}
	got := Describe(&reporterChannel{Status: want})
	if got != want {
		t.Fatalf("Describe = %+v, want %+v", got, want)
	}

	// A channel that does not track health still gets a usable block rather
	// than a zero one.
	plain := Describe(&plainChannel{ready: true})
	if plain.Kind != spec.ControlSourceRCON || !plain.Ready {
		t.Fatalf("Describe on a non-Reporter = %+v", plain)
	}
}

type reporterChannel struct {
	plainChannel
	Status Status
}

func (c *reporterChannel) Snapshot() Status { return c.Status }

type plainChannel struct{ ready bool }

func (c *plainChannel) Kind() Kind  { return spec.ControlSourceRCON }
func (c *plainChannel) Ready() bool { return c.ready }
func (c *plainChannel) Unready() string {
	if c.ready {
		return ""
	}
	return "down"
}
func (c *plainChannel) Exec(context.Context, string) (string, error) { return "", nil }
func (c *plainChannel) Close() error                                 { return nil }

// LastOK is omitzero rather than omitempty: a channel that has never succeeded
// must not report the zero time as a timestamp, because "last_ok":
// "0001-01-01T00:00:00Z" in status.json is a date an operator will try to
// interpret.
func TestStatusOmitsAnUnsetLastOK(t *testing.T) {
	b, err := marshalStatus(Status{Kind: spec.ControlSourceRCON, LastError: "connection refused"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b, "last_ok") {
		t.Errorf("status carries an unset last_ok: %s", b)
	}
	// last_error is always present, because "" is the answer to "what went
	// wrong" and an absent key is not.
	if !strings.Contains(b, `"last_error"`) {
		t.Errorf("status omits last_error: %s", b)
	}

	b, err = marshalStatus(Status{Kind: spec.ControlSourceRCON, LastOK: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "last_ok") {
		t.Errorf("status drops a real last_ok: %s", b)
	}
}

func marshalStatus(s Status) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}
