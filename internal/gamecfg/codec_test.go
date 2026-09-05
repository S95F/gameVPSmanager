package gamecfg

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func readSample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	return data
}

// sample pairs a codec with a realistic file and one edit that must be
// surgical.
type sample struct {
	format string
	file   string
	key    string
	value  string
}

// surgicalSamples covers every codec whose contract is "preserve everything you
// did not change". owned_lines is deliberately absent; see TestOwnedLines.
var surgicalSamples = []sample{
	{"pz_ini", "servertest.ini", "PublicName", "Knox County"},
	{"lua_sandboxvars", "servertest_SandboxVars.lua", "ZombieConfig.PopulationMultiplier", "0.65"},
	{"java_properties", "server.properties", "motd", "A gvm-managed server"},
	{"java_properties", "eula.txt", "eula", "true"},
	{"xml_property_list", "serverconfig.xml", "ServerName", "Navezgane Nights"},
	{"ue_option_settings", "palworldsettings.ini", "ServerName", "Palbox"},
	{"json_path", "enshrouded.json", "gameSettings.weatherFrequency", "Rare"},
	{"toml_kv", "server.toml", "network.bind", "127.0.0.1"},
}

// TestNoOpRoundTripIsByteForByte is the floor every codec must clear. A parse
// that reformats even one byte turns `gvm apply` into a whole-file rewrite, and
// the drift check that compares gvm's copy against the server's then reports a
// difference on every clean shutdown.
func TestNoOpRoundTripIsByteForByte(t *testing.T) {
	files := []string{
		"servertest.ini", "servertest_SandboxVars.lua", "server.properties",
		"eula.txt", "serverconfig.xml", "palworldsettings.ini",
		"enshrouded.json", "server.toml", "user_jvm_args.txt",
	}
	byFile := map[string]string{
		"servertest.ini":             "pz_ini",
		"servertest_SandboxVars.lua": "lua_sandboxvars",
		"server.properties":          "java_properties",
		"eula.txt":                   "java_properties",
		"serverconfig.xml":           "xml_property_list",
		"palworldsettings.ini":       "ue_option_settings",
		"enshrouded.json":            "json_path",
		"server.toml":                "toml_kv",
		"user_jvm_args.txt":          "owned_lines",
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			c, ok := Lookup(byFile[file])
			if !ok {
				t.Fatalf("codec %q is not registered", byFile[file])
			}
			in := readSample(t, file)
			doc, err := c.Parse(in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := doc.Bytes(); !bytes.Equal(got, in) {
				t.Errorf("round trip changed the file\nfirst difference at byte %d", firstDiff(in, got))
			}
		})
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestSetOneKeyChangesOnlyThatKey is the other half of the contract: comments,
// blank lines, ordering and every unrelated key survive an edit.
func TestSetOneKeyChangesOnlyThatKey(t *testing.T) {
	for _, s := range surgicalSamples {
		t.Run(s.format+"/"+s.file+"/"+s.key, func(t *testing.T) {
			c, _ := Lookup(s.format)
			in := readSample(t, s.file)
			doc, err := c.Parse(in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			beforeKeys := doc.Keys()
			before := map[string]string{}
			for _, k := range beforeKeys {
				v, _ := doc.Get(k)
				before[k] = v
			}

			if err := doc.Set(s.key, s.value); err != nil {
				t.Fatalf("set %s: %v", s.key, err)
			}

			got, ok := doc.Get(s.key)
			if !ok || got != s.value {
				t.Fatalf("after Set, Get(%q) = %q, %v; want %q", s.key, got, ok, s.value)
			}

			after := doc.Bytes()
			if changed := changedLines(in, after); len(changed) != 1 {
				t.Errorf("expected exactly one changed line, got %d:\n%s",
					len(changed), strings.Join(changed, "\n"))
			}
			if a, b := countLines(in), countLines(after); a != b {
				t.Errorf("line count changed: %d -> %d", a, b)
			}

			if !reflect.DeepEqual(doc.Keys(), beforeKeys) {
				t.Errorf("key set or order changed\nbefore %v\nafter  %v", beforeKeys, doc.Keys())
			}
			for _, k := range doc.Keys() {
				if k == s.key {
					continue
				}
				v, _ := doc.Get(k)
				if v != before[k] {
					t.Errorf("unrelated key %q changed: %q -> %q", k, before[k], v)
				}
			}
		})
	}
}

func changedLines(before, after []byte) []string {
	a := strings.Split(string(before), "\n")
	b := strings.Split(string(after), "\n")
	var out []string
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			out = append(out, "-"+x+"\n+"+y)
		}
	}
	return out
}

