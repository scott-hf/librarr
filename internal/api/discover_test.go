package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/db"
	"github.com/JeremiahM37/librarr/internal/discover"
)

// discoverTestServer returns an authenticated-API handler (API key "k")
// serving the real discover routes against a fake Open Library, plus the
// recorded Open Library queries.
func discoverTestServer(t *testing.T) (http.Handler, *[]url.Values, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var queries []url.Values
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		queries = append(queries, r.URL.Query())
		var docs []string
		for i := 0; i < 20; i++ {
			docs = append(docs, fmt.Sprintf(`{"key":"/works/OL%dW","title":"Book %d","author_name":["A"],"cover_i":%d,"readinglog_count":%d}`, i, i, 100+i, 500-i))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"numFound":9000,"docs":[%s]}`, strings.Join(docs, ","))
	}))
	t.Cleanup(ol.Close)

	dc := discover.New(ol.Client(), "ops@example.org")
	dc.SetBaseURL(ol.URL)

	database, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	s := &Server{
		cfg:            &config.Config{APIKey: "k"},
		db:             database,
		mux:            http.NewServeMux(),
		sessions:       NewSessionStore(),
		discoverClient: dc,
	}
	s.registerSearchRoutes()
	return authMiddleware(s.cfg, s.db, s.sessions, s.mux), &queries, &hits
}

func getDiscover(h http.Handler, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeDiscover(t *testing.T, rr *httptest.ResponseRecorder) discover.Result {
	t.Helper()
	var res discover.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("body is not a discover response: %v\n%s", err, rr.Body.String())
	}
	return res
}

func TestDiscoverRequiresAuth(t *testing.T) {
	h, _, hits := discoverTestServer(t)
	for _, p := range []string{
		"/api/discover/popular?genre=Fantasy",
		"/api/discover/similar?title=T&author=A",
		"/api/discover/options",
		"/api/discover/browse?catalog=popular",
	} {
		if rr := getDiscover(h, p, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without key: HTTP %d, want 401", p, rr.Code)
		}
		if rr := getDiscover(h, p, "wrong"); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s with wrong key: HTTP %d, want 401", p, rr.Code)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("unauthenticated requests reached Open Library (%d)", hits.Load())
	}
}

func TestDiscoverPopularHandler(t *testing.T) {
	h, queries, _ := discoverTestServer(t)
	rr := getDiscover(h, "/api/discover/popular?genre=Science+Fiction&limit=3", "k")
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	res := decodeDiscover(t, rr)
	if res.Status != discover.StatusOK || len(res.Items) != 3 || res.ResolvedSubject != "science fiction" {
		t.Errorf("got %+v", res)
	}
	if res.Items[0].Key != "/works/OL0W" || !strings.HasPrefix(res.Items[0].CoverURL, "https://covers.openlibrary.org/b/id/") {
		t.Errorf("item shape: %+v", res.Items[0])
	}
	if got := (*queries)[0].Get("subject"); got != "science fiction" {
		t.Errorf("OL subject %q", got)
	}
}

func TestDiscoverLimitIsClamped(t *testing.T) {
	h, _, _ := discoverTestServer(t)
	for _, tc := range []struct {
		q    string
		want int
	}{
		{"", discover.DefaultLimit},
		{"&limit=999", discover.MaxLimit},
		{"&limit=abc", discover.DefaultLimit},
		{"&limit=-4", discover.DefaultLimit},
		{"&limit=7", 7},
	} {
		res := decodeDiscover(t, getDiscover(h, "/api/discover/popular?genre=Fantasy"+tc.q, "k"))
		if len(res.Items) != tc.want {
			t.Errorf("limit %q -> %d items, want %d", tc.q, len(res.Items), tc.want)
		}
	}
}

func TestDiscoverBlankInputIsHTTP200WithInvalidRequest(t *testing.T) {
	h, _, hits := discoverTestServer(t)
	for _, p := range []string{
		"/api/discover/popular",
		"/api/discover/popular?genre=%20%20",
		"/api/discover/similar",
		"/api/discover/similar?title=&author=A",
	} {
		rr := getDiscover(h, p, "k")
		if rr.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d, want 200", p, rr.Code)
			continue
		}
		res := decodeDiscover(t, rr)
		if res.Status != discover.StatusInvalidRequest || res.Error == "" || len(res.Items) != 0 {
			t.Errorf("%s: %+v", p, res)
		}
		if !strings.Contains(rr.Body.String(), `"items":[]`) {
			t.Errorf("%s: items must serialize as [] : %s", p, rr.Body.String())
		}
	}
	if hits.Load() != 0 {
		t.Errorf("blank input reached Open Library (%d)", hits.Load())
	}
}

func TestDiscoverSimilarHandler(t *testing.T) {
	h, queries, _ := discoverTestServer(t)
	rr := getDiscover(h, "/api/discover/similar?title=Book+0&author=A&genre=Fantasy", "k")
	res := decodeDiscover(t, rr)
	if rr.Code != http.StatusOK || res.Status != discover.StatusOK {
		t.Fatalf("HTTP %d %+v", rr.Code, res)
	}
	for _, it := range res.Items {
		if it.Title == "Book 0" {
			t.Error("seed title was not excluded")
		}
	}
	if got := (*queries)[0].Get("author"); got != "A" {
		t.Errorf("OL author %q", got)
	}
}

func TestDiscoverWithoutClientDegrades(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDiscoverPopular(rr, httptest.NewRequest(http.MethodGet, "/api/discover/popular?genre=x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rr.Code)
	}
	if res := decodeDiscover(t, rr); res.Status != discover.StatusUpstreamUnavailable {
		t.Errorf("got %+v", res)
	}
}

func decodeStrict(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(rr.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("body does not match the wire type: %v\n%s", err, rr.Body.String())
	}
}

func TestDiscoverOptionsHandler(t *testing.T) {
	h, _, hits := discoverTestServer(t)
	rr := getDiscover(h, "/api/discover/options", "k")
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var res discover.OptionsResult
	decodeStrict(t, rr, &res)
	if res.Status != discover.StatusOK || len(res.Catalogs) != 3 || len(res.Genres) != len(discover.CuratedGenres()) {
		t.Errorf("got %+v", res)
	}
	if got := res.Catalogs[0]; got.ID != "popular" || got.Label != "Popular" {
		t.Errorf("first catalog %+v", got)
	}
	if hits.Load() != 0 {
		t.Errorf("options reached Open Library (%d)", hits.Load())
	}
}

func TestDiscoverBrowseHandler(t *testing.T) {
	h, queries, _ := discoverTestServer(t)
	rr := getDiscover(h, "/api/discover/browse?catalog=new&genre=Science+Fiction&offset=20", "k")
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var res discover.BrowseResult
	decodeStrict(t, rr, &res)
	if res.Status != discover.StatusOK || res.Catalog != "new" || res.Genre != "Science Fiction" || res.Offset != 20 {
		t.Errorf("echo/status wrong: %+v", res)
	}
	if res.NextOffset == nil || *res.NextOffset != 40 || res.Partial || len(res.Items) == 0 {
		t.Errorf("paging wrong: next=%v partial=%v items=%d", res.NextOffset, res.Partial, len(res.Items))
	}
	q := (*queries)[0]
	if !strings.Contains(q.Get("q"), `subject:"science fiction"`) || q.Get("sort") != "readinglog" || q.Get("offset") != "20" {
		t.Errorf("OL query %v", q)
	}
}

func TestDiscoverBrowseBadInputIsHTTP200WithInvalidRequest(t *testing.T) {
	h, _, hits := discoverTestServer(t)
	for _, p := range []string{
		"/api/discover/browse",
		"/api/discover/browse?catalog=trending",
		"/api/discover/browse?catalog=popular&offset=abc",
		"/api/discover/browse?catalog=popular&offset=7",
		"/api/discover/browse?catalog=popular&offset=200",
		"/api/discover/browse?catalog=popular&offset=-20",
		"/api/discover/browse?catalog=popular&genre=a%22b",
	} {
		rr := getDiscover(h, p, "k")
		if rr.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d, want 200", p, rr.Code)
			continue
		}
		var res discover.BrowseResult
		decodeStrict(t, rr, &res)
		if res.Status != discover.StatusInvalidRequest || res.Error == "" || len(res.Items) != 0 || res.NextOffset != nil {
			t.Errorf("%s: %+v", p, res)
		}
		if !strings.Contains(rr.Body.String(), `"items":[]`) || !strings.Contains(rr.Body.String(), `"next_offset":null`) {
			t.Errorf("%s: items must be [] and next_offset null: %s", p, rr.Body.String())
		}
	}
	if hits.Load() != 0 {
		t.Errorf("bad input reached Open Library (%d)", hits.Load())
	}
}

func TestEveryDiscoverResponseIsNoCache(t *testing.T) {
	h, _, _ := discoverTestServer(t)
	for _, p := range []string{
		"/api/discover/popular?genre=Fantasy",
		"/api/discover/popular",
		"/api/discover/similar?title=Book+0&author=A",
		"/api/discover/options",
		"/api/discover/browse?catalog=popular&genre=fantasy",
		"/api/discover/browse?catalog=nope",
	} {
		rr := getDiscover(h, p, "k")
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", p, got)
		}
	}
}

func TestDiscoverBrowseAndOptionsWithoutClientDegrade(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDiscoverBrowse(rr, httptest.NewRequest(http.MethodGet, "/api/discover/browse?catalog=new&genre=x&offset=40", nil))
	var b discover.BrowseResult
	decodeStrict(t, rr, &b)
	if rr.Code != http.StatusOK || b.Status != discover.StatusUpstreamUnavailable || b.Catalog != "new" || b.Offset != 40 || b.Items == nil {
		t.Errorf("browse: HTTP %d %+v", rr.Code, b)
	}
	rr = httptest.NewRecorder()
	s.handleDiscoverOptions(rr, httptest.NewRequest(http.MethodGet, "/api/discover/options", nil))
	var o discover.OptionsResult
	decodeStrict(t, rr, &o)
	if rr.Code != http.StatusOK || o.Status != discover.StatusUpstreamUnavailable {
		t.Errorf("options: HTTP %d %+v", rr.Code, o)
	}
}

func TestRateLimiterRoutesDiscoverSeparately(t *testing.T) {
	rl := NewRateLimiter(60, map[string]int{"discover": 5, "api": 100, "default": 600})
	for _, p := range []string{"/api/discover/popular", "/api/discover/similar", "/api/discover/options", "/api/discover/browse"} {
		if got := rl.ruleForPath(p); got != "discover" {
			t.Errorf("ruleForPath(%q) = %q, want discover", p, got)
		}
	}
}
