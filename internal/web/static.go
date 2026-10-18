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

// gvmCSS is written mobile-first: inputs and buttons are at least 1rem (16px)
// so phone browsers don't zoom the page when a field gets focus, touch
// targets are comfortable, wide content (tables, pre) scrolls inside its own
// box instead of stretching the page, and the two breakpoints only adjust
// spacing — a phone, a Tab S11 Ultra and a desktop all see the same markup.
const gvmCSS = `*,*::before,*::after{box-sizing:border-box}
body{font-family:system-ui,sans-serif;margin:0;background:#1a1a1a;color:#e0e0e0}
nav{background:#111;padding:0.5rem 1rem;display:flex;gap:0.5rem 1rem;align-items:center;flex-wrap:wrap}
nav a{color:#7ec8e3;text-decoration:none;padding:0.4rem 0}
nav span{overflow-wrap:anywhere}
nav form{margin-left:auto}
main{max-width:960px;margin:1rem auto;padding:0 1rem}
h1{margin-top:0;overflow-wrap:anywhere}
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse}
th,td{padding:0.5rem;border-bottom:1px solid #333;text-align:left}
a{color:#7ec8e3}
label{display:block;margin:0.8rem 0}
input{background:#111;color:#e0e0e0;border:1px solid #555;padding:0.5rem;border-radius:3px;font-size:1rem;max-width:100%}
label input{display:block;width:100%;max-width:24rem;margin-top:0.3rem}
button{background:#333;color:#e0e0e0;border:1px solid #555;padding:0.5rem 1rem;cursor:pointer;border-radius:3px;font-size:1rem}
.actions{display:flex;gap:0.5rem;flex-wrap:wrap;margin:1rem 0}
.actions form{margin:0}
.flash{background:#2a2a1a;border:1px solid #665;padding:0.5rem 1rem;border-radius:4px;overflow-wrap:anywhere}
#console{background:#111;color:#e0e0e0;font-family:monospace;font-size:0.9rem;height:60vh;height:60dvh;overflow-y:auto;padding:0.5rem;white-space:pre-wrap;overflow-wrap:anywhere}
#console-state{color:#9a9;margin:0.2rem 0}
.console-send{display:flex;gap:0.5rem;margin-top:0.5rem}
.console-send input{flex:1;min-width:0;font-family:monospace}
pre{background:#111;padding:1rem;border-radius:4px;overflow-x:auto;max-width:100%}
code{overflow-wrap:anywhere}
@media (pointer:coarse){button{min-height:44px}input{min-height:44px}}
@media (min-width:700px){main{margin:2rem auto}th,td{padding:0.5rem 1rem}}
@media (max-width:640px){thead{display:none}table,tbody,tr,td{display:block}tr{border-bottom:1px solid #333;padding:0.5rem 0}td{padding:0.1rem 0;border-bottom:0}td:first-child{font-weight:600}}
`

// consoleJS attaches an EventSource to #console. The stream URL comes from
// the data-stream attribute, so this file carries no per-instance state.
// EventSource reconnects itself with Last-Event-ID, which the server reads as
// the resume cursor; a gap the ring has dropped renders as "— N lines
// skipped —" (§16.6).
const consoleJS = `(function () {
	'use strict';
	var el = document.getElementById('console');
	var state = document.getElementById('console-state');
	if (!el || !el.dataset.stream) { return; }

	function note(text) {
		el.textContent += '— ' + text + ' —\n';
		el.scrollTop = el.scrollHeight;
	}

	var src = new EventSource(el.dataset.stream);
	src.addEventListener('hello', function (e) {
		try {
			var d = JSON.parse(e.data);
			if (state && d.state) { state.textContent = d.state; }
		} catch (ex) { /* skip malformed frame */ }
	});
	src.addEventListener('line', function (e) {
		try {
			var d = JSON.parse(e.data);
			el.textContent += d.line + '\n';
		} catch (ex) { /* skip malformed frame */ }
		el.scrollTop = el.scrollHeight;
	});
	src.addEventListener('gap', function (e) {
		var n = 0;
		try { n = JSON.parse(e.data).skipped || 0; } catch (ex) {}
		note(n + ' lines skipped');
	});
	src.addEventListener('stopped', function (e) {
		var text = 'server stopped';
		try {
			var d = JSON.parse(e.data);
			if (d.state) { text = d.state; }
		} catch (ex) {}
		if (state) { state.textContent = text; }
	});
	src.addEventListener('shutdown', function () { note('shutting down'); });
	src.addEventListener('close', function () { src.close(); });
})();
`
