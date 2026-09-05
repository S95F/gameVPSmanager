package rcon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- harness

// fakeServer runs handler against a real loopback TCP connection so that the
// client's read/write deadlines behave exactly as they do in production. A
// net.Pipe would bypass them entirely and hide timeout bugs.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	received []packet
}

func newFakeServer(t *testing.T, handler func(s *fakeServer, conn net.Conn, br *bufio.Reader)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{t: t, ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(s, conn, bufio.NewReaderSize(conn, maxPacketSize))
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) record(p packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, packet{ID: p.ID, Type: p.Type, Body: append([]byte(nil), p.Body...)})
}

func (s *fakeServer) sawClientType(t PacketType) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.received {
		if p.Type == t {
			return true
		}
	}
	return false
}

func send(t *testing.T, conn net.Conn, p packet) {
	t.Helper()
	b, err := p.encode()
	if err != nil {
		t.Errorf("encode: %v", err)
		return
	}
	if _, err := conn.Write(b); err != nil {
		t.Logf("server write: %v", err)
	}
}

// handleAuth performs the server side of the handshake, optionally sending
// the empty RESPONSE_VALUE that some servers emit first and others omit.
func handleAuth(t *testing.T, s *fakeServer, conn net.Conn, br *bufio.Reader, password string, sendJunk bool) bool {
	t.Helper()
	p, err := readPacket(br)
	if err != nil {
		return false
	}
	s.record(p)
	if p.Type != TypeAuth {
		t.Errorf("first client packet was type %d, want TypeAuth(3)", p.Type)
		return false
	}
	if sendJunk {
		send(t, conn, packet{ID: p.ID, Type: TypeResponseValue})
	}
	if string(p.Body) != password {
		send(t, conn, packet{ID: authFailureID, Type: TypeAuthResponse})
		return false
	}
	send(t, conn, packet{ID: p.ID, Type: TypeAuthResponse})
	return true
}

func dialTest(t *testing.T, addr, password string, opts Options) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Dial(ctx, addr, password, opts)
}

func mustExec(t *testing.T, c *Client, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.Execute(ctx, cmd)
	if err != nil {
		t.Fatalf("Execute(%q): %v", cmd, err)
	}
	return out
}

// ---------------------------------------------------------------- packet codec

func TestPacketRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   packet
	}{
		{"empty body", packet{ID: 7, Type: TypeResponseValue}},
		{"command", packet{ID: 42, Type: TypeExecCommand, Body: []byte("players")}},
		{"auth", packet{ID: 1, Type: TypeAuth, Body: []byte("hunter2")}},
		{"max body", packet{ID: 9, Type: TypeResponseValue, Body: bytes.Repeat([]byte("x"), maxResponseBody)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.in.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := readPacket(bufio.NewReader(bytes.NewReader(b)))
			if err != nil {
				t.Fatalf("readPacket: %v", err)
			}
			if got.ID != tc.in.ID || got.Type != tc.in.Type || !bytes.Equal(got.Body, tc.in.Body) {
				t.Errorf("round trip mismatch:\n got %d/%d/%q\nwant %d/%d/%q",
					got.ID, got.Type, got.Body, tc.in.ID, tc.in.Type, tc.in.Body)
			}
		})
	}
}

// The size field counts everything after itself and never itself. Getting
// this backwards is the classic RCON framing bug: it works for the first
// packet and desynchronises the stream for every one after it.
func TestPacketSizeFieldExcludesItself(t *testing.T) {
	b, err := packet{ID: 1, Type: TypeExecCommand, Body: []byte("hi")}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got, want := len(b), 4+headerSize+2+paddingSize; got != want {
		t.Fatalf("wire length %d, want %d", got, want)
	}
	declared := int(int32(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24))
	if want := len(b) - 4; declared != want {
		t.Errorf("size field %d, want %d (total %d minus its own 4 bytes)", declared, want, len(b))
	}
}

func TestPacketRejectsOversizeBody(t *testing.T) {
	_, err := packet{ID: 1, Type: TypeExecCommand, Body: bytes.Repeat([]byte("x"), maxPacketSize)}.encode()
	if err == nil {
		t.Fatal("encode accepted an oversize body, want error")
	}
}

func TestReadPacketRejectsBadFraming(t *testing.T) {
	for _, tc := range []struct {
		name string
		size uint32
	}{
		{"below minimum", 4},
		{"above maximum", maxPacketSize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte{byte(tc.size), byte(tc.size >> 8), byte(tc.size >> 16), byte(tc.size >> 24)}
			if _, err := readPacket(bufio.NewReader(bytes.NewReader(b))); err == nil {
				t.Error("readPacket accepted invalid framing, want error")
			}
		})
	}
}

