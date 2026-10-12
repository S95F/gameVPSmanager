// Package steamworkshop is the Steam Workshop content source (section 12.5).
//
// It holds three things and deliberately not a fourth.
//
// The KEYLESS metadata client. ISteamRemoteStorage/GetPublishedFileDetails and
// GetCollectionDetails need no API key, which is what makes a workshop sync
// something gvm can do on a fresh VPS with no operator credential at all.
// Everything they return is EVIDENCE: title, time_updated, file_size,
// consumer_app_id, banned, and the tags[] array that says "Build 41" or
// "Build 42".
//
// The COLLECTION FETCH, in two paths that both fail closed. The API path
// cross-checks every child and refuses on a result it cannot account for; the
// HTML path keeps rebuild.sh's raw-count cross-check and refuses to rewrite
// anything when the page's markup no longer pairs. The legacy script said it
// best: "Steam markup changed. mods.txt was not touched."
//
// The SOURCE itself (source.go, observe.go): Resolve, Materialize, Activate,
// Observe over a server_boot acquisition.
//
// What is NOT here is any scoring. Which variant inside a dual-build workshop
// item this server should enable is internal/pz's question, answered from the
// downloaded files, and the tags are layered on top of that answer by
// internal/content's resolver. A second copy of those rules living next to the
// API client is how "Steam says Build 41" would quietly become authority over
// what the operator's own disk says.
package steamworkshop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/fetch"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// APIName is the value recorded in a lock entry's upstream.api, and the
// manifest's `metadata_api:` spelling. One constant, so a lockfile written by
// this package names something an operator can find in games/*.yaml.
const APIName = "steam_remote_storage"

// The two keyless endpoints and the community page, as section 12.5 and
// docs/research/steamcmd.md name them.
const (
	// DefaultAPIBase is the Web API host. On the allowlist (section 12.4).
	DefaultAPIBase = "https://api.steampowered.com"
	// DefaultCommunityBase serves the collection HTML the fallback parses.
	DefaultCommunityBase = "https://steamcommunity.com"

	pathPublishedFileDetails = "/ISteamRemoteStorage/GetPublishedFileDetails/v1/"
	pathCollectionDetails    = "/ISteamRemoteStorage/GetCollectionDetails/v1/"
	pathCollectionPage       = "/sharedfiles/filedetails/"
)

// Steam EResult values this package distinguishes.
//
// Only these two are named. The rest are reported by number, because a table
// of 110 EResult names mostly made of codes nobody has observed from these
// endpoints would be a table that goes stale and gets trusted.
const (
	// resultOK is EResultOK.
	resultOK = 1
	// resultNotFound is EResultFileNotFound: the item or collection is
	// private, removed, or never existed. On a COLLECTION it is the trigger
	// for the HTML fallback; on a CHILD it is recorded as removed.
	resultNotFound = 9
)

// fileTypeItem is EWorkshopFileType 0, a plain workshop item -- the only kind
// of published file that can be a member of a mod list.
//
// Named rather than inlined, because `child.FileType != 0` reads like a
// paranoia check and this is the sub-collection refusal. The only other value
// GetCollectionDetails has been observed to return for a child is 2,
// k_EWorkshopFileTypeCollection, which is why `filetype` is in the children
// envelope at all. A nested collection matters because it is invisible to
// every other cross-check: a collection IS a published file, so
// GetPublishedFileDetails answers for it with result 1, a title and the game's
// own consumer_app_id, and Merge would then drop every mod that lives inside
// the sub-collection and append an id the server cannot download.
const fileTypeItem = 0

// maxBatchItems bounds one GetPublishedFileDetails call.
//
// Steam accepts larger batches, and a 500-item collection in one request is
// also one request that either works or loses every item's metadata at once.
// 100 keeps a partial failure partial, which matters because the resolve path
// degrades per item.
const maxBatchItems = 100

