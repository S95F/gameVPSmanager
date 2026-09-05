package rcon

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

// PacketType is the RCON packet type field.
//
// Note that 2 is overloaded: client->server it means EXECCOMMAND, and
// server->client it means AUTH_RESPONSE. Only the direction disambiguates
// them, which is why there is no single constant for the value.
type PacketType int32

const (
	TypeResponseValue PacketType = 0 // S->C command output; C->S the Valve terminator probe
	TypeExecCommand   PacketType = 2 // C->S run a command
	TypeAuthResponse  PacketType = 2 // S->C result of an auth attempt
	TypeAuth          PacketType = 3 // C->S authenticate
	TypeRustMessage   PacketType = 4 // Rust-only chat/log broadcast; must be discarded
)

const (
	// A packet is: int32 size | int32 id | int32 type | body | 0x00 | 0x00.
	// The size field counts everything after itself, never itself.
	headerSize    = 8 // id + type
	paddingSize   = 2 // body terminator + packet terminator
	minPacketSize = headerSize + paddingSize

	// Valve caps a body at 4096, so a whole packet at 4106. Minecraft is
	// happy to send 4096 of body too but its framing tops out at 4110, so
	// read up to that and reject anything larger as a desync.
	maxResponseBody = 4096
	maxPacketSize   = 4110
)

// authFailureID is the id a server returns to reject an auth attempt.
const authFailureID int32 = -1

type packet struct {
	ID   int32
	Type PacketType
	Body []byte
}

func (p packet) encode() ([]byte, error) {
	size := headerSize + len(p.Body) + paddingSize
	if size > maxPacketSize {
		return nil, fmt.Errorf("rcon: outgoing packet is %d bytes, over the %d limit", size, maxPacketSize)
	}
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(p.ID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(p.Type))
	copy(buf[12:], p.Body)
	// The final two bytes stay zero: the body terminator and the packet
	// terminator. Servers reject a packet that omits them.
	return buf, nil
}

// readPacket reads one framed packet. It is deliberately tolerant of the
// malformed trailing packet SRCDS emits after the multi-packet terminator,
// which declares a size that does not match its own padding.
func readPacket(r *bufio.Reader) (packet, error) {
	var sizeBuf [4]byte
	if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
		return packet{}, err
	}
	size := int32(binary.LittleEndian.Uint32(sizeBuf[:]))
	if size < minPacketSize {
		return packet{}, fmt.Errorf("rcon: framing error, packet declares %d bytes (minimum %d)", size, minPacketSize)
	}
	if size > maxPacketSize {
		return packet{}, fmt.Errorf("rcon: framing error, packet declares %d bytes (maximum %d)", size, maxPacketSize)
	}

	rest := make([]byte, size)
	if _, err := io.ReadFull(r, rest); err != nil {
		return packet{}, err
	}

	p := packet{
		ID:   int32(binary.LittleEndian.Uint32(rest[0:4])),
		Type: PacketType(int32(binary.LittleEndian.Uint32(rest[4:8]))),
	}
	// Spec says the last two bytes are terminators. Slice them off when they
	// are there; a malformed packet that is too short keeps whatever it has.
	if len(rest) >= headerSize+paddingSize {
		p.Body = rest[headerSize : len(rest)-paddingSize]
	} else {
		p.Body = rest[headerSize:]
	}
	return p, nil
}

// decodeText turns a response body into a Go string.
//
// Source servers send UTF-8. Minecraft sends ISO-8859-1 and routinely
// includes the section sign (0xA7) that prefixes its colour codes, which is
// not valid UTF-8 on its own. Decoding those bytes as UTF-8 would replace
// them with U+FFFD and corrupt the text, so fall back to Latin-1, where every
// byte maps to the code point of the same value and nothing is ever lost.
func decodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}
