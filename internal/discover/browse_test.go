package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// browseOK serves a full page of covered docs from a large result set.
func browseOK(prefix string) func(url.Values) (int, string) {
	return func(q url.Values) (int, string) { return 200, docsJSON(prefix, 20, 20, 5000) }
}

func browse(c *Client, catalog, genre string, offset int) BrowseResult {
	return c.Browse(context.Background(), catalog, genre, offset)
}

func TestBrowseSendsExactOpenLibraryQueryPerCatalog(t *testing.T) {
	const fields = "key,title,author_name,first_publish_year,cover_i,readinglog_count"
	for _, tc := range []struct {
		name, catalog, genre string
		offset               int
		want                 map[string]string // every param that must be present and equal
		absent               []string
	}{
		{"popular genre", CatalogPopular, "Science Fiction", 20,
			map[string]string{"q": `subject_key:"science_fiction"`, "sort": "readinglog", "offset": "20", "language": "eng", "limit": "20", "fields": fields},
			[]string{"subject"}},
		{"popular apostrophe genre", CatalogPopular, "children's", 0,
			map[string]string{"q": `subject_key:"children's_fiction"`, "sort": "readinglog", "offset": "0", "language": "eng", "limit": "20"},
			[]string{"subject"}},
		{"popular hyphen genre", CatalogPopular, "self-help", 0,
			map[string]string{"q": `subject_key:"self-help"`, "sort": "readinglog", "offset": "0", "language": "eng", "limit": "20"},
			[]string{"subject"}},
		{"popular multiword genre", CatalogPopular, "historical fiction", 0,
			map[string]string{"q": `subject_key:"historical_fiction"`, "sort": "readinglog", "offset": "0", "language": "eng", "limit": "20"},
			[]string{"subject"}},
		{"popular all genres", CatalogPopular, "", 0,
			map[string]string{"q": "language:eng", "sort": "readinglog", "offset": "0", "language": "eng", "limit": "20", "fields": fields},
			[]string{"subject"}},
		{"new genre", CatalogNew, "fantasy", 40,
			map[string]string{"q": `subject_key:"fantasy" first_publish_year:[2025 TO 2026] cover_i:[1 TO *]`, "sort": "readinglog", "offset": "40", "language": "eng", "limit": "20", "fields": fields},
			[]string{"subject"}},
		{"new all genres", CatalogNew, "", 0,
			map[string]string{"q": "first_publish_year:[2025 TO 2026] cover_i:[1 TO *]", "sort": "readinglog", "offset": "0", "language": "eng", "limit": "20"},
			[]string{"subject"}},
		{"top_rated genre", CatalogTopRated, "children's", 0,
			map[string]string{"q": `subject_key:"children's_fiction" ratings_count:[50 TO *]`, "sort": "rating", "offset": "0", "language": "eng", "limit": "20"},
			[]string{"subject"}},
		{"top_rated all genres", CatalogTopRated, "", 180,
			map[string]string{"q": "ratings_count:[100 TO *]", "sort": "rating", "offset": "180", "language": "eng", "limit": "20"},
			[]string{"subject"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOL(t, browseOK("W"))
			c, _ := newTestClient(f)
			res := browse(c, tc.catalog, tc.genre, tc.offset)
			if res.Status != StatusOK {
				t.Fatalf("status %q: %+v", res.Status, res)
			}
			if f.hits() != 1 {
				t.Fatalf("%d Open Library requests, want 1", f.hits())
			}
			q := f.query(0)
			for k, v := range tc.want {
				if got := q.Get(k); got != v {
					t.Errorf("param %s = %q, want %q", k, got, v)
				}
			}
			for _, k := range tc.absent {
				if _, ok := q[k]; ok {
					t.Errorf("param %s must not be sent: %q", k, q.Get(k))
				}
			}
		})
	}
}

func TestBrowseNeverUsesNewSortOrTrending(t *testing.T) {
	for _, cat := range []string{CatalogPopular, CatalogNew, CatalogTopRated} {
		p := browseParams(cat, "fantasy", 0, 2026)
		if p.sort == "new" || p.sort == "" {
			t.Errorf("%s sort = %q", cat, p.sort)
		}
	}
}

