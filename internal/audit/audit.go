// Package audit records privileged actions in a form that survives the
// database being down.
//
// That is the whole reason it is a separate package with no store dependency.
// Section 1 promises that killing gvmd, gvm-web and Postgres leaves every game
// server running and every root CLI verb working; a `sudo gvm stop pz-main`
// executed during a Postgres outage is still a privileged action against
// somebody's world, and an audit trail that only exists when the database is
// up is an audit trail that is missing exactly when something has gone wrong.
//
// So there are two off-database copies and they are not redundant with each
// other. journald (slogsink.go) is the DURABLE one: it is rotated, it is
// off-box if the operator forwards it, and it survives /var/lib/gvm being
// wiped. The NDJSON spool (spool.go) is the RECONCILING one: it exists only so
// Postgres can catch up later, it lives under /var/lib/gvm which section 5
// calls disposable, and losing it costs history in the dashboard and nothing
// else.
//
// Two properties hold this together. Every record carries a UUIDv7 spool_id
// assigned once, before any sink sees it, so the journald line, the spool line
// and the eventual audit_log row are the same event written three times.
// And params are filtered by a per-action ALLOWLIST (actions.go), never a
// denylist, because audit_log is append-only by grant -- the application roles
// hold SELECT and INSERT and nothing else -- so a secret that reaches it
// cannot be deleted by the software that put it there.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Outcome mirrors the outcome_t enum.
//
// There is no "updated": a long operation writes a second row pointing at the
// first through ParentSpoolID. A PZ stop legitimately takes three minutes, and
// a table the application can UPDATE is not an audit log.
type Outcome string

const (
	// Started is the first row of a two-row operation. It is written before
	// the work begins, so an operation that crashes the daemon still leaves
	// a trace of having been attempted.
	Started Outcome = "started"
	// OK is the terminal success row.
	OK Outcome = "ok"
	// Failed is the terminal failure row. It is spelled Failed rather than
	// Error so the constant does not read like the Record.Error field.
	Failed Outcome = "error"
)

// Valid reports whether the outcome is one the outcome_t enum accepts.
func (o Outcome) Valid() bool { return o == Started || o == OK || o == Failed }

// Terminal reports whether this outcome closes an operation.
func (o Outcome) Terminal() bool { return o == OK || o == Failed }

// ActorKind mirrors the actor_t enum: who did it.
type ActorKind string

const (
	ActorUser    ActorKind = "user"
	ActorToken   ActorKind = "token"
	ActorSystem  ActorKind = "system"
	ActorTimer   ActorKind = "timer"
	ActorRootCLI ActorKind = "root_cli"
)

// Via is which process asserted the actor.
//
// It is a separate column from ActorKind because gvmd can verify that a
// CLAIMED actor holds a role and cannot verify that they authenticated
// (section 3.2). A compromised gvm-web can claim to be the owner; recording
// that the claim arrived over the gvm-web socket, next to the socket peer uid,
// is what makes that claim auditable rather than merely trusted.
type Via string

const (
	ViaCLI    Via = "cli"
	ViaWeb    Via = "gvm-web"
	ViaDaemon Via = "gvmd"
	ViaTimer  Via = "timer"
)

// Actor is the identity half of a record: the actor_* columns.
//
// It is embedded into Record rather than nested so the NDJSON keys are flat
// and match the audit_log columns one for one, which is what lets the replay
// INSERT be a straight column mapping with no reshaping step to get wrong.
type Actor struct {
	Kind    ActorKind `json:"actor_kind"`
	UserID  string    `json:"actor_user_id,omitempty"`
	TokenID string    `json:"actor_token_id,omitempty"`
	IP      string    `json:"actor_ip,omitempty"`
	Via     Via       `json:"actor_via"`

	// PeerUID is the SO_PEERCRED uid of the socket peer, when there was one.
	//
	// It is a pointer because uid 0 is root and is the single most
	// interesting value this field can hold: an int whose zero value means
	// "unknown" would report every actor with no socket as root. That is not
	// hypothetical -- platform.Peer.Root() shipped with exactly that bug
	// because Peer{} has UID 0.
	PeerUID *int `json:"actor_peer_uid,omitempty"`
}

// RootCLI is the actor for `sudo gvm`. Root is not RBAC-gated (section 15.1):
// anyone with root can already read master.key and every game secret, so
// gating them would be theatre. They are recorded instead.
func RootCLI() Actor { return Actor{Kind: ActorRootCLI, Via: ViaCLI} }

