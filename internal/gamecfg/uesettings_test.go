package gamecfg

import (
	"errors"
	"strings"
	"testing"
)

// The sample's ServerName contains a comma. A naive split on "," turns one
// option into two malformed ones, and Palworld's response to a malformed
// OptionSettings list is to ignore the whole thing and run every setting at its
// compiled-in default.
func TestUEQuoteAwareSplit(t *testing.T) {
	u := ParseUESettings(readSample(t, "palworldsettings.ini"))
	if v, ok := u.Get("ServerName"); !ok || v != "Default Palworld Server, run by nobody" {
		t.Fatalf("Get(ServerName) = %q, %v", v, ok)
	}
	if _, ok := u.Get("run by nobody"); ok {
		t.Error("the comma inside a quoted value was treated as a separator")
	}
	if v, _ := u.Get("PublicPort"); v != "8211" {
		t.Errorf("Get(PublicPort) = %q; want 8211", v)
	}
	if v, _ := u.Get("BanListURL"); v != "https://api.palworldgame.com/api/banlist.txt" {
		t.Errorf("Get(BanListURL) = %q", v)
	}
}

// Quoting is per option and carries meaning: PublicPort=8211 is a number and
// ServerPassword="8211" is a string. Neither can be derived from the value, so
// the file's own choice is copied.
func TestUEQuotingStyleIsPreserved(t *testing.T) {
	u := ParseUESettings(readSample(t, "palworldsettings.ini"))
	if err := u.Set("ServerName", "Palbox"); err != nil {
		t.Fatal(err)
	}
	if err := u.Set("PublicPort", "8300"); err != nil {
		t.Fatal(err)
	}
	out := u.String()
	if !strings.Contains(out, `ServerName="Palbox"`) {
		t.Error("a quoted option lost its quotes")
	}
	if !strings.Contains(out, "PublicPort=8300,") {
		t.Error("an unquoted option gained quotes")
	}
	if strings.Contains(out, `PublicPort="`) {
		t.Error(`PublicPort was written as a string`)
	}
}

// Splicing a comma into an unquoted slot redefines where the next option
// starts, which silently shifts every setting after it.
func TestUEUnquotedOptionRefusesAValueThatWouldBreakTheList(t *testing.T) {
	u := ParseUESettings(readSample(t, "palworldsettings.ini"))
	before := u.String()
	err := u.Set("PublicPort", "8211,RCONEnabled=True")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Set = %v; want ErrUnsupported", err)
	}
	if u.String() != before {
		t.Error("the file was modified by a refused write")
	}
}

func TestUEQuotedValueEscaping(t *testing.T) {
	u := ParseUESettings([]byte(`OptionSettings=(ServerName="x")` + "\n"))
	if err := u.Set("ServerName", `a "quoted" \ name`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.String(), `ServerName="a \"quoted\" \\ name"`) {
		t.Fatalf("escaping is wrong: %s", u.String())
	}
	if v, _ := ParseUESettings(u.Bytes()).Get("ServerName"); v != `a "quoted" \ name` {
		t.Errorf("read back %q", v)
	}
}

// PalWorldSettings.ini is seeded from the version's own DefaultPalWorldSettings.ini,
// so an option that is not there is one the installed build does not have.
// Appending it would produce a line the game reads, ignores, and never mentions.
func TestUEUnknownOptionIsNotInvented(t *testing.T) {
	u := ParseUESettings(readSample(t, "palworldsettings.ini"))
	before := u.String()
	err := u.Set("NoSuchOption", "1")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set = %v; want ErrKeyNotFound", err)
	}
	if !strings.Contains(err.Error(), "pal.settings_seed") {
		t.Errorf("the message does not say where the option list comes from: %v", err)
	}
	if u.String() != before {
		t.Error("a refused Set still modified the file")
	}
}

// Only the options inside OptionSettings are addressable. MaxPlayers in
// [/Script/Engine.GameSession] is a different setting with a similar name, and
// exposing the outer INI keys would make "OptionSettings" itself writable.
func TestUEOnlyInnerOptionsAreAddressable(t *testing.T) {
	u := ParseUESettings(readSample(t, "palworldsettings.ini"))
	for _, k := range u.Keys() {
		if k == "MaxPlayers" || k == "OptionSettings" {
			t.Errorf("Keys() exposed the outer INI key %q", k)
		}
	}
	if _, ok := u.Get("MaxPlayers"); ok {
		t.Error("Get reached an outer INI key")
	}
	if u.Keys()[0] != "Difficulty" {
		t.Errorf("Keys() is not in list order: %v", u.Keys()[:3])
	}
}

func TestUEMissingOptionSettingsLineIsReported(t *testing.T) {
	u := ParseUESettings([]byte("[/Script/Engine.GameSession]\nMaxPlayers=32\n"))
	err := u.Set("ServerName", "x")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set = %v; want ErrKeyNotFound", err)
	}
	if !strings.Contains(err.Error(), "OptionSettings") {
		t.Errorf("the message does not name the missing line: %v", err)
	}
}

func TestUEDeleteRemovesOneOptionAndOneComma(t *testing.T) {
	tests := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{"middle", "OptionSettings=(A=1,B=2,C=3)\n", "B", "OptionSettings=(A=1,C=3)\n"},
		{"first", "OptionSettings=(A=1,B=2)\n", "A", "OptionSettings=(B=2)\n"},
		{"last", "OptionSettings=(A=1,B=2)\n", "B", "OptionSettings=(A=1)\n"},
		{"only", "OptionSettings=(A=1)\n", "A", "OptionSettings=()\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := ParseUESettings([]byte(tt.in))
			if n := u.Delete(tt.key); n != 1 {
				t.Fatalf("Delete removed %d", n)
			}
			if got := u.String(); got != tt.want {
				t.Errorf("Delete produced %q; want %q", got, tt.want)
			}
		})
	}
}

func TestUECRLFSurvives(t *testing.T) {
	in := "[/Script/Pal.PalGameWorldSettings]\r\nOptionSettings=(ServerName=\"a\")\r\nMaxPlayers=8\r\n"
	u := ParseUESettings([]byte(in))
	if err := u.Set("ServerName", "b"); err != nil {
		t.Fatal(err)
	}
	want := "[/Script/Pal.PalGameWorldSettings]\r\nOptionSettings=(ServerName=\"b\")\r\nMaxPlayers=8\r\n"
	if got := u.String(); got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}
