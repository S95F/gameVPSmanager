package audit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrRejected marks a record a sink can never accept, no matter how many times
// it is offered.
//
// It exists to keep one bad record from making a whole spool file
// unreplayable. Replay treats every other sink error as transient -- Postgres
// is down, which is the situation the spool exists for -- and aborts so the
// file is retried intact. A record that violates a NOT NULL or an enum
// constraint would then be retried forever, blocking every good record behind
// it in the same file. A sink that knows a record is structurally impossible
// says so with this, and replay steps over it.
var ErrRejected = errors.New("audit: sink permanently rejected the record")

// ErrDuplicate is a sink reporting that it already holds a record with this
// spool_id. It is not a failure -- it is the successful outcome of
// INSERT … ON CONFLICT (spool_id) DO NOTHING.
//
// It exists so replay's counters can be honest. spool_replays records rows_ok
// and rows_dup separately (section 14.4), and a sink that silently swallowed
// the conflict would make every re-replay of a file look like an outage twice
// the size it was. A sink that genuinely cannot tell -- a bare INSERT with no
// RETURNING -- returns nil instead, and its duplicates count as accepted.
var ErrDuplicate = errors.New("audit: the sink already holds this spool_id")

// SpoolExt is the spool file's suffix. NDJSON, not JSON: the file is appended
// to by several processes and read after a crash, and a top-level array would
// have to be closed by whoever wrote last.
const SpoolExt = ".ndjson"

// SpoolPerm is 0600. The spool holds every audit record gvm has not yet
// managed to hand to Postgres, including instance names, operator emails and
// console lines.
const SpoolPerm fs.FileMode = 0o600

// spoolDirPerm is 0700 root:root, matching the rest of /var/lib/gvm's
// non-world-readable state.
const spoolDirPerm fs.FileMode = 0o700

// BootIDPath is the kernel's per-boot UUID.
//
// boot_id and not /etc/machine-id, which is stable across reboots: the whole
// point of putting it in the filename is that a file from a previous boot is
// known to be closed forever, so a replayer can tell "nobody is still
// appending to this" from the name alone. A machine-id would make every file
// on the box look like it might still be growing.
const BootIDPath = "/proc/sys/kernel/random/boot_id"

// bootIDRE is the shape the kernel writes: a lowercase hex UUID.
//
// No version or variant nibble is required. boot_id is 16 random bytes with no
// RFC 4122 fields set, so demanding a version would reject the real thing.
// What this does enforce is that the string cannot contain a path separator, a
// dot or anything else that would let a boot id escape the spool directory
// once it is pasted into a filename.
var bootIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateBootID reports whether s can be used in a spool filename.
func ValidateBootID(s string) error {
	if s == "" {
		return errors.New("audit: boot id is empty")
	}
	if !bootIDRE.MatchString(s) {
		return fmt.Errorf("audit: boot id %q is not a lowercase hex UUID", s)
	}
	return nil
}