func TestBrowseMapsItemsAndEchoesRequest(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, docsJSON("A", 20, 20, 5000) })
	c, _ := newTestClient(f)
	res := browse(c, "  popular ", "Science  Fiction", 0)
	if res.Status != StatusOK || res.Catalog != "popular" || res.Genre != "Science Fiction" || res.Offset != 0 {
		t.Fatalf("status/echo: %+v", res)
	}
	if res.Partial || len(res.Items) != 20 {
		t.Fatalf("partial=%v items=%d", res.Partial, len(res.Items))
	}
	it := res.Items[0]
	if it.Key != "/works/A0" || it.Title != "A Book 0" || it.Author != "A Author" || it.Year != 1990 || it.Popularity != 5000 ||
		it.CoverURL != "https://covers.openlibrary.org/b/id/1000-M.jpg" {
		t.Errorf("item mapping: %+v", it)
	}
}

func TestBrowseNextOffsetPaging(t *testing.T) {
	for _, tc := range []struct {
		name            string
		offset, docs    int
		found           int
		wantNext        int // -1 = null
		wantStatus      string
		wantItemsAtMost int
	}{
		{"first page of many", 0, 20, 5000, 20, StatusOK, 20},
		{"middle page", 60, 20, 5000, 80, StatusOK, 20},
		{"page before the cap", 160, 20, 5000, 180, StatusOK, 20},
		{"last page at the cap", 180, 20, 5000, -1, StatusOK, 20},
		{"thin genre numFound 14", 0, 14, 14, -1, StatusOK, 14},
		{"exact multiple, last page", 20, 20, 40, -1, StatusOK, 20},
		{"one more than a page", 0, 20, 21, 20, StatusOK, 20},
		{"offset past the end", 20, 0, 14, -1, StatusEmpty, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOL(t, func(url.Values) (int, string) { return 200, docsJSON("P", tc.docs, tc.docs, tc.found) })
			c, _ := newTestClient(f)
			res := browse(c, CatalogPopular, "fantasy", tc.offset)
			if res.Status != tc.wantStatus {
				t.Fatalf("status %q, want %q: %+v", res.Status, tc.wantStatus, res)
			}
			switch {
			case tc.wantNext < 0 && res.NextOffset != nil:
				t.Errorf("next_offset %d, want null", *res.NextOffset)
			case tc.wantNext >= 0 && (res.NextOffset == nil || *res.NextOffset != tc.wantNext):
				t.Errorf("next_offset %v, want %d", res.NextOffset, tc.wantNext)
			}
			if len(res.Items) > tc.wantItemsAtMost {
				t.Errorf("%d items", len(res.Items))
			}
		})
	}
}

func TestBrowseNextOffsetIsNotDerivedFromVisibleItems(t *testing.T) {
	// Later page: 20 docs but only 3 carry covers; more results follow. The
	// page is short but paging must go on.
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, docsJSON("S", 20, 3, 5000) })
	c, _ := newTestClient(f)
	res := browse(c, CatalogNew, "fantasy", 20)
	if res.Status != StatusOK || len(res.Items) != 3 {
		t.Fatalf("got %+v", res)
	}
	if res.NextOffset == nil || *res.NextOffset != 40 {
		t.Errorf("next_offset %v, want 40", res.NextOffset)
	}

	// Later page with zero visible cards but more pages: ok, not empty/end.
	f2 := newFakeOL(t, func(url.Values) (int, string) { return 200, docsJSON("Z", 20, 0, 5000) })
	c2, _ := newTestClient(f2)
	res = browse(c2, CatalogTopRated, "fantasy", 40)
	if res.Status != StatusOK || len(res.Items) != 0 || res.NextOffset == nil || *res.NextOffset != 60 {
		t.Errorf("zero-card page: %+v", res)
	}
}

