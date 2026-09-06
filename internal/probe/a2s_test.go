package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// listenUDP binds a loopback datagram socket on an ephemeral port. Loopback
// and not a fake conn: the probe's job is to talk to a real socket, and the
// one thing a fake Dial cannot exercise is the framing going out on the wire.
func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

// The A2S_INFO app id field is 16 bits wide, so a Steam app id above 65535 --
// which most modern ones are, Project Zomboid's dedicated server included --
// arrives TRUNCATED. That is the protocol, not a bug in the parser, and these
// constants are the truncated values a real server sends.
const (
	pzQueryAppID      = uint16(380870 & 0xFFFF)
	valheimQueryAppID = uint16(896660 & 0xFFFF)
)

// infoPacket builds a well-formed S2A_INFO_SRC reply. It is a builder rather
// than a captured hex blob so a test can say "the same packet but truncated at
// byte 14" without hand-editing a literal.
func infoPacket(name, mapName string, appID uint16, players, max byte, version string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, a2sSimpleHeader)
	b = append(b, a2sInfoResponseByte)
	b = append(b, 17) // protocol
	for _, s := range []string{name, mapName, "pzserver", "Project Zomboid"} {
		b = append(b, s...)
		b = append(b, 0)
	}
	b = binary.LittleEndian.AppendUint16(b, appID)
	b = append(b, players, max, 0) // players, max players, bots
	b = append(b, 'd', 'l')        // dedicated, linux
	b = append(b, 0, 1)            // public, VAC secured
	if appID == theShipAppID {
		b = append(b, 0, 0, 0)
	}
	b = append(b, version...)
	return append(b, 0)
}

func challengePacket(challenge [4]byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, a2sSimpleHeader)
	b = append(b, a2sChallengeByte)
	return append(b, challenge[:]...)
}

// fakeA2S is a UDP server on loopback that answers with whatever the test
// hands it, in order, and records every request it saw.
type fakeA2S struct {
	addr string

	mu       sync.Mutex
	requests [][]byte
}

func (f *fakeA2S) seen() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.requests))
	copy(out, f.requests)
	return out
}

// startFakeA2S replies with replies[i] to the i'th request, and stops
// answering once the script runs out -- which is how "the server went quiet"
// is tested without unplugging anything.
func startFakeA2S(t *testing.T, replies func(req []byte, n int) []byte) *fakeA2S {
	t.Helper()
	pc := listenUDP(t)
	f := &fakeA2S{addr: pc.LocalAddr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for n := 0; ; n++ {
			read, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := append([]byte(nil), buf[:read]...)
			f.mu.Lock()
			f.requests = append(f.requests, req)
			f.mu.Unlock()
			if reply := replies(req, n); reply != nil {
				if _, err := pc.WriteTo(reply, from); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		<-done
	})
	return f
}

// The challenge round-trip is the whole reason this probe is not three lines.
// Since Valve's 2020 amplification fix a server answers the first A2S_INFO
// with S2C_CHALLENGE, so a probe that did not repeat the request with those
// four bytes appended would report every modern server as never ready.
func TestA2SFollowsTheChallenge(t *testing.T) {
	challenge := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	f := startFakeA2S(t, func(req []byte, n int) []byte {
		if n == 0 {
			return challengePacket(challenge)
		}
		return infoPacket("pz-main", "Muldraugh, KY", pzQueryAppID, 3, 16, "41.78.16")
	})

	p := &A2S{Addr: f.addr, Port: "query", Window: 2 * time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.Last == nil {
		t.Fatal("Last is nil after a successful run")
	}
	if p.Last.Name != "pz-main" || p.Last.Map != "Muldraugh, KY" {
		t.Errorf("parsed %+v, want name pz-main and map Muldraugh, KY", p.Last)
	}
	if p.Last.Players != 3 || p.Last.MaxPlayers != 16 {
		t.Errorf("players %d/%d, want 3/16", p.Last.Players, p.Last.MaxPlayers)
	}
	if p.Last.Version != "41.78.16" {
		t.Errorf("version %q, want 41.78.16", p.Last.Version)
	}

	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2 (the challenge round-trip)", len(reqs))
	}
	if len(reqs[0]) != 4+1+len(a2sInfoPayload) {
		t.Errorf("first request is %d bytes, want a bare A2S_INFO", len(reqs[0]))
	}
	if got := reqs[1][len(reqs[1])-4:]; string(got) != string(challenge[:]) {
		t.Errorf("second request ends with %x, want the challenge %x", got, challenge)
	}
}

// A server that answers straight away (older builds, and some proxies) must
// not be made to do a pointless second round trip.
func TestA2SAcceptsAnUnchallengedReply(t *testing.T) {
	f := startFakeA2S(t, func([]byte, int) []byte {
		return infoPacket("valheim", "dedicated", valheimQueryAppID, 0, 10, "0.217.46")
	})
	p := &A2S{Addr: f.addr, Window: time.Second}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(f.seen()); got != 1 {
		t.Fatalf("server saw %d requests, want 1", got)
	}
}

// A server that answers every request with a challenge is either broken or
// being used to bounce packets somewhere. The probe must give up, not loop.
func TestA2SGivesUpOnAChallengeLoop(t *testing.T) {
	f := startFakeA2S(t, func([]byte, int) []byte {
		return challengePacket([4]byte{1, 2, 3, 4})
	})
	p := &A2S{Addr: f.addr, Window: 500 * time.Millisecond}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against a server that only ever challenges")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if got := len(f.seen()); got != 2 {
		t.Fatalf("server saw %d requests, want exactly 2: one challenge is followed, "+
			"a second is a loop", got)
	}
}

