package ports

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// The fixtures are byte-for-byte the shape a Linux kernel writes, so this whole
// file runs on the development machine, which has no /proc at all. That is the
// point: the address decoding and the state filter are where the bugs live, and
// a test that only runs on the deployment host is a test nobody runs.

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func parseFixture(t *testing.T, name string, proto Proto) []NetEntry {
	t.Helper()
	es, err := ParseProcNet(fixture(t, name), proto)
	if err != nil {
		t.Fatalf("ParseProcNet(%s): %v", name, err)
	}
	return es
}

func TestTCPCountsOnlyStateListen(t *testing.T) {
	es := parseFixture(t, "proc_net_tcp", TCP)
	if len(es) != 6 {
		t.Fatalf("parsed %d rows, want 6 (the header must not become a row)", len(es))
	}

	// Every row in the fixture, and whether it occupies its port. The two
	// non-listening rows are an ESTABLISHED connection on 9100 and a TIME_WAIT
	// on 3000: a port in TIME_WAIT is not a port a server has to lose, and
	// counting it would make every recently restarted service look like a
	// permanent blocker.
	want := map[int]bool{
		22:    true,
		27015: true,
		8081:  true,
		9100:  false,
		3000:  false,
		53:    true,
	}
	got := map[int]bool{}
	for _, e := range es {
		got[int(e.Local.Port())] = e.Bound()
	}
	for port, bound := range want {
		if got[port] != bound {
			t.Errorf("tcp/%d bound = %v, want %v", port, got[port], bound)
		}
	}

	var boundPorts []int
	for _, e := range es {
		if e.Bound() {
			boundPorts = append(boundPorts, int(e.Local.Port()))
		}
	}
	if len(boundPorts) != 4 {
		t.Errorf("bound tcp ports = %v, want exactly the four listeners", boundPorts)
	}
}

func TestUDPCountsEveryBoundEntry(t *testing.T) {
	es := parseFixture(t, "proc_net_udp", UDP)
	if len(es) != 6 {
		t.Fatalf("parsed %d rows, want 6", len(es))
	}

	// UDP has no LISTEN state. State 07 is what an unconnected bound socket
	// reports and state 01 is a connected one -- a WireGuard endpoint, say --
	// and both hold the port. The only row that does not is the one with no
	// local port at all.
	bound := map[int]bool{}
	for _, e := range es {
		bound[int(e.Local.Port())] = e.Bound()
	}
	for _, port := range []int{68, 123, 16261, 16262, 51820} {
		if !bound[port] {
			t.Errorf("udp/%d is not counted; every bound UDP entry occupies its port", port)
		}
	}
	if bound[0] {
		t.Error("an entry with local port 0 is not bound to anything")
	}

	// The connected one specifically: this is the row a "count only state 07"
	// filter would drop.
	var found bool
	for _, e := range es {
		if e.Local.Port() == 51820 {
			found = true
			if e.State != StateEstablished {
				t.Errorf("udp/51820 state = %#x, want the connected state", e.State)
			}
			if !e.Bound() {
				t.Error("a connected UDP socket still holds its local port")
			}
		}
	}
	if !found {
		t.Fatal("the connected UDP row vanished from the fixture")
	}
}

func TestHexAddressesDecodeAsHostOrderWords(t *testing.T) {
	// 0100007F is 127.0.0.1 and not 1.0.0.127. Getting this backwards is the
	// classic /proc bug: every loopback-only service looks externally bound.
	es := parseFixture(t, "proc_net_tcp", TCP)
	byPort := map[int]NetEntry{}
	for _, e := range es {
		byPort[int(e.Local.Port())] = e
	}
	for port, want := range map[int]string{
		27015: "127.0.0.1",
		8081:  "127.0.0.1",
		53:    "127.0.0.53",
		22:    "0.0.0.0",
	} {
		if got := byPort[port].Local.Addr().String(); got != want {
			t.Errorf("tcp/%d local address = %s, want %s", port, got, want)
		}
	}
	if got := byPort[9100].Remote; got.String() != "127.0.0.1:49588" {
		t.Errorf("remote address = %s", got)
	}
}

func TestIPv6TableDecodesIncludingMappedIPv4(t *testing.T) {
	es := parseFixture(t, "proc_net_tcp6", TCP)
	if len(es) != 4 {
		t.Fatalf("parsed %d rows, want 4", len(es))
	}
	byPort := map[int]NetEntry{}
	for _, e := range es {
		byPort[int(e.Local.Port())] = e
	}
	if got := byPort[22].Local.Addr().String(); got != "::" {
		t.Errorf("tcp6/22 = %s, want ::", got)
	}
	if got := byPort[2222].Local.Addr().String(); got != "::" {
		t.Errorf("tcp6/2222 = %s, want ::", got)
	}
	// An IPv4 socket showing up in the v6 table is unmapped, so a report never
	// makes the operator work out which table a row came from.
	if got := byPort[5432].Local.Addr().String(); got != "127.0.0.1" {
		t.Errorf("tcp6/5432 = %s, want the unmapped 127.0.0.1", got)
	}
	if got := byPort[80].Local.Addr().String(); got != "10.10.5.195" {
		t.Errorf("tcp6/80 = %s, want 10.10.5.195", got)
	}
	// And the established v6 row is still not a listener.
	if byPort[80].Bound() {
		t.Error("an ESTABLISHED tcp6 connection is not a listener")
	}

	u6 := parseFixture(t, "proc_net_udp6", UDP)
	byPort = map[int]NetEntry{}
	for _, e := range u6 {
		byPort[int(e.Local.Port())] = e
	}
	if got := byPort[2456].Local.Addr().String(); got != "::" {
		t.Errorf("udp6/2456 = %s", got)
	}
	if got := byPort[5353].Local.Addr().String(); got != "192.168.1.10" {
		t.Errorf("udp6/5353 = %s, want 192.168.1.10", got)
	}
	if byPort[0].Bound() {
		t.Error("udp6 entry with no local port is not bound")
	}
}

