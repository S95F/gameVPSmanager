package steamworkshop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/fetch"
)

func TestDetailsReadsTheKeylessEndpoint(t *testing.T) {
	s := newStub(t)
	det, err := s.api(t).Details(context.Background(),
		ids("3171167894", "3435796523", "3508537032", "9999999999"))
	if err != nil {
		t.Fatalf("Details: %v", err)
	}

	lib := det.Upstream["3171167894"]
	if lib == nil {
		t.Fatal("no metadata for 3171167894")
	}
	if lib.API != APIName {
		t.Errorf("api = %q, want %q (the manifest's metadata_api spelling)", lib.API, APIName)
	}
	if lib.Title != "that DAMN Library" {
		t.Errorf("title = %q", lib.Title)
	}
	if lib.ConsumerAppID != pzWorkshopAppID {
		t.Errorf("consumer_app_id = %d, want %d", lib.ConsumerAppID, pzWorkshopAppID)
	}
	if lib.FileSize != 48211934 {
		t.Errorf("file_size = %d", lib.FileSize)
	}
	// time_updated is the cheap change signal for a tree artifact: gvm never
	// had these bytes, so until `verify --deep` walks the tree this timestamp
	// is the only thing that can say an item moved.
	if want := time.Unix(1755749469, 0).UTC(); !lib.TimeUpdated.Equal(want) {
		t.Errorf("time_updated = %v, want %v", lib.TimeUpdated, want)
	}
	// Tags in page order, repeats and non-build tags included: pz.TagsFrom
	// decides what carries a build opinion, and a report an operator can check
	// against the page needs the page's own list.
	if !slices.Equal(lib.Tags, []string{"Build 41", "Build 42", "Vehicles"}) {
		t.Errorf("tags = %v", lib.Tags)
	}

	if banned := det.Upstream["3508537032"]; banned == nil || !banned.Banned {
		t.Errorf("3508537032 is banned in the fixture: %+v", banned)
	} else if banned.BanReason == "" {
		t.Error("a banned item with no recorded reason is an operator with nothing to act on")
	}

	// result 9 is a REFUSAL, not an absence: Steam looked and said no.
	if code := det.Refused["9999999999"]; code != resultNotFound {
		t.Errorf("9999999999 refused with %d, want %d", code, resultNotFound)
	}
	if _, ok := det.Upstream["9999999999"]; ok {
		t.Error("a result-9 item was given metadata")
	}
	if len(det.Absent) != 0 {
		t.Errorf("absent = %v; every requested id was answered for", det.Absent)
	}
	if !strings.Contains(det.Reason("9999999999"), "result 9") {
		t.Errorf("the refusal reason does not name the code: %q", det.Reason("9999999999"))
	}
}

// TestFileSizeDecodesBothShapesSteamUses is not a hypothetical. The same
// endpoint has returned file_size as a JSON number and as a decimal string,
// and a plain int64 field silently becomes 0 on the string form -- which is a
// disk pre-flight that passes on a six-gigabyte item.
func TestFileSizeDecodesBothShapesSteamUses(t *testing.T) {
	s := newStub(t)
	det, err := s.api(t).Details(context.Background(), ids("3435796523"))
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	// The fixture carries this one as a STRING, deliberately.
	if got := det.Upstream["3435796523"].FileSize; got != 193844120 {
		t.Fatalf("file_size = %d, want 193844120 decoded from the string form", got)
	}

	for _, bad := range []string{`1.5`, `"12abc"`, `{}`, `"1e9"`} {
		var f flexInt64
		if err := json.Unmarshal([]byte(bad), &f); err == nil {
			t.Errorf("file_size %s decoded to %d; a field that changed meaning is refused "+
				"rather than coerced, because the value is a disk pre-flight", bad, f)
		}
	}
	for in, want := range map[string]flexInt64{`0`: 0, `null`: 0, `""`: 0, `"42"`: 42, `42`: 42} {
		var f flexInt64
		if err := json.Unmarshal([]byte(in), &f); err != nil || f != want {
			t.Errorf("file_size %s = %d, %v; want %d", in, f, err, want)
		}
	}
}

// TestDetailsRefusesAResponseNamingAnItemNobodyAskedFor is the cross-check that
// matters most in this file.
//
// GetPublishedFileDetails is addressed by indexed form fields and matched up by
// the id it echoes. If the two ever stop agreeing, every tag is attributed to
// the wrong mod -- which on a dual-build item means the wrong variant enabled,
// silently. Degrading here would record that; refusing loses the metadata and
// falls back to the files, which is the documented safe answer.
func TestDetailsRefusesAResponseNamingAnItemNobodyAskedFor(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"resultcount":1,
			"publishedfiledetails":[{"publishedfileid":"111","result":1,
			"consumer_app_id":108600,"title":"not what you asked for"}]}}`))
	}
	_, err := s.api(t).Details(context.Background(), ids("3171167894"))
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("error = %v, want ErrAPI", err)
	}
	if !strings.Contains(err.Error(), "111") {
		t.Errorf("the refusal does not name the unexpected id: %v", err)
	}
}

func TestDetailsRefusesADuplicateEntryAndANamelessOne(t *testing.T) {
	for name, body := range map[string]string{
		"twice": `{"response":{"result":1,"publishedfiledetails":[
			{"publishedfileid":"3171167894","result":1,"consumer_app_id":108600},
			{"publishedfileid":"3171167894","result":1,"consumer_app_id":108600}]}}`,
		"nameless": `{"response":{"result":1,"publishedfiledetails":[
			{"result":1,"consumer_app_id":108600,"title":"which item is this"}]}}`,
		"top level result": `{"response":{"result":8,"publishedfiledetails":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.details = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, []byte(body))
			}
			if _, err := s.api(t).Details(context.Background(), ids("3171167894")); !errors.Is(
				err, ErrAPI) {
				t.Fatalf("error = %v, want ErrAPI", err)
			}
		})
	}
}

