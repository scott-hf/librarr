// Package discover serves the "shelf" endpoints (popular-in-genre and
// similar-to-a-book) from Open Library search.
//
// Librarr owns every Open Library policy here so that clients stay thin:
// a descriptive User-Agent with a contact address, a local rate limiter with
// backoff after a 429, a bounded list cache, coalescing of identical in-flight
// requests, a short failure cache (failures are cached as failures, never as
// an empty success), and the sort=readinglog quality guard.
//
// Only the standard library is used.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Response statuses. OL failures are always reported through these (HTTP 200
// at the API layer); only Librarr transport/auth failures become HTTP errors.
const (
	StatusOK                  = "ok"
	StatusEmpty               = "empty"
	StatusUpstreamUnavailable = "upstream_unavailable"
	StatusRateLimited         = "rate_limited"
	StatusInvalidRequest      = "invalid_request"
)

const (
	// DefaultLimit and MaxLimit bound the number of items returned.
	DefaultLimit = 20
	MaxLimit     = 20

	// MaxInputRunes caps each raw input string (genre, title, author).
	MaxInputRunes = 200

	defaultBaseURL = "https://openlibrary.org"
	defaultContact = "contact@librarr.invalid"

	// olFetchLimit is what we ask Open Library for; the cached list is sliced
	// to the caller's limit, so the cache key does not depend on the limit.
	olFetchLimit = 20

	// minCovers is the quality bar: a sorted page is trusted, and a subject is
	// "good enough", only when at least this many docs carry cover_i.
	minCovers = 8
	// similarMin is the number of author results below which Similar tops the
	// list up from the seed's genre.
	similarMin = 6

	okTTL        = 30 * time.Minute
	failureTTL   = 30 * time.Second
	maxCacheSize = 512
	fetchTimeout = 15 * time.Second
	maxBodyBytes = 4 << 20

	olRatePerSecond = 3
	limiterMaxWait  = 5 * time.Second
	maxBackoff      = 60 * time.Second
)

// Item is one book card.
type Item struct {
	Key        string `json:"key"` // Open Library work key, e.g. /works/OL45804W
	Title      string `json:"title"`
	Author     string `json:"author"`
	CoverURL   string `json:"cover_url"`
	Year       int    `json:"year"`       // first publish year, 0 when unknown
	Popularity int    `json:"popularity"` // Open Library readinglog_count
}

// Result is the wire response for both endpoints.
type Result struct {
	Status          string `json:"status"`
	Error           string `json:"error"`
	ResolvedSubject string `json:"resolved_subject"`
	Items           []Item `json:"items"`
}

func failure(status, msg string) Result {
	return Result{Status: status, Error: msg, Items: []Item{}}
}

func clone(r Result, limit int) Result {
	out := r
	n := len(r.Items)
	if limit > 0 && n > limit {
		n = limit
	}
	out.Items = make([]Item, n)
	copy(out.Items, r.Items[:n])
	if out.Status == StatusOK && n == 0 {
		out.Status = StatusEmpty
	}
	return out
}

// ClampLimit applies the default and maximum.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

type cacheEntry struct {
	res     Result
	expires time.Time
}

type call struct {
	done    chan struct{}
	res     Result
	waiters int
}

// Client queries Open Library for shelf data.
type Client struct {
	http      *http.Client
	baseURL   string
	userAgent string
	now       func() time.Time
	lim       *limiter

	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]*call
}

// New builds a Client. contact is an email address (or URL) that identifies
// the operator to Open Library; empty selects an obvious placeholder.
func New(httpClient *http.Client, contact string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: fetchTimeout}
	}
	contact = strings.TrimSpace(contact)
	if contact == "" {
		contact = defaultContact
	}
	c := &Client{
		http:      httpClient,
		baseURL:   defaultBaseURL,
		userAgent: "Librarr/2.0 (book download manager; github.com/JeremiahM37/librarr; contact: " + contact + ")",
		now:       time.Now,
		cache:     make(map[string]cacheEntry),
		inflight:  make(map[string]*call),
	}
	c.lim = newLimiter(time.Second/olRatePerSecond, limiterMaxWait, func() time.Time { return c.now() })
	return c
}

