package socket

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
)

const (
	consoleGID = 993
	ownerUID   = 5001
	ownerGID   = 5001
)

func testPolicy() Policy {
	return Policy{OwnerUID: ownerUID, ConsoleGIDs: []uint32{consoleGID}}
}

// The four columns of section 11.4's table, plus the two ways a connection can
// have no identity at all.
var (
	idRoot = Identity{Peer: platform.Peer{PID: 100, UID: 0, GID: 0}, Name: "root"}
	// gvm-web: User=gvm, SupplementaryGroups=gvm-console. Its PRIMARY gid is
	// gvm's, so the console tier is reachable only through the resolved set.
	idWeb   = Identity{Peer: platform.Peer{PID: 200, UID: 990, GID: 990}, Groups: []uint32{990, consoleGID}, Name: "gvm"}
	idOwner = Identity{Peer: platform.Peer{PID: 300, UID: ownerUID, GID: ownerGID}, Name: "pzserver"}
	idOther = Identity{Peer: platform.Peer{PID: 400, UID: 1000, GID: 1000}, Name: "steve"}
	// What the kernel reports for a peer in another pid namespace, and what a
	// failed PeerCred leaves in the caller's variable.
	idNamespaced = Identity{Peer: platform.Peer{PID: 0, UID: 0, GID: 0}}
	idUnknown    = Identity{}
)

// TestTheAuthorizationMatrixIsSectionElevenFour transcribes the peer-credential
// table and checks every cell, so D-09 is a test rather than a paragraph: the
// data plane (tail/send/status/caps) is open to uid 0, to group gvm-console
// and to the instance user, while verb and stop are uid 0 only.
func TestTheAuthorizationMatrixIsSectionElevenFour(t *testing.T) {
	// tiers in the order the table's columns run.
	type row struct {
		root, console, owner, other bool
	}
	table := map[console.Op]row{
		console.OpHello:  {root: true, console: true, owner: true},
		console.OpPing:   {root: true, console: true, owner: true},
		console.OpTail:   {root: true, console: true, owner: true},
		console.OpStatus: {root: true, console: true, owner: true},
		console.OpCaps:   {root: true, console: true, owner: true},
		console.OpSend:   {root: true, console: true, owner: true},
		console.OpVerb:   {root: true},
		console.OpStop:   {root: true},
	}

	// Completeness in both directions: an op added to the protocol without a
	// row here fails, and a row naming an op the protocol dropped fails too.
	for _, op := range console.Ops() {
		if _, ok := table[op]; !ok {
			t.Fatalf("op %q has no row in the authorization matrix; section 11.4's table must gain a column before the op ships", op)
		}
	}
	if len(table) != len(console.Ops()) {
		t.Fatalf("the matrix has %d rows for %d ops", len(table), len(console.Ops()))
	}

	p := testPolicy()
	for op, want := range table {
		cells := []struct {
			name string
			id   Identity
			tier Tier
			want bool
		}{
			{"uid 0", idRoot, TierRoot, want.root},
			{"group " + ConsoleGroup, idWeb, TierConsole, want.console},
			{"socket owner", idOwner, TierOwner, want.owner},
			{"anyone else", idOther, TierOther, want.other},
			{"unidentified", idUnknown, TierNone, false},
			{"foreign pid namespace", idNamespaced, TierNone, false},
		}
		for _, c := range cells {
			if got := p.Tier(c.id); got != c.tier {
				t.Errorf("%s: tier is %s, want %s", c.name, got, c.tier)
			}
			d := p.Authorize(c.id, op)
			if d.Allow != c.want {
				t.Errorf("op %q from %s: allow=%v, want %v (reason %q)", op, c.name, d.Allow, c.want, d.Reason)
			}
			if got := c.tier.Allows(op); got != c.want {
				t.Errorf("op %q, tier %s: Allows=%v, want %v", op, c.tier, got, c.want)
			}
			if !d.Allow && d.Code != console.CodeDenied {
				t.Errorf("op %q from %s: refused with code %q, want %q", op, c.name, d.Code, console.CodeDenied)
			}
			if !d.Allow && strings.TrimSpace(d.Reason) == "" {
				t.Errorf("op %q from %s: refused with no reason", op, c.name)
			}
		}
	}
}

// TestALifecycleDenialNamesTheRoute: a refusal an operator reads has to say
// what to do instead, because "denied" on a Stop button is indistinguishable
// from a broken socket.
func TestALifecycleDenialNamesTheRoute(t *testing.T) {
	p := testPolicy()
	for _, op := range []console.Op{console.OpVerb, console.OpStop} {
		d := p.Authorize(idWeb, op)
		if d.Allow {
			t.Fatalf("op %q is not uid-0 only", op)
		}
		if !strings.Contains(d.Reason, "gvmd") {
			t.Errorf("op %q denial does not point at gvmd: %q", op, d.Reason)
		}
	}
	d := p.Authorize(idOther, console.OpTail)
	if !strings.Contains(d.Reason, "1000") || !strings.Contains(d.Reason, ConsoleGroup) {
		t.Errorf("a stranger's denial should name the uid and the group it lacks, got %q", d.Reason)
	}
}

