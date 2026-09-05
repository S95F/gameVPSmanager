package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/secrets"
)

func slogLine(t *testing.T, r Record) map[string]any {
	t.Helper()
	buf := &syncBuf{}
	sink := NewSlogSink(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := sink.Write(context.Background(), r); err != nil {
		t.Fatalf("SlogSink.Write returned %v; slog has no error channel to report", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("the line is not JSON: %v\n%s", err, buf.String())
	}
	return out
}

func TestSlogSinkEmitsTheColumns(t *testing.T) {
	rec := Record{
		SpoolID:       "01920000-0000-7000-8000-000000000001",
		At:            time.Date(2026, 9, 5, 5, 0, 12, 0, time.UTC),
		RequestID:     "r-8fc2",
		ParentSpoolID: "01920000-0000-7000-8000-000000000000",
		Actor:         Timer().WithPeerUID(0),
		InstanceName:  "pz-main",
		Action:        InstanceStop,
		Outcome:       OK,
		DurationMS:    48210,
		Params:        Params{"rung": "console_quit", "degraded": false},
	}
	line := slogLine(t, rec)

	if line["msg"] != SlogMessage {
		t.Fatalf("msg = %v, want %q so an audit record is greppable apart from ordinary logging",
			line["msg"], SlogMessage)
	}
	for k, want := range map[string]any{
		"spool_id":        rec.SpoolID,
		"request_id":      "r-8fc2",
		"parent_spool_id": rec.ParentSpoolID,
		"actor_kind":      "timer",
		"actor_via":       "timer",
		"action":          "instance.stop",
		"outcome":         "ok",
		"instance_name":   "pz-main",
	} {
		if line[k] != want {
			t.Errorf("%s = %v, want %v", k, line[k], want)
		}
	}
	if line["actor_peer_uid"] != float64(0) {
		t.Errorf("actor_peer_uid = %v, want 0 recorded rather than omitted", line["actor_peer_uid"])
	}
	if line["duration_ms"] != float64(48210) {
		t.Errorf("duration_ms = %v", line["duration_ms"])
	}
	params, ok := line["params"].(map[string]any)
	if !ok || params["rung"] != "console_quit" {
		t.Fatalf("params = %#v, want the rung that ended the process", line["params"])
	}
	if _, present := line["error"]; present {
		t.Error("an empty error was emitted; the journal line should carry only what happened")
	}
}

// TestAFailedOutcomeLogsAtErrorLevel: `journalctl -p err` is the first thing
// anyone runs, and it should surface a refused permission or a failed stop
// without the operator needing to know this subsystem exists.
func TestAFailedOutcomeLogsAtErrorLevel(t *testing.T) {
	ok := slogLine(t, Record{SpoolID: "a", Action: InstanceStop, Outcome: OK, At: time.Now()})
	if ok["level"] != "INFO" {
		t.Errorf("a successful action logged at %v, want INFO", ok["level"])
	}
	bad := slogLine(t, Record{
		SpoolID: "b", Action: AuthDenied, Outcome: Failed,
		Error: "forbidden", At: time.Now(),
	})
	if bad["level"] != "ERROR" {
		t.Errorf("a failed action logged at %v, want ERROR", bad["level"])
	}
	if bad["error"] != "forbidden" {
		t.Errorf("error = %v", bad["error"])
	}
}

func TestSpooledRecordsAreMarkedInTheJournal(t *testing.T) {
	live := slogLine(t, Record{SpoolID: "a", Action: JobRun, Outcome: OK, At: time.Now()})
	if _, present := live["spooled"]; present {
		t.Error("a live record claims to have been reconstructed")
	}
	replayed := slogLine(t, Record{SpoolID: "b", Action: JobRun, Outcome: OK, At: time.Now(), Spooled: true})
	if replayed["spooled"] != true {
		t.Error("a replayed record is indistinguishable from one recorded live; every record carries " +
			"a spool_id, so the id alone cannot answer that question")
	}
}

// TestASecretValueIsRedactedThroughEveryHandler. The JSON handler and the text
// handler reach a secrets.Value by different routes -- MarshalJSON and Format
// -- and gvm ships with journald (JSON) while a developer running gvmd by hand
// gets the text handler. Both have to be safe or the redaction is a property
// of the deployment rather than of the type.
func TestASecretValueIsRedactedThroughEveryHandler(t *testing.T) {
	const plaintext = "hunter2-correct-horse"
	rec := Record{
		SpoolID: "01920000-0000-7000-8000-000000000001",
		At:      time.Now(),
		Actor:   RootCLI(),
		Action:  SecretWrite,
		Outcome: OK,
		Params:  Params{"scope": "instance", "name": secrets.New(plaintext)},
	}

	for _, tc := range []struct {
		name string
		make func(*syncBuf) slog.Handler
	}{
		{"json", func(b *syncBuf) slog.Handler { return slog.NewJSONHandler(b, nil) }},
		{"text", func(b *syncBuf) slog.Handler { return slog.NewTextHandler(b, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &syncBuf{}
			if err := NewSlogSink(slog.New(tc.make(buf))).Write(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			if strings.Contains(got, "hunter2") {
				t.Fatalf("the plaintext reached the journal:\n%s", got)
			}
			if !strings.Contains(got, secrets.Redacted) {
				t.Fatalf("the redaction is not in the line, so the value went somewhere unexpected:\n%s", got)
			}
		})
	}
}

// TestSlogSinkNeverReportsAFailure: slog drops silently when its handler
// cannot write, so inventing an error here would make MultiSink report a
// failure that did not happen.
func TestSlogSinkNeverReportsAFailure(t *testing.T) {
	sink := NewSlogSink(slog.New(slog.NewJSONHandler(&syncBuf{}, nil)))
	if err := sink.Write(context.Background(), Record{}); err != nil {
		t.Fatalf("Write returned %v even for an empty record", err)
	}
}

// TestTheJournalCopyIsWrittenWhenTheSpoolCannotBe is section 14.6's promise
// made concrete: journald is the durable off-database copy, and it is most
// valuable exactly when the disposable one has failed.
func TestTheJournalCopyIsWrittenWhenTheSpoolCannotBe(t *testing.T) {
	buf := &syncBuf{}
	journal := NewSlogSink(slog.New(slog.NewJSONHandler(buf, nil)))
	broken := &failingSink{err: errNoSpace}

	log, _ := quietLogger()
	l := New(MultiSink{broken, journal}).WithLogger(log)
	l.Record(context.Background(), stopEvent(), Started, nil)

	if !strings.Contains(buf.String(), "instance.stop") {
		t.Fatalf("the journal copy is missing while the spool was failing:\n%s", buf.String())
	}
}

var errNoSpace = &noSpaceError{}

type noSpaceError struct{}

func (*noSpaceError) Error() string { return "write /var/lib/gvm/spool: no space left on device" }
