package reconcile

import (
	"errors"
	"strings"
	"testing"
)

func pzSandbox(t *testing.T) Sandbox {
	t.Helper()
	in, _ := loadInstance(t, "pz-main")
	sb, err := BuildSandbox(in)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestTheProbeCommandCarriesExactlyTheSandboxTheDropInWillCarry.
//
// This is the property that makes the probe meaningful at all. A transient
// unit built from its own idea of the confinement would answer a question
// about a unit nobody is going to run.
func TestTheProbeCommandCarriesExactlyTheSandboxTheDropInWillCarry(t *testing.T) {
	sb := pzSandbox(t)
	args, err := ProbeSpec{Instance: "pz-main", User: "pzserver", Group: "pzserver", Sandbox: sb}.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--unit=gvm-probe-pz-main",
		"--slice=gvm-jobs.slice",
		"--uid=pzserver",
		"--gid=pzserver",
		"--property=ProtectHome=no",
		"--property=ProtectSystem=full",
		"--property=NoNewPrivileges=yes",
		"--property=ReadWritePaths=" + strings.Join(sb.Paths, " "),
		"/usr/bin/gvm probe-write",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the probe command is missing %q:\n%s", want, joined)
		}
	}
	// --wait, or the probe returns before the answer exists.
	if !strings.Contains(joined, "--wait") || !strings.Contains(joined, "--pipe") {
		t.Errorf("the probe does not wait for its own output:\n%s", joined)
	}
	// Every path must be a separate argv element, because a path is one
	// argument and a shell is not involved.
	for _, p := range sb.Paths {
		if !containsExact(args, p) {
			t.Errorf("%s is not a separate argv element: %v", p, args)
		}
	}
}

// TestTheProbeRunsTheInstalledBinaryAndNotThisOne.
//
// The sandbox's ProtectSystem= and NoNewPrivileges= apply to the file that is
// executed. Probing with a binary in an operator's home directory would test a
// path the unit never takes.
func TestTheProbeRunsTheInstalledBinaryAndNotThisOne(t *testing.T) {
	args, err := ProbeSpec{Instance: "pz-main", User: "pzserver", Sandbox: pzSandbox(t)}.Args()
	if err != nil {
		t.Fatal(err)
	}
	if !containsExact(args, ProbeBinary) {
		t.Errorf("the probe does not run %s: %v", ProbeBinary, args)
	}
}

// TestTheProbeRefusesToRunAsRoot.
//
// root writes through most of these directives, so a probe as root would pass
// on exactly the hosts where the real account fails.
func TestTheProbeRefusesToRunAsRoot(t *testing.T) {
	_, err := ProbeSpec{Instance: "pz-main", Sandbox: pzSandbox(t)}.Args()
	if err == nil {
		t.Fatal("a probe with no account was built")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("the refusal does not explain why an account is required: %v", err)
	}
}

// TestABlameNamesTheDirectiveAndNotJustThePermission.
//
// Inside the namespace, a directory hidden by ProtectHome and a directory that
// was never there produce the same errno. The parent built the sandbox and can
// say which directive covers the path, which is the difference between a
// five-minute fix and an afternoon.
func TestABlameNamesTheDirectiveAndNotJustThePermission(t *testing.T) {
	hidden := Sandbox{ProtectHome: "read-only", ProtectSystem: "full",
		Paths: []string{"/opt/pzserver", "/home/pzserver/Zomboid"}}
	if got := Blame(hidden, "/home/pzserver/Zomboid"); got != "ProtectHome=read-only" {
		t.Errorf("Blame = %q, want the directive that hides /home", got)
	}

	strict := Sandbox{ProtectHome: "no", ProtectSystem: "strict", Paths: []string{"/opt/pzserver"}}
	if got := Blame(strict, "/var/log/gvm/pz-main"); !strings.Contains(got, "ProtectSystem=strict") {
		t.Errorf("Blame = %q, want the directive that makes the filesystem read-only", got)
	}

	// Everything the sandbox governs permits the write, so the honest answer
	// is ownership -- and saying so sends the operator to chown rather than to
	// the unit file.
	fine := Sandbox{ProtectHome: "no", ProtectSystem: "full", Paths: []string{"/opt/pzserver"}}
	if got := Blame(fine, "/opt/pzserver"); !strings.Contains(got, "owner and mode") {
		t.Errorf("Blame = %q, want a pointer at ownership", got)
	}
}

