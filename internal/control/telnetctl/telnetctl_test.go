package telnetctl

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestTelnetClientKind(t *testing.T) {
	c, _ := New(Options{Addr: "127.0.0.1:8081", Gate: func() bool { return true }})
	if c.Kind() != spec.ControlTelnet {
		t.Errorf("Kind() = %q, want %q", c.Kind(), spec.ControlTelnet)
	}
}

func TestTelnetClientReadyFollowsGate(t *testing.T) {
	open := false
	c, _ := New(Options{Addr: "127.0.0.1:8081", Gate: func() bool { return open }})
	if c.Ready() {
		t.Error("Ready() = true when gate is closed")
	}
	if c.Unready() == "" {
		t.Error("Unready() is empty when gate is closed")
	}
	open = true
	if !c.Ready() {
		t.Error("Ready() = false when gate is open")
	}
	if c.Unready() != "" {
		t.Errorf("Unready() = %q when gate is open, want empty", c.Unready())
	}
}

func TestTelnetClientNotReadyReasonPassthrough(t *testing.T) {
	c, _ := New(Options{
		Addr:           "127.0.0.1:8081",
		Gate:           func() bool { return false },
		NotReadyReason: "7 Days to Die needs 5 minutes to load",
	})
	if !strings.Contains(c.Unready(), "7 Days to Die") {
		t.Errorf("Unready() = %q, want the custom reason", c.Unready())
	}
}

func TestTelnetClientNewRequiresAddr(t *testing.T) {
	_, err := New(Options{Addr: "", Gate: func() bool { return true }})
	if err == nil {
		t.Error("New with empty Addr must fail")
	}
}

func TestTelnetClientExec(t *testing.T) {
	// Start a minimal echo listener that sends one reply line then a blank.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		cmd := strings.TrimRight(string(buf[:n]), "\n\r")
		fmt.Fprintf(conn, "echo: %s\n\n", cmd)
	}()

	c, _ := New(Options{
		Addr: ln.Addr().String(),
		Gate: func() bool { return true },
		Dialer: func(network, addr string, _ time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, addr, time.Second)
		},
	})

	out, err := c.Exec(context.Background(), "version")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(out, "version") {
		t.Errorf("Exec reply = %q, want it to contain the command", out)
	}
}
