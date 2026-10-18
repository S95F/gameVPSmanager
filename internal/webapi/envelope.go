// Package webapi defines the one JSON error envelope every gvm HTTP surface
// speaks (§16.3), the X-Request-Id plumbing that ties a response to its
// audit_log row, and the single "datastore down" 503 template.
//
// It sits below internal/web and internal/auth so both render errors the same
// way, and it deliberately knows nothing about sessions, tokens or Postgres:
// anything here must be safe to call from any handler, in any auth state.
package webapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
)

// Code is a machine-readable error code the CLI and the dashboard branch on.
// The set is closed (§16.3); a handler that needs a new failure mode adds it
// here, never as a bare string.
type Code string

const (
	CodeUnauthenticated      Code = "unauthenticated"
	CodeForbidden            Code = "forbidden"
	CodeNotFound             Code = "not_found"
	CodeConflict             Code = "conflict"
	CodeConfirmationRequired Code = "confirmation_required"
	CodePlanExpired          Code = "plan_expired"
	CodePreconditionFailed   Code = "precondition_failed"
	CodeRateLimited          Code = "rate_limited"
	CodeDegraded             Code = "degraded" // Postgres down; game servers unaffected
	CodeInternal             Code = "internal"

	// CodeNotImplemented is U3's honest refusal over HTTP: the route exists,
	// the engine behind it is not in this build, and a 501 with this code can
	// never be mistaken for success (web-dashboard-2, day2-ops-6).
	CodeNotImplemented Code = "not_implemented"
)

// Codes returns the closed set, for the tests that keep handlers inside it.
func Codes() []Code {
	return []Code{
		CodeUnauthenticated, CodeForbidden, CodeNotFound, CodeConflict,
		CodeConfirmationRequired, CodePlanExpired, CodePreconditionFailed,
		CodeRateLimited, CodeDegraded, CodeInternal, CodeNotImplemented,
	}
}

// ErrorBody is the inner object of the envelope.
type ErrorBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
	// RequestID always equals the X-Request-Id response header and
	// audit_log.request_id, so one string finds the row (§16.3).
	RequestID string `json:"request_id"`
}

// Envelope is the wire shape: {"error": {...}}.
type Envelope struct {
	Error ErrorBody `json:"error"`
}

// requestIDKey is the unexported context key for the request id.
type requestIDKey struct{}

// RequestIDHeader is the header carrying the request id, set on every
// response by the RequestID middleware.
const RequestIDHeader = "X-Request-Id"

// RequestID wraps next so every request carries a request id: generated here,
// set as the X-Request-Id response header, and stored in the context so
// WriteError can echo it in the envelope.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFromContext returns the id stored by the RequestID middleware, or
// "" when the middleware did not run (tests calling a handler directly).
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// crockford is Crockford base32: no I, L, O or U, so an id survives being
// read over the phone at 4am.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newRequestID returns "req_" + 16 Crockford characters (80 random bits).
// Entropy exhaustion is unrecoverable, and a request id must never be empty,
// so failure falls back to all-zero rather than panicking mid-request.
func newRequestID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	out := make([]byte, 16)
	// 10 bytes = 80 bits = 16 five-bit groups.
	var acc uint
	var bits int
	i := 0
	for _, by := range b {
		acc = acc<<8 | uint(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[i] = crockford[(acc>>uint(bits))&31]
			i++
		}
	}
	return "req_" + string(out)
}

// WriteError writes the envelope with the given status. The request id comes
// from the context; handlers never pass one.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code Code, message string, detail map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(Envelope{Error: ErrorBody{
		Code:      code,
		Message:   message,
		Detail:    detail,
		RequestID: RequestIDFromContext(r.Context()),
	}})
}

// IsAPIPath reports whether the request is for the JSON surface, which gets
// envelopes, as opposed to a browser page, which gets HTML.
func IsAPIPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// ServeDegraded renders THE 503 template — the only one (web-dashboard-13).
// It carries generic commands and never an instance name: this page is shown
// before anything about the host is known, and a hard-coded example server
// ("pz-main") reads as a lie on every box not named that.
func ServeDegraded(w http.ResponseWriter, r *http.Request) {
	if IsAPIPath(r) {
		WriteError(w, r, http.StatusServiceUnavailable, CodeDegraded,
			"the datastore is unavailable; your game servers are not affected", nil)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(degraded503HTML))
}

// degraded503HTML is the browser body of the single 503 template. Generic
// commands only — asserted by TestDegradedPageNamesNoInstance.
const degraded503HTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>gvm dashboard — datastore down</title>
<style>
body{font-family:monospace;max-width:72ch;margin:2rem auto;padding:0 1rem;background:#1a1a1a;color:#e0e0e0}
h1{color:#f08080}pre{background:#111;padding:1rem;border-radius:4px;overflow-x:auto}
</style>
</head>
<body>
<h1>gvm dashboard unavailable &#8212; the datastore is down.</h1>
<p>Your game servers are <strong>NOT</strong> affected. They run independently of the dashboard.</p>
<p>To investigate the Postgres issue:</p>
<pre>  systemctl status postgresql
  journalctl -u postgresql -n 50
  df -h /var/lib/postgresql</pre>
<p>Everything the dashboard does is available on the host over SSH:</p>
<pre>  sudo gvm status
  sudo gvm doctor</pre>
</body>
</html>`
