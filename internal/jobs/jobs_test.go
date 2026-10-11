package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// steamExample is section 7.7's worked SteamCMD unit, as a value.
//
// The whole argv test is written against the documented example rather than
// against something convenient, so that a future edit which quietly drops a
// hardening directive has to disagree with the architecture document to pass.
func steamExample() Sandbox {
	return Sandbox{
		Unit:             "gvm-steam-pz-main",
		User:             "pzserver",
		Group:            "pzserver",
		Home:             "/var/lib/gvm/cache/steam/homes/pz-main",
		WorkDir:          "/opt/steamcmd",
		Env:              map[string]string{"LANG": "en_US.UTF-8"},
		ProtectSystem:    "strict",
		ProtectHome:      "no",
		PrivateTmp:       Yes(),
		NoNewPrivileges:  Yes(),
		RestrictSUIDSGID: Yes(),
		MemoryMax:        "2G",
		LimitNOFILE:      8192,
		ReadWritePaths: []string{
			"/opt/pzserver",
			"/var/lib/gvm/cache/steam/homes/pz-main",
		},
		Timeout: 30 * time.Minute,
	}
}

func TestArgsRendersTheDocumentedUnit(t *testing.T) {
	args, err := steamExample().Args([]string{
		"/opt/steamcmd/steamcmd.sh", "+runscript", "/var/lib/gvm/instances/pz-main/steam/update.txt",
	})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--collect", "--wait", "--pipe", "--quiet",
		"--unit=gvm-steam-pz-main",
		"--slice=gvm-jobs.slice",
		"--uid=pzserver",
		"--gid=pzserver",
		"--property=Type=exec",
		"--setenv=HOME=/var/lib/gvm/cache/steam/homes/pz-main",
		"--setenv=LANG=en_US.UTF-8",
		"--property=WorkingDirectory=/opt/steamcmd",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=no",
		"--property=PrivateTmp=yes",
		"--property=NoNewPrivileges=yes",
		"--property=RestrictSUIDSGID=yes",
		"--property=MemoryMax=2G",
		"--property=LimitNOFILE=8192",
		"--property=RuntimeMaxSec=1800",
		"--property=ReadWritePaths=/opt/pzserver /var/lib/gvm/cache/steam/homes/pz-main",
	} {
		if !contains(args, want) {
			t.Errorf("argv is missing %q\ngot: %s", want, joined)
		}
	}

	// The command and its arguments come last and untouched, as separate argv
	// elements. One string would be a shell invocation wearing an exec's name.
	tail := args[len(args)-3:]
	if tail[0] != "/opt/steamcmd/steamcmd.sh" || tail[1] != "+runscript" {
		t.Errorf("the command is not the tail of the argv: %v", tail)
	}
}

// TestArgsIsNeverAScope is D-16 as an assertion.
//
// A scope runs in the caller's cgroup and unit context: it takes no meaningful
// hardening properties and inherits root's capabilities, which would leave
// SteamCMD less confined than the game it installs. The flag must never appear,
// for any sandbox this package can build.
func TestArgsIsNeverAScope(t *testing.T) {
	for name, sb := range map[string]Sandbox{"steam": steamExample(), "fetch": fetchExample(t)} {
		args, err := sb.Args([]string{"/usr/local/bin/gvm", "content-fetch"})
		if err != nil {
			t.Fatalf("%s: Args: %v", name, err)
		}
		for _, a := range args {
			if strings.Contains(a, "--scope") {
				t.Errorf("%s: argv contains %q; third-party code runs in a transient SERVICE "+
					"(D-16)", name, a)
			}
		}
		if !contains(args, "--unit="+sb.Unit) {
			t.Errorf("%s: a transient service has to be named so `systemctl status` can find "+
				"it while it runs", name)
		}
	}
}

func fetchExample(t *testing.T) Sandbox {
	t.Helper()
	sb, err := FetchSandbox(FetchInput{
		JobID:   "a1b2c3",
		User:    "gvm-fetch",
		Group:   "gvm-fetch",
		BlobDir: "/var/lib/gvm/blobs",
		Staging: "/var/lib/gvm/blobs/tmp",
		BindReadOnly: []string{
			"/var/lib/gvm/downloads",
		},
		Credentials: map[string]string{"curseforge": "/etc/gvm/secrets/host/curseforge"},
	})
	if err != nil {
		t.Fatalf("FetchSandbox: %v", err)
	}
	return sb
}

