package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// KindA2S is the manifest's `readiness.probes[].kind` for this probe.
const KindA2S = "a2s"

// The A2S wire constants, named rather than inlined because the difference
// between 0x49 and 0x41 is the difference between an answer and a demand for a
// challenge, and a bare hex literal in a switch hides that.
const (
	// a2sSimpleHeader prefixes every unsplit A2S datagram: int32 -1.
	a2sSimpleHeader uint32 = 0xFFFFFFFF
	// a2sSplitHeader prefixes a fragment of a split response: int32 -2.
	a2sSplitHeader uint32 = 0xFFFFFFFE

	a2sInfoRequestByte  = 'T' // A2S_INFO
	a2sInfoResponseByte = 'I' // S2A_INFO_SRC
	a2sChallengeByte    = 'A' // S2C_CHALLENGE
	a2sGoldSourceByte   = 'm' // S2A_INFO_DETAILED, the pre-2006 layout
)

// a2sInfoPayload is the string every A2S_INFO request carries. The trailing
// NUL is part of it; servers reject a request without it.
const a2sInfoPayload = "Source Engine Query\x00"

// theShipAppID is the one app whose S2A_INFO_SRC has three extra bytes in the
// middle of it. Valve documented the exception rather than versioning the
// packet, so every parser that wants to read Version on any other game has to
// know about a 2007 co-op murder game.
const theShipAppID = 2400

// maxA2SDatagram bounds the read buffer. A2S_INFO answers are a few hundred
// bytes; the ceiling exists so a hostile or confused peer on the query port
// cannot decide how much this process allocates.
const maxA2SDatagram = 4096

// ErrA2SUnsupportedFormat means something answered on the query port in a
// dialect this parser does not read. It is separated from a plain failure
// because the operator's next move is different: the port is alive and
// something is listening, but it is not a Source-format server.
var ErrA2SUnsupportedFormat = errors.New("probe: unrecognised A2S response format")

// Info is a parsed S2A_INFO_SRC reply.
//
// Everything up to and including Version is parsed. The Extra Data Field that
// may follow is deliberately not: its contents are gated on a flags byte whose
// meaning has changed between engine branches, nothing in gvm reads any of it,
// and a parser that guessed there would turn a healthy server into a failed
// readiness probe the day a game shipped a new flag.
type Info struct {
	Protocol    byte
	Name        string
	Map         string
	Folder      string
	Game        string
	AppID       uint16
	Players     uint8
	MaxPlayers  uint8
	Bots        uint8
	ServerType  byte // 'd' dedicated, 'l' listen, 'p' SourceTV proxy
	Environment byte // 'l' linux, 'w' windows, 'm' or 'o' mac
	Private     bool
	VAC         bool
	Version     string
}

// A2S reports a port ready when a Source-family server answers A2S_INFO on it.
//
// This is the probe for games whose logs cannot be trusted. Palworld's manifest
// says it outright: no log marker in that engine is stable across builds, so
// readiness is answered by the protocol instead. Unlike a bare udp_probe, an
// A2S reply is a POSITIVE acknowledgement -- the game's own query thread built
// it -- which is the difference between "a socket is bound" and "the server is
// answering players".
//
// The challenge round-trip is not optional. Since Valve's 2020 reflection-
// amplification fix a server answers the first A2S_INFO with S2C_CHALLENGE, and
// the request has to be repeated with those four bytes appended. A probe that
// did not follow it would report every modern server as never ready.
type A2S struct {
	// Port is the manifest port NAME, for the description an operator reads.
	Port string

	// Addr is host:port -- the QUERY port, which on Palworld is a different
	// port from the game port and on 7 Days to Die is the same one.
	Addr string

	// Window bounds one request/reply exchange. Zero means DefaultUDPWindow.
	// Each leg of a challenged exchange gets its own window, because a server
	// mid-worldgen answers slowly on both.
	Window time.Duration

	// Timeout bounds the whole attempt including the challenge round-trip.
	// Zero defers to the caller's context.
	Timeout time.Duration

	// MinPlayersOnline requires the server to report at least this many
	// players before it counts as ready. It exists for the adopt path, where
	// "the query port answers" and "the world finished loading" can be
	// minutes apart; zero (the default) asks only for a well-formed reply.
	MinPlayersOnline int

	// Dial is the seam for tests. nil uses a real net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// Last is the most recent successful reply, for the status line and for
	// the player count the dashboard shows without paying RCON's main-thread
	// cost (section 11.9). It is written only by a successful Run.
	Last *Info
}

