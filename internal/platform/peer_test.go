package platform

import "testing"

func TestPeerFormatting(t *testing.T) {
	p := Peer{PID: 41799, UID: 1001, GID: 1001}
	if got := p.String(); got != "pid=41799 uid=1001 gid=1001" {
		t.Fatalf("got %q", got)
	}
	if p.Root() {
		t.Error("uid 1001 reported as root")
	}
	if !(Peer{PID: 1, UID: 0, GID: 0}).Root() {
		t.Error("uid 0 not reported as root")
	}
	// The zero Peer is what a failed PeerCred leaves behind, and its uid is 0.
	// A caller that forgot to check the error must not thereby be handed the
	// lifecycle ops (verb, stop).
	var zero Peer
	if zero.Root() || zero.Identified() {
		t.Fatal("the zero Peer authorizes as root")
	}
	// A peer in another pid namespace comes back with pid 0 and a uid we
	// cannot attribute; the same rule applies.
	if (Peer{PID: 0, UID: 0, GID: 0}).Root() {
		t.Fatal("an unidentifiable peer authorizes as root")
	}
	if !p.Identified() {
		t.Error("a real peer reported as unidentified")
	}
}
