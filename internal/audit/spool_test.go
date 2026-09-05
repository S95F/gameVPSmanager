package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/secrets"
)

// testBootID is shaped like the kernel's: a lowercase hex UUID with four
// interior hyphens, which is what makes ParseSpoolName's job non-trivial.
const testBootID = "d0f1a2b3-4c5d-6e7f-8091-a2b3c4d5e6f7"

func mustSpool(t *testing.T, now func() time.Time) (*Spool, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := openSpool(dir, testBootID, now)
	if err != nil {
		t.Fatalf("openSpool: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func backupRecord(t *testing.T, params Params) Record {
	t.Helper()
	sink := NewMemorySink()
	log, _ := quietLogger()
	l := New(sink).WithLogger(log)
	return l.Record(context.Background(), Event{
		Actor:    RootCLI(),
		Action:   BackupCreate,
		Instance: "pz-main",
		Params:   params,
	}, OK, nil)
}

func TestSpoolNameIsUTCAndCarriesTheBootID(t *testing.T) {
	// 00:30 in a +02:00 zone is still the previous UTC day.
	at := time.Date(2026, 9, 6, 0, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	got := SpoolName(testBootID, at)
	want := testBootID + "-20260905" + SpoolExt
	if got != want {
		t.Fatalf("SpoolName = %q, want %q. The box's timezone is whatever the VPS provider set; a "+
			"filename that shifts by a day depending on it makes 'which file holds the incident' "+
			"a question with two answers.", got, want)
	}
}

// TestParseSpoolNameSplitsOnTheLastHyphen is the trap. A boot id is a
// hyphenated UUID, so the obvious split returns the UUID's first group and a
// "date" that is actually its second.
func TestParseSpoolNameSplitsOnTheLastHyphen(t *testing.T) {
	name := SpoolName(testBootID, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))

	gotBoot, gotDay, err := ParseSpoolName(name)
	if err != nil {
		t.Fatalf("ParseSpoolName(%q): %v", name, err)
	}
	if gotBoot != testBootID {
		t.Fatalf("boot id = %q, want %q", gotBoot, testBootID)
	}
	if !gotDay.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day = %v, want 2026-09-05 UTC", gotDay)
	}

	// The naive answer, kept here so the trap cannot quietly stop being one.
	naive := strings.Split(strings.TrimSuffix(name, SpoolExt), "-")
	if naive[0] == testBootID {
		t.Fatal("splitting on the FIRST hyphen now returns the whole boot id, so this fixture no " +
			"longer exercises the mistake it was written for")
	}
	if naive[1] == "20260905" {
		t.Fatal("the naive split happens to find the date; pick a boot id whose second group is not " +
			"a date or this test proves nothing")
	}
}

func TestParseSpoolNameRejectsRubbish(t *testing.T) {
	for _, name := range []string{
		"notes.txt",
		"20260905.ndjson",                                    // no boot id
		testBootID + "-2026099" + SpoolExt,                   // seven digits
		testBootID + "-notaday" + SpoolExt,                   // not a date
		"../../etc/passwd-20260905" + SpoolExt,               // path escape in the boot id
		strings.ToUpper(testBootID) + "-20260905" + SpoolExt, // uppercase is not what the kernel writes
	} {
		if _, _, err := ParseSpoolName(name); err == nil {
			t.Errorf("ParseSpoolName(%q) succeeded", name)
		}
	}
}

// TestValidateBootIDRefusesAnythingThatCouldEscapeTheDirectory: the boot id is
// pasted straight into a filename.
func TestValidateBootIDRefusesAnythingThatCouldEscapeTheDirectory(t *testing.T) {
	for _, bad := range []string{
		"", "..", "../..", "a/b", `a\b`, "d0f1a2b3-4c5d-6e7f-8091-a2b3c4d5e6f", // one short
		"D0F1A2B3-4C5D-6E7F-8091-A2B3C4D5E6F7",
		"d0f1a2b3_4c5d_6e7f_8091_a2b3c4d5e6f7",
	} {
		if err := ValidateBootID(bad); err == nil {
			t.Errorf("ValidateBootID(%q) accepted it", bad)
		}
	}
	if err := ValidateBootID(testBootID); err != nil {
		t.Fatalf("ValidateBootID rejected a real boot id: %v", err)
	}
}

func TestNewSpoolIDIsAUUIDv7(t *testing.T) {
	id := NewSpoolID()
	if len(id) != 36 {
		t.Fatalf("id = %q (%d chars), want 36", id, len(id))
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("id = %q, want 8-4-4-4-12", id)
	}
	if id[14] != '7' {
		t.Fatalf("version nibble = %q in %q, want 7", id[14], id)
	}
	if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("variant nibble = %q in %q, want 8-b (RFC 4122)", v, id)
	}

	// The first 48 bits are Unix milliseconds, which is what makes the id
	// sortable and therefore useful as a reconstruction order.
	ms, err := strconv.ParseInt(id[0:8]+id[9:13], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	if delta := time.Since(time.UnixMilli(ms)); delta < 0 || delta > time.Minute {
		t.Fatalf("the timestamp prefix decodes to %v, which is %v away from now", time.UnixMilli(ms), delta)
	}
}

func TestSpoolIDsAreUniqueAndTimeOrdered(t *testing.T) {
	const n = 20000
	seen := make(map[string]struct{}, n)
	prev := ""
	for range n {
		id := NewSpoolID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate spool id %q; it is the deduplication key for replay", id)
		}
		seen[id] = struct{}{}
		if prev != "" && id[0:12] < prev[0:12] {
			t.Fatalf("timestamp prefix went backwards: %q then %q", prev, id)
		}
		prev = id
	}
}

