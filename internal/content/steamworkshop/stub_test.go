package steamworkshop

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/content"
	"github.com/S95F/gameVPSmanager/internal/content/fetch"
)

// NO TEST IN THIS PACKAGE TOUCHES THE REAL NETWORK, and the construction is
// what guarantees it rather than a convention.
//
// Every client is built from fetch.Loopback("127.0.0.1"), which is the only
// policy constructor that permits plain HTTP and which refuses any host that
// is not a loopback literal or a reserved never-resolvable name. Both base
// URLs point at an httptest server, whose address is a loopback literal. A
// test that reached api.steampowered.com would have to change the policy to do
// it, and the allowlist test below proves the policy is still what refuses.

// stub serves the two keyless endpoints and the collection page out of
// testdata, and records what was asked of it.
type stub struct {
	mu        sync.Mutex
	batches   [][]string
	collCalls int
	pageCalls int

	// Each may be replaced to exercise an answer no fixture should contain.
	details    http.HandlerFunc
	collection http.HandlerFunc
	page       http.HandlerFunc

	srv *httptest.Server
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{}
	mux := http.NewServeMux()
	mux.HandleFunc(pathPublishedFileDetails, func(w http.ResponseWriter, r *http.Request) {
		if s.details != nil {
			s.details(w, r)
			return
		}
		s.serveDetails(t, w, r)
	})
	mux.HandleFunc(pathCollectionDetails, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.collCalls++
		s.mu.Unlock()
		if s.collection != nil {
			s.collection(w, r)
			return
		}
		writeJSON(w, fixture(t, "getcollectiondetails.json"))
	})
	mux.HandleFunc(pathCollectionPage, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.pageCalls++
		s.mu.Unlock()
		if s.page != nil {
			s.page(w, r)
			return
		}
		if got := r.URL.Query().Get("id"); got == "" {
			http.Error(w, "no id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write(fixture(t, "collection.html"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// api builds a client pointed at the stub, through the loopback policy.
func (s *stub) api(t *testing.T) *API {
	t.Helper()
	p := fetch.Loopback("127.0.0.1")
	a, err := NewAPI(Config{
		WorkshopAppID: pzWorkshopAppID,
		Policy:        &p,
		APIBase:       s.srv.URL,
		CommunityBase: s.srv.URL,
	})
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	return a
}

// requested is every batch of ids the stub was asked about, in order.
func (s *stub) requested() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.batches))
	copy(out, s.batches)
	return out
}

// serveDetails answers out of the fixture, filtered to what was asked for.
//
// Filtered rather than returned whole, because the real endpoint answers the
// request and a stub that ignored it would make the "named an item nobody
// asked for" cross-check fire on every subset test -- hiding the bug the
// check exists for behind a test artefact.
func (s *stub) serveDetails(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	want := askedFor(t, r)
	s.mu.Lock()
	s.batches = append(s.batches, want)
	s.mu.Unlock()

	byID := map[string]json.RawMessage{}
	for _, raw := range fixtureDetails(t) {
		var head struct {
			ID string `json:"publishedfileid"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatalf("fixture entry: %v", err)
		}
		byID[head.ID] = raw
	}

	out := make([]json.RawMessage, 0, len(want))
	for _, id := range want {
		if raw, ok := byID[id]; ok {
			out = append(out, raw)
			continue
		}
		out = append(out, json.RawMessage(
			fmt.Sprintf(`{"publishedfileid":%q,"result":9}`, id)))
	}
	writeJSON(w, envelope(t, out))
}

// askedFor decodes the indexed form fields the endpoint is addressed by, and
// cross-checks itemcount against them: a client that sent a count disagreeing
// with its own fields would be a client whose batching had drifted.
func askedFor(t *testing.T, r *http.Request) []string {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if r.Method != http.MethodPost {
		t.Errorf("GetPublishedFileDetails was called with %s; the endpoint is POST-only",
			r.Method)
	}
	var out []string
	for i := 0; ; i++ {
		v := r.PostForm.Get("publishedfileids[" + strconv.Itoa(i) + "]")
		if v == "" {
			break
		}
		out = append(out, v)
	}
	if got := r.PostForm.Get("itemcount"); got != strconv.Itoa(len(out)) {
		t.Errorf("itemcount = %q and %d ids were sent", got, len(out))
	}
	return out
}

func envelope(t *testing.T, details []json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"response": map[string]any{
			"result":               resultOK,
			"resultcount":          len(details),
			"publishedfiledetails": details,
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func fixtureDetails(t *testing.T) []json.RawMessage {
	t.Helper()
	var env struct {
		Response struct {
			Details []json.RawMessage `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(fixture(t, "getpublishedfiledetails.json"), &env); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(env.Response.Details) == 0 {
		t.Fatal("the details fixture is empty, which would make every assertion vacuous")
	}
	return env.Response.Details
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(body)
}

// ids is a shorthand for a declared acquisition list.
func ids(in ...string) []content.AcquireID {
	out := make([]content.AcquireID, 0, len(in))
	for _, s := range in {
		out = append(out, content.AcquireID(s))
	}
	return out
}

// pzWorkshopAppID is Project Zomboid's GAME appid, which is what a workshop
// block declares. 380870 is the SERVER appid and appears nowhere here.
const pzWorkshopAppID = 108600

// loopbackPolicy is the only policy constructor that permits plain HTTP, and
// it refuses any host that is not a loopback literal or a reserved
// never-resolvable name. Every client in this package's tests is built from
// it, which is what makes "no test touches the real network" a property of the
// construction rather than a convention.
func loopbackPolicy() *fetch.Policy {
	p := fetch.Loopback("127.0.0.1")
	return &p
}