// SetBaseURL points the client at another Open Library compatible server
// (tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// Popular returns the most-read books for a genre string as ABS reports it.
func (c *Client) Popular(ctx context.Context, genre string, limit int) Result {
	limit = ClampLimit(limit)
	norm := NormalizeInput(genre)
	if norm == "" {
		return failure(StatusInvalidRequest, "No genre provided")
	}
	tokens := GenreSubjects(norm)
	if len(tokens) == 0 {
		return failure(StatusEmpty, "No usable genre in input")
	}
	key := "popular|" + strings.Join(tokens, "|")
	return clone(c.do(ctx, key, func(fctx context.Context) (Result, time.Duration) {
		return c.computePopular(fctx, tokens)
	}), limit)
}

// Similar returns books related to a seed book: the seed author's most-read
// books (seed excluded), topped up from the seed's genre when thin.
func (c *Client) Similar(ctx context.Context, title, author, genre string, limit int) Result {
	limit = ClampLimit(limit)
	title = NormalizeInput(title)
	author = NormalizeInput(author)
	genre = NormalizeInput(genre)
	if title == "" {
		return failure(StatusInvalidRequest, "No title provided")
	}
	var tokens []string
	if genre != "" {
		tokens = GenreSubjects(genre)
	}
	author = firstAuthor(author)
	if author == "" && len(tokens) == 0 {
		return failure(StatusInvalidRequest, "Author or genre required")
	}
	key := "similar|" + strings.ToLower(title) + "|" + strings.ToLower(author) + "|" + strings.Join(tokens, "|")
	return clone(c.do(ctx, key, func(fctx context.Context) (Result, time.Duration) {
		return c.computeSimilar(fctx, title, author, tokens)
	}), limit)
}

// NormalizeInput trims, collapses whitespace and caps the string at
// MaxInputRunes runes. The normalized value is what cache keys derive from.
func NormalizeInput(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > MaxInputRunes {
		s = strings.TrimSpace(string([]rune(s)[:MaxInputRunes]))
	}
	return s
}

var genreAliases = map[string]string{
	"sci-fi": "science fiction",
	"scifi":  "science fiction",
	"sf":     "science fiction",
	"teen":   "young adult",
}

// GenreSubjects turns an ABS genre string ("Science Fiction & Fantasy",
// "Mystery, Thriller & Suspense") into Open Library subject candidates in
// order: lowercase, split on & , /, noise dropped, aliases applied.
func GenreSubjects(genre string) []string {
	genre = strings.ToLower(strings.Join(strings.Fields(genre), " "))
	parts := strings.FieldsFunc(genre, func(r rune) bool { return r == '&' || r == ',' || r == '/' })
	var tokens []string
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "audiobook" || p == "audiobooks" {
			continue
		}
		if a, ok := genreAliases[p]; ok {
			p = a
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		tokens = append(tokens, p)
	}
	// fiction / nonfiction are noise only when a more specific token exists.
	if len(tokens) > 1 {
		kept := tokens[:0:0]
		for _, t := range tokens {
			if t == "fiction" || t == "nonfiction" || t == "non-fiction" {
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) > 0 {
			tokens = kept
		}
	}
	return tokens
}

func firstAuthor(a string) string {
	if i := strings.Index(a, ";"); i >= 0 {
		a = a[:i]
	}
	if i := strings.Index(a, " & "); i >= 0 {
		a = a[:i]
	}
	return strings.TrimSpace(a)
}

// normTitle reduces a title to lowercase alphanumeric words with any
// parenthetical removed, for dedupe and seed exclusion.
func normTitle(t string) string {
	t = strings.ToLower(t)
	for {
		i := strings.Index(t, "(")
		if i < 0 {
			break
		}
		j := strings.Index(t[i:], ")")
		if j < 0 {
			t = t[:i]
			break
		}
		t = t[:i] + " " + t[i+j+1:]
	}
	var b strings.Builder
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// do serves a request from the cache, joins an identical in-flight request, or
// starts one. The fetch runs detached from the caller's cancellation (with its
// own timeout) so one cancelled caller cannot fail the others.
func (c *Client) do(ctx context.Context, key string, compute func(context.Context) (Result, time.Duration)) Result {
	c.mu.Lock()
	if e, ok := c.cache[key]; ok {
		if c.now().Before(e.expires) {
			c.mu.Unlock()
			return e.res
		}
		delete(c.cache, key)
	}
	cl, joined := c.inflight[key]
	if !joined {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		go c.run(ctx, key, cl, compute)
	}
	cl.waiters++
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.res
	case <-ctx.Done():
		return failure(StatusUpstreamUnavailable, "request cancelled")
	}
}

func (c *Client) run(ctx context.Context, key string, cl *call, compute func(context.Context) (Result, time.Duration)) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	res, ttl := failure(StatusUpstreamUnavailable, "internal error"), time.Duration(0)
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("discover compute panicked", "key", key, "panic", r)
			}
		}()
		res, ttl = compute(fctx)
	}()

	c.mu.Lock()
	delete(c.inflight, key)
	if ttl > 0 {
		c.store(key, res, ttl)
	}
	cl.res = res
	c.mu.Unlock()
	close(cl.done)
}