// Minecraft sends ISO-8859-1 and routinely includes the section sign (0xA7)
// that prefixes its colour codes. Decoding that as UTF-8 replaces it with
// U+FFFD and corrupts the output.
func TestDecodeTextLatin1Fallback(t *testing.T) {
	if got, want := decodeText([]byte("plain ascii")), "plain ascii"; got != want {
		t.Errorf("ascii: got %q want %q", got, want)
	}
	if got, want := decodeText([]byte("héllo")), "héllo"; got != want {
		t.Errorf("utf-8 should pass through: got %q want %q", got, want)
	}
	got := decodeText([]byte{'a', 0xA7, 'c'})
	if want := "a§c"; got != want {
		t.Errorf("latin-1 fallback: got %q want %q", got, want)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("latin-1 fallback produced a replacement character; bytes were lost")
	}
}

// ---------------------------------------------------------------- auth

func TestAuthSuccessToleratesJunkPacket(t *testing.T) {
	for _, sendJunk := range []bool{true, false} {
		name := "with junk packet"
		if !sendJunk {
			name = "without junk packet"
		}
		t.Run(name, func(t *testing.T) {
			s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
				handleAuth(t, s, conn, br, "pw", sendJunk)
				time.Sleep(50 * time.Millisecond)
			})
			c, err := dialTest(t, s.addr(), "pw", Options{})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer c.Close()
		})
	}
}

func TestAuthFailureReturnsErrAuthFailed(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		handleAuth(t, s, conn, br, "correct", false)
	})
	_, err := dialTest(t, s.addr(), "wrong", Options{})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial with bad password: got %v, want ErrAuthFailed", err)
	}
}

// Some servers just drop the connection instead of sending the -1 packet.
func TestAuthConnectionDropIsAuthFailure(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		if p, err := readPacket(br); err == nil {
			s.record(p)
		}
		_ = conn.Close()
	})
	_, err := dialTest(t, s.addr(), "pw", Options{})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial against a server that hangs up: got %v, want ErrAuthFailed", err)
	}
}

// ---------------------------------------------------------------- source

// sourceHandler mimics SRCDS: it fragments the response, answers the client's
// empty RESPONSE_VALUE probe by mirroring it, and then emits the documented
// malformed trailing packet.
func sourceHandler(t *testing.T, fragments []string) func(*fakeServer, net.Conn, *bufio.Reader) {
	return func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		if !handleAuth(t, s, conn, br, "pw", true) {
			return
		}
		var pending int32
		for {
			p, err := readPacket(br)
			if err != nil {
				return
			}
			s.record(p)
			switch p.Type {
			case TypeExecCommand:
				pending = p.ID
				for _, f := range fragments {
					send(t, conn, packet{ID: pending, Type: TypeResponseValue, Body: []byte(f)})
				}
			case TypeResponseValue:
				// The terminator probe: mirror it empty, then send the
				// malformed packet SRCDS is documented to append.
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue})
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue, Body: []byte{0x00, 0x01, 0x00, 0x00}})
			}
		}
	}
}

func TestSourceReassemblesFragmentedResponse(t *testing.T) {
	big := strings.Repeat("A", maxResponseBody)
	s := newFakeServer(t, sourceHandler(t, []string{big, "TAIL"}))

	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectSource})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out := mustExec(t, c, "status")
	if want := big + "TAIL"; out != want {
		t.Fatalf("fragmented response not reassembled: got %d bytes, want %d", len(out), len(want))
	}
}

// The malformed trailing packet must be consumed, or it is still sitting in
// the buffer when the next command runs and every later response is shifted
// by one packet.
func TestSourceSecondCommandIsNotDesynchronised(t *testing.T) {
	s := newFakeServer(t, sourceHandler(t, []string{"first"}))

	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectSource})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if got := mustExec(t, c, "one"); got != "first" {
		t.Fatalf("first command: got %q want %q", got, "first")
	}
	if got := mustExec(t, c, "two"); got != "first" {
		t.Fatalf("second command desynchronised: got %q want %q", got, "first")
	}
}

// ---------------------------------------------------------------- minecraft

