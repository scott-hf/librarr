package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/sources/sourcestest"
	"github.com/PuerkitoBio/goquery"
)

type abbRoundTripper struct {
	body string
	req  *http.Request
}

func (r *abbRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.req = req.Clone(req.Context())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func TestAudioBookBaySearchSetsBrowserHeaders(t *testing.T) {
	rt := &abbRoundTripper{
		body: `<html><body>
		<div class="post">
			<div class="postTitle"><h2><a href="/abss/the-martian-andy-weir/">The Martian - Andy Weir</a></h2></div>
			<div class="postInfo">Language: English <span style="margin-left:100px;">Keywords:</div>
		</div>
		</body></html>`,
	}
	reg, err := sourcestest.Registry()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	cfg := &config.Config{
		UserAgent: "test",
		Sources:   reg,
	}
	cfg.Sources.AudioBookBay.Mirrors = []string{"audiobookbay.lu"}
	a := &AudioBookBay{cfg: cfg, client: &http.Client{Transport: rt}}

	results, err := a.searchDomain(context.Background(), "audiobookbay.lu", "The Martian")
	if err != nil {
		t.Fatalf("searchDomain returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if got := rt.req.Header.Get("User-Agent"); got != abbBrowserUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, abbBrowserUserAgent)
	}
	if got := rt.req.Header.Get("Accept-Language"); got != "en-US,en;q=0.9" {
		t.Fatalf("Accept-Language = %q, want en-US,en;q=0.9", got)
	}
	if got := rt.req.Header.Get("Upgrade-Insecure-Requests"); got != "1" {
		t.Fatalf("Upgrade-Insecure-Requests = %q, want 1", got)
	}
}

// AudioBookBay 301s any search term that begins with an uppercase letter to its
// home page, which the client follows silently into an empty-looking result.
// The stub in the other tests returns the same HTML whatever was asked, so this
// test captures the outgoing request and asserts on what `s=` actually held.
func TestAudioBookBaySearchSendsTrimmedLowercaseTerm(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"mixed case with padding", "  ToLkIeN  ", "tolkien"},
		{"leading uppercase", "Dune", "dune"},
		{"already lowercase", "dune", "dune"},
		{"tabs and newlines", "\tThe Martian\n", "the martian"},
		{"inner spacing preserved", "The  Martian", "the  martian"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &abbRoundTripper{body: `<html><body></body></html>`}
			reg, err := sourcestest.Registry()
			if err != nil {
				t.Fatalf("load registry: %v", err)
			}
			cfg := &config.Config{UserAgent: "test", Sources: reg}
			cfg.Sources.AudioBookBay.Mirrors = []string{"audiobookbay.lu"}
			a := &AudioBookBay{cfg: cfg, client: &http.Client{Transport: rt}}

			if _, err := a.searchDomain(context.Background(), "audiobookbay.lu", tc.query); err != nil {
				t.Fatalf("searchDomain returned error: %v", err)
			}
			if rt.req == nil {
				t.Fatal("no request was captured")
			}
			params := rt.req.URL.Query()
			if got := params.Get("s"); got != tc.want {
				t.Fatalf("s = %q, want %q", got, tc.want)
			}
			if got := params.Get("tt"); got != "1" {
				t.Fatalf("tt = %q, want 1", got)
			}
		})
	}
}

// Live AudioBookBay markup puts the keyword span immediately after the
// language with no separating whitespace, so .postInfo text reads
// "Language: EnglishKeywords: ...". The language gate must still accept it.
func TestAudioBookBaySearchAcceptsLanguageRunTogetherWithKeywords(t *testing.T) {
	cases := []struct {
		name     string
		postInfo string
		want     int
	}{
		{
			name:     "english run together with keywords",
			postInfo: `Category: Horror&nbsp; <br />Language: English<span style="margin-left:100px;">Keywords: Classic&nbsp; Gothic&nbsp; </span><br />`,
			want:     1,
		},
		{
			name:     "english with separating space",
			postInfo: `Language: English <span style="margin-left:100px;">Keywords:</span>`,
			want:     1,
		},
		{
			name:     "non-english run together with keywords is still rejected",
			postInfo: `Language: German<span style="margin-left:100px;">Keywords: Klassiker&nbsp; </span>`,
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &abbRoundTripper{
				body: `<html><body>
				<div class="post">
					<div class="postTitle"><h2><a href="/abss/dracula-bram-stoker/">Dracula - Bram Stoker</a></h2></div>
					<div class="postInfo">` + tc.postInfo + `</div>
				</div>
				</body></html>`,
			}
			reg, err := sourcestest.Registry()
			if err != nil {
				t.Fatalf("load registry: %v", err)
			}
			cfg := &config.Config{UserAgent: "test", Sources: reg}
			cfg.Sources.AudioBookBay.Mirrors = []string{"audiobookbay.lu"}
			a := &AudioBookBay{cfg: cfg, client: &http.Client{Transport: rt}}

			results, err := a.searchDomain(context.Background(), "audiobookbay.lu", "Dracula")
			if err != nil {
				t.Fatalf("searchDomain returned error: %v", err)
			}
			if len(results) != tc.want {
				t.Fatalf("got %d results, want %d", len(results), tc.want)
			}
		})
	}
}

