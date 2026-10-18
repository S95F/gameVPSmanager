package web

// Static assets, served from constants so `go build ./cmd/gvm` stays the
// whole product (§16.5: no npm, no build step). They exist because the CSP is
// default-src 'self' with no unsafe-*: an inline <style> or <script> would be
// blocked by the very policy the dashboard ships.

import "net/http"

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("file") {
	case "gvm.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(gvmCSS))
	case "console.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(consoleJS))
	default:
		http.NotFound(w, r)
	}
}

const gvmCSS = `body{font-family:system-ui,sans-serif;margin:0;background:#1a1a1a;color:#e0e0e0}
nav{background:#111;padding:0.5rem 1rem;display:flex;gap:1rem;align-items:center}
nav a{color:#7ec8e3;text-decoration:none}
nav form{margin-left:auto}
main{max-width:960px;margin:2rem auto;padding:0 1rem}
h1{margin-top:0}
table{width:100%;border-collapse:collapse}
th,td{padding:0.5rem 1rem;border-bottom:1px solid #333;text-align:left}
a{color:#7ec8e3}
label{display:block;margin:0.5rem 0}
input{background:#111;color:#e0e0e0;border:1px solid #555;padding:0.3rem;border-radius:3px}
button{background:#333;color:#e0e0e0;border:1px solid #555;padding:0.3rem 0.8rem;cursor:pointer;border-radius:3px}
.flash{background:#2a2a1a;border:1px solid #665;padding:0.5rem 1rem;border-radius:4px}
#console{background:#111;color:#e0e0e0;font-family:monospace;font-size:0.9rem;height:70vh;overflow-y:auto;padding:0.5rem;white-space:pre-wrap}
`

// consoleJS attaches an EventSource to #console. The stream URL comes from
// the data-stream attribute, so this file carries no per-instance state.
// EventSource reconnects itself with Last-Event-ID (§16.5).
const consoleJS = `(function () {
	'use strict';
	var el = document.getElementById('console');
	if (!el || !el.dataset.stream) { return; }
	var src = new EventSource(el.dataset.stream);
	src.addEventListener('line', function (e) {
		try {
			var d = JSON.parse(e.data);
			el.textContent += d.line + '\n';
		} catch (ex) { /* skip malformed frame */ }
		el.scrollTop = el.scrollHeight;
	});
	src.addEventListener('lagged', function (e) {
		el.textContent += '\n[stream lagged; some lines were dropped]\n';
	});
	src.addEventListener('close', function () { src.close(); });
	src.onerror = function () { el.textContent += '\n[connection lost]\n'; };
})();
`