// TestAnUnidentifiedPeerIsRefusedNotDemoted is the rule that a peer-cred
// failure must not fall through to the least privileged tier: a zero Peer is
// exactly what a failed PeerCred leaves behind, and it must be able to do
// nothing at all -- not even say hello.
func TestAnUnidentifiedPeerIsRefusedNotDemoted(t *testing.T) {
	p := testPolicy()
	for _, id := range []Identity{idUnknown, idNamespaced} {
		if p.Tier(id).Serveable() {
			t.Fatalf("peer %s is serveable", id.Peer)
		}
		for _, op := range console.Ops() {
			if d := p.Authorize(id, op); d.Allow {
				t.Errorf("peer %s was allowed %q", id.Peer, op)
			}
		}
	}
	// uid 0 with pid 0 is the trap: it is root by uid and unidentified by the
	// only rule that matters. platform.Peer.Root already encodes that; this
	// asserts the socket uses it rather than re-deriving uid == 0.
	if idNamespaced.Peer.UID != 0 {
		t.Fatal("the namespaced peer must carry uid 0 for this test to mean anything")
	}
	if p.Tier(idNamespaced) == TierRoot {
		t.Fatal("a peer with pid 0 was treated as root")
	}
}

// TestTheConsoleTierComesFromTheGroupNotThePrimaryGid: SO_PEERCRED reports the
// primary gid only, and gvm-web's membership is supplementary. A build that
// compared peer.GID alone would deny gvm-web every op and D-09 would be dead
// on arrival.
func TestTheConsoleTierComesFromTheGroupNotThePrimaryGid(t *testing.T) {
	p := testPolicy()
	if got := p.Tier(idWeb); got != TierConsole {
		t.Fatalf("gvm-web landed in tier %s, want %s", got, TierConsole)
	}
	// A process whose PRIMARY group is gvm-console (a plain `sg gvm-console`)
	// is in the tier too.
	primary := Identity{Peer: platform.Peer{PID: 201, UID: 991, GID: consoleGID}}
	if got := p.Tier(primary); got != TierConsole {
		t.Fatalf("a primary-gid member landed in tier %s, want %s", got, TierConsole)
	}
	// And a uid whose group set could not be resolved keeps whatever its uid
	// alone earns it, which for gvm-web is nothing: the lookup can only grant.
	noGroups := Identity{Peer: idWeb.Peer}
	if got := p.Tier(noGroups); got != TierOther {
		t.Fatalf("an unresolvable uid landed in tier %s, want %s", got, TierOther)
	}
}

// TestGidZeroIsNeverTheConsoleGroup. Group 0 is root's, so accepting it would
// silently widen the console tier to every process whose primary group is
// root -- the exact distinction the tiers exist to draw.
func TestGidZeroIsNeverTheConsoleGroup(t *testing.T) {
	p := Policy{OwnerUID: ownerUID, ConsoleGIDs: []uint32{0}}
	if err := p.Validate(); err != ErrConsoleGIDZero {
		t.Fatalf("Validate returned %v, want ErrConsoleGIDZero", err)
	}
	// Even unvalidated, the tier decision must not honour it.
	rootGroup := Identity{Peer: platform.Peer{PID: 500, UID: 1000, GID: 0}}
	if got := p.Tier(rootGroup); got != TierOther {
		t.Fatalf("gid 0 granted tier %s", got)
	}
	// A zero Policy has no console tier at all, rather than gid 0.
	var zero Policy
	if err := zero.Validate(); err != nil {
		t.Fatalf("the zero policy does not validate: %v", err)
	}
	if got := zero.Tier(rootGroup); got != TierOther {
		t.Fatalf("the zero policy granted tier %s", got)
	}
}

// TestUnknownOpsAreRefusedEvenForRoot: the matrix is a closed set, so an op
// this build does not implement is denied rather than falling through to a
// tier's default answer.
func TestUnknownOpsAreRefusedEvenForRoot(t *testing.T) {
	p := testPolicy()
	d := p.Authorize(idRoot, console.Op("destroy"))
	if d.Allow {
		t.Fatal("root was allowed an op that does not exist")
	}
	if d.Code != console.CodeBadRequest {
		t.Errorf("unknown op refused with code %q, want %q", d.Code, console.CodeBadRequest)
	}
	if TierRoot.Allows(console.Op("")) {
		t.Error("the empty op is not an op")
	}
}