func countLines(b []byte) int { return bytes.Count(b, []byte("\n")) }

// TestRegistryMatchesSpecFormats keeps the two closed lists in step. spec keeps
// its own copy because manifest validation must not depend on this package
// being imported; a divergence means either a format that validates and has no
// codec, or a codec no manifest may name.
func TestRegistryMatchesSpecFormats(t *testing.T) {
	want := make([]string, 0)
	for name := range spec.ConfigFormats() {
		want = append(want, name)
	}
	sort.Strings(want)

	if got := Formats(); !reflect.DeepEqual(got, want) {
		t.Errorf("codec registry and spec.ConfigFormats disagree\ngamecfg %v\nspec    %v", got, want)
	}

	// A name reaches spec.ConfigFormats only if a codec is registered under it
	// — the check above forces the two sets equal — and Register takes a working
	// codec, so "planned" is never a truthful state for a config format. The
	// name-set check alone let toml_kv sit at SupportPlanned while shipping,
	// which told a manifest author their file would not be written.
	for name, sup := range spec.ConfigFormats() {
		if sup != spec.SupportImplemented {
			t.Errorf("spec marks config format %q as %v, but it has a registered codec", name, sup)
		}
	}
}

func TestSortedKeysOrderIsFileOrderNotAlphabetical(t *testing.T) {
	doc, err := jsonPathCodec{}.Parse(readSample(t, "enshrouded.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	keys := doc.Keys()
	if len(keys) < 3 || keys[0] != "name" || keys[1] != "password" {
		t.Fatalf("keys are not in file order: %v", keys)
	}
}

func TestNewRefusesWhereOnlyTheGameCanWriteTheFile(t *testing.T) {
	cannot := []string{"pz_ini", "lua_sandboxvars", "xml_property_list", "ue_option_settings"}
	for _, name := range cannot {
		t.Run(name, func(t *testing.T) {
			c, _ := Lookup(name)
			if _, err := c.New(); !errors.Is(err, ErrCannotCreate) {
				t.Fatalf("New() error = %v; want ErrCannotCreate", err)
			}
		})
	}

	can := []string{"java_properties", "json_path", "toml_kv", "owned_lines"}
	for _, name := range can {
		t.Run(name, func(t *testing.T) {
			c, _ := Lookup(name)
			doc, err := c.New()
			if err != nil {
				t.Fatalf("New() = %v", err)
			}
			if doc == nil {
				t.Fatal("New() returned a nil Doc with no error")
			}
		})
	}
}

func TestKeySyntaxRejectsWhatCanNeverResolve(t *testing.T) {
	tests := []struct {
		format string
		key    string
		want   string // substring of the message
	}{
		{"pz_ini", "", "empty"},
		{"pz_ini", " Public", "whitespace"},
		{"pz_ini", "a=b", "'='"},
		{"pz_ini", "a\nb", "spans lines"},
		{"lua_sandboxvars", "ZombieConfig.Sub.Deep", "two levels"},
		{"lua_sandboxvars", "Zombie-Config", "Lua identifier"},
		{"lua_sandboxvars", "a..b", "empty path segment"},
		{"xml_property_list", " ServerName", "whitespace"},
		{"xml_property_list", "Server Name", "letters, digits"},
		{"xml_property_list", "A&B", "letters, digits"},
		{"ue_option_settings", "Server.Name", "identifier"},
		{"json_path", "users[0].name", "array"},
		{"json_path", ".name", "empty path segment"},
		{"toml_kv", "world.the seed", "bare key"},
		{"owned_lines", "jvm_args", "exactly one key"},
	}
	for _, tt := range tests {
		t.Run(tt.format+"/"+tt.key, func(t *testing.T) {
			c, ok := Lookup(tt.format)
			if !ok {
				t.Fatalf("no codec %q", tt.format)
			}
			err := c.KeySyntax(tt.key)
			if err == nil {
				t.Fatalf("KeySyntax(%q) accepted a key that can never resolve", tt.key)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("message %q does not explain the problem (want substring %q)", err, tt.want)
			}
		})
	}
}

func TestKeySyntaxAcceptsTheKeysTheShippedManifestsUse(t *testing.T) {
	tests := []struct {
		format string
		keys   []string
	}{
		{"pz_ini", []string{"PublicName", "Public", "DefaultPort", "SteamVAC"}},
		{"lua_sandboxvars", []string{"Zombies", "ZombieConfig.PopulationMultiplier"}},
		{"java_properties", []string{"motd", "max-players", "rcon.password", "query.port", "eula"}},
		{"xml_property_list", []string{"ServerName", "GameWorld", "TelnetPort"}},
		{"ue_option_settings", []string{"ServerName", "PublicPort", "RCONEnabled"}},
		{"json_path", []string{"name", "gameSettings.weatherFrequency"}},
		{"toml_kv", []string{"port", "network.bind"}},
		{"owned_lines", []string{OwnedLinesKey}},
	}
	for _, tt := range tests {
		c, _ := Lookup(tt.format)
		for _, k := range tt.keys {
			if err := c.KeySyntax(k); err != nil {
				t.Errorf("%s: KeySyntax(%q) = %v; the shipped manifests use this key", tt.format, k, err)
			}
		}
	}
}

// Section 8.6 asks for check 6 to be proved over games/*.yaml itself, not over
// a hand-written key list that can drift from what actually ships. It lives
// here because internal/spec must never import this package, so this is the one
// side of the seam that can hold both.
func TestEveryShippedManifestKeyPassesItsCodec(t *testing.T) {
	files, err := fs.Glob(games.FS, "*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped manifests: %v", err)
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			raw, err := fs.ReadFile(games.FS, f)
			if err != nil {
				t.Fatal(err)
			}
			m, diags, err := spec.ValidateBytes(f, raw)
			if err != nil || diags.HasErrors() {
				t.Fatalf("shipped manifest does not validate: %v\n%s", err, diags)
			}

			byName := map[string]spec.ConfigFile{}
			for i, cf := range m.Spec.Configs {
				byName[cf.Name] = cf
				c, ok := Lookup(cf.Format)
				if !ok {
					t.Fatalf("config_files[%d] names format %q, which no codec provides", i, cf.Format)
				}
				for k := range cf.Keys {
					if err := c.KeySyntax(k); err != nil {
						t.Errorf("config_files[%d].keys.%s: %v", i, k, err)
					}
				}
				for j, k := range cf.DenyKeys {
					if err := c.KeySyntax(k); err != nil {
						t.Errorf("config_files[%d].deny_keys[%d] (%q): %v", i, j, k, err)
					}
				}
			}
			for i, p := range m.Spec.Ports {
				if p.ConfigKey == "" {
					continue
				}
				cf, ok := byName[p.ConfigFile]
				if !ok {
					if len(m.Spec.Configs) == 0 {
						t.Fatalf("ports[%d] writes %q with no config file to write it to", i, p.ConfigKey)
					}
					cf = m.Spec.Configs[0]
				}
				c, _ := Lookup(cf.Format)
				if err := c.KeySyntax(p.ConfigKey); err != nil {
					t.Errorf("ports[%d].config_key: %v", i, err)
				}
			}
		})
	}
}

