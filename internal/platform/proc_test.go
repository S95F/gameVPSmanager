package platform

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// The five fixtures are all the same process with different comms, so any
// difference in the decoded starttime is the comm parsing and nothing else.
const fixtureStartTicks = 8391244

func TestParseStatAcceptsACommWithSpacesAndParens(t *testing.T) {
	cases := []struct {
		fixture string
		pid     int
		comm    string
	}{
		{"stat_simple", 41799, "java"},
		{"stat_webcontent", 4412, "Web Content"},
		{"stat_parens", 9001, "a) (b"},
		{"stat_maxcomm", 777, "systemd-journal"},
		{"stat_trap", 1234, ") 9 9 9 9 9 9 9"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			st, err := ParseStat(readFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("ParseStat: %v", err)
			}
			if st.PID != tc.pid {
				t.Errorf("PID = %d, want %d", st.PID, tc.pid)
			}
			if st.Comm != tc.comm {
				t.Errorf("Comm = %q, want %q", st.Comm, tc.comm)
			}
			if st.StartTicks != fixtureStartTicks {
				t.Errorf("StartTicks = %d, want %d", st.StartTicks, fixtureStartTicks)
			}
			if st.State != "S" {
				t.Errorf("State = %q, want S", st.State)
			}
			if st.PPID != 41798 || st.PGID != 41799 {
				t.Errorf("PPID/PGID = %d/%d, want 41798/41799", st.PPID, st.PGID)
			}
		})
	}
}

// The whole reason ParseStat scans backwards. If this test ever fails because
// the naive answer matches, the fixture stopped exercising the trap and the
// parser is no longer being proven correct by it.
func TestWhitespaceSplittingStatWouldPinTheWrongProcess(t *testing.T) {
	raw := readFixture(t, "stat_trap")

	naive := strings.Fields(string(raw))
	if len(naive) < 22 {
		t.Fatalf("fixture has %d whitespace-separated tokens", len(naive))
	}
	wrong, err := strconv.ParseUint(naive[21], 10, 64)
	if err != nil {
		t.Fatalf("naive field 22 %q is not a number, so the fixture no longer models the trap", naive[21])
	}
	if wrong == fixtureStartTicks {
		t.Fatalf("naive split already yields %d; the fixture no longer exercises the comm trap", wrong)
	}

	got, err := ParseStartTicks(raw)
	if err != nil {
		t.Fatalf("ParseStartTicks: %v", err)
	}
	if got != fixtureStartTicks {
		t.Fatalf("ParseStartTicks = %d, want %d (naive split would have said %d)", got, fixtureStartTicks, wrong)
	}
}

func TestParseStatRejectsMalformedRecords(t *testing.T) {
	tail := " S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 8391244 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no parens", "41799 java S 1 1"},
		{"unclosed comm", "41799 (java S 1 1"},
		{"pid not a number", "four (java)" + tail},
		{"truncated after comm", "41799 (java) S 1 1 1 0 -1 0"},
		{"starttime not a number", "41799 (java) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 later 0 0"},
		{"ppid not a number", "41799 (java) S x 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 8391244 0 0"},
		// A comm longer than TASK_COMM_LEN-1 means the ')' we found is not
		// the comm's closer, so every field index after it is shifted.
		{"comm over 15 bytes", "41799 (abcdefghijklmnop)" + tail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if st, err := ParseStat([]byte(tc.in)); err == nil {
				t.Fatalf("ParseStat accepted %q as %+v", tc.in, st)
			}
		})
	}
}

func TestParseStatAcceptsExactlyTheMinimumFieldCount(t *testing.T) {
	// Field 22 is the last one gvm needs; a kernel that stops there must
	// still parse, and one field short must not.
	fields := make([]string, 0, 20)
	fields = append(fields, "S")
	for i := 4; i <= 21; i++ {
		fields = append(fields, "0")
	}
	fields = append(fields, "8391244")

	ok := "41799 (java) " + strings.Join(fields, " ")
	if _, err := ParseStat([]byte(ok)); err != nil {
		t.Fatalf("20 fields after comm rejected: %v", err)
	}
	short := "41799 (java) " + strings.Join(fields[:len(fields)-1], " ")
	if _, err := ParseStat([]byte(short)); err == nil {
		t.Fatal("19 fields after comm accepted")
	}
}

func TestParseStatHandlesAnEmptyComm(t *testing.T) {
	st, err := ParseStat(readFixture(t, "stat_simple"))
	if err != nil {
		t.Fatal(err)
	}
	blanked := strings.Replace(string(readFixture(t, "stat_simple")), "(java)", "()", 1)
	got, err := ParseStat([]byte(blanked))
	if err != nil {
		t.Fatalf("empty comm rejected: %v", err)
	}
	if got.Comm != "" || got.StartTicks != st.StartTicks {
		t.Fatalf("empty comm decoded as %+v", got)
	}
}

