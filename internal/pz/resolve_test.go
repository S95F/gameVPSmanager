package pz

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func modInfo(id, name string, extra ...string) string {
	var b strings.Builder
	b.WriteString("name=" + name + "\n")
	b.WriteString("id=" + id + "\n")
	b.WriteString("poster=poster.png\n")
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	return b.String()
}

func root(name string, files fstest.MapFS) WorkshopRoot {
	return WorkshopRoot{Name: name, FS: files}
}

func TestParseModInfo(t *testing.T) {
	got := ParseModInfo([]byte("name = That DAMN Library\r\nid = TDL\r\nversionMin=42\r\n"))
	if got.ID != "TDL" {
		t.Errorf("ID = %q, want TDL", got.ID)
	}
	if got.Name != "That DAMN Library" {
		t.Errorf("Name = %q", got.Name)
	}
}

// Only the first assignment counts, matching grep -m1 and the game itself.
func TestParseModInfoTakesFirstIDOnly(t *testing.T) {
	got := ParseModInfo([]byte("id=first\nid=second\n"))
	if got.ID != "first" {
		t.Errorf("ID = %q, want first", got.ID)
	}
}

// A key that merely contains "id" must not be mistaken for the id assignment.
func TestParseModInfoIgnoresLookalikeKeys(t *testing.T) {
	got := ParseModInfo([]byte("modid=wrong\ngridsize=9\nid=right\n"))
	if got.ID != "right" {
		t.Errorf("ID = %q, want right", got.ID)
	}
}

func TestResolveSimpleItem(t *testing.T) {
	r := root("/data/workshop", fstest.MapFS{
		"3171167894/mods/TDL/mod.info":        {Data: []byte(modInfo("TDL", "that DAMN Library"))},
		"3171167894/mods/TDL/42/x.lua":        {Data: []byte("-- b42")},
		"3437629766/mods/CleanUI/42/mod.info": {Data: []byte(modInfo("CleanUI", "CleanUI"))},
	})

	res := Resolve([]WorkshopRoot{r}, []string{"3171167894", "3437629766"})

	if got, want := res.ModsLine(), "TDL;CleanUI"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
	if got, want := res.WorkshopItemsLine(), "3171167894;3437629766"; got != want {
		t.Errorf("WorkshopItemsLine = %q, want %q", got, want)
	}
	for _, it := range res.Items {
		if it.Status != StatusOK {
			t.Errorf("item %s status = %s, want ok", it.WorkshopID, it.Status)
		}
	}
}

// The core silent-failure case: an item shipping both a B41 and a B42 build,
// each with its own internal id. Enabling the B41 id on a B42 server downloads
// the item and never loads the mod, with nothing logged.
func TestResolvePrefersB42Variant(t *testing.T) {
	r := root("/data/workshop", fstest.MapFS{
		"2866258937/mods/MiniHealthPanel/41/mod.info": {Data: []byte(modInfo("MiniHealthPanel41", "Mini Health Panel"))},
		"2866258937/mods/MiniHealthPanel/42/mod.info": {Data: []byte(modInfo("MiniHealthPanel42", "Mini Health Panel"))},
	})

	res := Resolve([]WorkshopRoot{r}, []string{"2866258937"})

	if got, want := res.ModsLine(), "MiniHealthPanel42"; got != want {
		t.Fatalf("ModsLine = %q, want %q (the B41 variant must be dropped)", got, want)
	}
	it := res.Items[0]
	if it.Status != StatusOK {
		t.Errorf("status = %s, want ok", it.Status)
	}
	if len(it.Dropped) != 1 || it.Dropped[0] != "MiniHealthPanel41" {
		t.Errorf("Dropped = %v, want [MiniHealthPanel41]", it.Dropped)
	}
}

