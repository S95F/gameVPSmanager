package console

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log file defaults, from section 11.3's persistence paragraph.
const (
	DefaultLogMaxBytes   int64 = 64 << 20
	DefaultLogKeep             = 5
	DefaultLogBufSize          = 64 << 10
	DefaultLogFlushEvery       = 2 * time.Second
	// DefaultLogPerm is console.log's mode from section 5: readable by the
	// instance user and its group, never world-readable, because a console log
	// contains join passwords typed by mistake.
	DefaultLogPerm os.FileMode = 0o640
	// DefaultLogDirPerm matches LogsDirectoryMode= on the unit.
	DefaultLogDirPerm os.FileMode = 0o750
)

// ErrLogClosed is returned by Write after Close.
var ErrLogClosed = errors.New("console: log file closed")

// LogOptions configures a LogFile. Zero values take the defaults above.
type LogOptions struct {
	Path       string
	MaxBytes   int64
	Keep       int
	BufSize    int
	FlushEvery time.Duration
	Clock      Clock
	Perm       os.FileMode
	DirPerm    os.FileMode

	// NoCompress rotates to a plain file instead of a .gz. The runner never
	// sets it; it exists so a test can read an archive without a decompressor
	// and so a filesystem that is already compressing can skip the work.
	NoCompress bool

	// OnError sees every error the write path swallows. Optional; the runner
	// points it at its journal logger. It is called on the caller's goroutine
	// for write-path errors and on the compressor's for rotation errors, so it
	// must be cheap and must not call back into the LogFile.
	OnError func(error)
}

// LogFile is the rotating console log: the complete, on-disk record that
// outlives the ring.
//
// The console deliberately does not go to journald -- journald drops more than
// 10 000 messages per 30 s per service by default, and a modded server hits
// that during the crash it is being read for, so the post-mortem would have
// holes in exactly the wrong place.
//
// The rule this type is shaped by: ROTATION MUST NOT STALL THE PUMP. The
// inline part of a rotation is a flush, an fsync, a close, a rename and an
// open -- all bounded. Compression is queued to a single background goroutine
// with an unbounded queue, so a slow CPU or a busy disk can never push back
// into Write and from there into the game's stdout.
type LogFile struct {
	opts LogOptions

	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	size   int64
	rotSeq uint64
	timer  Timer
	armed  bool
	closed bool
	// broken records a rotation that renamed the live log away and then could
	// not open a new one -- the log directory disappearing mid-run. Writes
	// then fail loudly instead of accumulating in a buffer attached to a
	// closed descriptor, so the ingest reports it and annotates the ring
	// rather than the operator finding an empty log after the crash.
	broken error

	qmu     sync.Mutex
	pending []string
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup

	// beforeCompress is a test hook, guarded by qmu because the compressor
	// goroutine is already running by the time a test installs one. It runs
	// on that goroutine before any work, which is how logfile_test.go blocks
	// compression and proves that Write keeps going anyway.
	beforeCompress func(string)
}

// OpenLog opens (creating and appending) the console log at opts.Path.
func OpenLog(opts LogOptions) (*LogFile, error) {
	if opts.Path == "" {
		return nil, errors.New("console: OpenLog needs a path")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultLogMaxBytes
	}
	if opts.Keep < 0 {
		opts.Keep = 0
	} else if opts.Keep == 0 {
		opts.Keep = DefaultLogKeep
	}
	if opts.BufSize <= 0 {
		opts.BufSize = DefaultLogBufSize
	}
	if opts.FlushEvery == 0 {
		opts.FlushEvery = DefaultLogFlushEvery
	}
	if opts.Perm == 0 {
		opts.Perm = DefaultLogPerm
	}
	if opts.DirPerm == 0 {
		opts.DirPerm = DefaultLogDirPerm
	}
	opts.Clock = clockOr(opts.Clock)

	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, opts.DirPerm); err != nil {
			return nil, err
		}
	}
	f := &LogFile{
		opts: opts,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := f.openLocked(); err != nil {
		return nil, err
	}
	go f.compressor()
	return f, nil
}

