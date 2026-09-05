package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer that survives -race. slog handlers serialise their
// own writes, but a test that reads while a goroutine logs does not.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// quietLogger returns a logger whose output the test can inspect, so the
// audit system's own error reporting is asserted rather than sprayed over the
// test run.
func quietLogger() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// fixedClock is a wall clock that does not move, so At is assertable.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// failingSink reports an error on every write and records what it saw anyway,
// which is how the "one sink failing does not stop the others" property is
// checked.
type failingSink struct {
	mu   sync.Mutex
	err  error
	seen []Record
}

func (f *failingSink) Write(_ context.Context, r Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r)
	return f.err
}

func (f *failingSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func newTestLogger(t *testing.T) (*Logger, *MemorySink, *syncBuf) {
	t.Helper()
	sink := NewMemorySink()
	log, buf := quietLogger()
	return New(sink).WithLogger(log), sink, buf
}

func stopEvent() Event {
	return Event{
		Actor:    RootCLI(),
		Action:   InstanceStop,
		Instance: "pz-main",
		Params:   Params{"reason": "nightly", "budget_sec": 300},
	}
}

// TestMain silences the package-level slog calls Replay makes when it steps
// over a torn or malformed line. They are correct in production -- an operator
// needs the line number -- and pure noise in a suite that deliberately writes
// broken files.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func TestRecordCarriesASpoolIDAndAUTCTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 5, 5, 0, 12, 0, time.FixedZone("CEST", 2*3600))
	log, sink, _ := newTestLogger(t)
	log.WithClock(fixedClock(at))

	rec := log.Record(context.Background(), stopEvent(), Started, nil)

	if rec.SpoolID == "" {
		t.Fatal("no spool_id was assigned; every sink has to see the same id or the journald line, " +
			"the spool line and the eventual row are three unrelated events")
	}
	if loc := rec.At.Location(); loc != time.UTC {
		t.Fatalf("At is in %v, want UTC: the spool filename is UTC and a record whose timestamp is "+
			"in the VPS provider's timezone lands in the wrong day's file", loc)
	}
	if !rec.At.Equal(at) {
		t.Fatalf("At = %v, want the same instant as %v", rec.At, at)
	}
	if got := sink.Records(); len(got) != 1 || got[0].SpoolID != rec.SpoolID {
		t.Fatalf("the sink saw %d records; the returned record must be the one that was written", len(got))
	}
}

func TestStartAndDoneAreTwoRowsCorrelatedByParent(t *testing.T) {
	log, sink, _ := newTestLogger(t)
	ctx := context.Background()

	span := log.Start(ctx, stopEvent())
	span.Done(ctx, nil, Params{"rung": "console_quit", "degraded": false})

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want the started row and the terminal row: a PZ stop takes three "+
			"minutes and an UPDATE is not available in an append-only table", len(recs))
	}
	start, done := recs[0], recs[1]
	if start.Outcome != Started || done.Outcome != OK {
		t.Fatalf("outcomes = %q, %q; want started then ok", start.Outcome, done.Outcome)
	}
	if done.ParentSpoolID != start.SpoolID {
		t.Fatalf("parent_spool_id = %q, want the started row's %q", done.ParentSpoolID, start.SpoolID)
	}
	if done.SpoolID == start.SpoolID {
		t.Fatal("the two rows share a spool_id, so replay would insert only one of them")
	}
	if done.Params["rung"] != "console_quit" {
		t.Fatalf("rung = %v; it is the field that makes the shutdown ladder observable", done.Params["rung"])
	}
	if done.Params["reason"] != "nightly" {
		t.Fatal("the terminal row lost the started row's params; a reader of the ok row should not " +
			"have to fetch the started row to learn why the stop happened")
	}
	if done.DurationMS < 1 {
		t.Fatalf("duration_ms = %d; a terminal row always has a measured duration, and 0 is "+
			"indistinguishable from absent in this encoding", done.DurationMS)
	}
}

