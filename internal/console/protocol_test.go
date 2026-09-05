package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func encodeOne(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(v); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// TestTheDocumentedTranscriptRoundTripsByteForByte holds this codec to section
// 11.4's own example session. Every frame there is decoded into the Go type
// and re-encoded, and the bytes have to come back identical -- so the spec's
// transcript is a test rather than a comment, and a renamed field or a
// reordered struct fails here instead of on a live socket.
func TestTheDocumentedTranscriptRoundTripsByteForByte(t *testing.T) {
	requests := []string{
		`{"op":"hello","client":"gvm-cli/0.1.0"}`,
		`{"op":"tail","from":0,"backlog":500,"follow":true}`,
		`{"op":"send","line":"players","actor":"user:alice@example.com"}`,
		`{"op":"verb","verb":"broadcast","args":{"Msg":"restart in 5 minutes"},"actor":"user:alice@example.com"}`,
		`{"op":"status"}`,
		`{"op":"caps"}`,
		`{"op":"ping"}`,
		`{"op":"stop","reason":"operator requested","actor":"user:alice@example.com"}`,
	}
	// Requests round trip semantically rather than byte for byte: the spec
	// writes `"from":0` explicitly and the encoder omits it, which is the same
	// cursor either way (Seq 0 is never assigned, so it means "the beginning").
	// The load-bearing claims are that every documented frame decodes, that it
	// passes Validate, and that re-encoding it means the same thing.
	for _, want := range requests {
		var r Request
		if err := json.Unmarshal([]byte(want), &r); err != nil {
			t.Fatalf("decode %s: %v", want, err)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("the spec's own frame %s does not validate: %v", want, err)
		}
		var back Request
		if err := json.Unmarshal([]byte(encodeOne(t, r)), &back); err != nil {
			t.Fatalf("re-decode %s: %v", want, err)
		}
		if back.Op != r.Op || back.Client != r.Client || back.From != r.From ||
			back.Backlog != r.Backlog || back.Follow != r.Follow || back.Line != r.Line ||
			back.Verb != r.Verb || back.Reason != r.Reason || back.Actor != r.Actor ||
			len(back.Args) != len(r.Args) {
			t.Errorf("request round trip changed %s into %+v", want, back)
		}
		for k, v := range r.Args {
			if back.Args[k] != v {
				t.Errorf("arg %q became %q", k, back.Args[k])
			}
		}
	}

	events := []string{
		`{"ev":"hello","ok":true,"proto":1,"instance":"pz-main","build":"a1b2c3d","peer":{"uid":0,"write":true}}`,
		`{"ev":"lines","head":18999,"oldest":901,"lines":[{"seq":18442,"t":"2026-09-04T18:23:58Z","src":"game","line":"SERVER STARTED"}]}`,
		`{"ev":"lagged","dropped":137,"resume_from":19140}`,
		`{"ev":"truncated","oldest":901}`,
		`{"ev":"sent","ok":true,"seq":19141}`,
		`{"ev":"verb","ok":true,"channel":"control","output":"Message sent."}`,
		`{"ev":"shutdown","phase":"step","rung":"console","send":"save"}`,
		`{"ev":"shutdown","phase":"done","report":{"final_rung":"console","degraded":false}}`,
	}
	for _, want := range events {
		var ev Event
		if err := json.Unmarshal([]byte(want), &ev); err != nil {
			t.Fatalf("decode %s: %v", want, err)
		}
		if got := encodeOne(t, ev); got != want {
			t.Errorf("event round trip\n got %s\nwant %s", got, want)
		}
	}
}

func TestEventConstructorsMatchTheDocumentedFrames(t *testing.T) {
	at := time.Date(2026, 9, 4, 18, 23, 58, 0, time.UTC)
	tests := []struct {
		ev   Event
		want string
	}{
		{HelloEvent("pz-main", "a1b2c3d", PeerInfo{UID: 0, Write: true}),
			`{"ev":"hello","ok":true,"proto":1,"instance":"pz-main","build":"a1b2c3d","peer":{"uid":0,"write":true}}`},
		{LinesEvent(18999, 901, []Line{{Seq: 18442, At: at, Source: SourceGame, Text: "SERVER STARTED"}}),
			`{"ev":"lines","head":18999,"oldest":901,"lines":[{"seq":18442,"t":"2026-09-04T18:23:58Z","src":"game","line":"SERVER STARTED"}]}`},
		{LaggedEvent(137, 19140), `{"ev":"lagged","dropped":137,"resume_from":19140}`},
		{TruncatedEvent(901), `{"ev":"truncated","oldest":901}`},
		{SentEvent(19141), `{"ev":"sent","ok":true,"seq":19141}`},
		{VerbEvent("control", "Message sent."), `{"ev":"verb","ok":true,"channel":"control","output":"Message sent."}`},
		{ShutdownStepEvent("console", "save"), `{"ev":"shutdown","phase":"step","rung":"console","send":"save"}`},
		{PongEvent(), `{"ev":"pong","ok":true}`},
		{ErrorEvent(CodeDenied, "verb requires uid 0"), `{"ev":"error","ok":false,"code":"denied","error":"verb requires uid 0"}`},
		{StatusEvent(json.RawMessage(`{"state":"ready"}`)), `{"ev":"status","status":{"state":"ready"}}`},
		{CapsEvent(json.RawMessage(`[{"cap":"console.read"}]`)), `{"ev":"caps","caps":[{"cap":"console.read"}]}`},
		{ShutdownDoneEvent(json.RawMessage(`{"degraded":true}`)), `{"ev":"shutdown","phase":"done","report":{"degraded":true}}`},
	}
	for _, tc := range tests {
		if got := encodeOne(t, tc.ev); got != tc.want {
			t.Errorf("\n got %s\nwant %s", got, tc.want)
		}
	}
}

func TestOkIsAbsentRatherThanFalseWhenItDoesNotApply(t *testing.T) {
	// ok:false and "no ok field" are different answers, so OK is a pointer.
	// An event that reported ok:false for every line batch would be alarming
	// and wrong.
	if got := encodeOne(t, LinesEvent(1, 1, nil)); strings.Contains(got, `"ok"`) {
		t.Errorf("lines event carries an ok field: %s", got)
	}
	if !ErrorEvent(CodeInternal, "x").Ok() && ErrorEvent(CodeInternal, "x").OK == nil {
		t.Error("an error event should carry an explicit ok:false")
	}
	if !SentEvent(1).Ok() {
		t.Error("SentEvent should report ok")
	}
	if (Event{Ev: EvLines}).Ok() {
		t.Error("an event with no ok field reported ok")
	}
}

func TestOkPointersAreNotShared(t *testing.T) {
	a, b := SentEvent(1), SentEvent(2)
	*a.OK = false
	if !b.Ok() {
		t.Fatal("two events share one *bool; mutating one rewrote the other")
	}
}

func TestSourceMarshalsAsAName(t *testing.T) {
	for _, tc := range []struct {
		src  Source
		want string
	}{{SourceGame, `"game"`}, {SourceRunner, `"runner"`}, {SourceActor, `"actor"`}} {
		got, err := json.Marshal(tc.src)
		if err != nil {
			t.Fatalf("marshal %v: %v", tc.src, err)
		}
		if string(got) != tc.want {
			t.Errorf("Source(%d) marshalled as %s, want %s", tc.src, got, tc.want)
		}
		var back Source
		if err := json.Unmarshal(got, &back); err != nil || back != tc.src {
			t.Errorf("round trip of %s gave %v (%v)", tc.want, back, err)
		}
	}

	// The set is closed in both directions: a source this build does not know
	// must fail, not decode to "game" and quietly mis-attribute a line.
	var s Source
	for _, bad := range []string{`"web"`, `0`, `"GAME"`, `""`} {
		if err := json.Unmarshal([]byte(bad), &s); !errors.Is(err, ErrBadSource) {
			t.Errorf("unmarshal %s: err = %v, want ErrBadSource", bad, err)
		}
	}
	if _, err := json.Marshal(Source(9)); err == nil {
		t.Error("marshalled an undeclared source")
	}
	if got := Source(9).String(); got != "source(9)" {
		t.Errorf("Source(9).String() = %q", got)
	}
}

func TestLineRoundTripsThroughTheCodec(t *testing.T) {
	want := Line{
		Seq: 19141, At: time.Date(2026, 9, 4, 19, 2, 3, 0, time.UTC),
		Source: SourceActor, Actor: "user:alice@example.com", Text: "players", Partial: true,
	}
	var got Line
	if err := json.Unmarshal([]byte(encodeOne(t, want)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Seq != want.Seq || !got.At.Equal(want.At) || got.Source != want.Source ||
		got.Actor != want.Actor || got.Text != want.Text || got.Partial != want.Partial {
		t.Fatalf("round trip gave %+v, want %+v", got, want)
	}
}

func TestRequestValidation(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want error
	}{
		{"no op", Request{}, ErrEmptyOp},
		{"unknown op", Request{Op: "shutdown"}, &UnknownOpError{Op: "shutdown"}},
		{"hello", Request{Op: OpHello}, nil},
		{"tail", Request{Op: OpTail, Backlog: 500}, nil},
		{"tail with a negative backlog", Request{Op: OpTail, Backlog: -1}, ErrBadBacklog},
		{"tail asking for more than the ring holds", Request{Op: OpTail, Backlog: MaxBacklog + 1}, ErrBadBacklog},
		{"send", Request{Op: OpSend, Line: "players"}, nil},
		{"send nothing", Request{Op: OpSend}, ErrLineEmpty},
		{"send with a newline", Request{Op: OpSend, Line: "save\nquit"}, ErrLineBreak},
		{"send with a carriage return", Request{Op: OpSend, Line: "save\rquit"}, ErrLineBreak},
		{"send with a NUL", Request{Op: OpSend, Line: "save\x00quit"}, ErrLineBreak},
		{"send too long", Request{Op: OpSend, Line: strings.Repeat("a", MaxSendLine+1)}, ErrLineTooLong},
		{"send at the limit", Request{Op: OpSend, Line: strings.Repeat("a", MaxSendLine)}, nil},
		{"send UTF-8", Request{Op: OpSend, Line: "servermsg \"café ouvert\""}, nil},
		{"verb", Request{Op: OpVerb, Verb: "broadcast"}, nil},
		{"verb with no name", Request{Op: OpVerb, Verb: "  "}, ErrNoVerb},
		{"stop", Request{Op: OpStop, Reason: "operator requested"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("Validate = %v, want nil", err)
			case tc.want == nil:
			case err == nil:
				t.Fatalf("Validate = nil, want %v", tc.want)
			default:
				var unknown *UnknownOpError
				if errors.As(tc.want, &unknown) {
					var got *UnknownOpError
					if !errors.As(err, &got) || got.Op != unknown.Op {
						t.Fatalf("Validate = %v, want an UnknownOpError for %q", err, unknown.Op)
					}
					if !strings.Contains(got.Error(), "shutdown") {
						t.Errorf("the error does not name the op: %v", got)
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("Validate = %v, want %v", err, tc.want)
				}
			}
		})
	}
}

func TestOpsAndLifecycleTiers(t *testing.T) {
	// Section 11.4's table: verb and stop are the two ops the peer-cred check
	// restricts to uid 0.
	lifecycle := map[Op]bool{OpVerb: true, OpStop: true}
	for _, op := range Ops() {
		if got := op.Lifecycle(); got != lifecycle[op] {
			t.Errorf("%s.Lifecycle() = %v", op, got)
		}
	}
	if len(Ops()) != 8 {
		t.Errorf("Ops() returned %d ops, want the 8 section 11.4 declares", len(Ops()))
	}
}

func TestDecoderReadsAStreamOfInterleavedFrames(t *testing.T) {
	// tail with follow:true upgrades a connection to a stream, and a client
	// may still interleave send and status on it, so the decoder cannot
	// assume one op per connection.
	stream := strings.Join([]string{
		`{"op":"hello","client":"gvm-cli/0.1.0"}`,
		`{"op":"tail","from":18000,"follow":true}`,
		"", // a bare newline is a legal keepalive
		`{"op":"send","line":"players","actor":"root_cli"}`,
		`{"op":"ping"}`,
	}, "\n") + "\n"

	d := NewDecoder(strings.NewReader(stream))
	var got []Op
	for {
		r, err := d.Request()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		got = append(got, r.Op)
	}
	want := []Op{OpHello, OpTail, OpSend, OpPing}
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("read %v, want %v", got, want)
		}
	}
}

