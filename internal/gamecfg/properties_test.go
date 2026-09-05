package gamecfg

import (
	"bytes"
	"strings"
	"testing"
)

func TestPropertiesSeparatorsAndKeyBoundaries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{"equals", "a=1\n", "a", "1"},
		{"colon", "a:1\n", "a", "1"},
		{"spaces around equals", "a = 1\n", "a", "1"},
		{"whitespace only separator", "a 1\n", "a", "1"},
		{"tab separator", "a\t1\n", "a", "1"},
		{"empty value", "a=\n", "a", ""},
		{"value keeps internal spaces", "motd=A Minecraft Server\n", "motd", "A Minecraft Server"},
		{"escaped colon in value", `level-type=minecraft\:normal` + "\n", "level-type", "minecraft:normal"},
		{"escaped space in key", `a\ b=1` + "\n", "a b", "1"},
		{"unicode escape", `a=caf\u00e9` + "\n", "a", "café"},
		{"surrogate pair rejoins", `a=\uD83C\uDFAE` + "\n", "a", "🎮"},
		{"leading whitespace ignored", "   a=1\n", "a", "1"},
		{"bang comment is not a key", "!a=1\nb=2\n", "b", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ParseProperties([]byte(tt.in))
			got, ok := p.Get(tt.key)
			if !ok {
				t.Fatalf("Get(%q) not found; keys are %v", tt.key, p.Keys())
			}
			if got != tt.want {
				t.Errorf("Get(%q) = %q; want %q", tt.key, got, tt.want)
			}
		})
	}
}

// A key ends at the first unescaped separator, so "a b=1" is the key "a" with
// the value "b=1" and not the key "a b". Getting this wrong means a manifest
// writing "a b" creates a second, different setting and the server keeps
// running on the first.
func TestPropertiesKeyEndsAtTheFirstSeparator(t *testing.T) {
	p := ParseProperties([]byte("a b=1\n"))
	if _, ok := p.Get("a b"); ok {
		t.Error(`"a b=1" was read as the key "a b"; the space ends the key`)
	}
	if v, ok := p.Get("a"); !ok || v != "b=1" {
		t.Errorf(`Get("a") = %q, %v; want "b=1"`, v, ok)
	}
}

