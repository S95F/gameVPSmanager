package ports

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// Socket states as the kernel writes them in the `st` column of /proc/net/*.
// Only two of the twelve matter here, and they are spelled out rather than
// compared as magic numbers in the middle of the filter.
const (
	// StateEstablished is a connected socket. A TCP one is not a listener; a
	// UDP one still holds its local port and still collides.
	StateEstablished uint8 = 0x01

	// StateListen is the only TCP state that occupies a port for our purposes.
	// A socket in TIME_WAIT or CLOSE is not something a server has to lose a
	// port to, and counting them would make every recently restarted service
	// look like a permanent blocker.
	StateListen uint8 = 0x0A

	// StateClose is what an unconnected bound UDP socket reports. It is not a
	// closed socket: /proc/net/udp has no LISTEN state, so every ordinary UDP
	// server appears here.
	StateClose uint8 = 0x07
)

// The columns a row must have, and where the two that matter sit.
//
// The kernel's header names twelve things but prints ten fields: tx_queue and
// rx_queue share one colon-joined column, and so do tr and tm->when. Counting
// header words instead of fields puts uid and inode one place to the right,
// which parses cleanly against most rows and then attributes every socket to
// the wrong process.
const (
	procNetFields = 10 // sl local rem st tx:rx tr:when retrnsmt uid timeout inode
	colUID        = 7
	colInode      = 9
)

// NetEntry is one row of /proc/net/{tcp,tcp6,udp,udp6}.
type NetEntry struct {
	Proto  Proto
	Local  netip.AddrPort
	Remote netip.AddrPort
	State  uint8
	UID    uint32
	Inode  uint64

	// Line is the 1-based line number the row came from, so a later
	// attribution failure can name the row it could not resolve.
	Line int
}

// Bound reports whether the entry occupies its local port.
//
// The two protocols are asked different questions on purpose. TCP has a real
// LISTEN state and anything else is a connection that will go away, so only
// state 0A counts. UDP is connectionless and has no LISTEN state at all: every
// entry with a local port is a socket somebody bound, whether or not it has a
// peer, and refusing to count the connected ones would let the allocator hand
// out the port a WireGuard endpoint is sitting on.
func (e NetEntry) Bound() bool {
	if e.Proto == TCP {
		return e.State == StateListen && e.Local.Port() != 0
	}
	return e.Local.Port() != 0
}

// Endpoint is the entry's local endpoint.
func (e NetEntry) Endpoint() Endpoint {
	return Endpoint{Proto: e.Proto, Port: int(e.Local.Port())}
}

// ParseProcNet decodes one of the kernel's socket tables.
//
// It takes an io.Reader and not a path so that the interesting half -- the
// hex address decoding, the state filter, the IPv4-mapped case -- is testable
// on a machine that has no /proc at all. The four fixtures under testdata are
// what it is tested against.
//
// A malformed row is an error rather than a skipped line. Skipping is the
// dangerous direction: a row this parser did not understand is a port somebody
// is listening on, and quietly dropping it means the allocator hands that port
// to a server that will then fail to bind.
func ParseProcNet(r io.Reader, proto Proto) ([]NetEntry, error) {
	if proto != TCP && proto != UDP {
		return nil, fmt.Errorf("ports: %q is not a protocol", proto)
	}
	sc := bufio.NewScanner(r)
	// A /proc/net line is well under 512 bytes; the larger buffer is for a
	// truncated read or a fixture with long trailing columns.
	sc.Buffer(make([]byte, 0, 8192), 64*1024)

	var out []NetEntry
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		// The header is the only line whose first column is not "N:".
		if strings.HasPrefix(text, "sl") || strings.Contains(text, "local_address") {
			continue
		}
		e, err := parseProcNetLine(text, proto, line)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ports: reading %s socket table: %w", proto, err)
	}
	return out, nil
}

func parseProcNetLine(text string, proto Proto, line int) (NetEntry, error) {
	f := strings.Fields(text)
	if len(f) < procNetFields {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: want at least %d columns, got %d: %q",
			proto, line, procNetFields, len(f), text)
	}
	e := NetEntry{Proto: proto, Line: line}

	var err error
	if e.Local, err = parseHexAddrPort(f[1]); err != nil {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: local address: %w", proto, line, err)
	}
	if e.Remote, err = parseHexAddrPort(f[2]); err != nil {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: remote address: %w", proto, line, err)
	}
	st, err := strconv.ParseUint(f[3], 16, 8)
	if err != nil {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: state %q: %w", proto, line, f[3], err)
	}
	e.State = uint8(st)

	uid, err := strconv.ParseUint(f[colUID], 10, 32)
	if err != nil {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: uid %q: %w", proto, line, f[colUID], err)
	}
	e.UID = uint32(uid)

	inode, err := strconv.ParseUint(f[colInode], 10, 64)
	if err != nil {
		return NetEntry{}, fmt.Errorf("ports: %s socket table line %d: inode %q: %w", proto, line, f[colInode], err)
	}
	e.Inode = inode
	return e, nil
}

// parseHexAddrPort decodes the kernel's "HEXADDR:HEXPORT" spelling.
//
// The address is printed as 32-bit words in host byte order, which on every
// machine gvm targets means each four-byte group arrives reversed. Decoding it
// as a plain big-endian hex string is the classic bug here: 127.0.0.1 comes out
// as 1.0.0.127, the loopback check silently stops working, and a probe reports
// a service as publicly bound when it is not.
func parseHexAddrPort(s string) (netip.AddrPort, error) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return netip.AddrPort{}, fmt.Errorf("%q has no port separator", s)
	}
	rawAddr, rawPort := s[:i], s[i+1:]

	port, err := strconv.ParseUint(rawPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("port %q: %w", rawPort, err)
	}
	if len(rawAddr) != 8 && len(rawAddr) != 32 {
		return netip.AddrPort{}, fmt.Errorf("address %q is %d hex digits, want 8 (IPv4) or 32 (IPv6)", rawAddr, len(rawAddr))
	}
	b, err := hex.DecodeString(rawAddr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("address %q: %w", rawAddr, err)
	}
	for w := 0; w+4 <= len(b); w += 4 {
		b[w], b[w+1], b[w+2], b[w+3] = b[w+3], b[w+2], b[w+1], b[w]
	}

	var addr netip.Addr
	if len(b) == 4 {
		addr = netip.AddrFrom4([4]byte(b))
	} else {
		// Unmap so that an IPv4 socket showing up in tcp6 as ::ffff:127.0.0.1
		// prints as 127.0.0.1. The operator reading a conflict report should
		// not have to know which table the row came out of.
		addr = netip.AddrFrom16([16]byte(b)).Unmap()
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}
