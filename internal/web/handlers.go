package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
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

// writeJSONError writes {"error": msg} with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// storeDown returns true when the InstanceQuerier is nil (store not wired up)
// or when an expected "unavailable" error is returned.
func storeDown(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unavailable") ||
		strings.Contains(err.Error(), "store"))
}

// serve503Page renders the specific Postgres-down page.
func serve503Page(w http.ResponseWriter, r *http.Request) {
	isAPI := strings.HasPrefix(r.URL.Path, "/api/")
	if isAPI {
		writeJSONError(w, http.StatusServiceUnavailable, "datastore unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, downPage503HTML)
}

const downPage503HTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>gvm dashboard — datastore down</title>
<style>
body{font-family:monospace;max-width:72ch;margin:4rem auto;padding:0 1rem;background:#1a1a1a;color:#e0e0e0}
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
<p>Everything the dashboard does is available from the box:</p>
<pre>  sudo gvm status      sudo gvm console pz-main
  sudo gvm stop pz-main   sudo gvm doctor</pre>
</body>
</html>`

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
		serve503Page(w, r)
		return
	}
	instances, err := s.cfg.InstanceQuerier.ListInstances(r.Context())
	if err != nil {
		if storeDown(err) {
			serve503Page(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sess, _ := auth.FromContext(r.Context())
	csrfToken := s.csrf.Token(r.Context())
	renderInstanceListPage(w, sess, csrfToken, instances)
}

func (s *Server) handleInstanceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.cfg.InstanceQuerier == nil {
		serve503Page(w, r)
		return
	}
	inst, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name)
	if err != nil {
		if storeDown(err) {
			serve503Page(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	sess, _ := auth.FromContext(r.Context())
	csrfToken := s.csrf.Token(r.Context())
	renderInstanceDetailPage(w, sess, csrfToken, inst)
}

func (s *Server) handleConsoleView(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sess, _ := auth.FromContext(r.Context())
	csrfToken := s.csrf.Token(r.Context())
	renderConsolePage(w, sess, csrfToken, name)
}

// ── API endpoints ─────────────────────────────────────────────────────────────

func (s *Server) handleAPIInstanceList(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InstanceQuerier == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "datastore unavailable")
		return
	}
	instances, err := s.cfg.InstanceQuerier.ListInstances(r.Context())
	if err != nil {
		if storeDown(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "datastore unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances})
}

func (s *Server) handleAPIInstanceStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.cfg.InstanceQuerier == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "datastore unavailable")
		return
	}
	inst, err := s.cfg.InstanceQuerier.GetInstance(r.Context(), name)
	if err != nil {
		if storeDown(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "datastore unavailable")
			return
		}
		writeJSONError(w, http.StatusNotFound, "instance not found")
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

// handleAPIConsoleEvents streams the ring for the named instance as SSE.
//
// Content-Type is text/event-stream. The Caddy encode matcher must NOT include
// text/* — it must use application/x-www-form-urlencoded so it never buffers
// this stream.
func (s *Server) handleAPIConsoleEvents(w http.ResponseWriter, r *http.Request) {
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

// handleAPIStart handles POST /api/v1/instances/{name}/start.
func (s *Server) handleAPIStart(w http.ResponseWriter, r *http.Request) {
	if err := s.csrf.Validate(r); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	name := r.PathValue("name")
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "action": "start", "status": "accepted"})
}

// handleAPIStop handles POST /api/v1/instances/{name}/stop.
func (s *Server) handleAPIStop(w http.ResponseWriter, r *http.Request) {
	if err := s.csrf.Validate(r); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	name := r.PathValue("name")
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "action": "stop", "status": "accepted"})
}

// handleAPIRestart handles POST /api/v1/instances/{name}/restart.
func (s *Server) handleAPIRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.csrf.Validate(r); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	name := r.PathValue("name")
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "action": "restart", "status": "accepted"})
}

// ── Page renderers ────────────────────────────────────────────────────────────

func renderInstanceListPage(w http.ResponseWriter, sess *auth.Session, csrf string, instances []InstanceSummary) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<title>gvm — instances</title>`)
	fmt.Fprint(w, commonCSS)
	fmt.Fprint(w, `</head><body>`)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprint(w, `<main><h1>Instances</h1>`)
	if len(instances) == 0 {
		fmt.Fprint(w, `<p>No instances found.</p>`)
	} else {
		fmt.Fprint(w, `<table><thead><tr><th>Name</th><th>Game</th><th>Status</th><th></th></tr></thead><tbody>`)
		for _, inst := range instances {
			fmt.Fprintf(w, `<tr><td><a href="/instances/%s">%s</a></td><td>%s</td><td>%s</td>`,
				htmlEscape(inst.Name), htmlEscape(inst.Name),
				htmlEscape(inst.Game), htmlEscape(inst.Status))
			fmt.Fprintf(w, `<td><a href="/instances/%s/console">console</a></td></tr>`,
				htmlEscape(inst.Name))
		}
		fmt.Fprint(w, `</tbody></table>`)
	}
	fmt.Fprint(w, `</main></body></html>`)
}

func renderInstanceDetailPage(w http.ResponseWriter, sess *auth.Session, csrf string, inst *InstanceDetail) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprintf(w, `<title>gvm — %s</title>`, htmlEscape(inst.Name))
	fmt.Fprint(w, commonCSS)
	fmt.Fprint(w, `</head><body>`)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>%s</h1>`, htmlEscape(inst.Name))
	fmt.Fprintf(w, `<dl><dt>Game</dt><dd>%s</dd><dt>Status</dt><dd>%s</dd></dl>`,
		htmlEscape(inst.Game), htmlEscape(inst.Status))
	fmt.Fprintf(w, `<p><a href="/instances/%s/console">Open console &rarr;</a></p>`,
		htmlEscape(inst.Name))

	// Lifecycle buttons with CSRF token.
	for _, action := range []string{"start", "stop", "restart"} {
		fmt.Fprintf(w, `<form method="post" action="/api/v1/instances/%s/%s" style="display:inline">`,
			htmlEscape(inst.Name), action)
		fmt.Fprintf(w, `<input type="hidden" name="_csrf" value="%s">`, htmlEscape(csrf))
		fmt.Fprintf(w, `<button type="submit">%s</button></form> `, action)
	}
	fmt.Fprint(w, `</main></body></html>`)
}

func renderConsolePage(w http.ResponseWriter, sess *auth.Session, csrf string, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprintf(w, `<title>gvm — console: %s</title>`, htmlEscape(name))
	fmt.Fprint(w, commonCSS)
	fmt.Fprint(w, `<style>
#console{background:#111;color:#e0e0e0;font-family:monospace;font-size:0.9rem;
 height:70vh;overflow-y:auto;padding:0.5rem;white-space:pre-wrap;}