// BootID reads the current boot's id.
//
// It is a plain file read rather than a syscall, so it needs no build tag: on
// a machine with no /proc it fails the same way a missing file always fails,
// and the caller decides what to do. This package deliberately invents no
// fallback id -- a synthesised one would change on every process start, and
// the resulting file-per-process defeats the reason the name carries a boot id
// at all.
func BootID() (string, error) {
	b, err := os.ReadFile(BootIDPath)
	if err != nil {
		return "", fmt.Errorf("audit: read boot id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if err := ValidateBootID(id); err != nil {
		return "", err
	}
	return id, nil
}

// spoolDay is the date layout in a spool filename.
const spoolDay = "20060102"

// SpoolName is the file one record written at t belongs in.
//
// UTC, always. The box's timezone is whatever the VPS provider set, and a
// filename that shifts by a day depending on where the machine thinks it is
// makes "which file holds the records from the incident at 01:00" a question
// with two answers.
func SpoolName(bootID string, t time.Time) string {
	return bootID + "-" + t.UTC().Format(spoolDay) + SpoolExt
}

// ParseSpoolName splits a spool filename back into its boot id and its day.
//
// It splits on the LAST hyphen, which is the whole reason this function
// exists: a boot id is a hyphenated UUID, so the obvious
// strings.Split(name, "-")[0] returns the UUID's first group and a date that
// is actually the UUID's second. The date is returned as midnight UTC.
func ParseSpoolName(name string) (bootID string, day time.Time, err error) {
	base := filepath.Base(name)
	if !strings.HasSuffix(base, SpoolExt) {
		return "", time.Time{}, fmt.Errorf("audit: %q is not a spool file (no %s suffix)", base, SpoolExt)
	}
	stem := strings.TrimSuffix(base, SpoolExt)

	cut := strings.LastIndex(stem, "-")
	if cut < 0 {
		return "", time.Time{}, fmt.Errorf("audit: spool name %q has no <boot-id>-<yyyymmdd> separator", base)
	}
	bootID, dayStr := stem[:cut], stem[cut+1:]

	if err := ValidateBootID(bootID); err != nil {
		return "", time.Time{}, fmt.Errorf("audit: spool name %q: %w", base, err)
	}
	day, perr := time.ParseInLocation(spoolDay, dayStr, time.UTC)
	if perr != nil {
		return "", time.Time{}, fmt.Errorf("audit: spool name %q: %q is not a yyyymmdd date", base, dayStr)
	}
	return bootID, day, nil
}

// NewSpoolID returns a UUIDv7: 48 bits of Unix milliseconds, then randomness.
//
// v7 rather than v4 because the id is also the primary sort key of anything
// reconstructed from a spool. Records arrive out of order after an outage --
// three processes appended to the same file -- and a time-ordered id means the
// dashboard can render them in the order they happened without trusting the
// clock skew between them.
func NewSpoolID() string {
	var b [16]byte

	// Big-endian milliseconds in the first six bytes, so lexical order on the
	// hex string is chronological order.
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ms[2:])

	// crypto/rand.Read always fills its argument and never returns an error
	// (it terminates the program if the system source fails), so there is no
	// error path here to get wrong.
	_, _ = rand.Read(b[6:])

	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// Spool is the on-disk NDJSON sink.
//
// One record is one line and one write(2). That is load-bearing twice over.
// Several processes append to the same file -- the root CLI, gvmd and a timer
// can all be auditing at once -- and O_APPEND makes each write atomic with
// respect to the file offset, so whole lines interleave and never halves of
// two records. And on the read side, a line with no terminating newline
// therefore proves a torn write rather than merely a missing separator, which
// is what lets Replay tell a crash apart from corruption.
type Spool struct {
	dir    string
	bootID string
	now    func() time.Time

	mu   sync.Mutex
	f    *os.File
	name string
}

// OpenSpool opens (or creates) the spool for the current day.
//
// dir must already have a parent: this creates the spool directory and never
// its parent. An os.MkdirAll that made /var/lib/gvm on the way past would
// create it 0700 root:root instead of the mode the package's tmpfiles.d
// declares, and the first symptom would be some other subsystem losing access
// to its own state directory.
func OpenSpool(dir, bootID string) (*Spool, error) {
	return openSpool(dir, bootID, time.Now)
}

func openSpool(dir, bootID string, now func() time.Time) (*Spool, error) {
	if err := ValidateBootID(bootID); err != nil {
		return nil, err
	}
	if err := ensureSpoolDir(dir); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, bootID: bootID, now: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rollLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func ensureSpoolDir(dir string) error {
	switch fi, err := os.Stat(dir); {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("audit: spool path %s exists and is not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("audit: stat spool directory %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	if _, err := os.Stat(parent); err != nil {
		return fmt.Errorf("audit: spool parent %s is not there; it belongs to the gvm package's "+
			"tmpfiles.d and creating it here would give it the wrong mode: %w", parent, err)
	}
	if err := os.Mkdir(dir, spoolDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("audit: create spool directory %s: %w", dir, err)
	}
	return nil
}

// rollLocked points the spool at the file for the current day, opening it if
// the name changed. A gvmd that has been up for a week crosses six midnights.
func (s *Spool) rollLocked() error {
	want := SpoolName(s.bootID, s.now())
	if s.f != nil && s.name == want {
		return nil
	}
	if s.f != nil {
		// A close error here is worth reporting but not worth losing the new
		// file over: the data was fsynced at write time, so there is nothing
		// buffered to lose.
		_ = s.f.Close()
		s.f, s.name = nil, ""
	}
	p := filepath.Join(s.dir, want)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, SpoolPerm)
	if err != nil {
		return fmt.Errorf("audit: open spool %s: %w", p, err)
	}
	s.f, s.name = f, want
	return nil
}

// Path is the file currently being appended to.
func (s *Spool) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.name == "" {
		return ""
	}
	return filepath.Join(s.dir, s.name)
}

// Write appends one record and fsyncs it.
//
// The fsync is per record and it is not negotiable: the spool's only job is to
// hold records across the failure of something else, and a record sitting in
// the page cache when the box loses power is a record that was never written.
// The cost is bounded by how often gvm performs a privileged action, which is
// a human-scale rate.
func (s *Spool) Write(_ context.Context, r Record) error {
	if r.SpoolID == "" {
		// Refused rather than fixed up. audit_log's unique index on spool_id
		// is partial -- WHERE spool_id IS NOT NULL -- so a row without one
		// never conflicts, and every replay of this file would insert it
		// again. Silently minting an id here would be worse still: the
		// journald copy and the row would then disagree about the event's
		// identity.
		return errors.New("audit: refusing to spool a record with no spool_id; " +
			"replay deduplicates on it and a row without one is inserted again on every replay")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: encode record %s: %w", r.SpoolID, err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rollLocked(); err != nil {
		return err
	}
	// One Write call, newline included. See the type comment.
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("audit: append to %s: %w", s.name, err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("audit: fsync %s: %w", s.name, err)
	}
	return nil
}

// Close releases the file. Records already written are already durable.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f, s.name = nil, ""
	return err
}

// ReplayResult is what one pass over one spool file did.
type ReplayResult struct {
	File string

	// Records is how many distinct records the sink accepted.
	Records int
	// Duplicate is records the sink already held (ErrDuplicate): the normal
	// result of re-reading a file whose previous replay did not finish.
	Duplicate int
	// Repeated is records whose spool_id appeared earlier in this SAME file,
	// which a retry after a torn append can legitimately produce. They never
	// reach the sink.
	Repeated int
	// Rejected is records the sink said it can never accept (ErrRejected).
	Rejected int
	// Malformed is complete lines that did not parse, or parsed without a
	// spool_id. Skipped, because an append-only file with one bad line in the
	// middle would otherwise be unreplayable forever.
	Malformed int
	// Torn is a trailing line with no newline that could not be recovered:
	// a write interrupted by a crash or a full disk.
	Torn int
	// Recovered is a trailing line with no newline that parsed anyway. It
	// happens when the crash landed between the record's last byte and its
	// newline.
	Recovered int

	Bytes int64
}

// Replay reads a spool file and offers every record to sink.
//
// Idempotency has two halves and this is only one of them. Here: records whose
// spool_id already appeared in this file are skipped, so a torn-then-retried
// append does not double-count. There: Sink.Write is required to be idempotent
// on spool_id, which is what makes replaying the whole file after a failed
// replay safe. Both are needed, because a crash can duplicate within a file
// and an aborted replay duplicates across runs.
//
// A sink error that is neither ErrDuplicate nor ErrRejected aborts the replay
// and is returned with the partial result. That is deliberate: the expected
// cause is that Postgres went away again, and continuing would let the caller
// mark the file replayed with rows missing from it. Pass one sink, not a
// MultiSink: an error joined from several sinks cannot be classified.
func Replay(ctx context.Context, path string, sink Sink) (ReplayResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return ReplayResult{File: path}, fmt.Errorf("audit: open spool %s: %w", path, err)
	}
	defer f.Close()
	return ReplayReader(ctx, path, f, sink)
}

