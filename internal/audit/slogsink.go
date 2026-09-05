package audit

import (
	"context"
	"log/slog"
	"time"
)

// SlogMessage is the message every audit line carries.
//
// One fixed string, so `journalctl -u gvmd MESSAGE=audit` and a grep for
// `"msg":"audit"` both work, and so an operator reading the journal can tell an
// audit record from gvm's ordinary logging at a glance.
const SlogMessage = "audit"

// SlogSink mirrors every record into log/slog, which under systemd means
// journald.
//
// This is the durable off-database copy, and until Phase 14 it is the only
// sink there is. Its value is highest exactly when everything else has failed:
// journald is already running before gvmd is, it is rotated and size-capped by
// the system, it survives /var/lib/gvm being wiped, and if the operator
// forwards it, the record is off the box before the box catches fire.
//
// The Sink contract asks for idempotency on spool_id. This satisfies it
// trivially and for a different reason than the database sink does: journald
// is a log, and the same record appearing twice in a log is a duplicate line,
// not duplicated history. Replay therefore targets the database sink, not this
// one -- a spool replayed through here would re-narrate an outage that already
// scrolled past.
type SlogSink struct{ log *slog.Logger }

// NewSlogSink writes to l, or to slog.Default() if l is nil.
func NewSlogSink(l *slog.Logger) *SlogSink {
	if l == nil {
		l = slog.Default()
	}
	return &SlogSink{log: l}
}

// Write emits one line.
//
// It always returns nil. slog has no error channel -- a handler that cannot
// write drops the record silently -- so returning an error here would invent a
// failure signal the underlying logger does not provide, and MultiSink would
// report a failure that never happened.
//
// Nothing here needs its own redaction pass. Params arrive already filtered by
// the action allowlist, and a secrets.Value that a call site put in an
// allowlisted key renders as its redaction through every route slog can take:
// JSONHandler reaches MarshalJSON, TextHandler reaches Format.
func (s *SlogSink) Write(ctx context.Context, r Record) error {
	attrs := make([]slog.Attr, 0, 16)
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}

	attrs = append(attrs,
		slog.String("spool_id", r.SpoolID),
		slog.String("at", r.At.Format(time.RFC3339Nano)),
		slog.String("action", string(r.Action)),
		slog.String("outcome", string(r.Outcome)),
	)
	add("request_id", r.RequestID)
	add("parent_spool_id", r.ParentSpoolID)
	add("actor_kind", string(r.Actor.Kind))
	add("actor_via", string(r.Actor.Via))
	add("actor_user_id", r.Actor.UserID)
	add("actor_token_id", r.Actor.TokenID)
	add("actor_ip", r.Actor.IP)
	if r.Actor.PeerUID != nil {
		attrs = append(attrs, slog.Int("actor_peer_uid", *r.Actor.PeerUID))
	}
	add("instance_name", r.InstanceName)
	add("target", r.Target)
	add("error", r.Error)
	if r.DurationMS > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", r.DurationMS))
	}
	if r.Spooled {
		attrs = append(attrs, slog.Bool("spooled", true))
	}
	if len(r.Params) > 0 {
		attrs = append(attrs, slog.Any("params", r.Params))
	}

	// A failed action logs at error level so that `journalctl -p err` -- the
	// first thing anyone runs -- surfaces refused permissions and failed
	// stops without needing to know this subsystem exists.
	level := slog.LevelInfo
	if r.Outcome == Failed {
		level = slog.LevelError
	}
	s.log.LogAttrs(ctx, level, SlogMessage, attrs...)
	return nil
}