// Timer is the actor for a systemd timer firing -- the nightly update, the
// backup, gvm-guard.
func Timer() Actor { return Actor{Kind: ActorTimer, Via: ViaTimer} }

// System is gvm acting on its own behalf: a chained job, a reconcile.
func System(via Via) Actor { return Actor{Kind: ActorSystem, Via: via} }

// User is an authenticated person. id is users.id.
func User(id string, via Via) Actor { return Actor{Kind: ActorUser, UserID: id, Via: via} }

// Token is an API token acting for its owning user.
func Token(userID, tokenID string, via Via) Actor {
	return Actor{Kind: ActorToken, UserID: userID, TokenID: tokenID, Via: via}
}

// WithPeerUID records the socket peer's uid.
//
// The caller checks platform.Peer.Identified() first; this package does not
// import platform, so that an audit record can be built in a context that has
// no host at all.
func (a Actor) WithPeerUID(uid int) Actor { a.PeerUID = &uid; return a }

// WithIP records the client address. Section 17: an empty trusted_proxies list
// is the default, so this is the socket address unless an edge was configured.
func (a Actor) WithIP(ip string) Actor { a.IP = ip; return a }

// Validate reports why the actor could not be written to audit_log.
func (a Actor) Validate() error {
	switch a.Kind {
	case ActorUser, ActorToken, ActorSystem, ActorTimer, ActorRootCLI:
	case "":
		return errors.New("actor_kind is empty")
	default:
		return fmt.Errorf("actor_kind %q is not in actor_t", a.Kind)
	}
	switch a.Via {
	case ViaCLI, ViaWeb, ViaDaemon, ViaTimer:
	case "":
		return errors.New("actor_via is empty")
	default:
		return fmt.Errorf("actor_via %q is not one of cli|gvm-web|gvmd|timer", a.Via)
	}
	if a.Kind == ActorToken && a.TokenID == "" {
		return errors.New("actor_kind is token but actor_token_id is empty")
	}
	if a.Kind == ActorUser && a.UserID == "" {
		return errors.New("actor_kind is user but actor_user_id is empty")
	}
	return nil
}

// Params is the allowlist-filtered params column.
//
// The value type is `any` so a call site can pass the number, list or bool it
// already has. Two things make that safe: Redact drops every key nobody
// declared, and MarshalJSON below never lets one bad value take the record
// down with it.
type Params map[string]any

