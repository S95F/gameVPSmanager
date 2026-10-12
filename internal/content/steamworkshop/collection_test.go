package steamworkshop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
)

const fixtureCollection = "3374983339"

func TestCollectionCrossChecksEveryChild(t *testing.T) {
	s := newStub(t)
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if c.From != CollectionFromAPI {
		t.Errorf("from = %q", c.From)
	}
	if !c.Verified {
		t.Fatalf("the API path left the collection unverified: %s", c.Summary())
	}
	if err := c.CheckAccounted(); err != nil {
		t.Fatalf("CheckAccounted: %v", err)
	}
	if got := c.IDs(); !slices.Equal(got,
		ids("3171167894", "3508537032", "3435796523", "2866258937")) {
		t.Fatalf("children = %v", got)
	}
	if c.Declared != 4 || len(c.Items) != 4 {
		t.Errorf("declared %d, items %d", c.Declared, len(c.Items))
	}

	lib, _ := c.Item("3171167894")
	if lib.Title != "that DAMN Library" {
		t.Errorf("title = %q; the title comes from the metadata on the API path", lib.Title)
	}
	if lib.Upstream == nil || lib.Upstream.ConsumerAppID != pzWorkshopAppID {
		t.Errorf("upstream = %+v", lib.Upstream)
	}
	// The collection's own sortorder is RECORDED and never used for load
	// order: a collection cannot know that that DAMN Library has to load
	// before the vehicle mods that need it.
	if lib.Order != 0 || lib.Upstream.UpstreamOrder != 0 {
		t.Errorf("order = %d / %d", lib.Order, lib.Upstream.UpstreamOrder)
	}
	hilux, _ := c.Item("3435796523")
	if hilux.Order != 2 || hilux.Upstream.UpstreamOrder != 2 {
		t.Errorf("sortorder was not recorded: %+v", hilux)
	}

	// A banned child is KEPT and flagged. gvm does not delete a mod because
	// the Workshop has an opinion about it.
	banned, _ := c.Item("3508537032")
	if !banned.Banned {
		t.Error("the banned child was not flagged")
	}
	if !slices.Equal(c.Flagged(), ids("3508537032")) {
		t.Errorf("flagged = %v", c.Flagged())
	}
	if !strings.Contains(c.Summary(), "banned or removed") {
		t.Errorf("summary = %q", c.Summary())
	}
}

// TestCollectionRefusesOnResultNine: private or removed, and refusing rather
// than falling through to an empty list is the whole point. An empty list is a
// perfectly valid thing for a collection to be, which is exactly why it cannot
// be the error value.
func TestCollectionRefusesOnResultNine(t *testing.T) {
	s := newStub(t)
	s.collection = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"resultcount":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":9}]}}`))
	}
	_, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if !errors.Is(err, ErrCollectionUnavailable) {
		t.Fatalf("error = %v, want ErrCollectionUnavailable", err)
	}
	// The operator's next move has to be in the message.
	if !strings.Contains(err.Error(), "--allow-html-fallback") {
		t.Errorf("the refusal does not name the fallback: %v", err)
	}
}

