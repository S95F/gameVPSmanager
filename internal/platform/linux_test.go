package platform

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are the assertions that cannot be made without a kernel. Everything
// they exercise has its logic in an untagged file that is tested on both
// platforms; what is left here is that the syscalls are wired to it correctly.

func TestStatfsReportsTheRealFilesystem(t *testing.T) {
	if !isLinux() {
		t.Skip("statfs is Linux-only")
	}
	d, err := Statfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 {
		t.Fatal("Total is zero")
	}
	if d.Avail > d.Free {
		t.Errorf("Avail %d exceeds Free %d", d.Avail, d.Free)
	}
	if d.Free > d.Total {
		t.Errorf("Free %d exceeds Total %d", d.Free, d.Total)
	}
	if _, err := Statfs(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("statfs on a missing path succeeded")
	}
}

func TestEphemeralRangeReadsTheLiveSysctl(t *testing.T) {
	if !isLinux() {
		t.Skip("/proc/sys is Linux-only")
	}
	r, err := EphemeralRange()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Valid() {
		t.Fatalf("kernel reported an invalid range %v", r)
	}
}

func TestStartTicksAndAliveAgreeAboutThisProcess(t *testing.T) {
	if !isLinux() {
		t.Skip("/proc is Linux-only")
	}
	self := os.Getpid()
	ticks, err := StartTicks(self)
	if err != nil {
		t.Fatal(err)
	}
	if ticks == 0 {
		t.Fatal("StartTicks returned 0 for a running process")
	}

	alive, err := Alive(self, ticks)
	if err != nil || !alive {
		t.Fatalf("Alive(self, its own ticks) = %v, %v", alive, err)
	}
	// The pin's entire purpose: same pid, different start time, not the same
	// process. This is what stops the ladder signalling a recycled pid.
	if alive, err = Alive(self, ticks+1); err != nil || alive {
		t.Fatalf("Alive(self, wrong ticks) = %v, %v", alive, err)
	}

	st, err := ReadStat(self)
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != self || st.PPID != os.Getppid() {
		t.Fatalf("ReadStat = %+v, want pid %d ppid %d", st, self, os.Getppid())
	}
	// A real comm from a real kernel, so the 15-byte guard is exercised
	// against something other than a fixture.
	if st.Comm == "" || len(st.Comm) > 15 {
		t.Fatalf("comm = %q", st.Comm)
	}
}

// A pid that does not exist is definitively dead: (false, nil). A pid that
// cannot be read for any other reason is (false, err), because a caller that
// cannot tell must refuse to spawn rather than race a live server.
func TestAliveSeparatesGoneFromUnknowable(t *testing.T) {
	if !isLinux() {
		t.Skip("/proc is Linux-only")
	}
	// pid_max is at least 2^15 on any Linux; this one is not allocatable.
	alive, err := Alive(0x7FFFFFFE, 1)
	if alive || err != nil {
		t.Fatalf("Alive(unallocated) = %v, %v; want false, nil", alive, err)
	}
	if _, err := StartTicks(0); err == nil {
		t.Error("StartTicks(0) succeeded")
	}
	if _, err := StartTicks(-1); err == nil {
		t.Error("StartTicks(-1) succeeded")
	}
}

func TestSignalGroupRefusesTheCatastrophicTargetsOnLinuxToo(t *testing.T) {
	if !isLinux() {
		t.Skip("kill(-pgid) is Linux-only")
	}
	// The guard has to be in front of the syscall, not merely in front of a
	// caller: SignalGroup(0, SIGKILL) would kill the test binary.
	for _, pgid := range []int{0, 1, -1} {
		if err := SignalGroup(pgid, SIGTERM); err == nil {
			t.Fatalf("SignalGroup(%d) was allowed", pgid)
		}
	}
	if err := SignalGroup(41799, Signal(64)); err == nil {
		t.Fatal("SignalGroup accepted a signal outside the closed set")
	}
}

// ProcCgroup answers for an ARBITRARY pid, which is what lets a shutdown prove
// the process it is about to signal is the game rather than whatever inherited
// the caller's cgroup along with its INVOCATION_ID.
func TestProcCgroupAgreesWithSelfCgroup(t *testing.T) {
	if !isLinux() {
		t.Skip("cgroups are Linux-only")
	}
	want, err := SelfCgroup()
	if err != nil {
		t.Skipf("no cgroup v2 entry for this process: %v", err)
	}
	got, err := ProcCgroup(os.Getpid())
	if err != nil {
		t.Fatalf("ProcCgroup(self) = %v", err)
	}
	if got != want {
		t.Errorf("ProcCgroup(self) = %q, SelfCgroup = %q", got, want)
	}
	if _, err := ProcCgroup(0); err == nil {
		t.Error("ProcCgroup accepted pid 0, which kill(2) reads as the caller's own group")
	}
}