func TestDecoderHandlesCRLFTerminatedFrames(t *testing.T) {
	d := NewDecoder(strings.NewReader("{\"op\":\"ping\"}\r\n"))
	r, err := d.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if r.Op != OpPing {
		t.Fatalf("op = %q", r.Op)
	}
}

func TestDecoderRefusesAnOversizedFrame(t *testing.T) {
	// The web tier is a peer on this socket (D-09), so a frame limit that only
	// exists on the client is not a limit.
	huge := `{"op":"send","line":"` + strings.Repeat("a", MaxRequestFrame) + `"}` + "\n"
	if _, err := NewDecoder(strings.NewReader(huge)).Request(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Request: %v, want ErrFrameTooLarge", err)
	}

	d := NewDecoder(strings.NewReader(huge))
	d.SetMaxFrame(1 << 10)
	if _, err := d.Event(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Event: %v, want ErrFrameTooLarge", err)
	}
}

func TestDecoderReportsATruncatedFinalFrame(t *testing.T) {
	// A frame cut off by a closed socket is a protocol error, not a short
	// read to be silently accepted.
	d := NewDecoder(strings.NewReader(`{"op":"ping"}`))
	if _, err := d.Request(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Request: %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDecoderReportsEOFOnACleanEndOfStream(t *testing.T) {
	d := NewDecoder(strings.NewReader("\n\n"))
	if _, err := d.Request(); !errors.Is(err, io.EOF) {
		t.Fatalf("Request: %v, want io.EOF", err)
	}
}

func TestDecoderRejectsNonJSON(t *testing.T) {
	d := NewDecoder(strings.NewReader("not json at all\n"))
	if _, err := d.Request(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Request: %v", err)
	}
}

func TestDecoderReturnsTheRequestAlongsideAValidationError(t *testing.T) {
	// The server has to name the offending op in the error event it sends
	// back, so a validation failure must not discard what it decoded.
	d := NewDecoder(strings.NewReader(`{"op":"send","line":"a\nb"}` + "\n"))
	r, err := d.Request()
	if !errors.Is(err, ErrLineBreak) {
		t.Fatalf("err = %v, want ErrLineBreak", err)
	}
	if r.Op != OpSend {
		t.Fatalf("the decoded op was lost: %+v", r)
	}
}

func TestEncoderRefusesAFrameOverTheLimit(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetMaxFrame(256)
	if err := e.Event(LinesEvent(1, 1, []Line{{Text: strings.Repeat("x", 512)}})); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode: %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused frame still wrote %d bytes", buf.Len())
	}
}

func TestEncoderLeavesConsoleTextGreppable(t *testing.T) {
	// The far side escapes at render; escaping here too would make the wire
	// unreadable to the socat session section 11.4 promises works.
	got := encodeOne(t, LinesEvent(1, 1, []Line{{Text: `<script>alert("x")</script> & more`}}))
	if !strings.Contains(got, `<script>`) || !strings.Contains(got, ` & `) {
		t.Errorf("wire = %s", got)
	}
	if strings.Contains(got, "\\u003c") || strings.Contains(got, "\\u0026") {
		t.Errorf("HTML escaping is on: %s", got)
	}
}

// TestEncoderIsSafeForConcurrentUse: the runner answers ops on one goroutine
// while the ring fan-out streams lines on another. Two interleaved encodes on
// one socket would produce a frame neither side can parse.
func TestEncoderIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)

	const writers, each = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := e.Event(LinesEvent(uint64(w), uint64(i), []Line{{Text: strings.Repeat("x", 200)}})); err != nil {
					t.Errorf("Encode: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	d := NewDecoder(bytes.NewReader(buf.Bytes()))
	n := 0
	for {
		ev, err := d.Event()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("frame %d is corrupt: %v", n, err)
		}
		if ev.Ev != EvLines || len(ev.Lines) != 1 || len(ev.Lines[0].Text) != 200 {
			t.Fatalf("frame %d decoded as %+v", n, ev)
		}
		n++
	}
	if n != writers*each {
		t.Fatalf("read %d frames, want %d", n, writers*each)
	}
}

