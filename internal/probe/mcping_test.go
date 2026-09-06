package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusResponse frames a JSON status the way a server does: a length-prefixed
// packet whose body is a VarInt id of 0 followed by a length-prefixed string.
func statusResponse(jsonBody string) []byte {
	body := appendVarInt(nil, mcStatusPacketID)
	body = appendMCString(body, jsonBody)
	return frameMCPacket(body)
}

const sampleStatusJSON = `{"version":{"name":"Paper 1.21.9","protocol":773},` +
	`"players":{"max":20,"online":2,"sample":[{"name":"steve","id":"1-2-3"}]},` +
	`"description":{"text":"gvm ","extra":[{"text":"test server"}]},` +
	`"enforcesSecureChat":true}`

// fakeMC is a loopback TCP server that reads a handshake plus a status
// request and answers with a scripted response, recording what it was sent.
type fakeMC struct {
	addr string

	mu       sync.Mutex
	requests [][]byte
}

func (f *fakeMC) seen() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.requests))
	copy(out, f.requests)
	return out
}

// startFakeMC serves exactly one connection. reply is called with the two
// packets the probe sent and returns the bytes to answer with; returning nil
// closes the connection without answering, which is how a pre-1.7 server's
// reaction to a modern handshake is simulated.
func startFakeMC(t *testing.T, reply func(handshake, request []byte) []byte) *fakeMC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMC{addr: ln.Addr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(c)
		var packets [][]byte
		for i := 0; i < 2; i++ {
			p, err := readWholePacket(br)
			if err != nil {
				return
			}
			packets = append(packets, p)
		}
		f.mu.Lock()
		f.requests = packets
		f.mu.Unlock()
		if out := reply(packets[0], packets[1]); out != nil {
			_, _ = c.Write(out)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return f
}

// readWholePacket reads a length-prefixed packet and returns its body.
func readWholePacket(br *bufio.Reader) ([]byte, error) {
	n, err := readVarInt(br)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func TestMCPingAgainstAFakeServer(t *testing.T) {
	f := startFakeMC(t, func(handshake, request []byte) []byte {
		return statusResponse(sampleStatusJSON)
	})

	p := &MCPing{Addr: f.addr, Port: "game", Timeout: 5 * time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.Last == nil {
		t.Fatal("Last is nil after a successful run")
	}
	if p.Last.Players.Online != 2 || p.Last.Players.Max != 20 {
		t.Errorf("players %d/%d, want 2/20", p.Last.Players.Online, p.Last.Players.Max)
	}
	if p.Last.Version.Name != "Paper 1.21.9" || p.Last.Version.Protocol != 773 {
		t.Errorf("version %+v", p.Last.Version)
	}
	if got := p.Last.DescriptionText(); got != "gvm test server" {
		t.Errorf("DescriptionText() = %q, want %q", got, "gvm test server")
	}

	// The handshake is what makes this a status ping rather than a login: it
	// must ask for state 1, carry the host and port it dialled, and claim no
	// protocol version of its own.
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d packets, want a handshake and a status request", len(reqs))
	}
	id, protocol, host, port, next := parseHandshake(t, reqs[0])
	if id != mcHandshakePacketID || next != mcStateStatus {
		t.Errorf("handshake id %d next state %d, want %d and %d",
			id, next, mcHandshakePacketID, mcStateStatus)
	}
	if protocol != mcProtocolUnknown {
		t.Errorf("protocol = %d, want %d: a pinger must not claim a client version",
			protocol, mcProtocolUnknown)
	}
	wantHost, wantPort, err := splitHostPort(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	if host != wantHost || port != wantPort {
		t.Errorf("handshake announced %s:%d, want %s:%d", host, port, wantHost, wantPort)
	}
	if len(reqs[1]) != 1 || reqs[1][0] != mcStatusPacketID {
		t.Errorf("status request = %x, want a single 0x00 byte", reqs[1])
	}
}

// A server behind a proxy routes on the handshake's address field, so a probe
// that always announced the dial address could never reach one.
func TestMCPingAnnouncesTheHostOverride(t *testing.T) {
	f := startFakeMC(t, func(handshake, request []byte) []byte {
		return statusResponse(sampleStatusJSON)
	})
	p := &MCPing{Addr: f.addr, Host: "mc.example.test", Timeout: 5 * time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, host, _, _ := parseHandshake(t, f.seen()[0])
	if host != "mc.example.test" {
		t.Fatalf("handshake announced %q, want the override", host)
	}
}

// A pre-1.7 server does not speak this exchange. Closing the connection is
// what one actually does, and the probe must report not-ready rather than
// blocking or panicking on the empty read.
func TestMCPingFailsWhenTheServerHangsUp(t *testing.T) {
	f := startFakeMC(t, func(handshake, request []byte) []byte { return nil })
	p := &MCPing{Addr: f.addr, Timeout: 2 * time.Second}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against a server that hung up")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
}

func TestMCPingFailsAgainstAClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := &MCPing{Addr: addr, Timeout: 2 * time.Second}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded against a closed port")
	}
}

// parseHandshake decodes the handshake packet body the probe sent.
func parseHandshake(t *testing.T, body []byte) (id, protocol int32, host string, port uint16, next int32) {
	t.Helper()
	r := bytes.NewReader(body)
	var err error
	if id, err = readVarInt(r); err != nil {
		t.Fatal(err)
	}
	if protocol, err = readVarInt(r); err != nil {
		t.Fatal(err)
	}
	if host, err = readMCString(r); err != nil {
		t.Fatal(err)
	}
	hi, err := r.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	lo, err := r.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	port = uint16(hi)<<8 | uint16(lo)
	if next, err = readVarInt(r); err != nil {
		t.Fatal(err)
	}
	return id, protocol, host, port, next
}

// The VarInt is the protocol's whole framing, so it gets the table. -1 costing
// five bytes is not a curiosity: it is what the handshake sends, and an
// encoder that stopped at the sign bit would produce a packet no server reads.
func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 2, 127, 128, 255, 2097151, 2147483647, -1, -2147483648} {
		encoded := appendVarInt(nil, v)
		got, err := readVarInt(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("readVarInt(%d encoded as %x): %v", v, encoded, err)
		}
		if got != v {
			t.Errorf("round trip of %d gave %d (encoding %x)", v, got, encoded)
		}
	}
	if n := len(appendVarInt(nil, -1)); n != 5 {
		t.Errorf("-1 encodes to %d bytes, want 5", n)
	}
	if n := len(appendVarInt(nil, 0)); n != 1 {
		t.Errorf("0 encodes to %d bytes, want 1", n)
	}
}