func TestSelfCgroupAndCgroupProcs(t *testing.T) {
	if !isLinux() {
		t.Skip("cgroups are Linux-only")
	}
	rel, err := SelfCgroup()
	if err != nil {
		// A container without the unified hierarchy mounted is a legitimate
		// environment for the test to run in; the parser is covered anyway.
		t.Skipf("no cgroup v2 entry for this process: %v", err)
	}
	if !strings.HasPrefix(rel, "/") {
		t.Fatalf("SelfCgroup = %q, want an absolute path", rel)
	}

	pids, err := CgroupProcs(rel)
	if err != nil {
		t.Skipf("cgroup.procs unreadable in this environment: %v", err)
	}
	if len(pids) == 0 {
		// A cgroup namespace makes /proc/self/cgroup report a path relative
		// to the namespace root while /sys/fs/cgroup shows the host's, so the
		// join lands on nothing. That is an environment gvm does not deploy
		// into, not a parsing failure.
		t.Skipf("no cgroup.procs under %q; cgroups are namespaced in this environment", rel)
	}
	self := os.Getpid()
	for _, pid := range pids {
		if pid == self {
			return
		}
	}
	t.Errorf("CgroupProcs(%q) = %v, which does not include this process %d", rel, pids, self)
}

func TestCgroupProcsOfAVanishedCgroupIsEmptyNotAnError(t *testing.T) {
	if !isLinux() {
		t.Skip("cgroups are Linux-only")
	}
	// A cgroup removed between the read of runner.json and the last rung of
	// the ladder means there is nothing left to kill, not a failure.
	pids, err := CgroupProcs("/gvm-does-not-exist.slice/gvm@nobody.service")
	if err != nil || len(pids) != 0 {
		t.Fatalf("got %v, %v", pids, err)
	}
}

func TestUserExistsAndGroupList(t *testing.T) {
	if !isLinux() {
		t.Skip("unix accounts are Linux-only")
	}
	ok, err := UserExists("root")
	if err != nil || !ok {
		t.Fatalf("UserExists(root) = %v, %v", ok, err)
	}
	ok, err = UserExists("gvm-no-such-instance-user")
	if err != nil || ok {
		t.Fatalf("UserExists(absent) = %v, %v; want false and no error", ok, err)
	}

	groups, err := GroupList("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("root belongs to no groups")
	}
	for i := 1; i < len(groups); i++ {
		if groups[i-1] > groups[i] {
			t.Fatalf("GroupList is not sorted: %v", groups)
		}
	}
	if _, err := GroupList("gvm-no-such-instance-user"); err == nil {
		t.Error("GroupList on an absent user succeeded")
	}
}

func TestPeerCredIdentifiesThisProcess(t *testing.T) {
	if !isLinux() {
		t.Skip("SO_PEERCRED is Linux-only")
	}
	sock := filepath.Join(t.TempDir(), "console.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type accepted struct {
		peer Peer
		err  error
	}
	done := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- accepted{err: err}
			return
		}
		defer c.Close()
		p, err := PeerCred(c.(*net.UnixConn))
		done <- accepted{peer: p, err: err}
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PeerCred: %v", got.err)
		}
		if int(got.peer.PID) != os.Getpid() {
			t.Errorf("peer pid = %d, want %d", got.peer.PID, os.Getpid())
		}
		if int(got.peer.UID) != os.Getuid() {
			t.Errorf("peer uid = %d, want %d", got.peer.UID, os.Getuid())
		}
		if got.peer.Root() != (os.Getuid() == 0) {
			t.Errorf("Root() = %v for uid %d", got.peer.Root(), got.peer.UID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the accept")
	}
}

func TestNotifierDeliversTheDatagram(t *testing.T) {
	if !isLinux() {
		t.Skip("unixgram is Linux-only")
	}
	sock := filepath.Join(t.TempDir(), "notify")
	pc, err := net.ListenPacket("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	n := NotifierAt(sock)
	if !n.Enabled() {
		t.Fatal("notifier is disabled for a path NOTIFY_SOCKET")
	}
	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	buf := make([]byte, 256)
	if err := pc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	nb, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:nb]); got != "READY=1" {
		t.Fatalf("received %q", got)
	}

	if err := n.Status("booting: loading world"); err != nil {
		t.Fatal(err)
	}
	if nb, _, err = pc.ReadFrom(buf); err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:nb]); got != "STATUS=booting: loading world" {
		t.Fatalf("received %q", got)
	}
}

func TestNotifierReportsADeadSocket(t *testing.T) {
	if !isLinux() {
		t.Skip("unixgram is Linux-only")
	}
	// systemd removing the notify socket must surface as an error the runner
	// can log, not as a silent success that hides a lost readiness report.
	err := NotifierAt(filepath.Join(t.TempDir(), "gone")).Ready()
	if err == nil {
		t.Fatal("notifying a nonexistent socket succeeded")
	}
	if errors.Is(err, ErrNotSupported) {
		t.Fatalf("Linux returned the stub error: %v", err)
	}
}
