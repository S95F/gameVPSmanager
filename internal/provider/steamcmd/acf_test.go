package steamcmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseFixture(t *testing.T, name string) *AppManifest {
	t.Helper()
	m, err := ParseAppManifest(strings.NewReader(string(fixture(t, name))))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func TestParseAppManifestReadsEveryFactVerifyNeeds(t *testing.T) {
	m := parseFixture(t, "appmanifest_ok.acf")

	if m.AppID != 380870 {
		t.Errorf("appid = %d, want 380870", m.AppID)
	}
	if m.Name != "Project Zomboid Dedicated Server" {
		t.Errorf("name = %q", m.Name)
	}
	if !m.HasStateFlags || m.StateFlags != StateInstalled {
		t.Errorf("StateFlags = %d (present %t), want 4", m.StateFlags, m.HasStateFlags)
	}
	if !m.Installed() {
		t.Error("a StateFlags 4 manifest does not report installed")
	}
	if m.BuildID != "18452901" {
		t.Errorf("buildid = %q", m.BuildID)
	}
	if !m.HasDownloadPair || m.BytesDownloaded != m.BytesToDownload {
		t.Errorf("download pair = %d/%d (present %t)", m.BytesDownloaded, m.BytesToDownload,
			m.HasDownloadPair)
	}
	if !m.HasStagePair || m.BytesStaged != m.BytesToStage {
		t.Errorf("stage pair = %d/%d (present %t)", m.BytesStaged, m.BytesToStage, m.HasStagePair)
	}
	if m.SizeOnDisk != 3461823232 {
		t.Errorf("SizeOnDisk = %d", m.SizeOnDisk)
	}
	if got := m.LastUpdated.Unix(); got != 1757461234 {
		t.Errorf("LastUpdated = %v", m.LastUpdated)
	}

	// The nested blocks are reachable, which is the half a line-oriented grep
	// cannot do: this "size" belongs to a depot, not to the app.
	if got := m.Root.Str("InstalledDepots", "380871", "size"); got != "3461823232" {
		t.Errorf("depot size = %q", got)
	}
	if got := m.Root.Str("UserConfig", "language"); got != "english" {
		t.Errorf("UserConfig.language = %q", got)
	}
}

// 1062 is the case that makes a bare "StateFlags != 4" message useless: the
// fully-installed bit IS set, alongside "files missing".
func TestParseAppManifestStateFlags1062(t *testing.T) {
	m := parseFixture(t, "appmanifest_1062.acf")
	if m.StateFlags != 1062 {
		t.Fatalf("StateFlags = %d, want 1062", m.StateFlags)
	}
	if m.Installed() {
		t.Error("1062 reports as installed")
	}
	desc := StateDescription(m.StateFlags)
	for _, want := range []string{"fully installed", "update required", "files missing",
		"update started"} {
		if !strings.Contains(desc, want) {
			t.Errorf("StateDescription(1062) = %q, missing %q", desc, want)
		}
	}
	if strings.Contains(desc, "unknown bits") {
		t.Errorf("1062 decoded with leftover bits: %q", desc)
	}
}

func TestParseAppManifestShortDownload(t *testing.T) {
	m := parseFixture(t, "appmanifest_bytes_mismatch.acf")
	if !m.HasStateFlags || m.StateFlags != StateInstalled {
		t.Fatalf("the fixture is meant to isolate the byte check: StateFlags = %d", m.StateFlags)
	}
	if !m.HasDownloadPair {
		t.Fatal("the download pair is absent")
	}
	if m.BytesDownloaded >= m.BytesToDownload {
		t.Fatalf("the fixture is not short: %d >= %d", m.BytesDownloaded, m.BytesToDownload)
	}
	if m.BytesStaged != m.BytesToStage {
		t.Error("the fixture also mismatches the stage pair, so it no longer isolates one failure")
	}
}

// Complete() is the same arithmetic Verify.checkBytes performs, collapsed to
// one boolean. Two spellings of one rule is how the poll and the doctor start
// disagreeing about the same file, so the agreement is asserted rather than
// reviewed.
func TestCompleteAgreesWithTheVerifyChecks(t *testing.T) {
	for _, name := range []string{
		"appmanifest_ok.acf", "appmanifest_1062.acf", "appmanifest_bytes_mismatch.acf",
	} {
		t.Run(name, func(t *testing.T) {
			m := parseFixture(t, name)
			r := &Report{AppID: m.AppID, ACFPath: "x"}
			r.checkBytes(m)
			bytesOK := r.Err() == nil
			if got, want := m.Complete(), m.Installed() && bytesOK; got != want {
				t.Errorf("Complete() = %t, but Installed() && checkBytes-passed = %t", got, want)
			}
		})
	}
	t.Run("an absent pair is not a pass", func(t *testing.T) {
		// The reason the Has* fields exist: a manifest with no BytesToDownload
		// key would otherwise pass by having nothing to compare.
		m := &AppManifest{HasStateFlags: true, StateFlags: StateInstalled}
		if m.Complete() {
			t.Error("Complete() is true for a manifest carrying neither byte pair")
		}
	})
}

func TestKeyValuesGrammar(t *testing.T) {
	// Everything the format actually does, in one document: a comment, escapes
	// inside a quoted value, a bare token, an empty block, a nested block, a
	// platform conditional, and a repeated key at two levels.
	doc := `// written by a thing that is not gvm
"AppState"
{
	"name"		"a \"quoted\" name\twith a tab"
	"path"		"C:\depots\380871"
	bare		token
	"StateFlags"	"4"
	"empty"
	{
	}
	"nested"
	{
		"StateFlags"	"1026"
		"deep"
		{
			"leaf"	"yes"	[$WIN32]
		}
	}
}
`
	root, err := ParseKeyValues(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	app, ok := root.Child("appstate")
	if !ok {
		t.Fatal("the root block was not found by a case-insensitive lookup")
	}
	if got, want := app.Str("name"), "a \"quoted\" name\twith a tab"; got != want {
		t.Errorf("escapes: got %q want %q", got, want)
	}
	// An unknown escape keeps its backslash, because a Windows depot path is a
	// far more likely explanation than an escape this parser has not heard of.
	if got, want := app.Str("path"), `C:\depots\380871`; got != want {
		t.Errorf("windows path: got %q want %q", got, want)
	}
	if got := app.Str("bare"); got != "token" {
		t.Errorf("unquoted pair: %q", got)
	}
	empty, ok := app.Child("empty")
	if !ok || !empty.Block || len(empty.Children) != 0 {
		t.Errorf("empty block: %+v", empty)
	}
	if empty.Value != "" {
		t.Error("an empty block came back with a value, so it is indistinguishable from an " +
			"empty string")
	}
	if got := app.Str("StateFlags"); got != "4" {
		t.Errorf("outer StateFlags = %q; the nested one won", got)
	}
	if got := app.Str("nested", "StateFlags"); got != "1026" {
		t.Errorf("nested StateFlags = %q", got)
	}
	if got := app.Str("nested", "deep", "leaf"); got != "yes" {
		t.Errorf("a [$WIN32] conditional ate the value: %q", got)
	}
}

func TestKeyValuesRefusals(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"an unterminated quote", `"AppState" { "name" "half`},
		{"an unclosed block", `"AppState" { "name" "x"`},
		{"a stray closing brace at the top level", `"a" "b" }`},
		{"an include directive", "#base \"other.acf\"\n\"AppState\" { }"},
		{"a lone slash", `"AppState" { / "x" "y" }`},
		{"a block where a key belongs", `{ "a" "b" }`},
		{"a key followed by a closing brace", `"AppState" { "name" }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseKeyValues(strings.NewReader(tc.doc)); err == nil {
				t.Fatal("parsed without complaint; a truncated document must never be read as " +
					"the keys it managed to reach")
			} else if !errors.Is(err, ErrKeyValues) {
				t.Errorf("err = %v, want ErrKeyValues", err)
			}
		})
	}
}

func TestKeyValuesRefusesRunawayNesting(t *testing.T) {
	doc := `"a"` + strings.Repeat("{\"b\"", maxKVDepth+2) + "\"c\""
	if _, err := ParseKeyValues(strings.NewReader(doc)); err == nil {
		t.Fatal("a document nested past the depth limit parsed")
	}
}

// An absent number and a zero must never be the same answer: a missing
// BytesToDownload read as 0 makes the byte-parity check pass by having nothing
// to compare.
func TestIntDistinguishesAbsentFromZero(t *testing.T) {
	root, err := ParseKeyValues(strings.NewReader(`"AppState" { "zero" "0" "words" "many" }`))
	if err != nil {
		t.Fatal(err)
	}
	app, _ := root.Child("AppState")
	if v, ok := app.Int("zero"); !ok || v != 0 {
		t.Errorf(`Int("zero") = %d, %t`, v, ok)
	}
	if _, ok := app.Int("absent"); ok {
		t.Error("a missing key reported a number")
	}
	if _, ok := app.Int("words"); ok {
		t.Error("a non-numeric value reported a number")
	}
	if _, ok := app.Uint("absent"); ok {
		t.Error("Uint invented a value too")
	}
}

func TestParseAppManifestRefusesAnotherDocument(t *testing.T) {
	// appworkshop_108600.acf lives in the same directory and is not this.
	_, err := ParseAppManifest(strings.NewReader(`"AppWorkshop" { "appid" "108600" }`))
	if !errors.Is(err, ErrNotAppManifest) {
		t.Fatalf("err = %v, want ErrNotAppManifest", err)
	}
	if _, err := ParseAppManifest(strings.NewReader(`"AppState" { "name" "x" }`)); !errors.Is(err,
		ErrNotAppManifest) {
		t.Fatalf("an AppState with no appid: err = %v", err)
	}
}

const appInfoOutput = `Redirecting stderr to '/home/x/.local/share/Steam/logs/stderr.txt'
Steam Console Client (c) Valve Corporation - version 1757457223
-- type 'quit' to exit --
Loading Steam API...OK
Connecting anonymously to Steam Public...OK
Waiting for client config...OK
Waiting for user info...OK
AppID : 380870, change number : 25881818/0, last change : Tue Sep  9 18:00:00 2025
"380870"
{
	"common"
	{
		"name"		"Project Zomboid Dedicated Server"
		"type"		"Tool"
	}
	"depots"
	{
		"380871"
		{
			"config"
			{
				"oslist"		"linux"
			}
			"maxsize"		"3461823232"
		}
		"380872"
		{
			"config"
			{
				"oslist"		"windows"
			}
			"maxsize"		"4000000000"
		}
		"branches"
		{
			"public"
			{
				"buildid"		"18461233"
				"timeupdated"		"1757461234"
			}
			"unstable"
			{
				"buildid"		"18470001"
				"description"		"B42 unstable"
				"pwdrequired"		"1"
			}
		}
	}
}
Steam>quit
`

func TestParseAppInfoSkipsThePreambleAndTheTrailingConsole(t *testing.T) {
	node, err := ParseAppInfo(strings.NewReader(appInfoOutput), 380870)
	if err != nil {
		t.Fatal(err)
	}
	if got := node.Str("common", "name"); got != "Project Zomboid Dedicated Server" {
		t.Errorf("common.name = %q", got)
	}
	if got := node.Str("depots", "branches", "public", "buildid"); got != "18461233" {
		t.Errorf("public buildid = %q", got)
	}
}

// "Steam told us nothing about this app" and "this app has no buildid" lead to
// different places: the first is usually a failed login or a stale appinfo
// cache.
func TestParseAppInfoRefusesAMissingBlock(t *testing.T) {
	_, err := ParseAppInfo(strings.NewReader(appInfoOutput), 896660)
	if !errors.Is(err, ErrNoAppInfo) {
		t.Fatalf("err = %v, want ErrNoAppInfo", err)
	}
	if !strings.Contains(err.Error(), "896660") {
		t.Errorf("the refusal does not name the app it looked for: %v", err)
	}
}

func TestParseAppInfoRefusesAHeaderWithNoBlock(t *testing.T) {
	if _, err := ParseAppInfo(strings.NewReader("\"380870\"\n\"not a block\"\n"), 380870); err == nil {
		t.Fatal("a header with no block parsed")
	}
}