// abbRoundTripFunc lets a test script per-request behaviour (by host / query).
type abbRoundTripFunc func(*http.Request) *http.Response

func (f abbRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	resp := f(req)
	resp.Request = req
	return resp, nil
}

func abbStubResponse(status int, location, body string) *http.Response {
	h := make(http.Header)
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func abbPostPage(title string) string {
	return `<html><body>
	<div class="post">
		<div class="postTitle"><h2><a href="/abss/` + strings.ToLower(strings.ReplaceAll(title, " ", "-")) + `/">` + title + `</a></h2></div>
		<div class="postInfo">Language: English <span style="margin-left:100px;">Keywords:</span></div>
	</div>
	</body></html>`
}

// newRedirectTestABB builds the searcher through NewAudioBookBay (not a struct
// literal): the redirect policy lives in the constructor, so a literal would
// bypass exactly the code under test.
func newRedirectTestABB(t *testing.T, shared *http.Client, mirrors ...string) *AudioBookBay {
	t.Helper()
	reg, err := sourcestest.Registry()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	cfg := &config.Config{UserAgent: "test", Sources: reg}
	cfg.Sources.AudioBookBay.Mirrors = mirrors
	return NewAudioBookBay(cfg, shared)
}

// AudioBookBay answers a search it dislikes with a 3xx to the home page. If the
// client follows it, the home page's "latest posts" are scraped as if they were
// hits. The stub reproduces that trap: any request carrying a query string is
// redirected, and the bare home page (what a following client would land on)
// is a valid 200 page with a decoy post. A refused redirect must therefore be
// an error with NO results, and the home page must never be requested.
func TestAudioBookBayRefusesRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requested []string
			rt := abbRoundTripFunc(func(req *http.Request) *http.Response {
				requested = append(requested, req.URL.String())
				if req.URL.RawQuery != "" {
					return abbStubResponse(status, "https://audiobookbay.lu/", "")
				}
				return abbStubResponse(http.StatusOK, "", abbPostPage("Home Page Decoy"))
			})
			a := newRedirectTestABB(t, &http.Client{Transport: rt}, "audiobookbay.lu")

			results, err := a.searchDomain(context.Background(), "audiobookbay.lu", "Tolkien")
			if err == nil {
				t.Fatalf("searchDomain returned no error for a %d redirect (results=%d)", status, len(results))
			}
			wantStatus := "HTTP " + strconv.Itoa(status)
			if !strings.Contains(err.Error(), wantStatus) {
				t.Errorf("error %q does not mention %q", err, wantStatus)
			}
			if !strings.Contains(err.Error(), "https://audiobookbay.lu/") {
				t.Errorf("error %q does not mention the Location", err)
			}
			if len(results) != 0 {
				t.Errorf("got %d results from a refused redirect, want 0 (scraped the home page?)", len(results))
			}
			if len(requested) != 1 {
				t.Errorf("redirect was followed: requests = %v", requested)
			}

			// Through Search, with only this mirror, the source as a whole fails.
			results, err = a.Search(context.Background(), "Tolkien")
			if err == nil || !strings.Contains(err.Error(), "all AudioBookBay domains failed") {
				t.Errorf("Search error = %v, want all AudioBookBay domains failed", err)
			}
			if len(results) != 0 {
				t.Errorf("Search returned %d results, want 0", len(results))
			}
		})
	}
}

