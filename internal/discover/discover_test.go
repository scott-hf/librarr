package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOL is an Open Library stand-in. handler receives the parsed query and
// returns the HTTP status and body; every request is recorded.
type fakeOL struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queries []url.Values
	uas     []string
	handler func(q url.Values) (int, string)
	// retryAfter, when set, is sent as the Retry-After header.
	retryAfter string
}

func newFakeOL(t *testing.T, handler func(q url.Values) (int, string)) *fakeOL {
	t.Helper()
	f := &fakeOL{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search.json" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		f.queries = append(f.queries, q)
		f.uas = append(f.uas, r.Header.Get("User-Agent"))
		f.mu.Unlock()
		status, body := f.handler(q)
		w.Header().Set("Content-Type", "application/json")
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOL) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeOL) query(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[i]
}

// newTestClient wires a Client to the fake with a fast limiter and a
// controllable clock.
func newTestClient(f *fakeOL) (*Client, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	c := New(f.srv.Client(), "ops@example.org")
	c.SetBaseURL(f.srv.URL)
	c.now = clk.Now
	c.lim = newLimiter(time.Microsecond, time.Hour, clk.Now)
	return c, clk
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// docsJSON builds an OL response with n docs. withCover docs carry cover_i;
// numFound is reported as found.
func docsJSON(prefix string, n, withCover, found int) string {
	var docs []string
	for i := 0; i < n; i++ {
		cover := 0
		if i < withCover {
			cover = 1000 + i
		}
		docs = append(docs, fmt.Sprintf(
			`{"key":"/works/%s%d","title":"%s Book %d","author_name":["%s Author","Second Author"],"first_publish_year":%d,"cover_i":%d,"readinglog_count":%d}`,
			prefix, i, prefix, i, prefix, 1990+i, cover, 5000-i))
	}
	return fmt.Sprintf(`{"numFound":%d,"docs":[%s]}`, found, strings.Join(docs, ","))
}

func okHandler(prefix string) func(url.Values) (int, string) {
	return func(url.Values) (int, string) { return 200, docsJSON(prefix, 20, 20, 5000) }
}

func TestNormalizeInput(t *testing.T) {
	if got := NormalizeInput("  Science \t Fiction \n"); got != "Science Fiction" {
		t.Errorf("collapse: %q", got)
	}
	long := strings.Repeat("é", 500)
	got := NormalizeInput(long)
	if n := len([]rune(got)); n != MaxInputRunes {
		t.Errorf("cap: got %d runes, want %d", n, MaxInputRunes)
	}
}

func TestGenreSubjects(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Science Fiction & Fantasy", "science fiction|fantasy"},
		{"Sci-Fi & Fantasy", "science fiction|fantasy"},
		{"Teen & Young Adult", "young adult"},
		{"Mystery, Thriller & Suspense", "mystery|thriller|suspense"},
		{"Literature & Fiction", "literature"},
		{"Fiction", "fiction"},
		{"Audiobook", ""},
		{"Self-Help", "self-help"},
		{"Fantasy / fantasy", "fantasy"},
	}
	for _, tc := range cases {
		if got := strings.Join(GenreSubjects(tc.in), "|"); got != tc.want {
			t.Errorf("GenreSubjects(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPopularMapsItemsAndDedupes(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) {
		// 10 good docs, one duplicate key, one duplicate title (different key),
		// one without cover.
		body := `{"numFound":9000,"docs":[`
		var docs []string
		for i := 0; i < 10; i++ {
			docs = append(docs, fmt.Sprintf(`{"key":"/works/OL%dW","title":"Title %d","author_name":["Auth %d"],"first_publish_year":2000,"cover_i":%d,"readinglog_count":%d}`, i, i, i, 100+i, 900-i))
		}
		docs = append(docs, `{"key":"/works/OL0W","title":"Title 0 again","cover_i":5}`)
		docs = append(docs, `{"key":"/works/OL99W","title":"title 1!","cover_i":6}`)
		docs = append(docs, `{"key":"/works/OL98W","title":"No cover","author_name":["x"]}`)
		return 200, body + strings.Join(docs, ",") + "]}"
	})
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), " Science  Fiction ", 12)
	if res.Status != StatusOK || res.Error != "" {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	if res.ResolvedSubject != "science fiction" {
		t.Errorf("resolved %q", res.ResolvedSubject)
	}
	if len(res.Items) != 10 {
		t.Fatalf("items = %d, want 10 (dupes and coverless dropped)", len(res.Items))
	}
	first := res.Items[0]
	if first.Key != "/works/OL0W" || first.Title != "Title 0" || first.Author != "Auth 0" ||
		first.Year != 2000 || first.Popularity != 900 ||
		first.CoverURL != "https://covers.openlibrary.org/b/id/100-M.jpg" {
		t.Errorf("first item = %+v", first)
	}
	q := f.query(0)
	if q.Get("subject") != "science fiction" || q.Get("sort") != "readinglog" || q.Get("language") != "eng" || q.Get("limit") != "20" {
		t.Errorf("OL query = %v", q)
	}
	if ua := f.uas[0]; !strings.Contains(ua, "Librarr/") || !strings.Contains(ua, "ops@example.org") {
		t.Errorf("User-Agent lacks product/contact: %q", ua)
	}
}

