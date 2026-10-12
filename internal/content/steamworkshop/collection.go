package steamworkshop

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// The collection fetch: rebuild.sh step 1, upgraded (section 12.5).
//
// The script fetched one HTML page, paired ids with titles in awk, counted the
// ids a second way straight out of the raw markup, and refused to rewrite
// mods.txt if the two counts disagreed. That refusal is the only reason the
// live server's hand-tuned load order survived two years of Steam redesigns,
// and it is reproduced here exactly -- plus the API path the script never had.
//
// Both paths fail closed, for the same reason and in different ways:
//
//   - The API path cross-checks every child. A child must come back result==1,
//     or be explicitly recorded as banned or removed. A child the response
//     never mentioned at all is UNACCOUNTED, and nothing is rewritten.
//     consumer_app_id must be this game's workshop appid for every item: an
//     item from another game is not a parse error, it is the WRONG
//     COLLECTION, and it is a hard stop. A child that is not an ITEM -- a
//     nested collection -- is a hard stop for the same reason.
//   - The HTML path keeps the raw-count cross-check (collection_html.go) and
//     is UNVERIFIED until Enrich has cross-checked its children, because a
//     page gvm scraped has no consumer_app_id in it.
//
// And one rule shared with the tag table: gvm never REMOVES an item on the
// strength of an API answer. A child Steam reports as banned or removed stays
// in the collection's item list, flagged, so a report can say so and an
// operator can decide. Dropping it would be gvm deleting a mod because a web
// service had an opinion.

// CollectionSource names which path produced a collection.
type CollectionSource string

const (
	// CollectionFromAPI is GetCollectionDetails.
	CollectionFromAPI CollectionSource = "api"
	// CollectionFromHTML is the scraped page: the fallback, used on
	// result==9 or with --allow-html-fallback.
	CollectionFromHTML CollectionSource = "html"
)

// CollectionItem is one child of a collection.
type CollectionItem struct {
	ID    content.AcquireID
	Title string

	// Order is the collection's own sortorder. Recorded for display and NEVER
	// used for load order: a collection cannot know that that DAMN Library has
	// to load before the vehicle mods that need it, and that knowledge lives
	// in the operator's ordering and in the comments beside it.
	Order int

	// Upstream is the child's metadata, or nil when Steam refused it.
	Upstream *content.Upstream

	// Banned and Removed are the two "recorded as such" states of section
	// 12.5. Both keep the item in the list.
	Banned  bool
	Removed bool
}

// Collection is a collection as read, with its accounting.
type Collection struct {
	ID   string
	From CollectionSource

	// Declared is how many children the collection said it had, counted
	// before any metadata lookup and by a SECOND pass over the response rather
	// than by incrementing inside the walk that builds Items -- a count
	// derived from the same loop as the list could never disagree with it,
	// which would make the check decorative.
	//
	// The two paths give it different strength, and the difference is worth
	// knowing. On the HTML path it is an independent READING: a different regex
	// over the raw bytes, so a misread page shows up as a disagreement
	// (collection_html.go). On the API path one JSON decode is the only
	// reading there is, so the second pass can only catch a walk that lost
	// track of its own input -- not a response gvm misunderstood. That is why
	// the API path carries per-child refusals as well.
	Declared int

	Items []CollectionItem

	// Verified records that every child has been cross-checked: metadata
	// looked up, consumer_app_id matched, nothing left unexplained. The API
	// path sets it; the HTML path does not until Enrich runs. CheckAccounted
	// refuses an unverified collection, so Merge cannot produce a rewrite from
	// a page nobody checked.
	Verified bool

	// Unaccounted is the fail-closed gate, in the same shape the plan uses so
	// the refusal an operator reads is the same sentence either way.
	Unaccounted []content.Unaccounted
}

// IDs are the children, in collection order.
func (c *Collection) IDs() []content.AcquireID {
	if c == nil {
		return nil
	}
	out := make([]content.AcquireID, 0, len(c.Items))
	for _, it := range c.Items {
		out = append(out, it.ID)
	}
	return out
}

// Item returns one child.
func (c *Collection) Item(id content.AcquireID) (CollectionItem, bool) {
	if c == nil {
		return CollectionItem{}, false
	}
	for _, it := range c.Items {
		if it.ID == id {
			return it, true
		}
	}
	return CollectionItem{}, false
}

// Flagged lists the children recorded as banned or removed upstream. Kept in
// the list, reported, never dropped by gvm.
func (c *Collection) Flagged() []content.AcquireID {
	if c == nil {
		return nil
	}
	var out []content.AcquireID
	for _, it := range c.Items {
		if it.Banned || it.Removed {
			out = append(out, it.ID)
		}
	}
	return out
}

