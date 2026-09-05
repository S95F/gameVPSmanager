package probe

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func listenTCP(t *testing.T) (addr string, stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l.Addr().String(), func() {
		_ = l.Close()
		<-done
	}
}

func TestTCPProbeSucceedsAgainstAListener(t *testing.T) {
	addr, stop := listenTCP(t)
	defer stop()

	p := &TCP{Port: "rcon", Addr: addr, Timeout: 5 * time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(p.Describe(), "rcon") || !strings.Contains(p.Describe(), addr) {
		t.Errorf("Describe() = %q", p.Describe())
	}
	if p.Kind() != KindTCP {
		t.Errorf("Kind() = %q", p.Kind())
	}
}

func TestTCPProbeFailsAgainstAClosedPort(t *testing.T) {
	addr, stop := listenTCP(t)
	stop() // the port is now free, and nothing is listening on it

	p := &TCP{Addr: addr, Timeout: 2 * time.Second}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("a closed port reported ready")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error does not name the target: %v", err)
	}
}

// The probe connects and closes without writing. It has no idea what protocol
// is on the other side, and a game's handshake parser reading an unexpected
// byte is at best a warning in the server log.
func TestTCPProbeWritesNothing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	got := make(chan int, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		got <- n
	}()

	p := &TCP{Addr: l.Addr().String(), Timeout: 2 * time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := <-got; n > 0 {
		t.Fatalf("the probe wrote %d bytes into a game's protocol handler", n)
	}
}

// A probe carries its own timeout so a caller sequencing several does not have
// to remember which of them can block.
func TestTCPProbeAppliesItsOwnTimeout(t *testing.T) {
	p := &TCP{
		Addr:    "203.0.113.1:27015",
		Timeout: 20 * time.Millisecond,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	start := time.Now()
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("a dial that never completes reported ready")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the probe's own timeout was ignored: waited %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the underlying cause is lost: %v", err)
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
}