func TestPopularLimitSlicesCachedList(t *testing.T) {
	f := newFakeOL(t, okHandler("A"))
	c, _ := newTestClient(f)
	if n := len(c.Popular(context.Background(), "Fantasy", 3).Items); n != 3 {
		t.Errorf("limit 3 -> %d items", n)
	}
	if n := len(c.Popular(context.Background(), "Fantasy", 999).Items); n != MaxLimit {
		t.Errorf("limit 999 -> %d items, want %d", n, MaxLimit)
	}
	if f.hits() != 1 {
		t.Errorf("hits = %d, want 1 (limit must not change the cache key)", f.hits())
	}
}

func TestPopularCacheHitAndAliasMerge(t *testing.T) {
	f := newFakeOL(t, okHandler("A"))
	c, _ := newTestClient(f)
	a := c.Popular(context.Background(), "Sci-Fi", 20)
	b := c.Popular(context.Background(), "  science   fiction", 20)
	if a.ResolvedSubject != "science fiction" || b.ResolvedSubject != a.ResolvedSubject {
		t.Errorf("resolved subjects differ: %q vs %q", a.ResolvedSubject, b.ResolvedSubject)
	}
	if f.hits() != 1 {
		t.Errorf("hits = %d, want 1 (alias + whitespace variants share one cache entry)", f.hits())
	}
}

func TestPopularCacheExpires(t *testing.T) {
	f := newFakeOL(t, okHandler("A"))
	c, clk := newTestClient(f)
	c.Popular(context.Background(), "Fantasy", 5)
	clk.Advance(okTTL + time.Second)
	c.Popular(context.Background(), "Fantasy", 5)
	if f.hits() != 2 {
		t.Errorf("hits = %d, want 2 after TTL", f.hits())
	}
}

func TestPopularTriesNextSubjectWhenFirstIsThin(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("subject") == "mystery" {
			return 200, docsJSON("M", 3, 3, 3) // complete but thin
		}
		return 200, docsJSON("T", 20, 20, 5000)
	})
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), "Mystery, Thriller & Suspense", 20)
	if res.Status != StatusOK || res.ResolvedSubject != "thriller" || len(res.Items) != 20 {
		t.Errorf("got status=%s subject=%q items=%d", res.Status, res.ResolvedSubject, len(res.Items))
	}
	if f.hits() != 2 {
		t.Errorf("hits = %d, want 2", f.hits())
	}
}

func TestPopularEmptyAndInvalid(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, `{"numFound":0,"docs":[]}` })
	c, _ := newTestClient(f)
	if res := c.Popular(context.Background(), "xyzzy nonsense", 20); res.Status != StatusEmpty || res.Items == nil || len(res.Items) != 0 {
		t.Errorf("nonsense genre: %+v", res)
	}
	if f.hits() != 1 {
		t.Errorf("a complete empty set must not trigger the sort-guard retry, hits=%d", f.hits())
	}
	if res := c.Popular(context.Background(), "   ", 20); res.Status != StatusInvalidRequest || res.Error == "" || len(res.Items) != 0 || res.Items == nil {
		t.Errorf("blank genre: %+v", res)
	}
	if res := c.Popular(context.Background(), "Audiobook", 20); res.Status != StatusEmpty {
		t.Errorf("noise-only genre: %+v", res)
	}
}