</style>`)
	fmt.Fprint(w, `</head><body>`)
	fmt.Fprint(w, renderNav(sess))
	fmt.Fprintf(w, `<main><h1>Console: %s</h1>`, htmlEscape(name))
	fmt.Fprint(w, `<div id="console"></div>`)
	fmt.Fprintf(w, `<script>
(function(){
  var el = document.getElementById('console');
  var src = new EventSource('/api/v1/instances/%s/console/events');
  src.addEventListener('line', function(e) {
    try { var d = JSON.parse(e.data); el.textContent += d.line + '\n'; } catch(ex){}
    el.scrollTop = el.scrollHeight;
  });
  src.addEventListener('close', function(){ src.close(); });
  src.onerror = function(){ el.textContent += '\n[connection lost]\n'; };
})();
</script>`, htmlEscape(name))
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
	return fmt.Sprintf(`<nav><a href="/instances">Instances</a> &bull; <span>%s</span></nav>`, user)
}

const commonCSS = `<style>
body{font-family:system-ui,sans-serif;margin:0;background:#1a1a1a;color:#e0e0e0}
nav{background:#111;padding:0.5rem 1rem;display:flex;gap:1rem;align-items:center}
nav a{color:#7ec8e3;text-decoration:none}
main{max-width:960px;margin:2rem auto;padding:0 1rem}
h1{margin-top:0}
table{width:100%;border-collapse:collapse}
th,td{padding:0.5rem 1rem;border-bottom:1px solid #333;text-align:left}
a{color:#7ec8e3}
button{background:#333;color:#e0e0e0;border:1px solid #555;padding:0.3rem 0.8rem;cursor:pointer;border-radius:3px}
</style>`

// htmlEscape escapes s for safe insertion into HTML content and attributes.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
