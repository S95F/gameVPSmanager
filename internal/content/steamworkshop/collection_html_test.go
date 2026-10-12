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

func TestParseCollectionHTMLPairsIDsAndTitles(t *testing.T) {
	c, err := ParseCollectionHTML(fixture(t, "collection.html"), fixtureCollection)
	if err != nil {
		t.Fatalf("ParseCollectionHTML: %v", err)
	}

	if c.From != CollectionFromHTML {
		t.Errorf("from = %q", c.From)
	}
	if !slices.Equal(c.IDs(), ids("3171167894", "3508537032", "3435796523", "2866258937")) {
		t.Fatalf("ids = %v", c.IDs())
	}
	want := map[content.AcquireID]string{
		"3171167894": "that DAMN Library",
		// &amp; unescaped. The bash did three entities with sed;
		// html.UnescapeString is a superset, and a title is display-only data.
		"3508537032": "Neat UI Framework & Widgets",
		// A MINIFIED entry: the collectionItem div and its title div on one
		// line, with &#39; in it. The awk evaluated its two rules in order for
		// every line, so this pairs -- an if/else would silently pair nothing
		// at all on a real Steam response.
		"3435796523": "KI5's Toyota Hilux 1988",
		"2866258937": "Mini Health Panel [B41/B42.20]",
	}
	for id, title := range want {
		it, ok := c.Item(id)
		if !ok {
			t.Errorf("%s was not paired", id)
			continue
		}
		if it.Title != title {
			t.Errorf("%s title = %q, want %q", id, it.Title, title)
		}
	}
	// Page order is recorded as the collection's order, and it is DISPLAY
	// data: nothing downstream may use it for load order.
	for i, it := range c.Items {
		if it.Order != i {
			t.Errorf("%s order = %d, want %d", it.ID, it.Order, i)
		}
	}
}