func TestRegisterRefusesADuplicateName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a second codec under an existing name was allowed; " +
				"the winner would depend on package init order")
		}
	}()
	Register(iniCodec{})
}

// INI.Set panics on a key it cannot write, which suits its direct callers.
// Reached through Doc it is one entry in a loop over a manifest's keys, where a
// crash would take down the whole apply instead of reporting one bad key.
func TestINIDocReturnsAnErrorWhereINISetWouldPanic(t *testing.T) {
	c, _ := Lookup("pz_ini")
	doc, err := c.Parse([]byte("Public=false\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Doc.Set panicked instead of returning an error: %v", r)
		}
	}()
	for _, bad := range []string{"a=b", "a\nb", ""} {
		if err := doc.Set(bad, "1"); err == nil {
			t.Errorf("Doc.Set(%q) was accepted", bad)
		}
	}
	if got := doc.Bytes(); string(got) != "Public=false\n" {
		t.Errorf("a refused Set modified the file: %q", got)
	}
}

// pz_ini prefix-matches "key=" so a write to Public cannot land on PublicName.
// The Doc surface must inherit that, not re-derive it.
func TestINIDocKeepsThePrefixMatchRule(t *testing.T) {
	c, _ := Lookup("pz_ini")
	doc, _ := c.Parse(readSample(t, "servertest.ini"))
	if err := doc.Set("Public", "true"); err != nil {
		t.Fatal(err)
	}
	if v, _ := doc.Get("PublicName"); v != "My PZ Server" {
		t.Errorf("writing Public changed PublicName to %q", v)
	}
	if v, _ := doc.Get("Public"); v != "true" {
		t.Errorf("Public = %q", v)
	}
}
