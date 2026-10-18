package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIReferenceIsUpToDate is U16's CI diff: docs/cli-reference.md is
// generated from the dispatch tables, so a command added without running
// `make docs-cli` turns the build red rather than leaving the reference quietly
// wrong. It is the same shape as TestReadmeUnbuiltMatchesRegistry.
func TestCLIReferenceIsUpToDate(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "cli-reference.md")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make docs-cli`)", path, err)
	}
	// Normalise line endings: a Windows checkout with core.autocrlf would hand
	// back CRLF, and this test is about content drift, not the repo's
	// end-of-line policy.
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	if norm(string(committed)) != norm(GenerateCLIReference()) {
		t.Errorf("docs/cli-reference.md is out of date; run `make docs-cli`.")
	}
}