func TestInodeAndUIDComeOffTheRightColumns(t *testing.T) {
	// They are columns 9 and 11 of a row whose trailing columns differ between
	// tcp and udp and between kernel versions, so an off-by-one here is silent
	// until an inode fails to map to a pid.
	es := parseFixture(t, "proc_net_tcp", TCP)
	for _, e := range es {
		if e.Local.Port() == 27015 {
			if e.UID != 1001 {
				t.Errorf("uid = %d, want 1001", e.UID)
			}
			if e.Inode != 20114 {
				t.Errorf("inode = %d, want 20114", e.Inode)
			}
		}
	}
	// UDP rows carry three extra trailing columns and must decode identically.
	us := parseFixture(t, "proc_net_udp", UDP)
	for _, e := range us {
		if e.Local.Port() == 16261 {
			if e.UID != 1001 || e.Inode != 20115 {
				t.Errorf("udp/16261 uid=%d inode=%d, want 1001/20115", e.UID, e.Inode)
			}
		}
	}
}

func TestAMalformedRowIsAnErrorAndNotASkippedLine(t *testing.T) {
	// Skipping is the dangerous direction: a row the parser did not understand
	// is a port somebody is listening on, and dropping it hands that port to a
	// server that will then fail to bind.
	cases := []struct {
		name, text, want string
	}{
		{"short row", "   0: 00000000:0016 00000000:0000 0A\n", "columns"},
		{"no port separator", "   0: 000000000016 00000000:0000 0A 0:0 00:0 0 0 0 18443\n", "port separator"},
		{"odd address width", "   0: 000000:0016 00000000:0000 0A 0:0 00:0 0 0 0 18443\n", "hex digits"},
		{"non-hex state", "   0: 00000000:0016 00000000:0000 ZZ 0:0 00:0 0 0 0 18443\n", "state"},
		{"non-numeric inode", "   0: 00000000:0016 00000000:0000 0A 0:0 00:0 0 0 0 socket\n", "inode"},
		{"non-numeric uid", "   0: 00000000:0016 00000000:0000 0A 0:0 00:0 0 root 0 18443\n", "uid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseProcNet(strings.NewReader(c.text), TCP)
			if err == nil {
				t.Fatal("want an error, got a silently dropped listener")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Errorf("error %q does not name the line", err)
			}
		})
	}
}

func TestParseProcNetRefusesAProtocolItDoesNotKnow(t *testing.T) {
	if _, err := ParseProcNet(strings.NewReader(""), Proto("sctp")); err == nil {
		t.Fatal("want an error for an unknown protocol")
	}
}

func TestParseProcNetAcceptsAnEmptyTable(t *testing.T) {
	// A host with nothing listening on udp6 has a header and no rows. That is
	// not an error and must not be confused with an unreadable table.
	es, err := ParseProcNet(strings.NewReader("  sl  local_address rem_address   st ... inode\n"), UDP)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 0 {
		t.Errorf("parsed %d rows from a header-only table", len(es))
	}
}

func TestEndpointFromEntry(t *testing.T) {
	e := NetEntry{Proto: UDP, Local: netip.MustParseAddrPort("0.0.0.0:16261")}
	if got := e.Endpoint(); got != (Endpoint{Proto: UDP, Port: 16261}) {
		t.Errorf("Endpoint() = %v", got)
	}
	if got := e.Endpoint().String(); got != "udp/16261" {
		t.Errorf("String() = %q", got)
	}
}

func TestParseSocketLink(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  uint64
		found bool
	}{
		{"socket:[18443]", 18443, true},
		{"socket:[0]", 0, true},
		{"/dev/null", 0, false},
		{"anon_inode:[eventpoll]", 0, false},
		{"pipe:[41022]", 0, false},
		{"socket:[]", 0, false},
		{"socket:[abc]", 0, false},
		{"socket:[18443", 0, false},
	} {
		got, ok := ParseSocketLink(c.in)
		if ok != c.found || got != c.want {
			t.Errorf("ParseSocketLink(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.found)
		}
	}
}

// TestHostListenersIsHonestOffLinux pins the stub's contract: an empty slice
// would be read as "nothing is listening on this host", which is the one answer
// that must never come from a platform that did not look.
func TestHostListenersIsHonestOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("the stub's contract is what this asserts; on Linux the real scan runs")
	}
	ls, err := HostListeners()
	if err == nil {
		t.Fatalf("HostListeners returned %d listeners on a platform with no /proc", len(ls))
	}
	if !errors.Is(err, platform.ErrNotSupported) {
		t.Errorf("error = %v, want one wrapping platform.ErrNotSupported", err)
	}
	if ls != nil {
		t.Errorf("HostListeners returned listeners alongside an error: %v", ls)
	}
}