// Write appends raw child output. It is the first thing Ingest does with a
// read, so that the on-disk log is a byte-for-byte record including the
// carriage returns and escape sequences the ring's lines have had normalised
// away.
func (f *LogFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrLogClosed
	}
	if f.broken != nil {
		return 0, f.broken
	}
	if f.size > 0 && f.size+int64(len(p)) > f.opts.MaxBytes {
		if err := f.rotateLocked(); err != nil {
			// A rotation that fails must not cost console output: an oversized
			// log is a nuisance, a missing crash tail is the outage.
			f.onError(fmt.Errorf("console: rotate %s: %w", f.opts.Path, err))
		}
	}
	n, err := f.bw.Write(p)
	f.size += int64(n)
	f.armLocked()
	return n, err
}

// Flush pushes the buffer to the kernel without an fsync.
func (f *LogFile) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrLogClosed
	}
	if err := f.bw.Flush(); err != nil {
		return err
	}
	return f.broken
}

// Path is the live log's path, for status.json.
func (f *LogFile) Path() string { return f.opts.Path }

// Size is how many bytes the live log holds, buffered ones included.
func (f *LogFile) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size
}

// Rotate forces a rotation. `gvm doctor` and the test suite use it; the write
// path reaches it on its own at MaxBytes.
func (f *LogFile) Rotate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrLogClosed
	}
	if f.broken != nil {
		return f.broken
	}
	if f.size == 0 {
		return nil
	}
	return f.rotateLocked()
}

// Close flushes, fsyncs, and waits for queued compressions to finish.
func (f *LogFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	if f.armed {
		f.timer.Stop()
		f.armed = false
	}
	err := f.bw.Flush()
	if f.f != nil {
		if serr := f.f.Sync(); err == nil {
			err = serr
		}
		if cerr := f.f.Close(); err == nil {
			err = cerr
		}
	} else if err == nil {
		err = f.broken
	}
	f.mu.Unlock()

	close(f.stop)
	<-f.done
	return err
}

// Wait blocks until every queued compression has finished. Tests use it; the
// runner does not need it, because Close waits.
func (f *LogFile) Wait() { f.wg.Wait() }

func (f *LogFile) onError(err error) {
	if err != nil && f.opts.OnError != nil {
		f.opts.OnError(err)
	}
}

func (f *LogFile) openLocked() error {
	h, err := os.OpenFile(f.opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, f.opts.Perm)
	if err != nil {
		return err
	}
	size := int64(0)
	if st, serr := h.Stat(); serr == nil {
		size = st.Size()
	}
	f.f = h
	f.size = size
	if f.bw == nil {
		f.bw = bufio.NewWriterSize(h, f.opts.BufSize)
	} else {
		f.bw.Reset(h)
	}
	return nil
}

// rotateLocked does the bounded half of a rotation inline and hands the
// unbounded half to the compressor.
func (f *LogFile) rotateLocked() error {
	if err := f.bw.Flush(); err != nil {
		return err
	}
	// fsync on rotate only. This is the one moment the file is about to be
	// renamed out from under every reader and then rewritten as a .gz, so it
	// is the one moment where losing the page cache loses the log. Ordinary
	// writes stay in the 64 KiB buffer, because fsyncing a game's console
	// output line by line would put the pump on the disk's latency.
	if err := f.f.Sync(); err != nil {
		return err
	}
	if err := f.f.Close(); err != nil {
		return err
	}
	f.f = nil // every path below either reopens or marks the log broken

	// A unique staging name, so a compressor still working on the previous
	// rotation cannot collide with this one.
	f.rotSeq++
	staged := fmt.Sprintf("%s.rot.%d", f.opts.Path, f.rotSeq)
	if err := os.Rename(f.opts.Path, staged); err != nil {
		// Reopen the original: it is still there, and writes must continue.
		if oerr := f.openLocked(); oerr != nil {
			f.breakLocked(oerr)
			return errors.Join(err, oerr)
		}
		return err
	}
	if err := f.openLocked(); err != nil {
		f.breakLocked(err)
		f.enqueue(staged) // the rotated bytes are still worth keeping
		return err
	}
	f.enqueue(staged)
	return nil
}

