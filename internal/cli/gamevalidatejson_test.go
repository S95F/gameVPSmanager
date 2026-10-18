package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestGameValidateJSON is Phase 46 item 2's `--json`: the same verdict the human
// output reaches, in a shape a CI step or an authoring tool can read.
func TestGameValidateJSON(t *testing.T) {
	h := newTestHost(t)

	// A shipped game validates clean: ok, no errors, and the output is JSON.
	code, out, errb := runWith(t, h, "", "game", "validate", "valheim", "--json")
	if code != 0 {
		t.Fatalf("validate valheim --json: exit %d\n%s\n%s", code, out, errb)
	}
	var got []validateResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	if got[0].Target != "valheim" || !got[0].OK || got[0].Errors != 0 {
		t.Errorf("unexpected result: %+v", got[0])
	}

	// A broken manifest reports not-ok with the findings, and exits non-zero.
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(bad, []byte("metadata:\n  id: broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runWith(t, h, "", "game", "validate", bad, "--json")
	if code == 0 {
		t.Fatalf("a broken manifest validated clean:\n%s", out)
	}
	var got2 []validateResult
	if err := json.Unmarshal([]byte(out), &got2); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got2) != 1 || got2[0].OK {
		t.Fatalf("a broken manifest reported ok: %+v", got2)
	}
	if len(got2[0].Diags) == 0 && got2[0].ReadError == "" {
		t.Errorf("a failing result carries neither a diag nor a read error: %+v", got2[0])
	}
}