func TestSpoolRefusesARecordWithNoSpoolID(t *testing.T) {
	s, _ := mustSpool(t, time.Now)

	err := s.Write(context.Background(), Record{Action: InstanceStop, Outcome: OK})
	if err == nil {
		t.Fatal("a record with no spool_id was spooled. audit_log's unique index on spool_id is " +
			"partial (WHERE spool_id IS NOT NULL), so such a row never conflicts and every replay " +
			"of the file inserts it again.")
	}
	if !strings.Contains(err.Error(), "spool_id") {
		t.Fatalf("err = %v, want it to name the missing field", err)
	}
}

// TestOneRecordIsOneLineAndOneNewline: several processes append to this file
// and O_APPEND only makes a single write(2) atomic. A record split across two
// writes would let another process's line land in the middle of it.
func TestOneRecordIsOneLineAndOneNewline(t *testing.T) {
	s, dir := mustSpool(t, time.Now)
	ctx := context.Background()

	rec := backupRecord(t, Params{
		"path":   "/var/backups/gvm/pz-main/x.tar.zst",
		"sha256": "abc",
		"bytes":  int64(1024),
		// A value that would break NDJSON if it were not escaped.
		"target": "line one\nline two\r\n",
	})
	if err := s.Write(ctx, rec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, SpoolName(testBootID, time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(b, []byte("\n")); n != 1 {
		t.Fatalf("the record spans %d newlines; an embedded one would make replay read half a "+
			"record as a whole one", n)
	}
	if b[len(b)-1] != '\n' {
		t.Fatal("the record does not end with its newline, so a reader cannot tell a complete " +
			"record from a torn one")
	}
}

func TestSpoolIsWrittenAsFlatColumns(t *testing.T) {
	s, dir := mustSpool(t, time.Now)
	rec := backupRecord(t, Params{"path": "/p", "sha256": "abc", "bytes": 1})
	rec.Actor = RootCLI().WithPeerUID(0)
	if err := s.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, SpoolName(testBootID, time.Now())))

	var flat map[string]json.RawMessage
	if err := json.Unmarshal(b, &flat); err != nil {
		t.Fatalf("the spool line is not one JSON object: %v", err)
	}
	// The replay INSERT is a straight column mapping, so the keys have to be
	// the columns, not a nested actor object.
	for _, k := range []string{"spool_id", "at", "actor_kind", "actor_via", "actor_peer_uid",
		"instance_name", "action", "params", "outcome"} {
		if _, ok := flat[k]; !ok {
			t.Errorf("no %q key in %s", k, b)
		}
	}
	if _, nested := flat["Actor"]; nested {
		t.Fatal("the actor was encoded as a nested object; the columns are flat")
	}
}

// TestSpooledFlagIsNotSpooled: a record inside the file has by definition not
// been replayed, so writing the marker would make every replayed record claim
// it had already been replayed once before.
func TestSpooledFlagIsNotSpooled(t *testing.T) {
	s, dir := mustSpool(t, time.Now)
	rec := backupRecord(t, Params{"path": "/p", "sha256": "abc", "bytes": 1})
	rec.Spooled = true
	if err := s.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, SpoolName(testBootID, time.Now())))
	if bytes.Contains(b, []byte("spooled")) {
		t.Fatalf("the spooled marker was written to the file: %s", b)
	}
}

