package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// KindMCPing is the manifest's `readiness.probes[].kind` for this probe.
const KindMCPing = "mc_ping"

// The two packet ids this probe uses. Both are 0x00: in the Minecraft protocol
// packet ids are scoped to the connection STATE, so the handshake's 0x00 and
// the status request's 0x00 are different packets that happen to share a
// number. They are named separately for exactly that reason.
const (
	mcHandshakePacketID = 0x00
	mcStatusPacketID    = 0x00
)

// mcStateStatus is the `next state` a handshake asks for when it wants a
// server-list ping rather than a login. Asking for status is what makes this
// probe cost the server nothing: it never enters the login flow, never touches
// the player list, and never appears in the join log.
const mcStateStatus = 1

// mcProtocolUnknown is the protocol version a pinger sends.
//
// -1 is the convention for "I am not a client and I am not claiming a version".
// Sending a real protocol number would make the server's version comparison
// meaningful, and a status ping that reports "outdated client" instead of the
// server's own version is a readiness probe that fails on every version bump.
const mcProtocolUnknown int32 = -1

// maxMCPacket bounds a status response. A JSON status with a 64x64 favicon
// inlined as a base64 data URI is normally under 100 KB; the ceiling is far
// above that and exists only so a broken or hostile peer cannot name the
// allocation size.
const maxMCPacket = 2 << 20

// maxVarIntBytes is the VarInt encoding's own limit. A fifth byte with the
// continuation bit set is not a large number, it is a malformed stream, and
// reading on would let a peer stall the probe one byte at a time.
const maxVarIntBytes = 5

var (
	// ErrMCVarInt is a malformed VarInt: more than five bytes, or a truncated
	// one.
	ErrMCVarInt = errors.New("probe: malformed VarInt")
	// ErrMCPacketTooLarge means the server announced a packet above
	// maxMCPacket.
	ErrMCPacketTooLarge = errors.New("probe: status packet exceeds the read budget")
	// ErrMCUnexpectedPacket means the server answered the status request with
	// something other than a status response. A pre-1.7 server answering the
	// legacy 0xFE ping lands here.
	ErrMCUnexpectedPacket = errors.New("probe: unexpected packet in response to a status request")
)