// Silence is a failure here, unlike the bare udp_probe. A2S has a positive
// acknowledgement, so the absence of one means the game's query thread is not
// answering yet -- which during worldgen is the normal state for minutes.
func TestA2SFailsOnSilence(t *testing.T) {
	f := startFakeA2S(t, func([]byte, int) []byte { return nil })
	p := &A2S{Addr: f.addr, Window: 150 * time.Millisecond}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against a silent server")
	}
	if !strings.Contains(err.Error(), "no reply") {
		t.Errorf("error = %q, want it to say no reply arrived", err)
	}
}

func TestA2SMinPlayersOnline(t *testing.T) {
	f := startFakeA2S(t, func([]byte, int) []byte {
		return infoPacket("pz-main", "Muldraugh, KY", pzQueryAppID, 0, 16, "41.78.16")
	})
	p := &A2S{Addr: f.addr, Window: time.Second, MinPlayersOnline: 1}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with 0 players and MinPlayersOnline 1")
	}
	if p.Last != nil {
		t.Error("Last was written by a failing run; it must only record a satisfied probe")
	}
}

// The parser is a pure function over a []byte precisely so the failure modes
// that cannot be produced on demand from a socket -- a split response, a
// GoldSource server, a packet cut off mid-string -- are all testable anywhere.
func TestParseA2SResponse(t *testing.T) {
	good := infoPacket("srv", "map", pzQueryAppID, 2, 8, "41.78.16")

	for _, tc := range []struct {
		name    string
		in      []byte
		wantErr error
		check   func(t *testing.T, info *Info, challenge []byte)
	}{
		{
			name: "a full info reply",
			in:   good,
			check: func(t *testing.T, info *Info, challenge []byte) {
				if info == nil {
					t.Fatal("no info")
				}
				if challenge != nil {
					t.Error("a challenge was returned alongside an info reply")
				}
				if info.AppID != pzQueryAppID || info.Players != 2 || info.MaxPlayers != 8 {
					t.Errorf("got %+v", info)
				}
				if info.Private || !info.VAC {
					t.Errorf("visibility/VAC decoded as %v/%v, want public and secured",
						info.Private, info.VAC)
				}
			},
		},
		{
			name: "a challenge",
			in:   challengePacket([4]byte{9, 8, 7, 6}),
			check: func(t *testing.T, info *Info, challenge []byte) {
				if info != nil {
					t.Error("info returned for a challenge packet")
				}
				if string(challenge) != string([]byte{9, 8, 7, 6}) {
					t.Errorf("challenge = %x", challenge)
				}
			},
		},
		{
			name:    "a split response is refused rather than half-parsed",
			in:      append(binary.LittleEndian.AppendUint32(nil, a2sSplitHeader), 'I', 0),
			wantErr: ErrA2SUnsupportedFormat,
		},
		{
			name: "a GoldSource reply is named, not guessed at",
			in: append(binary.LittleEndian.AppendUint32(nil, a2sSimpleHeader),
				a2sGoldSourceByte, 0),
			wantErr: ErrA2SUnsupportedFormat,
		},
		{
			name: "an unknown response type",
			in: append(binary.LittleEndian.AppendUint32(nil, a2sSimpleHeader),
				'Z', 0),
			wantErr: ErrA2SUnsupportedFormat,
		},
		{
			name:    "a garbage header",
			in:      []byte{1, 2, 3, 4, 'I'},
			wantErr: ErrA2SUnsupportedFormat,
		},
		{
			name:    "an empty datagram",
			in:      nil,
			wantErr: errShort,
		},
		{
			name:    "cut off inside the server name",
			in:      good[:8],
			wantErr: errShort,
		},
		{
			// The last 12 bytes are the version string, its NUL, and the
			// three flag bytes before it. Losing a required flag is a
			// truncated packet; losing only the version is not (see
			// TestParseA2SToleratesAMissingVersion).
			name:    "cut off inside the trailing flags",
			in:      good[:len(good)-12],
			wantErr: errShort,
		},
		{
			name:    "a truncated challenge",
			in:      challengePacket([4]byte{1, 2, 3, 4})[:7],
			wantErr: errShort,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, challenge, err := parseA2SResponse(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, info, challenge)
		})
	}
}