// Production has two dead mirrors; a future redirect on the live one must
// not stop a later working mirror from answering, and the redirecting mirror
// must not be followed.
func TestAudioBookBaySearchFallsThroughRedirectingMirror(t *testing.T) {
	hits := map[string]int{}
	rt := abbRoundTripFunc(func(req *http.Request) *http.Response {
		hits[req.URL.Host]++
		switch req.URL.Host {
		case "redirecting.test":
			if req.URL.RawQuery != "" {
				return abbStubResponse(http.StatusMovedPermanently, "https://redirecting.test/", "")
			}
			return abbStubResponse(http.StatusOK, "", abbPostPage("Home Page Decoy"))
		case "good.test":
			return abbStubResponse(http.StatusOK, "", abbPostPage("Good Result"))
		}
		t.Errorf("unexpected host %q", req.URL.Host)
		return abbStubResponse(http.StatusInternalServerError, "", "")
	})
	a := newRedirectTestABB(t, &http.Client{Transport: rt}, "redirecting.test", "good.test")

	results, err := a.Search(context.Background(), "Tolkien")
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Good Result" {
		t.Fatalf("results = %+v, want exactly the second mirror's \"Good Result\"", results)
	}
	if hits["redirecting.test"] != 1 || hits["good.test"] != 1 {
		t.Errorf("hits = %v, want 1 each (redirect must not be followed)", hits)
	}
}

// The no-redirect policy belongs to AudioBookBay alone: the client handed in is
// the one shared with every other source and must keep its redirect behaviour.
// A plain 200 must still parse through the constructor-built client.
func TestNewAudioBookBayDoesNotMutateSharedClientAndParses200(t *testing.T) {
	// A pointer transport (not abbRoundTripFunc) so the Transport can be compared.
	shared := &http.Client{Transport: &abbRoundTripper{body: abbPostPage("The Martian")}}
	a := newRedirectTestABB(t, shared, "audiobookbay.lu")

	if shared.CheckRedirect != nil {
		t.Fatal("NewAudioBookBay mutated the shared client's CheckRedirect")
	}
	if a.client == shared {
		t.Fatal("NewAudioBookBay kept the shared client pointer instead of a copy")
	}
	if a.client.Transport != shared.Transport {
		t.Error("copy does not share the Transport (connection pool would be lost)")
	}

	results, err := a.searchDomain(context.Background(), "audiobookbay.lu", "The Martian")
	if err != nil {
		t.Fatalf("searchDomain returned error on a 200: %v", err)
	}
	if len(results) != 1 || results[0].Title != "The Martian" {
		t.Fatalf("results = %+v, want one \"The Martian\"", results)
	}
}

func TestResolveABBMagnetUsesBrowserHeaders(t *testing.T) {
	rt := &abbRoundTripper{
		body: `<html><body>
		<h1>The Martian</h1>
		<table>
			<tr><td>Info Hash:</td><td>0123456789ABCDEF0123456789ABCDEF01234567</td></tr>
			<tr><td>udp://tracker.example:1337/announce</td></tr>
		</table>
		</body></html>`,
	}
	client := &http.Client{Transport: rt}

	magnet, err := ResolveABBMagnet(context.Background(), client, "test", "/abss/the-martian-andy-weir/", []string{"audiobookbay.lu"}, []string{"udp://fallback.example:1337/announce"})
	if err != nil {
		t.Fatalf("ResolveABBMagnet returned error: %v", err)
	}
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567") {
		t.Fatalf("unexpected magnet: %s", magnet)
	}
	if got := rt.req.Header.Get("User-Agent"); got != abbBrowserUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, abbBrowserUserAgent)
	}
	if got := rt.req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
}

func TestResolveABBMagnetFallsBackToLegacyInfoHashRegex(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		infoHash string
	}{
		{
			name:     "same line text",
			body:     `<html><body><h1>The Martian</h1><div>Info Hash: 89ABCDEF0123456789ABCDEF0123456789ABCDEF</div></body></html>`,
			infoHash: "89ABCDEF0123456789ABCDEF0123456789ABCDEF",
		},
		{
			name:     "same line td",
			body:     `<html><body><h1>The Martian</h1><table><tr><td colspan="2">Info Hash: legacy layout</td></tr><tr><td>FEDCBA9876543210FEDCBA9876543210FEDCBA98</td></tr></table></body></html>`,
			infoHash: "FEDCBA9876543210FEDCBA9876543210FEDCBA98",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &abbRoundTripper{body: tt.body}
			client := &http.Client{Transport: rt}

			magnet, err := ResolveABBMagnet(context.Background(), client, "test", "/abss/the-martian-andy-weir/", []string{"audiobookbay.lu"}, []string{"udp://fallback.example:1337/announce"})
			if err != nil {
				t.Fatalf("ResolveABBMagnet returned error: %v", err)
			}
			if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:"+tt.infoHash) {
				t.Fatalf("unexpected magnet: %s", magnet)
			}
			if !strings.Contains(magnet, "tr=udp%3A%2F%2Ffallback.example%3A1337%2Fannounce") {
				t.Fatalf("expected fallback tracker in magnet: %s", magnet)
			}
		})
	}
}