func TestSortGuardRetriesWithoutSortOn400(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("sort") != "" {
			return 400, `{"error":"bad sort"}`
		}
		return 200, docsJSON("R", 20, 20, 5000)
	})
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), "Fantasy", 20)
	if res.Status != StatusOK || len(res.Items) != 20 {
		t.Fatalf("got %s with %d items", res.Status, len(res.Items))
	}
	if f.hits() != 2 || f.query(0).Get("sort") != "readinglog" || f.query(1).Has("sort") {
		t.Errorf("expected one sorted then one unsorted request, got %v", f.queries)
	}
}

func TestSortGuardRetriesOnLowQuality(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("sort") != "" {
			return 200, docsJSON("Bad", 20, 3, 5000) // sort ignored: few covers of a big set
		}
		return 200, docsJSON("Good", 20, 20, 5000)
	})
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), "Fantasy", 20)
	if res.Status != StatusOK || len(res.Items) != 20 || !strings.HasPrefix(res.Items[0].Title, "Good") {
		t.Fatalf("got %s %d items first=%v", res.Status, len(res.Items), res.Items[:1])
	}
	if f.hits() != 2 || f.query(1).Has("sort") {
		t.Errorf("expected sorted then unsorted request, got %v", f.queries)
	}
}

func TestSortGuardBothFailIsUpstreamUnavailable(t *testing.T) {
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Get("sort") != "" {
			return 400, `{}`
		}
		return 500, `oops`
	})
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), "Fantasy", 20)
	if res.Status != StatusUpstreamUnavailable || res.Error == "" || len(res.Items) != 0 {
		t.Errorf("got %+v", res)
	}
	if f.hits() != 2 {
		t.Errorf("hits = %d, want 2 (one retry only)", f.hits())
	}
}

func TestFailureIsCachedAsFailureNotEmpty(t *testing.T) {
	var healthy atomic.Bool
	f := newFakeOL(t, func(url.Values) (int, string) {
		if healthy.Load() {
			return 200, docsJSON("H", 20, 20, 5000)
		}
		return 503, `down`
	})
	c, clk := newTestClient(f)

	first := c.Popular(context.Background(), "Fantasy", 20)
	if first.Status != StatusUpstreamUnavailable {
		t.Fatalf("first = %s", first.Status)
	}
	// Immediately again: served from the failure cache, still a failure
	// (never an empty success), and Open Library is not hit again.
	second := c.Popular(context.Background(), "Fantasy", 20)
	if second.Status != StatusUpstreamUnavailable {
		t.Errorf("cached failure reported as %q", second.Status)
	}
	if f.hits() != 1 {
		t.Errorf("hits = %d, want 1 while the failure is cached", f.hits())
	}
	// After the short failure TTL the request is retried.
	healthy.Store(true)
	clk.Advance(failureTTL + time.Second)
	third := c.Popular(context.Background(), "Fantasy", 20)
	if third.Status != StatusOK || len(third.Items) == 0 {
		t.Errorf("after recovery: %+v", third)
	}
}

func TestMalformedBodyIsUpstreamUnavailable(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, `<html>` })
	c, _ := newTestClient(f)
	if res := c.Popular(context.Background(), "Fantasy", 20); res.Status != StatusUpstreamUnavailable {
		t.Errorf("got %+v", res)
	}
}