// TestCollectionRefusesAnItemFromAnotherGame: a wrong consumer_app_id is not a
// parse error. It is the wrong collection, and rewriting a Project Zomboid mod
// list out of a 7 Days to Die collection is the one outcome worth a hard stop.
func TestCollectionRefusesAnItemFromAnotherGame(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			appID := pzWorkshopAppID
			if id == "3435796523" {
				appID = 294420 // 7 Days to Die
			}
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":%d,"title":"x"}`, id, appID))
		}
		writeJSON(w, []byte(fmt.Sprintf(
			`{"response":{"result":1,"publishedfiledetails":[%s]}}`, strings.Join(out, ","))))
	}
	_, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if !errors.Is(err, ErrWrongGame) {
		t.Fatalf("error = %v, want ErrWrongGame", err)
	}
	for _, want := range []string{"3435796523", "294420", "108600"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestCollectionRefusesWhenAChildIsUnaccountedFor is rebuild.sh's RAW_COUNT !=
// COUNT, on the API path: the request went out, the answer came back, and a
// child was not in it. Neither confirmed nor denied, so nothing is rewritten.
func TestCollectionRefusesWhenAChildIsUnaccountedFor(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			if id == "2866258937" {
				continue // answered the batch without mentioning it
			}
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":%d,"title":"x"}`,
				id, pzWorkshopAppID))
		}
		writeJSON(w, []byte(fmt.Sprintf(
			`{"response":{"result":1,"publishedfiledetails":[%s]}}`, strings.Join(out, ","))))
	}

	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if c.Verified {
		t.Fatal("a collection with an unexplained child was marked verified")
	}
	gate := c.CheckAccounted()
	if !errors.Is(gate, content.ErrUnaccounted) {
		t.Fatalf("CheckAccounted = %v, want content.ErrUnaccounted", gate)
	}
	if !strings.Contains(gate.Error(), "2866258937") {
		t.Errorf("the refusal does not name the child: %v", gate)
	}

	// And the refusal is structural: no merge can be produced from it.
	if _, err := Merge(nil, c); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("Merge off an unaccounted collection = %v; a partial list looks exactly like "+
			"a deliberate one", err)
	}
}

// TestCollectionRefusesAnOddResultForAChild: a code that is neither success
// nor removal is not something to record as "removed" -- that would be gvm
// writing a conclusion Steam did not state.
func TestCollectionRefusesAnOddResultForAChild(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			result, extra := 1, fmt.Sprintf(`,"consumer_app_id":%d,"title":"x"`, pzWorkshopAppID)
			if id == "2866258937" {
				result, extra = 8, ""
			}
			out = append(out, fmt.Sprintf(`{"publishedfileid":%q,"result":%d%s}`, id, result,
				extra))
		}
		writeJSON(w, []byte(fmt.Sprintf(
			`{"response":{"result":1,"publishedfiledetails":[%s]}}`, strings.Join(out, ","))))
	}
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if c.Verified {
		t.Fatal("result 8 on a child was treated as accounted for")
	}
	if it, _ := c.Item("2866258937"); it.Removed {
		t.Error("result 8 was recorded as a removal, which Steam did not say")
	}
}

// TestARemovedChildIsRecordedAndKept: result 9 on a CHILD is the "explicitly
// banned or removed and recorded as such" case of section 12.5. Accounted for,
// kept in the list, reported.
func TestARemovedChildIsRecordedAndKept(t *testing.T) {
	s := newStub(t)
	s.collection = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"3171167894","sortorder":0},
				{"publishedfileid":"9999999999","sortorder":1}]}]}}`))
	}
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if err := c.CheckAccounted(); err != nil {
		t.Fatalf("a recorded removal is accounted for: %v", err)
	}
	gone, ok := c.Item("9999999999")
	if !ok {
		t.Fatal("a removed child was dropped from the list; gvm does not delete a mod the " +
			"operator asked for on the strength of an API answer")
	}
	if !gone.Removed || gone.Upstream != nil {
		t.Errorf("removed child = %+v", gone)
	}

	// Merge keeps it, so the operator sees it and decides.
	m, err := Merge([]content.Ref{{ID: "9999999999", Note: "mine"}}, c)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(m.Dropped) != 0 {
		t.Errorf("dropped = %v", m.Dropped)
	}
}

func TestCollectionRefusesAnEmptyOrMislabelledAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"no children": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[]}]}}`,
		"another collection": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"111","result":1,"children":[
				{"publishedfileid":"3171167894"}]}]}}`,
		"two collections": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[]},
			{"publishedfileid":"111","result":1,"children":[]}]}}`,
		"top level result": `{"response":{"result":15,"collectiondetails":[]}}`,
		"odd child result": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":15}]}}`,
		"child is not an id": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"fabric-api"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.collection = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, []byte(body))
			}
			_, err := s.api(t).Collection(context.Background(), fixtureCollection)
			if err == nil {
				t.Fatal("the answer was accepted")
			}
			if !errors.Is(err, ErrAPI) && !errors.Is(err, ErrCollectionUnavailable) {
				t.Fatalf("error = %v, want an ErrAPI or ErrCollectionUnavailable refusal", err)
			}
		})
	}
}