// ReplayReader is Replay over an already-open stream.
func ReplayReader(ctx context.Context, name string, rd io.Reader, sink Sink) (ReplayResult, error) {
	res := ReplayResult{File: name}
	// bufio.Reader, not bufio.Scanner: a Scanner gives up on a line longer
	// than its buffer, and "the replay stopped because one console line was
	// 70 KiB" is a failure nobody would diagnose quickly.
	br := bufio.NewReader(rd)
	seen := make(map[string]struct{})

	for lineNo := 1; ; lineNo++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		line, rerr := br.ReadString('\n')
		res.Bytes += int64(len(line))
		terminated := rerr == nil
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return res, fmt.Errorf("audit: read %s line %d: %w", name, lineNo, rerr)
		}
		if line == "" {
			break // clean end of file
		}

		rec, ok := decodeLine(line)
		switch {
		case ok && !terminated:
			// The crash landed after the record's closing brace and before
			// its newline. A shorter truncation cannot balance the braces, so
			// a line that parses at all is a whole record.
			res.Recovered++
		case !ok && !terminated:
			res.Torn++
			slog.Warn("audit: skipping torn trailing line in spool",
				"file", name, "line", lineNo, "bytes", len(line))
		case !ok:
			res.Malformed++
			slog.Error("audit: skipping unparseable spool line",
				"file", name, "line", lineNo, "bytes", len(line))
		}

		if ok {
			if _, dup := seen[rec.SpoolID]; dup {
				res.Repeated++
			} else {
				seen[rec.SpoolID] = struct{}{}
				rec.Spooled = true
				switch err := sink.Write(ctx, rec); {
				case err == nil:
					res.Records++
				case errors.Is(err, ErrDuplicate):
					res.Duplicate++
				case errors.Is(err, ErrRejected):
					res.Rejected++
					slog.Error("audit: sink permanently rejected a spooled record",
						"file", name, "line", lineNo, "spool_id", rec.SpoolID, "err", err.Error())
				default:
					return res, fmt.Errorf("audit: replay %s line %d (spool_id %s): %w",
						name, lineNo, rec.SpoolID, err)
				}
			}
		}

		if !terminated {
			break // an unterminated line is by definition the last one
		}
	}
	return res, nil
}

