package hostops

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// hostops runs two programs and no shell. This file is the assertion.
//
// Why it needs one. Every line in this package executes as root, and the
// obvious shortcut for half of it is a shell: `systemctl restart gvm@%s`,
// `chown -R`, `install -o root -m 0640`. Each of those is wrong in the same
// four ways. The command string is assembled from an instance name, a unit
// name and a path, so a quoting bug becomes a root shell rather than a
// mis-rendered message. The program is resolved through PATH, so what answers
// depends on an environment gvm inherited from systemd. The output has to be
// parsed back, and systemctl's exit codes conflate "the unit failed" with "the
// unit does not exist". And a subprocess that hangs -- on a name lookup, on a
// dbus timeout -- hangs a root daemon that has no timeout for it.
//
// The two exceptions are useradd and groupadd. There is no library for
// /etc/passwd worth writing, and writing that file directly is far worse than
// exec'ing the tool that locks it. Both are invoked as argv with an absolute
// path this package resolved by stat, in a file that only builds on Linux.

// execAllowed is the one file that may import os/exec, and the reason it may.
var execAllowed = map[string]string{
	"user_linux.go": "shadow-utils is the only correct way to add a line to /etc/passwd",
}

var forbiddenImports = map[string]string{
	"os/exec":               "running a program",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"exec.LookPath", "a PATH lookup deciding which binary runs as root"},
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
	{`"sh"`, "a shell interpreter"},
	{`"bash"`, "a shell interpreter"},
	{`"-c"`, "the flag that turns an argv into a command string"},
	{`"systemctl"`, "systemd is driven over D-Bus; systemctl's exit codes conflate failure with absence"},
	{`"chown"`, "os.Chown takes numbers and cannot be quoted wrong"},
	{`"install"`, "the atomic write is spec.WriteFileAtomic, whose crash ordering is pinned by a test"},
	{"os.Getenv", "an inherited environment is one more thing that decides what a root process does"},
}

// programLiterals is every absolute program path this package is allowed to
// name. Anything else appearing as a string literal is a new program running
// as root, which is a decision that belongs in a review and not in a diff
// nobody looked at twice.
var programLiterals = []string{
	"/usr/sbin/useradd", "/sbin/useradd",
	"/usr/sbin/groupadd", "/sbin/groupadd",
	"/usr/bin/gpasswd", "/bin/gpasswd", "/usr/sbin/gpasswd",
	"/usr/sbin/nologin", "/sbin/nologin",
}

func packageSources(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	// A walk that found nothing would pass silently forever.
	if len(out) < 7 {
		t.Fatalf("scanned only %d source files; the walk is broken", len(out))
	}
	for _, want := range []string{"verbs.go", "ops.go", "fs.go", "user.go", "user_linux.go", "systemd_linux.go"} {
		if _, ok := out[want]; !ok {
			t.Fatalf("%s was not scanned", want)
		}
	}
	return out
}

func TestHostopsRunsNothingButTheAccountTools(t *testing.T) {
	fset := token.NewFileSet()
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			why, bad := forbiddenImports[path]
			if !bad {
				continue
			}
			if path == "os/exec" {
				if _, ok := execAllowed[name]; ok {
					continue
				}
				allowed := make([]string, 0, len(execAllowed))
				for f := range execAllowed {
					allowed = append(allowed, f)
				}
				slices.Sort(allowed)
				t.Errorf("%s imports os/exec; only %v may, and only for shadow-utils",
					name, allowed)
				continue
			}
			t.Errorf("%s imports %q (%s)", name, path, why)
		}
	}
}

func TestHostopsNamesNoShellAndNoExternalTool(t *testing.T) {
	for name, src := range packageSources(t) {
		for i, line := range strings.Split(string(src), "\n") {
			// Comments are where these names legitimately appear, explaining
			// why the code does not use them.
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range forbiddenTokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestEveryProgramPathIsDeclared catches the case the import check cannot: a
// new absolute path added to an existing candidate list, in the file that is
// already allowed to exec.
func TestEveryProgramPathIsDeclared(t *testing.T) {
	fset := token.NewFileSet()
	prefixes := []string{"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/local/bin/", "/usr/local/sbin/"}
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, p := range prefixes {
				if !strings.HasPrefix(s, p) {
					continue
				}
				if slices.Contains(programLiterals, s) {
					return true
				}
				t.Errorf("%s:%d names %q, which is a program running as root that "+
					"nobody declared", name, fset.Position(lit.Pos()).Line, s)
			}
			return true
		})
	}
}

// TestTheCandidateListsAreAbsoluteAndUnmodifying is the runtime half: the
// static scan proves no new literal appeared, this proves the ones that are
// there are what they claim to be.
func TestTheCandidateListsAreAbsoluteAndUnmodifying(t *testing.T) {
	for _, list := range [][]string{useraddPaths, groupaddPaths, gpasswdPaths, nologinPaths} {
		if len(list) == 0 {
			t.Fatal("an empty candidate list would resolve to nothing and fail with ErrNoTool forever")
		}
		for _, p := range list {
			if !strings.HasPrefix(p, "/") {
				t.Errorf("%q is not absolute; PATH would decide what runs as root", p)
			}
			base := p[strings.LastIndex(p, "/")+1:]
			if !slices.Contains([]string{"useradd", "groupadd", "gpasswd", "nologin"}, base) {
				t.Errorf("%q is not one of the account tools this package may run", p)
			}
		}
	}
}

// TestTheHappyPathRunsNoProgramAtAll is the behavioural half.
//
// Every verb except user.ensure is D-Bus, os and spec.WriteFileAtomic. An
// apply against a host whose account already exists -- which is every apply
// after the first, and every apply for an adopted instance -- executes nothing.
func TestTheHappyPathRunsNoProgramAtAll(t *testing.T) {
	f := newFakeTools()
	// The account already exists, which is every apply after the first and
	// every apply for an adopted instance.
	f.users["gvm-pz-main"] = platform.User{Name: "gvm-pz-main", UID: 991, GID: 991}
	su := f.users_()

	fsys := newFakeFS(controlPlaneDirs()...)
	o, _ := testOps(t, fsys, newFakeSystemd(), su)
	ctx := context.Background()

	for _, req := range []Request{
		{Verb: VerbFileWrite, Actor: rootActor(), Instance: "pz-main", File: FileDropIn, Body: []byte("[Service]\n")},
		{Verb: VerbDirEnsure, Actor: rootActor(), Instance: "pz-main", Dir: DirState},
		{Verb: VerbDaemonReload, Actor: rootActor(), Instance: "pz-main"},
		{Verb: VerbUnitEnable, Actor: rootActor(), Instance: "pz-main"},
		{Verb: VerbUnitStart, Actor: rootActor(), Instance: "pz-main"},
		{Verb: VerbUserEnsure, Actor: rootActor(), Instance: "pz-main"},
	} {
		if _, err := o.Do(ctx, req); err != nil {
			t.Fatalf("%s: %v", req.Verb, err)
		}
	}
	if len(f.ran) != 0 {
		t.Fatalf("an apply against an existing host ran %v", f.ran)
	}
}