func TestSpoolRotatesAtUTCMidnight(t *testing.T) {
	var mu sync.Mutex
	at := time.Date(2026, 9, 5, 23, 59, 59, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return at
	}
	s, dir := mustSpool(t, now)
	ctx := context.Background()

	rec := backupRecord(t, Params{"path": "/p", "sha256": "a", "bytes": 1})
	if err := s.Write(ctx, rec); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	at = at.Add(2 * time.Second) // over midnight
	mu.Unlock()

	rec2 := backupRecord(t, Params{"path": "/p", "sha256": "b", "bytes": 2})
	if err := s.Write(ctx, rec2); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		testBootID + "-20260905" + SpoolExt,
		testBootID + "-20260906" + SpoolExt,
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("no %s: a gvmd that has been up for a week crosses six midnights", want)
		}
	}
}

// TestOpenSpoolCreatesItsDirectoryAndNeverItsParent: an os.MkdirAll that made
// /var/lib/gvm on the way past would create it with this directory's mode, and
// the first symptom would be some other subsystem losing its own state
// directory.
func TestOpenSpoolCreatesItsDirectoryAndNeverItsParent(t *testing.T) {
	root := t.TempDir()

	s, err := openSpool(filepath.Join(root, "spool"), testBootID, time.Now)
	if err != nil {
		t.Fatalf("openSpool under an existing parent: %v", err)
	}
	// Windows refuses to remove an open file, and t.TempDir's cleanup would
	// report that as a test failure.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(root, "not-there")
	if _, err := openSpool(filepath.Join(missing, "spool"), testBootID, time.Now); err == nil {
		t.Fatal("openSpool created its own parent")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatalf("%s was created", missing)
	}
}

func TestOpenSpoolRefusesABadBootID(t *testing.T) {
	if _, err := openSpool(filepath.Join(t.TempDir(), "spool"), "../evil", time.Now); err == nil {
		t.Fatal("openSpool accepted a boot id that is not a UUID")
	}
}

