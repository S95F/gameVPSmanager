package pz

import (
	"os"
	"testing"
)

// The real mods.txt from the live server is the best available fixture: it
// exercises apostrophes in titles, bracketed tags, a dated header block and
// section comments all at once.
func TestParsesLiveModsFile(t *testing.T) {
	data, err := os.ReadFile("../../legacy/projectzomboid-bash/mods.txt")
	if err != nil {
		t.Skipf("live mods.txt unavailable: %v", err)
	}
	m := ParseModsFile(data)
	ids := m.IDs()
	if len(ids) != 33 {
		t.Errorf("parsed %d ids from the live file, want 33", len(ids))
	}
	if ids[0] != "3171167894" {
		t.Errorf("first id = %q, want the DAMN Library (load order must be preserved)", ids[0])
	}
	if string(m.Bytes()) != string(data) {
		t.Error("round trip of the live file was not byte-identical")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %s in the live file", id)
		}
		seen[id] = true
	}
}
