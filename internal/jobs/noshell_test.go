package jobs

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The job layer ORCHESTRATES. Third-party code runs through RunSandboxed.
//
// noscope_test.go asserts the two things the sandbox itself must never do: no
// --scope, no shell. This file asserts the division of labour that makes those
// assertions worth having, and it is a different claim.
//
// os/exec is legitimate in this package -- it is the D-16 seam's
// implementation -- but it is legitimate in exactly ONE file. The engine, the
// step tables, the guard, the lock and the slug resolver are root-privileged
// orchestration, and a single exec.Command anywhere in them would be
// third-party code running as root with no unit, no uid, no slice and no
// RuntimeMaxSec around it. That is the regression D-16 names, and it would
// walk straight past noscope_test.go: a correctly spelled, scope-free,
// shell-free exec.Command is still a root spawn.
//
// So: os/exec may be imported by sandbox_linux.go and by nothing else here.
// The module-level import-graph assertion in internal/platform/deps_test.go is
// the other half -- it is what stops internal/hook or the SteamCMD driver from
// reaching RunSandboxed indirectly, which no source scan of either package
// could see.

// execFiles are the files allowed to start a process.
//
// sandbox_linux.go is the implementation. sandbox_stub.go is on the list
// because the development machine's build has to be checked by the same rule,
// and it is expected to contain no exec at all -- which the second assertion
// below confirms rather than assumes.
var execFiles = map[string]bool{
	"sandbox_linux.go": true,
	"sandbox_stub.go":  true,
}

// spawnImports are the import paths that can start a process.
//
// An import gate rather than a token scan, because a token scan reads code
// text and can be defeated by a spelling it has not heard of, while an import
// cannot be hidden. The tokens in noscope_test.go are the second line of
// defence for the names that need no new import (os.StartProcess).
var spawnImports = map[string]string{
	"os/exec": "the orchestration half of this package runs as root; a process started from " +
		"it has no transient unit, no --uid, no slice and no RuntimeMaxSec around it (D-16)",
	"syscall": "raw syscalls belong in internal/platform behind a build tag",
}

func TestOnlyTheSandboxCanStartAProcess(t *testing.T) {
	fset := token.NewFileSet()
	var scanned, withExec int
	for name, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		scanned++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			why, spawner := spawnImports[path]
			if !spawner {
				continue
			}
			if !execFiles[name] {
				t.Errorf("%s imports %q (%s); third-party code runs through RunSandboxed",
					name, path, why)
				continue
			}
			if path == "os/exec" {
				withExec++
			}
		}
	}
	if scanned < 10 {
		t.Fatalf("parsed only %d source files; the walk is broken and every assertion above "+
			"is vacuous", scanned)
	}
	// The other half, so the test cannot pass for the wrong reason. On Linux
	// exactly one file imports os/exec; off Linux none does, because the stub
	// refuses instead of running anything.
	if withExec > 1 {
		t.Errorf("%d files import os/exec; the seam is supposed to be one file", withExec)
	}
}

// TestTheSandboxSeamStillExists: if RunSandboxed stopped being how a process
// is started, the assertion above would be about nothing.
func TestTheSandboxSeamStillExists(t *testing.T) {
	src := packageSources(t)
	var found bool
	for name, b := range src {
		if !strings.HasPrefix(name, "sandbox_") {
			continue
		}
		if strings.Contains(string(b), "func RunSandboxed(") {
			found = true
		}
	}
	if !found {
		t.Fatal("no sandbox_*.go declares RunSandboxed; the exec gate above proves nothing")
	}
	// And the step tables reach it the way the design says: through the
	// steamcmd Runner, which is the only caller in this package.
	steam, ok := src["steam.go"]
	if !ok {
		t.Fatal("steam.go is gone; it is the one caller of RunSandboxed here")
	}
	if !strings.Contains(string(steam), "RunSandboxed(ctx, s, cmd, out)") {
		t.Error("steam.go no longer routes through RunSandboxed")
	}
}

// TestTheOrchestrationFilesRunNoShell.
//
// Every command this package starts is a []string handed to systemd-run, so a
// path containing a semicolon is one argv element rather than a second
// command. The orchestration files start nothing at all.
//
// The import gate above is the authoritative control; this is the second line
// of defence, for the spellings that need no new import once `os` is in scope.
// It deliberately does NOT look for the legacy scripts' names: the step tables
// quote update.sh's own lines in their diagnostics on purpose -- "an outdated
// server beats one left stopped" is the operator's reasoning and belongs in
// the message -- and a scan that banned the words would ban the explanation
// rather than the behaviour. Calling one of those scripts needs os/exec, which
// the import gate refuses outright.
func TestTheOrchestrationFilesRunNoShell(t *testing.T) {
	shellTokens := []struct{ token, why string }{
		{"exec.Command", "the step tables orchestrate; they do not spawn"},
		{"exec.CommandContext", "the step tables orchestrate; they do not spawn"},
		{"/bin/sh", "there is no shell in the orchestration path"},
		{"/bin/bash", "there is no shell in the orchestration path"},
		{"/usr/bin/env", "env is a program that runs another program"},
	}
	var scanned int
	for name, b := range packageSources(t) {
		if execFiles[name] {
			continue
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			// Comments are where these names legitimately appear: the step
			// tables quote update.sh's own lines to explain why a step is
			// non-fatal.
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range shellTokens {
				if strings.Contains(code, f.token) {
					t.Errorf("%s:%d contains %q (%s):\n\t%s",
						name, i+1, f.token, f.why, strings.TrimSpace(code))
				}
			}
		}
	}
	if scanned < 9 {
		t.Fatalf("scanned only %d orchestration files; the walk is broken", scanned)
	}
}

// TestEveryStepIsWiredToASeam.
//
// The job layer decides WHAT happens and in what order; the packages behind
// the seams decide HOW. A step whose Do called into the host directly would be
// a step the never-leaves-stopped test cannot inject a failure into -- which
// is the same thing as a step whose failure nobody designed. Running every
// kind's table with every seam present and asserting that each step's work
// reached a seam is how that stays true.
func TestEveryStepIsWiredToASeam(t *testing.T) {
	for _, kind := range Kinds() {
		r, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		name := testInstance
		if !r.Scope.Accepts(name) {
			name = ""
		}
		rig := newRig(t, kind, name)
		steps, err := StepNames(rig.env)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if _, err := Execute(context.Background(), rig.env); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		reached := map[string]bool{}
		for _, op := range rig.inj.ops() {
			reached[op] = true
		}
		// The decision steps are pure and reach nothing, by design: they are
		// the gate on the near side of everything expensive.
		pure := map[string]bool{"update.decide": true, "content.decide": true}
		for _, s := range steps {
			if pure[s] || reached[s] {
				continue
			}
			// A stop or a start reaches the Supervisor under its own name.
			if strings.HasSuffix(s, ".stop") || strings.HasSuffix(s, "_stop") {
				if reached["super.stop"] {
					continue
				}
			}
			if strings.HasSuffix(s, ".start") || strings.HasSuffix(s, "_start") {
				if reached["super.start"] {
					continue
				}
			}
			t.Errorf("%s step %q reached no seam; a step whose failure cannot be injected is "+
				"a step whose failure nobody designed", kind, s)
		}
	}
}

// packageSources is this package's non-test Go files.
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
	return out
}