// A fifth byte with the continuation bit set is not a large number, it is a
// malformed stream -- and reading on would let a peer stall the probe one byte
// at a time for as long as it liked.
func TestVarIntRefusesAnOverlongEncoding(t *testing.T) {
	overlong := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}
	if _, err := readVarInt(bytes.NewReader(overlong)); !errors.Is(err, ErrMCVarInt) {
		t.Fatalf("err = %v, want ErrMCVarInt", err)
	}
	if _, err := readVarInt(bytes.NewReader([]byte{0x80})); !errors.Is(err, ErrMCVarInt) {
		t.Fatalf("truncated VarInt err = %v, want ErrMCVarInt", err)
	}
	if _, err := readVarInt(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reader err = %v, want io.EOF", err)
	}
}

// The decoder takes an io.Reader so every response shape is tested against
// recorded bytes, with no socket and no platform in the way.
func TestReadMCStatusRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []byte
		wantErr error
	}{
		{
			name:    "a packet id that is not a status response",
			in:      frameMCPacket(append(appendVarInt(nil, 0x01), 0x00)),
			wantErr: ErrMCUnexpectedPacket,
		},
		{
			name:    "a zero length packet",
			in:      appendVarInt(nil, 0),
			wantErr: ErrMCUnexpectedPacket,
		},
		{
			// A server announcing a packet larger than the read budget does
			// not get to name this process's allocation size.
			name:    "a packet above the read budget",
			in:      appendVarInt(nil, maxMCPacket+1),
			wantErr: ErrMCPacketTooLarge,
		},
		{
			name:    "a body shorter than its own length prefix",
			in:      append(appendVarInt(nil, 32), 0x00, 0x01),
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			// The string's own length prefix outruns the packet: a status
			// that decoded the fragment would hand the caller half a MOTD and
			// a JSON error nobody can act on.
			name: "a json string shorter than its length prefix",
			in: frameMCPacket(append(append(appendVarInt(nil, mcStatusPacketID),
				appendVarInt(nil, 99)...), "{}"...)),
			wantErr: io.ErrUnexpectedEOF,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadMCStatus(bytes.NewReader(tc.in))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadMCStatusRejectsMalformedJSON(t *testing.T) {
	if _, err := ReadMCStatus(bytes.NewReader(statusResponse("{not json"))); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

// The MOTD is a chat component in every version that matters and a bare string
// in some older and modded ones. All three shapes appear in the wild, so the
// flattener handles all three rather than asserting one.
func TestDescriptionText(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"a bare string", `"a plain motd"`, "a plain motd"},
		{"a text component", `{"text":"hello"}`, "hello"},
		{"text plus extra", `{"text":"a ","extra":[{"text":"b "},{"text":"c"}]}`, "a b c"},
		{"an array", `["one ",{"text":"two"}]`, "one two"},
		{"nested extra", `{"text":"1","extra":[{"text":"2","extra":["3"]}]}`, "123"},
		{"an unknown shape", `42`, ""},
		{"absent", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s MCStatus
			if tc.raw != "" {
				s.Description = json.RawMessage(tc.raw)
			}
			if got := s.DescriptionText(); got != tc.want {
				t.Fatalf("DescriptionText() = %q, want %q", got, tc.want)
			}
		})
	}
	var nilStatus *MCStatus
	if got := nilStatus.DescriptionText(); got != "" {
		t.Errorf("nil status DescriptionText() = %q", got)
	}
}

func TestMCPingDescribe(t *testing.T) {
	p := &MCPing{Addr: "127.0.0.1:25565", Port: "game"}
	got := p.Describe()
	for _, want := range []string{"server-list ping", "127.0.0.1:25565", "game"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}

func TestMCPingRejectsABadAddress(t *testing.T) {
	p := &MCPing{Addr: "not-a-host-port"}
	err := p.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("err = %v, want a complaint about the address", err)
	}
}