// The package's refusals.
var (
	// ErrConfig is a source that cannot be constructed: no workshop app id, a
	// base URL that is not one.
	ErrConfig = errors.New("steamworkshop: invalid source configuration")
	// ErrAPI is an answer from Steam that gvm cannot use: a top-level result
	// that is not OK, a response that names an item nobody asked about, a
	// field that is not the shape the endpoint documents. TERMINAL for the
	// collection path, and a DEGRADATION for the resolve path -- see
	// Source.Resolve.
	ErrAPI = errors.New("steamworkshop: the Steam Web API returned an answer gvm cannot use")
	// ErrCollectionUnavailable is result==9 on the collection itself: private,
	// removed, or a typo. Refused rather than falling through to an empty
	// list, and it is the documented trigger for the HTML fallback.
	ErrCollectionUnavailable = errors.New(
		"steamworkshop: the collection is private, removed, or does not exist")
	// ErrWrongGame is a collection child whose consumer_app_id is not this
	// game's workshop appid. Not a parse error: it is the WRONG COLLECTION,
	// and rewriting a mod list from another game's collection is the one
	// outcome worth a hard stop.
	ErrWrongGame = errors.New("steamworkshop: the collection holds an item from another game")
	// ErrMarkup is the HTML fallback refusing: no items paired, or the
	// raw-count cross-check disagreeing with the pairing. Nothing is
	// rewritten.
	ErrMarkup = errors.New("steamworkshop: the collection page did not parse")
	// ErrMechanism is an acquisition mechanism this build does not implement
	// for the Workshop.
	ErrMechanism = errors.New("steamworkshop: unsupported acquisition mechanism")
	// ErrInput is a caller handing this source something it cannot act on: no
	// schema, the wrong source kind, no reading of the live config.
	ErrInput = errors.New("steamworkshop: invalid source input")
)

// Config constructs the source and its client.
type Config struct {
	// WorkshopAppID is the GAME appid -- 108600 for Project Zomboid, never the
	// 380870 server appid. It is required, because it is the only thing the
	// collection cross-check can compare a child's consumer_app_id against,
	// and a zero value would turn that check into a no-op rather than into a
	// refusal.
	WorkshopAppID int

	// Offline builds a source that makes no requests at all: `--no-fetch`, and
	// a box with no egress.
	//
	// It is a first-class mode rather than an error path, because section
	// 12.5's rule is that an unreachable API degrades to exactly the mod.info
	// heuristic. Offline is that degradation, asked for on purpose.
	Offline bool

	// Policy overrides the source's egress contract. nil means
	// fetch.PolicyFor(steam_workshop), which is the hard hostname allowlist of
	// section 12.4.
	//
	// The allowlist is enforced independently of the two base URLs below, so
	// an override of either cannot reach a host the policy does not name: a
	// test points both at a loopback literal and uses fetch.Loopback, and
	// production cannot be re-pointed by editing a URL.
	Policy *fetch.Policy

	// APIBase and CommunityBase override the two hostnames. Empty means the
	// defaults.
	APIBase       string
	CommunityBase string

	Log *slog.Logger
}

// API is the keyless metadata client.
type API struct {
	c         *fetch.Client
	api       string
	community string
	appID     int
	log       *slog.Logger
}

// NewAPI builds the metadata client.
func NewAPI(cfg Config) (*API, error) {
	if cfg.WorkshopAppID <= 0 {
		return nil, fmt.Errorf("%w: no workshop app id; with nothing to compare a child's "+
			"consumer_app_id against, a collection for another game reads as a valid "+
			"collection (the manifest declares workshop_appid: 108600 for Project Zomboid, "+
			"which is the GAME appid and not the 380870 server appid)", ErrConfig)
	}

	p := fetch.Policy{}
	if cfg.Policy != nil {
		p = *cfg.Policy
	} else {
		def, err := fetch.PolicyFor(spec.SourceSteamWorkshop)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		p = def
	}
	c, err := fetch.New(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	api, err := cleanBase("api_base", cfg.APIBase, DefaultAPIBase)
	if err != nil {
		return nil, err
	}
	community, err := cleanBase("community_base", cfg.CommunityBase, DefaultCommunityBase)
	if err != nil {
		return nil, err
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &API{c: c, api: api, community: community, appID: cfg.WorkshopAppID, log: log}, nil
}

// AppID is the workshop (game) appid this client cross-checks against.
func (a *API) AppID() int { return a.appID }

// Policy is the egress contract this client enforces, for a `gvm doctor` line.
func (a *API) Policy() fetch.Policy { return a.c.Policy() }

// cleanBase validates a base URL and strips its trailing slash.
//
// A base carrying a query or a fragment is refused rather than cleaned: the
// endpoints below append their own path, and silently dropping somebody's
// "?key=…" would be dropping the thing they were trying to add.
func cleanBase(field, raw, def string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = def
	}
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q: %v", ErrConfig, field, raw, err)
	}
	switch {
	case u.Scheme == "" || u.Host == "":
		return "", fmt.Errorf("%w: %s %q needs a scheme and a host", ErrConfig, field, raw)
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%w: %s %q carries a query or a fragment, and the endpoint paths "+
			"are appended to it", ErrConfig, field, raw)
	}
	return s, nil
}

