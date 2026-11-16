package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/JeremiahM37/librarr/internal/discover"
)

// discoverLimit reads ?limit=, defaulting and clamping to the discover bounds
// (a value above the maximum is capped, not ignored).
func discoverLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		return discover.DefaultLimit
	}
	return discover.ClampLimit(n)
}

// writeDiscoverJSON writes a /api/discover/* body. Every discover response is
// no-cache (house style, as the search streams): a catalog page must never be
// served stale by an intermediary, the server-side cache owns freshness.
func writeDiscoverJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, v)
}

// handleDiscoverPopular serves GET /api/discover/popular?genre=&limit=.
// Open Library failures are reported in the body's status field (HTTP 200),
// matching the search endpoints; auth/transport failures stay HTTP errors.
func (s *Server) handleDiscoverPopular(w http.ResponseWriter, r *http.Request) {
	if s.discoverClient == nil {
		writeDiscoverJSON(w, discoverUnavailable())
		return
	}
	res := s.discoverClient.Popular(r.Context(), r.URL.Query().Get("genre"), discoverLimit(r))
	writeDiscoverJSON(w, res)
}

// handleDiscoverSimilar serves GET /api/discover/similar?title=&author=&genre=&limit=.
func (s *Server) handleDiscoverSimilar(w http.ResponseWriter, r *http.Request) {
	if s.discoverClient == nil {
		writeDiscoverJSON(w, discoverUnavailable())
		return
	}
	q := r.URL.Query()
	res := s.discoverClient.Similar(r.Context(), q.Get("title"), q.Get("author"), q.Get("genre"), discoverLimit(r))
	writeDiscoverJSON(w, res)
}

// handleDiscoverOptions serves GET /api/discover/options: the catalogs and
// genres the app may offer. The list is static (no Open Library call).
func (s *Server) handleDiscoverOptions(w http.ResponseWriter, r *http.Request) {
	if s.discoverClient == nil {
		writeDiscoverJSON(w, discover.OptionsUnavailable("Discover is not configured"))
		return
	}
	writeDiscoverJSON(w, discover.Options())
}

// handleDiscoverBrowse serves GET /api/discover/browse?catalog=&genre=&offset=.
// A missing offset is 0; an unparsable or out-of-range one is an
// invalid_request status in the body (HTTP 200), not an HTTP error.
func (s *Server) handleDiscoverBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	catalog, genre := q.Get("catalog"), q.Get("genre")
	offset := 0
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			n = -1 // Browse rejects it as invalid_request
		}
		offset = n
	}
	if s.discoverClient == nil {
		writeDiscoverJSON(w, discover.BrowseUnavailable(catalog, genre, offset, "Discover is not configured"))
		return
	}
	writeDiscoverJSON(w, s.discoverClient.Browse(r.Context(), catalog, genre, offset))
}

func discoverUnavailable() discover.Result {
	return discover.Result{
		Status: discover.StatusUpstreamUnavailable,
		Error:  "Discover is not configured",
		Items:  []discover.Item{},
	}
}