func TestExtractABBInfoHash(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<html><body>
		<table>
			<tr>
				<td>Info Hash:</td>
				<td>0123456789ABCDEF0123456789ABCDEF01234567</td>
			</tr>
		</table>
	</body></html>`))
	if err != nil {
		t.Fatalf("parse HTML: %v", err)
	}

	got := extractABBInfoHash(doc, regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b`))
	want := "0123456789ABCDEF0123456789ABCDEF01234567"
	if got != want {
		t.Fatalf("extractABBInfoHash = %q, want %q", got, want)
	}
}

// The language and keywords fields need not have whitespace between their tags.
func TestAudioBookBaySearchLanguage(t *testing.T) {
	tests := []struct {
		name, info string
		want       int
	}{
		{"adjacent keywords", `Category: Fantasy&nbsp; <br />Language: English<span style="margin-left:100px;">Keywords: Alchemy&nbsp;</span><br />`, 1},
		{"space before keywords", `Language: English <span>Keywords: Alchemy</span>`, 1},
		{"line break", `Language: English<br />Keywords: Alchemy`, 1},
		{"formatted language", `<b>Language:</b><span>English</span><span>Keywords: Alchemy</span>`, 1},
		{"nonbreaking space", `Language:&nbsp;English&nbsp;Keywords: Alchemy`, 1},
		{"mixed case", `LANGUAGE: eNgLiSh<span>Keywords: Alchemy</span>`, 1},
		{"comma separated", `Language: English, French`, 1},
		{"non English", `Language: French<span>Keywords: English</span>`, 0},
		{"English prefix is not English", `Language: Englishish<span>Keywords: Alchemy</span>`, 0},
		{"no language", `Category: Fantasy`, 1},
		{"empty language", `Language:`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &abbRoundTripper{body: `<div class="post"><h2><a href="/abss/example/">Example Book</a></h2><div class="postInfo">` + tt.info + `</div></div>`}
			a := NewAudioBookBay(&config.Config{}, &http.Client{Transport: rt})
			results, err := a.searchDomain(context.Background(), "abb.example", "example")
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != tt.want {
				t.Fatalf("got %d results, want %d", len(results), tt.want)
			}
			if len(results) > 0 && (results[0].Title != "Example Book" || results[0].AbbURL != "/abss/example/" || results[0].Source != "audiobook") {
				t.Fatalf("unexpected result: %+v", results[0])
			}
		})
	}
}

func TestAudioBookBaySearchLowercasesQuery(t *testing.T) {
	for _, query := range []string{"Example Book", "EXAMPLE BOOK", "example book", "Éxample & Book+One"} {
		t.Run(query, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Query().Get("s") != strings.ToLower(query) || r.URL.Query().Get("tt") != "1" {
					// Reproduce ABB redirecting capitalized searches to its home page.
					if r.URL.RawQuery != "" {
						http.Redirect(w, r, "/", http.StatusMovedPermanently)
						return
					}
					io.WriteString(w, `<html><body>Home page</body></html>`)
					return
				}
				io.WriteString(w, `<div class="post"><h2><a href="/abss/example/">Example Book</a></h2><div class="postInfo">Language: English<span>Keywords: Alchemy</span></div></div>`)
			}))
			defer server.Close()
			reg, err := sourcestest.Registry()
			if err != nil {
				t.Fatal(err)
			}
			reg.AudioBookBay.Mirrors = []string{strings.TrimPrefix(server.URL, "https://")}
			a := NewAudioBookBay(&config.Config{Sources: reg}, server.Client())
			results, err := a.Search(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Errorf("got %d requests, want one search without a home-page redirect", requests)
			}
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
		})
	}
}