// flexInt64 decodes a number that Steam has returned both ways.
//
// file_size comes back as a JSON number from some revisions of
// GetPublishedFileDetails and as a decimal STRING from others, on the same
// endpoint. A plain int64 field silently becomes 0 on the string form, and a
// zero file_size is a disk pre-flight that passes on a 6 GB item.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Refused rather than coerced. A float or an expression where a byte
		// count belongs is a field that has changed meaning, and guessing is
		// how a size pre-flight starts lying.
		return fmt.Errorf("%w: %q is not a byte count", ErrAPI, string(b))
	}
	*f = flexInt64(n)
	return nil
}

// detailsEnvelope is GetPublishedFileDetails' response.
//
// DisallowUnknownFields is not used anywhere in this package: Steam adds
// fields, and a sync that broke because "views" became "view_count" would be a
// sync that breaks for no reason. The fields gvm depends on are cross-checked
// instead, per item, which is section 12.5's rule.
type detailsEnvelope struct {
	Response struct {
		Result      int             `json:"result"`
		ResultCount int             `json:"resultcount"`
		Details     []publishedFile `json:"publishedfiledetails"`
	} `json:"response"`
}

type publishedFile struct {
	PublishedFileID string    `json:"publishedfileid"`
	Result          int       `json:"result"`
	CreatorAppID    int       `json:"creator_app_id"`
	ConsumerAppID   int       `json:"consumer_app_id"`
	FileSize        flexInt64 `json:"file_size"`
	Title           string    `json:"title"`
	TimeCreated     int64     `json:"time_created"`
	TimeUpdated     int64     `json:"time_updated"`
	Visibility      int       `json:"visibility"`
	Banned          bool      `json:"banned"`
	BanReason       string    `json:"ban_reason"`
	// FileType is the published file's EWorkshopFileType, when the endpoint
	// supplies one. ISteamRemoteStorage has not been observed to return it, so
	// the usual reading is the zero value -- which is fileTypeItem, so this
	// decodes to "an ordinary item" and the check below is dormant on this
	// endpoint. It is decoded anyway because it is the ONLY signal the HTML
	// fallback could ever have that a scraped row is a nested collection: a
	// page carries no filetype, and Enrich is the only cross-check that path
	// gets. A dormant assertion that can refuse the day Steam does answer is
	// worth more than no assertion at all, and it cannot produce a false
	// refusal while the field is absent.
	FileType int `json:"file_type"`
	Tags     []struct {
		Tag string `json:"tag"`
	} `json:"tags"`
}

// Details is one batched metadata reading, with every requested id placed in
// exactly one of the three buckets.
//
// Three buckets rather than a map plus an error, because the three lead to
// three different decisions. An item with metadata contributes evidence; an
// item Steam refused is recorded as removed or banned and KEPT; an item the
// response never mentioned is the one the collection path refuses on, because
// neither a confirmation nor a denial arrived.
type Details struct {
	// Upstream is the items the API answered for with result==1.
	Upstream map[content.AcquireID]*content.Upstream
	// Refused maps an id to the non-OK result code Steam returned for it.
	// resultNotFound (9) is private, removed, or never existed.
	Refused map[content.AcquireID]int
	// Absent are the ids the response did not mention at all.
	Absent []content.AcquireID

	// FileTypes is each answered id's EWorkshopFileType, for the one caller
	// that has to know an id is not an ordinary item: Enrich, on the HTML
	// path, where no filetype came off the page.
	//
	// Package-local rather than a field on content.Upstream, because this is a
	// cross-check input and not evidence an operator needs in the lockfile --
	// and because a new Upstream field would change the shape of every lock
	// gvm has written for a value that is 0 on every real item.
	FileTypes map[content.AcquireID]int
}

// FileType is one answered id's EWorkshopFileType, and fileTypeItem when the
// endpoint said nothing -- which is the answer that lets the check pass.
func (d *Details) FileType(id content.AcquireID) int {
	if d == nil {
		return fileTypeItem
	}
	return d.FileTypes[id]
}

// Reason renders one refused id's code as a sentence.
func (d *Details) Reason(id content.AcquireID) string {
	if d == nil {
		return ""
	}
	code, ok := d.Refused[id]
	if !ok {
		return ""
	}
	if code == resultNotFound {
		return "the Workshop reports it as private, removed, or never published (result 9)"
	}
	return fmt.Sprintf("the Workshop returned result %d for it", code)
}

