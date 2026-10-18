package web

// The live console (§16.6): backlog on attach, Last-Event-ID resume, gap
// markers, the stopped-server banner, and the send box's refusals.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	consolepkg "github.com/S95F/gameVPSmanager/internal/console"
)

// stubStreamer replays a scripted event list and records what it was asked.
type stubStreamer struct {
	from    uint64
	backlog int
	events  []consolepkg.Event
	err     error
}

func (s *stubStreamer) Stream(_ context.Context, _ string, from uint64, backlog int, emit func(consolepkg.Event)) error {
	s.from = from
	s.backlog = backlog
	for _, ev := range s.events {
		emit(ev)
	}
	return s.err
}

func lines(seqs ...uint64) consolepkg.Event {
	ev := consolepkg.Event{Ev: consolepkg.EvLines}
	for _, seq := range seqs {
		ev.Lines = append(ev.Lines, consolepkg.Line{Seq: seq, At: time.Now(), Text: fmt.Sprintf("line %d", seq)})
	}
	return ev
}

func consoleServer(t *testing.T, streamer ConsoleStreamer, dispatcher Dispatcher) http.Handler {
	t.Helper()
	s, _ := newTestServer(t, func(c *Config) {
		c.Console = streamer
		c.Dispatcher = dispatcher
	})
	return s.Handler()
}

func getStream(h http.Handler, lastEventID string) *httptest.ResponseRecorder {
	req := withSession(httptest.NewRequest("GET", "/stream/i/pz-main/console", nil))
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestConsoleStreamHelloThenBacklog: a fresh attach sends hello{state} first,
// asks the socket for the backlog, and ids every line with its sequence so
// the browser can resume.
func TestConsoleStreamHelloThenBacklog(t *testing.T) {
	st := &stubStreamer{events: []consolepkg.Event{lines(1, 2, 3)}}
	rec := getStream(consoleServer(t, st, nil), "")

	body := rec.Body.String()
	if st.from != 0 || st.backlog != consoleBacklog {
		t.Errorf("fresh attach asked from=%d backlog=%d, want 0/%d", st.from, st.backlog, consoleBacklog)
	}
	helloAt := strings.Index(body, "event: hello")
	lineAt := strings.Index(body, "event: line")
	if helloAt < 0 || lineAt < 0 || helloAt > lineAt {
		t.Fatalf("hello must precede the first line:\n%s", body)
	}
	if !strings.Contains(body, "id: 3") || !strings.Contains(body, `"line":"line 3"`) {
		t.Errorf("lines must carry their sequence as the SSE id:\n%s", body)
	}
	if !strings.Contains(body, `"state":"running"`) {
		t.Errorf("hello should carry the instance's plain-language state:\n%s", body)
	}
}

// TestConsoleStreamResume: Last-Event-ID resumes after the last seen line,
// with no backlog replay.
func TestConsoleStreamResume(t *testing.T) {
	st := &stubStreamer{events: []consolepkg.Event{lines(42)}}
	getStream(consoleServer(t, st, nil), "41")
	if st.from != 42 || st.backlog != 0 {
		t.Errorf("resume asked from=%d backlog=%d, want 42/0", st.from, st.backlog)
	}
}

// TestConsoleStreamGapMarker: a sequence jump the ring already dropped shows
// as a gap, never silently (§16.6).
func TestConsoleStreamGapMarker(t *testing.T) {
	st := &stubStreamer{events: []consolepkg.Event{lines(42), lines(50)}}
	rec := getStream(consoleServer(t, st, nil), "41")
	body := rec.Body.String()
	if !strings.Contains(body, "event: gap") || !strings.Contains(body, `"skipped":7`) {
		t.Errorf("a 43..49 gap should render as 7 skipped lines:\n%s", body)
	}

	// The runner's own lagged event is a gap too.
	st = &stubStreamer{events: []consolepkg.Event{{Ev: consolepkg.EvLagged, Dropped: 12, ResumeFrom: 100}}}
	rec = getStream(consoleServer(t, st, nil), "")
	if !strings.Contains(rec.Body.String(), `"skipped":12`) {
		t.Errorf("lagged should render as skipped lines:\n%s", rec.Body.String())
	}
}

// TestConsoleStreamStoppedBanner: a dead socket is a state, not an error.
func TestConsoleStreamStoppedBanner(t *testing.T) {
	st := &stubStreamer{err: fmt.Errorf("%w: dial refused", ErrConsoleDown)}
	rec := getStream(consoleServer(t, st, nil), "")
	if !strings.Contains(rec.Body.String(), "event: stopped") {
		t.Errorf("a down console should banner, not error:\n%s", rec.Body.String())
	}
}

// TestConsoleStreamUnwired: no streamer, one honest comment.
func TestConsoleStreamUnwired(t *testing.T) {
	rec := getStream(consoleServer(t, nil, nil), "")
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Errorf("an unwired stream should say so:\n%s", rec.Body.String())
	}
}