// decodeLine parses one spool line. A record with no spool_id is treated as
// unparseable: it cannot be deduplicated, so replaying it would insert it
// again on every pass.
func decodeLine(line string) (Record, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return Record{}, false
	}
	var rec Record
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		return Record{}, false
	}
	if rec.SpoolID == "" {
		return Record{}, false
	}
	return rec, true
}

// ListSpools returns the spool files in dir, oldest day first, with the
// current boot's files last within a day.
//
// The ordering matters for replay: records from an earlier boot can only be
// older, and a file the current boot is still appending to is the one to read
// last so the greatest number of its records are present.
func ListSpools(dir, currentBootID string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("audit: list spool directory %s: %w", dir, err)
	}
	type spoolFile struct {
		name    string
		day     time.Time
		current bool
	}
	var found []spoolFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		bootID, day, err := ParseSpoolName(e.Name())
		if err != nil {
			continue // not ours; the directory is not exclusively spool files
		}
		found = append(found, spoolFile{name: e.Name(), day: day, current: bootID == currentBootID})
	}
	slices.SortFunc(found, func(a, b spoolFile) int {
		if !a.day.Equal(b.day) {
			if a.day.Before(b.day) {
				return -1
			}
			return 1
		}
		switch {
		case a.current == b.current:
			return strings.Compare(a.name, b.name)
		case a.current:
			return 1
		default:
			return -1
		}
	})
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, filepath.Join(dir, f.name))
	}
	return out, nil
}