// versionMin is the other explicit B42 marker, for items that do not use a
// 42/ folder.
func TestResolveVersionMinMarker(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"111/mods/Old/mod.info": {Data: []byte(modInfo("OldMod", "Old"))},
		"111/mods/New/mod.info": {Data: []byte(modInfo("NewMod", "New", "versionMin=42.0"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"111"})
	if got, want := res.ModsLine(), "NewMod"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
}

// An item genuinely containing two B42 mods keeps both.
func TestResolveMultiModItem(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"222/mods/A/42/mod.info": {Data: []byte(modInfo("ModA", "A"))},
		"222/mods/B/42/mod.info": {Data: []byte(modInfo("ModB", "B"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"222"})

	if res.Items[0].Status != StatusMulti {
		t.Errorf("status = %s, want multi", res.Items[0].Status)
	}
	if got, want := res.ModsLine(), "ModA;ModB"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
}

// With no build evidence anywhere, every id is enabled and the item is flagged
// for a human to check, rather than guessed at.
func TestResolveAmbiguousItemIsFlagged(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"333/mods/Thing/mod.info": {Data: []byte(modInfo("Thing", "Thing"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"333"})

	if res.Items[0].Status != StatusCheck {
		t.Errorf("status = %s, want check", res.Items[0].Status)
	}
	if len(res.Ambiguous) != 1 || res.Ambiguous[0] != "333" {
		t.Errorf("Ambiguous = %v, want [333]", res.Ambiguous)
	}
	if got := res.ModsLine(); got != "Thing" {
		t.Errorf("ModsLine = %q; ambiguous items are still enabled", got)
	}
}

// A common/ or 42/ media directory is weaker evidence than an explicit marker
// but still beats nothing.
func TestResolveLayoutEvidenceOutranksNone(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"444/mods/Plain/mod.info":          {Data: []byte(modInfo("PlainMod", "Plain"))},
		"444/mods/Layout/mod.info":         {Data: []byte(modInfo("LayoutMod", "Layout"))},
		"444/mods/Layout/common/media.txt": {Data: []byte("x")},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"444"})
	if got, want := res.ModsLine(), "LayoutMod"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
	if res.Items[0].Status != StatusOK {
		t.Errorf("status = %s, want ok", res.Items[0].Status)
	}
}

// An item that has not downloaded must still appear in WorkshopItems=, since
// that is what makes the server retry the download on its next boot.
func TestResolveMissingItemStaysInWorkshopItems(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"111/mods/A/42/mod.info": {Data: []byte(modInfo("ModA", "A"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"111", "999"})

	if got, want := res.WorkshopItemsLine(), "111;999"; got != want {
		t.Errorf("WorkshopItemsLine = %q, want %q", got, want)
	}
	if got, want := res.ModsLine(), "ModA"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "999" {
		t.Errorf("Missing = %v, want [999]", res.Missing)
	}
	if res.Items[1].Status != StatusMissing {
		t.Errorf("status = %s, want missing", res.Items[1].Status)
	}
}

func TestResolveEmptyItemFolder(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"555/readme.txt": {Data: []byte("nothing here")},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"555"})
	if res.Items[0].Status != StatusEmpty {
		t.Errorf("status = %s, want empty", res.Items[0].Status)
	}
	if res.ModsLine() != "" {
		t.Errorf("ModsLine = %q, want empty", res.ModsLine())
	}
}

// The server writes workshop content to different places depending on version
// and both roots can exist at once; the first hit wins.
func TestResolveFirstRootWins(t *testing.T) {
	a := root("/data", fstest.MapFS{
		"111/mods/A/42/mod.info": {Data: []byte(modInfo("FromData", "A"))},
	})
	b := root("/opt", fstest.MapFS{
		"111/mods/A/42/mod.info": {Data: []byte(modInfo("FromOpt", "A"))},
		"222/mods/B/42/mod.info": {Data: []byte(modInfo("OnlyInOpt", "B"))},
	})
	res := Resolve([]WorkshopRoot{a, b}, []string{"111", "222"})
	if got, want := res.ModsLine(), "FromData;OnlyInOpt"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
	if !strings.HasPrefix(res.Items[0].Dir, "/data/") {
		t.Errorf("item 111 resolved from %q, want the first root", res.Items[0].Dir)
	}
}