// TestDoneIsIdempotent: a deferred Done plus an explicit one on the success
// path is the natural way to write a call site, and it must not produce two
// terminal rows for one operation.
func TestDoneIsIdempotent(t *testing.T) {
	log, sink, _ := newTestLogger(t)
	ctx := context.Background()

	span := log.Start(ctx, stopEvent())
	span.Done(ctx, nil, Params{"rung": "console_quit"})
	second := span.Done(ctx, errors.New("ignored"), Params{"rung": "sigterm"})

	if second.SpoolID != "" {
		t.Fatal("the second Done produced a record")
	}
	if n := len(sink.Records()); n != 2 {
		t.Fatalf("got %d records, want 2", n)
	}
}

func TestDoWritesTheTerminalRowOnError(t *testing.T) {
	log, sink, _ := newTestLogger(t)
	want := errors.New("systemctl stop timed out")

	err := log.Do(context.Background(), stopEvent(), func(context.Context) (Params, error) {
		return Params{"rung": "sigkill", "degraded": true}, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Do returned %v, want the callback's error unchanged", err)
	}
	recs := sink.Records()
	if len(recs) != 2 || recs[1].Outcome != Failed {
		t.Fatalf("got %d records ending in %q, want a started row and an error row", len(recs), recs[len(recs)-1].Outcome)
	}
	if !strings.Contains(recs[1].Error, "timed out") {
		t.Fatalf("error = %q, want the failure text", recs[1].Error)
	}
}

// TestDoClosesTheRecordThroughAPanic is the reason Do exists. A rebuild that
// panics halfway must not leave a row saying it started and nothing else --
// that record is indistinguishable from the daemon being killed, which is a
// different and much more alarming event.
func TestDoClosesTheRecordThroughAPanic(t *testing.T) {
	log, sink, _ := newTestLogger(t)

	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Error("Do swallowed the panic; auditing must never change what the program does")
			}
		}()
		_ = log.Do(context.Background(), stopEvent(), func(context.Context) (Params, error) {
			panic("boom")
		})
	}()

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want the started row and a terminal row", len(recs))
	}
	if recs[1].Outcome != Failed || !strings.Contains(recs[1].Error, "boom") {
		t.Fatalf("terminal row = %q / %q, want the panic recorded as the failure", recs[1].Outcome, recs[1].Error)
	}
	if n := len(log.OpenSpans()); n != 0 {
		t.Fatalf("%d spans still open after a panic", n)
	}
}

func TestAForgottenSpanIsVisibleAndClosable(t *testing.T) {
	log, sink, _ := newTestLogger(t)
	ctx := context.Background()

	span := log.Start(ctx, stopEvent())

	open := log.OpenSpans()
	if len(open) != 1 || open[0].SpoolID != span.SpoolID() {
		t.Fatalf("OpenSpans = %v, want the one unfinished operation: a forgotten Done has to be "+
			"findable, not merely absent", open)
	}

	if n := log.CloseAll(ctx, errors.New("gvmd is shutting down")); n != 1 {
		t.Fatalf("CloseAll closed %d spans, want 1", n)
	}
	if n := len(log.OpenSpans()); n != 0 {
		t.Fatalf("%d spans still open after CloseAll", n)
	}
	recs := sink.Records()
	if len(recs) != 2 || recs[1].Outcome != Failed {
		t.Fatalf("CloseAll must write a terminal row, got %d records", len(recs))
	}
}

func TestDeniedRecordsWhatWasRefusedAndWhy(t *testing.T) {
	log, sink, _ := newTestLogger(t)

	rec := log.Denied(context.Background(), User("019f2c7a", ViaWeb), "pz-main",
		"instance.stop", "instance.stop", "viewer")

	if rec.Action != AuthDenied {
		t.Fatalf("action = %q, want %q", rec.Action, AuthDenied)
	}
	if rec.Target != "instance.stop" {
		t.Fatalf("target = %q; the refused action has nowhere else to go and is the most useful "+
			"field in the row", rec.Target)
	}
	if rec.Params["permission"] != "instance.stop" || rec.Params["effective_role"] != "viewer" {
		t.Fatalf("params = %v, want the permission and the effective role", rec.Params)
	}
	if rec.Outcome != Failed {
		t.Fatalf("outcome = %q, want %q: a forbidden action is exactly as interesting as an allowed one",
			rec.Outcome, Failed)
	}
	if len(sink.Records()) != 1 {
		t.Fatal("the denial was not written")
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Denied produced a record that does not validate: %v", err)
	}
}