// CheckAccounted is the gate, and Merge's first statement.
//
// Three conditions, and the third is the one the bash suite taught:
//
//  1. The children were cross-checked at all (Verified).
//  2. Nothing is recorded as unaccounted.
//  3. The arithmetic adds up: items plus unaccounted equals the number of
//     children the collection declared. This is RAW_COUNT != COUNT, and it
//     catches the case where the walk lost track of its own input and
//     recorded no reason -- which is exactly what a Steam markup change looks
//     like from the inside.
func (c *Collection) CheckAccounted() error {
	if c == nil {
		return fmt.Errorf("%w: there is no collection", content.ErrUnaccounted)
	}
	// The specific refusal before the general one. Enrich clears Verified
	// whenever it records an unaccounted child, so a "not cross-checked"
	// message checked first would shadow the one that names the child -- and
	// a refusal nobody can act on is a refusal operators route around.
	if n := len(c.Unaccounted); n > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "collection %s left %d of %d child item(s) unexplained, so nothing was "+
			"rewritten", c.ID, n, c.Declared)
		for _, u := range c.Unaccounted {
			b.WriteString("\n  - ")
			b.WriteString(u.String())
		}
		b.WriteString("\n  a partial list looks exactly like a deliberate one, so the list was " +
			"left as it is")
		return fmt.Errorf("%w: %s", content.ErrUnaccounted, b.String())
	}
	if got := len(c.Items); got != c.Declared {
		return fmt.Errorf("%w: collection %s declares %d children and %d were accounted for, "+
			"with no reason recorded for the difference; the list was left as it is",
			content.ErrUnaccounted, c.ID, c.Declared, got)
	}
	// The generic refusal last, for the same reason: it is the one that says
	// least about what to do next.
	if !c.Verified {
		return fmt.Errorf("%w: collection %s was read from the %s path and its children have "+
			"not been cross-checked, so an item from another game would be indistinguishable "+
			"from one of this game's", content.ErrUnaccounted, c.ID, c.From)
	}
	return nil
}

// Summary is the line a report prints.
func (c *Collection) Summary() string {
	if c == nil {
		return "no collection"
	}
	parts := []string{fmt.Sprintf("%d item(s) from the %s path", len(c.Items), c.From)}
	if n := len(c.Flagged()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d banned or removed upstream", n))
	}
	if n := len(c.Unaccounted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d UNACCOUNTED (refusing)", n))
	}
	if !c.Verified {
		parts = append(parts, "NOT cross-checked")
	}
	return strings.Join(parts, ", ")
}

// collectionEnvelope is GetCollectionDetails' response.
type collectionEnvelope struct {
	Response struct {
		Result      int `json:"result"`
		ResultCount int `json:"resultcount"`
		Details     []struct {
			PublishedFileID string            `json:"publishedfileid"`
			Result          int               `json:"result"`
			Children        []collectionChild `json:"children"`
		} `json:"collectiondetails"`
	} `json:"response"`
}

// collectionChild is one entry of collectiondetails[0].children[].
//
// Named rather than anonymous so the independent recount below can take the
// same slice the pairing walk did without restating its shape -- two readings
// of one input, which is the only thing that makes the arithmetic check
// anything other than a tautology.
type collectionChild struct {
	PublishedFileID string `json:"publishedfileid"`
	SortOrder       int    `json:"sortorder"`
	// FileType is EWorkshopFileType: 0 for an ordinary item, 2 for a nested
	// collection. See fileTypeItem.
	FileType int `json:"filetype"`
}

// countChildren is the SECOND, independent count of a collection's children.
//
// It is deliberately not the length of the walk's output. On the HTML path
// Declared comes from a different regex over the raw bytes, and the whole power
// of CheckAccounted's arithmetic is that the two readings can disagree. One
// JSON decode is the only reading available here, so this is weaker than that
// -- it cannot catch a misread response, only a walk that lost track of its own
// input. It still catches the case the old `c.Declared++` inside the append
// loop could not: a `continue` added to that loop by a later change, which
// would silently shorten both Items and the count together and leave the gate
// agreeing with itself.
func countChildren(children []collectionChild, collectionID string) int {
	seen := make(map[string]bool, len(children))
	n := 0
	for _, child := range children {
		id := strings.TrimSpace(child.PublishedFileID)
		// The self-reference is subtracted explicitly, for the same reason the
		// walk skips it: a collection listing itself is a rendering quirk and
		// not a child. The bash filtered it out of BOTH counts.
		if id == "" || id == collectionID || seen[id] {
			continue
		}
		seen[id] = true
		n++
	}
	return n
}