// store inserts into the bounded cache. Caller holds c.mu.
func (c *Client) store(key string, res Result, ttl time.Duration) {
	now := c.now()
	if len(c.cache) >= maxCacheSize {
		for k, e := range c.cache {
			if !now.Before(e.expires) {
				delete(c.cache, k)
			}
		}
	}
	for len(c.cache) >= maxCacheSize {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.cache {
			if oldestKey == "" || e.expires.Before(oldest) {
				oldestKey, oldest = k, e.expires
			}
		}
		delete(c.cache, oldestKey)
	}
	c.cache[key] = cacheEntry{res: res, expires: now.Add(ttl)}
}

// ---- Open Library access ----------------------------------------------

var (
	errRateLimited = errors.New("open library rate limited")
	errBadRequest  = errors.New("open library HTTP 400")
)

type olDoc struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	AuthorName       []string `json:"author_name"`
	FirstPublishYear int      `json:"first_publish_year"`
	CoverI           int      `json:"cover_i"`
	ReadingLogCount  int      `json:"readinglog_count"`
}

type olResponse struct {
	NumFound int     `json:"numFound"`
	Docs     []olDoc `json:"docs"`
}

func (c *Client) query(ctx context.Context, field, value string, sorted bool) (*olResponse, error) {
	if err := c.lim.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set(field, value)
	if sorted {
		q.Set("sort", "readinglog")
	}
	q.Set("language", "eng")
	q.Set("limit", strconv.Itoa(olFetchLimit))
	q.Set("fields", "key,title,author_name,first_publish_year,cover_i,readinglog_count")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/search.json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		var after time.Duration
		if secs, convErr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); convErr == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		c.lim.penalize(after)
		return nil, errRateLimited
	case resp.StatusCode == http.StatusBadRequest:
		return nil, errBadRequest
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("open library HTTP %d", resp.StatusCode)
	}
	var out olResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("open library decode: %w", err)
	}
	c.lim.success()
	return &out, nil
}

func coverCount(docs []olDoc) int {
	n := 0
	for _, d := range docs {
		if d.CoverI > 0 {
			n++
		}
	}
	return n
}

// sortTrusted reports whether a sorted page can be believed: enough covered
// docs, or the page already holds the entire result set (sort cannot matter).
func sortTrusted(r *olResponse) bool {
	return coverCount(r.Docs) >= minCovers || len(r.Docs) >= r.NumFound
}

// ranked runs a readinglog-sorted search with the quality guard: on a 400 or
// an untrusted 200 it retries once without sort; transport errors, 429 and 5xx
// are not sort problems and are returned as is.
func (c *Client) ranked(ctx context.Context, field, value string) ([]olDoc, error) {
	resp, err := c.query(ctx, field, value, true)
	if err == nil && sortTrusted(resp) {
		return resp.Docs, nil
	}
	if err != nil && !errors.Is(err, errBadRequest) {
		return nil, err
	}
	slog.Warn("discover: sorted Open Library search unusable, retrying without sort", "field", field, "value", value, "err", err)
	resp, err = c.query(ctx, field, value, false)
	if err != nil {
		return nil, err
	}
	return resp.Docs, nil
}

// toItems keeps docs with a cover, dedupes by work key and normalized title,
// and drops anything whose normalized title is in skip.
func toItems(docs []olDoc, skip map[string]bool) []Item {
	seenKey := map[string]bool{}
	seenTitle := map[string]bool{}
	items := make([]Item, 0, len(docs))
	for _, d := range docs {
		if d.CoverI <= 0 || d.Key == "" || strings.TrimSpace(d.Title) == "" {
			continue
		}
		nt := normTitle(d.Title)
		if nt == "" || skip[nt] || seenKey[d.Key] || seenTitle[nt] {
			continue
		}
		seenKey[d.Key] = true
		seenTitle[nt] = true
		it := Item{
			Key:        d.Key,
			Title:      strings.TrimSpace(d.Title),
			CoverURL:   fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", d.CoverI),
			Year:       d.FirstPublishYear,
			Popularity: d.ReadingLogCount,
		}
		if len(d.AuthorName) > 0 {
			it.Author = strings.TrimSpace(d.AuthorName[0])
		}
		items = append(items, it)
	}
	return items
}

