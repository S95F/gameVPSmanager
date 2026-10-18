// Package gvmd is the privileged verb gateway behind /run/gvm/admin.sock.
//
// Section 3.2 splits the control plane in two: an internet-reachable web tier
// that holds no master key and cannot touch the host, and this root daemon,
// which can. The verb set is CLOSED (like internal/hostops, which it calls):
// every verb below has a handler and nothing else is reachable over the
// socket, so the privileged surface stays enumerable.
//
// The protocol is NDJSON over SOCK_STREAM, one Request per line, answered by
// one Response, authorized by SO_PEERCRED — root, or the web tier's own
// account. The web tier cannot widen its reach by talking to this socket: the
// verbs are exactly the dashboard's needs and nothing more, and every
// mutation audits as the actor the web tier named.
//
// At startup gvmd never auto-resumes a lease and never actuates the
// reconciler (rule 6), and it never runs migrations (rule 8): it checks
// AssertCompatible and refuses auth verbs while the schema is behind.
package gvmd

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// Verb is one operation the gateway exposes. The set is closed; adding one
// means adding a handler, a Decl row and a test in the same change.
type Verb string

const (
	// VerbStatus returns every instance's lifecycle.Status — the dashboard's
	// instance list and detail pages, derived from the filesystem and systemd,
	// never from Postgres (rule 1).
	VerbStatus Verb = "status"

	// Lifecycle. Each walks the same path as the root CLI — hostops and the
	// shutdown ladder — so a Stop from the dashboard saves the world exactly
	// like `sudo gvm stop` (rule 4).
	VerbStart   Verb = "start"
	VerbStop    Verb = "stop"
	VerbRestart Verb = "restart"

	// VerbConsoleSend writes one line to an instance's console, over the
	// instance's own console socket.
	VerbConsoleSend Verb = "console.send"

	// VerbBackupNow starts gvm-job@<instance>_backup.service through systemd,
	// so the run survives gvmd dying.
	VerbBackupNow Verb = "backup.now"

	// VerbVerifyTOTP checks a login code. TOTP secrets are readable only by
	// gvm_ops (D-27), so the web tier must ask this daemon (rule 9).
	VerbVerifyTOTP Verb = "auth.verify_totp"
)

// Verbs returns the closed set, for the test that holds handlers to it.
func Verbs() []Verb {
	return []Verb{
		VerbStatus, VerbStart, VerbStop, VerbRestart,
		VerbConsoleSend, VerbBackupNow, VerbVerifyTOTP,
	}
}

// Request is one call. Verb decides which other fields are read; a field set
// on a verb that ignores it is refused, for the same reason hostops refuses
// one (a caller that believes it did something it did not).
type Request struct {
	ID   string `json:"id,omitempty"`
	Verb Verb   `json:"verb"`

	// Instance names the target for status (optional there), the lifecycle
	// verbs, console.send and backup.now.
	Instance string `json:"instance,omitempty"`

	// Actor is who asked, for the audit row — "user:alice@example.com" or
	// "api_token:ci". The web tier fills it from its own authenticated
	// session; a root CLI caller is "root".
	Actor string `json:"actor,omitempty"`

	// Line is console.send's one line (at most console.MaxLine bytes).
	Line string `json:"line,omitempty"`

	// Email and Code are auth.verify_totp's inputs.
	Email string `json:"email,omitempty"`
	Code  string `json:"code,omitempty"`
}

// Response is one answer.
type Response struct {
	ID string `json:"id,omitempty"`
	OK bool   `json:"ok"`

	// Code is the machine-readable failure class; see the Code* constants.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	// OpID correlates a 202-style acknowledgement with the journal and audit
	// rows the operation writes. Set on acknowledged lifecycle dispatches.
	OpID string `json:"op_id,omitempty"`

	// Statuses answers VerbStatus.
	Statuses []lifecycle.Status `json:"statuses,omitempty"`

	// Lease is the holder's one-line description on a CodeLeaseHeld refusal,
	// rendered to the operator verbatim.
	Lease string `json:"lease,omitempty"`
}

// Machine-readable response codes. The web tier maps them onto its envelope.
const (
	CodeUnknownVerb  = "unknown_verb"
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeLeaseHeld    = "lease_held" // 409 at the web tier, with the lease line
	CodeTOTPInvalid  = "totp_invalid"
	CodeNoTOTP       = "totp_not_enrolled"
	CodeDegraded     = "degraded" // Postgres unreachable or schema behind
	CodeInternal     = "internal"
)

// newOpID returns a 16-hex-char correlation id for acknowledged dispatches.
func newOpID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "op_" + hex.EncodeToString(b)
}