func TestRateLimitedBacksOff(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 429, `slow down` })
	c, clk := newTestClient(f)
	res := c.Popular(context.Background(), "Fantasy", 20)
	if res.Status != StatusRateLimited {
		t.Fatalf("got %+v", res)
	}
	// A different request during the backoff never reaches Open Library.
	res = c.Popular(context.Background(), "Romance", 20)
	if res.Status != StatusRateLimited || f.hits() != 1 {
		t.Errorf("during backoff: status=%s hits=%d", res.Status, f.hits())
	}
	// After the backoff (and the cached failure) a request goes out again.
	clk.Advance(2*time.Minute + time.Second)
	c.Popular(context.Background(), "Romance", 20)
	if f.hits() != 2 {
		t.Errorf("after backoff hits = %d, want 2", f.hits())
	}
}

func TestRetryAfterHeaderSetsBackoff(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 429, `` })
	f.retryAfter = "40"
	c, clk := newTestClient(f)
	c.Popular(context.Background(), "Fantasy", 20)
	// 35 s later the default 2 s backoff would be over, but Retry-After asked for 40 s.
	clk.Advance(35 * time.Second)
	if res := c.Popular(context.Background(), "Romance", 20); res.Status != StatusRateLimited || f.hits() != 1 {
		t.Errorf("inside Retry-After: status=%s hits=%d", res.Status, f.hits())
	}
	clk.Advance(6 * time.Second)
	c.Popular(context.Background(), "Horror", 20)
	if f.hits() != 2 {
		t.Errorf("after Retry-After hits = %d, want 2", f.hits())
	}
}