func TestBrowseCoverGuardIsPageZeroOnly(t *testing.T) {
	low := func(url.Values) (int, string) { return 200, docsJSON("L", 20, 5, 5000) }

	// new / top_rated page 0: untrusted sorted page -> upstream_unavailable, no unsorted retry.
	for _, cat := range []string{CatalogNew, CatalogTopRated} {
		f := newFakeOL(t, low)
		c, _ := newTestClient(f)
		res := browse(c, cat, "fantasy", 0)
		if res.Status != StatusUpstreamUnavailable || res.Error == "" || len(res.Items) != 0 || res.Partial || res.NextOffset != nil {
			t.Errorf("%s page 0: %+v", cat, res)
		}
		if f.hits() != 1 {
			t.Errorf("%s: %d OL requests, want 1 (no unsorted fallback)", cat, f.hits())
		}
	}
	// Later pages never fail on cover count, for every catalog.
	for _, cat := range []string{CatalogPopular, CatalogNew, CatalogTopRated} {
		f := newFakeOL(t, low)
		c, _ := newTestClient(f)
		res := browse(c, cat, "fantasy", 20)
		if res.Status != StatusOK || len(res.Items) != 5 || res.Partial {
			t.Errorf("%s page 1: %+v", cat, res)
		}
		if f.hits() != 1 {
			t.Errorf("%s page 1: %d OL requests, want 1", cat, f.hits())
		}
	}
	// A page that holds the whole (small) result set is trusted even with few covers.
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, docsJSON("W", 5, 2, 5) })
	c, _ := newTestClient(f)
	res := browse(c, CatalogNew, "cooking", 0)
	if res.Status != StatusOK || len(res.Items) != 2 || res.NextOffset != nil {
		t.Errorf("whole result set: %+v", res)
	}
}

func TestBrowsePopularPageZeroFallsBackUnsortedAsPartial(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("sort") != "" {
			return 200, docsJSON("B", 20, 2, 5000) // sorted page unusable
		}
		return 200, docsJSON("R", 20, 20, 5000)
	})
	c, clk := newTestClient(f)
	res := browse(c, CatalogPopular, "fantasy", 0)
	if res.Status != StatusOK || !res.Partial || res.NextOffset != nil || len(res.Items) != 20 {
		t.Fatalf("fallback: %+v", res)
	}
	if f.hits() != 2 || f.query(1).Get("sort") != "" || f.query(1).Get("q") != `subject_key:"fantasy"` || f.query(1).Get("subject") != "" {
		t.Errorf("hits=%d retry query=%v", f.hits(), f.query(1))
	}
	// A degraded page is cached only briefly so the sorted answer is retried.
	browse(c, CatalogPopular, "fantasy", 0)
	if f.hits() != 2 {
		t.Errorf("degraded page was not cached: %d hits", f.hits())
	}
	clk.Advance(failureTTL + time.Second)
	browse(c, CatalogPopular, "fantasy", 0)
	if f.hits() != 4 {
		t.Errorf("degraded page was cached too long: %d hits", f.hits())
	}
}

func TestBrowsePopularPageZeroFallsBackOnBadRequest(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("sort") != "" {
			return 400, `{"error":"bad sort"}`
		}
		return 200, docsJSON("R", 20, 20, 5000)
	})
	c, _ := newTestClient(f)
	res := browse(c, CatalogPopular, "", 0)
	if res.Status != StatusOK || !res.Partial || res.NextOffset != nil {
		t.Errorf("got %+v", res)
	}
	// new on a 400 has no fallback.
	f2 := newFakeOL(t, func(url.Values) (int, string) { return 400, `{}` })
	c2, _ := newTestClient(f2)
	if res := browse(c2, CatalogNew, "fantasy", 0); res.Status != StatusUpstreamUnavailable || f2.hits() != 1 {
		t.Errorf("new 400: %+v hits=%d", res, f2.hits())
	}
}