// TestMultiSinkDeliversEverywhereEvenWhenOneFails is the whole reason there are
// two sinks. /var/lib/gvm filling is precisely the moment the journald copy has
// to still be written.
func TestMultiSinkDeliversEverywhereEvenWhenOneFails(t *testing.T) {
	broken := &failingSink{err: errors.New("no space left on device")}
	mem := NewMemorySink()

	err := MultiSink{broken, mem}.Write(context.Background(), Record{SpoolID: "x"})
	if err == nil {
		t.Fatal("MultiSink hid a sink failure")
	}
	if mem.Len() != 1 {
		t.Fatal("the second sink was skipped because the first failed; they fail independently and " +
			"that is the point of having both")
	}
}

// TestASinkFailureDoesNotStopTheOperation: refusing to stop a game server
// because the audit disk is full would convert a disk problem into an outage,
// and journald already holds the copy that matters.
func TestASinkFailureDoesNotStopTheOperation(t *testing.T) {
	broken := &failingSink{err: errors.New("no space left on device")}
	log, buf := quietLogger()
	l := New(broken).WithLogger(log)

	rec := l.Record(context.Background(), stopEvent(), Started, nil)
	if rec.SpoolID == "" {
		t.Fatal("Record returned nothing usable when the sink failed")
	}
	if broken.count() != 1 {
		t.Fatal("the record was never offered to the sink")
	}
	if !strings.Contains(buf.String(), "audit sink failed") {
		t.Fatalf("the failure was silent. logged:\n%s", buf.String())
	}
}

// TestAnInvalidRecordIsWrittenAnywayAndSaysWhy: refusing to write would turn a
// call-site bug into a missing trail, which is strictly worse than a
// malformed one.
func TestAnInvalidRecordIsWrittenAnywayAndSaysWhy(t *testing.T) {
	log, sink, buf := newTestLogger(t)

	rec := log.Record(context.Background(), Event{
		Actor:  RootCLI(),
		Action: BackupCreate, // requires an instance name and three params
		Params: Params{"path": "/var/backups/gvm/pz-main/x.tar.zst"},
	}, OK, nil)

	if sink.Len() != 1 {
		t.Fatal("the invalid record was dropped")
	}
	problems, ok := rec.Params[KeyInvalid].([]string)
	if !ok || len(problems) == 0 {
		t.Fatalf("%s = %#v, want the validation failures recorded inside the row", KeyInvalid, rec.Params[KeyInvalid])
	}
	joined := strings.Join(problems, " | ")
	for _, want := range []string{"instance_name", "sha256"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s does not mention %q: %s", KeyInvalid, want, joined)
		}
	}
	if !strings.Contains(buf.String(), "audit record is invalid") {
		t.Fatalf("the problem was not logged. logged:\n%s", buf.String())
	}
}

func TestValidateRejectsTheZeroActor(t *testing.T) {
	err := Actor{}.Validate()
	if err == nil {
		t.Fatal("the zero Actor validated; actor_kind and actor_via are NOT NULL columns")
	}
	if !strings.Contains(err.Error(), "actor_kind") {
		t.Fatalf("err = %v, want it to name the empty column", err)
	}
}

