package discover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode"
)

// Catalog ids served by Browse and advertised by Options. The id "for_you" is
// client-only and never used by the server.
const (
	CatalogPopular  = "popular"
	CatalogNew      = "new"
	CatalogTopRated = "top_rated"
)

const (
	// BrowsePageSize is the fixed number of Open Library docs per page. The
	// number of items is smaller whenever cover-less or duplicate docs drop out.
	BrowsePageSize = 20
	// BrowseMaxOffset is Librarr's own paging policy (Open Library has no cap
	// at this depth): the last page starts here.
	BrowseMaxOffset = 180

	// topRated thresholds (P0 probe): thin genres still end early, which the
	// app shows as the empty state.
	topRatedMinCountGenre = 50
	topRatedMinCountAll   = 100
)

// Option is one selectable id/label pair in the options response.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// OptionsResult is the wire response of /api/discover/options.
type OptionsResult struct {
	Status   string   `json:"status"`
	Error    string   `json:"error"`
	Catalogs []Option `json:"catalogs"`
	Genres   []Option `json:"genres"`
}

// BrowseResult is the wire response of /api/discover/browse. It is a separate
// type from Result (the shelf shape stays frozen).
//
// Catalog, Genre and Offset echo the request so the client can drop a late
// response that no longer matches what it asked for. NextOffset is null when
// there is no further page (never derived from the item count).
type BrowseResult struct {
	Status     string `json:"status"`
	Error      string `json:"error"`
	Catalog    string `json:"catalog"`
	Genre      string `json:"genre"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset"`
	Partial    bool   `json:"partial"`
	Items      []Item `json:"items"`
}

func browseFailure(status, msg string) BrowseResult {
	return BrowseResult{Status: status, Error: msg, Items: []Item{}}
}

// catalogs is the pinned catalog list (labels are what the app shows).
var catalogs = []Option{
	{ID: CatalogPopular, Label: "Popular"},
	{ID: CatalogNew, Label: "New"},
	{ID: CatalogTopRated, Label: "Top rated"},
}

// CuratedGenres are the Open Library subjects offered as genres. Each id is
// also a valid Browse genre input (GenreSubjects maps it to itself). Chosen
// from the WI-1760 P0 probe: every genre fills at least one page on the
// popular catalog ("true crime" was dropped: the new catalog has 5 works).
var curatedGenres = []Option{
	{ID: "biography", Label: "Biography"},
	{ID: "business", Label: "Business"},
	{ID: "children's", Label: "Children's"},
	{ID: "classics", Label: "Classics"},
	{ID: "cooking", Label: "Cooking"},
	{ID: "dystopian", Label: "Dystopian"},
	{ID: "fantasy", Label: "Fantasy"},
	{ID: "historical fiction", Label: "Historical fiction"},
	{ID: "history", Label: "History"},
	{ID: "horror", Label: "Horror"},
	{ID: "humor", Label: "Humor"},
	{ID: "literary fiction", Label: "Literary fiction"},
	{ID: "memoir", Label: "Memoir"},
	{ID: "mystery", Label: "Mystery"},
	{ID: "paranormal", Label: "Paranormal"},
	{ID: "philosophy", Label: "Philosophy"},
	{ID: "poetry", Label: "Poetry"},
	{ID: "psychology", Label: "Psychology"},
	{ID: "religion", Label: "Religion"},
	{ID: "romance", Label: "Romance"},
	{ID: "science", Label: "Science"},
	{ID: "science fiction", Label: "Science fiction"},
	{ID: "self-help", Label: "Self-help"},
	{ID: "thriller", Label: "Thriller"},
	{ID: "young adult", Label: "Young adult"},
}

// CuratedGenres returns a copy of the curated genre list.
func CuratedGenres() []Option { return append([]Option(nil), curatedGenres...) }

// Options returns the static list of catalogs and genres. It never touches
// Open Library.
func Options() OptionsResult {
	return OptionsResult{
		Status:   StatusOK,
		Catalogs: append([]Option(nil), catalogs...),
		Genres:   CuratedGenres(),
	}
}

// OptionsUnavailable is the options response when discover is not configured.
func OptionsUnavailable(msg string) OptionsResult {
	return OptionsResult{Status: StatusUpstreamUnavailable, Error: msg, Catalogs: []Option{}, Genres: []Option{}}
}

// BrowseUnavailable is the browse response when discover is not configured.
func BrowseUnavailable(catalog, genre string, offset int, msg string) BrowseResult {
	r := browseFailure(StatusUpstreamUnavailable, msg)
	r.Catalog, r.Genre, r.Offset = NormalizeInput(catalog), NormalizeInput(genre), offset
	return r
}

func validCatalog(c string) bool {
	return c == CatalogPopular || c == CatalogNew || c == CatalogTopRated
}