func TestEncoderAndDecoderAgreeOnEveryOp(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	for _, op := range Ops() {
		r := Request{Op: op, Client: "gvm-cli", From: 7, Backlog: 10, Follow: true,
			Line: "players", Verb: "broadcast", Args: map[string]string{"Msg": "hi"},
			Reason: "because", Actor: "root_cli"}
		if err := e.Request(r); err != nil {
			t.Fatalf("encode %s: %v", op, err)
		}
	}
	d := NewDecoder(&buf)
	for _, op := range Ops() {
		r, err := d.Request()
		if err != nil {
			t.Fatalf("decode %s: %v", op, err)
		}
		if r.Op != op || r.Actor != "root_cli" || r.Args["Msg"] != "hi" {
			t.Fatalf("%s decoded as %+v", op, r)
		}
	}
}

func TestDecoderAcceptsUnknownFieldsFromANewerPeer(t *testing.T) {
	// Rejecting them would make every field added to a future client a hard
	// incompatibility with a runner that has not been restarted yet.
	d := NewDecoder(strings.NewReader(`{"op":"ping","future_field":42}` + "\n"))
	if _, err := d.Request(); err != nil {
		t.Fatalf("Request: %v", err)
	}
}

func TestSplitBatchKeepsEveryFrameUnderTheLimit(t *testing.T) {
	lines := make([]Line, 500)
	for i := range lines {
		lines[i] = Line{Seq: uint64(i + 1), At: time.Now(), Source: SourceGame, Text: strings.Repeat("m", 900)}
	}
	batches := SplitBatch(lines, 64<<10)
	if len(batches) < 2 {
		t.Fatalf("500 lines of ~1 KiB produced %d batches for a 64 KiB limit", len(batches))
	}

	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetMaxFrame(64 << 10)
	total := 0
	for i, b := range batches {
		if len(b) == 0 {
			t.Fatalf("batch %d is empty", i)
		}
		if err := e.Event(LinesEvent(500, 1, b)); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		total += len(b)
	}
	if total != len(lines) {
		t.Fatalf("batches hold %d of %d lines", total, len(lines))
	}
	// Order is preserved: a console that reordered a stack trace is useless.
	if batches[0][0].Seq != 1 {
		t.Errorf("first batch starts at Seq %d", batches[0][0].Seq)
	}
}

