package socket

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

// ConsoleGroup is the unix group behind D-10. Membership in it *is* the
// console tier: gvm-web runs as User=gvm with SupplementaryGroups=gvm-console
// and reaches every instance's socket that way, while an instance user is a
// member of nothing but its own group -- so instance A's compromised mod
// cannot drive instance B's console.
const ConsoleGroup = "gvm-console"

// MaxActor bounds a claimed actor string. The claim is recorded, never
// trusted: SO_PEERCRED decides what a connection may do, and this string only
// decides what the console log says about it. It still needs a bound, because
// it ends up in every echoed line in the ring.
const MaxActor = 128

// Tier is one column of section 11.4's peer-credential table.
//
// The numeric order is for logging and for the String below, not for
// comparisons: TierOwner and TierConsole hold exactly the same rights by
// different routes, and every question about what a tier may do is answered by
// Allows, which is the transcription of the table itself.
type Tier uint8

const (
	// TierNone is a connection the kernel could not name. It is refused
	// outright rather than treated as the least privileged caller: a peer-cred
	// failure means the identity is unknown, and "unknown" is not a tier.
	TierNone Tier = iota
	// TierOther is an identified uid in none of the three tiers. The runtime
	// directory is 2750, so it should be unreachable; refusing it anyway is
	// what makes that mode a second lock rather than the only one.
	TierOther
	// TierOwner is the instance user -- the game itself. It may write to its
	// own console because it can already write to /proc/self/fd/0, and that is
	// what makes screen-compat work.
	TierOwner
	// TierConsole is a member of gvm-console: gvm-web, on the D-09 data plane.
	TierConsole
	// TierRoot is uid 0: gvmd or a root CLI, the only tier permitted the
	// lifecycle ops.
	TierRoot
)

var tierNames = [...]string{"unidentified", "other", "owner", "console", "root"}

func (t Tier) String() string {
	if int(t) >= len(tierNames) {
		return "tier(" + strconv.Itoa(int(t)) + ")"
	}
	return tierNames[t]
}

// knownOps is the protocol's closed op set, materialised so that the matrix
// refuses an op this build does not know instead of letting it fall through to
// a tier's default answer. An op added to the protocol and not to this file is
// denied, which is the safe direction to be wrong in.
var knownOps = func() map[console.Op]bool {
	ops := console.Ops()
	m := make(map[console.Op]bool, len(ops))
	for _, op := range ops {
		m[op] = true
	}
	return m
}()

// Allows is section 11.4's table, in code and in one place.
//
// The lifecycle pair comes from console.Op.Lifecycle rather than a second list
// of op names here, so the protocol and the authorization matrix cannot
// disagree about which ops are lifecycle actions.
func (t Tier) Allows(op console.Op) bool {
	if !knownOps[op] {
		return false
	}
	switch t {
	case TierRoot:
		return true
	case TierConsole, TierOwner:
		// The data plane, and only the data plane. verb and stop are routed
		// through gvmd, where they are RBAC-gated and audited a second time.
		return !op.Lifecycle()
	default:
		return false
	}
}

// Serveable reports whether a connection in this tier is worth reading frames
// from at all. A tier that cannot even say hello is refused on accept, so a
// caller with no business here never gets to send a frame.
func (t Tier) Serveable() bool { return t.Allows(console.OpHello) }

// PeerInfo builds the hello event's peer block from the same matrix that
// enforces it. Deriving the flags rather than restating them is what stops a
// dashboard's enabled Stop button from disagreeing with the socket that
// refuses it.
func (t Tier) PeerInfo(p platform.Peer) console.PeerInfo {
	return console.PeerInfo{
		UID:   p.UID,
		GID:   p.GID,
		PID:   p.PID,
		Write: t.Allows(console.OpSend),
		Verb:  t.Allows(console.OpVerb),
		Stop:  t.Allows(console.OpStop),
	}
}

// Identity is everything the server knows about a connection before it reads a
// single frame: the credentials the kernel stamped on the socket at connect
// time, plus the group set and account name resolved for that uid.
//
// Groups and Name are best-effort. A failed lookup leaves them empty, which
// can only cost a caller the console tier -- it can never grant one -- so the
// resolver is allowed to fail without failing the connection.
type Identity struct {
	Peer   platform.Peer
	Groups []uint32
	Name   string
}