func TestParsePIDs(t *testing.T) {
	got, err := ParsePIDs([]byte("41799\n41822\n41830\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{41799, 41822, 41830}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// An emptied cgroup is the normal end state of the shutdown ladder.
	got, err = ParsePIDs(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty body: %v, %v", got, err)
	}

	for _, bad := range []string{"41799\nnotapid\n", "0\n", "-1\n"} {
		if _, err := ParsePIDs([]byte(bad)); err == nil {
			t.Errorf("ParsePIDs accepted %q", bad)
		}
	}
}

func TestParseSelfCgroup(t *testing.T) {
	v2 := "0::/system.slice/gvm-pz.slice/gvm@pz-main.service\n"
	got, err := ParseSelfCgroup([]byte(v2))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/system.slice/gvm-pz.slice/gvm@pz-main.service"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A hybrid host still has the unified entry; it is the one to use.
	hybrid := "8:memory:/system.slice/gvm@pz-main.service\n" +
		"1:name=systemd:/system.slice/gvm@pz-main.service\n" +
		"0::/system.slice/gvm@pz-main.service\n"
	if got, err = ParseSelfCgroup([]byte(hybrid)); err != nil || got != "/system.slice/gvm@pz-main.service" {
		t.Fatalf("hybrid: %q, %v", got, err)
	}

	for _, bad := range []string{
		"",
		"1:name=systemd:/system.slice/gvm@pz-main.service\n",
		"0::relative/path\n",
		"garbage\n",
	} {
		if _, err := ParseSelfCgroup([]byte(bad)); err == nil {
			t.Errorf("ParseSelfCgroup accepted %q", bad)
		}
	}
}

func TestCgroupProcsPath(t *testing.T) {
	got, err := cgroupProcsPath("/system.slice/gvm@pz-main.service")
	if err != nil {
		t.Fatal(err)
	}
	want := "/sys/fs/cgroup/system.slice/gvm@pz-main.service/cgroup.procs"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Forward slashes even when the test runs on Windows: the result names a
	// file on the deployment host, not on this one.
	if strings.Contains(got, `\`) {
		t.Fatalf("path contains a backslash: %q", got)
	}

	if got, err = cgroupProcsPath("/"); err != nil || got != "/sys/fs/cgroup/cgroup.procs" {
		t.Fatalf("root cgroup: %q, %v", got, err)
	}

	// path.Clean("/../../etc") is "/etc", so a check that cleans first would
	// turn each of these into a different, plausible, wrong cgroup.
	for _, bad := range []string{"", "system.slice", "/../../etc", "/..", "/a/../b", `\system.slice`} {
		if got, err := cgroupProcsPath(bad); err == nil {
			t.Errorf("cgroupProcsPath(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCheckPGIDRefusesTheCatastrophicTargets(t *testing.T) {
	// kill(-0) hits gvm's own process group and kill(-1) hits every process
	// the uid can reach; both are one typo away from a normal pgid.
	for _, pgid := range []int{0, 1, -1, -41799} {
		if err := checkPGID(pgid); err == nil {
			t.Errorf("checkPGID(%d) allowed the signal", pgid)
		}
	}
	for _, pgid := range []int{2, 41799} {
		if err := checkPGID(pgid); err != nil {
			t.Errorf("checkPGID(%d) = %v", pgid, err)
		}
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]Signal{
		"SIGTERM":  SIGTERM,
		"sigterm":  SIGTERM,
		" SIGINT ": SIGINT,
		"SIGKILL":  SIGKILL,
		"SIGHUP":   SIGHUP,
		"SIGQUIT":  SIGQUIT,
		"SIGUSR1":  SIGUSR1,
		"SIGUSR2":  SIGUSR2,
	}
	for in, want := range cases {
		got, err := ParseSignal(in)
		if err != nil {
			t.Errorf("ParseSignal(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSignal(%q) = %v, want %v", in, got, want)
		}
	}
	// One spelling only: the number hides SIGKILL behind a digit, and the
	// bare kill(1) form is a second way to write a ladder step that the
	// manifest validator would reject anyway.
	for _, bad := range []string{"", "9", "SIGSAVE", "SIG", "TERM", "kill", "SIGCONT"} {
		if got, err := ParseSignal(bad); err == nil {
			t.Errorf("ParseSignal(%q) = %v, want an error", bad, got)
		}
	}
}

// The manifest validator has its own closed signal list; a name it accepts that
// this package cannot resolve would fail at the shutdown ladder instead of at
// validation, which is the worst possible time to find out.
func TestEverySignalTheManifestValidatorAcceptsResolves(t *testing.T) {
	for _, name := range []string{"SIGTERM", "SIGINT", "SIGKILL", "SIGHUP", "SIGQUIT", "SIGUSR1", "SIGUSR2"} {
		sig, err := ParseSignal(name)
		if err != nil {
			t.Fatalf("spec accepts %s but platform cannot resolve it: %v", name, err)
		}
		if !sig.Known() || sig.String() != name {
			t.Fatalf("%s round-tripped as %q", name, sig.String())
		}
	}
	if got := Signal(31).String(); got != "signal 31" {
		t.Fatalf("unknown signal rendered as %q", got)
	}
}

func TestProcOperationsAreUnsupportedOffLinux(t *testing.T) {
	if isLinux() {
		t.Skip("these are the real implementations on Linux")
	}
	if _, err := StartTicks(1); !errors.Is(err, ErrNotSupported) {
		t.Errorf("StartTicks: %v", err)
	}
	if _, err := ReadStat(1); !errors.Is(err, ErrNotSupported) {
		t.Errorf("ReadStat: %v", err)
	}
	// Fails closed: not "the process is gone", but "this cannot be answered".
	alive, err := Alive(1, 1)
	if alive || !errors.Is(err, ErrNotSupported) {
		t.Errorf("Alive = %v, %v", alive, err)
	}
	if err := SignalGroup(41799, SIGTERM); !errors.Is(err, ErrNotSupported) {
		t.Errorf("SignalGroup: %v", err)
	}
	if _, err := SelfCgroup(); !errors.Is(err, ErrNotSupported) {
		t.Errorf("SelfCgroup: %v", err)
	}
	if _, err := CgroupProcs("/system.slice"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("CgroupProcs: %v", err)
	}
}
