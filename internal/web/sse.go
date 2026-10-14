package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SSEWriter wraps an http.ResponseWriter for Server-Sent Events.
//
// SSE wire format: https://html.spec.whatwg.org/multipage/server-sent-events.html
//
// The Caddy encode matcher must NOT include text/* because text/event-stream
// would be buffered and frames would never reach the browser. Use explicit
// content-type matching (application/x-www-form-urlencoded) for compressed
// responses on form POSTs, never a wildcard that captures SSE streams.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter sets SSE headers and returns an SSEWriter. Returns nil if the
// underlying ResponseWriter does not implement http.Flusher.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx/Caddy not to buffer
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &SSEWriter{w: w, flusher: f}
}

// WriteEvent writes an SSE event with optional event type. data is the event
// data; a multi-line value is split across multiple "data:" lines as per spec.
func (s *SSEWriter) WriteEvent(event, data string) error {
	var sb strings.Builder
	if event != "" {
		fmt.Fprintf(&sb, "event: %s\n", event)
	}
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&sb, "data: %s\n", line)
	}
	sb.WriteString("\n")
	_, err := fmt.Fprint(s.w, sb.String())
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// WriteComment writes an SSE comment (keepalive). Comments do not dispatch an
// event; they keep the connection open and reset proxy idle timeouts.
func (s *SSEWriter) WriteComment(comment string) error {
	_, err := fmt.Fprintf(s.w, ": %s\n\n", comment)
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// WriteEventWithID writes an SSE event with an id field for reconnect cursor tracking.
func (s *SSEWriter) WriteEventWithID(event, data, id string) error {
	var sb strings.Builder
	if id != "" {
		fmt.Fprintf(&sb, "id: %s\n", id)
	}
	if event != "" {
		fmt.Fprintf(&sb, "event: %s\n", event)
	}
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&sb, "data: %s\n", line)
	}
	sb.WriteString("\n")
	_, err := fmt.Fprint(s.w, sb.String())
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Done is a no-op finalizer; reserved for future cleanup hooks.
func (s *SSEWriter) Done() {}

// RunKeepalive sends ": keepalive" comments every interval until ctx is cancelled.
// It should be called in a goroutine.
func (s *SSEWriter) RunKeepalive(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.WriteComment("keepalive"); err != nil {
				return
			}
		}
	}
}
