package cli

import (
	"encoding/json"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/store"
)

// TestVersionJSONCarriesTheFrozenFormats: the upgrade check reads these, so the
// JSON must carry the real format constants, not a hand-kept copy that could
// drift from what the binary actually writes.
func TestVersionJSONCarriesTheFrozenFormats(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var got struct {
		Version string         `json:"version"`
		Formats map[string]int `json:"formats"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, out)
	}
	if got.Version == "" {
		t.Error("no version field")
	}
	if got.Formats["store_schema"] != store.SchemaVersion {
		t.Errorf("store_schema = %d, want %d", got.Formats["store_schema"], store.SchemaVersion)
	}
	if got.Formats["archive_schema"] != archive.SchemaVersion {
		t.Errorf("archive_schema = %d, want %d", got.Formats["archive_schema"], archive.SchemaVersion)
	}
	for _, k := range []string{"console_proto", "lease_proto", "freeze_lease", "status_proto", "runner_file", "shutdown_plan"} {
		if _, ok := got.Formats[k]; !ok {
			t.Errorf("format %q missing from version --json", k)
		}
	}
}

// TestVersionPlainPrintsTheBanner: the no-flag form stays the one-line banner,
// so `gvm version` in a script keeps working.
func TestVersionPlainPrintsTheBanner(t *testing.T) {
	h := newTestHost(t)
	code, out, errb := runWith(t, h, "", "version")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if len(out) == 0 || out[0] != 'g' { // "gvm …"
		t.Errorf("version banner = %q, want it to start with \"gvm \"", out)
	}
}
