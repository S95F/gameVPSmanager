//go:build linux

package socket

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// TestListenGivesTheSocketItsSectionFiveMode. The group comes from the setgid
// bit on /run/gvm/<name>, so the only thing this side owns is the 0660.
func TestListenGivesTheSocketItsSectionFiveMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %s)", path, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != SocketMode {
		t.Errorf("socket mode is %04o, want %04o", perm, SocketMode)
	}
}

// TestListenRefusesToStealALiveSocket. Unlinking a socket another runner is
// serving would take the console away from it while leaving both processes
// convinced they are listening -- the same class of failure as two servers on
// one world, one layer down.
func TestListenRefusesToStealALiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.sock")
	first, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer first.Close()

	if _, err := Listen(path); err == nil {
		t.Fatal("a second Listen took over a live socket")
	}

	// A socket with nobody behind it is stale and may be replaced: that is the
	// state a SIGKILLed runner leaves behind.
	first.SetUnlinkOnClose(false)
	first.Close()
	second, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	second.Close()

	// A regular file at that path is not a socket gvm may remove.
	other := filepath.Join(t.TempDir(), "console.sock")
	if err := os.WriteFile(other, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Listen(other); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
}

// TestPeerIdentifierReadsThisProcess is the getsockopt half that the pure
// matrix in auth_test.go cannot cover.
func TestPeerIdentifierReadsThisProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	type result struct {
		id  Identity
		err error
	}
	got := make(chan result, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- result{err: err}
			return
		}
		defer c.Close()
		id, err := PeerIdentifier(c)
		got <- result{id: id, err: err}
	}()

	client, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	r := <-got
	if r.err != nil {
		t.Fatalf("PeerIdentifier: %v", r.err)
	}
	if r.id.Peer.PID != int32(os.Getpid()) {
		t.Errorf("peer pid %d, want %d", r.id.Peer.PID, os.Getpid())
	}
	if r.id.Peer.UID != uint32(os.Getuid()) {
		t.Errorf("peer uid %d, want %d", r.id.Peer.UID, os.Getuid())
	}
	if !r.id.Peer.Identified() {
		t.Error("the kernel named the peer but Identified is false")
	}
	// The supplementary set is what the console tier is decided on, and this
	// process is in at least its own primary group.
	if len(r.id.Groups) == 0 {
		t.Error("no groups resolved for this uid")
	}
}

// TestTheTiersHoldOverARealSocket ties the getsockopt to the matrix: a
// connection from this very process lands in the tier its uid earns, and the
// hello event says so before anything is refused.
func TestTheTiersHoldOverARealSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	be := newFakeBackend("pz-main")
	t.Cleanup(func() { be.Close() })
	// The runner runs as the instance user, so its own uid is the owner tier.
	srv, err := New(be, Policy{OwnerUID: uint32(os.Getuid())}, PeerIdentifier,
		WithTimeouts(2*time.Second, -1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()

	cli, err := Dial(ctx, path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	pi, err := cli.Hello("gvm-test/0.1.0")
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if !pi.Write {
		t.Error("a same-uid connection may not write to its own console")
	}
	// Running the suite as root is a legitimate CI shape, and root is the one
	// tier that does get the lifecycle ops.
	wantLifecycle := os.Getuid() == 0
	if pi.Stop != wantLifecycle || pi.Verb != wantLifecycle {
		t.Errorf("peer block %+v with uid %d", pi, os.Getuid())
	}
	if !wantLifecycle {
		if _, err := cli.Stop("root_cli", "test", nil, nil); !Denied(err) {
			t.Errorf("the instance user was allowed a stop: %v", err)
		}
	}

	be.Emit("SERVER STARTED")
	if err := cli.Tail(0, 10, false); err != nil {
		t.Fatalf("tail: %v", err)
	}
	ev, err := cli.Next()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if ev.Ev != console.EvLines || len(ev.Lines) != 1 || ev.Lines[0].Text != "SERVER STARTED" {
		t.Fatalf("tail returned %+v", ev)
	}

	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
	// The runner takes its own socket away, because RuntimeDirectoryPreserve
	// keeps the directory across a restart.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket outlived its listener: %v", err)
	}
}

// TestDefaultPolicyIsUsableWithoutTheGroup. A host that has not been prepared
// yet should still get a console for root and for the instance user rather
// than no console at all; `gvm doctor` is where the missing group is a finding.
func TestDefaultPolicyIsUsableWithoutTheGroup(t *testing.T) {
	p, err := DefaultPolicy()
	if p.OwnerUID != uint32(os.Getuid()) {
		t.Errorf("policy owner uid %d, want %d", p.OwnerUID, os.Getuid())
	}
	if err != nil {
		if !errors.Is(err, ErrNoConsoleGroup) {
			t.Fatalf("DefaultPolicy: %v", err)
		}
		if len(p.ConsoleGIDs) != 0 {
			t.Errorf("a missing group left gids %v", p.ConsoleGIDs)
		}
	} else if len(p.ConsoleGIDs) != 1 || p.ConsoleGIDs[0] == 0 {
		t.Errorf("policy console gids are %v", p.ConsoleGIDs)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("the default policy does not validate: %v", err)
	}
}