// Collection reads a collection through the API and cross-checks every child.
//
// result==9 on the collection itself is ErrCollectionUnavailable, not an empty
// list. That distinction is the whole reason this returns an error there: a
// private or mistyped collection that fell through to "zero children" would
// drop every mod on the next sync, and an empty list is a perfectly valid
// thing for a collection to be.
func (a *API) Collection(ctx context.Context, id string) (*Collection, error) {
	cid := strings.TrimSpace(id)
	if !numericID(cid) {
		return nil, fmt.Errorf("%w: %q is not a collection id", ErrConfig, id)
	}

	form := url.Values{}
	form.Set("collectioncount", "1")
	form.Set("publishedfileids[0]", cid)

	var env collectionEnvelope
	if err := a.c.PostFormJSON(ctx, a.api+pathCollectionDetails, form, &env); err != nil {
		return nil, fmt.Errorf("GetCollectionDetails %s: %w", cid, err)
	}
	if env.Response.Result != resultOK {
		return nil, fmt.Errorf("%w: GetCollectionDetails returned result %d for collection %s",
			ErrAPI, env.Response.Result, cid)
	}
	if len(env.Response.Details) != 1 {
		return nil, fmt.Errorf("%w: GetCollectionDetails returned %d collections for one "+
			"request", ErrAPI, len(env.Response.Details))
	}
	d := env.Response.Details[0]
	if got := strings.TrimSpace(d.PublishedFileID); got != "" && got != cid {
		return nil, fmt.Errorf("%w: asked for collection %s and the answer describes %s",
			ErrAPI, cid, got)
	}
	switch d.Result {
	case resultOK:
	case resultNotFound:
		return nil, fmt.Errorf("%w: %s (result 9). Check the id, or make the collection "+
			"public; `--allow-html-fallback` scrapes the page instead, and refuses just as "+
			"loudly if the markup has moved", ErrCollectionUnavailable, cid)
	default:
		return nil, fmt.Errorf("%w: collection %s came back with result %d", ErrAPI, cid,
			d.Result)
	}

	c := &Collection{ID: cid, From: CollectionFromAPI}
	seen := map[content.AcquireID]bool{}
	for _, child := range d.Children {
		childID := content.AcquireID(strings.TrimSpace(child.PublishedFileID))
		switch {
		case childID == "":
			return nil, fmt.Errorf("%w: collection %s lists a child with no publishedfileid",
				ErrAPI, cid)
		case !numericID(string(childID)):
			return nil, fmt.Errorf("%w: collection %s lists child %q, which is not a "+
				"publishedfileid", ErrAPI, cid, childID)
		case childID == content.AcquireID(cid):
			// The page and the API both list the collection among its own
			// children in some renderings. It is not a mod.
			//
			// This case is BEFORE the file-type refusal below on purpose: a
			// collection that lists itself carries filetype 2 on that
			// self-reference, and turning the rendering quirk the bash filtered
			// with `grep -vx "$COLLECTION_ID"` into a hard failure would refuse
			// ordinary collections.
			continue
		case child.FileType != fileTypeItem:
			// A NESTED COLLECTION, refused rather than counted as one child.
			//
			// Nothing downstream can catch this. A collection is itself a
			// published file, so GetPublishedFileDetails answers for it with
			// result 1, a title and this game's consumer_app_id -- which is
			// what makes Enrich's cross-check wave it through -- and Declared
			// agrees because it counts the same children this walk accepted.
			// Merge would then send every mod that lives inside the
			// sub-collection to Dropped and append an id the server cannot
			// download, with the gate quiet throughout. A hard stop, like the
			// other "this child is not an item" cases above, because an answer
			// gvm cannot use is not a per-item note.
			return nil, fmt.Errorf("%w: collection %s lists child %s with filetype %d, which is "+
				"a nested COLLECTION and not a workshop item. A collection cannot be a member of "+
				"a mod list, and counting it as one child would rewrite the declared list down "+
				"to that single id; nothing was rewritten -- sync from the sub-collection "+
				"directly, or flatten it", ErrAPI, cid, childID, child.FileType)
		case seen[childID]:
			continue
		}
		seen[childID] = true
		c.Items = append(c.Items, CollectionItem{ID: childID, Order: child.SortOrder})
	}
	c.Declared = countChildren(d.Children, cid)
	if c.Declared == 0 {
		// Refused rather than returned: an empty answer is what a markup
		// change, a rate limit and a genuinely empty collection all look like,
		// and only one of those is a reason to delete every mod.
		return nil, fmt.Errorf("%w: collection %s lists no items. Nothing was rewritten -- an "+
			"empty answer is also what a changed response shape looks like", ErrAPI, cid)
	}

	if err := a.Enrich(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Enrich looks up every child's metadata and runs section 12.5's cross-checks.
//
// It is a separate, exported step because the HTML fallback needs exactly the
// same checks over a list it got from a web page, and running them in two
// places is how one of the two would lose the consumer_app_id check.
//
// On success the collection is Verified. On any check that cannot be answered
// the child goes in Unaccounted, and CheckAccounted then refuses the rewrite --
// an ABSENT consumer_app_id is that case, because absence is not proof of
// another game and gvm does not drop an item on a check it could not run.
// Two answers are hard refusals instead, because no per-item record makes them
// safe: a consumer_app_id that is present and WRONG (the collection is the
// wrong collection, ErrWrongGame), and a child the Workshop describes as
// something other than an item (a nested collection, see fileTypeItem).
func (a *API) Enrich(ctx context.Context, c *Collection) error {
	if c == nil {
		return fmt.Errorf("%w: there is no collection to cross-check", ErrInput)
	}
	if len(c.Items) == 0 {
		return fmt.Errorf("%w: collection %s has no children to cross-check", ErrInput, c.ID)
	}

	det, err := a.Details(ctx, c.IDs())
	if err != nil {
		// A metadata failure is terminal for a REWRITE even though it is a
		// degradation for a resolve. The asymmetry is deliberate: a resolve
		// must not change which mods a server enables when Steam is down, and
		// a rewrite must not happen at all when Steam is half-down.
		return fmt.Errorf("cross-checking collection %s: %w", c.ID, err)
	}

	for i := range c.Items {
		it := &c.Items[i]
		if u, ok := det.Upstream[it.ID]; ok {
			if ft := det.FileType(it.ID); ft != fileTypeItem {
				// The nested-collection guard, on the metadata rather than on
				// the children array -- because this is the only cross-check
				// the SCRAPED path has. A page carries no filetype, so a
				// sub-collection renders as an ordinary collectionItem row,
				// pairs cleanly and lands in both counts. See fileTypeItem for
				// why no other check can see it.
				return fmt.Errorf("%w: %s is a Workshop file of type %d, not an item. A "+
					"collection cannot be a member of a mod list, and rewriting the declared "+
					"list around it would drop every mod inside it; nothing was rewritten",
					ErrAPI, it.ID, ft)
			}
			if u.ConsumerAppID == 0 {
				// Absence is not proof of another game, so this is not
				// ErrWrongGame -- but it is not a pass either. The cross-check
				// could not be RUN, and an unrunnable check belongs in the
				// Unaccounted bucket: the item stays in the list (gvm deletes
				// nothing on an API answer) and the gate closes on the rewrite.
				//
				// Section 12.5 requires the comparison for EVERY item, and
				// folding it into a `!= 0 &&` conjunct made the one hard stop
				// this subsystem's refusal narrative rests on optional for the
				// remote party. This package deliberately does not use
				// DisallowUnknownFields because Steam adds and renames fields;
				// the price of that choice is that every depended-on field has
				// to be cross-checked for its ABSENCE too.
				c.Unaccounted = append(c.Unaccounted, content.Unaccounted{
					ID: it.ID,
					Reason: "the Workshop answered for it without a consumer_app_id, so gvm " +
						"cannot tell whether it belongs to this game",
					Hint: "retry, or use --no-fetch to sync from the declared list as it is",
				})
				continue
			}
			if u.ConsumerAppID != a.appID {
				return fmt.Errorf("%w: %s belongs to app %d and this game's workshop appid is "+
					"%d. That is a mistyped collection id, not a parse error, so nothing was "+
					"rewritten", ErrWrongGame, it.ID, u.ConsumerAppID, a.appID)
			}
			u.UpstreamOrder = it.Order
			it.Upstream = u
			it.Banned = u.Banned
			if it.Title == "" {
				it.Title = u.Title
			}
			continue
		}
		if code, ok := det.Refused[it.ID]; ok {
			// Explicitly recorded as removed: accounted for, kept in the list,
			// and reported. gvm does not delete a mod because the Workshop
			// stopped serving its page.
			it.Removed = true
			if code != resultNotFound {
				it.Removed = false
				c.Unaccounted = append(c.Unaccounted, content.Unaccounted{
					ID: it.ID,
					Reason: fmt.Sprintf("the Workshop returned result %d for it, which is "+
						"neither a success nor a removal", code),
					Hint: "retry once Steam is healthy, or use --no-fetch to sync from the " +
						"declared list as it is",
				})
			}
			continue
		}
		// Neither confirmed nor denied. This is the bucket the whole gate
		// exists for: the request went out, the answer came back, and this
		// item was not in it.
		c.Unaccounted = append(c.Unaccounted, content.Unaccounted{
			ID:     it.ID,
			Reason: "the metadata lookup returned neither details nor a result code for it",
			Hint: "Steam answered a batch without this item in it; retry, or use --no-fetch " +
				"to sync from the declared list as it is",
		})
	}

	c.Verified = len(c.Unaccounted) == 0
	return nil
}

// MergeResult is what a collection sync would change, as data.
//
// It carries the NEW ref list rather than mutating the one it was given: a
// merge that edited in place could not be shown in a --dry-run without having
// already happened.
type MergeResult struct {
	Refs    []content.Ref
	Added   []CollectionItem
	Dropped []content.AcquireID
	// Kept are the vendored refs a collection has no say over. Reported so a
	// --dry-run explains why an id not in the collection survived.
	Kept []content.AcquireID
}

// Changed reports whether the merge would alter the declared list.
func (r *MergeResult) Changed() bool {
	return r != nil && (len(r.Added) > 0 || len(r.Dropped) > 0)
}

// Merge reconciles a declared acquisition list against a collection.
//
// The semantics are internal/pz.ModsFile.SyncCollection's, moved onto the
// declared sequence (section 12.5):
//
//   - KNOWN ITEMS KEEP THEIR POSITION, and their note, pin, requires and
//     vendored flag with it. Hand-tuned load order is not knowable from a
//     collection, and the reasoning behind it lives in comments gvm cannot
//     read.
//   - Delisted items are removed.
//   - New items are APPENDED, never inserted. A new item's correct load
//     position is unknowable: if it turns out to be a library it has to be
//     moved up by hand, and `gvm content move` is how.
//   - A VENDORED ref is never dropped. Those are operator bytes -- never
//     fetched, never pruned -- and a collection has no opinion about them.
//
// The dated head comment the bash wrote above the new block is the writer's
// job: this function produces refs, and a YAML comment needs the YAML node.
// MergeResult.Added is what that comment is rendered from.
func Merge(refs []content.Ref, c *Collection) (*MergeResult, error) {
	// The gate first. A merge computed from a collection that did not add up
	// is the rewrite rebuild.sh refused to do.
	if err := c.CheckAccounted(); err != nil {
		return nil, err
	}

	inCollection := make(map[content.AcquireID]bool, len(c.Items))
	for _, it := range c.Items {
		inCollection[it.ID] = true
	}

	out := &MergeResult{Refs: make([]content.Ref, 0, len(refs)+len(c.Items))}
	known := make(map[content.AcquireID]bool, len(refs))
	for _, r := range refs {
		id := content.RefID(r)
		switch {
		case id == "":
			// A ref with no id is not something a collection sync can place.
			// Kept verbatim so the operator still sees their own line.
			out.Refs = append(out.Refs, r)
		case known[id]:
			// A duplicate in the declared list is the operator's to resolve;
			// dropping one half here would look like the collection did it.
			out.Refs = append(out.Refs, r)
		case r.Vendored:
			known[id] = true
			out.Kept = append(out.Kept, id)
			out.Refs = append(out.Refs, r)
		case inCollection[id]:
			known[id] = true
			out.Refs = append(out.Refs, r)
		default:
			out.Dropped = append(out.Dropped, id)
		}
	}

	for _, it := range c.Items {
		if known[it.ID] {
			continue
		}
		known[it.ID] = true
		out.Added = append(out.Added, it)
		// Sanitized again, redundantly, which is the point: this is the one
		// line in the package that writes third-party text into a file the
		// operator owns and gvm re-reads. Both producers of Item.Title already
		// scrub it, and this is the assertion that nothing reaches the
		// operator's YAML unscrubbed whatever a third producer does later.
		out.Refs = append(out.Refs, content.Ref{
			ID:   string(it.ID),
			Note: sanitizeTitle(it.Title),
		})
	}
	return out, nil
}

// collectionPageURL is the page the HTML fallback scrapes.
func (a *API) collectionPageURL(id string) string {
	return a.community + pathCollectionPage + "?id=" + url.QueryEscape(id)
}