// Policy is the host's side of the decision: who owns this instance and which
// gids are the console group. It holds no sockets and no lookups, so the whole
// authorization matrix is table-testable off Linux.
type Policy struct {
	// OwnerUID is the uid the runner runs as, i.e. the instance user.
	OwnerUID uint32
	// ConsoleGIDs are the gids of ConsoleGroup. It is a slice and not a single
	// gid because an empty policy must mean "no console tier on this host"
	// rather than "gid 0", and because a host may carry the group twice during
	// a migration.
	ConsoleGIDs []uint32
}

// ErrConsoleGIDZero rejects gid 0 as the console group. Group 0 is root's, and
// accepting it would collapse the console tier into "any process whose primary
// group is root" -- the exact distinction D-09 depends on.
var ErrConsoleGIDZero = fmt.Errorf("socket: gid 0 cannot be the %s group", ConsoleGroup)

// Validate rejects a policy that would grant more than it names.
func (p Policy) Validate() error {
	for _, gid := range p.ConsoleGIDs {
		if gid == 0 {
			return ErrConsoleGIDZero
		}
	}
	return nil
}

// Tier places a connection in one of the four columns.
//
// Root is tested first so that a uid-0 caller is root even if it also happens
// to sit in the console group, and the console group is tested before the
// owner so that the tier reported to a client names the route it actually
// arrived by. Both grants below root are the same rights either way.
func (p Policy) Tier(id Identity) Tier {
	switch {
	case !id.Peer.Identified():
		return TierNone
	case id.Peer.Root():
		return TierRoot
	case p.inConsoleGroup(id):
		return TierConsole
	case id.Peer.UID == p.OwnerUID:
		return TierOwner
	default:
		return TierOther
	}
}

// inConsoleGroup checks both the primary gid the kernel reported and the
// resolved supplementary set, because SO_PEERCRED reports only the primary one
// and gvm-web's membership is supplementary.
func (p Policy) inConsoleGroup(id Identity) bool {
	for _, want := range p.ConsoleGIDs {
		if want == 0 {
			// A policy that failed Validate must not grant anything.
			continue
		}
		if id.Peer.GID == want {
			return true
		}
		for _, got := range id.Groups {
			if got == want {
				return true
			}
		}
	}
	return false
}

// Decision is one authorization answer: the tier, whether the op is permitted,
// and -- when it is not -- the code and the sentence to put in the error event.
type Decision struct {
	Tier   Tier
	Allow  bool
	Code   string
	Reason string
}

// Authorize answers "may this connection issue this op", which is the whole of
// D-09 in one pure function.
func (p Policy) Authorize(id Identity, op console.Op) Decision {
	t := p.Tier(id)
	if !knownOps[op] {
		return Decision{Tier: t, Code: console.CodeBadRequest,
			Reason: fmt.Sprintf("unknown op %q", string(op))}
	}
	if t.Allows(op) {
		return Decision{Tier: t, Allow: true}
	}
	d := Decision{Tier: t, Code: console.CodeDenied}
	switch t {
	case TierNone:
		d.Reason = "the kernel reported no identity for this connection"
	case TierOther:
		d.Reason = fmt.Sprintf("uid %d is neither root, a member of %s, nor the instance user",
			id.Peer.UID, ConsoleGroup)
	default:
		d.Reason = fmt.Sprintf("op %q is uid 0 only; route it through gvmd (D-09)", string(op))
	}
	return d
}

// Actor resolves the string recorded against an injected line or a verb.
//
// A claim is honoured only for the two tiers that legitimately speak for
// somebody else: gvmd and gvm-web, which is where sessions, RBAC and the audit
// log actually live (D-09). The owner tier's claim is discarded, because the
// instance user *is* the game: a mod writing to its own stdin must not be able
// to sign the console log as "user:alice".
func (p Policy) Actor(id Identity, t Tier, claimed string) string {
	if t == TierRoot || t == TierConsole {
		if a, ok := SanitizeActor(claimed); ok {
			return a
		}
	}
	if t == TierRoot {
		return "root_cli"
	}
	if id.Name != "" {
		return "unix:" + id.Name
	}
	return "unix:uid-" + strconv.FormatUint(uint64(id.Peer.UID), 10)
}

// SanitizeActor accepts a claimed actor string only if it can be written into
// a console line without changing what that line means: valid UTF-8, bounded,
// and free of the control characters that would let an attributed echo forge
// extra lines in the log it is recorded in.
func SanitizeActor(s string) (string, bool) {
	// The scan runs BEFORE the trim, deliberately: TrimSpace strips \r and \n,
	// so trimming first would quietly accept "user:alice\r" as "user:alice"
	// and record a claim nobody made.
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxActor || !utf8.ValidString(s) {
		return "", false
	}
	return s, true
}