var _ Probe = (*A2S)(nil)

// Kind implements Probe.
func (p *A2S) Kind() string { return KindA2S }

// Describe implements Probe.
func (p *A2S) Describe() string {
	what := "an A2S_INFO reply from " + p.Addr
	if p.Port != "" {
		what += " (port " + p.Port + ")"
	}
	if p.MinPlayersOnline > 0 {
		what += " reporting at least " + strconv.Itoa(p.MinPlayersOnline) + " players"
	}
	return what
}

// Run implements Probe.
func (p *A2S) Run(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx, p.Timeout)
	defer cancel()

	dial := p.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	// Connected, like the UDP probe: only a connected socket is told about an
	// ICMP unreachable, which is what makes "nothing is listening" fail fast
	// instead of waiting out the window.
	c, err := dial(ctx, "udp", p.Addr)
	if err != nil {
		return fail(KindA2S, p.Addr, "cannot open a udp socket", err)
	}
	defer func() { _ = c.Close() }()

	info, err := a2sExchange(ctx, c, p.Addr, p.Window)
	if err != nil {
		return err
	}
	if p.MinPlayersOnline > 0 && int(info.Players) < p.MinPlayersOnline {
		return fail(KindA2S, p.Addr,
			"only "+strconv.Itoa(int(info.Players))+" players online", nil)
	}
	p.Last = info
	return nil
}

// a2sExchange runs the request, follows at most one challenge, and parses the
// answer.
//
// One challenge and no more. A server that answers a challenged request with
// another challenge is either broken or being used to bounce traffic at us, and
// looping on it would turn a readiness probe into a packet source. Failing
// after one round is correct: the driver retries the whole probe on its own
// interval anyway.
func a2sExchange(ctx context.Context, c net.Conn, addr string, window time.Duration) (*Info, error) {
	var challenge []byte
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := a2sRoundTrip(ctx, c, addr, challenge, window)
		if err != nil {
			return nil, err
		}
		info, next, err := parseA2SResponse(reply)
		switch {
		case err != nil:
			return nil, fail(KindA2S, addr, "malformed reply", err)
		case info != nil:
			return info, nil
		case next != nil && challenge == nil:
			challenge = next
		default:
			// A second challenge, or a challenge to an already-challenged
			// request.
			return nil, fail(KindA2S, addr, "server keeps asking for a challenge", nil)
		}
	}
	return nil, fail(KindA2S, addr, "server keeps asking for a challenge", nil)
}

// a2sRoundTrip writes one request and reads one datagram.
func a2sRoundTrip(ctx context.Context, c net.Conn, addr string, challenge []byte, window time.Duration) ([]byte, error) {
	if window <= 0 {
		window = DefaultUDPWindow
	}
	deadline := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)

	if _, err := c.Write(a2sInfoRequest(challenge)); err != nil {
		return nil, fail(KindA2S, addr, "sending A2S_INFO", err)
	}
	buf := make([]byte, maxA2SDatagram)
	n, err := c.Read(buf)
	if err != nil {
		if isTimeout(err) {
			// No ICMP rejection and no answer. On a game that is still loading
			// its world this is the normal state for minutes, which is why it
			// reads as "not ready yet" and not as an error about the network.
			return nil, fail(KindA2S, addr, "no reply within the window", nil)
		}
		return nil, fail(KindA2S, addr, "rejected", err)
	}
	return buf[:n], nil
}

// a2sInfoRequest builds an A2S_INFO request, with the challenge appended when
// the server has demanded one.
func a2sInfoRequest(challenge []byte) []byte {
	b := make([]byte, 0, 4+1+len(a2sInfoPayload)+len(challenge))
	b = binary.LittleEndian.AppendUint32(b, a2sSimpleHeader)
	b = append(b, a2sInfoRequestByte)
	b = append(b, a2sInfoPayload...)
	return append(b, challenge...)
}