func TestBrowseInvalidRequests(t *testing.T) {
	f := newFakeOL(t, browseOK("X"))
	c, _ := newTestClient(f)
	for _, tc := range []struct {
		name, catalog, genre string
		offset               int
	}{
		{"unknown catalog", "trending", "fantasy", 0},
		{"for_you is client-only", "for_you", "", 0},
		{"empty catalog", "", "fantasy", 0},
		{"catalog is case sensitive", "Popular", "", 0},
		{"negative offset", CatalogPopular, "", -20},
		{"off-grid offset", CatalogPopular, "", 7},
		{"beyond the cap", CatalogPopular, "", 200},
		{"double quote in genre", CatalogNew, `fant"asy`, 0},
		{"backslash in genre", CatalogNew, `fant\asy`, 0},
		{"control char in genre", CatalogNew, "fant\x07asy", 0},
		{"genre without a usable token", CatalogNew, "audiobooks", 0},
		{"separator-only genre", CatalogNew, " & , ", 0},
	} {
		res := browse(c, tc.catalog, tc.genre, tc.offset)
		if res.Status != StatusInvalidRequest || res.Error == "" || len(res.Items) != 0 || res.NextOffset != nil {
			t.Errorf("%s: %+v", tc.name, res)
		}
		if res.Offset != tc.offset {
			t.Errorf("%s: offset echo %d", tc.name, res.Offset)
		}
	}
	if f.hits() != 0 {
		t.Errorf("invalid requests reached Open Library (%d)", f.hits())
	}
}

func TestBrowseNormalizesRawABSGenre(t *testing.T) {
	f := newFakeOL(t, browseOK("N"))
	c, _ := newTestClient(f)
	res := browse(c, CatalogPopular, "Science Fiction & Fantasy", 0)
	if res.Status != StatusOK || f.query(0).Get("q") != `subject_key:"science_fiction"` {
		t.Errorf("first token: status=%s q=%q", res.Status, f.query(0).Get("q"))
	}
	// Echo is the request, not the resolved subject, so the client can match it.
	if res.Genre != "Science Fiction & Fantasy" {
		t.Errorf("genre echo %q", res.Genre)
	}
	f2 := newFakeOL(t, browseOK("N"))
	c2, _ := newTestClient(f2)
	browse(c2, CatalogNew, "Sci-Fi", 0)
	if !strings.Contains(f2.query(0).Get("q"), `subject_key:"science_fiction"`) {
		t.Errorf("alias not applied: %q", f2.query(0).Get("q"))
	}
}