// MarshalJSON encodes params one key at a time.
//
// Encoding the map as a unit would mean one unmarshalable value -- a func, a
// channel, a NaN from some ratio -- fails the whole record, and a lost audit
// record is a worse outcome than a lost field. Keys are sorted so the same
// params always produce the same bytes, which is what makes the round-trip
// test in spool_test.go able to compare bytes at all.
func (p Params) MarshalJSON() ([]byte, error) {
	if len(p) == 0 {
		return []byte("{}"), nil
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil { // a Go map key is a string; this cannot fail.
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')

		vb, err := json.Marshal(p[k])
		if err != nil {
			// The type, never the error text. A custom Marshaler's error
			// message is written by the type being marshalled, and the one
			// type in gvm most likely to fail here is one wrapping something
			// it should not be printing.
			vb, _ = json.Marshal(fmt.Sprintf("<unencodable %T>", p[k]))
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// UnmarshalJSON keeps every value as raw JSON.
//
// Decoding into `any` would route every number through float64, and
// backup.create records archive sizes: a 2^53 boundary crossed silently on the
// replay path would write a byte count into Postgres that does not match the
// file on disk. json.RawMessage re-encodes verbatim, so a record that is read
// and written again is byte-identical to the one that was spooled.
func (p *Params) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(Params, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	*p = out
	return nil
}

// String renders params as their JSON.
//
// It exists for the text-handler path. A record that came back from the spool
// holds json.RawMessage values -- that is what keeps large integers exact
// across a round trip -- and fmt's default rendering of one is a list of byte
// values. It is also the safe rendering: it goes through MarshalJSON, so a
// secrets.Value inside is redacted here exactly as it is everywhere else.
func (p Params) String() string {
	b, err := p.MarshalJSON()
	if err != nil {
		return "{}"
	}
	return string(b)
}

// maxErrorBytes bounds Record.Error. A steamcmd or systemd failure can carry
// several megabytes of stderr, and this string is fsynced.
const maxErrorBytes = 4 << 10

// Record is one audit_log row.
//
// Field order and JSON names follow the table in section 14.4 so the replay
// INSERT is a column mapping. The one field that is not a column is Spooled.
type Record struct {
	// SpoolID is a UUIDv7 assigned once, by the Logger, before any sink sees
	// the record. It is the deduplication key for replay
	// (INSERT … ON CONFLICT (spool_id) DO NOTHING) and the correlation key
	// between the journald copy and the database row.
	SpoolID string `json:"spool_id"`

	At time.Time `json:"at"`

	// RequestID equals the X-Request-Id header and audit_log.request_id, so a
	// support thread that starts with a 403's request id ends in a grep.
	RequestID string `json:"request_id,omitempty"`

	// ParentSpoolID points at the `started` row of this operation.
	//
	// The column in Postgres is parent_id, a bigint, but that id does not
	// exist yet when the terminal row is spooled during an outage. Carrying
	// the parent's spool_id instead lets replay resolve the pair after the
	// fact, in either arrival order.
	ParentSpoolID string `json:"parent_spool_id,omitempty"`

	// Embedded and untagged, which is how encoding/json flattens it: the
	// actor_* keys sit at the top level of the line, exactly as they sit in
	// the table.
	Actor

	InstanceName string  `json:"instance_name,omitempty"`
	Action       Action  `json:"action"`
	Target       string  `json:"target,omitempty"`
	Params       Params  `json:"params"`
	Outcome      Outcome `json:"outcome"`
	Error        string  `json:"error,omitempty"`

	// DurationMS is set on terminal rows only. A measured sub-millisecond
	// duration rounds up to 1: in this encoding 0 and "not measured" are the
	// same value, and a `started` row is the one that legitimately has none.
	DurationMS int64 `json:"duration_ms,omitempty"`

	// Spooled is not spooled -- that is the point of the "-" tag. It marks a
	// record that reached a sink through replay rather than live, so history
	// can distinguish "recorded live" from "reconstructed" (section 14.6).
	// A live record also carries a spool_id, so the id alone cannot answer
	// that question.
	Spooled bool `json:"-"`
}

// Validate reports every reason the record would be rejected or would be
// useless, joined.
//
// Nothing calls this to decide whether to write: an invalid record is still
// evidence, and refusing to write it would convert a call-site bug into a
// silent gap. The Logger records the failure inside the record instead, under
// KeyInvalid, and logs it at error level.
func (r Record) Validate() error {
	var errs []error
	if r.SpoolID == "" {
		errs = append(errs, errors.New("spool_id is empty"))
	}
	if r.At.IsZero() {
		errs = append(errs, errors.New("at is zero"))
	}
	if err := r.Actor.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !r.Outcome.Valid() {
		errs = append(errs, fmt.Errorf("outcome %q is not in outcome_t", r.Outcome))
	}
	if !Known(r.Action) {
		errs = append(errs, fmt.Errorf("action %q has no allowlist entry", r.Action))
	} else {
		spec, _ := SpecFor(r.Action)
		if spec.Instance && r.InstanceName == "" {
			errs = append(errs, fmt.Errorf("action %q requires instance_name", r.Action))
		}
		if spec.Target && r.Target == "" {
			errs = append(errs, fmt.Errorf("action %q requires target", r.Action))
		}
		if r.Outcome == OK {
			if missing := missingRequired(r.Action, r.Params); len(missing) > 0 {
				errs = append(errs, fmt.Errorf("action %q is missing required params on success: %s",
					r.Action, strings.Join(missing, ", ")))
			}
		}
	}
	if r.Outcome == Failed && r.Error == "" {
		errs = append(errs, errors.New("outcome is error but no error text was recorded"))
	}
	return errors.Join(errs...)
}

// Sink is where a record goes.
//
// Write MUST be idempotent on Record.SpoolID. That is not a nicety: replay
// re-reads a whole spool file whenever the previous replay did not finish, so
// a sink that appends blindly turns one Postgres outage into duplicated
// history. The database sink gets this from
// INSERT … ON CONFLICT (spool_id) DO NOTHING plus the partial unique index on
// spool_id; MemorySink below gets it from a map, for the same reason and with
// the same contract.
//
// A sink that can tell it already held the record returns ErrDuplicate, which
// is a report rather than a failure; one that cannot tell returns nil and its
// duplicates count as accepted. A sink that knows a record can never be stored
// -- it violates a constraint no retry will satisfy -- returns ErrRejected, so
// that one poison record does not block every good one behind it in the file.
//
// Write must not block indefinitely. It is called on the path of an operator's
// `gvm stop`, and an audit sink that hangs is an audit sink that stops the
// operator from stopping their server.
type Sink interface {
	Write(ctx context.Context, r Record) error
}

// MultiSink fans one record out to every sink.
type MultiSink []Sink

// Write delivers to every sink even when one fails, and returns the failures
// joined.
//
// Never short-circuit here. The reason the spool and journald are both wired
// up is that they fail independently: /var/lib/gvm filling is precisely the
// moment the journald copy has to still be written.
func (m MultiSink) Write(ctx context.Context, r Record) error {
	var errs []error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Write(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MemorySink keeps records in memory, deduplicated by spool_id.
//
// It lives outside _test.go because it is the reference implementation of the
// Sink idempotency contract: the property "replaying a file twice yields no
// duplicates" is a property of the sink and the replay loop together, and the
// packages that grow their own audit call sites later need something to assert
// against that already behaves the way Postgres will.
type MemorySink struct {
	mu   sync.Mutex
	seen map[string]struct{}
	recs []Record
}

// NewMemorySink returns an empty sink.
func NewMemorySink() *MemorySink {
	return &MemorySink{seen: make(map[string]struct{})}
}

// Write stores the record, or reports ErrDuplicate if its spool_id is already
// stored -- the same answer a real INSERT … ON CONFLICT DO NOTHING gives when
// it is asked whether it inserted anything.
//
// A record with no spool_id is refused rather than stored: it could never be
// deduplicated, so accepting it would quietly break the contract this type
// exists to model.
func (m *MemorySink) Write(_ context.Context, r Record) error {
	if r.SpoolID == "" {
		return errors.New("audit: record has no spool_id, so it cannot be deduplicated")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.seen[r.SpoolID]; dup {
		return ErrDuplicate
	}
	m.seen[r.SpoolID] = struct{}{}
	m.recs = append(m.recs, r)
	return nil
}

// Records returns a copy of everything written, in arrival order.
func (m *MemorySink) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.recs))
	copy(out, m.recs)
	return out
}

// Len is the number of distinct records written.
func (m *MemorySink) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

// Event is what a call site describes. The Logger supplies everything else:
// the spool id, the clock, the redaction and the outcome.
type Event struct {
	Actor     Actor
	Action    Action
	Instance  string
	Target    string
	RequestID string
	Params    Params
}

// Logger turns Events into Records and hands them to sinks.
//
// None of its methods return an error, and that is deliberate. An audit sink
// failing must not fail the operation being audited: refusing to stop a game
// server because /var/lib/gvm is full would convert a disk problem into an
// outage, and journald already holds the copy that matters. Sink failures go
// to the error handler instead, where they are visible.
//
// The one case that genuinely must fail the operation -- a pure database
// mutation whose audit row is written inside the same transaction, so that a
// grant change without a record is impossible -- is not this path. That is the
// Postgres sink called directly inside the transaction (section 14.8).
type Logger struct {
	sink Sink

	// now is injectable for tests. It is the wall clock: At is a timestamp a
	// human reads next to journald, not an interval.
	now func() time.Time

	// log receives the audit system's own complaints: a dropped param, an
	// invalid record, a sink failure. It is not where records go.
	log *slog.Logger

	mu   sync.Mutex
	open map[string]*Span
}

// New returns a Logger writing to sink.
func New(sink Sink) *Logger {
	return &Logger{
		sink: sink,
		now:  time.Now,
		log:  slog.Default(),
		open: make(map[string]*Span),
	}
}

// WithLogger sets where the audit system reports its own problems. Call it
// during construction: it is not safe to call once records are being written.
func (l *Logger) WithLogger(s *slog.Logger) *Logger {
	if s != nil {
		l.log = s
	}
	return l
}

// WithClock replaces the clock. Tests only, and only during construction.
func (l *Logger) WithClock(now func() time.Time) *Logger {
	if now != nil {
		l.now = now
	}
	return l
}

// Record writes a single row and returns it, so the caller can correlate.
func (l *Logger) Record(ctx context.Context, ev Event, out Outcome, err error) Record {
	return l.write(ctx, ev, out, err, "", nil)
}

// Denied records a refused authorization.
//
// It is its own helper because the shape is fixed and the fields are easy to
// lose: the action that was REFUSED goes in target (auth.denied is the action
// here), and the permission and effective role are the two things that make a
// denial diagnosable rather than merely annoying. Passing them as arguments
// rather than as a params map is what guarantees they are present.
//
// A forbidden action is exactly as interesting as an allowed one, which is why
// this exists at all.
func (l *Logger) Denied(ctx context.Context, actor Actor, instance, attempted, permission, effectiveRole string) Record {
	return l.write(ctx, Event{
		Actor:    actor,
		Action:   AuthDenied,
		Instance: instance,
		Target:   attempted,
		Params: Params{
			"permission":     permission,
			"effective_role": effectiveRole,
		},
	}, Failed, fmt.Errorf("forbidden: %s requires %s, actor has %s", attempted, permission, effectiveRole), "", nil)
}

// Span is an operation whose outcome arrives later.
type Span struct {
	l       *Logger
	started Record

	// begin carries a monotonic reading. Subtracting wall clocks would let an
	// NTP step during a three-minute PZ shutdown produce a negative duration.
	begin time.Time

	mu   sync.Mutex
	done bool
}

// Start writes the `started` row and returns the span that must close it.
//
// The started row is committed FIRST for side-effectful operations, so an
// operation that crashes the daemon still leaves a trace of having begun.
//
// Prefer Do: a Span that is never closed leaves an operation that appears to
// have started and never finished, which is indistinguishable from a crash.
// Start exists for the cases Do cannot express -- an operation whose outcome
// arrives on another goroutine, or from the runner over a socket.
func (l *Logger) Start(ctx context.Context, ev Event) *Span {
	rec := l.write(ctx, ev, Started, nil, "", nil)
	s := &Span{l: l, started: rec, begin: time.Now()}
	l.mu.Lock()
	l.open[rec.SpoolID] = s
	l.mu.Unlock()
	return s
}

// SpoolID is the started row's id, which is also this operation's
// parent_spool_id.
func (s *Span) SpoolID() string { return s.started.SpoolID }

// Done writes the terminal row. It is idempotent: the second call is ignored,
// because a deferred Done plus an explicit one on the success path is a
// natural way to write a call site and must not produce two terminal rows.
//
// params are merged over the started row's params, so a caller only has to
// supply what it learned -- the rung a stop ended on, the sha of the archive
// it wrote.
func (s *Span) Done(ctx context.Context, err error, params Params) Record {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return Record{}
	}
	s.done = true
	s.mu.Unlock()

	s.l.mu.Lock()
	delete(s.l.open, s.started.SpoolID)
	s.l.mu.Unlock()

	out := OK
	if err != nil {
		out = Failed
	}
	dur := time.Since(s.begin)
	return s.l.write(ctx, Event{
		Actor:     s.started.Actor,
		Action:    s.started.Action,
		Instance:  s.started.InstanceName,
		Target:    s.started.Target,
		RequestID: s.started.RequestID,
		Params:    mergeParams(s.started.Params, params),
	}, out, err, s.started.SpoolID, &dur)
}

// Do is Start plus a guaranteed Done, including on a panic.
//
// This is the form to reach for. A call site that uses it cannot forget to
// close the record, cannot close it twice, and cannot lose it to a panic
// halfway through a rebuild -- and the panic is re-raised afterwards, so the
// audit record never changes the program's behaviour.
func (l *Logger) Do(ctx context.Context, ev Event, fn func(ctx context.Context) (Params, error)) error {
	span := l.Start(ctx, ev)

	var (
		params Params
		err    error
	)
	defer func() {
		if p := recover(); p != nil {
			span.Done(ctx, fmt.Errorf("panic: %v", p), params)
			panic(p)
		}
		span.Done(ctx, err, params)
	}()

	params, err = fn(ctx)
	return err
}

// OpenSpans returns the `started` rows of every span not yet closed.
//
// It is how a forgotten Done becomes visible: gvm doctor and the tests can ask
// whether anything has been "in progress" since before the process's own
// start, instead of waiting for somebody to notice a dangling row in the
// dashboard months later.
func (l *Logger) OpenSpans() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.open))
	for _, s := range l.open {
		out = append(out, s.started)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.SpoolID, b.SpoolID) })
	return out
}

