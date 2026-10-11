//go:build linux

package provider

import (
	"os"
	"testing"
)

// The mode is the only thing stopping one instance's compromised mod from
// rewriting a jar every other instance on the box is linked to, so the value
// section 5 gives is pinned here rather than left to whatever the stub says.
func TestPublishedBlobsAreReadOnlyOnTheDeployTarget(t *testing.T) {
	if BlobMode.Perm() != 0o444 {
		t.Fatalf("BlobMode is %v, and section 5 says 0444", BlobMode.Perm())
	}
	s := newStore(t)
	_, j := mustPut(t, s, []byte("a published blob"))
	fi, err := os.Stat(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("published blob is %v, want 0444", fi.Mode().Perm())
	}
}
