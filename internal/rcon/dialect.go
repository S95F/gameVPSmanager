package rcon

// Dialect selects the per-game quirk set. Every server below speaks the same
// wire format; they disagree about how a client is supposed to know that a
// multi-packet response has finished, and about what junk they interleave.
type Dialect string

const (
	// DialectSource is Valve's SRCDS behaviour and the correct default for
	// most SteamCMD games. Responses fragment at 4096 bytes with no end
	// marker, so the client sends an empty SERVERDATA_RESPONSE_VALUE after
	// the command; the server answers strictly in order, mirrors it back
	// empty, and then emits one malformed packet that must be swallowed.
	DialectSource Dialect = "source"

	// DialectMinecraft is vanilla/Paper/Fabric/Forge Minecraft. It rejects a
	// client-sent SERVERDATA_RESPONSE_VALUE with the body "Unknown request 0",
	// so the Valve terminator trick actively breaks. A second EXECCOMMAND
	// with its own id is used as the terminator instead. Requests are also
	// capped far lower than Source allows.
	DialectMinecraft Dialect = "minecraft"

	// DialectRust is Source plus two Rust-specific behaviours: unsolicited
	// type-4 broadcast packets that arrive at any time, and replies to "say"
	// that come back with id -1 instead of the request id.
	DialectRust Dialect = "rust"

	// DialectSimple is the conservative fallback for a server whose
	// terminator support is unknown, including Project Zomboid. No probe is
	// sent at all; the client reads response packets until the server goes
	// idle. Costs one idle interval per command, and cannot distinguish "the
	// response is finished" from "the server is being slow", so only use it
	// where the trick is known to misbehave.
	DialectSimple Dialect = "simple"
)

type terminatorKind int

const (
	// termMirror sends an empty RESPONSE_VALUE and waits for it to be echoed.
	termMirror terminatorKind = iota
	// termSecondCommand sends a harmless second EXECCOMMAND and waits for its ack.
	termSecondCommand
	// termIdle sends no probe and stops when the server stops talking.
	termIdle
)

type dialectSpec struct {
	terminator terminatorKind

	// maxRequestBody is the largest command body the server will accept.
	// Source allows a 4096-byte body; Minecraft's client->server ceiling is
	// a 1460-byte packet, which leaves 1446 for the body.
	maxRequestBody int

	// discardTypes are packet types that arrive unsolicited and carry no
	// response data.
	discardTypes []PacketType

	// remapNegativeID treats a response with id -1 as belonging to the
	// in-flight command rather than as an error. Only Rust needs this, and
	// only outside the auth handshake, where -1 genuinely means failure.
	remapNegativeID bool

	// trailingJunk is the number of malformed packets to swallow after the
	// terminator is seen.
	trailingJunk int
}

func specFor(d Dialect) dialectSpec {
	switch d {
	case DialectMinecraft:
		return dialectSpec{
			terminator:     termSecondCommand,
			maxRequestBody: 1446,
		}
	case DialectRust:
		return dialectSpec{
			terminator:      termMirror,
			maxRequestBody:  maxResponseBody,
			discardTypes:    []PacketType{TypeRustMessage},
			remapNegativeID: true,
			trailingJunk:    1,
		}
	case DialectSimple:
		return dialectSpec{
			terminator:     termIdle,
			maxRequestBody: maxResponseBody,
		}
	default: // DialectSource
		return dialectSpec{
			terminator:     termMirror,
			maxRequestBody: maxResponseBody,
			trailingJunk:   1,
		}
	}
}

func (s dialectSpec) shouldDiscard(t PacketType) bool {
	for _, d := range s.discardTypes {
		if d == t {
			return true
		}
	}
	return false
}