// TestParseCollectionHTMLRefusesABrokenPairing is why the fixture beside it
// exists.
//
// Steam renamed one title element. The pairer's state machine then carries
// that entry's id forward until the next collectionItem line overwrites it, so
// the item does not get a bad title -- it DISAPPEARS, and what comes out is a
// perfectly ordinary-looking mod list that is one mod short. The second,
// independent id count out of the raw bytes is the only thing that can see it.
func TestParseCollectionHTMLRefusesABrokenPairing(t *testing.T) {
	c, err := ParseCollectionHTML(fixture(t, "collection-broken-pairing.html"),
		fixtureCollection)
	if err == nil {
		t.Fatalf("the broken page parsed into %d item(s); a short list and a deliberate list "+
			"are the same bytes", len(c.Items))
	}
	if !errors.Is(err, ErrMarkup) {
		t.Fatalf("error = %v, want ErrMarkup", err)
	}
	if c != nil {
		t.Error("a collection was returned alongside the refusal, which a caller could use")
	}
	// Both counts, and the sentence that tells an operator this is Steam's
	// fault and not theirs.
	for _, want := range []string{"parsed 2", "contains 3", "Steam markup changed",
		"was not touched"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

func TestParseCollectionHTMLRefusesAPageWithNoItems(t *testing.T) {
	for name, page := range map[string]string{
		"empty":           "",
		"a login wall":    `<html><body><div id="global_header">Sign in</div></body></html>`,
		"a rate limiter":  `<html><body>Too many requests</body></html>`,
		"ids but no divs": `<html><body><a id="sharedfile_3171167894">x</a></body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCollectionHTML([]byte(page), fixtureCollection)
			if !errors.Is(err, ErrMarkup) {
				t.Fatalf("error = %v, want ErrMarkup; an empty answer is also what a changed "+
					"response shape looks like", err)
			}
		})
	}
}

// TestParseCollectionHTMLRefusesADuplicatePairing keeps the bash's asymmetry
// between its two counts: COUNT was a line count and did not de-duplicate,
// RAW_COUNT ran through `sort -u` and did. A page that paired one id twice is
// a page gvm does not understand, and the counts disagreeing is the right
// answer rather than a tidy-up.
func TestParseCollectionHTMLRefusesADuplicatePairing(t *testing.T) {
	page := `<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library</div></div>
		<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library again</div></div>`

	_, err := ParseCollectionHTML([]byte(page), fixtureCollection)
	if !errors.Is(err, ErrMarkup) {
		t.Fatalf("error = %v, want ErrMarkup", err)
	}
	if !strings.Contains(err.Error(), "parsed 2") || !strings.Contains(err.Error(), "contains 1") {
		t.Errorf("the refusal does not show both counts: %v", err)
	}
}

// TestTheTwoCountsReadTheMarkupDifferently is the property that gives the
// cross-check its power. One regex used twice would agree with itself about a
// page it had misread.
func TestTheTwoCountsReadTheMarkupDifferently(t *testing.T) {
	// The pairer's regex matches `sharedfile_<digits>` anywhere on a
	// collectionItem line; the counter's needs the full `id="sharedfile_N"`.
	// Here the id arrives through a data attribute, so the pairer sees it and
	// the counter does not -- and the disagreement refuses.
	page := `<div class="collectionItem" data-ref="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library</div></div>`

	if _, err := ParseCollectionHTML([]byte(page), fixtureCollection); !errors.Is(err,
		ErrMarkup) {
		t.Fatalf("error = %v, want ErrMarkup: the two readings disagreed and that is the "+
			"whole mechanism", err)
	}
}

// TestParseCollectionHTMLExcludesTheCollectionsOwnID: the page lists the
// collection among its own entries in some renderings, and it is not a mod.
func TestParseCollectionHTMLExcludesTheCollectionsOwnID(t *testing.T) {
	page := `<div class="collectionItem" id="sharedfile_3374983339">
		<div class="workshopItemTitle">Hardcore Apocalypse</div></div>
		<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library</div></div>`

	c, err := ParseCollectionHTML([]byte(page), fixtureCollection)
	if err != nil {
		t.Fatalf("ParseCollectionHTML: %v", err)
	}
	if !slices.Equal(c.IDs(), ids("3171167894")) {
		t.Fatalf("ids = %v", c.IDs())
	}
}

// TestParseCollectionHTMLRecordsATitleShapeItDoesNotKnow: the bash would have
// printed the whole line as the title and counted the item as paired. A mod
// whose "name" is a fragment of HTML is a page gvm no longer understands, so
// it is recorded instead -- and the count check refuses on it too.
func TestParseCollectionHTMLRecordsATitleShapeItDoesNotKnow(t *testing.T) {
	page := `<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle"
		   >that DAMN Library</div></div>
		<div class="collectionItem" id="sharedfile_3508537032">
		<div class="workshopItemTitle">Neat UI Framework</div></div>
		<div class="collectionItem" id="sharedfile_3435796523">
		<div class="workshopItemTitle">Hilux</div></div>`

	_, err := ParseCollectionHTML([]byte(page), fixtureCollection)
	if !errors.Is(err, ErrMarkup) {
		t.Fatalf("error = %v, want ErrMarkup", err)
	}
}

func TestParseCollectionHTMLRefusesANonNumericCollectionID(t *testing.T) {
	if _, err := ParseCollectionHTML(fixture(t, "collection.html"), "hardcore"); !errors.Is(
		err, ErrConfig) {
		t.Fatalf("error = %v, want ErrConfig", err)
	}
}

// TestTheHTMLPathIsUnverifiedUntilEnriched: a page has no consumer_app_id in
// it, so on its own it cannot tell one game's item from another's. The type is
// what enforces that -- CheckAccounted refuses, so Merge cannot produce a
// rewrite out of a page nobody checked.
func TestTheHTMLPathIsUnverifiedUntilEnriched(t *testing.T) {
	s := newStub(t)
	a := s.api(t)

	c, err := a.CollectionHTML(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("CollectionHTML: %v", err)
	}
	if s.pageCalls != 1 {
		t.Errorf("the page was fetched %d time(s)", s.pageCalls)
	}
	if c.Verified {
		t.Fatal("a scraped page was trusted without a cross-check")
	}
	if err := c.CheckAccounted(); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("CheckAccounted = %v, want content.ErrUnaccounted", err)
	}
	if _, err := Merge(nil, c); !errors.Is(err, content.ErrUnaccounted) {
		t.Fatalf("Merge off an unverified page = %v", err)
	}
	if !strings.Contains(c.Summary(), "NOT cross-checked") {
		t.Errorf("summary = %q", c.Summary())
	}

	// Enrich is the same set of checks the API path runs, over the list the
	// page produced -- including the consumer_app_id check the page cannot
	// supply by itself.
	if err := a.Enrich(context.Background(), c); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !c.Verified {
		t.Fatalf("still unverified after Enrich: %s", c.Summary())
	}
	if err := c.CheckAccounted(); err != nil {
		t.Fatalf("CheckAccounted after Enrich: %v", err)
	}
	// The titles from the page survive; the metadata fills everything else in.
	it, _ := c.Item("3508537032")
	if it.Title != "Neat UI Framework & Widgets" {
		t.Errorf("Enrich overwrote the page's title: %q", it.Title)
	}
	if it.Upstream == nil || !it.Banned {
		t.Errorf("Enrich did not attach the metadata: %+v", it)
	}
}

func TestEnrichOnTheHTMLPathStillRefusesAnotherGamesItem(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			out = append(out, `{"publishedfileid":"`+id+
				`","result":1,"consumer_app_id":294420,"title":"x"}`)
		}
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
			strings.Join(out, ",")+`]}}`))
	}
	a := s.api(t)
	c, err := a.CollectionHTML(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("CollectionHTML: %v", err)
	}
	if err := a.Enrich(context.Background(), c); !errors.Is(err, ErrWrongGame) {
		t.Fatalf("error = %v, want ErrWrongGame", err)
	}
}