func TestPropertiesSeparatorStyleSurvivesAWrite(t *testing.T) {
	p := ParseProperties([]byte("a : 1\n"))
	if err := p.Set("a", "2"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "a : 2\n" {
		t.Errorf("Set rewrote the separator: %q", got)
	}
}

// java.util.Properties.load overwrites as it reads, so the LAST assignment is
// the one the server runs on. Reporting the first would describe a value that
// is not in effect.
func TestPropertiesLastDuplicateWinsAndSetCollapsesThem(t *testing.T) {
	p := ParseProperties([]byte("a=1\nb=2\na=3\n"))
	if v, _ := p.Get("a"); v != "3" {
		t.Errorf(`Get("a") = %q; the later assignment is the one in effect`, v)
	}
	if err := p.Set("a", "9"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "a=9\nb=2\n" {
		t.Errorf("Set left a stale duplicate behind: %q", got)
	}
	if v, _ := p.Get("a"); v != "9" {
		t.Errorf(`Get("a") = %q after Set; want "9"`, v)
	}
}

func TestPropertiesContinuationLineIsOneValue(t *testing.T) {
	in := "before=x\nlong=first \\\n      second\nafter=y\n"
	p := ParseProperties([]byte(in))
	if v, _ := p.Get("long"); v != "first second" {
		t.Fatalf(`Get("long") = %q; want "first second"`, v)
	}
	if got := p.String(); got != in {
		t.Errorf("no-op round trip changed a continued entry:\n%q", got)
	}

	if err := p.Set("long", "one"); err != nil {
		t.Fatal(err)
	}
	got := p.String()
	if got != "before=x\nlong=one\nafter=y\n" {
		t.Errorf("rewriting a continued entry disturbed its neighbours: %q", got)
	}
}

// An even number of trailing backslashes is an escaped backslash, not a
// continuation. Treating it as one swallows the next line into this value.
func TestPropertiesEvenBackslashesDoNotContinue(t *testing.T) {
	p := ParseProperties([]byte(`path=C:\\` + "\nnext=1\n"))
	if v, _ := p.Get("path"); v != `C:\` {
		t.Errorf(`Get("path") = %q; want %q`, v, `C:\`)
	}
	if _, ok := p.Get("next"); !ok {
		t.Error("the following line was swallowed as a continuation")
	}
}

// The file is ISO-8859-1, not UTF-8. A Latin-1 byte must survive untouched and
// anything above it must be written as a \uXXXX escape, or the game reads a
// different string than the one gvm wrote.
func TestPropertiesISO88591(t *testing.T) {
	raw := []byte("motd=caf\xe9\n")
	p := ParseProperties(raw)
	if v, _ := p.Get("motd"); v != "café" {
		t.Fatalf("Get = %q; want %q", v, "café")
	}
	if !bytes.Equal(p.Bytes(), raw) {
		t.Errorf("a Latin-1 byte did not survive an untouched round trip: %q", p.Bytes())
	}

	if err := p.Set("motd", "café"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Bytes(), raw) {
		t.Errorf("writing back the same Latin-1 text changed the bytes: %q", p.Bytes())
	}

	if err := p.Set("motd", "€5"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != `motd=\u20AC5`+"\n" {
		t.Errorf("a non-Latin-1 character was not escaped: %q", got)
	}
	if v, _ := p.Get("motd"); v != "€5" {
		t.Errorf("escaped value did not read back: %q", v)
	}

	if err := p.Set("motd", "play 🎮 now"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.String(), `\uD83C\uDFAE`) {
		t.Errorf("an astral character was not written as a surrogate pair: %q", p.String())
	}
	if v, _ := p.Get("motd"); v != "play 🎮 now" {
		t.Errorf("surrogate pair did not read back: %q", v)
	}
}

func TestPropertiesEscapesOnWrite(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"equals in value", "a", "x=y", `a=x\=y` + "\n"},
		{"colon in value", "a", "x:y", `a=x\:y` + "\n"},
		{"hash in value", "a", "x#y", `a=x\#y` + "\n"},
		{"bang in value", "a", "x!y", `a=x\!y` + "\n"},
		{"backslash in value", "a", `x\y`, `a=x\\y` + "\n"},
		{"leading space in value", "a", " x", `a=\ x` + "\n"},
		{"trailing space is left alone", "a", "x ", "a=x \n"},
		{"newline in value", "a", "x\ny", `a=x\ny` + "\n"},
		{"space in key", "a b", "1", `a\ b=1` + "\n"},
		{"colon in key", "a:b", "1", `a\:b=1` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ParseProperties(nil)
			if err := p.Set(tt.key, tt.value); err != nil {
				t.Fatal(err)
			}
			if got := p.String(); got != tt.want {
				t.Fatalf("Set wrote %q; want %q", got, tt.want)
			}
			round := ParseProperties(p.Bytes())
			if v, _ := round.Get(tt.key); v != tt.value {
				t.Errorf("value did not survive a read back: %q -> %q", tt.value, v)
			}
		})
	}
}

func TestPropertiesAppendsKeepingComments(t *testing.T) {
	in := "#header\na=1\n"
	p := ParseProperties([]byte(in))
	if err := p.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "#header\na=1\nb=2\n" {
		t.Errorf("append disturbed the file: %q", got)
	}
}

// A file that does not end in a newline is common when something wrote it with
// printf. Appending without adding one glues the new key onto the old value.
func TestPropertiesAppendAfterAFileWithNoTrailingNewline(t *testing.T) {
	p := ParseProperties([]byte("a=1"))
	if err := p.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "a=1\nb=2\n" {
		t.Errorf("append produced %q", got)
	}
}

func TestPropertiesCRLFSurvives(t *testing.T) {
	in := "#c\r\na=1\r\nb=2\r\n"
	p := ParseProperties([]byte(in))
	if err := p.Set("a", "9"); err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "#c\r\na=9\r\nb=2\r\n" {
		t.Errorf("CRLF endings were rewritten: %q", got)
	}
}

func TestPropertiesDelete(t *testing.T) {
	p := ParseProperties([]byte("#c\na=1\nb=2\na=3\n"))
	if n := p.Delete("a"); n != 2 {
		t.Errorf("Delete removed %d assignments; want 2", n)
	}
	if got := p.String(); got != "#c\nb=2\n" {
		t.Errorf("Delete produced %q", got)
	}
	if n := p.Delete("missing"); n != 0 {
		t.Errorf("Delete of an absent key reported %d", n)
	}
}

func TestPropertiesKeysAreInFileOrder(t *testing.T) {
	p := ParseProperties(readSample(t, "server.properties"))
	keys := p.Keys()
	if len(keys) < 50 {
		t.Fatalf("only %d keys parsed out of the sample", len(keys))
	}
	if keys[0] != "accepts-transfers" || keys[1] != "allow-flight" {
		t.Errorf("keys are not in file order: %v", keys[:3])
	}
	if p.Keys()[len(keys)-1] != "white-list" {
		t.Errorf("last key = %q; want white-list", keys[len(keys)-1])
	}
}
