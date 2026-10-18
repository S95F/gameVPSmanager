package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
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

// storeDown returns true when an expected "unavailable" error is returned.
func storeDown(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unavailable") ||
		strings.Contains(err.Error(), "store"))
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
	msg, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	return string(msg)
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
	renderInstanceDetailPage(w, sess, takeFlash(w, r), inst)
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
	renderConsolePage(w, sess, name)
}

// ── Browser form POSTs ───────────────────────────────────────────────────────
//
// Answered with 303 and a flash (§16.3 full-page form POST). Dispatch is not
// in this build, the pages render no start/stop/restart buttons, and these
// routes exist so a stale page's POST gets an honest answer rather than a 404.

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
	// U3: never a 2xx without a dispatch, and nothing dispatches in this
	// build — s.cfg.Dispatcher is deliberately not consulted.
	setFlash(w, notImplementedMessage(verb, name))
	http.Redirect(w, r, "/i/"+url.PathEscape(name), http.StatusSeeOther)
}

// notImplementedMessage is the one honest sentence for an unbuilt mutation
// (web-dashboard-2, day2-ops-6).
func notImplementedMessage(verb, name string) string {
	return strings.ToUpper(verb[:1]) + verb[1:] +
		" from the dashboard is not in this release; on the host run: sudo gvm " +
		verb + " " + name
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

// API lifecycle mutations: 501 with the envelope, never a 2xx without a
// dispatch (U3). s.cfg.Dispatcher is deliberately not consulted — wiring it
// is Phase 48's work, behind authz.

func (s *Server) handleAPIStart(w http.ResponseWriter, r *http.Request) {
	s.apiLifecycle(w, r, "start")
}
func (s *Server) handleAPIStop(w http.ResponseWriter, r *http.Request) { s.apiLifecycle(w, r, "stop") }
func (s *Server) handleAPIRestart(w http.ResponseWriter, r *http.Request) {
	s.apiLifecycle(w, r, "restart")
}

func (s *Server) apiLifecycle(w http.ResponseWriter, r *http.Request, verb string) {
	name := r.PathValue("name")
	webapi.WriteError(w, r, http.StatusNotImplemented, webapi.CodeNotImplemented,
		notImplementedMessage(verb, name), nil)
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

func renderInstanceDetailPage(w http.ResponseWriter, sess *auth.Session, flash string, inst *InstanceDetail) {
	pageHead(w, "gvm — "+inst.Name)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>%s</h1>`, htmlEscape(inst.Name))
	renderFlash(w, flash)
	fmt.Fprintf(w, `<dl><dt>Game</dt><dd>%s</dd><dt>Status</dt><dd>%s</dd></dl>`,
		htmlEscape(inst.Game), htmlEscape(inst.Status))
	fmt.Fprintf(w, `<p><a href="/i/%s/console">Open console &rarr;</a></p>`,
		htmlEscape(inst.Name))
	// No start/stop/restart buttons: dispatch is not in this release, and a
	// button that cannot act must not render (U3, web-dashboard-2).
	fmt.Fprint(w, `</main></body></html>`)
}

func renderConsolePage(w http.ResponseWriter, sess *auth.Session, name string) {
	pageHead(w, "gvm — console: "+name)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>Console: %s</h1>`, htmlEscape(name))
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
