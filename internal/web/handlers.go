package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/authz"
	consolepkg "github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/webapi"
)

// sseKeepaliveInterval is how often SSE handlers send a keepalive comment.
const sseKeepaliveInterval = 15 * time.Second

// ── Helpers ──────────────────────────────────────────────────────────────────

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// storeDown returns true when the failure means "a backend is down, the game
// servers are fine" — the cases that get the single degraded template rather
// than a 500 or a 404.
func storeDown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrControlUnavailable) || errors.Is(err, ErrIdentityUnavailable) {
		return true
	}
	return strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "store")
}

// ── Flash messages ───────────────────────────────────────────────────────────

// flashCookie carries a one-shot message across the 303 after a form POST.
const flashCookie = "gvm_flash"

// setFlash queues msg for the next page render.
func setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(msg)),
		Path:     "/",
		MaxAge:   60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash returns the queued message, if any, and clears it.
func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	msg, err := decodeFlash(c.Value)
	if err != nil {
		return ""
	}
	return msg
}

// decodeFlash decodes a flash cookie's value.
func decodeFlash(v string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return string(b), err
}

// ── Health endpoints (no auth) ────────────────────────────────────────────────

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "OK\n")
}

func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Browser pages ─────────────────────────────────────────────────────────────

func (s *Server) handleInstanceList(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InstanceQuerier == nil {
		webapi.ServeDegraded(w, r)
		return
	}
	instances, err := s.cfg.InstanceQuerier.ListInstances(r.Context())
	if err != nil {
		if storeDown(err) {
			webapi.ServeDegraded(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sess, _ := auth.FromContext(r.Context())
	renderInstanceListPage(w, sess, takeFlash(w, r), instances)
}

func (s *Server) handleInstanceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.cfg.InstanceQuerier == nil {
		webapi.ServeDegraded(w, r)
		return
	}
	inst, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name)
	if err != nil {
		if storeDown(err) {
			webapi.ServeDegraded(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	sess, _ := auth.FromContext(r.Context())
	renderInstanceDetailPage(w, sess, takeFlash(w, r), inst, s.cfg.Dispatcher != nil)
}

// handleConsoleView renders the console page, 404ing an unknown instance so a
// mistyped URL cannot show a live-looking empty console (web-dashboard-3).
func (s *Server) handleConsoleView(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.cfg.InstanceQuerier == nil {
		webapi.ServeDegraded(w, r)
		return
	}
	if _, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name); err != nil {
		if storeDown(err) {
			webapi.ServeDegraded(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	sess, _ := auth.FromContext(r.Context())
	renderConsolePage(w, sess, name, s.cfg.Console != nil, s.cfg.Dispatcher != nil)
}

// ── Lifecycle mutations, shared plumbing ─────────────────────────────────────

// permForVerb maps a lifecycle verb to its §15.2 permission.
func permForVerb(verb string) authz.Permission {
	switch verb {
	case "start":
		return authz.PermInstanceStart
	case "stop":
		return authz.PermInstanceStop
	default:
		return authz.PermInstanceRestart
	}
}

// effectiveRole resolves what the actor's base role (a session's global role,
// or a token's ceiling-capped role) becomes for one instance: grants only
// ever add (§15.2), and a token's ceiling caps the result again.
func (s *Server) effectiveRole(r *http.Request, instance string) (authz.Role, error) {
	if sess, ok := auth.FromContext(r.Context()); ok {
		base, err := authz.ParseRole(sess.Role)
		if err != nil {
			return "", err
		}
		if s.cfg.Authorizer == nil {
			return base, nil
		}
		eff, err := s.cfg.Authorizer.EffectiveRole(r.Context(), sess.UserID, instance)
		if err != nil {
			return "", err
		}
		return authz.Greatest(base, eff), nil
	}
	if actor, ok := auth.TokenActorFromContext(r.Context()); ok {
		eff := actor.Role
		if s.cfg.Authorizer != nil {
			granted, err := s.cfg.Authorizer.EffectiveRole(r.Context(), actor.UserID, instance)
			if err != nil {
				return "", err
			}
			// A grant may raise the user, never the token: re-apply the cap.
			if granted.Rank() > eff.Rank() && granted.Rank() <= actor.Ceiling.Rank() {
				eff = granted
			}
		}
		return eff, nil
	}
	return "", fmt.Errorf("web: no authenticated actor in context")
}

// actorLabel is the audit identity handed to gvmd.
func actorLabel(r *http.Request) string {
	if sess, ok := auth.FromContext(r.Context()); ok {
		return "user:" + sess.Email
	}
	if actor, ok := auth.TokenActorFromContext(r.Context()); ok {
		return "api_token:" + actor.TokenName
	}
	return "unknown"
}

// dispatchFor returns the Dispatcher method for verb. Callers have already
// checked that a Dispatcher is wired.
func (s *Server) dispatchFor(verb string) func(context.Context, string, string) (string, error) {
	switch verb {
	case "start":
		return s.cfg.Dispatcher.Start
	case "stop":
		return s.cfg.Dispatcher.Stop
	default:
		return s.cfg.Dispatcher.Restart
	}
}

// ── Browser form POSTs ───────────────────────────────────────────────────────
//
// Answered with 303 and a flash (§16.3 full-page form POST): the refusal, the
// lease line, or the acknowledged dispatch — never a silent success.

func (s *Server) handleFormStart(w http.ResponseWriter, r *http.Request) {
	s.formLifecycle(w, r, "start")
}
func (s *Server) handleFormStop(w http.ResponseWriter, r *http.Request) {
	s.formLifecycle(w, r, "stop")
}
func (s *Server) handleFormRestart(w http.ResponseWriter, r *http.Request) {
	s.formLifecycle(w, r, "restart")
}

func (s *Server) formLifecycle(w http.ResponseWriter, r *http.Request, verb string) {
	name := r.PathValue("name")
	back := "/i/" + url.PathEscape(name)

	// U3: never a 2xx (or a cheerful flash) without a dispatch. With no gvmd
	// client wired, the honest answer is the CLI command.
	if s.cfg.Dispatcher == nil {
		setFlash(w, notImplementedMessage(verb, name))
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	role, err := s.effectiveRole(r, name)
	if err != nil {
		webapi.ServeDegraded(w, r)
		return
	}
	if err := authz.Check(role, permForVerb(verb)); err != nil {
		http.Error(w, "your role may not "+verb+" this server", http.StatusForbidden)
		return
	}

	opID, err := s.dispatchFor(verb)(r.Context(), name, actorLabel(r))
	var lease *LeaseHeldError
	switch {
	case err == nil:
		setFlash(w, strings.ToUpper(verb[:1])+verb[1:]+" dispatched ("+opID+").")
	case errors.As(err, &lease):
		setFlash(w, "Refused — a job holds this server: "+lease.Line)
	case errors.Is(err, ErrControlUnavailable):
		setFlash(w, "Refused — the control daemon is not reachable. Your game servers are unaffected.")
	default:
		setFlash(w, "Refused — "+err.Error())
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// ── API endpoints ─────────────────────────────────────────────────────────────

func (s *Server) handleAPIInstanceList(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InstanceQuerier == nil {
		webapi.ServeDegraded(w, r)
		return
	}
	instances, err := s.cfg.InstanceQuerier.ListInstances(r.Context())
	if err != nil {
		if storeDown(err) {
			webapi.ServeDegraded(w, r)
			return
		}
		webapi.WriteError(w, r, http.StatusInternalServerError, webapi.CodeInternal, "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances})
}

func (s *Server) handleAPIInstanceStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.cfg.InstanceQuerier == nil {
		webapi.ServeDegraded(w, r)
		return
	}
	inst, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name)
	if err != nil {
		if storeDown(err) {
			webapi.ServeDegraded(w, r)
			return
		}
		webapi.WriteError(w, r, http.StatusNotFound, webapi.CodeNotFound, "instance not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

// consoleBacklog is §16.6's replay on a fresh attach.
const consoleBacklog = 200

// handleConsoleStream streams one instance's live console as SSE. It serves
// both the browser route (/stream/i/{name}/console) and its API twin
// (/api/v1/stream/instances/{name}/console); each mux has already
// authenticated by its own path.
//
// Reconnects resume: EventSource replays its Last-Event-ID, which is the last
// line's sequence; a gap the ring has already dropped renders as "— N lines
// skipped —", never silently (§16.6).
//
// Content-Type is text/event-stream, which is why these routes live under
// the reserved /stream/ prefixes: Caddy's stream matcher flushes them
// immediately and the encoder never sees them (D-38).
func (s *Server) handleConsoleStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	sse := NewSSEWriter(w)
	if sse == nil {
		// ResponseWriter does not support flushing; cannot stream SSE.
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	defer sse.Done()

	if s.cfg.Console == nil {
		_ = sse.WriteComment("console streaming is not wired in this build")
		return
	}

	// Resume after the last line the browser saw, or replay the backlog.
	var from uint64
	backlog := consoleBacklog
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			from = n + 1
			backlog = 0
		}
	}

	// The hello frame carries what the pane needs before the first line:
	// the instance and its plain-language state (§16.6 hello{state,...}).
	state := ""
	if s.cfg.InstanceQuerier != nil {
		if inst, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name); err == nil {
			state = inst.Status
		}
	}
	helloData, _ := json.Marshal(map[string]any{"instance": name, "state": state, "from": from})
	_ = sse.WriteEvent("hello", string(helloData))

	// The keepalive goroutine must stop when THIS handler returns, not when
	// the request context eventually dies.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Keepalive goroutine: sends ": keepalive" every 15s so proxies don't
	// kill idle SSE connections.
	go sse.RunKeepalive(ctx, sseKeepaliveInterval)

	expected := from
	err := s.cfg.Console.Stream(ctx, name, from, backlog, func(ev consolepkg.Event) {
		switch ev.Ev {
		case consolepkg.EvLines:
			for _, line := range ev.Lines {
				if expected != 0 && line.Seq > expected {
					_ = sse.WriteEvent("gap", fmt.Sprintf(`{"skipped":%d}`, line.Seq-expected))
				}
				expected = line.Seq + 1
				data, jerr := json.Marshal(line)
				if jerr != nil {
					continue
				}
				_ = sse.WriteEventWithID("line", string(data), fmt.Sprintf("%d", line.Seq))
			}
		case consolepkg.EvLagged, consolepkg.EvTruncated:
			_ = sse.WriteEvent("gap", fmt.Sprintf(`{"skipped":%d,"resume_from":%d}`, ev.Dropped, ev.ResumeFrom))
			if ev.ResumeFrom > 0 {
				expected = ev.ResumeFrom
			}
		case consolepkg.EvShutdown:
			_ = sse.WriteEvent("shutdown", fmt.Sprintf(`{"phase":%q}`, ev.Channel))
		}
	})
	switch {
	case err == nil || ctx.Err() != nil:
		return
	case errors.Is(err, ErrConsoleDown):
		// A stopped server is a state, not an error: the banner says so and
		// EventSource's own reconnect keeps watching for it to come back.
		_ = sse.WriteEvent("stopped", string(mustJSON(map[string]string{"state": state})))
	default:
		_ = sse.WriteEvent("close", string(mustJSON(map[string]string{"reason": "stream error"})))
	}
}

// mustJSON marshals values this file builds itself; a failure is a
// programming error and renders as null rather than panicking mid-stream.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// ── The console send box (§16.6) ──────────────────────────────────────────────

// consoleStopWords are refused outright: stopping is the Stop button's job,
// because that path walks the save ladder. The manifest's own ladder words are
// a later addition; these four cover every bash-suite game shipped today.
var consoleStopWords = map[string]bool{
	"quit": true, "stop": true, "shutdown": true, "exit": true,
}

// handleConsoleSend is the browser form POST for one console line.
func (s *Server) handleConsoleSend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := "/i/" + url.PathEscape(name) + "/console"

	if s.cfg.Dispatcher == nil {
		setFlash(w, "Typing into the console is not wired in this build; on the host run: sudo gvm console "+name)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	// console:write is operator and above (§15.2).
	role, err := s.effectiveRole(r, name)
	if err != nil {
		webapi.ServeDegraded(w, r)
		return
	}
	if err := authz.Check(role, authz.PermInstanceConsole); err != nil {
		http.Error(w, "your role may not type into this console", http.StatusForbidden)
		return
	}

	line := strings.TrimRight(r.PostFormValue("line"), "\r\n")
	switch {
	case strings.TrimSpace(line) == "":
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	case len(line) > consolepkg.MaxSendLine:
		setFlash(w, fmt.Sprintf("Not sent — one line of at most %d bytes.", consolepkg.MaxSendLine))
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	case strings.ContainsAny(line, "\r\n"):
		setFlash(w, "Not sent — one line at a time.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	// The ladder words are refused, not forwarded: a typed `quit` skips the
	// save ladder, and "the world saved" must never depend on which box an
	// operator clicked from (§16.6).
	first := strings.ToLower(strings.Fields(strings.TrimSpace(line))[0])
	if consoleStopWords[strings.Trim(first, "/")] {
		setFlash(w, "Not sent — use the Stop button: it saves the world first.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	if err := s.cfg.Dispatcher.ConsoleSend(r.Context(), name, line, actorLabel(r)); err != nil {
		if errors.Is(err, ErrControlUnavailable) {
			setFlash(w, "Not sent — the control daemon is not reachable. Your game servers are unaffected.")
		} else {
			setFlash(w, "Not sent — "+err.Error())
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// API lifecycle mutations: 202 with the operation id only after gvmd has
// acknowledged the dispatch, 409 with the lease line while a job holds the
// instance, and 501 while no Dispatcher is wired — never a 2xx without a real
// dispatch (U3).

func (s *Server) handleAPIStart(w http.ResponseWriter, r *http.Request) {
	s.apiLifecycle(w, r, "start")
}
func (s *Server) handleAPIStop(w http.ResponseWriter, r *http.Request) { s.apiLifecycle(w, r, "stop") }
func (s *Server) handleAPIRestart(w http.ResponseWriter, r *http.Request) {
	s.apiLifecycle(w, r, "restart")
}

func (s *Server) apiLifecycle(w http.ResponseWriter, r *http.Request, verb string) {
	name := r.PathValue("name")
	if s.cfg.Dispatcher == nil {
		webapi.WriteError(w, r, http.StatusNotImplemented, webapi.CodeNotImplemented,
			notImplementedMessage(verb, name), nil)
		return
	}

	role, err := s.effectiveRole(r, name)
	if err != nil {
		webapi.WriteError(w, r, http.StatusInternalServerError, webapi.CodeInternal, "internal error", nil)
		return
	}
	if err := authz.Check(role, permForVerb(verb)); err != nil {
		webapi.WriteError(w, r, http.StatusForbidden, webapi.CodeForbidden,
			"role "+string(role)+" may not "+verb+" this server", nil)
		return
	}

	opID, err := s.dispatchFor(verb)(r.Context(), name, actorLabel(r))
	var lease *LeaseHeldError
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{
			"name": name, "action": verb, "status": "accepted", "op_id": opID,
		})
	case errors.As(err, &lease):
		webapi.WriteError(w, r, http.StatusConflict, webapi.CodeConflict,
			"a job holds this server: "+lease.Line, map[string]any{"lease": lease.Line})
	case errors.Is(err, ErrControlUnavailable):
		webapi.WriteError(w, r, http.StatusServiceUnavailable, webapi.CodeDegraded,
			"the control daemon is not reachable; game servers are not affected", nil)
	default:
		webapi.WriteError(w, r, http.StatusInternalServerError, webapi.CodeInternal, err.Error(), nil)
	}
}

// notImplementedMessage is the one honest sentence for an unwired mutation
// (web-dashboard-2, day2-ops-6).
func notImplementedMessage(verb, name string) string {
	return strings.ToUpper(verb[:1]) + verb[1:] +
		" from the dashboard is not in this release; on the host run: sudo gvm " +
		verb + " " + name
}

// ── Page renderers ────────────────────────────────────────────────────────────
//
// No inline <style> or <script>: the CSP is default-src 'self' with no
// unsafe-* (§16.5), so everything executable or styled comes from /static.

// pageHead opens the document. title is escaped here.
func pageHead(w http.ResponseWriter, title string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<meta name="viewport" content="width=device-width, initial-scale=1">`)
	fmt.Fprintf(w, `<title>%s</title>`, htmlEscape(title))
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body>`)
}

func renderFlash(w http.ResponseWriter, flash string) {
	if flash != "" {
		fmt.Fprintf(w, `<p class="flash">%s</p>`, htmlEscape(flash))
	}
}

func renderInstanceListPage(w http.ResponseWriter, sess *auth.Session, flash string, instances []InstanceSummary) {
	pageHead(w, "gvm — instances")
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprint(w, `<main><h1>Instances</h1>`)
	renderFlash(w, flash)
	if len(instances) == 0 {
		fmt.Fprint(w, `<p>No instances found.</p>`)
	} else {
		// The wrapper scrolls sideways on narrow screens instead of letting a
		// long name or status sentence squeeze the whole table off the page.
		fmt.Fprint(w, `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Game</th><th>Status</th><th></th></tr></thead><tbody>`)
		for _, inst := range instances {
			fmt.Fprintf(w, `<tr><td><a href="/i/%s">%s</a></td><td>%s</td><td>%s</td>`,
				htmlEscape(inst.Name), htmlEscape(inst.Name),
				htmlEscape(inst.Game), htmlEscape(inst.Status))
			fmt.Fprintf(w, `<td><a href="/i/%s/console">console</a></td></tr>`,
				htmlEscape(inst.Name))
		}
		fmt.Fprint(w, `</tbody></table></div>`)
	}
	fmt.Fprint(w, `</main></body></html>`)
}

func renderInstanceDetailPage(w http.ResponseWriter, sess *auth.Session, flash string, inst *InstanceDetail, canAct bool) {
	pageHead(w, "gvm — "+inst.Name)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>%s</h1>`, htmlEscape(inst.Name))
	renderFlash(w, flash)
	fmt.Fprintf(w, `<dl><dt>Game</dt><dd>%s</dd><dt>Status</dt><dd>%s</dd></dl>`,
		htmlEscape(inst.Game), htmlEscape(inst.Status))
	fmt.Fprintf(w, `<p><a href="/i/%s/console">Open console &rarr;</a></p>`,
		htmlEscape(inst.Name))
	// The lifecycle buttons render only when a gvmd client is wired: a button
	// that cannot act must not render (U3, web-dashboard-2).
	if canAct {
		fmt.Fprint(w, `<div class="actions">`)
		for _, verb := range []string{"start", "stop", "restart"} {
			fmt.Fprintf(w, `<form method="post" action="/i/%s/%s">`, htmlEscape(inst.Name), verb)
			fmt.Fprintf(w, `<button type="submit">%s</button></form> `, verb)
		}
		fmt.Fprint(w, `</div>`)
	}
	fmt.Fprint(w, `</main></body></html>`)
}

func renderConsolePage(w http.ResponseWriter, sess *auth.Session, name string, live, canSend bool) {
	pageHead(w, "gvm — console: "+name)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>Console: %s</h1>`, htmlEscape(name))
	if !live {
		// No streamer, no pane: say so instead of rendering one that looks
		// broken (U3). The CLI path always works.
		fmt.Fprintf(w, `<p class="flash">The live console stream is not wired in this build; on the host run: sudo gvm console %s</p>`,
			htmlEscape(name))
		fmt.Fprint(w, `</main></body></html>`)
		return
	}
	fmt.Fprintf(w, `<p id="console-state"></p>`)
	// console.js reads the stream URL from the data attribute; no inline JS.
	fmt.Fprintf(w, `<div id="console" data-stream="/stream/i/%s/console"></div>`,
		htmlEscape(name))
	if canSend {
		// The send box POSTs like every other mutation — same CSRF, same
		// RBAC, same audit path through gvmd — never a WebSocket (§16.5).
		fmt.Fprintf(w, `<form method="post" action="/i/%s/console" class="console-send">`, htmlEscape(name))
		fmt.Fprintf(w, `<input type="text" name="line" maxlength="%d" placeholder="server command" autocomplete="off">`, consolepkg.MaxSendLine)
		fmt.Fprint(w, `<button type="submit">Send</button></form>`)
	}
	fmt.Fprint(w, `<script src="/static/console.js"></script>`)
	fmt.Fprint(w, `</main></body></html>`)
}

func renderNav(sess *auth.Session) string {
	user := "guest"
	if sess != nil {
		user = htmlEscape(sess.DisplayName)
		if user == "" {
			user = htmlEscape(sess.Email)
		}
	}
	return fmt.Sprintf(`<nav><a href="/instances">Instances</a> &bull; <span>%s</span>`+
		`<form method="post" action="/logout"><button type="submit">Sign out</button></form></nav>`, user)
}

// htmlEscape escapes s for safe insertion into HTML content and attributes.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
