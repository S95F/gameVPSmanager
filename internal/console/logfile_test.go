package console

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func readGz(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", path, err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func openLog(t *testing.T, opts LogOptions) (*LogFile, *FakeClock, string) {
	t.Helper()
	clock := NewFakeClock(time.Time{})
	dir := t.TempDir()
	if opts.Path == "" {
		opts.Path = filepath.Join(dir, "console.log")
	}
	opts.Clock = clock
	if opts.OnError == nil {
		opts.OnError = func(err error) { t.Errorf("LogFile reported: %v", err) }
	}
	f, err := OpenLog(opts)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, clock, opts.Path
}

func TestLogWritesAreBufferedUntilFlushed(t *testing.T) {
	const payload = "SERVER STARTED\n"
	f, _, path := openLog(t, LogOptions{})
	mustWrite(t, f, []byte(payload))
	if got := len(readAll(t, path)); got != 0 {
		t.Fatalf("%d bytes hit the disk before a flush; the 64 KiB buffer is not doing its job", got)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := string(readAll(t, path)); got != payload {
		t.Fatalf("file = %q, want %q", got, payload)
	}
	if got := f.Size(); got != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", got, len(payload))
	}
}

func TestTheTwoSecondTimerFlushesTheBuffer(t *testing.T) {
	// Without this an idle server's last words sit in a 64 KiB buffer until
	// the buffer fills, which for a quiet game is never.
	f, clock, path := openLog(t, LogOptions{})
	if _, err := f.Write([]byte("last words\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	clock.Advance(1999 * time.Millisecond)
	if got := len(readAll(t, path)); got != 0 {
		t.Fatalf("flushed early: %d bytes", got)
	}
	clock.Advance(time.Millisecond)
	if got, want := string(readAll(t, path)), "last words\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	// Nothing buffered means no armed timer: an idle instance costs nothing.
	if got := clock.Timers(); got != 0 {
		t.Errorf("%d timers armed with an empty buffer", got)
	}
}

func TestRotationCompressesTheOldGenerationAndKeepsWriting(t *testing.T) {
	f, _, path := openLog(t, LogOptions{MaxBytes: 1024, BufSize: 64})
	first := bytes.Repeat([]byte("a"), 1000)
	if _, err := f.Write(first); err != nil {
		t.Fatalf("Write: %v", err)
	}
	second := bytes.Repeat([]byte("b"), 100)
	if _, err := f.Write(second); err != nil { // 1000+100 > 1024, so this rotates
		t.Fatalf("Write: %v", err)
	}
	f.Wait()
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := readAll(t, path); !bytes.Equal(got, second) {
		t.Fatalf("live log has %d bytes, want the %d written after the rotation", len(got), len(second))
	}
	if got := readGz(t, path+".1.gz"); !bytes.Equal(got, first) {
		t.Fatalf("archive has %d bytes, want %d", len(got), len(first))
	}
	if got := f.Size(); got != int64(len(second)) {
		t.Errorf("Size = %d after rotation, want %d", got, len(second))
	}
	// No staging files left behind.
	matches, _ := filepath.Glob(path + ".rot.*")
	if len(matches) != 0 {
		t.Errorf("staging files left: %v", matches)
	}
}

// TestRotationNeverStallsThePump is the rule this type is shaped by. gzip runs
// off the ingest path, so a busy CPU or a slow disk cannot push back into
// Write and from there into the game's stdout.
func TestRotationNeverStallsThePump(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 8)

	clock := NewFakeClock(time.Time{})
	path := filepath.Join(t.TempDir(), "console.log")
	f, err := OpenLog(LogOptions{Path: path, MaxBytes: 512, BufSize: 64, Clock: clock,
		OnError: func(err error) { t.Errorf("LogFile reported: %v", err) }})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	f.setBeforeCompress(func(p string) {
		entered <- p
		<-release
	})
	defer func() {
		close(release)
		_ = f.Close()
	}()

	blob := bytes.Repeat([]byte("x"), 400)

	// Rotate once and let the compressor block on it.
	mustWrite(t, f, blob)
	mustWrite(t, f, blob)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the compressor never started")
	}

	// While it is wedged, the pump keeps going -- including through three more
	// rotations, which must queue rather than block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 6; i++ {
			mustWrite(t, f, blob)
		}
		_ = f.Flush()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked behind a wedged compressor")
	}

	f.qmu.Lock()
	queued := len(f.pending)
	f.qmu.Unlock()
	if queued == 0 {
		t.Error("rotations did not queue while the compressor was blocked")
	}
	if got := len(readAll(t, path)); got == 0 {
		t.Error("nothing reached the live log while the compressor was blocked")
	}
}

