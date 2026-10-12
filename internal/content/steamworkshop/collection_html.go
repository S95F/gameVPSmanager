package steamworkshop

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// The HTML fallback: rebuild.sh's awk pairer, ported, with its cross-check
// intact.
//
// The script is worth quoting, because the comment above the check is the
// whole design:
//
//	# Belt and braces: count ids straight from the raw HTML. If the id/title
//	# pairing above missed any, refuse to rewrite mods.txt off a bad parse.
//
// and the message it died with:
//
//	Parsed $COUNT items but the page contains $RAW_COUNT - Steam markup
//	changed. mods.txt was not touched.
//
// That is the only defence a scraper can have. A pairing walk over markup
// somebody else controls will one day produce a SHORTER list, with no error,
// and a shorter list is indistinguishable from a deliberate one. So the ids
// are counted a SECOND, independent way -- straight out of the raw bytes, with
// a different regex, not touching the pairing state machine at all -- and the
// two counts have to agree or nothing is rewritten.
//
// The two regexes are deliberately NOT the same, and that is not sloppiness.
// The pairer looks for `sharedfile_<digits>` on a line that also carries
// `class="collectionItem"`; the counter looks for the full `id="sharedfile_
// <digits>"` anywhere in the file. Two different readings of the same markup
// are what gives the cross-check its power: a single regex used twice would
// agree with itself about a page it had misread.

var (
	// reCollectionItem matches the line that opens one entry. Each renders as
	//
	//	<div class="collectionItem" id="sharedfile_<ID>">
	reCollectionItem = regexp.MustCompile(`class="collectionItem"`)
	// reSharedFileLoose is the pairer's id regex: `sharedfile_<digits>`, as
	// the awk's `match($0, /sharedfile_[0-9]+/)` had it.
	reSharedFileLoose = regexp.MustCompile(`sharedfile_([0-9]+)`)
	// reSharedFileStrict is the COUNTER's id regex, the awk's
	// `grep -oE 'id="sharedfile_[0-9]+"'`. The quotes and the `id=` are the
	// difference, and the difference is the point.
	reSharedFileStrict = regexp.MustCompile(`id="sharedfile_([0-9]+)"`)
	// reTitleMarker is the line the awk paired on: anything containing
	// `workshopItemTitle`.
	reTitleMarker = regexp.MustCompile(`workshopItemTitle`)
)

// titleOpen is the exact substring the awk stripped up to. Kept as a literal
// because the pairing depends on the `">` as much as on the class name: a page
// that renders the title text on the NEXT line still matches reTitleMarker and
// would otherwise pair an id with a fragment of markup.
const titleOpen = `workshopItemTitle">`

// CollectionHTML scrapes the collection's web page.
//
// The result is UNVERIFIED: a page has no consumer_app_id in it, so it cannot
// tell one game's item from another's. Enrich is what makes it verified, and
// Collection.CheckAccounted refuses until it has run -- so this function
// cannot, on its own, produce a rewrite.
//
// A page carries no `filetype` either, so the API walk's nested-collection
// refusal has no counterpart here: a sub-collection renders as an ordinary
// collectionItem row with a workshopItemTitle, pairs cleanly, and lands in both
// counts. Enrich carries that assertion too (see fileTypeItem) and is the only
// place it can live for this path -- and it is dormant for as long as
// ISteamRemoteStorage declines to report a file type, which is the known
// residual gap on the scraped path.
func (a *API) CollectionHTML(ctx context.Context, id string) (*Collection, error) {
	cid := strings.TrimSpace(id)
	if !numericID(cid) {
		return nil, fmt.Errorf("%w: %q is not a collection id", ErrConfig, id)
	}
	resp, err := a.c.Get(ctx, a.collectionPageURL(cid))
	if err != nil {
		return nil, fmt.Errorf("fetch collection page %s: %w", cid, err)
	}
	return ParseCollectionHTML(resp.Body, cid)
}