// TestTheZeroActorIsNotRoot: an int PeerUID whose zero value is 0 would report
// every actor with no socket as uid 0. platform.Peer.Root() shipped with
// exactly that bug, which is why this field is a pointer.
func TestTheZeroActorIsNotRoot(t *testing.T) {
	if (Actor{}).PeerUID != nil {
		t.Fatal("the zero Actor claims a peer uid")
	}
	a := RootCLI().WithPeerUID(0)
	if a.PeerUID == nil || *a.PeerUID != 0 {
		t.Fatal("an explicitly recorded uid 0 was lost, which is the value this field most needs to carry")
	}

	b, err := json.Marshal(Record{Actor: Actor{}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("actor_peer_uid")) {
		t.Fatalf("an unset peer uid was encoded: %s", b)
	}
}

func TestParamsEncodeSortedAndSurviveAnUnencodableValue(t *testing.T) {
	p := Params{
		"b":   1,
		"a":   "x",
		"bad": make(chan int), // nothing encodes this
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("one bad value took the whole record down: %v. A lost audit record is worse than "+
			"a lost field.", err)
	}
	got := string(b)
	if !strings.HasPrefix(got, `{"a":"x","b":1,`) {
		t.Fatalf("params = %s, want keys in sorted order so the same params always encode alike", got)
	}
	if !strings.Contains(got, "unencodable chan int") {
		t.Fatalf("params = %s, want the bad value replaced by its type", got)
	}
}

// TestAnUnencodableValueReportsOnlyItsType: a custom Marshaler's error text is
// written by the type being marshalled, and the type most likely to fail here
// is one wrapping something it should not be printing.
func TestAnUnencodableValueReportsOnlyItsType(t *testing.T) {
	b, err := json.Marshal(Params{"k": talkativeFailure{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("the marshaler's error text reached the record: %s", b)
	}
	if !strings.Contains(string(b), "talkativeFailure") {
		t.Fatalf("params = %s, want the type name", b)
	}
}

type talkativeFailure struct{}

func (talkativeFailure) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("cannot encode the value hunter2")
}

func TestMemorySinkDeduplicatesAndRefusesAnUnidentifiedRecord(t *testing.T) {
	m := NewMemorySink()
	r := Record{SpoolID: "01920000-0000-7000-8000-000000000001"}

	if err := m.Write(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(context.Background(), r); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("the second write returned %v, want ErrDuplicate: replay has to be able to count "+
			"rows_ok and rows_dup separately, and a silently swallowed conflict makes every "+
			"re-replay look like a fresh outage", err)
	}
	if m.Len() != 1 {
		t.Fatalf("stored %d copies, want 1", m.Len())
	}
	if err := m.Write(context.Background(), Record{}); err == nil {
		t.Fatal("a record with no spool_id was stored; it could never be deduplicated, so accepting " +
			"it quietly breaks the contract this type models")
	}
}

func TestDurationRoundsUpRatherThanToZero(t *testing.T) {
	if got := clampMillis(400 * time.Microsecond); got != 1 {
		t.Fatalf("clampMillis(400us) = %d, want 1: 0 and 'not measured' are the same value here", got)
	}
	if got := clampMillis(90 * 24 * time.Hour); got != 1<<31-1 {
		t.Fatalf("clampMillis(90d) = %d, want the int32 ceiling; audit_log.duration_ms is an integer "+
			"and an out-of-range value takes the whole row with it", got)
	}
}

// TestTheStartedRowsOwnAnnotationsDoNotLeakIntoTheTerminalRow: _unallowlisted
// on the ok row must describe that row's params, not the started row's.
func TestTheStartedRowsOwnAnnotationsDoNotLeakIntoTheTerminalRow(t *testing.T) {
	log, sink, _ := newTestLogger(t)
	ctx := context.Background()

	ev := stopEvent()
	ev.Params["nonsense"] = "dropped here"
	span := log.Start(ctx, ev)
	span.Done(ctx, nil, Params{"rung": "console_quit"})

	recs := sink.Records()
	if _, ok := recs[0].Params[KeyUnallowlisted]; !ok {
		t.Fatal("the started row should report its own dropped key")
	}
	if v, ok := recs[1].Params[KeyUnallowlisted]; ok {
		t.Fatalf("the terminal row inherited %s = %v from the started row, so it reports a drop "+
			"that happened on a different record", KeyUnallowlisted, v)
	}
}