// TestTheFetchUnitCarriesTheFilterTheSteamUnitCannot records the asymmetry
// deliberately, because the obvious review comment on these two sandboxes is
// "why are they different".
func TestTheFetchUnitCarriesTheFilterTheSteamUnitCannot(t *testing.T) {
	args, err := fetchExample(t).Args([]string{"/usr/local/bin/gvm", "content-fetch"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--uid=gvm-fetch",
		"--property=ProtectHome=yes",
		"--property=RestrictAddressFamilies=AF_INET AF_INET6",
		"--property=SystemCallFilter=@system-service",
		"--property=SystemCallFilter=~@privileged @resources",
		"--property=TasksMax=64",
		"--property=MemoryMax=1G",
		"--property=MemorySwapMax=0",
		"--property=ReadWritePaths=/var/lib/gvm/blobs /var/lib/gvm/blobs/tmp",
		"--property=BindReadOnlyPaths=/var/lib/gvm/downloads",
		"--property=LoadCredential=curseforge:/etc/gvm/secrets/host/curseforge",
	} {
		if !contains(args, want) {
			t.Errorf("fetch argv is missing %q\ngot: %v", want, args)
		}
	}

	// The two syscall directives have to stay two directives, in order: an
	// allow list followed by a deny list. Joined into one they would be a
	// filter that means neither.
	allow, deny := -1, -1
	for i, a := range args {
		switch a {
		case "--property=SystemCallFilter=@system-service":
			allow = i
		case "--property=SystemCallFilter=~@privileged @resources":
			deny = i
		}
	}
	if allow < 0 || deny < 0 || deny < allow {
		t.Errorf("the syscall allow list must precede the deny list; got allow=%d deny=%d",
			allow, deny)
	}

	// No instance path anywhere. A zip-slip in a modpack has to land in a uid
	// that owns nothing but a cache.
	for _, a := range args {
		if strings.Contains(a, "/opt/pzserver") || strings.Contains(a, "/srv/gvm") {
			t.Errorf("the fetch unit can reach an instance tree: %q", a)
		}
	}
}

func TestValidateRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Sandbox)
		want string
	}{
		{"root by name", func(s *Sandbox) { s.User = "root" }, "as root"},
		{"root by number", func(s *Sandbox) { s.User = "0" }, "as root"},
		{"no account", func(s *Sandbox) { s.User = "" }, "no account"},
		{"root group", func(s *Sandbox) { s.Group = "root" }, "root group"},
		{"no group", func(s *Sandbox) { s.Group = "  " }, "no group"},
		{"no budget", func(s *Sandbox) { s.Timeout = 0 }, "RuntimeMaxSec"},
		{"negative budget", func(s *Sandbox) { s.Timeout = -time.Second }, "RuntimeMaxSec"},
		{"unset ProtectHome", func(s *Sandbox) { s.ProtectHome = "" }, "ProtectHome"},
		{"bogus ProtectHome", func(s *Sandbox) { s.ProtectHome = "maybe" }, "ProtectHome"},
		{"unset ProtectSystem", func(s *Sandbox) { s.ProtectSystem = "" }, "ProtectSystem"},
		{"nil PrivateTmp", func(s *Sandbox) { s.PrivateTmp = nil }, "PrivateTmp is unset"},
		{"nil NoNewPrivileges", func(s *Sandbox) { s.NoNewPrivileges = nil }, "NoNewPrivileges is unset"},
		{"nil RestrictSUIDSGID", func(s *Sandbox) { s.RestrictSUIDSGID = nil }, "RestrictSUIDSGID is unset"},
		{"no writable paths", func(s *Sandbox) { s.ReadWritePaths = nil }, "no ReadWritePaths"},
		{"no HOME", func(s *Sandbox) { s.Home = "" }, "no HOME"},
		{"HOME twice", func(s *Sandbox) { s.Env = map[string]string{"HOME": "/elsewhere"} }, "HOME is in both"},
		{"no unit", func(s *Sandbox) { s.Unit = "" }, "no unit name"},
		{"unit with a path", func(s *Sandbox) { s.Unit = "../other" }, "path element"},
		{"unit with a type", func(s *Sandbox) { s.Unit = "gvm-steam.timer" }, "contains a dot"},
		{"unit with a newline", func(s *Sandbox) { s.Unit = "gvm\nExecStart=/bin/sh" }, "second directive"},
		{"relative credential", func(s *Sandbox) {
			s.Credentials = map[string]string{"cf": "secrets/cf"}
		}, "not an absolute path"},
		{"credential with a colon", func(s *Sandbox) {
			s.Credentials = map[string]string{"a:b": "/etc/gvm/secrets/host/cf"}
		}, "separator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := steamExample()
			tc.mut(&sb)
			err := sb.Validate()
			if err == nil {
				t.Fatalf("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the refusal\n got: %v\nwant substring: %q",
					err, tc.want)
			}
			// Args is the only way out of this package, so it has to refuse
			// too rather than relying on a caller having validated first.
			if _, aerr := sb.Args([]string{"/bin/true"}); aerr == nil {
				t.Error("Args built an argv for a sandbox Validate refused")
			}
		})
	}
}

func TestArgsRefusesUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Sandbox)
		cmd  []string
		want string
	}{
		{"relative writable path", func(s *Sandbox) {
			s.ReadWritePaths = []string{"opt/pzserver"}
		}, nil, "not absolute"},
		{"space in a writable path", func(s *Sandbox) {
			s.ReadWritePaths = []string{"/opt/pz server"}
		}, nil, "separates this directive's paths"},
		{"newline in a writable path", func(s *Sandbox) {
			s.ReadWritePaths = []string{"/opt/pz\nProtectSystem=no"}
		}, nil, "second directive"},
		{"newline in an env value", func(s *Sandbox) {
			s.Env = map[string]string{"LANG": "C\nExecStartPre=/bin/sh"}
		}, nil, "second directive"},
		{"space in an env name", func(s *Sandbox) {
			s.Env = map[string]string{"LA NG": "C"}
		}, nil, "is not a name"},
		{"relative binary", nil, []string{"steamcmd.sh"}, "inherited PATH"},
		{"no command", nil, []string{}, "no command to run"},
		{"newline in an argument", nil,
			[]string{"/opt/steamcmd/steamcmd.sh", "+login\nExecStopPost=/bin/sh"}, "second directive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := steamExample()
			if tc.mut != nil {
				tc.mut(&sb)
			}
			cmd := tc.cmd
			if cmd == nil {
				cmd = []string{"/opt/steamcmd/steamcmd.sh"}
			}
			_, err := sb.Args(cmd)
			if err == nil {
				t.Fatal("Args accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the refusal\n got: %v\nwant substring: %q",
					err, tc.want)
			}
		})
	}
}

func TestBudgetDefaultsRatherThanRunningForever(t *testing.T) {
	var s Sandbox
	if got := s.Budget(); got != DefaultTimeout {
		t.Errorf("Budget() = %v, want %v", got, DefaultTimeout)
	}
	// And Validate still refuses the zero, so the default is only ever reached
	// through a sandbox somebody filled in.
	if err := s.Validate(); err == nil {
		t.Error("the zero Sandbox validated")
	}
}

func TestParseShow(t *testing.T) {
	in := strings.Join([]string{
		"Id=gvm-steam-pz-main.service",
		"User=pzserver",
		"",
		"Environment=HOME=/var/lib/gvm/cache/steam/homes/pz-main LANG=en_US.UTF-8",
		"ReadWritePaths=/opt/pzserver /var/lib/gvm/cache/steam/homes/pz-main",
		"RuntimeMaxUSec=1800000000",
		"WorkingDirectory=",
	}, "\n")
	got, err := ParseShow(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseShow: %v", err)
	}
	// A value containing '=' survives: Environment= is exactly that shape, and
	// splitting on every '=' would turn HOME=/x into an empty value.
	if want := "HOME=/var/lib/gvm/cache/steam/homes/pz-main LANG=en_US.UTF-8"; got["Environment"] != want {
		t.Errorf("Environment = %q, want %q", got["Environment"], want)
	}
	// A property systemd reports as empty is PRESENT and empty, which is what
	// lets CheckShow distinguish "the host ignored the directive" from "not
	// asked about".
	if v, ok := got["WorkingDirectory"]; !ok || v != "" {
		t.Errorf("WorkingDirectory = %q, present=%v; an empty answer is still an answer", v, ok)
	}
	if got["RuntimeMaxUSec"] != "1800000000" {
		t.Errorf("RuntimeMaxUSec = %q", got["RuntimeMaxUSec"])
	}
}

func TestParseShowRefusesGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"not key=value": "Id=x\nsomething went wrong\n",
		"empty key":     "Id=x\n=value\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseShow(strings.NewReader(in)); err == nil {
				t.Fatal("ParseShow accepted it; the output of this command is the evidence a " +
					"sandbox is verified on, and silently dropping half of it means verifying " +
					"on evidence that was not there")
			} else if !errors.Is(err, ErrProperties) {
				t.Errorf("error does not wrap ErrProperties: %v", err)
			}
		})
	}
}

// goodShow is what a correctly configured host reports back for steamExample.
func goodShow() map[string]string {
	return map[string]string{
		"Id":               "gvm-steam-pz-main.service",
		"Slice":            "gvm-jobs.slice",
		"User":             "pzserver",
		"Group":            "pzserver",
		"ProtectSystem":    "strict",
		"ProtectHome":      "no",
		"PrivateTmp":       "yes",
		"NoNewPrivileges":  "yes",
		"RestrictSUIDSGID": "yes",
		"RuntimeMaxUSec":   "1800000000",
		"ReadWritePaths":   "/opt/pzserver /var/lib/gvm/cache/steam/homes/pz-main",
		"WorkingDirectory": "/opt/steamcmd",
		"Environment":      "HOME=/var/lib/gvm/cache/steam/homes/pz-main LANG=en_US.UTF-8",
		"MemoryMax":        "2147483648",
		"LimitNOFILE":      "8192",
	}
}