// Details fetches metadata for ids, in batches.
//
// It returns an error for anything that makes the WHOLE reading untrustworthy:
// a transport failure, a top-level result that is not OK, or a response that
// names an item nobody asked about. The last one matters more than it looks:
// GetPublishedFileDetails is addressed by indexed form fields
// (publishedfileids[0], [1], …) and matched up by the id it echoes, so a
// response carrying an unexpected id means the pairing is wrong and every
// tag would be attributed to the wrong mod.
//
// Per-ITEM failures are not errors. They are in Refused and Absent, and the
// caller decides: Source.Resolve records them as evidence and keeps the
// operator's items, and the collection path refuses the rewrite.
func (a *API) Details(ctx context.Context, ids []content.AcquireID) (*Details, error) {
	out := &Details{
		Upstream:  map[content.AcquireID]*content.Upstream{},
		Refused:   map[content.AcquireID]int{},
		FileTypes: map[content.AcquireID]int{},
	}
	want := make([]content.AcquireID, 0, len(ids))
	seen := make(map[content.AcquireID]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		if !numericID(string(id)) {
			return nil, fmt.Errorf("%w: %q is not a publishedfileid, so it cannot be looked "+
				"up; the acquisition namespace for a Steam Workshop block is numeric",
				ErrConfig, id)
		}
		seen[id] = true
		want = append(want, id)
	}
	if len(want) == 0 {
		return out, nil
	}

	for chunk := range batches(want, maxBatchItems) {
		if err := a.detailsBatch(ctx, chunk, out); err != nil {
			return nil, err
		}
	}
	for _, id := range want {
		if _, ok := out.Upstream[id]; ok {
			continue
		}
		if _, ok := out.Refused[id]; ok {
			continue
		}
		out.Absent = append(out.Absent, id)
	}
	return out, nil
}

func (a *API) detailsBatch(ctx context.Context, chunk []content.AcquireID, out *Details) error {
	// The request set, per REQUEST. Scoped to the chunk and not to the whole
	// resolve, because the pairing this check defends is one
	// GetPublishedFileDetails call's indexed form fields against the ids that
	// call echoed back. A resolve-wide set answers the much weaker question
	// "is this id somewhere in the list", which any list longer than
	// maxBatchItems defeats: batch one answers for a batch-two member, batch
	// two stays silent about it, the duplicate guard has nothing to fire on
	// and the Absent sweep finds nothing missing.
	requested := make(map[content.AcquireID]bool, len(chunk))
	for _, id := range chunk {
		requested[id] = true
	}

	form := url.Values{}
	form.Set("itemcount", strconv.Itoa(len(chunk)))
	for i, id := range chunk {
		form.Set("publishedfileids["+strconv.Itoa(i)+"]", string(id))
	}

	var env detailsEnvelope
	if err := a.c.PostFormJSON(ctx, a.api+pathPublishedFileDetails, form, &env); err != nil {
		return fmt.Errorf("GetPublishedFileDetails for %d item(s): %w", len(chunk), err)
	}
	if env.Response.Result != resultOK {
		return fmt.Errorf("%w: GetPublishedFileDetails returned result %d for %d item(s)",
			ErrAPI, env.Response.Result, len(chunk))
	}

	for _, d := range env.Response.Details {
		id := content.AcquireID(strings.TrimSpace(d.PublishedFileID))
		if id == "" {
			return fmt.Errorf("%w: GetPublishedFileDetails returned an entry with no "+
				"publishedfileid, so there is no way to tell which item it describes", ErrAPI)
		}
		if !requested[id] {
			// The pairing is wrong. Degrading here instead of refusing would
			// mean one item's tags recorded against another item's mod.
			return fmt.Errorf("%w: GetPublishedFileDetails returned details for %s, which was "+
				"not in the request; the indexed form fields and the echoed ids no longer "+
				"agree, so every item's metadata would be suspect", ErrAPI, id)
		}
		_, haveUp := out.Upstream[id]
		_, haveRef := out.Refused[id]
		if haveUp || haveRef {
			return fmt.Errorf("%w: GetPublishedFileDetails returned %s twice", ErrAPI, id)
		}
		if d.Result != resultOK {
			out.Refused[id] = d.Result
			continue
		}
		out.FileTypes[id] = d.FileType
		out.Upstream[id] = upstreamOf(d)
	}
	return nil
}

