package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

func TestLogMarkerMatchesALineThatArrivesLater(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	p, err := NewLogMarker(f.Ring, "SERVER STARTED", 0)
	if err != nil {
		t.Fatalf("NewLogMarker: %v", err)
	}

	go func() {
		f.Emit("1699999999 Loading world")
		f.Emit("*** SERVER STARTED ****")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.Contains(l.Text, "SERVER STARTED") {
		t.Fatalf("matched %q", l.Text)
	}
	if p.Kind() != KindLogMarker {
		t.Errorf("Kind() = %q", p.Kind())
	}
	if !strings.Contains(p.Describe(), "SERVER STARTED") {
		t.Errorf("Describe() = %q", p.Describe())
	}
}

// A marker already in the ring counts for readiness: boot output arrives while
// the probe list is still being built, and a probe that ignored history would
// wait forever for a line the game already printed.
func TestLogMarkerMatchesHistoryFromZero(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	f.Emit("*** SERVER STARTED ****")

	p, err := NewLogMarker(f.Ring, "SERVER STARTED", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Section 11.6 rule 1, and the reason After exists. A `save` typed an hour ago
// must not satisfy the wait that guards `quit`, or `quit` lands mid-write.
func TestAfterExcludesTheLineAlreadyInTheRing(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	stale := f.Emit("SAVED")

	since := After(f.Ring)
	if since != stale+1 {
		t.Fatalf("After() = %d with head %d; a cursor of head itself accepts the stale line", since, stale)
	}

	p, err := NewLogMarker(f.Ring, "SAVED", since)
	if err != nil {
		t.Fatal(err)
	}
	if p.Match(console.Line{Seq: stale, Text: "SAVED"}) {
		t.Fatal("the stale marker satisfied the probe")
	}
	if !p.Match(console.Line{Seq: since, Text: "SAVED"}) {
		t.Fatal("a fresh marker did not satisfy the probe")
	}

	// And end to end: the wait must not return until a NEW line matches.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err == nil {
		t.Fatal("the wait was satisfied by a line that predates it")
	}

	f.Emit("SAVED")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := p.Run(ctx2); err != nil {
		t.Fatalf("the wait missed the fresh marker: %v", err)
	}
}

func TestLogMarkerTimesOutWithItsOwnTimeout(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	p, err := NewLogMarker(f.Ring, "NEVER PRINTED", 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Timeout = 50 * time.Millisecond

	start := time.Now()
	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("a marker that never arrives reported ready")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe's own timeout was ignored")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the underlying cause is lost: %v", err)
	}
	if !strings.Contains(err.Error(), "NEVER PRINTED") {
		t.Errorf("error does not name the pattern an operator is waiting for: %v", err)
	}
}

// A closed ring means the runner is gone. The probe must return rather than
// block a readiness loop on a console that will never produce another line.
func TestLogMarkerReturnsWhenTheRingCloses(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	p, err := NewLogMarker(f.Ring, "SERVER STARTED", 0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()
	f.Ring.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a closed ring reported the marker as seen")
		}
		if !errors.Is(err, console.ErrRingClosed) {
			t.Errorf("error does not carry ErrRingClosed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the ring closed")
	}
}

// Section 8.6 check 9: the pattern compiles when the manifest is read, so a bad
// regex is a validation error rather than a runner that waits an hour for a
// marker it can never match.
func TestNewLogMarkerRefusesABadPattern(t *testing.T) {
	f := console.NewFake(console.FakeOptions{Instance: "pz-main"})
	if _, err := NewLogMarker(f.Ring, "SERVER (STARTED", 0); err == nil {
		t.Fatal("an uncompilable pattern was accepted")
	}
	if _, err := NewLogMarker(f.Ring, "", 0); err == nil {
		t.Fatal("an empty pattern was accepted")
	}
	if _, err := NewLogMarker(nil, "x", 0); err == nil {
		t.Fatal("a probe was built with no ring")
	}
}

// A log marker is evidence about the GAME. An operator's line and a gvm
// annotation both land in the same ring, and both can contain the marker word
// -- an operator broadcasting "world saved, server restarting" is the shape
// that releases `quit` onto a world that has not finished writing.
func TestLogMarkerOnlyBelievesTheGame(t *testing.T) {
	r := console.NewRing(1<<10, 8)
	m, err := NewLogMarker(r, "(?i)saved", 0)
	if err != nil {
		t.Fatal(err)
	}
	m.Timeout = 50 * time.Millisecond

	for _, l := range []console.Line{
		{Source: console.SourceActor, Actor: "user:alice", Text: `servermsg "world saved"`},
		{Source: console.SourceRunner, Text: "-- gvm: the world was saved"},
	} {
		seq := r.Append(l)
		if m.Match(console.Line{Seq: seq, Source: l.Source, Text: l.Text}) {
			t.Errorf("Match accepted a %s line: %q", l.Source, l.Text)
		}
	}
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("Run matched a line the game never printed")
	}

	seq := r.Append(console.Line{Source: console.SourceGame, Text: "WORLD SAVED"})
	if !m.Match(console.Line{Seq: seq, Source: console.SourceGame, Text: "WORLD SAVED"}) {
		t.Error("Match refused the game's own line")
	}
	got, err := m.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.Seq != seq {
		t.Errorf("matched seq %d, want the game's line at %d", got.Seq, seq)
	}
}