// Load order is the order of mods.txt, and duplicates collapse to their first
// appearance.
func TestResolvePreservesOrderAndDeduplicates(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"1/mods/L/42/mod.info": {Data: []byte(modInfo("Library", "L"))},
		"2/mods/M/42/mod.info": {Data: []byte(modInfo("Mod", "M"))},
		"3/mods/N/42/mod.info": {Data: []byte(modInfo("Library", "dup"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"1", "2", "3"})
	if got, want := res.ModsLine(), "Library;Mod"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
}

// mod.info nested deeper than the walk limit is not a layout PZ recognises.
func TestResolveRespectsDepthLimit(t *testing.T) {
	deep := "666/" + strings.Repeat("a/", maxModInfoDepth+2) + "mod.info"
	r := root("/w", fstest.MapFS{
		deep: {Data: []byte(modInfo("TooDeep", "Deep"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"666"})
	if res.ModsLine() != "" {
		t.Errorf("ModsLine = %q; a mod.info below the depth limit must be ignored", res.ModsLine())
	}
}

// Both PZ lists are semicolon-separated. A comma silently disables everything.
func TestResolveUsesSemicolonSeparator(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"1/mods/A/42/mod.info": {Data: []byte(modInfo("A", "A"))},
		"2/mods/B/42/mod.info": {Data: []byte(modInfo("B", "B"))},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"1", "2"})
	if strings.Contains(res.ModsLine(), ",") || strings.Contains(res.WorkshopItemsLine(), ",") {
		t.Error("a comma appeared in a PZ list; it silently breaks the whole list")
	}
	if got, want := res.ModsLine(), "A;B"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
}

// An unreadable subtree must not abort resolution of the rest of the item.
func TestResolveToleratesWalkErrors(t *testing.T) {
	r := root("/w", fstest.MapFS{
		"777/mods/Good/42/mod.info": {Data: []byte(modInfo("GoodMod", "Good"))},
		"777/mods/Bad/mod.info":     {Data: []byte("no id assignment here\n")},
	})
	res := Resolve([]WorkshopRoot{r}, []string{"777"})
	if got, want := res.ModsLine(), "GoodMod"; got != want {
		t.Errorf("ModsLine = %q, want %q", got, want)
	}
}

var _ fs.FS = fstest.MapFS(nil)

// TestFindModInfosSkipsAnythingThatIsNotARegularFile.
//
// The declared read roots are inside the instance's own install and data trees,
// which ARCHITECTURE 18.1's first row hands outright to a compromised workshop
// item -- so every byte under them is third-party input, and `mod.info` is a
// name an item chooses.
//
// A FIFO named mod.info blocks the open forever, which holds the job lease with
// the server stopped: a hang, not a failure, and the one failure mode a
// shutdown ladder cannot recover from. A symlink named mod.info is a read of a
// file the item did not ship. Three other readers in this tree already check
// the dirent type for exactly these reasons (steamcmd/buildid.go walks a
// Steam-written tree with !d.Type().IsRegular() so "a symlink named
// appinfo.vdf must not be followed"; content/digest.go records a non-regular
// entry instead of reading it); this was the only untrusted-bytes reader with
// neither guard.
func TestFindModInfosSkipsAnythingThatIsNotARegularFile(t *testing.T) {
	for name, mode := range map[string]fs.FileMode{
		"a named pipe blocks the open forever":         fs.ModeNamedPipe,
		"a symlink reads a file the item did not ship": fs.ModeSymlink,
		"a device node": fs.ModeDevice | fs.ModeCharDevice,
		"a socket":      fs.ModeSocket,
	} {
		t.Run(name, func(t *testing.T) {
			files := fstest.MapFS{
				// The bait, as the item would plant it.
				"777/mods/Evil/mod.info": &fstest.MapFile{
					Data: []byte(modInfo("EvilMod", "Evil", "versionMin=42")),
					Mode: mode,
				},
				// A real one beside it, so the skip is a skip and not an abort.
				"777/mods/Good/mod.info": &fstest.MapFile{
					Data: []byte(modInfo("GoodMod", "Good", "versionMin=42")),
				},
			}
			res := Resolve([]WorkshopRoot{root("r", files)}, []string{"777"})
			if len(res.Items) != 1 {
				t.Fatalf("items = %d", len(res.Items))
			}
			if got := res.Items[0].ModIDs; len(got) != 1 || got[0] != "GoodMod" {
				t.Fatalf("mod ids = %v, want only GoodMod: a %v entry named mod.info was read",
					got, mode)
			}
		})
	}
}

// TestFindModInfosBoundsWhatItReads.
//
// fs.ReadFile allocates whatever it is handed, and the bytes are then
// re-materialised twice more as strings inside the parse -- so one oversized
// file named mod.info is several times its own size in a root process. A real
// mod.info is a few hundred bytes; the bound is three orders of magnitude above
// that, which is the same reasoning steamcmd/acf.go's maxKVBytes records ("a
// parser that will allocate whatever it is handed is a denial of service with a
// friendly name").
//
// An item whose only mod.info is skipped lands as StatusEmpty, which the
// content resolver already renders as `modinfo:no-usable-mod-info` -- so the
// skip is reported rather than silent.
func TestFindModInfosBoundsWhatItReads(t *testing.T) {
	huge := modInfo("HugeMod", "Huge", "versionMin=42") +
		strings.Repeat("# padding padding padding\n", (maxModInfoBytes/26)+64)
	if len(huge) <= maxModInfoBytes {
		t.Fatalf("the fixture is %d bytes and the bound is %d", len(huge), maxModInfoBytes)
	}
	files := fstest.MapFS{
		"888/mods/Huge/mod.info": &fstest.MapFile{Data: []byte(huge)},
		"888/mods/Good/mod.info": &fstest.MapFile{
			Data: []byte(modInfo("GoodMod", "Good", "versionMin=42")),
		},
	}
	res := Resolve([]WorkshopRoot{root("r", files)}, []string{"888"})
	if len(res.Items) != 1 {
		t.Fatalf("items = %d", len(res.Items))
	}
	if got := res.Items[0].ModIDs; len(got) != 1 || got[0] != "GoodMod" {
		t.Fatalf("mod ids = %v, want only GoodMod", got)
	}

	// And a file at exactly the bound is still read: the check must be a
	// bound, not an off-by-one that drops legitimate items.
	ok := modInfo("BigMod", "Big", "versionMin=42")
	ok += strings.Repeat("#", maxModInfoBytes-len(ok))
	res = Resolve([]WorkshopRoot{root("r", fstest.MapFS{
		"999/mods/Big/mod.info": &fstest.MapFile{Data: []byte(ok)},
	})}, []string{"999"})
	if got := res.ModIDs; len(got) != 1 || got[0] != "BigMod" {
		t.Fatalf("mod ids = %v; a file at exactly the bound was dropped", got)
	}
}