func TestSaturatedLimiterRefusesInsteadOfQueueing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	l := newLimiter(time.Second, 2*time.Second, clk.Now)
	if err := l.wait(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Slots are now 1 s and 2 s out; the third would wait 3 s > maxWait.
	go func() { _ = l.wait(context.Background()) }()
	go func() { _ = l.wait(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		l.mu.Lock()
		reserved := l.next
		l.mu.Unlock()
		if reserved.Sub(clk.Now()) >= 3*time.Second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slots were not reserved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := l.wait(context.Background()); err != errRateLimited {
		t.Errorf("saturated limiter returned %v, want errRateLimited", err)
	}
}

func TestIdenticalInFlightRequestsAreCoalesced(t *testing.T) {
	c := New(nil, "")
	const callers = 8
	var computes atomic.Int32
	release := make(chan struct{})
	compute := func(context.Context) (Result, time.Duration) {
		computes.Add(1)
		<-release
		// ttl 0: nothing is cached, so only coalescing can explain one compute.
		return Result{Status: StatusOK, Items: []Item{{Key: "k", Title: "t"}}}, 0
	}

	var wg sync.WaitGroup
	results := make([]Result, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.do(context.Background(), "same-key", compute)
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		w := 0
		if cl := c.inflight["same-key"]; cl != nil {
			w = cl.waiters
		}
		c.mu.Unlock()
		if w == callers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d callers joined the in-flight request", w, callers)
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if n := computes.Load(); n != 1 {
		t.Errorf("computes = %d, want 1", n)
	}
	for i, r := range results {
		if r.Status != StatusOK || len(r.Items) != 1 {
			t.Errorf("caller %d got %+v", i, r)
		}
	}
	c.mu.Lock()
	left := len(c.inflight)
	c.mu.Unlock()
	if left != 0 {
		t.Errorf("inflight not cleaned up: %d", left)
	}
}

func TestCancelledCallerDoesNotFailTheFetch(t *testing.T) {
	c := New(nil, "")
	release := make(chan struct{})
	compute := func(context.Context) (Result, time.Duration) {
		<-release
		return Result{Status: StatusOK, Items: []Item{{Key: "k", Title: "t"}}}, okTTL
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- c.do(ctx, "k", compute) }()
	for {
		c.mu.Lock()
		cl := c.inflight["k"]
		ready := cl != nil && cl.waiters == 1
		c.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if r := <-done; r.Status != StatusUpstreamUnavailable {
		t.Errorf("cancelled caller got %+v", r)
	}
	close(release)
	// The fetch finishes and is cached for the next caller.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := c.do(context.Background(), "k", func(context.Context) (Result, time.Duration) {
			return failure(StatusUpstreamUnavailable, "should have been cached"), 0
		})
		if r.Status == StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("result never cached, last %+v", r)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestInputIsCappedBeforeReachingOpenLibrary(t *testing.T) {
	f := newFakeOL(t, okHandler("A"))
	c, _ := newTestClient(f)
	c.Popular(context.Background(), strings.Repeat("x", 5000), 20)
	if got := len([]rune(f.query(0).Get("subject"))); got != MaxInputRunes {
		t.Errorf("subject length %d, want %d", got, MaxInputRunes)
	}
}

func TestCacheIsBounded(t *testing.T) {
	c := New(nil, "")
	for i := 0; i < maxCacheSize+50; i++ {
		c.mu.Lock()
		c.store(fmt.Sprintf("k%d", i), Result{Status: StatusOK}, time.Duration(i+1)*time.Minute)
		c.mu.Unlock()
	}
	if len(c.cache) > maxCacheSize {
		t.Errorf("cache size %d exceeds %d", len(c.cache), maxCacheSize)
	}
}

// similarHandler serves author searches from authorDocs and subject searches
// from subjectDocs.
func similarHandler(authorDocs, subjectDocs string) func(url.Values) (int, string) {
	return func(q url.Values) (int, string) {
		if q.Has("author") {
			return 200, authorDocs
		}
		return 200, subjectDocs
	}
}

func TestSimilarExcludesSeedAndSkipsTopUpWhenEnough(t *testing.T) {
	// 10 author docs including the seed (with a parenthetical edition suffix).
	var docs []string
	docs = append(docs, `{"key":"/works/SEED","title":"The Hobbit (Illustrated Edition)","author_name":["J. R. R. Tolkien"],"cover_i":1}`)
	for i := 0; i < 9; i++ {
		docs = append(docs, fmt.Sprintf(`{"key":"/works/OL%dW","title":"Tolkien %d","author_name":["J. R. R. Tolkien"],"cover_i":%d,"readinglog_count":%d}`, i, i, 10+i, 100-i))
	}
	f := newFakeOL(t, similarHandler(`{"numFound":300,"docs":[`+strings.Join(docs, ",")+`]}`, docsJSON("G", 20, 20, 5000)))
	c, _ := newTestClient(f)

	res := c.Similar(context.Background(), "The Hobbit", "J. R. R. Tolkien", "Fantasy", 20)
	if res.Status != StatusOK || len(res.Items) != 9 {
		t.Fatalf("got %s with %d items", res.Status, len(res.Items))
	}
	for _, it := range res.Items {
		if it.Key == "/works/SEED" {
			t.Error("seed not excluded")
		}
	}
	if res.ResolvedSubject != "" {
		t.Errorf("resolved subject %q set without a genre top-up", res.ResolvedSubject)
	}
	if f.hits() != 1 || f.query(0).Get("author") != "J. R. R. Tolkien" || f.query(0).Get("sort") != "readinglog" {
		t.Errorf("expected one sorted author query, got %v", f.queries)
	}
}

func TestSimilarTopsUpFromGenreWhenAuthorIsThin(t *testing.T) {
	author := `{"numFound":3,"docs":[` +
		`{"key":"/works/SEED","title":"Gone Girl","author_name":["Gillian Flynn"],"cover_i":1},` +
		`{"key":"/works/A1","title":"Sharp Objects","author_name":["Gillian Flynn"],"cover_i":2},` +
		`{"key":"/works/A2","title":"Dark Places","author_name":["Gillian Flynn"],"cover_i":3}]}`
	subject := `{"numFound":5000,"docs":[` +
		`{"key":"/works/A1","title":"Sharp Objects","cover_i":2},` + // already present by key and title
		`{"key":"/works/SEED2","title":"gone girl!","cover_i":4},` + // seed under another key
		strings.TrimPrefix(strings.TrimSuffix(docsJSON("S", 18, 18, 5000), "]}"), `{"numFound":5000,"docs":[`) + `]}`
	f := newFakeOL(t, similarHandler(author, subject))
	c, _ := newTestClient(f)

	res := c.Similar(context.Background(), "Gone Girl", "Gillian Flynn", "Mystery, Thriller & Suspense", 20)
	if res.Status != StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if res.ResolvedSubject != "mystery" {
		t.Errorf("resolved subject %q, want mystery", res.ResolvedSubject)
	}
	if len(res.Items) != 20 {
		t.Errorf("items = %d, want 2 author + 18 genre = 20", len(res.Items))
	}
	titles := map[string]int{}
	for _, it := range res.Items {
		titles[strings.ToLower(it.Title)]++
	}
	if titles["gone girl"] != 0 || titles["gone girl!"] != 0 {
		t.Error("seed leaked through the genre top-up")
	}
	if titles["sharp objects"] != 1 {
		t.Errorf("sharp objects appears %d times", titles["sharp objects"])
	}
	if f.hits() != 2 || f.query(1).Get("subject") != "mystery" {
		t.Errorf("expected author then subject query, got %v", f.queries)
	}
}

func TestSimilarWithoutAuthorUsesGenreOnly(t *testing.T) {
	f := newFakeOL(t, okHandler("G"))
	c, _ := newTestClient(f)
	res := c.Similar(context.Background(), "Some Book", "", "Fantasy", 5)
	if res.Status != StatusOK || len(res.Items) != 5 || f.hits() != 1 || !f.query(0).Has("subject") {
		t.Errorf("status=%s items=%d hits=%d", res.Status, len(res.Items), f.hits())
	}
}

func TestSimilarInvalidInputs(t *testing.T) {
	f := newFakeOL(t, okHandler("G"))
	c, _ := newTestClient(f)
	if r := c.Similar(context.Background(), "", "A", "g", 20); r.Status != StatusInvalidRequest || r.Items == nil {
		t.Errorf("blank title: %+v", r)
	}
	if r := c.Similar(context.Background(), "Title", "", "", 20); r.Status != StatusInvalidRequest {
		t.Errorf("no author and no genre: %+v", r)
	}
	if f.hits() != 0 {
		t.Errorf("invalid requests reached Open Library: %d", f.hits())
	}
}

func TestSimilarAuthorFailureWithoutGenreIsFailure(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 500, `` })
	c, _ := newTestClient(f)
	if r := c.Similar(context.Background(), "T", "Author", "", 20); r.Status != StatusUpstreamUnavailable {
		t.Errorf("got %+v", r)
	}
}

func TestSimilarPartialAnswerIsCachedBriefly(t *testing.T) {
	var genreDown atomic.Bool
	genreDown.Store(true)
	author := docsJSON("Au", 3, 3, 3)
	f := newFakeOL(t, func(q url.Values) (int, string) {
		if q.Has("author") {
			return 200, author
		}
		if genreDown.Load() {
			return 500, ``
		}
		return 200, docsJSON("Ge", 20, 20, 5000)
	})
	c, clk := newTestClient(f)
	first := c.Similar(context.Background(), "T", "Author", "Fantasy", 20)
	if first.Status != StatusOK || len(first.Items) != 3 {
		t.Fatalf("degraded answer: %+v", first)
	}
	genreDown.Store(false)
	clk.Advance(failureTTL + time.Second) // well inside okTTL
	second := c.Similar(context.Background(), "T", "Author", "Fantasy", 20)
	if len(second.Items) <= 3 {
		t.Errorf("partial answer was cached for the long TTL: %d items", len(second.Items))
	}
}

func TestWireFixtureMatchesResultShape(t *testing.T) {
	raw, err := os.ReadFile("testdata/popular_ok.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var res Result
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("fixture has fields the Go wire type does not: %v", err)
	}
	if res.Status != StatusOK || res.ResolvedSubject != "science fiction" || len(res.Items) != 2 {
		t.Errorf("fixture decoded to %+v", res)
	}
	// Round trip: marshalling the Go type must reproduce the same field set.
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(out, &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("fixture and marshalled Result differ:\n%v\n%v", a, b)
	}
}

func TestResultJSONNeverHasNullItems(t *testing.T) {
	out, _ := json.Marshal(failure(StatusInvalidRequest, "x"))
	if !strings.Contains(string(out), `"items":[]`) {
		t.Errorf("items must be [] not null: %s", out)
	}
}