func hasUnsafeRune(s string) bool {
	for _, r := range s {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Browse returns one page of a catalog, optionally narrowed to a genre.
//
// genre is an Open Library subject id from Options or a raw Audiobookshelf
// genre string; the first usable token is the single subject for every page.
// An empty genre means all genres. offset must be a multiple of BrowsePageSize
// in [0, BrowseMaxOffset]; anything else (or an unknown catalog / unusable
// genre) is invalid_request. The response echoes catalog, genre (as sent,
// normalized) and offset.
func (c *Client) Browse(ctx context.Context, catalog, genre string, offset int) BrowseResult {
	catalog = NormalizeInput(catalog)
	genre = NormalizeInput(genre)
	echo := func(r BrowseResult) BrowseResult {
		out := r
		out.Items = append(make([]Item, 0, len(r.Items)), r.Items...)
		if r.NextOffset != nil {
			n := *r.NextOffset
			out.NextOffset = &n
		}
		out.Catalog, out.Genre, out.Offset = catalog, genre, offset
		return out
	}
	if !validCatalog(catalog) {
		return echo(browseFailure(StatusInvalidRequest, "Unknown catalog"))
	}
	if offset < 0 || offset > BrowseMaxOffset || offset%BrowsePageSize != 0 {
		return echo(browseFailure(StatusInvalidRequest, "Offset must be a multiple of 20 between 0 and 180"))
	}
	subject := ""
	if genre != "" {
		if hasUnsafeRune(genre) {
			return echo(browseFailure(StatusInvalidRequest, "Unknown genre"))
		}
		tokens := GenreSubjects(genre)
		if len(tokens) == 0 {
			return echo(browseFailure(StatusInvalidRequest, "Unknown genre"))
		}
		subject = tokens[0]
	}
	year := 0
	if catalog == CatalogNew {
		year = c.now().UTC().Year()
	}
	key := "browse|" + catalog + "|" + subject + "|" + strconv.Itoa(offset) + "|" + strconv.Itoa(year)
	v := c.doAny(ctx, key,
		browseFailure(StatusUpstreamUnavailable, "request cancelled"),
		browseFailure(StatusUpstreamUnavailable, "internal error"),
		func(fctx context.Context) (any, time.Duration) {
			return c.computeBrowse(fctx, catalog, subject, offset, year)
		})
	res, ok := v.(BrowseResult)
	if !ok {
		res = browseFailure(StatusUpstreamUnavailable, "internal error")
	}
	return echo(res)
}

// browseParams builds the Open Library search for one catalog page.
// subject is a single normalized token without quotes or backslashes (Browse
// rejects those), so quoting it in the q string is injection-safe.
func browseParams(catalog, subject string, offset, year int) olParams {
	p := olParams{offset: offset, sendOffset: true}
	switch catalog {
	case CatalogPopular:
		if subject != "" {
			p.field, p.value = "subject", subject
		} else {
			p.q = "language:eng"
		}
		p.sort = "readinglog"
	case CatalogNew:
		cond := fmt.Sprintf("first_publish_year:[%d TO %d] cover_i:[1 TO *]", year-1, year)
		if subject != "" {
			p.q = fmt.Sprintf(`subject:"%s" %s`, subject, cond)
		} else {
			p.q = cond
		}
		p.sort = "readinglog"
	case CatalogTopRated:
		if subject != "" {
			p.q = fmt.Sprintf(`subject:"%s" ratings_count:[%d TO *]`, subject, topRatedMinCountGenre)
		} else {
			p.q = fmt.Sprintf("ratings_count:[%d TO *]", topRatedMinCountAll)
		}
		p.sort = "rating"
	}
	return p
}

func (c *Client) computeBrowse(ctx context.Context, catalog, subject string, offset, year int) (BrowseResult, time.Duration) {
	p := browseParams(catalog, subject, offset, year)
	resp, err := c.query(ctx, p)
	partial := false
	if offset == 0 {
		// The quality guard applies to page 0 only; later pages never fail on
		// cover count (the app filters owned/duplicate cards anyway).
		bad := errors.Is(err, errBadRequest) || (err == nil && !sortTrusted(resp))
		if bad {
			if catalog != CatalogPopular {
				slog.Warn("discover browse: sorted Open Library page unusable", "catalog", catalog, "subject", subject, "err", err)
				return browseFailure(StatusUpstreamUnavailable, "Open Library results are unusable right now"), failureTTL
			}
			// Popular may fall back to relevance order, flagged partial so the
			// app does not page a differently ordered list.
			slog.Warn("discover browse: sorted popular page unusable, retrying without sort", "subject", subject, "err", err)
			p.sort = ""
			resp, err = c.query(ctx, p)
			partial = true
		}
	}
	if err != nil {
		slog.Warn("discover browse failed", "catalog", catalog, "subject", subject, "offset", offset, "err", err)
		r := errResult(err)
		return browseFailure(r.Status, r.Error), failureTTL
	}

	res := BrowseResult{Status: StatusOK, Partial: partial, Items: toItems(resp.Docs, nil)}
	if !partial && len(resp.Docs) > 0 && offset+len(resp.Docs) < resp.NumFound && offset+BrowsePageSize <= BrowseMaxOffset {
		next := offset + BrowsePageSize
		res.NextOffset = &next
	}
	// A page can hold no visible card (cover-less docs) while more pages
	// follow: that is still ok. Only a page with nothing and no next is empty.
	if len(res.Items) == 0 && res.NextOffset == nil {
		res.Status = StatusEmpty
	}
	ttl := okTTL
	if partial {
		ttl = failureTTL // a degraded answer is retried soon
	}
	return res, ttl
}
