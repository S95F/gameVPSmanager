package gamecfg

import (
	"strings"
	"testing"
)

// A realistic fragment of a Project Zomboid <servername>.ini.
const pzINI = `nightlengthmodifier=1.0
PVP=true
PauseEmpty=true
GlobalChat=true
Public=false
PublicName=My PZ Server
PublicDescription=
MaxPlayers=32
WorkshopItems=
Mods=
`

func TestINIGet(t *testing.T) {
	f := ParseINI([]byte(pzINI))
	for _, tc := range []struct{ key, want string }{
		{"PVP", "true"},
		{"MaxPlayers", "32"},
		{"PublicName", "My PZ Server"},
		{"PublicDescription", ""},
		{"Mods", ""},
	} {
		got, ok := f.Get(tc.key)
		if !ok {
			t.Errorf("Get(%q): not found", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if _, ok := f.Get("NoSuchKey"); ok {
		t.Error("Get(NoSuchKey) reported found")
	}
}

// Project Zomboid's ini has Public, PublicName and PublicDescription. Matching
// on the bare key rather than "key=" makes a write to Public land on
// PublicName instead, which is the kind of corruption that shows up much later
// as a server that will not go public.
func TestINIKeyPrefixCollision(t *testing.T) {
	f := ParseINI([]byte(pzINI))

	if got, _ := f.Get("Public"); got != "false" {
		t.Fatalf("Get(Public) = %q, want %q", got, "false")
	}

	f.Set("Public", "true")
	out := f.String()

	if got, _ := f.Get("Public"); got != "true" {
		t.Errorf("after Set, Get(Public) = %q, want true", got)
	}
	if got, _ := f.Get("PublicName"); got != "My PZ Server" {
		t.Errorf("PublicName was corrupted: %q", got)
	}
	if got, _ := f.Get("PublicDescription"); got != "" {
		t.Errorf("PublicDescription was corrupted: %q", got)
	}
	if !strings.Contains(out, "PublicName=My PZ Server") {
		t.Error("PublicName line missing from output")
	}
	if strings.Count(out, "\nPublic=") != 1 {
		t.Errorf("expected exactly one Public= line, got:\n%s", out)
	}
}

func TestINISetPreservesPositionAndUnknownLines(t *testing.T) {
	src := `# a comment
PVP=true

MaxPlayers=32
some garbage line
`
	f := ParseINI([]byte(src))
	f.Set("PVP", "false")

	want := `# a comment
PVP=false

MaxPlayers=32
some garbage line
`
	if got := f.String(); got != want {
		t.Errorf("Set rewrote surrounding content.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestINISetAppendsWhenMissing(t *testing.T) {
	f := ParseINI([]byte("PVP=true\n"))
	f.Set("RCONPort", "27015")
	if got, want := f.String(), "PVP=true\nRCONPort=27015\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// The game picks one of a duplicated key and there is no way to know which, so
// a stale second copy means the file no longer describes the live config.
func TestINISetRemovesDuplicates(t *testing.T) {
	f := ParseINI([]byte("Mods=old\nPVP=true\nMods=older\n"))
	f.Set("Mods", "new")
	got := f.String()
	if want := "Mods=new\nPVP=true\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Count(got, "Mods=") != 1 {
		t.Errorf("duplicate Mods= survived:\n%s", got)
	}
}

func TestINIValueContainingEquals(t *testing.T) {
	f := ParseINI([]byte("Password=a=b=c\n"))
	if got, _ := f.Get("Password"); got != "a=b=c" {
		t.Errorf("got %q, want %q", got, "a=b=c")
	}
	f.Set("Password", "x=y")
	if got, _ := f.Get("Password"); got != "x=y" {
		t.Errorf("after Set got %q, want %q", got, "x=y")
	}
}

// A newline smuggled into a value would inject an arbitrary extra line into
// the file, which for a semicolon-separated Mods= list is a real corruption
// path from a mod title.
func TestINISetStripsNewlinesFromValue(t *testing.T) {
	f := ParseINI([]byte("PublicDescription=\n"))
	f.Set("PublicDescription", "line one\nPVP=false\r\nline two")
	if strings.Count(f.String(), "\n") != 1 {
		t.Errorf("value newline leaked into the file:\n%q", f.String())
	}
	if got, _ := f.Get("PublicDescription"); got != "line one PVP=false line two" {
		t.Errorf("got %q", got)
	}
}

func TestINIPreservesCRLF(t *testing.T) {
	f := ParseINI([]byte("PVP=true\r\nMaxPlayers=8\r\n"))
	f.Set("PVP", "false")
	got := f.String()
	if !strings.Contains(got, "PVP=false\r\n") {
		t.Errorf("CRLF not preserved: %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("mixed line endings in output: %q", got)
	}
}

func TestININoTrailingNewlinePreserved(t *testing.T) {
	f := ParseINI([]byte("PVP=true"))
	f.Set("PVP", "false")
	if got, want := f.String(), "PVP=false"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestINIEmptyFile(t *testing.T) {
	f := ParseINI(nil)
	if got := f.String(); got != "" {
		t.Errorf("empty input produced %q", got)
	}
	f.Set("PVP", "true")
	if got, want := f.String(), "PVP=true"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestINIDeleteAndKeys(t *testing.T) {
	f := ParseINI([]byte("A=1\n# note\nB=2\nA=3\n"))
	if n := f.Delete("A"); n != 2 {
		t.Errorf("Delete removed %d, want 2", n)
	}
	if f.Has("A") {
		t.Error("A survived Delete")
	}
	keys := f.Keys()
	if len(keys) != 1 || keys[0] != "B" {
		t.Errorf("Keys() = %v, want [B]", keys)
	}
}

func TestINISetAll(t *testing.T) {
	f := ParseINI([]byte(pzINI))
	f.SetAll([][2]string{
		{"Public", "true"},
		{"PublicName", "MadGaymers.club"},
		{"MaxPlayers", "8"},
		{"RCONPort", "27015"},
	})
	for _, tc := range []struct{ key, want string }{
		{"Public", "true"},
		{"PublicName", "MadGaymers.club"},
		{"MaxPlayers", "8"},
		{"RCONPort", "27015"},
		{"PVP", "true"}, // untouched
	} {
		if got, _ := f.Get(tc.key); got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
