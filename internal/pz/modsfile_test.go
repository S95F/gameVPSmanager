package pz

import (
	"strings"
	"testing"
	"time"
)

const modsTxt = `# Workshop IDs from the collection.
# ORDER MATTERS. Libraries and frameworks must come first.

# --- Libraries / frameworks (must load first) ---
3171167894   # that DAMN Library          (required by all KI5 vehicle mods)
3508537032   # NeatUI Framework [B42]

# --- Vehicles ---
3435796523   # '88 Toyota Hilux
3330403100   # Trailers!
`

var syncDate = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

func TestModsFileIDs(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	want := []string{"3171167894", "3508537032", "3435796523", "3330403100"}
	got := m.IDs()
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestModsFileIgnoresCommentOnlyLines(t *testing.T) {
	m := ParseModsFile([]byte("# 1234567890 this is commented out\n999\n"))
	got := m.IDs()
	if len(got) != 1 || got[0] != "999" {
		t.Errorf("IDs() = %v, want [999]; a commented id must not be enabled", got)
	}
}

func TestModsFileRoundTripUnchanged(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	if got := m.String(); got != modsTxt {
		t.Errorf("round trip changed the file:\n%s", got)
	}
}

// Load order is hand-tuned and the comments explain why, so a sync must keep
// existing lines exactly where they are.
func TestSyncCollectionPreservesOrderAndComments(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	res := m.SyncCollection([]CollectionItem{
		{ID: "3435796523", Title: "'88 Toyota Hilux"},
		{ID: "3171167894", Title: "that DAMN Library"},
		{ID: "3330403100", Title: "Trailers!"},
		{ID: "3508537032", Title: "NeatUI Framework [B42]"},
	}, syncDate)

	if res.Changed() {
		t.Errorf("a collection matching the file reported changes: %+v", res)
	}
	if got := m.String(); got != modsTxt {
		t.Errorf("an unchanged sync rewrote the file:\n%s", got)
	}
}

// A new item's correct load position is not knowable from the collection, so
// it is appended and left for a human to move if it turns out to be a library.
func TestSyncCollectionAppendsNewItems(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	res := m.SyncCollection([]CollectionItem{
		{ID: "3171167894", Title: "that DAMN Library"},
		{ID: "3508537032", Title: "NeatUI Framework [B42]"},
		{ID: "3435796523", Title: "'88 Toyota Hilux"},
		{ID: "3330403100", Title: "Trailers!"},
		{ID: "2503622437", Title: "Skill Recovery Journal"},
	}, syncDate)

	if len(res.Added) != 1 || res.Added[0].ID != "2503622437" {
		t.Fatalf("Added = %+v, want the one new item", res.Added)
	}
	out := m.String()
	if !strings.Contains(out, "# --- Added from collection 2026-09-04 ---") {
		t.Errorf("dated header missing:\n%s", out)
	}
	if !strings.Contains(out, "2503622437   # Skill Recovery Journal") {
		t.Errorf("new item line missing or misformatted:\n%s", out)
	}
	// It must land at the bottom, never in the libraries block at the top.
	libIdx := strings.Index(out, "3171167894")
	newIdx := strings.Index(out, "2503622437")
	if newIdx < libIdx {
		t.Error("new item was inserted above the libraries block")
	}
}

func TestSyncCollectionDropsDelistedItems(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	res := m.SyncCollection([]CollectionItem{
		{ID: "3171167894", Title: "that DAMN Library"},
		{ID: "3508537032", Title: "NeatUI Framework [B42]"},
		{ID: "3435796523", Title: "'88 Toyota Hilux"},
	}, syncDate)

	if len(res.Dropped) != 1 || res.Dropped[0] != "3330403100" {
		t.Fatalf("Dropped = %v, want [3330403100]", res.Dropped)
	}
	if m.Has("3330403100") {
		t.Error("the delisted item is still in the file")
	}
	// Its surrounding structure must survive.
	out := m.String()
	if !strings.Contains(out, "# --- Vehicles ---") {
		t.Error("section comment was removed along with the item")
	}
	if !strings.Contains(out, "3435796523") {
		t.Error("a sibling item was removed")
	}
}

func TestSyncCollectionAddsAndDropsTogether(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	res := m.SyncCollection([]CollectionItem{
		{ID: "3171167894", Title: "that DAMN Library"},
		{ID: "3508537032", Title: "NeatUI Framework [B42]"},
		{ID: "3435796523", Title: "'88 Toyota Hilux"},
		{ID: "9999999999", Title: "Brand New Mod"},
	}, syncDate)

	if len(res.Added) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("Added=%v Dropped=%v, want one of each", res.Added, res.Dropped)
	}
	ids := m.IDs()
	want := []string{"3171167894", "3508537032", "3435796523", "9999999999"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("IDs() = %v, want %v", ids, want)
	}
}

// Only one dated header is emitted no matter how many items arrive.
func TestSyncCollectionSingleHeaderForManyAdditions(t *testing.T) {
	m := ParseModsFile([]byte("111\n"))
	m.SyncCollection([]CollectionItem{
		{ID: "111", Title: "Existing"},
		{ID: "222", Title: "New A"},
		{ID: "333", Title: "New B"},
	}, syncDate)

	if n := strings.Count(m.String(), "Added from collection"); n != 1 {
		t.Errorf("emitted %d headers, want 1:\n%s", n, m.String())
	}
}

// Sync must be idempotent: running it twice against the same collection must
// not append the same items again or re-emit the header.
func TestSyncCollectionIsIdempotent(t *testing.T) {
	items := []CollectionItem{
		{ID: "111", Title: "Existing"},
		{ID: "222", Title: "New"},
	}
	m := ParseModsFile([]byte("111\n"))
	m.SyncCollection(items, syncDate)
	first := m.String()

	res := m.SyncCollection(items, syncDate)
	if res.Changed() {
		t.Errorf("second sync reported changes: %+v", res)
	}
	if m.String() != first {
		t.Errorf("second sync altered the file:\nfirst:\n%s\nsecond:\n%s", first, m.String())
	}
}

func TestSyncCollectionEmptyFile(t *testing.T) {
	m := ParseModsFile(nil)
	res := m.SyncCollection([]CollectionItem{{ID: "111", Title: "First"}}, syncDate)
	if len(res.Added) != 1 {
		t.Fatalf("Added = %+v", res.Added)
	}
	if !m.Has("111") {
		t.Error("item not added to an empty file")
	}
}

func TestModsFileHas(t *testing.T) {
	m := ParseModsFile([]byte(modsTxt))
	if !m.Has("3171167894") {
		t.Error("Has() missed a listed id")
	}
	if m.Has("1234567890") {
		t.Error("Has() reported an id that is not listed")
	}
}