// TestProbeOutputParsesTheShapesItWillActuallySee.
func TestProbeOutputParsesTheShapesItWillActuallySee(t *testing.T) {
	in := `{"path":"/opt/pzserver","ok":true}
{"path":"/home/pzserver/Zomboid","ok":false,"error":"open /home/pzserver/Zomboid/.gvm-probe-9: read-only file system"}

{"path":"/var/log/gvm/pz-main","ok":true}
`
	got, err := ParseProbeResults(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d results, want 3: %+v", len(got), got)
	}
	if got[1].OK || !strings.Contains(got[1].Error, "read-only file system") {
		t.Errorf("the refused path lost its reason: %+v", got[1])
	}
}

// TestUnparseableProbeOutputIsAnErrorAndNotASkippedLine.
//
// The output of the probe is the evidence the apply proceeds on. Quietly
// ignoring the half that did not parse is how an apply proceeds on evidence it
// does not have.
func TestUnparseableProbeOutputIsAnErrorAndNotASkippedLine(t *testing.T) {
	for _, in := range []string{
		"{\"path\":\"/opt/pz\",\"ok\":true}\nWarning: something\n",
		"{\"ok\":true}\n",
	} {
		if _, err := ParseProbeResults(strings.NewReader(in)); err == nil {
			t.Errorf("ParseProbeResults(%q) accepted output it could not read", in)
		}
	}
}

// TestSilenceIsNotApproval.
func TestSilenceIsNotApproval(t *testing.T) {
	want := []string{"/a", "/b", "/c"}
	missing := MissingProbePaths(want, []ProbeResult{{Path: "/a", OK: true}})
	if strings.Join(missing, ",") != "/b,/c" {
		t.Errorf("MissingProbePaths = %v, want /b and /c", missing)
	}
}

// TestProbeErrorReportsEveryRefusalAtOnce.
//
// An operator who has to widen a sandbox wants to widen it once. Reporting the
// first failure per apply turns a three-path problem into three applies.
func TestProbeErrorReportsEveryRefusalAtOnce(t *testing.T) {
	sb := Sandbox{ProtectHome: "read-only", ProtectSystem: "full", Paths: []string{"/opt/pz"}}
	err := ProbeError(sb, []ProbeResult{
		{Path: "/opt/pz", OK: true},
		{Path: "/home/a", OK: false, Error: "read-only file system"},
		{Path: "/home/b", OK: false, Error: "read-only file system"},
	})
	if err == nil {
		t.Fatal("two refused paths produced no error")
	}
	if !errors.Is(err, ErrProbe) {
		t.Errorf("the error is not an ErrProbe: %v", err)
	}
	for _, want := range []string{"/home/a", "/home/b", "ProtectHome=read-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message omits %q:\n%v", want, err)
		}
	}
	if ProbeError(sb, []ProbeResult{{Path: "/opt/pz", OK: true}}) != nil {
		t.Error("an all-clear probe produced an error")
	}
}

// TestProbePropertiesRefuseAValueThatWouldSplitTheLine.
//
// systemd-run's --property= takes one string, and a path with a space in it
// would become two ReadWritePaths inside the transient unit exactly as it
// would inside the drop-in. Refusing in both places, with the same function,
// is what keeps the two from disagreeing.
func TestProbePropertiesRefuseAValueThatWouldSplitTheLine(t *testing.T) {
	sb := Sandbox{ProtectHome: "no", ProtectSystem: "full", Paths: []string{"/srv/gvm/x/my worlds"}}
	if _, err := (ProbeSpec{Instance: "pz-main", User: "pzserver", Sandbox: sb}).Args(); err == nil {
		t.Fatal("a path with a space became a probe property")
	}
}

func containsExact(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestTrimStderrKeepsTheComplaintReadable.
//
// systemd-run's own failures are multi-line ("Failed to start transient service
// unit" then "See system logs..."), and they arrive in an error string an
// operator reads in one line of terminal output. This test exists because the
// function used to sit behind //go:build linux, where the development machine
// never compiled it and nothing here could exercise it.
func TestTrimStderrKeepsTheComplaintReadable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a trailing newline", "Failed to start transient service unit.\n",
			"Failed to start transient service unit."},
		{"systemd-run's two lines",
			"Failed to start transient service unit: Unit not found.\nSee system logs for details.\n",
			"Failed to start transient service unit: Unit not found. See system logs for details."},
		{"runs of mixed whitespace", "a \t\r\n  b", "a b"},
		{"nothing but whitespace", " \n\t\r\n ", ""},
		{"already one line", "unit failed", "unit failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimStderr(tc.in); got != tc.want {
				t.Errorf("trimStderr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// A wall of output is cut, because the whole point is that this ends up
	// inside an error string somebody reads.
	long := trimStderr(strings.Repeat("x y ", 400))
	if len(long) != 403 || !strings.HasSuffix(long, "...") {
		t.Errorf("a 1600-byte complaint came back %d bytes ending %q", len(long), long[max(0, len(long)-3):])
	}
}