// ParseCollectionHTML pairs ids with titles and cross-checks the count.
//
// Exported because it is the part worth testing directly, against a saved
// page, with no server and no network involved: the fixtures beside this file
// are a page that pairs and a page that does not, and the second one exists to
// prove the refusal.
//
// It REFUSES rather than returning a short list:
//
//   - no items paired at all -- the markup moved, or the collection is empty
//     or removed, and only one of those is a reason to delete every mod;
//   - the independent raw count disagrees with the pairing.
//
// Both return ErrMarkup, and nothing is rewritten.
func ParseCollectionHTML(page []byte, collectionID string) (*Collection, error) {
	cid := strings.TrimSpace(collectionID)
	if !numericID(cid) {
		return nil, fmt.Errorf("%w: %q is not a collection id", ErrConfig, collectionID)
	}

	c := &Collection{ID: cid, From: CollectionFromHTML}

	// Pass one: the pairer. A state machine over lines, exactly as the awk
	// had it -- an id is remembered when a collectionItem line goes past, and
	// consumed by the next line carrying a title.
	var (
		pending string
		paired  int
		seen    = map[content.AcquireID]bool{}
	)
	for _, line := range strings.Split(strings.ReplaceAll(string(page), "\r\n", "\n"), "\n") {
		// The awk's two rules, evaluated in order for EVERY line rather than
		// as an if/else. That is not a detail: a minified page puts the
		// collectionItem div and its title div on one line, and an else-branch
		// here would pair nothing at all on a real Steam response while
		// passing a prettified test fixture.
		if reCollectionItem.MatchString(line) {
			if m := reSharedFileLoose.FindStringSubmatch(line); m != nil {
				pending = m[1]
			}
		}
		if pending == "" || !reTitleMarker.MatchString(line) {
			continue
		}

		id, title := content.AcquireID(pending), ""
		pending = ""

		// A collection lists itself among its own entries in some renderings.
		// It is not a mod, and the bash filtered it out of BOTH counts with
		// `grep -vx "$COLLECTION_ID"` -- so it is filtered before either
		// count is touched, and before the unparseable-title branch below
		// could record the collection itself as a missing mod.
		if string(id) == cid {
			continue
		}

		// The awk did `sub(/.*workshopItemTitle">/, "", line)`, which is
		// greedy: with two titles on one line it takes the LAST. Reproduced,
		// because a page that minifies several entries onto one line has to
		// keep behaving the way it did.
		if i := strings.LastIndex(line, titleOpen); i >= 0 {
			title = line[i+len(titleOpen):]
		} else {
			// The marker matched but not in the shape the pairer knows. The
			// bash would have printed the whole line as the title and counted
			// the item as paired; this records it instead, because a mod whose
			// "name" is a fragment of HTML is a page gvm no longer
			// understands. The count check below refuses on it too.
			c.Unaccounted = append(c.Unaccounted, content.Unaccounted{
				ID: id,
				Reason: "the page has a title element for this item but not in the shape the " +
					"pairer knows",
				Hint: "Steam's markup changed; nothing was rewritten. Make the collection " +
					"public so the API path can be used, or use --no-fetch",
			})
			// Recorded in Items as well, with no title. Items is every child
			// the page named, whatever state it is in, so a report can show
			// the operator the id -- and so the arithmetic below has the same
			// meaning on both paths.
			if !seen[id] {
				seen[id] = true
				c.Items = append(c.Items, CollectionItem{ID: id, Order: len(c.Items)})
			}
			continue
		}
		// `sub(/<.*/, "", line)`: everything from the first tag onwards goes.
		if i := strings.IndexByte(title, '<'); i >= 0 {
			title = title[:i]
		}
		// The bash unescaped &amp;, &#39; and &quot; with sed. UnescapeString
		// is a superset of those three; a title is display-only data, so
		// handling &lt; and &#039; too costs nothing and loses nothing.
		//
		// Sanitized AFTER the unescape and never before: UnescapeString is
		// what MANUFACTURES the control byte -- `&#10;` and `&#27;` in markup
		// somebody else controls become a real newline and a real ESC -- so a
		// sanitizer running first would be looking at the wrong bytes.
		title = sanitizeTitle(html.UnescapeString(title))

		// COUNT in the bash was a line count over the paired ids and did NOT
		// de-duplicate, while RAW_COUNT did (`sort -u`). The asymmetry is
		// kept: a page that paired one id twice has a pairing gvm does not
		// understand, and the counts disagreeing is the right answer.
		paired++
		if seen[id] {
			continue
		}
		seen[id] = true
		c.Items = append(c.Items, CollectionItem{ID: id, Title: title, Order: len(c.Items)})
	}

	// Pass two: the independent count, straight out of the raw bytes.
	raw := map[string]bool{}
	for _, m := range reSharedFileStrict.FindAllStringSubmatch(string(page), -1) {
		if m[1] == cid {
			continue
		}
		raw[m[1]] = true
	}

	if paired == 0 {
		return nil, fmt.Errorf("%w: fetched collection %s and paired no items out of it, while "+
			"the page carries %d item id(s). Steam's markup changed, or the collection is "+
			"empty or removed -- nothing was rewritten", ErrMarkup, cid, len(raw))
	}
	if len(raw) != paired {
		// The sentence the bash died with, kept almost verbatim, because it is
		// the sentence that tells an operator this is Steam's fault and not
		// theirs.
		return nil, fmt.Errorf("%w: parsed %d item(s) from collection %s but the page contains "+
			"%d. Steam markup changed. The declared list was not touched -- use --no-fetch to "+
			"sync from it as it is", ErrMarkup, paired, cid, len(raw))
	}

	// Declared is the INDEPENDENT count, not a recount of the walk's own
	// output. That is what makes CheckAccounted's arithmetic a second line of
	// the same defence rather than a tautology -- and it closes a hole the
	// bash had: one id paired TWICE while another never paired at all gives
	// COUNT == RAW_COUNT, so the two counts agree and the list is still a mod
	// short. Here Items has one entry and Declared has two, and the gate says
	// so.
	c.Declared = len(raw)
	return c, nil
}
