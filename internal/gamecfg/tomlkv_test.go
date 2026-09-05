package gamecfg

import (
	"errors"
	"strings"
	"testing"
)

func TestTOMLGetAcrossTablesAndQuotingStyles(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	tests := []struct {
		path string
		want string
	}{
		{"name", "Test Server"},
		{"port", "27015"},
		{"max_players", "16"},
		{"enabled", "true"},
		{"tick_rate", "30.0"},
		{"world.seed", "abcdef"},
		{"world.size", "6144"},
		{"network.bind", "0.0.0.0"},
		{"network.compression", "256"},
		// A literal string carries no escapes, so a double quote inside one is
		// just a character; treating it as a delimiter loses the rest of the line.
		{"motd", `welcome, don"t break anything`},
	}
	for _, tt := range tests {
		if got, ok := doc.Get(tt.path); !ok || got != tt.want {
			t.Errorf("Get(%q) = %q, %v; want %q", tt.path, got, ok, tt.want)
		}
	}
}

// This is the refusal §8.4 asks for by name. Editing inside an inline table
// means rewriting the whole braced value, and every round-tripping TOML
// library does that by re-serialising the document.
func TestTOMLInlineTableIsALoadErrorNotASilentRewrite(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	before := doc.String()

	err := doc.Set("world.spawn.x", "10")
	if !errors.Is(err, ErrInlineTable) {
		t.Fatalf("Set(world.spawn.x) = %v; want ErrInlineTable", err)
	}
	if !strings.Contains(err.Error(), "world.spawn") {
		t.Errorf("the message does not name the inline table: %v", err)
	}
	if doc.String() != before {
		t.Fatal("a refused write still modified the file")
	}

	if err := doc.Set("world.spawn", "x"); !errors.Is(err, ErrInlineTable) {
		t.Errorf("Set on the inline table itself = %v; want ErrInlineTable", err)
	}
	for _, k := range doc.Keys() {
		if strings.HasPrefix(k, "world.spawn") {
			t.Errorf("Keys() offered %q, which Set refuses", k)
		}
	}
}

// In an [[array of tables]] "the" value does not exist: there are N of them and
// a dotted path says nothing about which.
func TestTOMLArrayOfTablesIsRefused(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	before := doc.String()

	if err := doc.Set("admins.name", "someone"); !errors.Is(err, ErrArrayOfTables) {
		t.Fatalf("Set(admins.name) = %v; want ErrArrayOfTables", err)
	}
	if err := doc.Set("admins", "someone"); !errors.Is(err, ErrArrayOfTables) {
		t.Errorf("Set(admins) = %v; want ErrArrayOfTables", err)
	}
	if doc.String() != before {
		t.Error("a refused write still modified the file")
	}
	for _, k := range doc.Keys() {
		if strings.HasPrefix(k, "admins") {
			t.Errorf("Keys() offered %q, which Set refuses", k)
		}
	}
}

// Rewriting a value that spans lines means reflowing the lines under it, which
// is exactly the whole-file churn this codec exists to avoid.
func TestTOMLMultiLineValueIsRefused(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	err := doc.Set("world.tags", `["pvp"]`)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Set(world.tags) = %v; want ErrUnsupported", err)
	}

	multiline := ParseTOML([]byte("a = \"\"\"\nline one\nline two\n\"\"\"\nb = 1\n"))
	if v, ok := multiline.Get("b"); !ok || v != "1" {
		t.Errorf("a key after a multi-line string was not found: %q, %v", v, ok)
	}
	if err := multiline.Set("a", "x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Set on a multi-line string = %v; want ErrUnsupported", err)
	}
}

func TestTOMLTypeIsPreservedNotInferred(t *testing.T) {
	tests := []struct {
		path  string
		value string
		want  string // "" means refused
	}{
		{"port", "27016", "port = 27016"},
		{"port", "many", ""},
		{"enabled", "false", "enabled = false"},
		{"enabled", "off", ""},
		{"name", "27016", `name = "27016"`},
		{"tick_rate", "60.0", "tick_rate = 60.0"},
	}
	for _, tt := range tests {
		t.Run(tt.path+"="+tt.value, func(t *testing.T) {
			doc := ParseTOML(readSample(t, "server.toml"))
			before := doc.String()
			err := doc.Set(tt.path, tt.value)
			if tt.want == "" {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("Set = %v; want ErrUnsupported", err)
				}
				if doc.String() != before {
					t.Error("a refused Set modified the file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(doc.String(), tt.want) {
				t.Errorf("wrote something other than %q", tt.want)
			}
		})
	}
}

// A trailing comment documents the setting next to it. Losing it on a write is
// how a config file stops explaining itself one key at a time.
func TestTOMLTrailingCommentSurvivesAWrite(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	if err := doc.Set("name", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.String(), `name = "Renamed"          # shown in the browser`) {
		t.Errorf("the trailing comment or its alignment was lost:\n%s", firstLines(doc.String(), 6))
	}
}

func TestTOMLHashInsideAStringIsNotAComment(t *testing.T) {
	doc := ParseTOML([]byte(`tag = "a # b"` + "\n"))
	if v, _ := doc.Get("tag"); v != "a # b" {
		t.Errorf("Get = %q; the '#' is inside the string", v)
	}
	if err := doc.Set("tag", "c # d"); err != nil {
		t.Fatal(err)
	}
	if got := doc.String(); got != `tag = "c # d"`+"\n" {
		t.Errorf("Set produced %q", got)
	}
}

func TestTOMLStringEscapingRoundTrips(t *testing.T) {
	doc := ParseTOML([]byte("a = \"x\"\n"))
	value := "quote \" backslash \\ tab \t newline \n"
	if err := doc.Set("a", value); err != nil {
		t.Fatal(err)
	}
	if got, _ := ParseTOML(doc.Bytes()).Get("a"); got != value {
		t.Errorf("read back %q; want %q", got, value)
	}
	if strings.Count(doc.String(), "\n") != 1 {
		t.Errorf("a newline in the value broke the line: %q", doc.String())
	}
}

func TestTOMLUnknownKeyIsNotInvented(t *testing.T) {
	doc := ParseTOML(readSample(t, "server.toml"))
	before := doc.String()
	if err := doc.Set("nosuchkey", "1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set = %v; want ErrKeyNotFound", err)
	}
	if doc.String() != before {
		t.Error("a refused Set modified the file")
	}
}

func TestTOMLDeleteRemovesTheLineAndLeavesTheComments(t *testing.T) {
	doc := ParseTOML([]byte("# header\na = 1\n# about b\nb = 2\n"))
	if n := doc.Delete("a"); n != 1 {
		t.Fatalf("Delete removed %d", n)
	}
	if got := doc.String(); got != "# header\n# about b\nb = 2\n" {
		t.Errorf("Delete produced %q", got)
	}
}

func TestTOMLDottedAndQuotedKeys(t *testing.T) {
	doc := ParseTOML([]byte("a.b = 1\n\"c\" = 2\n[t]\nd.e = 3\n"))
	for _, want := range []string{"a.b", "c", "t.d.e"} {
		if _, ok := doc.Get(want); !ok {
			t.Errorf("Get(%q) not found; keys are %v", want, doc.Keys())
		}
	}
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