// ── The send box ─────────────────────────────────────────────────────────────

func postConsole(h http.Handler, sessionID, line string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/i/pz-main/console", strings.NewReader("line="+line))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "gvm_sess", Value: sessionID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestConsoleSendRefusesTheLadderWords: a typed quit/stop/shutdown/exit skips
// the save ladder, so it is refused with the Stop-button sentence (§16.6).
func TestConsoleSendRefusesTheLadderWords(t *testing.T) {
	fake := &recordingDispatcher{}
	h := consoleServer(t, &stubStreamer{}, fake)

	for _, word := range []string{"quit", "stop", "shutdown", "exit", "QUIT", "/stop"} {
		rec := postConsole(h, "s1", word)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%q: status = %d, want 303", word, rec.Code)
		}
		if flash := flashValue(rec); !strings.Contains(flash, "Stop button") {
			t.Errorf("%q: flash = %q, want the Stop-button sentence", word, flash)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("a refused word was forwarded: %v", fake.calls)
	}
}

// TestConsoleSendHappyPath: a legitimate command goes through gvmd with the
// actor attached.
func TestConsoleSendHappyPath(t *testing.T) {
	fake := &recordingDispatcher{}
	h := consoleServer(t, &stubStreamer{}, fake)

	rec := postConsole(h, "s1", "say hello everyone")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(fake.calls) != 1 || !strings.Contains(fake.calls[0], "say hello everyone") ||
		!strings.Contains(fake.calls[0], "user:op@example.com") {
		t.Errorf("calls = %v", fake.calls)
	}
}

// TestConsoleSendViewerForbidden: console:write is operator and above.
func TestConsoleSendViewerForbidden(t *testing.T) {
	fake := &recordingDispatcher{}
	h := consoleServer(t, &stubStreamer{}, fake)

	rec := postConsole(h, "s2", "say hi") // s2 is the viewer session
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer send = %d, want 403", rec.Code)
	}
	if len(fake.calls) != 0 {
		t.Errorf("a forbidden send was forwarded: %v", fake.calls)
	}
}

// TestConsoleSendTooLongRefused: one line, at most MaxSendLine bytes.
func TestConsoleSendTooLongRefused(t *testing.T) {
	fake := &recordingDispatcher{}
	h := consoleServer(t, &stubStreamer{}, fake)

	rec := postConsole(h, "s1", strings.Repeat("a", consolepkg.MaxSendLine+1))
	if rec.Code != http.StatusSeeOther || len(fake.calls) != 0 {
		t.Errorf("status=%d calls=%v; an oversized line must be refused before gvmd", rec.Code, fake.calls)
	}
}

// TestConsoleSendUnwired: without a dispatcher the answer is the CLI command.
func TestConsoleSendUnwired(t *testing.T) {
	h := consoleServer(t, &stubStreamer{}, nil)
	rec := postConsole(h, "s1", "say hi")
	if flash := flashValue(rec); !strings.Contains(flash, "sudo gvm console") {
		t.Errorf("flash = %q, want the CLI fallback", flash)
	}
}