// TestACollectionListingItselfIsNotAMod: both the page and the API include the
// collection among its own children in some renderings, and the bash filtered
// it out of both counts with `grep -vx "$COLLECTION_ID"`.
func TestACollectionListingItselfIsNotAMod(t *testing.T) {
	s := newStub(t)
	s.collection = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"3374983339","sortorder":0},
				{"publishedfileid":"3171167894","sortorder":1},
				{"publishedfileid":"3171167894","sortorder":2}]}]}}`))
	}
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if !slices.Equal(c.IDs(), ids("3171167894")) {
		t.Fatalf("children = %v; neither the collection itself nor a repeat is a mod", c.IDs())
	}
	if c.Declared != 1 {
		t.Errorf("declared = %d", c.Declared)
	}
}

func TestCollectionRefusesANonNumericCollectionID(t *testing.T) {
	s := newStub(t)
	for _, bad := range []string{"", "hardcore-apocalypse", "33749 83339"} {
		if _, err := s.api(t).Collection(context.Background(), bad); !errors.Is(err, ErrConfig) {
			t.Errorf("Collection(%q) error = %v, want ErrConfig", bad, err)
		}
	}
	if s.collCalls != 0 {
		t.Error("a request went out for something that cannot be a collection id")
	}
}

// TestMergeKeepsPositionsDropsDelistedAndAppendsNew is
// internal/pz.ModsFile.SyncCollection's contract, moved onto the declared
// sequence. The positions are the whole thing: hand-tuned load order is not
// knowable from a collection, and the reasoning behind it lives in comments
// gvm cannot read.
func TestMergeKeepsPositionsDropsDelistedAndAppendsNew(t *testing.T) {
	s := newStub(t)
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	declared := []content.Ref{
		// Deliberately NOT the collection's sortorder: the library is first
		// because the operator put it there.
		{ID: "3171167894", Note: "that DAMN Library (required by all KI5 vehicle mods)"},
		{ID: "2866258937", Note: "Mini Health Panel", Requires: []string{"3171167894"}},
		{ID: "3435796523", Note: "Hilux", Pin: "v3"},
		{ID: "2222222222", Note: "no longer in the collection"},
	}
	m, err := Merge(declared, c)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	var got []string
	for _, r := range m.Refs {
		got = append(got, r.ID)
	}
	// The three known ids keep their declared positions; the delisted one is
	// gone; the new one is APPENDED, never inserted, because a new item's
	// correct load position is unknowable.
	if !slices.Equal(got, []string{"3171167894", "2866258937", "3435796523", "3508537032"}) {
		t.Fatalf("merged order = %v", got)
	}
	if !slices.Equal(m.Dropped, ids("2222222222")) {
		t.Errorf("dropped = %v", m.Dropped)
	}
	if len(m.Added) != 1 || m.Added[0].ID != "3508537032" {
		t.Fatalf("added = %+v", m.Added)
	}
	if m.Refs[3].Note != "Neat UI Framework" {
		t.Errorf("a new ref's note = %q, want the item's title", m.Refs[3].Note)
	}
	// The operator's own data on a known ref survives untouched: a merge that
	// rewrote notes, pins or requires would destroy the record of why an item
	// sits where it does.
	if m.Refs[1].Note != "Mini Health Panel" ||
		!slices.Equal(m.Refs[1].Requires, []string{"3171167894"}) {
		t.Errorf("a known ref was rewritten: %+v", m.Refs[1])
	}
	if m.Refs[2].Pin != "v3" {
		t.Errorf("a known ref lost its pin: %+v", m.Refs[2])
	}
	if !m.Changed() {
		t.Error("Changed() = false after an add and a drop")
	}
}

// TestMergeNeverDropsAVendoredRef: those are operator bytes -- never fetched,
// never pruned -- and a collection has no opinion about them.
func TestMergeNeverDropsAVendoredRef(t *testing.T) {
	s := newStub(t)
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	m, err := Merge([]content.Ref{
		{ID: "9000000001", Note: "my own build", Vendored: true},
		{ID: "9000000002", Note: "not vendored, not in the collection"},
	}, c)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !slices.Equal(m.Kept, ids("9000000001")) {
		t.Errorf("kept = %v, want the vendored ref", m.Kept)
	}
	if !slices.Equal(m.Dropped, ids("9000000002")) {
		t.Errorf("dropped = %v", m.Dropped)
	}
	if m.Refs[0].ID != "9000000001" || !m.Refs[0].Vendored {
		t.Errorf("the vendored ref did not survive in place: %+v", m.Refs[0])
	}
}

// TestMergeLeavesTheOperatorsOwnOdditiesAlone: an entry with no id and a
// duplicate are the operator's to resolve, and a merge that silently tidied
// them would look like the collection had done it.
func TestMergeLeavesTheOperatorsOwnOdditiesAlone(t *testing.T) {
	s := newStub(t)
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	m, err := Merge([]content.Ref{
		{Note: "a line with no id"},
		{ID: "3171167894"},
		{ID: "3171167894", Note: "declared twice"},
	}, c)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(m.Refs) != 3+3 {
		t.Fatalf("merged %d refs: %+v", len(m.Refs), m.Refs)
	}
	if m.Refs[0].Note != "a line with no id" {
		t.Errorf("the id-less entry was dropped: %+v", m.Refs)
	}
	if m.Refs[2].Note != "declared twice" {
		t.Errorf("the duplicate was tidied away: %+v", m.Refs)
	}
}

func TestMergeOfANilCollectionRefuses(t *testing.T) {
	if _, err := Merge(nil, nil); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("error = %v, want content.ErrUnaccounted", err)
	}
	// A composite literal can forge a Collection; it cannot forge a verified
	// one, so the gate still closes.
	if _, err := Merge(nil, &Collection{ID: "1", Items: []CollectionItem{{ID: "2"}},
		Declared: 1}); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("a hand-built collection merged anyway: %v", err)
	}
}

func TestEnrichRefusesNothingToCrossCheck(t *testing.T) {
	s := newStub(t)
	a := s.api(t)
	if err := a.Enrich(context.Background(), nil); !errors.Is(err, ErrInput) {
		t.Errorf("Enrich(nil) = %v", err)
	}
	if err := a.Enrich(context.Background(), &Collection{ID: "1"}); !errors.Is(err, ErrInput) {
		t.Errorf("Enrich of a childless collection = %v", err)
	}
}

// TestEnrichIsTerminalWhenTheMetadataLookupFails is the deliberate asymmetry
// with the resolve path: a resolve must not change which mods a server enables
// when Steam is down, and a REWRITE must not happen at all when Steam is
// half-down.
func TestEnrichIsTerminalWhenTheMetadataLookupFails(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}
	_, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err == nil {
		t.Fatal("a collection was returned with none of its children cross-checked")
	}
	if !strings.Contains(err.Error(), fixtureCollection) {
		t.Errorf("the failure does not name the collection: %v", err)
	}
}

// TestCollectionRefusesANestedCollectionAsAChild is the nested-collection hole.
//
// A sub-collection is a real Workshop feature and GetCollectionDetails is how
// it surfaces: the child comes back with filetype 2 where an ordinary item has
// 0. That is the only reason `filetype` is in the envelope at all.
//
// Nothing downstream can catch it. A collection IS a published file, so
// GetPublishedFileDetails answers for it with result 1, a title and the game's
// own consumer_app_id -- which is precisely what makes Enrich's cross-check
// wave it through. Declared is incremented in the same loop iteration that
// appends the item, so the arithmetic agrees too, and Merge then sends every
// real mod the operator declared to Dropped and appends the sub-collection id
// as the one surviving ref.
func TestCollectionRefusesANestedCollectionAsAChild(t *testing.T) {
	for name, body := range map[string]string{
		"only child is a collection": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"999000111","sortorder":0,"filetype":2}]}]}}`,
		// The partial case is the worse one: the counts still agree, three real
		// mods go to Dropped and an id the server cannot download is appended.
		"one collection among real items": `{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"3171167894","sortorder":0,"filetype":0},
				{"publishedfileid":"999000111","sortorder":1,"filetype":2},
				{"publishedfileid":"3508537032","sortorder":2,"filetype":0}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.collection = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, []byte(body))
			}
			// The details stub answers like the real endpoint does for a
			// collection id: result 1, this game's appid, a title. Without the
			// guard this is what makes the whole walk look healthy.
			s.details = func(w http.ResponseWriter, r *http.Request) {
				var out []string
				for _, id := range askedFor(t, r) {
					out = append(out, `{"publishedfileid":"`+id+
						`","result":1,"consumer_app_id":108600,"title":"PZ Essentials"}`)
				}
				writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
					strings.Join(out, ",")+`]}}`))
			}
			c, err := s.api(t).Collection(context.Background(), fixtureCollection)
			if err == nil {
				t.Fatalf("a nested collection was accepted as a mod: %s", c.Summary())
			}
			if !errors.Is(err, ErrAPI) {
				t.Fatalf("error = %v, want ErrAPI", err)
			}
			// A refusal nobody can act on is a refusal operators route around.
			if !strings.Contains(err.Error(), "999000111") {
				t.Errorf("the refusal does not name the child: %v", err)
			}
		})
	}
}