func TestCheckShowPassesACorrectUnit(t *testing.T) {
	if ms := steamExample().CheckShow(goodShow()); len(ms) != 0 {
		t.Fatalf("a correct unit was reported as wrong: %v", ms)
	}
	if err := steamExample().PropertiesError(nil); err != nil {
		t.Errorf("PropertiesError(nil) = %v, want nil", err)
	}
}

// TestCheckShowCatchesAHostThatIgnoredTheDirective is the assertion that makes
// building the argv worth anything.
//
// Every other test here proves gvm asked for the right thing. These are the
// answers a host gives when it did not do it: a scope rather than a service, no
// RuntimeMaxSec at all, root, a ProtectHome that would hide the adopted world,
// and a writable path that never made it into the unit. All of them look like
// success from the caller's side.
func TestCheckShowCatchesAHostThatIgnoredTheDirective(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"a scope, not a service", func(m map[string]string) {
			m["Id"] = "gvm-steam-pz-main.scope"
			delete(m, "User")
		}, "Id"},
		{"no runtime budget", func(m map[string]string) {
			m["RuntimeMaxUSec"] = "infinity"
		}, "RuntimeMaxUSec"},
		{"running as root", func(m map[string]string) { m["User"] = "root" }, "User"},
		{"ProtectHome ignored", func(m map[string]string) {
			m["ProtectHome"] = "read-only"
		}, "ProtectHome"},
		{"NoNewPrivileges ignored", func(m map[string]string) {
			m["NoNewPrivileges"] = "no"
		}, "NoNewPrivileges"},
		{"a writable path missing", func(m map[string]string) {
			m["ReadWritePaths"] = "/opt/pzserver"
		}, "ReadWritePaths"},
		{"the per-instance HOME missing", func(m map[string]string) {
			m["Environment"] = "LANG=en_US.UTF-8"
		}, "Environment"},
		{"a property the host does not know", func(m map[string]string) {
			m["RestrictSUIDSGID"] = ""
		}, "RestrictSUIDSGID"},
		{"no memory ceiling", func(m map[string]string) {
			m["MemoryMax"] = "infinity"
		}, "MemoryMax"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			show := goodShow()
			tc.mut(show)
			sb := steamExample()
			ms := sb.CheckShow(show)
			if len(ms) == 0 {
				t.Fatal("CheckShow approved a unit that does not carry what it was given; " +
					"nothing in this package is then measuring anything (D-16)")
			}
			var named bool
			for _, m := range ms {
				if m.Property == tc.want {
					named = true
					// A mismatch has to say what the directive was for, or an
					// operator reads a diff instead of a consequence.
					if strings.TrimSpace(m.Why) == "" {
						t.Errorf("%s has no Why", m.Property)
					}
				}
			}
			if !named {
				t.Errorf("mismatches do not name %s: %v", tc.want, ms)
			}
			err := sb.PropertiesError(ms)
			if err == nil || !errors.Is(err, ErrProperties) {
				t.Errorf("PropertiesError = %v, want an ErrProperties", err)
			}
			if !strings.Contains(err.Error(), sb.UnitName()) {
				t.Errorf("the error does not name the unit: %v", err)
			}
		})
	}
}

// TestShowPropertiesCoversWhatCheckShowJudges keeps the two halves in step: a
// property CheckShow compares but ShowProperties never asks for would be
// checked against an absent value and fail every time, or -- worse, if the
// comparison happened to expect "" -- pass without being read.
func TestShowPropertiesCoversWhatCheckShowJudges(t *testing.T) {
	sb := steamExample()
	sb.BindReadOnly = []string{"/opt/gvm/steamcmd"}
	sb.TasksMax = 64
	sb.MemorySwapMax = "0"
	asked := map[string]bool{}
	for _, p := range sb.ShowProperties() {
		asked[p] = true
	}
	// Every property CheckShow can report a mismatch for, found by feeding it
	// an empty answer and collecting the names.
	for _, m := range sb.CheckShow(map[string]string{}) {
		if !asked[m.Property] {
			t.Errorf("CheckShow judges %s but ShowProperties never asks for it", m.Property)
		}
	}
}

func TestExitErrorSaysWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		err  *ExitError
		want string
	}{
		{&ExitError{Unit: "u.service", Timeout: true}, "RuntimeMaxSec"},
		{&ExitError{Unit: "u.service", Signaled: true}, "killed by a signal"},
		{&ExitError{Unit: "u.service", Code: 7}, "status 7"},
	} {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%q does not contain %q", tc.err.Error(), tc.want)
		}
		if !strings.Contains(tc.err.Error(), "u.service") {
			t.Errorf("%q does not name the unit", tc.err.Error())
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
