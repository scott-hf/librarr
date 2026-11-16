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
	for _, p := range []string{"/api/discover/popular?genre=Fantasy", "/api/discover/similar?title=T&author=A"} {
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

func TestRateLimiterRoutesDiscoverSeparately(t *testing.T) {
	rl := NewRateLimiter(60, map[string]int{"discover": 5, "api": 100, "default": 600})
	for _, p := range []string{"/api/discover/popular", "/api/discover/similar"} {
		if got := rl.ruleForPath(p); got != "discover" {
			t.Errorf("ruleForPath(%q) = %q, want discover", p, got)
		}
	}
}
