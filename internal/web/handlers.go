package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/authz"
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
	renderConsolePage(w, sess, name, s.cfg.Hub != nil)
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

// handleConsoleStream streams the ring for the named instance as SSE. It
// serves both the browser route (/stream/i/{name}/console) and its API twin
// (/api/v1/stream/instances/{name}/console); each mux has already
// authenticated by its own path.
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

	// If no hub is configured, send one comment and close.
	if s.cfg.Hub == nil {
		_ = sse.WriteComment("no hub configured")
		return
	}

	ch, subID := s.cfg.Hub.Subscribe(name)
	defer s.cfg.Hub.Unsubscribe(name, subID)

	ctx := r.Context()
	// Keepalive goroutine: sends ": keepalive" every 15s so proxies don't
	// kill idle SSE connections.
	go sse.RunKeepalive(ctx, sseKeepaliveInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case batch, ok := <-ch:
			if !ok {
				_ = sse.WriteEvent("close", `{"reason":"ring closed"}`)
				return
			}
			if batch.Dropped > 0 {
				_ = sse.WriteEvent("lagged", fmt.Sprintf(`{"dropped":%d}`, batch.Dropped))
			}
			for _, line := range batch.Lines {
				data, jerr := json.Marshal(line)
				if jerr != nil {
					continue
				}
				_ = sse.WriteEventWithID("line", string(data), fmt.Sprintf("%d", line.Seq))
			}
		}
	}
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
		fmt.Fprint(w, `<table><thead><tr><th>Name</th><th>Game</th><th>Status</th><th></th></tr></thead><tbody>`)
		for _, inst := range instances {
			fmt.Fprintf(w, `<tr><td><a href="/i/%s">%s</a></td><td>%s</td><td>%s</td>`,
				htmlEscape(inst.Name), htmlEscape(inst.Name),
				htmlEscape(inst.Game), htmlEscape(inst.Status))
			fmt.Fprintf(w, `<td><a href="/i/%s/console">console</a></td></tr>`,
				htmlEscape(inst.Name))
		}
		fmt.Fprint(w, `</tbody></table>`)
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
		for _, verb := range []string{"start", "stop", "restart"} {
			fmt.Fprintf(w, `<form method="post" action="/i/%s/%s">`, htmlEscape(inst.Name), verb)
			fmt.Fprintf(w, `<button type="submit">%s</button></form> `, verb)
		}
	}
	fmt.Fprint(w, `</main></body></html>`)
}

func renderConsolePage(w http.ResponseWriter, sess *auth.Session, name string, live bool) {
	pageHead(w, "gvm — console: "+name)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>Console: %s</h1>`, htmlEscape(name))
	if !live {
		// No hub, no stream: say so instead of rendering a pane that looks
		// broken (U3). The CLI path always works.
		fmt.Fprintf(w, `<p class="flash">The live console stream is not wired in this build; on the host run: sudo gvm console %s</p>`,
			htmlEscape(name))
		fmt.Fprint(w, `</main></body></html>`)
		return
	}
	// console.js reads the stream URL from the data attribute; no inline JS.
	fmt.Fprintf(w, `<div id="console" data-stream="/stream/i/%s/console"></div>`,
		htmlEscape(name))
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