func TestSplitBatchEdgeCases(t *testing.T) {
	if got := SplitBatch(nil, 1024); got != nil {
		t.Errorf("SplitBatch(nil) = %v", got)
	}
	one := []Line{{Text: strings.Repeat("x", 1<<20)}}
	got := SplitBatch(one, 1024)
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("a single oversized line produced %d batches; it must not be dropped", len(got))
	}
	if got := SplitBatch([]Line{{Text: "a"}, {Text: "b"}}, 0); len(got) != 1 {
		t.Errorf("a zero limit should mean the default, not one batch per line: %d batches", len(got))
	}
}

// `reason` is the one free field on a stop frame and the spec's own worked
// example carries a web user's string relayed by gvmd. It ends up in
// Runner.Note, which writes the ring line and console.log, so a line break
// there forges a record of who did what. The neighbouring sinks -- SanitizeActor,
// ValidateSendLine, platform.sanitizeStatus -- all scrub this class already.
func TestStopReasonIsValidatedLikeEveryOtherFreeField(t *testing.T) {
	for _, c := range []struct {
		name    string
		reason  string
		wantErr bool
	}{
		{"the spec's own example", "operator requested", false},
		{"omitted", "", false},
		{"a line feed", "maintenance\n-- gvm: save complete", true},
		{"a carriage return", "maintenance\r-- gvm: save complete", true},
		{"a NUL", "maintenance\x00", true},
		{"over the limit", strings.Repeat("x", MaxReason+1), true},
		{"at the limit", strings.Repeat("x", MaxReason), false},
	} {
		err := Request{Op: OpStop, Reason: c.reason}.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: Validate = %v, wantErr %t", c.name, err, c.wantErr)
		}
	}
}