func TestSubjectKeySlug(t *testing.T) {
	for in, want := range map[string]string{
		"science fiction":    "science_fiction",
		"Science  Fiction":   "science_fiction",
		"children's":         "children's_fiction",
		"Children's":         "children's_fiction",
		"self-help":          "self-help",
		"historical fiction": "historical_fiction",
		"young adult":        "young_adult",
		"mystery, thriller":  "mystery_thriller",
		" _fantasy_ ":        "fantasy",
		"":                   "",
		"_ ,":                "",
	} {
		if got := subjectKey(in); got != want {
			t.Errorf("subjectKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBrowseFailureIsCachedAsFailureNotEmpty(t *testing.T) {
	var status = 500
	f := newFakeOL(t, func(url.Values) (int, string) {
		if status != 200 {
			return status, `{}`
		}
		return 200, docsJSON("F", 20, 20, 5000)
	})
	c, clk := newTestClient(f)
	res := browse(c, CatalogPopular, "fantasy", 20)
	if res.Status != StatusUpstreamUnavailable || len(res.Items) != 0 || res.NextOffset != nil {
		t.Fatalf("failure: %+v", res)
	}
	browse(c, CatalogPopular, "fantasy", 20)
	if f.hits() != 1 {
		t.Errorf("failure not cached within the failure TTL: %d hits", f.hits())
	}
	status = 200
	clk.Advance(failureTTL + time.Second)
	res = browse(c, CatalogPopular, "fantasy", 20)
	if res.Status != StatusOK || f.hits() != 2 {
		t.Errorf("failure outlived its short TTL: %+v hits=%d", res, f.hits())
	}
	// A success is then cached for the long TTL.
	clk.Advance(20 * time.Minute)
	browse(c, CatalogPopular, "fantasy", 20)
	if f.hits() != 2 {
		t.Errorf("success not cached: %d hits", f.hits())
	}
	clk.Advance(11 * time.Minute)
	browse(c, CatalogPopular, "fantasy", 20)
	if f.hits() != 3 {
		t.Errorf("success outlived the 30 minute TTL: %d hits", f.hits())
	}
}

func TestBrowseRateLimitedIsReported(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 429, `{}` })
	c, _ := newTestClient(f)
	res := browse(c, CatalogTopRated, "", 0)
	if res.Status != StatusRateLimited || res.NextOffset != nil || len(res.Items) != 0 {
		t.Errorf("got %+v", res)
	}
}

func TestBrowseCacheIsPerOffsetAndSharedAcrossGenreSpelling(t *testing.T) {
	f := newFakeOL(t, browseOK("C"))
	c, _ := newTestClient(f)
	browse(c, CatalogPopular, "fantasy", 0)
	browse(c, CatalogPopular, "fantasy", 20)
	browse(c, CatalogPopular, "fantasy", 40)
	if f.hits() != 3 {
		t.Fatalf("distinct offsets must be distinct cache entries: %d hits", f.hits())
	}
	browse(c, CatalogPopular, "fantasy", 20)
	browse(c, CatalogNew, "fantasy", 20)
	if f.hits() != 4 {
		t.Errorf("repeat should hit, other catalog should miss: %d hits", f.hits())
	}
	// Same subject spelled differently shares the entry but each caller gets its own echo.
	a := browse(c, CatalogPopular, "Fantasy", 20)
	b := browse(c, CatalogPopular, "fantasy", 20)
	if f.hits() != 4 {
		t.Errorf("genre spelling split the cache: %d hits", f.hits())
	}
	if a.Genre != "Fantasy" || b.Genre != "fantasy" {
		t.Errorf("echo leaked through the cache: %q / %q", a.Genre, b.Genre)
	}
	// Different genres miss.
	browse(c, CatalogPopular, "romance", 20)
	if f.hits() != 5 {
		t.Errorf("distinct genre must miss: %d hits", f.hits())
	}
}

func TestBrowseNewCatalogCacheSplitsAtNewYear(t *testing.T) {
	f := newFakeOL(t, browseOK("Y"))
	c, clk := newTestClient(f)
	clk.mu.Lock()
	clk.t = time.Date(2026, 12, 31, 23, 50, 0, 0, time.UTC)
	clk.mu.Unlock()

	browse(c, CatalogNew, "fantasy", 0)
	if got := f.query(0).Get("q"); !strings.Contains(got, "first_publish_year:[2025 TO 2026]") {
		t.Fatalf("pre-new-year window: %q", got)
	}
	clk.Advance(20 * time.Minute) // 2027-01-01 00:10, well inside the 30 minute TTL
	browse(c, CatalogNew, "fantasy", 0)
	if f.hits() != 2 {
		t.Fatalf("year window must key the cache: %d hits", f.hits())
	}
	if got := f.query(1).Get("q"); !strings.Contains(got, "first_publish_year:[2026 TO 2027]") {
		t.Errorf("post-new-year window: %q", got)
	}
	// popular has no year window: it stays cached across New Year.
	clk.mu.Lock()
	clk.t = time.Date(2026, 12, 31, 23, 50, 0, 0, time.UTC)
	clk.mu.Unlock()
	browse(c, CatalogPopular, "fantasy", 0)
	clk.Advance(20 * time.Minute)
	browse(c, CatalogPopular, "fantasy", 0)
	if f.hits() != 3 {
		t.Errorf("popular should not split at New Year: %d hits", f.hits())
	}
}

func TestBrowseCoalescesIdenticalRequests(t *testing.T) {
	f := newFakeOL(t, browseOK("Q"))
	c, _ := newTestClient(f)
	done := make(chan BrowseResult, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- browse(c, CatalogPopular, "horror", 0) }()
	}
	for i := 0; i < 4; i++ {
		if r := <-done; r.Status != StatusOK {
			t.Errorf("got %+v", r)
		}
	}
	if f.hits() != 1 {
		t.Errorf("%d Open Library requests for identical browse calls, want 1", f.hits())
	}
}