// MCStatus is the decoded server-list ping response.
//
// Description is kept raw because it is a chat component in every version that
// matters and a bare string in some older and some modded ones. Decoding it
// into a string is DescriptionText's job, and a caller that wants the component
// tree still has it.
type MCStatus struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
		Sample []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"sample"`
	} `json:"players"`
	Description json.RawMessage `json:"description"`
	// Favicon is deliberately not decoded: it is a base64 PNG that no part of
	// gvm renders, and holding it would make every status log line enormous.
	EnforcesSecureChat bool `json:"enforcesSecureChat"`
}

// DescriptionText flattens the MOTD to plain text, dropping colour codes and
// component formatting. It is what a status line shows.
func (s *MCStatus) DescriptionText() string {
	if s == nil || len(s.Description) == 0 {
		return ""
	}
	return chatText(s.Description)
}

// chatText walks a chat component. The three shapes are a bare string, an
// object with `text` plus optional `extra`, and an array whose first element is
// the parent -- all three appear in the wild depending on the server software,
// so the parser handles all three rather than asserting one.
func chatText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		var out string
		for _, e := range arr {
			out += chatText(e)
		}
		return out
	}
	var obj struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	out := obj.Text
	for _, e := range obj.Extra {
		out += chatText(e)
	}
	return out
}

// MCPing reports a port ready when a Minecraft server answers a server-list
// ping on it.
//
// The exchange is the same one the multiplayer menu makes: a handshake asking
// for the status state, then a status request. It is the cheapest positive
// signal a Minecraft server can give -- the listener is not merely bound, the
// server is far enough along to serialise its own status -- and unlike RCON it
// does not run on the main server thread (section 11.9).
//
// Pre-1.7 servers speak the legacy 0xFE ping instead and will close or garble
// this exchange. That is left unimplemented on purpose: gvm's Minecraft support
// targets Paper-family builds, and a fallback that half-parsed a 2013 protocol
// would report a version string nobody could act on.
type MCPing struct {
	// Port is the manifest port NAME, for the description.
	Port string

	// Addr is host:port.
	Addr string

	// Host is what the handshake claims it connected to. Empty means the host
	// part of Addr. It is separate because a server behind a proxy routes on
	// this field, so pinging 127.0.0.1 while announcing the public hostname is
	// sometimes the only exchange that gets an answer.
	Host string

	// Timeout bounds the whole attempt. Zero defers to the caller's context.
	Timeout time.Duration

	// Dial is the seam for tests. nil uses a real net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// Last is the most recent successful status, for the status line and the
	// player count.
	Last *MCStatus
}

var _ Probe = (*MCPing)(nil)

// Kind implements Probe.
func (p *MCPing) Kind() string { return KindMCPing }

// Describe implements Probe.
func (p *MCPing) Describe() string {
	what := "a Minecraft server-list ping reply from " + p.Addr
	if p.Port != "" {
		what += " (port " + p.Port + ")"
	}
	return what
}

// Run implements Probe.
func (p *MCPing) Run(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	host, port, err := splitHostPort(p.Addr)
	if err != nil {
		return fail(KindMCPing, p.Addr, "not a host:port address", err)
	}
	if p.Host != "" {
		host = p.Host
	}

	dial := p.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	c, err := dial(ctx, "tcp", p.Addr)
	if err != nil {
		return fail(KindMCPing, p.Addr, "connect failed", err)
	}
	defer func() { _ = c.Close() }()

	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}

	req := append(mcHandshake(mcProtocolUnknown, host, port, mcStateStatus), mcStatusRequest()...)
	if _, err := c.Write(req); err != nil {
		return fail(KindMCPing, p.Addr, "sending the status request", err)
	}

	status, err := ReadMCStatus(c)
	if err != nil {
		if isTimeout(err) {
			return fail(KindMCPing, p.Addr, "no status reply within the window", nil)
		}
		return fail(KindMCPing, p.Addr, "reading the status reply", err)
	}
	p.Last = status
	return nil
}

// ReadMCStatus reads one status response off a connection.
//
// It takes an io.Reader rather than doing its own dialling so the whole
// decoder -- VarInts, packet framing, the JSON shape and the chat component --
// is testable against a recorded byte string on any platform, with no socket
// involved.
func ReadMCStatus(r io.Reader) (*MCStatus, error) {
	id, body, err := readMCPacket(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	if id != mcStatusPacketID {
		return nil, fmt.Errorf("%w: packet id 0x%02x", ErrMCUnexpectedPacket, id)
	}
	payload, err := readMCString(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("status json: %w", err)
	}
	var st MCStatus
	if err := json.Unmarshal([]byte(payload), &st); err != nil {
		return nil, fmt.Errorf("status json: %w", err)
	}
	return &st, nil
}

// byteReader is what every decoder here needs: bulk reads for payloads and
// single bytes for VarInts. bufio.Reader and bytes.Reader both satisfy it, so
// the same functions decode a live socket and a recorded packet.
type byteReader interface {
	io.Reader
	io.ByteReader
}

// readMCPacket reads one length-prefixed packet and returns its id and body.
func readMCPacket(br byteReader) (id int32, body []byte, err error) {
	length, err := readVarInt(br)
	if err != nil {
		return 0, nil, fmt.Errorf("packet length: %w", err)
	}
	if length <= 0 {
		return 0, nil, fmt.Errorf("%w: packet length %d", ErrMCUnexpectedPacket, length)
	}
	if int64(length) > maxMCPacket {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrMCPacketTooLarge, length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(br, buf); err != nil {
		return 0, nil, fmt.Errorf("packet body: %w", err)
	}
	rest := bytes.NewReader(buf)
	id, err = readVarInt(rest)
	if err != nil {
		return 0, nil, fmt.Errorf("packet id: %w", err)
	}
	return id, buf[len(buf)-rest.Len():], nil
}

// mcHandshake builds the handshake packet.
func mcHandshake(protocol int32, host string, port uint16, next int32) []byte {
	var body []byte
	body = appendVarInt(body, mcHandshakePacketID)
	body = appendVarInt(body, protocol)
	body = appendMCString(body, host)
	body = append(body, byte(port>>8), byte(port))
	body = appendVarInt(body, next)
	return frameMCPacket(body)
}

// mcStatusRequest builds the empty status request that follows a handshake.
func mcStatusRequest() []byte {
	return frameMCPacket(appendVarInt(nil, mcStatusPacketID))
}

// frameMCPacket prefixes a packet body with its VarInt length.
func frameMCPacket(body []byte) []byte {
	out := appendVarInt(nil, int32(len(body)))
	return append(out, body...)
}

// appendMCString writes a VarInt-length-prefixed UTF-8 string.
func appendMCString(b []byte, s string) []byte {
	b = appendVarInt(b, int32(len(s)))
	return append(b, s...)
}

// readMCString reads a VarInt-length-prefixed UTF-8 string.
func readMCString(br byteReader) (string, error) {
	n, err := readVarInt(br)
	if err != nil {
		return "", err
	}
	if n < 0 || int64(n) > maxMCPacket {
		return "", fmt.Errorf("%w: string length %d", ErrMCPacketTooLarge, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// appendVarInt encodes v in Minecraft's VarInt form: seven bits per byte,
// little end first, high bit as the continuation flag. Negative numbers are
// encoded as their unsigned 32-bit two's complement, which is why -1 costs the
// full five bytes.
func appendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7F)|0x80)
		u >>= 7
	}
}

// readVarInt decodes one VarInt.
func readVarInt(r io.ByteReader) (int32, error) {
	var value uint32
	for i := 0; i < maxVarIntBytes; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && i > 0 {
				return 0, fmt.Errorf("%w: truncated after %d bytes", ErrMCVarInt, i)
			}
			return 0, err
		}
		value |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(value), nil
		}
	}
	return 0, fmt.Errorf("%w: longer than %d bytes", ErrMCVarInt, maxVarIntBytes)
}

// splitHostPort is net.SplitHostPort with the port already converted, because
// every caller here wants a uint16 and none of them wants to write the same
// two error paths again.
func splitHostPort(addr string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, err
	}
	return host, uint16(n), nil
}