// Valve documented The Ship's three extra bytes rather than versioning the
// packet, so every parser that reads Version on any other game has to know
// about a 2007 co-op murder game. Getting it wrong shifts Version by three
// bytes and reports a version string that is mostly garbage.
func TestParseA2STheShipOffset(t *testing.T) {
	info, _, err := parseA2SResponse(infoPacket("ship", "sea", theShipAppID, 1, 8, "1.0.0.4"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.0.0.4" {
		t.Fatalf("version %q, want 1.0.0.4: the three Ship-only bytes were not skipped",
			info.Version)
	}
}

// Version is the one field whose absence is tolerated: every field above it is
// required for the reply to mean anything, but a server that answered without
// a version string is still a server that answered, which is the entire
// question a readiness probe asks.
func TestParseA2SToleratesAMissingVersion(t *testing.T) {
	full := infoPacket("srv", "map", pzQueryAppID, 2, 8, "41.78.16")
	// Drop the trailing version string and its NUL.
	cut := full[:len(full)-len("41.78.16")-1]
	info, _, err := parseA2SResponse(cut)
	if err != nil {
		t.Fatalf("a reply without a version string was rejected: %v", err)
	}
	if info.Version != "" || info.Players != 2 {
		t.Fatalf("got %+v", info)
	}
}

func TestA2SInfoRequestShape(t *testing.T) {
	bare := a2sInfoRequest(nil)
	if got := binary.LittleEndian.Uint32(bare[:4]); got != a2sSimpleHeader {
		t.Errorf("header = %08x", got)
	}
	if bare[4] != a2sInfoRequestByte {
		t.Errorf("request byte = %q, want 'T'", bare[4])
	}
	// The trailing NUL is part of the payload; servers reject a request
	// without it.
	if bare[len(bare)-1] != 0 {
		t.Error("the payload is not NUL terminated")
	}
	withChallenge := a2sInfoRequest([]byte{1, 2, 3, 4})
	if len(withChallenge) != len(bare)+4 {
		t.Errorf("challenged request is %d bytes, want %d", len(withChallenge), len(bare)+4)
	}
}

func TestA2SDescribe(t *testing.T) {
	p := &A2S{Addr: "127.0.0.1:27016", Port: "query", MinPlayersOnline: 2}
	got := p.Describe()
	for _, want := range []string{"A2S_INFO", "127.0.0.1:27016", "query", "2 players"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}