// Vanilla Minecraft rejects a client-sent SERVERDATA_RESPONSE_VALUE with the
// body "Unknown request 0", so the Valve terminator trick must not be used.
func TestMinecraftNeverSendsResponseValueProbe(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		if !handleAuth(t, s, conn, br, "pw", false) {
			return
		}
		for {
			p, err := readPacket(br)
			if err != nil {
				return
			}
			s.record(p)
			switch {
			case p.Type == TypeResponseValue:
				// What vanilla actually does. If the client ever gets here
				// the terminator is broken.
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue, Body: []byte("Unknown request 0")})
			case p.Type == TypeExecCommand && len(p.Body) == 0:
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue})
			case p.Type == TypeExecCommand:
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue, Body: []byte("There are 3 of a max of 20 players online")})
			}
		}
	})

	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectMinecraft})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out := mustExec(t, c, "list")
	if want := "There are 3 of a max of 20 players online"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if s.sawClientType(TypeResponseValue) {
		t.Error("client sent a SERVERDATA_RESPONSE_VALUE probe to a Minecraft server; vanilla rejects it")
	}
}

func TestMinecraftRejectsOversizeCommand(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		handleAuth(t, s, conn, br, "pw", false)
		time.Sleep(100 * time.Millisecond)
	})
	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectMinecraft})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(context.Background(), strings.Repeat("x", 1447))
	if !errors.Is(err, ErrCommandTooLong) {
		t.Fatalf("got %v, want ErrCommandTooLong (Minecraft caps a request body at 1446)", err)
	}
	// The same command is fine on a Source server.
	if got := specFor(DialectSource).maxRequestBody; got <= 1447 {
		t.Errorf("source maxRequestBody is %d; expected the larger Valve limit", got)
	}
}

// ---------------------------------------------------------------- rust

// Rust interleaves unsolicited type-4 broadcast packets and answers some
// commands with id -1 instead of the request id.
func TestRustDiscardsBroadcastsAndRemapsNegativeID(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		if !handleAuth(t, s, conn, br, "pw", true) {
			return
		}
		for {
			p, err := readPacket(br)
			if err != nil {
				return
			}
			s.record(p)
			switch p.Type {
			case TypeExecCommand:
				send(t, conn, packet{ID: 0, Type: TypeRustMessage, Body: []byte("player joined")})
				send(t, conn, packet{ID: authFailureID, Type: TypeResponseValue, Body: []byte("said it")})
				send(t, conn, packet{ID: 0, Type: TypeRustMessage, Body: []byte("player left")})
			case TypeResponseValue:
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue})
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue, Body: []byte{0x00, 0x01, 0x00, 0x00}})
			}
		}
	})

	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectRust})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out := mustExec(t, c, "say hello")
	if out != "said it" {
		t.Fatalf("got %q want %q (type-4 broadcasts must be dropped, id -1 remapped)", out, "said it")
	}
}

// A -1 during the handshake still means auth failure, even for Rust.
func TestRustNegativeIDStillFailsAuth(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		handleAuth(t, s, conn, br, "correct", true)
	})
	_, err := dialTest(t, s.addr(), "wrong", Options{Dialect: DialectRust})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("got %v, want ErrAuthFailed", err)
	}
}

// ---------------------------------------------------------------- simple

// DialectSimple sends no probe at all and stops when the server goes quiet.
func TestSimpleDialectSendsNoProbeAndTerminatesOnIdle(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		if !handleAuth(t, s, conn, br, "pw", false) {
			return
		}
		for {
			p, err := readPacket(br)
			if err != nil {
				return
			}
			s.record(p)
			if p.Type == TypeExecCommand {
				send(t, conn, packet{ID: p.ID, Type: TypeResponseValue, Body: []byte("world saved")})
			}
		}
	})

	c, err := dialTest(t, s.addr(), "pw", Options{Dialect: DialectSimple, IdleWindow: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if got := mustExec(t, c, "save"); got != "world saved" {
		t.Fatalf("got %q want %q", got, "world saved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.received {
		if p.Type == TypeResponseValue {
			t.Error("DialectSimple sent a terminator probe; it is meant to send none")
		}
	}
}

// ---------------------------------------------------------------- lifecycle

func TestExecuteAfterCloseReturnsErrClosed(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, br *bufio.Reader) {
		handleAuth(t, s, conn, br, "pw", false)
		time.Sleep(100 * time.Millisecond)
	})
	c, err := dialTest(t, s.addr(), "pw", Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
	if _, err := c.Execute(context.Background(), "status"); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestRequestIDsAvoidZeroAndMinusOne(t *testing.T) {
	c := &Client{nextID: 1}
	seen := map[int32]bool{}
	for i := 0; i < 1000; i++ {
		id := c.nextRequestID()
		if id == 0 || id == authFailureID {
			t.Fatalf("nextRequestID produced reserved id %d", id)
		}
		seen[id] = true
	}
	// Wrapping must not stall on a single value.
	c.nextID = 1<<31 - 3
	for i := 0; i < 5; i++ {
		if id := c.nextRequestID(); id <= 0 {
			t.Fatalf("nextRequestID produced %d after wrap", id)
		}
	}
}
