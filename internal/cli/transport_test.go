package cli

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func envFor(euid int, vars map[string]string) *Env {
	return &Env{
		Getenv:  func(k string) string { return vars[k] },
		Geteuid: func() int { return euid },
	}
}

// TestResolveFollowsTheLadder is section 3.3, row by row.
func TestResolveFollowsTheLadder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		euid   int
		vars   map[string]string
		flag   string
		verb   string
		want   Mode
		errStr string
	}{
		{name: "root host op is in-process", euid: 0, verb: "stop", want: ModeLocalRoot},
		{name: "root console is the socket", euid: 0, verb: "console", want: ModeConsole},
		{
			name: "the flag selects remote even as root",
			euid: 0, verb: "stop", flag: "https://gvm.example.com",
			vars: map[string]string{EnvToken: "t"}, want: ModeRemote,
		},
		{
			name: "the environment selects remote for a non-root user",
			euid: 1000, verb: "stop",
			vars: map[string]string{EnvURL: "https://gvm.example.com", EnvToken: "t"},
			want: ModeRemote,
		},
		{
			name: "the flag beats the environment",
			euid: 1000, verb: "stop", flag: "https://one.example.com",
			vars: map[string]string{EnvURL: "https://two.example.com", EnvToken: "t"},
			want: ModeRemote,
		},
		{
			name: "a non-root host op with no remote is refused",
			euid: 1000, verb: "stop", errStr: "host operation",
		},
		{
			name: "a non-root console is refused and says why",
			euid: 1000, verb: "console", errStr: "2750",
		},
		{
			name: "a URL with no token names the command that mints one",
			euid: 1000, verb: "stop", flag: "https://gvm.example.com",
			errStr: "gvm token create",
		},
		{
			name: "plain http off-loopback is refused",
			euid: 1000, verb: "stop", flag: "http://gvm.example.com",
			vars: map[string]string{EnvToken: "t"}, errStr: "in clear",
		},
		{
			name: "plain http to loopback is a developer, and allowed",
			euid: 1000, verb: "stop", flag: "http://localhost:8080",
			vars: map[string]string{EnvToken: "t"}, want: ModeRemote,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(envFor(tc.euid, tc.vars), tc.verb, tc.flag)
			if tc.errStr != "" {
				if err == nil {
					t.Fatalf("resolved to %s, want a refusal naming %q", got.Mode, tc.errStr)
				}
				if !strings.Contains(err.Error(), tc.errStr) {
					t.Errorf("refusal = %v, want it to mention %q", err, tc.errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Mode != tc.want {
				t.Errorf("mode = %s, want %s", got.Mode, tc.want)
			}
		})
	}
}

// The refusal an unprivileged operator sees has to name BOTH fixes, because
// the two people who see it are on different machines with different problems.
func TestTheRefusalNamesBothFixes(t *testing.T) {
	_, err := Resolve(envFor(1000, nil), "apply", "")
	if err == nil {
		t.Fatal("an unprivileged apply resolved")
	}
	for _, want := range []string{"sudo gvm apply", "--remote", EnvToken, "gvm token create"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if !errors.Is(err, ErrNoTransport) {
		t.Errorf("error does not wrap ErrNoTransport: %v", err)
	}
}

// A resolved remote transport is a real answer and a build with no API client
// must say so distinctly, not fall back to doing the work locally.
func TestRemoteResolvesAndThenRefusesHonestly(t *testing.T) {
	tr, err := Resolve(envFor(0, map[string]string{EnvToken: "sekrit"}),
		"apply", "https://gvm.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if tr.URL != "https://gvm.example.com" {
		t.Errorf("URL = %q, want the trailing slash trimmed", tr.URL)
	}
	err = tr.RequireLocal()
	if !errors.Is(err, ErrRemoteUnbuilt) {
		t.Fatalf("RequireLocal = %v, want ErrRemoteUnbuilt", err)
	}
	// The token is the one thing that must never reach a diagnostic: the
	// moment a transport fails is exactly when everything about it gets
	// printed.
	if strings.Contains(err.Error(), "sekrit") {
		t.Error("the bearer token appears in the transport's own error message")
	}
	if strings.Contains(tr.Token.String(), "sekrit") {
		t.Error("the token renders as plaintext")
	}
}

// TestEveryCommandDeclaresARoute is the closure property in both directions.
//
// The Route was moved off Command (a func-valued table plus a function that
// reads it is an initialisation cycle), so the two lists are separate and this
// is what keeps them from drifting. A verb with no route silently becomes
// RouteLocal, which stops requiring root: exactly the failure worth a test.
func TestEveryCommandDeclaresARoute(t *testing.T) {
	for _, c := range commands {
		if _, ok := routes[c.Name]; !ok {
			t.Errorf("%s is dispatchable and declares no transport route", c.Name)
		}
	}
	names := map[string]bool{}
	for _, c := range commands {
		names[c.Name] = true
	}
	for verb := range routes {
		if !names[verb] {
			t.Errorf("routes declares %q, which is not a command", verb)
		}
	}
	// A hidden role must never be reachable as a host op: those verbs are
	// invoked by systemd with their own preconditions, and routing one through
	// the transport ladder would make `--remote` look like it could run one.
	for _, c := range commands {
		if c.Group == "roles" && routes[c.Name] != RouteRole {
			t.Errorf("%s is a systemd role and is routed as %v", c.Name, routes[c.Name])
		}
	}
}

// TestTheCLINeverNamesTheAdminSocket is the structural half of section 3.3's
// most important sentence.
//
// /run/gvm/admin.sock is 0660 root:gvm. A CLI that dialled it would hand the
// entire root verb surface to every member of group gvm -- which includes the
// internet-facing web tier's own account -- and that is the escalation the
// two-daemon split exists to prevent. So the path appears in exactly one file,
// as a constant used in a refusal, and this test fails if it appears anywhere
// else at all: a `socket.Dial(adminSock)` cannot be written without first
// writing the string.
func TestTheCLINeverNamesTheAdminSocket(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(v, "admin.sock") {
				return true
			}
			found++
			if f != "transport.go" {
				t.Errorf("%s:%d names the admin socket: %q", f, fset.Position(lit.Pos()).Line, v)
			}
			return true
		})
	}
	if found == 0 {
		t.Error("the admin socket constant has vanished; the refusal that names it " +
			"is what tells an operator why gvm will not use it")
	}
}
