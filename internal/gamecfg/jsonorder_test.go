package gamecfg

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustParseJSON(t *testing.T, name string) *JSONDoc {
	t.Helper()
	d, err := ParseJSON(readSample(t, name))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return d
}

func TestJSONGetByDottedPath(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	tests := []struct {
		path string
		want string
	}{
		{"name", "Enshrouded Server"},
		{"gamePort", "15636"},
		{"enableVoiceChat", "false"},
		{"gameSettings.playerHealthFactor", "1"},
		{"gameSettings.tombstoneMode", "AddBackpackMaterials"},
		{"gameSettings.enableDurability", "true"},
	}
	for _, tt := range tests {
		if got, ok := d.Get(tt.path); !ok || got != tt.want {
			t.Errorf("Get(%q) = %q, %v; want %q", tt.path, got, ok, tt.want)
		}
	}
}

// A slot holding 16 is a count and a slot holding "16" is a name. Coercing
// between them either makes the game ignore the setting or refuse to start, so
// the file's own type is copied and a value that does not fit is refused.
func TestJSONTypeIsPreservedNotInferred(t *testing.T) {
	tests := []struct {
		path    string
		value   string
		wantLit string // "" means the write must be refused
	}{
		{"slotCount", "24", "24"},
		{"slotCount", "twenty", ""},
		{"enableTextChat", "true", "true"},
		{"enableTextChat", "yes", ""},
		{"name", "16", `"16"`},
		{"gameSettings", "x", ""},
		{"userGroups", "x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path+"="+tt.value, func(t *testing.T) {
			d := mustParseJSON(t, "enshrouded.json")
			before := d.String()
			err := d.Set(tt.path, tt.value)

			if tt.wantLit == "" {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("Set = %v; want ErrUnsupported", err)
				}
				if d.String() != before {
					t.Error("a refused Set still modified the document")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(d.String(), `"`+lastSegment(tt.path)+`": `+tt.wantLit) {
				t.Errorf("wrote something other than %s:\n%s", tt.wantLit, d.String())
			}
		})
	}
}

func lastSegment(path string) string {
	_, leaf := splitParent(path)
	return leaf
}

func TestJSONStaysValidAfterEveryMutation(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	if err := d.Set("name", "a \"quoted\" name\nwith a newline"); err != nil {
		t.Fatal(err)
	}
	if err := d.Set("gameSettings.weatherFrequency", "Rare"); err != nil {
		t.Fatal(err)
	}
	if n := d.Delete("logDirectory"); n != 1 {
		t.Fatalf("Delete removed %d", n)
	}
	var v map[string]any
	if err := json.Unmarshal(d.Bytes(), &v); err != nil {
		t.Fatalf("the document is no longer valid JSON: %v\n%s", err, d.String())
	}
	if _, still := v["logDirectory"]; still {
		t.Error("Delete left the key behind")
	}
	if v["name"] != "a \"quoted\" name\nwith a newline" {
		t.Errorf("name = %v", v["name"])
	}
}

func TestJSONDeleteAtEveryPosition(t *testing.T) {
	tests := []struct {
		name string
		in   string
		key  string
	}{
		{"first", "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n", "a"},
		{"middle", "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n", "b"},
		{"last", "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n", "c"},
		{"only", "{\n  \"a\": 1\n}\n", "a"},
		{"compact", `{"a":1,"b":2}`, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseJSON([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			d.Delete(tt.key)
			var v map[string]any
			if err := json.Unmarshal(d.Bytes(), &v); err != nil {
				t.Fatalf("invalid JSON after delete: %v\n%q", err, d.String())
			}
			if _, still := v[tt.key]; still {
				t.Error("the key survived")
			}
		})
	}
}

// Array elements have no dotted address, so a manifest that names one has to
// fail rather than resolve to something.
func TestJSONArrayContentsAreNotAddressable(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	for _, k := range d.Keys() {
		if strings.HasPrefix(k, "userGroups.") {
			t.Errorf("Keys() reached inside an array: %q", k)
		}
	}
	if _, ok := d.Get("userGroups.name"); ok {
		t.Error("Get resolved a path through an array")
	}
}

func TestJSONInsertsIntoAnExistingObjectOnly(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	if err := d.Set("gameSettings.newSetting", "42"); err != nil {
		t.Fatal(err)
	}
	var v struct {
		GameSettings map[string]any `json:"gameSettings"`
	}
	if err := json.Unmarshal(d.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON after insert: %v\n%s", err, d.String())
	}
	if v.GameSettings["newSetting"] != float64(42) {
		t.Errorf("newSetting = %v", v.GameSettings["newSetting"])
	}
	if !strings.Contains(d.String(), "\n    \"newSetting\": 42") {
		t.Errorf("the new key did not copy its siblings' indentation:\n%s", d.String())
	}
}

// Inventing "world.limits" because the manifest addressed "world.limits.max"
// produces a file that parses, applies nothing, and reports no error anywhere.
func TestJSONNeverInventsIntermediateObjects(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	before := d.String()
	err := d.Set("nosuch.branch.leaf", "1")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set = %v; want ErrKeyNotFound", err)
	}
	if !strings.Contains(err.Error(), "nosuch.branch") {
		t.Errorf("the message does not name the missing parent: %v", err)
	}
	if d.String() != before {
		t.Error("a refused Set modified the document")
	}
}

func TestJSONInsertIntoAnEmptyObject(t *testing.T) {
	d, err := ParseJSON([]byte("{\n  \"opts\": {}\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Set("opts.a", "1"); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"opts\": {\n    \"a\": 1\n  }\n}\n"
	if got := d.String(); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

// A config file that is not valid JSON is one the game could not read either.
// Reporting that is more useful than silently editing nothing.
func TestJSONParseRefusesWhatTheGameCouldNotRead(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"truncated", `{"a": 1`},
		{"trailing comma", `{"a": 1,}`},
		{"array root", `[1, 2]`},
		{"scalar root", `"hello"`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(tt.in)); err == nil {
				t.Fatalf("ParseJSON accepted %q", tt.in)
			}
		})
	}
}

func TestJSONNestedObjectKeysAreListedByPath(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	want := map[string]bool{
		"gameSettings.playerHealthFactor": true,
		"gameSettings.tombstoneMode":      true,
		"slotCount":                       true,
	}
	for _, k := range d.Keys() {
		delete(want, k)
		if k == "gameSettings" {
			t.Error("Keys() listed an object as if it were a value")
		}
	}
	for k := range want {
		t.Errorf("Keys() is missing %q", k)
	}
}

func TestJSONInsertAtTheRoot(t *testing.T) {
	d := mustParseJSON(t, "enshrouded.json")
	if err := d.Set("newTopLevel", "hello"); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(d.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON after a root insert: %v\n%s", err, d.String())
	}
	if v["newTopLevel"] != "hello" {
		t.Errorf("newTopLevel = %v", v["newTopLevel"])
	}
	if v["name"] != "Enshrouded Server" {
		t.Error("an existing root key was disturbed")
	}

	empty, err := ParseJSON([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if got := empty.String(); got != "{\n  \"a\": 1\n}\n" {
		t.Errorf("insert into an empty document produced %q", got)
	}
}
