package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestHostInitPrintManifestIsRootlessAndWritesNothing: `--print-manifest` is a
// read of the build's own install table, for `make install` to consume. It
// needs no root and no host, and names the three install trees.
func TestHostInitPrintManifestIsRootlessAndWritesNothing(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "host", "init", "--print-manifest")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{
		"/usr/lib/systemd/system/gvm@.service",
		"/usr/lib/tmpfiles.d/gvm.conf",
		"/usr/lib/sysusers.d/gvm.conf",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest did not name %q:\n%s", want, out)
		}
	}
	if n := len(h.FS.writes); n != 0 {
		t.Errorf("--print-manifest wrote %d file(s)", n)
	}
}

// TestHostInitNeedsRoot: run as a normal user, `host init` refuses with the
// copy-paste sudo line and exits 5 (refused), before it touches anything.
func TestHostInitNeedsRoot(t *testing.T) {
	h := newTestHost(t)
	var out, errb bytes.Buffer
	env := &Env{
		Args: []string{"host", "init"}, Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Geteuid: func() int { return 1000 },
		Host:    h.HostEnv,
	}
	code := Main(context.Background(), env)
	if code != exitRefused {
		t.Errorf("exit = %d, want %d (refused)", code, exitRefused)
	}
	if !strings.Contains(errb.String(), "sudo gvm host init") {
		t.Errorf("refusal did not give the sudo line:\n%s", errb.String())
	}
	if n := len(h.FS.writes); n != 0 {
		t.Errorf("a refused host init wrote %d file(s)", n)
	}
}
