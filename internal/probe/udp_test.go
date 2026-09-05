package probe

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// classify is where the one genuinely platform-shaped decision lives: Linux
// reports ECONNREFUSED for an ICMP port-unreachable on a connected UDP socket
// and Windows reports WSAECONNRESET. The rule names neither -- a TIMEOUT means
// nothing rejected us, and anything else means something did -- and this is the
// table that proves it, on a box where neither errno can be produced on demand.
func TestClassify(t *testing.T) {
	timeout := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	refused := &net.OpError{Op: "read", Err: os.NewSyscallError("recvfrom", syscall.ECONNREFUSED)}

	for _, tc := range []struct {
		name        string
		n           int
		err         error
		expectReply bool
		wantErr     bool
	}{
		{"a reply is always ready", 12, nil, false, false},
		{"a reply satisfies expect_reply", 12, nil, true, false},
		{"silence passes without expect_reply", 0, timeout, false, false},
		{"silence fails with expect_reply", 0, timeout, true, true},
		{"a rejection fails", 0, refused, false, true},
		{"a rejection fails even with expect_reply", 0, refused, true, true},
		{"an empty datagram is a reply", 0, nil, false, false},
		{"an empty datagram is not enough for expect_reply", 0, nil, true, true},
		{"deadline exceeded from the context is a timeout", 0, context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(KindUDP, "127.0.0.1:2457", tc.n, tc.err, tc.expectReply)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classify(%d, %v, expectReply=%v) = %v, wantErr %v",
					tc.n, tc.err, tc.expectReply, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrNotReady) {
				t.Errorf("error does not wrap ErrNotReady: %v", err)
			}
		})
	}
}

// Silence is the pass condition, and it is a weak one on purpose: this is the
// probe every manifest sequences AFTER a log marker rather than instead of one.
func TestUDPProbePassesAgainstABoundSocket(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	p := &UDP{Port: "query", Addr: pc.LocalAddr().String(), Window: 100 * time.Millisecond}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run against a bound udp socket: %v", err)
	}
	if !strings.Contains(p.Describe(), "query") {
		t.Errorf("Describe() = %q", p.Describe())
	}
	if p.Kind() != KindUDP {
		t.Errorf("Kind() = %q", p.Kind())
	}
}

func TestUDPProbeWithExpectReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	// An echo responder, which is what A2S and server-list-ping will be in
	// Phase 5.
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()

	p := &UDP{
		Addr:        pc.LocalAddr().String(),
		Payload:     []byte("\xff\xff\xff\xffTSource Engine Query\x00"),
		ExpectReply: true,
		Window:      2 * time.Second,
	}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run against an echo responder: %v", err)
	}
}

// A silent socket fails the moment a reply is required, which is the
// difference between "bound" and "answering".
func TestUDPProbeExpectReplyFailsOnSilence(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	p := &UDP{Addr: pc.LocalAddr().String(), ExpectReply: true, Window: 50 * time.Millisecond}
	err = p.Run(context.Background())
	if err == nil {
		t.Fatal("a silent socket satisfied expect_reply")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if !strings.Contains(err.Error(), "no reply") {
		t.Errorf("error does not say what was missing: %v", err)
	}
}

// The default payload is one inert byte: a zero-length datagram is not
// delivered consistently by every stack, and a longer one risks being PARSED by
// the game.
func TestUDPProbeDefaultPayloadIsOneByte(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 512)
		_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			got <- -1
			return
		}
		got <- n
	}()

	p := &UDP{Addr: pc.LocalAddr().String(), Window: 100 * time.Millisecond}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := <-got; n != 1 {
		t.Fatalf("the probe sent %d bytes, want 1", n)
	}
}

func TestUDPProbeReportsADialFailure(t *testing.T) {
	p := &UDP{Addr: "not-a-host:not-a-port", Window: 10 * time.Millisecond}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("an unresolvable address reported ready")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
}