// TestReplayIsIdempotent is the property the whole design turns on: the spool
// is re-read whenever a previous replay did not finish, and a sink that saw
// the same records twice would turn one Postgres outage into duplicated
// history.
func TestReplayIsIdempotent(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()
	for i := range 5 {
		rec := backupRecord(t, Params{"path": fmt.Sprintf("/p%d", i), "sha256": "a", "bytes": i})
		if err := s.Write(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	sink := NewMemorySink()
	first, err := Replay(ctx, path, sink)
	if err != nil {
		t.Fatal(err)
	}
	if first.Records != 5 || sink.Len() != 5 {
		t.Fatalf("first pass: %+v, sink has %d", first, sink.Len())
	}

	second, err := Replay(ctx, path, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.Len() != 5 {
		t.Fatalf("replaying the same file twice produced %d records, want 5", sink.Len())
	}
	if second.Records != 0 || second.Duplicate != 5 {
		t.Fatalf("second pass = %+v, want 0 new and 5 duplicate. The sink deduplicated them either "+
			"way, but counters that call them new would report an outage twice the size it was, "+
			"and spool_replays stores rows_ok and rows_dup as separate columns.", second)
	}
}

// TestReplaySkipsATornTrailingLine: a crash mid-append leaves a partial
// record. It must not abort the replay of everything written before it.
func TestReplaySkipsATornTrailingLine(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()
	for i := range 3 {
		if err := s.Write(ctx, backupRecord(t, Params{"path": fmt.Sprintf("/p%d", i), "sha256": "a", "bytes": i})); err != nil {
			t.Fatal(err)
		}
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Append half of a fourth record, as an interrupted write(2) would.
	whole, _ := json.Marshal(backupRecord(t, Params{"path": "/p3", "sha256": "a", "bytes": 3}))
	if err := appendRaw(path, string(whole[:len(whole)/2])); err != nil {
		t.Fatal(err)
	}

	sink := NewMemorySink()
	res, err := Replay(ctx, path, sink)
	if err != nil {
		t.Fatalf("a torn trailing line aborted the replay: %v", err)
	}
	if res.Records != 3 || sink.Len() != 3 {
		t.Fatalf("%+v: the three complete records before the crash must still land", res)
	}
	if res.Torn != 1 {
		t.Fatalf("Torn = %d, want 1", res.Torn)
	}
}

// TestReplayRecoversALineThatLostOnlyItsNewline: if the crash landed between
// the record's last byte and its newline, the record is whole. A shorter
// truncation cannot balance the braces, so a trailing line that parses at all
// is complete.
func TestReplayRecoversALineThatLostOnlyItsNewline(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()
	if err := s.Write(ctx, backupRecord(t, Params{"path": "/p0", "sha256": "a", "bytes": 0})); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	whole, _ := json.Marshal(backupRecord(t, Params{"path": "/p1", "sha256": "a", "bytes": 1}))
	if err := appendRaw(path, string(whole)); err != nil { // no newline
		t.Fatal(err)
	}

	sink := NewMemorySink()
	res, err := Replay(ctx, path, sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 || res.Recovered != 1 {
		t.Fatalf("%+v: a whole record that lost only its newline should be kept, not discarded", res)
	}
}

// TestReplaySkipsAMalformedInteriorLine: the file is append-only, so aborting
// on one bad line in the middle would make every record after it unreplayable
// forever.
func TestReplaySkipsAMalformedInteriorLine(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()
	if err := s.Write(ctx, backupRecord(t, Params{"path": "/p0", "sha256": "a", "bytes": 0})); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := appendRaw(path, "{not json at all\n"); err != nil {
		t.Fatal(err)
	}
	if err := appendRaw(path, `{"action":"backup.create"}`+"\n"); err != nil { // valid JSON, no spool_id
		t.Fatal(err)
	}

	s2, err := openSpool(filepath.Dir(path), testBootID, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Write(ctx, backupRecord(t, Params{"path": "/p2", "sha256": "a", "bytes": 2})); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()

	sink := NewMemorySink()
	res, err := Replay(ctx, path, sink)
	if err != nil {
		t.Fatalf("a malformed interior line aborted the replay: %v", err)
	}
	if res.Records != 2 {
		t.Fatalf("%+v: the records on either side of the bad line must both land", res)
	}
	if res.Malformed != 2 {
		t.Fatalf("Malformed = %d, want 2 (unparseable, and parseable-but-unidentifiable)", res.Malformed)
	}
}

func TestReplayDeduplicatesWithinOneFile(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()
	rec := backupRecord(t, Params{"path": "/p", "sha256": "a", "bytes": 1})
	if err := s.Write(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, rec); err != nil { // a retry after a torn append
		t.Fatal(err)
	}
	path := s.Path()
	_ = s.Close()

	res, err := Replay(ctx, path, NewMemorySink())
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 1 || res.Repeated != 1 {
		t.Fatalf("%+v, want one record and one in-file repeat", res)
	}
	if res.Duplicate != 0 {
		t.Fatal("the repeat reached the sink; it should never have been offered")
	}
}

// TestATransientSinkErrorAbortsTheReplay: the expected cause is that Postgres
// went away again, and continuing would let the caller mark the file replayed
// with rows missing from it.
func TestATransientSinkErrorAbortsTheReplay(t *testing.T) {
	path := spoolWithRecords(t, 3)
	broken := &failingSink{err: errors.New("connection refused")}

	res, err := Replay(context.Background(), path, broken)
	if err == nil {
		t.Fatal("Replay reported success while the sink was rejecting everything")
	}
	if res.Records != 0 {
		t.Fatalf("Records = %d, want 0", res.Records)
	}
	if broken.count() != 1 {
		t.Fatalf("the sink saw %d records; replay should have stopped at the first failure", broken.count())
	}
}

// TestAPermanentlyRejectedRecordIsSteppedOver: one record Postgres can never
// accept must not block every good record behind it in the same file, forever.
func TestAPermanentlyRejectedRecordIsSteppedOver(t *testing.T) {
	path := spoolWithRecords(t, 3)
	picky := &rejectingSink{mem: NewMemorySink(), rejectNth: 2}

	res, err := Replay(context.Background(), path, picky)
	if err != nil {
		t.Fatalf("a permanently rejected record aborted the replay: %v", err)
	}
	if res.Records != 2 || res.Rejected != 1 {
		t.Fatalf("%+v, want two accepted and one rejected", res)
	}
}

type rejectingSink struct {
	mem       *MemorySink
	rejectNth int
	n         int
}

func (r *rejectingSink) Write(ctx context.Context, rec Record) error {
	r.n++
	if r.n == r.rejectNth {
		return fmt.Errorf("actor_kind is not in actor_t: %w", ErrRejected)
	}
	return r.mem.Write(ctx, rec)
}

// TestParamsRoundTripPreservesALargeInteger: backup.create records archive
// sizes. Decoding params through `any` routes every number via float64, and a
// 2^53 boundary crossed on the replay path writes a byte count into Postgres
// that does not match the file on disk.
func TestParamsRoundTripPreservesALargeInteger(t *testing.T) {
	const big = int64(1) << 62
	rec := backupRecord(t, Params{"path": "/p", "sha256": "a", "bytes": big})

	first, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatalf("a record is not byte-stable across a round trip:\n first: %s\nsecond: %s", first, again)
	}
	if !bytes.Contains(again, []byte(strconv.FormatInt(big, 10))) {
		t.Fatalf("the byte count did not survive: %s", again)
	}

	// What decoding through `any` would have done, kept so the trap stays live.
	var naive map[string]any
	if err := json.Unmarshal(first, &naive); err != nil {
		t.Fatal(err)
	}
	naiveAgain, _ := json.Marshal(naive)
	if bytes.Contains(naiveAgain, []byte(strconv.FormatInt(big, 10))) {
		t.Fatal("decoding through `any` now preserves the integer, so this fixture no longer " +
			"exercises the float64 trap; pick a larger value")
	}
}

// TestASecretValueInAParamSpoolsAsItsRedaction. The allowlist already refuses
// a key called "value", but a call site can put a secrets.Value into a key that
// IS allowed, and the file it lands in is fsynced and later inserted into a
// table gvm cannot delete from.
func TestASecretValueInAParamSpoolsAsItsRedaction(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()

	sink := NewMemorySink()
	log, _ := quietLogger()
	l := New(MultiSink{sink, s}).WithLogger(log)

	l.Record(ctx, Event{
		Actor:    RootCLI(),
		Action:   SecretWrite,
		Instance: "pz-main",
		Params: Params{
			"scope": "instance",
			// A plausible mistake: the operator's new password handed to the
			// field that is supposed to hold the secret's NAME.
			"name": secrets.New("hunter2-correct-horse"),
		},
	}, OK, nil)

	path := s.Path()
	_ = s.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("hunter2")) {
		t.Fatalf("the plaintext reached the spool file:\n%s", b)
	}
	if !bytes.Contains(b, []byte(secrets.Redacted)) {
		t.Fatalf("the redaction is not in the line, so the value went somewhere unexpected:\n%s", b)
	}
}

func TestConcurrentWritersProduceWholeLines(t *testing.T) {
	s, _ := mustSpool(t, time.Now)
	ctx := context.Background()

	const writers, each = 8, 40
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				rec := backupRecord(t, Params{
					"path": fmt.Sprintf("/p/%d/%d", w, i), "sha256": "a", "bytes": i,
				})
				if err := s.Write(ctx, rec); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	path := s.Path()
	_ = s.Close()

	res, err := Replay(ctx, path, NewMemorySink())
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != writers*each {
		t.Fatalf("%+v, want %d whole records", res, writers*each)
	}
	if res.Malformed != 0 || res.Torn != 0 || res.Repeated != 0 {
		t.Fatalf("%+v: lines interleaved", res)
	}
}

func TestListSpoolsOrdersOldestFirstAndCurrentBootLast(t *testing.T) {
	dir := t.TempDir()
	other := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	names := []string{
		SpoolName(testBootID, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)),
		SpoolName(other, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)),
		SpoolName(other, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)),
		"README.txt",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListSpools(dir, testBootID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		SpoolName(other, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)),
		SpoolName(other, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)),
		SpoolName(testBootID, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (README.txt is not a spool file)", got, want)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Fatalf("position %d = %s, want %s: the file still being appended to has to be read "+
				"last, so the greatest number of its records are present", i, filepath.Base(got[i]), want[i])
		}
	}
}

// appendRaw writes bytes to the spool the way a crashing process would: no
// encoding, no newline unless asked.
func appendRaw(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, SpoolPerm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

func spoolWithRecords(t *testing.T, n int) string {
	t.Helper()
	s, _ := mustSpool(t, time.Now)
	for i := range n {
		rec := backupRecord(t, Params{"path": fmt.Sprintf("/p%d", i), "sha256": "a", "bytes": i})
		if err := s.Write(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAReplayedRecordsParamsStillPrintReadably: params that came back from the
// spool hold json.RawMessage, and fmt's default rendering of one is a list of
// byte values. The text slog handler is what a developer running gvmd by hand
// sees.
func TestAReplayedRecordsParamsStillPrintReadably(t *testing.T) {
	path := spoolWithRecords(t, 1)
	sink := NewMemorySink()
	if _, err := Replay(context.Background(), path, sink); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%v", sink.Records()[0].Params)
	if strings.Contains(got, "[") && !strings.Contains(got, `"path"`) {
		t.Fatalf("params rendered as %s, want readable JSON", got)
	}
	if !strings.Contains(got, `"path":"/p0"`) {
		t.Fatalf("params = %s, want the spooled values", got)
	}
}