// upstreamOf renders one item's metadata as the lock's upstream block.
func upstreamOf(d publishedFile) *content.Upstream {
	u := &content.Upstream{
		API: APIName,
		// Sanitized at the boundary, not at the printers. This is where
		// third-party display text enters the package, and it is also the one
		// call site that covers every later surface: the collection report,
		// the `note:` Merge writes into the operator's instance YAML, and
		// Upstream.Title persisted in the lock for `gvm content why` to
		// reprint months later.
		Title:         sanitizeTitle(d.Title),
		Tags:          tagNames(d.Tags),
		ConsumerAppID: d.ConsumerAppID,
		FileSize:      int64(d.FileSize),
		Banned:        d.Banned,
		BanReason:     sanitizeTitle(d.BanReason),
	}
	if d.TimeUpdated > 0 {
		// The cheap change signal for a tree artifact: gvm never had these
		// bytes, so "has this item moved" is this timestamp and nothing else
		// until `gvm content verify --deep` walks the tree.
		u.TimeUpdated = time.Unix(d.TimeUpdated, 0).UTC()
	}
	return u
}

// tagNames flattens tags[] into the strings internal/pz scores as evidence.
//
// Order is the page's order, repeats are kept: pz.TagsFrom de-duplicates the
// build opinion, and a report showing the tags as the page lists them is a
// report an operator can check against the page.
func tagNames(in []struct {
	Tag string `json:"tag"`
}) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if s := strings.TrimSpace(t.Tag); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// numericID reports whether s is a publishedfileid.
//
// Hand-written because the alternative, strconv.ParseUint, accepts "+12" and a
// leading "0x" in some bases and would make "0x3171167894" an id. The
// acquisition namespace for this source is decimal digits and nothing else.
func numericID(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// batches yields consecutive slices of at most n.
func batches[T any](in []T, n int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(in); start += n {
			end := min(start+n, len(in))
			if !yield(in[start:end]) {
				return
			}
		}
	}
}

// maxTitleBytes bounds a display string from the Workshop.
//
// A real mod title is well under a hundred bytes. This is generous enough that
// no legitimate name is clipped and small enough that a title cannot be used to
// bury a report under a wall of plausible text.
const maxTitleBytes = 300

// sanitizeTitle makes a third-party display string safe to print, to persist as
// a `note:` in the operator's own YAML, and to reprint later out of the lock.
//
// A Workshop title and a ban reason are the same class of bytes as a forged
// appmanifest's `name`, which provider/steamcmd.sanitizeLabel already guards
// for the reason ARCHITECTURE 18.1 gives: a compromised workshop item owns that
// instance's install and data trees, and these strings come off the same
// supply chain. This path had four operator-facing surfaces and no sink -- the
// `+ <id> <title>` line a collection sync prints, the `note:` it WRITES into
// the instance YAML, the flagged-item report, and `gvm content why`'s row
// label out of the lock.
//
// What goes, and why:
//
//   - C0, DEL and C1, so a title cannot carry a newline plus a forged "- <id>
//     no longer in the collection" line and manufacture a line of gvm's own
//     output, nor an ANSI erase-line to hide what follows it. Dropped by RUNE
//     and not by byte, because 0x80-0x9F are also UTF-8 continuation bytes and
//     a byte filter would corrupt every accented mod name on the Workshop.
//   - utf8.RuneError, which is invalid UTF-8 or a literal U+FFFD; neither is
//     information in a mod name, and a lockfile is JSON.
//   - whitespace RUNS, folded to one space, which is what stops the forged
//     line surviving as indented plausible text once its newline is gone.
//
// Stripped and not refused: a cosmetic oddity in a display string must not fail
// a sync that is otherwise sound. The decisions this package makes are made on
// ids and result codes, never on a title.
func sanitizeTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			continue
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// A dropped control character still separates two words, so it
			// folds into the whitespace run rather than gluing them together.
			space = b.Len() > 0
			continue
		case r == ' ' || r == '\t':
			space = b.Len() > 0
			continue
		}
		if space {
			if b.Len()+1+utf8.RuneLen(r) > maxTitleBytes {
				return b.String()
			}
			b.WriteByte(' ')
			space = false
		}
		if b.Len()+utf8.RuneLen(r) > maxTitleBytes {
			return b.String()
		}
		b.WriteRune(r)
	}
	return b.String()
}