// breakLocked marks the log unusable after the live file was renamed away and
// could not be replaced.
func (f *LogFile) breakLocked(err error) {
	if f.f != nil {
		_ = f.f.Close() // the handle is unusable from here; do not leak it
		f.f = nil
	}
	f.broken = fmt.Errorf("console: log %s is unwritable: %w", f.opts.Path, err)
	f.bw.Reset(io.Discard)
}

// enqueue hands a staged file to the compressor without ever blocking. The
// queue is a slice rather than a buffered channel precisely so that it cannot
// fill: a full channel would mean either dropping a rotated log or blocking
// the pump, and neither is acceptable.
func (f *LogFile) enqueue(path string) {
	f.wg.Add(1)
	f.qmu.Lock()
	f.pending = append(f.pending, path)
	f.qmu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *LogFile) compressor() {
	defer close(f.done)
	for {
		select {
		case <-f.wake:
			f.drain()
		case <-f.stop:
			f.drain()
			return
		}
	}
}

func (f *LogFile) drain() {
	for {
		f.qmu.Lock()
		if len(f.pending) == 0 {
			f.qmu.Unlock()
			return
		}
		path := f.pending[0]
		f.pending = f.pending[1:]
		hook := f.beforeCompress
		f.qmu.Unlock()

		if hook != nil {
			hook(path)
		}
		f.archive(path)
		f.wg.Done()
	}
}

// setBeforeCompress installs the test hook under the queue's lock.
func (f *LogFile) setBeforeCompress(fn func(string)) {
	f.qmu.Lock()
	f.beforeCompress = fn
	f.qmu.Unlock()
}

// suffix is the extension rotated generations carry.
func (f *LogFile) suffix() string {
	if f.opts.NoCompress {
		return ""
	}
	return ".gz"
}

func (f *LogFile) gen(n int) string {
	return fmt.Sprintf("%s.%d%s", f.opts.Path, n, f.suffix())
}

// archive shifts the kept generations and turns the staged file into
// <path>.1[.gz]. It runs only on the compressor goroutine, so the renames it
// does are serialised with each other by construction.
func (f *LogFile) archive(staged string) {
	if f.opts.Keep <= 0 {
		f.onError(os.Remove(staged))
		return
	}

	if err := os.Remove(f.gen(f.opts.Keep)); err != nil && !os.IsNotExist(err) {
		f.onError(err)
	}
	for i := f.opts.Keep - 1; i >= 1; i-- {
		if err := os.Rename(f.gen(i), f.gen(i+1)); err != nil && !os.IsNotExist(err) {
			f.onError(err)
		}
	}

	dst := f.gen(1)
	if f.opts.NoCompress {
		if err := os.Rename(staged, dst); err != nil {
			f.onError(err)
		}
		return
	}
	if err := gzipFile(staged, dst, f.opts.Perm); err != nil {
		f.onError(fmt.Errorf("console: compress %s: %w", staged, err))
		// Keep the evidence: an uncompressed rotation beats a deleted one.
		if rerr := os.Rename(staged, f.opts.Path+".1"); rerr != nil {
			f.onError(rerr)
		}
		return
	}
	if err := os.Remove(staged); err != nil {
		f.onError(err)
	}
}

// gzipFile writes src to dst.tmp and renames, so a crash mid-compression
// cannot leave a truncated .gz that looks like a complete generation.
func gzipFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, cerr := io.Copy(zw, in)
	if err := zw.Close(); cerr == nil {
		cerr = err
	}
	if err := out.Sync(); cerr == nil {
		cerr = err
	}
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, dst)
}

// armLocked keeps the 2 s flush timer running exactly while there are buffered
// bytes, so an idle instance costs no wakeups at all.
func (f *LogFile) armLocked() {
	if f.opts.FlushEvery <= 0 || f.closed || f.armed || f.bw.Buffered() == 0 {
		return
	}
	if f.timer == nil {
		f.timer = f.opts.Clock.AfterFunc(f.opts.FlushEvery, f.onTimer)
	} else {
		f.timer.Reset(f.opts.FlushEvery)
	}
	f.armed = true
}

func (f *LogFile) onTimer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = false
	if f.closed {
		return
	}
	if err := f.bw.Flush(); err != nil {
		f.onError(err)
	}
}
