package curseforge

import "testing"

// TestDropBoxDirIsTheDocumentedPath pins the one exported drop-box constant to
// the path the CLI's `gvm content blocked` report and the docs name. The whole
// point of the constant is that these cannot drift into telling an operator to
// drop a jar in one directory while gvm scans another.
func TestDropBoxDirIsTheDocumentedPath(t *testing.T) {
	const want = "/var/lib/gvm/downloads/mods"
	if DropBoxDir != want {
		t.Errorf("DropBoxDir = %q, want %q", DropBoxDir, want)
	}
}