// CloseAll closes every open span with an error.
//
// gvmd calls it on shutdown. The alternative is a row that says an operation
// started and nothing else, forever -- which is the correct record for a
// crash, and the wrong one for an orderly stop that simply did not wait.
func (l *Logger) CloseAll(ctx context.Context, err error) int {
	l.mu.Lock()
	spans := make([]*Span, 0, len(l.open))
	for _, s := range l.open {
		spans = append(spans, s)
	}
	l.mu.Unlock()

	n := 0
	for _, s := range spans {
		if rec := s.Done(ctx, err, nil); rec.SpoolID != "" {
			n++
		}
	}
	return n
}

// write is the single construction point for every Record in gvm.
// dur is a pointer because "measured, and it was fast" and "not measured" are
// different facts and the difference is not expressible in a duration. Windows'
// monotonic clock is coarse enough that time.Since across a sub-millisecond
// operation returns exactly zero, so a `dur > 0` test here would silently drop
// duration_ms for every quick operation on a platform with a low-resolution
// clock -- which is most of them, some of the time.
func (l *Logger) write(ctx context.Context, ev Event, out Outcome, err error, parent string, dur *time.Duration) Record {
	params, dropped := Redact(ev.Action, ev.Params)

	rec := Record{
		SpoolID:       NewSpoolID(),
		At:            l.now().UTC(),
		RequestID:     ev.RequestID,
		ParentSpoolID: parent,
		Actor:         ev.Actor,
		InstanceName:  ev.Instance,
		Action:        ev.Action,
		Target:        ev.Target,
		Params:        params,
		Outcome:       out,
	}
	if err != nil {
		rec.Error = truncateError(err.Error())
	}
	if out.Terminal() && dur != nil {
		rec.DurationMS = clampMillis(*dur)
	}

	if len(dropped) > 0 {
		l.log.LogAttrs(ctx, slog.LevelError, "audit params dropped: not on the action's allowlist",
			slog.String("action", string(rec.Action)),
			slog.String("spool_id", rec.SpoolID),
			slog.Any("dropped", dropped))
	}

	if verr := rec.Validate(); verr != nil {
		// The record is written anyway; see Validate. Recording the problem
		// inside the row is what makes the call-site bug findable from the
		// same place the missing evidence is noticed.
		rec.Params[KeyInvalid] = splitJoined(verr)
		l.log.LogAttrs(ctx, slog.LevelError, "audit record is invalid and was written anyway",
			slog.String("action", string(rec.Action)),
			slog.String("spool_id", rec.SpoolID),
			slog.String("problems", verr.Error()))
	}

	if l.sink != nil {
		// A sink reporting it already has this record is not a failure, and
		// on the live path it means something re-offered a record rather than
		// that anything went wrong.
		if werr := l.sink.Write(ctx, rec); werr != nil && !errors.Is(werr, ErrDuplicate) {
			l.log.LogAttrs(ctx, slog.LevelError, "audit sink failed; the operation continues",
				slog.String("action", string(rec.Action)),
				slog.String("spool_id", rec.SpoolID),
				slog.String("err", werr.Error()))
		}
	}
	return rec
}

// mergeParams overlays b on a without mutating either. A nil result is never
// returned, so Redact always has a map to filter.
func mergeParams(a, b Params) Params {
	out := make(Params, len(a)+len(b))
	for k, v := range a {
		if strings.HasPrefix(k, reservedPrefix) {
			// Audit's own annotations are recomputed for the terminal row;
			// carrying the started row's forward would report a drop that
			// happened on a different record.
			continue
		}
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// splitJoined turns errors.Join's newline-separated text into a list, so the
// _invalid param is a JSON array rather than one string with embedded
// newlines.
func splitJoined(err error) []string {
	parts := strings.Split(err.Error(), "\n")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// clampMillis converts a duration to the int32 range audit_log.duration_ms
// accepts. 2^31 ms is 24.8 days, which no gvm operation reaches; the clamp is
// there so a clock anomaly cannot produce a value Postgres rejects, taking the
// whole row with it.
func clampMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	switch {
	case ms < 1:
		return 1
	case ms > math.MaxInt32:
		return math.MaxInt32
	default:
		return ms
	}
}

// truncateError bounds the error text on a rune boundary.
func truncateError(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… [truncated, %d bytes total]", s[:cut], len(s))
}
