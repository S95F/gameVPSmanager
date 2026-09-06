package ports

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc is a ProcSource backed by the testdata fixtures and an in-memory
// process table.
//
// The fd half cannot be a directory of real symlinks: /proc/<pid>/fd entries
// are symlinks to "socket:[N]", which is not a path, and the development
// machine will not create one without privilege. The seam is what makes the
// inode-to-pid logic testable anyway, which is the whole reason it exists.
type fakeProc struct {
	tables map[string]string // "tcp" -> fixture file name, absent means ENOENT
	fds    map[int][]uint64
	comm   map[int]string

	netErr  map[string]error
	pidErr  error
	fdErr   map[int]error
	commErr map[int]error
}

func (f fakeProc) NetFile(name string) (io.ReadCloser, error) {
	if err, ok := f.netErr[name]; ok {
		return nil, err
	}
	fixture, ok := f.tables[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: "/proc/net/" + name, Err: fs.ErrNotExist}
	}
	return os.Open(filepath.Join("testdata", fixture))
}

func (f fakeProc) PIDs() ([]int, error) {
	if f.pidErr != nil {
		return nil, f.pidErr
	}
	out := make([]int, 0, len(f.fds))
	for pid := range f.fds {
		out = append(out, pid)
	}
	// Sorted so attribution is deterministic when two processes share an inode.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (f fakeProc) SocketInodes(pid int) ([]uint64, error) {
	if err, ok := f.fdErr[pid]; ok {
		return nil, err
	}
	return f.fds[pid], nil
}

func (f fakeProc) Comm(pid int) (string, error) {
	if err, ok := f.commErr[pid]; ok {
		return "", err
	}
	return f.comm[pid], nil
}

func wholeHost() fakeProc {
	return fakeProc{
		tables: map[string]string{
			"tcp":  "proc_net_tcp",
			"tcp6": "proc_net_tcp6",
			"udp":  "proc_net_udp",
			"udp6": "proc_net_udp6",
		},
		fds: map[int][]uint64{
			1:    {18443, 18445, 18992}, // sshd on 22, both families, plus a second port
			4412: {20115, 20116},        // the game server's two UDP sockets
			4470: {20114, 20117, 22001}, // rcon, telnet and a wireguard socket
		},
		comm: map[int]string{1: "sshd", 4412: "java", 4470: "python3"},
	}
}

func byEndpoint(ls []Listener) map[Endpoint]Listener {
	out := map[Endpoint]Listener{}
	for _, l := range ls {
		out[l.Endpoint] = l
	}
	return out
}

func TestCollectListenersReadsAllFourTables(t *testing.T) {
	ls, err := CollectListeners(wholeHost())
	if err != nil {
		t.Fatal(err)
	}
	got := byEndpoint(ls)

	// A server bound to :: on a dual-stack host accepts IPv4 on the same port,
	// so reading only the v4 tables is how a port that is very obviously in use
	// gets handed out anyway.
	for _, ep := range []Endpoint{
		{TCP, 22}, {TCP, 27015}, {TCP, 8081}, {TCP, 53}, // tcp
		{TCP, 2222}, {TCP, 5432}, // tcp6
		{UDP, 68}, {UDP, 123}, {UDP, 16261}, {UDP, 16262}, {UDP, 51820}, // udp
		{UDP, 2456}, {UDP, 5353}, // udp6
	} {
		if _, ok := got[ep]; !ok {
			t.Errorf("%s is missing from the scan", ep)
		}
	}
	// And nothing that is not bound.
	for _, ep := range []Endpoint{{TCP, 9100}, {TCP, 3000}, {TCP, 80}, {UDP, 0}} {
		if _, ok := got[ep]; ok {
			t.Errorf("%s is not a bound listener and must not appear", ep)
		}
	}
}

func TestListenersAreAttributedThroughTheirInode(t *testing.T) {
	ls, err := CollectListeners(wholeHost())
	if err != nil {
		t.Fatal(err)
	}
	got := byEndpoint(ls)

	for ep, want := range map[Endpoint]struct {
		pid  int
		comm string
	}{
		{UDP, 16261}: {4412, "java"},
		{UDP, 16262}: {4412, "java"},
		{TCP, 27015}: {4470, "python3"},
		{TCP, 8081}:  {4470, "python3"},
		{TCP, 22}:    {1, "sshd"},
		{TCP, 2222}:  {1, "sshd"}, // a v6-only socket of the same process
	} {
		l := got[ep]
		if l.PID != want.pid || l.Comm != want.comm {
			t.Errorf("%s attributed to pid %d %q, want pid %d %q", ep, l.PID, l.Comm, want.pid, want.comm)
		}
	}
}

func TestAnUnattributableListenerIsReportedHonestly(t *testing.T) {
	// A socket owned by another user is invisible in /proc unless gvm runs as
	// root. Reporting it as unowned is right; dropping it would hand its port
	// to a new instance.
	ls, err := CollectListeners(wholeHost())
	if err != nil {
		t.Fatal(err)
	}
	l := byEndpoint(ls)[Endpoint{TCP, 53}]
	if l.PID != 0 {
		t.Fatalf("tcp/53 was attributed to pid %d; nothing in the fake holds its inode", l.PID)
	}
	d := l.Describe()
	for _, want := range []string{"foreign listener", "127.0.0.53", "not visible"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe() = %q, want it to mention %q", d, want)
		}
	}
	// It must still block.
	occ := emptyHost()
	occ.AddListeners(ls)
	if occ.Free(Endpoint{TCP, 53}, "anyone") {
		t.Error("an unattributed listener does not block, so its port can be handed out")
	}
}

