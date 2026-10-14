package console

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
)

// Hub multiplexes Ring subscriptions to web SSE connections.
//
// Multiple web clients can subscribe to the same instance's console stream
// simultaneously. When the Ring is detached (runner exits), the Ring is closed
// by the caller, so subscriber channels drain naturally.
type Hub struct {
	mu    sync.Mutex
	rings map[string]*Ring
	subs  map[string]map[string]*Subscription // instanceName → subID → sub
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{
		rings: make(map[string]*Ring),
		subs:  make(map[string]map[string]*Subscription),
	}
}

// Subscribe returns a channel of Batch for the named instance and a subID
// that the caller must pass to Unsubscribe when done. If no ring is currently
// attached, the returned channel is closed immediately so the caller can
// detect the "not running" state.
func (h *Hub) Subscribe(instanceName string) (<-chan Batch, string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subID := newSubID()

	ring, ok := h.rings[instanceName]
	if !ok {
		// No ring: return an already-closed channel so the SSE handler
		// can send one "close" event and exit cleanly.
		ch := make(chan Batch)
		close(ch)
		return ch, subID
	}

	// Subscribe from the tail so a fresh web client gets recent lines.
	sub := ring.Subscribe(ring.Head(), DefaultSubDepth)
	if h.subs[instanceName] == nil {
		h.subs[instanceName] = make(map[string]*Subscription)
	}
	h.subs[instanceName][subID] = sub
	return sub.C, subID
}

// Unsubscribe releases a subscription. Safe to call multiple times.
func (h *Hub) Unsubscribe(instanceName, subID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if subs, ok := h.subs[instanceName]; ok {
		if sub, ok := subs[subID]; ok {
			sub.Close()
			delete(subs, subID)
		}
		if len(subs) == 0 {
			delete(h.subs, instanceName)
		}
	}
}

// Attach connects a Ring to the Hub for an instance.
// Call when an instance runner starts and the Ring is ready.
func (h *Hub) Attach(instanceName string, ring *Ring) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rings[instanceName] = ring
}

// Detach removes the Ring for an instance.
// Call when a runner exits after closing the Ring.
func (h *Hub) Detach(instanceName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rings, instanceName)
	// Close any lingering subscriptions.
	for _, sub := range h.subs[instanceName] {
		sub.Close()
	}
	delete(h.subs, instanceName)
}

func newSubID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
