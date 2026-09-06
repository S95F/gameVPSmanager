package lifecycle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lifecycle runs no programs, and writes nothing. This file is the assertion.
//
// Why each half matters.
//
// NO PROGRAMS. Everything this package decides is decided from an instance
// name, and the obvious shortcut for half of it is a subprocess: `systemctl
// show gvm@$name`, `pgrep -f`, `ps -o lstart= -p $pid`. Each is wrong in the
// same four ways -- the command string is assembled from an operator-supplied
// name so a quoting bug becomes a root shell; the program is resolved through
// PATH so what answers depends on an inherited environment; systemctl's exit
// codes conflate "the unit failed" with "the unit does not exist", which is
// precisely the distinction the phase table is built on; and a subprocess that
// hangs hangs `gvm status` with no timeout. Everything here comes from
// internal/platform's typed seam instead.
//
// NO WRITES, except through hostops. observe.go reads /etc, /run and
// /var/lib through the Files seam, and the only file this package can WRITE is
// the hold, through hostops' declared file.write. A lifecycle package with its
// own os.WriteFile would put a privileged write outside the closed verb table,
// which is the one thing that table exists for. The `os` import is confined to
// osfiles.go, which is three read methods and nothing else.
//
// There is no exception list. Unlike internal/reconcile, which legitimately
// runs systemd-analyze and systemd-run, nothing here needs to run anything at
// all.

// fsAllowed is the complete list of files that may import `os`, and why.
var fsAllowed = map[string]string{
	"osfiles.go": "the production Files: three read methods over the real filesystem, and no writer",
}

var forbiddenImports = map[string]string{
	"os/exec":               "running a program",
	"os":                    "touching the filesystem outside the Files seam",
	"syscall":               "raw syscalls belong in internal/platform behind a build tag",
	"golang.org/x/sys/unix": "a Linux-only import would break the cross-platform build",
	"net/http":              "nothing that decides a phase may depend on the network",
	"database/sql":          "the sweep enumerates from /etc/gvm and never from Postgres (section 1)",
}

var forbiddenTokens = []struct {
	token string
	why   string
}{
	{"exec.LookPath", "a PATH lookup deciding which binary runs as root"},
	{"/bin/sh", "a shell interpreter"},
	{"/bin/bash", "a shell interpreter"},
	{`"sh"`, "a shell interpreter"},
	{`"-c"`, "the flag that turns an argv into a command string"},
	{`"systemctl"`, "systemd is driven through hostops over D-Bus, never by a subprocess"},
	{`"pgrep"`, "process liveness is platform.Alive, which pins start_ticks against pid reuse"},
	{"os.Getenv", "an inherited environment is one more thing that decides what a root process does"},
	{"os.WriteFile", "the write path is hostops.Do and nothing else"},
	{"os.Remove", "the write path is hostops.Do and nothing else"},
	{"os.MkdirAll", "the write path is hostops.Do and nothing else"},
	{"time.Sleep", "a sweep that slept would hold a lock or a terminal for a duration nobody chose"},
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
	if len(out) < 8 {
		t.Fatalf("scanned only %d source files; the walk is broken", len(out))
	}
	for _, want := range []string{
		"phase.go", "observe.go", "derive.go", "lease.go", "reconcile.go",
		"verbs.go", "manager.go", "status.go", "osfiles.go",
	} {
		if _, ok := out[want]; !ok {
			t.Fatalf("%s was not scanned", want)
		}
	}
	return out
}

func TestLifecycleRunsNoProgramsAndWritesNothingItself(t *testing.T) {
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
			if path == "os" {
				if _, ok := fsAllowed[name]; ok {
					continue
				}
			}
			t.Errorf("%s imports %q (%s)", name, path, why)
		}
	}
}

func TestLifecycleNamesNoShellAndNoExternalTool(t *testing.T) {
	for name, src := range packageSources(t) {
		for i, line := range strings.Split(string(src), "\n") {
			// Comments are where these names legitimately appear, explaining
			// why the code does not use them. The hint strings are the other
			// place: `systemctl unmask gvm@pz-main.service` is a sentence gvm
			// prints for an operator to type, not a command gvm runs.
			code, _, _ := strings.Cut(line, "//")
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, f := range forbiddenTokens {
				if !strings.Contains(code, f.token) {
					continue
				}
				if f.token == `"systemctl"` || strings.HasPrefix(f.token, "/bin/") {
					// Only a bare literal is a program; `"systemctl unmask " +
					// unit` is a hint.
					if !strings.Contains(code, f.token+")") && !strings.Contains(code, f.token+",") {
						continue
					}
				}
				t.Errorf("%s:%d contains %q (%s):\n\t%s", name, i+1, f.token, f.why,
					strings.TrimSpace(line))
			}
		}
	}
}

// TestNoAbsoluteProgramPathAppears. The static import check cannot catch a
// path added to a candidate list in a file that is already allowed to read the
// filesystem, and there is no file here that may run one at all.
func TestNoAbsoluteProgramPathAppears(t *testing.T) {
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
				if strings.HasPrefix(s, p) {
					t.Errorf("%s:%d names %q, which is a program nobody declared",
						name, fset.Position(lit.Pos()).Line, s)
				}
			}
			return true
		})
	}
}

// TestTheReconcilerHoldsNoMutatingSurface is the structural half of "the
// reconciler never actuates", read out of the source.
//
// TestReconcilerNeverActuates proves no start happened during one sweep.
// This proves there is nothing on the type to call: Reconciler's fields are an
// Observer (whose systemd is a UnitReader, which has no Start), a Mirror and a
// logger. A field of type platform.Systemd or lifecycle.Host would fail here,
// and testdata/reconciler-actuates fails to compile for the same reason.
func TestTheReconcilerHoldsNoMutatingSurface(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "reconcile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"platform.Systemd", "Host", "hostops.Request", "ShutdownFunc", "Manager"}
	var checked bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Reconciler" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		checked = true
		for _, field := range st.Fields.List {
			typ := types(fset, field.Type)
			if slices.Contains(banned, typ) {
				t.Errorf("Reconciler has a field of type %s. The sweep would then be able to "+
					"start, stop or write, and something that can eventually does", typ)
			}
		}
		return false
	})
	if !checked {
		t.Fatal("the Reconciler struct was not found; this test no longer checks anything")
	}
}

func types(fset *token.FileSet, e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return types(fset, t.X)
	case *ast.SelectorExpr:
		return types(fset, t.X) + "." + t.Sel.Name
	}
	return ""
}