func errResult(err error) Result {
	if errors.Is(err, errRateLimited) {
		return failure(StatusRateLimited, "Open Library is rate limiting requests")
	}
	return failure(StatusUpstreamUnavailable, "Open Library is unavailable")
}

func (c *Client) computePopular(ctx context.Context, tokens []string) (Result, time.Duration) {
	// At most two subjects are tried (each may retry once without sort).
	const maxSubjects = 2
	var best []Item
	var bestSubject string
	for i, tok := range tokens {
		if i >= maxSubjects {
			break
		}
		docs, err := c.ranked(ctx, "subject", tok)
		if err != nil {
			if best != nil {
				break
			}
			slog.Warn("discover popular failed", "subject", tok, "err", err)
			return errResult(err), failureTTL
		}
		items := toItems(docs, nil)
		if len(items) > len(best) {
			best, bestSubject = items, tok
		}
		if len(items) >= minCovers {
			break
		}
	}
	if len(best) == 0 {
		return Result{Status: StatusEmpty, ResolvedSubject: "", Items: []Item{}}, okTTL
	}
	return Result{Status: StatusOK, ResolvedSubject: bestSubject, Items: best}, okTTL
}

func (c *Client) computeSimilar(ctx context.Context, title, author string, tokens []string) (Result, time.Duration) {
	skip := map[string]bool{normTitle(title): true}
	var items []Item
	subject := ""
	partial := false

	if author != "" {
		docs, err := c.ranked(ctx, "author", author)
		if err != nil {
			slog.Warn("discover similar (author) failed", "author", author, "err", err)
			if len(tokens) == 0 {
				return errResult(err), failureTTL
			}
			// Fall through to the genre call; the response is partial.
			partial = true
		} else {
			items = toItems(docs, skip)
		}
	}
	if len(items) < similarMin && len(tokens) > 0 {
		docs, err := c.ranked(ctx, "subject", tokens[0])
		if err != nil {
			slog.Warn("discover similar (genre) failed", "subject", tokens[0], "err", err)
			if len(items) == 0 {
				return errResult(err), failureTTL
			}
			partial = true
		} else {
			have := map[string]bool{}
			for k := range skip {
				have[k] = true
			}
			for _, it := range items {
				have[normTitle(it.Title)] = true
			}
			for _, it := range toItems(docs, have) {
				items = append(items, it)
				if len(items) >= olFetchLimit {
					break
				}
			}
			subject = tokens[0]
		}
	}
	ttl := okTTL
	if partial {
		// Cache a degraded answer only briefly so the missing half is retried.
		ttl = failureTTL
	}
	if len(items) == 0 {
		return Result{Status: StatusEmpty, Items: []Item{}}, ttl
	}
	return Result{Status: StatusOK, ResolvedSubject: subject, Items: items}, ttl
}

// ---- Open Library rate limiter -----------------------------------------

// limiter spaces requests at a fixed rate and refuses (rather than queueing
// indefinitely) once saturated or while backing off after a 429.
type limiter struct {
	mu           sync.Mutex
	interval     time.Duration
	maxWait      time.Duration
	now          func() time.Time
	next         time.Time
	backoffUntil time.Time
	strikes      int
}

func newLimiter(interval, maxWait time.Duration, now func() time.Time) *limiter {
	return &limiter{interval: interval, maxWait: maxWait, now: now}
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	if now.Before(l.backoffUntil) {
		l.mu.Unlock()
		return errRateLimited
	}
	slot := l.next
	if slot.Before(now) {
		slot = now
	}
	if slot.Sub(now) > l.maxWait {
		l.mu.Unlock()
		return errRateLimited
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	d := slot.Sub(now)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// penalize starts a backoff after a 429: Retry-After when given, else
// exponential from 2 s, always capped.
func (l *limiter) penalize(retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.strikes++
	d := retryAfter
	if d <= 0 {
		shift := l.strikes - 1
		if shift > 5 {
			shift = 5
		}
		d = 2 * time.Second << shift
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	l.backoffUntil = l.now().Add(d)
}

func (l *limiter) success() {
	l.mu.Lock()
	l.strikes = 0
	l.mu.Unlock()
}