// TestDetailsRecordsAnIDTheResponseNeverMentioned: neither confirmed nor
// denied is its own bucket, because the collection path refuses on it and the
// resolve path only degrades.
func TestDetailsRecordsAnIDTheResponseNeverMentioned(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[]}}`))
	}
	det, err := s.api(t).Details(context.Background(), ids("3171167894", "2866258937"))
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if !slices.Equal(det.Absent, ids("3171167894", "2866258937")) {
		t.Fatalf("absent = %v, want both ids", det.Absent)
	}
	if len(det.Upstream) != 0 || len(det.Refused) != 0 {
		t.Error("an item nobody answered for was placed in a bucket that implies an answer")
	}
	if det.Reason("3171167894") != "" {
		t.Error("an absent id has no refusal reason; it was never refused")
	}
}

func TestDetailsBatchesAndDeduplicates(t *testing.T) {
	s := newStub(t)
	want := make([]content.AcquireID, 0, maxBatchItems+5)
	for i := range maxBatchItems + 5 {
		want = append(want, content.AcquireID(strconv.Itoa(3000000000+i)))
	}
	// A repeat must not become a second request, and must not become a second
	// entry the "returned twice" check then fires on.
	want = append(want, want[0])

	if _, err := s.api(t).Details(context.Background(), want); err != nil {
		t.Fatalf("Details: %v", err)
	}
	got := s.requested()
	if len(got) != 2 {
		t.Fatalf("%d batch(es) for %d ids, want 2 of at most %d", len(got), len(want),
			maxBatchItems)
	}
	if len(got[0]) != maxBatchItems || len(got[1]) != 5 {
		t.Errorf("batch sizes = %d, %d", len(got[0]), len(got[1]))
	}
}

func TestDetailsRefusesANonNumericID(t *testing.T) {
	s := newStub(t)
	_, err := s.api(t).Details(context.Background(), ids("ThatDamnLibrary"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("error = %v, want ErrConfig", err)
	}
	if len(s.requested()) != 0 {
		t.Error("a request went out for an id that cannot be a publishedfileid")
	}
	// numericID is deliberately not strconv.ParseUint, which accepts forms
	// that are not ids.
	for _, bad := range []string{"", "+12", "0x3171167894", "12 ", "1_2", "-1", "3171167894\n"} {
		if numericID(bad) {
			t.Errorf("numericID(%q) = true", bad)
		}
	}
	if !numericID("3171167894") {
		t.Error("numericID rejected a real publishedfileid")
	}
}

func TestDetailsOfNothingMakesNoRequest(t *testing.T) {
	s := newStub(t)
	det, err := s.api(t).Details(context.Background(), nil)
	if err != nil || len(det.Upstream) != 0 {
		t.Fatalf("Details(nil) = %+v, %v", det, err)
	}
	if len(s.requested()) != 0 {
		t.Error("an empty declared list produced a request")
	}
}

// TestTheAllowlistStillRefusesAnOverriddenBase is the reason APIBase and
// CommunityBase are allowed to be exported fields at all.
//
// The allowlist is enforced independently of them -- in CheckURL before the
// request, on every redirect hop, and at the socket -- so an override cannot
// reach a host the policy does not name. A test points both at a loopback
// literal; production cannot be re-pointed by editing a URL.
func TestTheAllowlistStillRefusesAnOverriddenBase(t *testing.T) {
	a, err := NewAPI(Config{
		WorkshopAppID: pzWorkshopAppID,
		APIBase:       "https://evil.example.com",
		CommunityBase: "https://evil.example.com",
	})
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	if _, err := a.Details(context.Background(), ids("3171167894")); !errors.Is(err,
		fetch.ErrHostNotAllowed) {
		t.Fatalf("error = %v, want fetch.ErrHostNotAllowed; the policy, not the base URL, is "+
			"what bounds egress", err)
	}
	if _, err := a.Collection(context.Background(), "3374983339"); !errors.Is(err,
		fetch.ErrHostNotAllowed) {
		t.Errorf("collection error = %v, want fetch.ErrHostNotAllowed", err)
	}
	if _, err := a.CollectionHTML(context.Background(), "3374983339"); !errors.Is(err,
		fetch.ErrHostNotAllowed) {
		t.Errorf("page error = %v, want fetch.ErrHostNotAllowed", err)
	}
}

// TestNewAPIRefusesAZeroWorkshopAppID: with nothing to compare a child's
// consumer_app_id against, the collection cross-check becomes a no-op rather
// than a refusal -- and a no-op check is worse than no check, because it reads
// as one.
func TestNewAPIRefusesAZeroWorkshopAppID(t *testing.T) {
	for _, appID := range []int{0, -1} {
		if _, err := NewAPI(Config{WorkshopAppID: appID}); !errors.Is(err, ErrConfig) {
			t.Errorf("NewAPI(appid %d) error = %v, want ErrConfig", appID, err)
		}
	}
	if _, err := New(Config{Offline: true}); !errors.Is(err, ErrConfig) {
		t.Errorf("an offline source with no appid was built: %v", err)
	}
}

func TestNewAPIRefusesABaseThatIsNotOne(t *testing.T) {
	for _, bad := range []string{
		"api.steampowered.com",                  // no scheme
		"https://",                              // no host
		"https://api.steampowered.com/?key=abc", // a query the paths would be appended to
		"https://api.steampowered.com/#frag",
	} {
		if _, err := NewAPI(Config{WorkshopAppID: pzWorkshopAppID, APIBase: bad}); !errors.Is(
			err, ErrConfig) {
			t.Errorf("NewAPI(api_base %q) error = %v, want ErrConfig", bad, err)
		}
	}
	// The defaults are the documented hostnames and are on section 12.4's
	// allowlist.
	a, err := NewAPI(Config{WorkshopAppID: pzWorkshopAppID})
	if err != nil {
		t.Fatalf("NewAPI with defaults: %v", err)
	}
	for _, host := range []string{"api.steampowered.com", "steamcommunity.com"} {
		if !a.Policy().AllowHost(host) {
			t.Errorf("the default policy does not allow %s", host)
		}
	}
	if a.AppID() != pzWorkshopAppID {
		t.Errorf("AppID() = %d", a.AppID())
	}
}

func TestDetailsHonoursCancellation(t *testing.T) {
	s := newStub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.api(t).Details(ctx, ids("3171167894")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestTagNamesDropsEmptyTags: a tags[] entry with an empty tag is not an
// opinion, and an empty string in the evidence list would read as one.
func TestTagNamesDropsEmptyTags(t *testing.T) {
	s := newStub(t)
	s.details = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(fmt.Sprintf(`{"response":{"result":1,"publishedfiledetails":[
			{"publishedfileid":"3171167894","result":1,"consumer_app_id":%d,
			 "tags":[{"tag":"  "},{"tag":"Build 42"},{"tag":""}]}]}}`, pzWorkshopAppID)))
	}
	det, err := s.api(t).Details(context.Background(), ids("3171167894"))
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if got := det.Upstream["3171167894"].Tags; !slices.Equal(got, []string{"Build 42"}) {
		t.Errorf("tags = %#v", got)
	}
}

// TestDetailsRefusesACrossBatchEcho is the echoed-id cross-check, per REQUEST.
//
// The pairing this check defends is ONE GetPublishedFileDetails call's indexed
// form fields against the ids that call echoed back. A request set scoped to
// the whole resolve answers a different and much weaker question -- "is this id
// somewhere in the list" -- which any list longer than maxBatchItems defeats by
// splitting into chunks whose answers can then cross-contaminate. The duplicate
// guard cannot catch it either, because only one batch answered for the id.
func TestDetailsRefusesACrossBatchEcho(t *testing.T) {
	const intruder = "3000000120"
	want := make([]content.AcquireID, 0, maxBatchItems+50)
	for i := range maxBatchItems + 50 {
		want = append(want, content.AcquireID(strconv.Itoa(3000000000+i)))
	}

	s := newStub(t)
	s.details = func(w http.ResponseWriter, r *http.Request) {
		asked := askedFor(t, r)
		first := asked[0] == "3000000000"
		var out []string
		for _, id := range asked {
			if !first && id == intruder {
				// Batch two does NOT answer for it, so the duplicate guard has
				// nothing to fire on and the Absent sweep finds nothing
				// missing. Exactly one answer arrives for this id, from the
				// call that never asked about it.
				continue
			}
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":108600,"title":"ok"}`, id))
		}
		if first {
			// Batch one answers for an id that belongs to batch two.
			out = append(out, fmt.Sprintf(
				`{"publishedfileid":%q,"result":1,"consumer_app_id":108600,`+
					`"title":"WRONG ITEM"}`, intruder))
		}
		writeJSON(w, []byte(`{"response":{"result":1,"publishedfiledetails":[`+
			strings.Join(out, ",")+`]}}`))
	}

	_, err := s.api(t).Details(context.Background(), want)
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("error = %v, want ErrAPI: one batch's answer carried another batch's item",
			err)
	}
	if !strings.Contains(err.Error(), intruder) {
		t.Errorf("the refusal does not name the intruding id: %v", err)
	}
}