// parseA2SResponse classifies and decodes one datagram.
//
// Exactly one of info and challenge is non-nil when err is nil, so a caller
// cannot forget to handle the challenge case: there is nothing else to return.
func parseA2SResponse(b []byte) (info *Info, challenge []byte, err error) {
	r := &leReader{b: b}
	header, err := r.uint32()
	if err != nil {
		return nil, nil, fmt.Errorf("truncated header: %w", err)
	}
	switch header {
	case a2sSimpleHeader:
	case a2sSplitHeader:
		// Split responses are reassembled by matching request ids across
		// fragments, with a compression variant that only Half-Life 1 era
		// servers emit. A2S_INFO does not need it -- the reply is a few
		// hundred bytes -- so the honest move is to say so rather than to
		// half-implement reassembly that would only ever run on a malformed
		// packet.
		return nil, nil, fmt.Errorf("%w: split response (0xFFFFFFFE) is not reassembled",
			ErrA2SUnsupportedFormat)
	default:
		return nil, nil, fmt.Errorf("%w: header 0x%08x", ErrA2SUnsupportedFormat, header)
	}

	kind, err := r.byte()
	if err != nil {
		return nil, nil, fmt.Errorf("truncated response type: %w", err)
	}
	switch kind {
	case a2sChallengeByte:
		ch, err := r.take(4)
		if err != nil {
			return nil, nil, fmt.Errorf("truncated challenge: %w", err)
		}
		// Copied: take aliases the read buffer, which the next round trip
		// overwrites in place.
		return nil, append([]byte(nil), ch...), nil

	case a2sInfoResponseByte:
		got, err := parseA2SInfoBody(r)
		if err != nil {
			return nil, nil, err
		}
		return got, nil, nil

	case a2sGoldSourceByte:
		// A GoldSource server answered. Its layout starts with an address
		// string and diverges from there. Nothing gvm ships targets that
		// engine, and guessing would report a wrong player count rather than
		// no player count.
		return nil, nil, fmt.Errorf("%w: GoldSource S2A_INFO_DETAILED ('m')", ErrA2SUnsupportedFormat)

	default:
		return nil, nil, fmt.Errorf("%w: response type 0x%02x", ErrA2SUnsupportedFormat, kind)
	}
}

// parseA2SInfoBody decodes S2A_INFO_SRC after the header and type byte.
func parseA2SInfoBody(r *leReader) (*Info, error) {
	var info Info
	var err error

	if info.Protocol, err = r.byte(); err != nil {
		return nil, fmt.Errorf("protocol: %w", err)
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{"name", &info.Name},
		{"map", &info.Map},
		{"folder", &info.Folder},
		{"game", &info.Game},
	} {
		if *f.dst, err = r.cstring(); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	if info.AppID, err = r.uint16(); err != nil {
		return nil, fmt.Errorf("appid: %w", err)
	}
	for _, f := range []struct {
		name string
		dst  *uint8
	}{
		{"players", &info.Players},
		{"max players", &info.MaxPlayers},
		{"bots", &info.Bots},
	} {
		if *f.dst, err = r.byte(); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	if info.ServerType, err = r.byte(); err != nil {
		return nil, fmt.Errorf("server type: %w", err)
	}
	if info.Environment, err = r.byte(); err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	visibility, err := r.byte()
	if err != nil {
		return nil, fmt.Errorf("visibility: %w", err)
	}
	info.Private = visibility != 0
	vac, err := r.byte()
	if err != nil {
		return nil, fmt.Errorf("vac: %w", err)
	}
	info.VAC = vac != 0

	if info.AppID == theShipAppID {
		if _, err := r.take(3); err != nil {
			return nil, fmt.Errorf("the ship fields: %w", err)
		}
	}
	// Version is where a truncated packet stops being an error. Every field
	// above is required for the reply to mean anything; Version is a label,
	// and a server that omitted it is still a server that answered, which is
	// the entire question a readiness probe asks.
	if v, err := r.cstring(); err == nil {
		info.Version = v
	}
	return &info, nil
}

// leReader walks a datagram. It exists so the parser is a pure function over a
// []byte and can be tested against captured packets on any platform, rather
// than being welded to a socket.
type leReader struct {
	b []byte
	i int
}

var errShort = errors.New("datagram ends early")

func (r *leReader) take(n int) ([]byte, error) {
	if n < 0 || r.i+n > len(r.b) {
		return nil, errShort
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out, nil
}

func (r *leReader) byte() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *leReader) uint16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *leReader) uint32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// cstring reads a NUL-terminated string. A missing terminator is an error and
// not a best-effort tail: a truncated packet that yielded a partial server name
// would be indistinguishable from a server whose name really is that prefix,
// and the probe would report ready on half a datagram.
func (r *leReader) cstring() (string, error) {
	for j := r.i; j < len(r.b); j++ {
		if r.b[j] == 0 {
			s := string(r.b[r.i:j])
			r.i = j + 1
			return s, nil
		}
	}
	return "", errShort
}