func TestCollectionHTMLRefusesANonNumericID(t *testing.T) {
	s := newStub(t)
	if _, err := s.api(t).CollectionHTML(context.Background(), "nope"); !errors.Is(err,
		ErrConfig) {
		t.Fatalf("error = %v, want ErrConfig", err)
	}
	if s.pageCalls != 0 {
		t.Error("a request went out for something that cannot be a collection id")
	}
}

// TestADoublePairingMaskingAMissedOneIsStillRefused closes a hole the bash
// had.
//
// One id paired TWICE while another never paired at all gives COUNT ==
// RAW_COUNT: the two counts agree, rebuild.sh would have rewritten mods.txt,
// and the list would be one mod short. Here the raw count is the collection's
// Declared and the walk's output is Items, so the arithmetic in CheckAccounted
// sees what the two counts could not.
func TestADoublePairingMaskingAMissedOneIsStillRefused(t *testing.T) {
	// 3171167894 appears twice with a title and once more as a bare id with
	// none, so: paired = 2, raw = {3171167894, 3508537032} = 2.
	page := `<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library</div></div>
		<div class="collectionItem" id="sharedfile_3171167894">
		<div class="workshopItemTitle">that DAMN Library again</div></div>
		<div class="collectionItem" id="sharedfile_3508537032">
		<div class="workshopItemName_v2">Neat UI Framework</div></div>`

	c, err := ParseCollectionHTML([]byte(page), fixtureCollection)
	if err != nil {
		t.Fatalf("the two counts agree here, so the parse itself does not refuse: %v", err)
	}
	if c.Declared != 2 || len(c.Items) != 1 {
		t.Fatalf("declared %d, items %d; Declared is the independent count and Items is the "+
			"walk's output, and conflating the two makes the check a tautology", c.Declared,
			len(c.Items))
	}
	gate := c.CheckAccounted()
	if !errors.Is(gate, content.ErrUnaccounted) {
		t.Fatalf("CheckAccounted = %v; a list that is one mod short was accepted", gate)
	}
	if !strings.Contains(gate.Error(), "1 were accounted for") {
		t.Errorf("the refusal does not show the arithmetic: %v", gate)
	}
}

// TestEnrichOnTheHTMLPathRefusesAWorkshopFileThatIsNotAnItem is the other half
// of the nested-collection guard.
//
// The scraped page carries no filetype: a sub-collection renders as an ordinary
// collectionItem row with a workshopItemTitle, so it pairs, it is in the raw
// count, and both counts agree. Enrich is the only cross-check this path has,
// so the file-type assertion has to live there as well as in the API walk --
// otherwise the API-path fix only looks complete.
func TestEnrichOnTheHTMLPathRefusesAWorkshopFileThatIsNotAnItem(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, id := range askedFor(t, r) {
			ft := 0
			if id == "3508537032" {
				ft = 2
			}
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":108600,"title":"x",`+
					`"file_type":%d}`, id, ft))
		}
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
			strings.Join(out, ",")+`]}}`))
	}
	a := s.api(t)
	c, err := a.CollectionHTML(context.Background(), fixtureCollection)
	if err != nil {
		t.Fatalf("CollectionHTML: %v", err)
	}
	err = a.Enrich(context.Background(), c)
	if err == nil {
		t.Fatalf("a Workshop file that is not an item was accepted: %s", c.Summary())
	}
	if !strings.Contains(err.Error(), "3508537032") {
		t.Errorf("the refusal does not name the child: %v", err)
	}
	if c.Verified {
		t.Error("the collection is Verified although a child is not an item")
	}
}