func TestBrowseResultJSONShape(t *testing.T) {
	out, _ := json.Marshal(browseFailure(StatusInvalidRequest, "x"))
	for _, want := range []string{`"items":[]`, `"next_offset":null`, `"partial":false`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(string(out), "resolved_subject") || strings.Contains(string(out), `"subject"`) {
		t.Errorf("dropped fields are back: %s", out)
	}
}

func TestOptionsArePinned(t *testing.T) {
	o := Options()
	if o.Status != StatusOK || o.Error != "" {
		t.Fatalf("options: %+v", o)
	}
	wantCats := []Option{{"popular", "Popular"}, {"new", "New"}, {"top_rated", "Top rated"}}
	if fmt.Sprint(o.Catalogs) != fmt.Sprint(wantCats) {
		t.Errorf("catalogs %v, want %v", o.Catalogs, wantCats)
	}
	if len(o.Genres) != 25 {
		t.Errorf("%d genres, want the 25 curated by the P0 probe", len(o.Genres))
	}
	for name, list := range map[string][]Option{"catalogs": o.Catalogs, "genres": o.Genres} {
		seen := map[string]bool{}
		for _, it := range list {
			if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Label) == "" {
				t.Errorf("%s: blank id/label %+v", name, it)
			}
			if seen[it.ID] {
				t.Errorf("%s: duplicate id %q", name, it.ID)
			}
			seen[it.ID] = true
		}
	}
	for _, it := range o.Catalogs {
		if it.ID == "for_you" {
			t.Error("server must never advertise for_you")
		}
		if !validCatalog(it.ID) {
			t.Errorf("advertised catalog %q is not browsable", it.ID)
		}
	}
	for _, g := range o.Genres {
		if g.ID == "true crime" {
			t.Error("true crime was dropped by the P0 probe")
		}
		// Every advertised genre must be accepted by Browse as itself.
		if toks := GenreSubjects(g.ID); len(toks) != 1 || toks[0] != g.ID || hasUnsafeRune(g.ID) {
			t.Errorf("genre id %q resolves to %v", g.ID, toks)
		}
	}
}

func TestOptionsReturnsCopies(t *testing.T) {
	o := Options()
	o.Catalogs[0].Label = "mutated"
	o.Genres[0].Label = "mutated"
	if Options().Catalogs[0].Label != "Popular" || CuratedGenres()[0].Label != "Biography" {
		t.Error("Options exposes shared slices")
	}
}

func TestEveryAdvertisedGenreBrowsesInEveryCatalog(t *testing.T) {
	f := newFakeOL(t, browseOK("G"))
	c, _ := newTestClient(f)
	for _, cat := range Options().Catalogs {
		for _, g := range Options().Genres {
			if res := browse(c, cat.ID, g.ID, 0); res.Status != StatusOK {
				t.Fatalf("%s/%s: %+v", cat.ID, g.ID, res)
			}
		}
	}
}

func decodeFixture(t *testing.T, name string, v any) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s has fields the Go wire type does not: %v", name, err)
	}
	return raw
}

func sameJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(out, &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("fixture and marshalled value differ:\n%v\n%v", a, b)
	}
}

func TestBrowseWireFixtureMatchesBrowseResultShape(t *testing.T) {
	var res BrowseResult
	raw := decodeFixture(t, "browse_ok.json", &res)
	if res.Status != StatusOK || res.Catalog != CatalogPopular || res.Genre != "science fiction" || res.Offset != 0 ||
		res.NextOffset == nil || *res.NextOffset != 20 || res.Partial || len(res.Items) != 2 {
		t.Errorf("fixture decoded to %+v", res)
	}
	sameJSON(t, raw, res)
}

func TestOptionsWireFixtureMatchesOptionsResultShape(t *testing.T) {
	var res OptionsResult
	raw := decodeFixture(t, "options_ok.json", &res)
	if res.Status != StatusOK || len(res.Catalogs) != 3 || len(res.Genres) == 0 {
		t.Errorf("fixture decoded to %+v", res)
	}
	sameJSON(t, raw, res)
	// Every id in the fixture is a real option, so the shared fixture cannot drift.
	live := map[string]bool{}
	for _, g := range Options().Genres {
		live[g.ID] = true
	}
	for _, g := range res.Genres {
		if !live[g.ID] {
			t.Errorf("fixture genre %q is not a live option", g.ID)
		}
	}
}