// TestPeerInfoIsDerivedFromTheSameMatrix. The hello event's write/verb/stop
// flags are what a dashboard renders its buttons from; deriving them from
// Allows is what stops an enabled Stop button from disagreeing with the socket
// that refuses it.
func TestPeerInfoIsDerivedFromTheSameMatrix(t *testing.T) {
	cases := []struct {
		tier                Tier
		write, verb, stopOK bool
	}{
		{TierRoot, true, true, true},
		{TierConsole, true, false, false},
		{TierOwner, true, false, false},
		{TierOther, false, false, false},
		{TierNone, false, false, false},
	}
	for _, c := range cases {
		pi := c.tier.PeerInfo(platform.Peer{PID: 7, UID: 11, GID: 12})
		if pi.Write != c.write || pi.Verb != c.verb || pi.Stop != c.stopOK {
			t.Errorf("tier %s: peer info %+v, want write=%v verb=%v stop=%v",
				c.tier, pi, c.write, c.verb, c.stopOK)
		}
		if pi.UID != 11 || pi.GID != 12 || pi.PID != 7 {
			t.Errorf("tier %s: peer info lost the credentials: %+v", c.tier, pi)
		}
	}
}

// TestTheOwnerCannotSignTheLogAsSomebodyElse. Every send is echoed into the
// ring with the resolved actor, so that echo is the record of who typed what.
// The instance user IS the game: a compromised mod writing to its own stdin
// must not be able to attribute the line to an operator.
func TestTheOwnerCannotSignTheLogAsSomebodyElse(t *testing.T) {
	p := testPolicy()
	if got := p.Actor(idOwner, TierOwner, "user:alice@example.com"); got != "unix:pzserver" {
		t.Errorf("owner actor is %q, want unix:pzserver", got)
	}
	// gvmd and gvm-web legitimately speak for somebody else: that is where
	// sessions, RBAC and the audit log live (D-09).
	if got := p.Actor(idRoot, TierRoot, "user:alice@example.com"); got != "user:alice@example.com" {
		t.Errorf("root actor is %q, want the claim", got)
	}
	if got := p.Actor(idWeb, TierConsole, "user:alice@example.com"); got != "user:alice@example.com" {
		t.Errorf("console actor is %q, want the claim", got)
	}
	// Unclaimed, each tier still names itself honestly.
	if got := p.Actor(idRoot, TierRoot, ""); got != "root_cli" {
		t.Errorf("unclaimed root actor is %q, want root_cli", got)
	}
	if got := p.Actor(idWeb, TierConsole, ""); got != "unix:gvm" {
		t.Errorf("unclaimed console actor is %q, want unix:gvm", got)
	}
	nameless := Identity{Peer: platform.Peer{PID: 9, UID: ownerUID, GID: ownerGID}}
	if got := p.Actor(nameless, TierOwner, ""); got != "unix:uid-5001" {
		t.Errorf("nameless owner actor is %q, want unix:uid-5001", got)
	}
}

// TestAnActorClaimCannotForgeExtraLines. The claim ends up inside a console
// line; a control character in it would let a caller manufacture log content
// that no operator typed.
func TestAnActorClaimCannotForgeExtraLines(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"user:alice\nuser:bob",
		"user:alice\r",
		"token:\x00ab12",
		"tab\there",
		strings.Repeat("a", MaxActor+1),
		"\xff\xfe not utf-8",
	}
	for _, s := range bad {
		if got, ok := SanitizeActor(s); ok {
			t.Errorf("SanitizeActor(%q) accepted it as %q", s, got)
		}
	}
	good := map[string]string{
		"user:alice@example.com": "user:alice@example.com",
		"  token:ab12  ":         "token:ab12",
		"root_cli":               "root_cli",
		"user:Ünïcode":           "user:Ünïcode",
		"user:Alice Smith":       "user:Alice Smith",
	}
	for in, want := range good {
		got, ok := SanitizeActor(in)
		if !ok || got != want {
			t.Errorf("SanitizeActor(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	// A rejected claim falls back to the tier's own name rather than being
	// recorded verbatim or dropped.
	p := testPolicy()
	if got := p.Actor(idRoot, TierRoot, "user:alice\nquit"); got != "root_cli" {
		t.Errorf("a forged claim resolved to %q, want root_cli", got)
	}
}

func TestTierStringsAreStable(t *testing.T) {
	want := map[Tier]string{
		TierNone: "unidentified", TierOther: "other", TierOwner: "owner",
		TierConsole: "console", TierRoot: "root",
	}
	for tier, s := range want {
		if got := tier.String(); got != s {
			t.Errorf("Tier(%d).String() = %q, want %q", tier, got, s)
		}
	}
	if got := Tier(99).String(); got != "tier(99)" {
		t.Errorf("an out-of-range tier renders as %q", got)
	}
}
