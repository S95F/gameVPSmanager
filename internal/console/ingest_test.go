package console

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type ingestHarness struct {
	in    *Ingest
	ring  *Ring
	clock *FakeClock
	raw   *bytes.Buffer
}

func newIngest(t *testing.T, opts IngestOptions) *ingestHarness {
	t.Helper()
	h := &ingestHarness{clock: NewFakeClock(time.Time{}), raw: &bytes.Buffer{}}
	h.ring = NewRing(opts.MaxLine*4+1<<20, 0).WithClock(h.clock.Now)
	opts.Ring = h.ring
	opts.Clock = h.clock
	if opts.Raw == nil {
		opts.Raw = h.raw
	}
	h.in = NewIngest(opts)
	t.Cleanup(func() { _ = h.in.Close() })
	return h
}

func (h *ingestHarness) write(s string) {
	n, err := h.in.Write([]byte(s))
	if err != nil {
		panic(err)
	}
	if n != len(s) {
		panic("short write")
	}
}

func (h *ingestHarness) lines() []Line {
	l, _, _ := h.ring.Snapshot(0, 0)
	return l
}

func (h *ingestHarness) texts() []string { return texts(h.lines()) }

func TestIngestSplitsOnLF(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("first\nsecond\n")
	if got, want := h.texts(), []string{"first", "second"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, l := range h.lines() {
		if l.Partial {
			t.Errorf("line %q marked partial", l.Text)
		}
		if l.Source != SourceGame {
			t.Errorf("line %q has source %v", l.Text, l.Source)
		}
	}
}

func TestIngestNormalisesCRLF(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("windows line\r\nanother\r\n")
	if got, want := h.texts(), []string{"windows line", "another"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, l := range h.lines() {
		if strings.ContainsAny(l.Text, "\r\n") {
			t.Errorf("line %q kept a terminator", l.Text)
		}
	}
}

func TestIngestNormalisesCRLFSplitAcrossReads(t *testing.T) {
	// The pump hands over whatever one read returned, so a CRLF straddling
	// two reads is not a corner case, it is Tuesday.
	h := newIngest(t, IngestOptions{})
	h.write("half\r")
	h.write("\nsecond\n")
	if got, want := h.texts(), []string{"half", "second"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestIngestKeepsBlankLines(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("a\n\n\nb\n")
	if got, want := h.texts(), []string{"a", "", "", "b"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// TestABareCRProgressBarRewritesOneLine is rule 2's real purpose: SteamCMD
// draws a download percentage by returning to the start of the line a few
// hundred times, and a console that turned that into a few hundred rows would
// scroll the error the operator is looking for off the screen.
func TestABareCRProgressBarRewritesOneLine(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("Update state (0x61) downloading, progress: 0.00\r")
	for i := 1; i <= 200; i++ {
		h.write("Update state (0x61) downloading, progress: " + itoa(i) + ".00\r")
	}
	h.write("Success! App '380870' fully installed.\n")

	got := h.texts()
	if len(got) != 2 {
		t.Fatalf("200 progress rewrites produced %d lines: %q", len(got), got)
	}
	if got[0] != "Update state (0x61) downloading, progress: 200.00" {
		t.Errorf("progress line settled on %q", got[0])
	}
	if got[1] != "Success! App '380870' fully installed." {
		t.Errorf("second line = %q", got[1])
	}
	// Rule 2 is explicit that a CR-terminated line is NOT partial: it is a
	// complete line, it just happens to be one that gets overdrawn.
	if h.lines()[0].Partial {
		t.Error("a CR-terminated line was marked partial")
	}
	if h.ring.Head() != 2 {
		t.Errorf("Head = %d; the rewrites must reuse one Seq, not burn 200", h.ring.Head())
	}
}

// TestANewlineEndsTheProgressRunRatherThanOverdrawingIt keeps the bar's last
// frame next to whatever followed it. A terminal would overdraw, and a
// download that stalled at 3% would leave no trace of having stalled.
func TestANewlineEndsTheProgressRunRatherThanOverdrawingIt(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("progress 1%\rprogress 2%\rprogress 3%\rtimed out\nnext\n")
	if got, want := h.texts(), []string{"progress 3%", "timed out", "next"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// TestAnIdleFlushDoesNotOverdrawAFinishedProgressLine: the idle timer's line
// is new output, not another frame of the bar that came before it.
func TestAnIdleFlushDoesNotOverdrawAFinishedProgressLine(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("progress 99%\r")
	h.write("Enter password: ")
	h.clock.Advance(DefaultPartialAfter)
	h.write("hunter2\n")
	if got, want := h.texts(), []string{"progress 99%", "Enter password: hunter2"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// TestACRAfterAnIdleFlushOverdrawsIt: the flushed line is a prefix of what is
// still buffered, so completing it any other way would print the prefix twice.
// It also covers the CR lookahead -- a CR at the end of a read could still
// turn out to be CRLF, and the idle window is what settles it.
func TestACRAfterAnIdleFlushOverdrawsIt(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("downloading 1")
	h.clock.Advance(DefaultPartialAfter)
	if got, want := h.texts(), []string{"downloading 1"}; !eqStrings(got, want) {
		t.Fatalf("after the idle flush: %q, want %q", got, want)
	}

	h.write("0%\r")
	h.clock.Advance(DefaultPartialAfter)
	if got, want := h.texts(), []string{"downloading 10%"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if h.lines()[0].Partial {
		t.Error("a CR-terminated line is complete, not partial")
	}
	if h.ring.Head() != 1 {
		t.Errorf("Head = %d; the flush and its completion are one line", h.ring.Head())
	}
}

func TestATrailingCRIsResolvedByTheNextRead(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("progress 10%\r")
	h.write("progress 20%\r")
	h.write("done\n")
	if got, want := h.texts(), []string{"progress 20%", "done"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestRepeatedCRsDoNotEmitBlankLines(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("\r\r\rreal\n")
	if got, want := h.texts(), []string{"real"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestIngestStripsANSIWhenTheManifestAsksAndKeepsItWhenItDoesNot(t *testing.T) {
	on := newIngest(t, IngestOptions{StripANSI: true})
	on.write("\x1b[31mERROR\x1b[0m: \x1b]0;title\x07disk full\n")
	if got, want := on.texts(), []string{"ERROR: disk full"}; !eqStrings(got, want) {
		t.Fatalf("stripped = %q, want %q", got, want)
	}

	off := newIngest(t, IngestOptions{})
	off.write("\x1b[31mERROR\x1b[0m\n")
	if got, want := off.texts(), []string{"\x1b[31mERROR\x1b[0m"}; !eqStrings(got, want) {
		t.Fatalf("unstripped = %q, want %q", got, want)
	}
}

// TestPartialLineIsFlushedAfterTheIdleWindowAndThenSuperseded is rule 5. An
// interactive prompt has no newline after it by definition, so without this a
// server waiting for input looks identical to a server that has hung.
func TestPartialLineIsFlushedAfterTheIdleWindowAndThenSuperseded(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("Enter admin password: ")

	if got := len(h.lines()); got != 0 {
		t.Fatalf("%d lines before the idle window elapsed", got)
	}
	h.clock.Advance(249 * time.Millisecond)
	if got := len(h.lines()); got != 0 {
		t.Fatalf("%d lines at 249ms", got)
	}

	h.clock.Advance(time.Millisecond)
	lines := h.lines()
	if len(lines) != 1 {
		t.Fatalf("%d lines at 250ms, want 1", len(lines))
	}
	if !lines[0].Partial {
		t.Error("the idle flush is not marked partial")
	}
	if lines[0].Text != "Enter admin password: " {
		t.Errorf("flushed %q", lines[0].Text)
	}
	partialSeq := lines[0].Seq

	// The completed line supersedes it: same Seq, no longer partial, and the
	// ring still holds exactly one line rather than the prompt twice.
	h.write("hunter2\n")
	lines = h.lines()
	if len(lines) != 1 {
		t.Fatalf("%d lines after completion, want 1", len(lines))
	}
	if lines[0].Seq != partialSeq {
		t.Errorf("completed line took Seq %d, want the partial's %d", lines[0].Seq, partialSeq)
	}
	if lines[0].Partial {
		t.Error("the completed line is still marked partial")
	}
	if lines[0].Text != "Enter admin password: hunter2" {
		t.Errorf("completed %q", lines[0].Text)
	}
}

func TestThePartialTimerKeepsRedrawingASlowlyWrittenLine(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("loading")
	h.clock.Advance(DefaultPartialAfter)
	h.write(" mods")
	h.clock.Advance(DefaultPartialAfter)
	h.write(" 1699")
	h.clock.Advance(DefaultPartialAfter)

	lines := h.lines()
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(lines), texts(lines))
	}
	if lines[0].Text != "loading mods 1699" {
		t.Errorf("text = %q", lines[0].Text)
	}
	if !lines[0].Partial {
		t.Error("an unterminated line should still be partial")
	}
}

func TestAnIdleConsoleArmsNoTimer(t *testing.T) {
	// Twenty instances each waking every 250 ms forever, to do nothing, is a
	// cost worth not paying.
	h := newIngest(t, IngestOptions{})
	h.write("complete line\n")
	if got := h.clock.Timers(); got != 0 {
		t.Fatalf("%d timers armed with nothing buffered", got)
	}
	h.write("pending")
	if got := h.clock.Timers(); got != 1 {
		t.Fatalf("%d timers armed with a line pending", got)
	}
	h.write("\n")
	if got := h.clock.Timers(); got != 0 {
		t.Fatalf("%d timers still armed after the line completed", got)
	}
}

func TestPartialFlushIsDisabledByANonPositiveWindow(t *testing.T) {
	h := newIngest(t, IngestOptions{PartialAfter: -1})
	h.write("pending forever")
	h.clock.Advance(time.Hour)
	if got := len(h.lines()); got != 0 {
		t.Fatalf("%d lines with the partial timer off", got)
	}
	h.in.Flush()
	if got, want := h.texts(), []string{"pending forever"}; !eqStrings(got, want) {
		t.Fatalf("after Flush = %q, want %q", got, want)
	}
}

// TestAHugeLineIsForceSplitRatherThanBuffered is rule 4. A mod dumping a 40 MB
// stack trace with no newline in it must not be able to OOM the runner --
// because an OOM-killed runner orphans a live game process.
func TestAHugeLineIsForceSplitRatherThanBuffered(t *testing.T) {
	const (
		maxLine = 64 << 10
		total   = 40 << 20
		chunk   = 1 << 20
	)
	h := newIngest(t, IngestOptions{MaxLine: maxLine})

	blob := bytes.Repeat([]byte("A"), chunk)
	for written := 0; written < total; written += chunk {
		if _, err := h.in.Write(blob); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := h.in.Pending(); got > maxLine {
			t.Fatalf("ingest buffered %d bytes; rule 4 caps it at %d", got, maxLine)
		}
	}
	h.in.Flush()

	if got, want := h.ring.Head(), uint64(total/maxLine); got != want {
		t.Fatalf("Head = %d, want %d chunks", got, want)
	}
	lines := h.lines()
	for i, l := range lines {
		if len(l.Text) != maxLine {
			t.Fatalf("chunk %d has %d bytes, want %d", i, len(l.Text), maxLine)
		}
		if !l.Partial {
			t.Fatalf("chunk %d is not marked partial", i)
		}
	}
	// Each chunk stands alone: superseding them into one line would be a
	// tidier console and a lost stack trace.
	if len(lines) < 2 {
		t.Fatalf("only %d lines retained", len(lines))
	}
	if lines[0].Seq == lines[1].Seq {
		t.Fatal("force-split chunks overdrew each other")
	}
	if got := h.ring.Bytes(); got > h.ring.maxBytes {
		t.Errorf("ring holds %d bytes, over its %d budget", got, h.ring.maxBytes)
	}
}

func TestRawBytesReachTheLogFirstAndVerbatim(t *testing.T) {
	// Rule 1: the on-disk log is the post-mortem, and it keeps the carriage
	// returns and escapes the ring's lines have had normalised away.
	h := newIngest(t, IngestOptions{StripANSI: true})
	const raw = "\x1b[31mboom\x1b[0m\r\nprogress 1\rprogress 2\rdone\n"
	h.write(raw)
	if got := h.raw.String(); got != raw {
		t.Fatalf("log got %q, want the bytes verbatim %q", got, raw)
	}
	if got, want := h.texts(), []string{"boom", "progress 2", "done"}; !eqStrings(got, want) {
		t.Fatalf("ring = %q, want %q", got, want)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestAFailingLogIsAnnouncedOnceAndDoesNotStopTheConsole(t *testing.T) {
	// A full /var/log must not stop a game. It must also not be silent: the
	// operator finds out in the console, which is the thing they are looking
	// at when the log they wanted is not there.
	want := errors.New("no space left on device")
	h := newIngest(t, IngestOptions{Raw: failingWriter{err: want}})

	h.write("first\n")
	h.write("second\n")

	if !errors.Is(h.in.RawErr(), want) {
		t.Fatalf("RawErr = %v, want %v", h.in.RawErr(), want)
	}
	got := h.texts()
	notes := 0
	for _, l := range h.lines() {
		if l.Source == SourceRunner {
			notes++
			if !strings.Contains(l.Text, want.Error()) {
				t.Errorf("annotation %q does not name the error", l.Text)
			}
		}
	}
	if notes != 1 {
		t.Errorf("%d runner annotations, want exactly 1", notes)
	}
	if len(got) != 3 || got[1] != "first" || got[2] != "second" {
		t.Errorf("console output was lost: %q", got)
	}
}

func TestIngestDecodesLatin1WhenTheBytesAreNotUTF8(t *testing.T) {
	// Minecraft's section sign is a bare 0xA7. Handing it to encoding/json as
	// invalid UTF-8 would rewrite it to U+FFFD and lose the colour code.
	h := newIngest(t, IngestOptions{})
	h.write("\xa7cRed \xe9\n")
	if got, want := h.texts(), []string{"§cRed é"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}

	// Valid UTF-8 passes through untouched rather than being double-decoded.
	h.write("déjà vu\n")
	if got := h.texts()[1]; got != "déjà vu" {
		t.Errorf("valid UTF-8 became %q", got)
	}
}

func TestFlushEmitsThePendingLineOnEOF(t *testing.T) {
	// The crash tail's last line often has no newline after it, because the
	// process died mid-write. Losing it is losing the answer.
	h := newIngest(t, IngestOptions{})
	h.write("Exception in thread \"main\"")
	h.in.Flush()
	lines := h.lines()
	if len(lines) != 1 || lines[0].Text != "Exception in thread \"main\"" {
		t.Fatalf("lines = %q", texts(lines))
	}
	if lines[0].Partial {
		t.Error("the final flush should produce a complete line")
	}
	h.in.Flush() // idempotent: nothing buffered, nothing emitted
	if got := len(h.lines()); got != 1 {
		t.Errorf("second Flush produced %d lines", got)
	}
}

func TestFlushDropsATrailingCarriageReturn(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("done\n\r")
	h.in.Flush()
	if got, want := h.texts(), []string{"done"}; !eqStrings(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestWriteAfterCloseIsRefused(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("pending")
	if err := h.in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := h.texts(), []string{"pending"}; !eqStrings(got, want) {
		t.Fatalf("Close did not flush: %q", got)
	}
	if _, err := h.in.Write([]byte("more\n")); !errors.Is(err, ErrIngestClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
	if err := h.in.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestIngestTracksProgressForTheStallDetector(t *testing.T) {
	// Section 11.5: any byte at all counts as progress, which is what keeps a
	// 40-minute workshop download extending the start timeout while a JVM
	// deadlocked with a silent console fails.
	h := newIngest(t, IngestOptions{})
	if !h.in.LastByte().IsZero() {
		t.Fatal("LastByte set before any output")
	}
	h.clock.Advance(time.Minute)
	h.write("partial with no newline")
	if got := h.in.LastByte(); !got.Equal(h.clock.Now()) {
		t.Errorf("LastByte = %v, want %v", got, h.clock.Now())
	}
	if got := h.in.BytesIn(); got != int64(len("partial with no newline")) {
		t.Errorf("BytesIn = %d", got)
	}
}

func TestIngestIsSafeWhileTheTimerFires(t *testing.T) {
	// Write runs on the pump goroutine and the partial timer on another; the
	// race detector is the assertion here.
	h := newIngest(t, IngestOptions{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			h.write("chunk")
			h.write(" more\n")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			h.clock.Advance(DefaultPartialAfter)
		}
	}()
	wg.Wait()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestThePartialTimerGoesQuietWhenNothingIsBeingWritten.
//
// Rule 5's motivating example is a prompt -- "Enter admin password: " -- which
// by definition stays pending until a human answers. onTimer republished the
// same unchanged bytes and re-armed unconditionally, so an unterminated line
// woke every subscriber four times a second forever: a WriteLines frame per
// fire to every attached console, and a re-run of every fail_marker regex in
// the readiness watcher.
func TestThePartialTimerGoesQuietWhenNothingIsBeingWritten(t *testing.T) {
	h := newIngest(t, IngestOptions{})
	h.write("> ")
	h.clock.Advance(DefaultPartialAfter)

	lines := h.lines()
	if len(lines) != 1 || !lines[0].Partial {
		t.Fatalf("the pending line was not published: %+v", lines)
	}
	at := lines[0].At

	for i := 0; i < 10; i++ {
		h.clock.Advance(DefaultPartialAfter)
	}
	if got := h.clock.Timers(); got != 0 {
		t.Errorf("%d timer(s) still armed on a pending line nobody is writing to", got)
	}
	lines = h.lines()
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: the prompt was republished %q", len(lines), texts(lines))
	}
	// The timestamp must not walk forward either, or the console log lies
	// about when the game printed the prompt.
	if !lines[0].At.Equal(at) {
		t.Errorf("the line's timestamp moved from %s to %s while nothing was written", at, lines[0].At)
	}

	// A byte after the quiet period has to wake it up again, or the slow-line
	// behaviour of TestThePartialTimerKeepsRedrawingASlowlyWrittenLine is gone.
	h.write("x")
	if got := h.clock.Timers(); got != 1 {
		t.Fatalf("%d timers armed after a new byte, want 1", got)
	}
	h.clock.Advance(DefaultPartialAfter)
	lines = h.lines()
	if len(lines) != 1 || lines[0].Text != "> x" {
		t.Errorf("lines = %q, want the redrawn prompt", texts(lines))
	}
}