func mustWrite(t *testing.T, w io.Writer, p []byte) {
	t.Helper()
	if _, err := w.Write(p); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestRotationKeepsExactlyNGenerations(t *testing.T) {
	f, _, path := openLog(t, LogOptions{MaxBytes: 100, Keep: 3, BufSize: 16})
	for i := 0; i < 6; i++ {
		mustWrite(t, f, bytes.Repeat([]byte{byte('0' + i)}, 90))
		f.Wait()
	}
	f.Wait()

	for i := 1; i <= 3; i++ {
		if _, err := os.Stat(f.gen(i)); err != nil {
			t.Errorf("generation %d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(f.gen(4)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("generation 4 exists with Keep=3: %v", err)
	}

	// Generation 1 is the most recent rotation, and the shift must not have
	// scrambled the order.
	if got := readGz(t, f.gen(1)); got[0] != '4' {
		t.Errorf("generation 1 starts with %q, want the second-newest content", got[0])
	}
	if got := readGz(t, f.gen(3)); got[0] != '2' {
		t.Errorf("generation 3 starts with %q", got[0])
	}
	if got := readAll(t, path); len(got) != 0 && got[0] != '5' {
		t.Errorf("live log starts with %q", got[0])
	}
}

func TestRotationWithoutCompression(t *testing.T) {
	f, _, path := openLog(t, LogOptions{MaxBytes: 100, BufSize: 16, NoCompress: true})
	mustWrite(t, f, bytes.Repeat([]byte("a"), 90))
	mustWrite(t, f, bytes.Repeat([]byte("b"), 90))
	f.Wait()
	if got := readAll(t, path+".1"); len(got) != 90 || got[0] != 'a' {
		t.Fatalf("plain rotation produced %d bytes starting %q", len(got), got[:1])
	}
	if _, err := os.Stat(path + ".1.gz"); !errors.Is(err, os.ErrNotExist) {
		t.Error("NoCompress still produced a .gz")
	}
}

func TestKeepZeroDiscardsRotations(t *testing.T) {
	f, _, path := openLog(t, LogOptions{MaxBytes: 100, Keep: -1, BufSize: 16})
	mustWrite(t, f, bytes.Repeat([]byte("a"), 90))
	mustWrite(t, f, bytes.Repeat([]byte("b"), 90))
	f.Wait()
	matches, _ := filepath.Glob(path + ".*")
	if len(matches) != 0 {
		t.Fatalf("Keep=0 left %v", matches)
	}
}

func TestReopeningAppendsAndPicksUpTheExistingSize(t *testing.T) {
	// RuntimeDirectoryPreserve keeps the log across a restart, and a runner
	// that reset its size counter would rotate at the wrong moment forever.
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")
	clock := NewFakeClock(time.Time{})

	first, err := OpenLog(LogOptions{Path: path, Clock: clock})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	mustWrite(t, first, []byte("run one\n"))
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := OpenLog(LogOptions{Path: path, Clock: clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if got := second.Size(); got != int64(len("run one\n")) {
		t.Errorf("Size = %d after reopen, want %d", got, len("run one\n"))
	}
	mustWrite(t, second, []byte("run two\n"))
	if err := second.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := string(readAll(t, path)), "run one\nrun two\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestExplicitRotateIsANoOpOnAnEmptyLog(t *testing.T) {
	f, _, path := openLog(t, LogOptions{})
	if err := f.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	f.Wait()
	if _, err := os.Stat(path + ".1.gz"); !errors.Is(err, os.ErrNotExist) {
		t.Error("rotated an empty log")
	}
}

func TestCloseFlushesWaitsAndIsIdempotent(t *testing.T) {
	clock := NewFakeClock(time.Time{})
	path := filepath.Join(t.TempDir(), "console.log")
	f, err := OpenLog(LogOptions{Path: path, MaxBytes: 92, BufSize: 16, Clock: clock})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	mustWrite(t, f, bytes.Repeat([]byte("a"), 90))
	mustWrite(t, f, []byte("tail\n"))
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := string(readAll(t, path)); got != "tail\n" {
		t.Errorf("live log = %q", got)
	}
	// Close waits for the queued compression, so the archive is readable the
	// moment Close returns.
	if got := len(readGz(t, path+".1.gz")); got != 90 {
		t.Errorf("archive has %d bytes", got)
	}
	if _, err := f.Write([]byte("x")); !errors.Is(err, ErrLogClosed) {
		t.Errorf("Write after Close: %v", err)
	}
	if err := f.Flush(); !errors.Is(err, ErrLogClosed) {
		t.Errorf("Flush after Close: %v", err)
	}
	if err := f.Rotate(); !errors.Is(err, ErrLogClosed) {
		t.Errorf("Rotate after Close: %v", err)
	}
}

func TestAFailedRotationDoesNotCostConsoleOutput(t *testing.T) {
	// An oversized log is a nuisance; a missing crash tail is the outage. If
	// the rename cannot happen, the runner keeps writing to the file it has.
	var reported []error
	clock := NewFakeClock(time.Time{})
	path := filepath.Join(t.TempDir(), "console.log")
	f, err := OpenLog(LogOptions{Path: path, MaxBytes: 100, BufSize: 16, Clock: clock,
		OnError: func(err error) { reported = append(reported, err) }})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer f.Close()

	// Block the staging name with a directory, so the rename must fail.
	if err := os.Mkdir(path+".rot.1", 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	mustWrite(t, f, bytes.Repeat([]byte("a"), 90))
	mustWrite(t, f, []byte("the line that matters\n"))
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(reported) != 1 {
		t.Fatalf("%d errors reported, want 1: %v", len(reported), reported)
	}
	got := readAll(t, path)
	if !bytes.HasSuffix(got, []byte("the line that matters\n")) {
		t.Fatalf("output written after the failed rotation was lost; log has %d bytes", len(got))
	}
	if len(got) != 90+len("the line that matters\n") {
		t.Errorf("log has %d bytes, want everything written", len(got))
	}
}

func TestAnUnwritableLogFailsLoudlyAndTheIngestSaysSo(t *testing.T) {
	// Reached when a rotation renames the live log away and the replacement
	// cannot be opened. Writes must then fail rather than accumulate in a
	// buffer bound to a closed descriptor, so that the ingest annotates the
	// ring and the operator finds out while they are still looking at it.
	f, _, _ := openLog(t, LogOptions{})
	boom := errors.New("log directory vanished")
	f.mu.Lock()
	f.breakLocked(boom)
	f.mu.Unlock()

	if _, err := f.Write([]byte("x")); !errors.Is(err, boom) {
		t.Errorf("Write = %v, want the break reason", err)
	}
	if err := f.Flush(); !errors.Is(err, boom) {
		t.Errorf("Flush = %v", err)
	}
	if err := f.Rotate(); !errors.Is(err, boom) {
		t.Errorf("Rotate = %v", err)
	}

	// The ingest turns that into one annotation and keeps the console alive.
	ring := NewRing(0, 0)
	in := NewIngest(IngestOptions{Ring: ring, Raw: f})
	if _, err := in.Write([]byte("still running\n")); err != nil {
		t.Fatalf("ingest Write: %v", err)
	}
	if !errors.Is(in.RawErr(), boom) {
		t.Errorf("RawErr = %v", in.RawErr())
	}
	lines, _, _ := ring.Snapshot(0, 0)
	if len(lines) != 2 || lines[0].Source != SourceRunner || lines[1].Text != "still running" {
		t.Fatalf("ring = %+v", lines)
	}

	// Close still shuts the compressor goroutine down cleanly.
	if err := f.Close(); !errors.Is(err, boom) {
		t.Errorf("Close = %v, want the break reason", err)
	}
}

func TestOpenLogCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pz-main", "console.log")
	f, err := OpenLog(LogOptions{Path: path, Clock: NewFakeClock(time.Time{})})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer f.Close()
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("log directory: %v", err)
	}
}

func TestOpenLogNeedsAPath(t *testing.T) {
	if _, err := OpenLog(LogOptions{}); err == nil {
		t.Fatal("OpenLog with no path returned no error")
	}
}

// TestIngestDrivesTheLogFile wires the two halves together the way the runner
// does: raw bytes to disk, normalised lines to the ring.
func TestIngestDrivesTheLogFile(t *testing.T) {
	clock := NewFakeClock(time.Time{})
	path := filepath.Join(t.TempDir(), "console.log")
	lf, err := OpenLog(LogOptions{Path: path, Clock: clock})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	ring := NewRing(0, 0).WithClock(clock.Now)
	in := NewIngest(IngestOptions{Ring: ring, Raw: lf, Clock: clock, StripANSI: true})

	const raw = "\x1b[32m[INFO]\x1b[0m boot\r\nprogress 1\rprogress 2\rSERVER STARTED\n"
	if _, err := in.Write([]byte(raw)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatalf("ingest Close: %v", err)
	}
	if err := lf.Close(); err != nil {
		t.Fatalf("log Close: %v", err)
	}

	if got := string(readAll(t, path)); got != raw {
		t.Errorf("log = %q, want the raw bytes", got)
	}
	lines, _, _ := ring.Snapshot(0, 0)
	if got, want := texts(lines), []string{"[INFO] boot", "progress 2", "SERVER STARTED"}; !eqStrings(got, want) {
		t.Errorf("ring = %q, want %q", got, want)
	}
}