func TestAttributedListenerDescribesLikeTheSpecExample(t *testing.T) {
	l := Listener{Endpoint: Endpoint{UDP, 16281}, PID: 4412, Comm: "python3"}
	if got := l.Describe(); got != "a foreign listener (pid 4412 python3)" {
		t.Errorf("Describe() = %q", got)
	}
}

func TestOneMissingTableIsToleratedAndAllFourIsNot(t *testing.T) {
	// A host booted with ipv6.disable=1 has no /proc/net/tcp6, and that is not
	// a reason to refuse to allocate.
	h := wholeHost()
	delete(h.tables, "tcp6")
	delete(h.tables, "udp6")
	ls, err := CollectListeners(h)
	if err != nil {
		t.Fatalf("an IPv6-less host must still scan: %v", err)
	}
	if len(ls) == 0 {
		t.Fatal("the v4 tables produced nothing")
	}

	// All four missing is a different thing entirely: "nothing is listening"
	// and "the tables could not be read" produce the same empty slice and mean
	// opposite things, and only one of them is safe to allocate on top of.
	h.tables = map[string]string{}
	if _, err := CollectListeners(h); err == nil {
		t.Fatal("a host where no socket table could be read must not report an empty one")
	} else if !strings.Contains(err.Error(), "unknown, not empty") {
		t.Errorf("error = %v", err)
	}
}

func TestAnUnreadableTableIsAnError(t *testing.T) {
	h := wholeHost()
	h.netErr = map[string]error{"udp": fs.ErrPermission}
	if _, err := CollectListeners(h); err == nil {
		t.Fatal("a permission error on a socket table is not an empty table")
	}
}

func TestAMalformedTableStopsTheScan(t *testing.T) {
	h := wholeHost()
	h.tables["tcp"] = "proc_net_malformed"
	if err := os.WriteFile(filepath.Join("testdata", "proc_net_malformed"), []byte("  sl  local_address\n  0: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(filepath.Join("testdata", "proc_net_malformed")) })

	if _, err := CollectListeners(h); err == nil {
		t.Fatal("a row the parser did not understand is a listener it would have dropped")
	}
}

func TestAProcessThatExitsMidScanIsNotAnIncident(t *testing.T) {
	h := wholeHost()
	h.fds[9999] = []uint64{20114}
	h.fdErr = map[int]error{9999: fs.ErrNotExist}
	ls, err := CollectListeners(h)
	if err != nil {
		t.Fatalf("a vanished process must not fail the scan: %v", err)
	}
	// The socket is still attributed to the process that really holds it.
	if got := byEndpoint(ls)[Endpoint{TCP, 27015}]; got.PID != 4470 {
		t.Errorf("tcp/27015 pid = %d, want 4470", got.PID)
	}
}

func TestAnUnreadableProcessTableIsAnError(t *testing.T) {
	// Individual processes vanishing is routine; /proc itself being unreadable
	// is a host problem worth naming rather than degrading past.
	h := wholeHost()
	h.pidErr = fs.ErrPermission
	if _, err := CollectListeners(h); err == nil {
		t.Fatal("want an error when the process table cannot be listed")
	}
}

func TestAttributeListenersStampsOwners(t *testing.T) {
	// Without the pid-to-instance mapping an instance's own live socket looks
	// like a foreign listener, which is harmless during allocation and reads as
	// a false conflict in doctor.
	ls, err := CollectListeners(wholeHost())
	if err != nil {
		t.Fatal(err)
	}
	owned := AttributeListeners(ls, map[int]string{4412: "pz-main"})
	got := byEndpoint(owned)[Endpoint{UDP, 16261}]
	if got.Owner != "pz-main" {
		t.Fatalf("owner = %q, want pz-main", got.Owner)
	}
	if d := got.Describe(); !strings.Contains(d, "gvm@pz-main") {
		t.Errorf("Describe() = %q", d)
	}
	// The input is not modified: a caller holding the raw scan keeps it.
	if byEndpoint(ls)[Endpoint{UDP, 16261}].Owner != "" {
		t.Error("AttributeListeners mutated its argument")
	}
	// And a pid of zero is never matched, or every unattributed socket on the
	// host would be claimed by whichever instance mapped 0.
	unowned := AttributeListeners(ls, map[int]string{0: "pz-main"})
	if byEndpoint(unowned)[Endpoint{TCP, 53}].Owner != "" {
		t.Error("an unattributed listener was given an owner through pid 0")
	}
}

func TestDirProcSourceReadsRealFiles(t *testing.T) {
	// The half of DirProcSource that does not need symlinks: net tables and the
	// numeric-directory filter. Root is a parameter precisely so this can be
	// asserted anywhere.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile(filepath.Join("testdata", "proc_net_"+n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "net", n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"1", "4412", "self", "sys", "irq"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "4412", "comm"), []byte("java\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewDirProcSource(root)
	f, err := src.NetFile("udp")
	if err != nil {
		t.Fatal(err)
	}
	es, err := ParseProcNet(f, UDP)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 6 {
		t.Errorf("read %d udp rows through DirProcSource, want 6", len(es))
	}

	pids, err := src.PIDs()
	if err != nil {
		t.Fatal(err)
	}
	// self, sys and irq are directories under /proc that are not processes.
	if len(pids) != 2 || pids[0] != 1 || pids[1] != 4412 {
		t.Errorf("PIDs = %v, want [1 4412]", pids)
	}

	if comm, err := src.Comm(4412); err != nil || comm != "java" {
		t.Errorf("Comm(4412) = %q, %v", comm, err)
	}
	if _, err := src.NetFile("sctp"); err == nil {
		t.Error("a table that does not exist must report so")
	}
	// A default root keeps a caller that forgot one pointed at the real procfs
	// rather than at the current directory.
	if NewDirProcSource("").Root != "/proc" {
		t.Error("an empty root must default to /proc")
	}
}