// TestACollectionListingItselfWithAFileTypeIsStillJustSkipped pins the ORDER of
// the two checks.
//
// A collection that lists itself can carry filetype 2 on that self-reference,
// and the self-reference is not a nested collection -- it is the rendering
// quirk the bash filtered with `grep -vx "$COLLECTION_ID"`. If the file-type
// refusal ran first it would turn an ordinary collection into a hard failure.
func TestACollectionListingItselfWithAFileTypeIsStillJustSkipped(t *testing.T) {
	s := newStub(t)
	s.collection = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"collectiondetails":[
			{"publishedfileid":"3374983339","result":1,"children":[
				{"publishedfileid":"3374983339","sortorder":0,"filetype":2},
				{"publishedfileid":"3171167894","sortorder":1,"filetype":0}]}]}}`))
	}
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if !slices.Equal(c.IDs(), ids("3171167894")) {
		t.Fatalf("children = %v", c.IDs())
	}
	if c.Declared != 1 {
		t.Errorf("declared = %d, want 1: the self-reference is not a child", c.Declared)
	}
}

// TestCollectionRefusesAChildWithNoConsumerAppID: a check that cannot be run is
// not a check that passed.
//
// Steam adds and renames fields, which is why this package does not use
// DisallowUnknownFields -- and the price of that choice is that every
// depended-on field must be cross-checked for its ABSENCE too, or the one
// field the hard stop rests on becomes optional for the remote party.
//
// Absence is recorded per child rather than returned as ErrWrongGame, because
// absence is not proof of another game. It closes the gate without dropping
// anything, which is the rule for every answer this package does not
// understand.
func TestCollectionRefusesAChildWithNoConsumerAppID(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			// No consumer_app_id at all. result 1, a title: everything else
			// about this answer looks healthy.
			out = append(out, `{"publishedfileid":"`+id+`","result":1,"title":"x"}`)
		}
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
			strings.Join(out, ",")+`]}}`))
	}
	c, err := s.api(t).Collection(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if c.Verified {
		t.Error("the collection is Verified although the appid check never ran")
	}
	gate := c.CheckAccounted()
	if !errors.Is(gate, content.ErrUnaccounted) {
		t.Fatalf("CheckAccounted = %v, want ErrUnaccounted", gate)
	}
	if !strings.Contains(gate.Error(), "3171167894") {
		t.Errorf("the refusal does not name a child: %v", gate)
	}
	// The Unaccounted route is chosen precisely so nothing is dropped.
	if len(c.Items) != 4 {
		t.Errorf("items = %d, want 4: an unrunnable check must not shorten the list",
			len(c.Items))
	}
	_, err = Merge([]content.Ref{{ID: "3171167894"}, {ID: "2222222222"}}, c)
	if !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("Merge = %v; a rewrite was offered off a collection nobody could check", err)
	}
}

// controlBytes reports the offending bytes in s, which is what an assertion
// message has to name to be actionable.
func controlBytes(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f {
			out = append(out, fmt.Sprintf("%#x at %d", b, i))
		}
	}
	return out
}

// TestAHostileWorkshopTitleNeverReachesTheOperatorsLine.
//
// A Workshop title and a ban reason are third-party display strings on the same
// threat model as a forged appmanifest's `name`, which
// steamcmd.sanitizeLabel already exists for (ARCHITECTURE 18.1: a compromised
// workshop item owns that instance's install and data trees). This path had no
// sink at all, and it has four surfaces: the `+ <id>  <title>` line a
// collection sync prints, the `note:` it writes into the operator's instance
// YAML, the flagged-item report, and Upstream.Title persisted in the lock and
// reprinted by `gvm content why`.
//
// The payload is the one that matters: a newline plus a forged `- <id>` drop
// line, so a title can manufacture a line of gvm's own output claiming a mod
// was removed, followed by an ANSI erase-line so the terminal hides the rest.
// The HTML path is where it is cheapest to reproduce -- html.UnescapeString is
// what MANUFACTURES the control byte out of `&#10;` -- but the API path carries
// the same bytes directly, since JSON spells \u001b with no escaping question
// at all.
func TestAHostileWorkshopTitleNeverReachesTheOperatorsLine(t *testing.T) {
	const payload = "Nice Mod&#10;  - 3171167894  no longer in the collection&#27;[2K"
	page := `<div class="collectionItem" id="sharedfile_999999">` +
		`<div class="workshopItemTitle">` + payload + `</div></div>`

	c, err := ParseCollectionHTML([]byte(page), fixtureCollection)
	if err != nil {
		t.Fatalf("ParseCollectionHTML: %v", err)
	}
	it, ok := c.Item("999999")
	if !ok {
		t.Fatal("the item did not pair")
	}

	// And the API path's own boundary, with the raw bytes JSON would carry.
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":108600,`+
					`"title":"Nice Mod\n  - 3171167894 gone\u001b[2K","banned":true,`+
					`"ban_reason":"spam\r\n[ OK ] nothing to see\u001b[2K"}`, id))
		}
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
			strings.Join(out, ",")+`]}}`))
	}
	api := s.api(t)
	det, err := api.Details(context.Background(), ids("3171167894"))
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	up := det.Upstream["3171167894"]
	if up == nil {
		t.Fatal("no upstream for the id")
	}

	c2 := &Collection{ID: fixtureCollection, From: CollectionFromAPI, Declared: 1,
		Items: []CollectionItem{{ID: "3171167894"}}}
	if err := api.Enrich(context.Background(), c2); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	merged, err := Merge([]content.Ref{}, c2)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	surfaces := map[string]string{
		"html Item.Title":     it.Title,
		"api Upstream.Title":  up.Title,
		"api Upstream.Reason": up.BanReason,
		"enriched Item.Title": c2.Items[0].Title,
	}
	for i, r := range merged.Refs {
		// This is the one the operator's YAML keeps.
		surfaces[fmt.Sprintf("Merge Refs[%d].Note", i)] = r.Note
	}
	// Per element, not joined: the payload's own separator is a control byte
	// and a joined string would fail this assertion for the test's own reason.
	for name, got := range surfaces {
		if bad := controlBytes(got); len(bad) > 0 {
			t.Errorf("%s carries control bytes %v: %q", name, bad, got)
		}
	}
	// The forged drop line must not survive as plausible text either.
	if strings.Contains(it.Title, "  - 3171167894") {
		t.Errorf("the forged report line survives whitespace folding: %q", it.Title)
	}
}

// TestSanitizingATitleLeavesAnOrdinaryOneAlone: the sanitizer must be invisible
// on every real mod name, including the non-ASCII ones. C1 is dropped by RUNE
// and not by byte for exactly this reason -- 0x80-0x9F are UTF-8 continuation
// bytes, so a byte filter corrupts every accented title on the Workshop.
func TestSanitizingATitleLeavesAnOrdinaryOneAlone(t *testing.T) {
	for _, want := range []string{
		"that DAMN Library",
		"Neat UI Framework & Widgets",
		"KI5's Toyota Hilux 1988",
		"Mini Health Panel [B41/B42.20]",
		"Café Zoméy — Überwald",
		"日本語のMOD",
	} {
		if got := sanitizeTitle(want); got != want {
			t.Errorf("sanitizeTitle(%q) = %q", want, got)
		}
	}
}
