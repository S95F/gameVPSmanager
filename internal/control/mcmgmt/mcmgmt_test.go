package mcmgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestMCMgmtClientKind(t *testing.T) {
	c, _ := New(Options{Addr: "127.0.0.1:25575", Gate: func() bool { return true }})
	if c.Kind() != spec.ControlMCMgmt {
		t.Errorf("Kind() = %q, want %q", c.Kind(), spec.ControlMCMgmt)
	}
}

func TestMCMgmtClientReadyFollowsGate(t *testing.T) {
	open := false
	c, _ := New(Options{Addr: "127.0.0.1:25575", Gate: func() bool { return open }})
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

func TestMCMgmtClientNewRequiresAddr(t *testing.T) {
	_, err := New(Options{Addr: "", Gate: func() bool { return true }})
	if err == nil {
		t.Error("New with empty Addr must fail")
	}
}

func TestMCMgmtClientExec(t *testing.T) {
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
		var req rpcRequest
		dec := json.NewDecoder(conn)
		if err := dec.Decode(&req); err != nil {
			return
		}
		result, _ := json.Marshal("OK")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"result":  json.RawMessage(result),
			"id":      req.ID,
		}
		enc, _ := json.Marshal(resp)
		fmt.Fprintf(conn, "%s\n", enc)
	}()

	c, _ := New(Options{
		Addr: ln.Addr().String(),
		Gate: func() bool { return true },
		Dialer: func(network, addr string, _ time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, addr, time.Second)
		},
	})

	out, err := c.Exec(context.Background(), "save-all")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("Exec reply = %q, want OK", out)
	}
}
