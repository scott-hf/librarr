package api

import (
	"net/http"
	"strconv"

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

// handleDiscoverPopular serves GET /api/discover/popular?genre=&limit=.
// Open Library failures are reported in the body's status field (HTTP 200),
// matching the search endpoints; auth/transport failures stay HTTP errors.
func (s *Server) handleDiscoverPopular(w http.ResponseWriter, r *http.Request) {
	if s.discoverClient == nil {
		writeJSON(w, http.StatusOK, discoverUnavailable())
		return
	}
	res := s.discoverClient.Popular(r.Context(), r.URL.Query().Get("genre"), discoverLimit(r))
	writeJSON(w, http.StatusOK, res)
}

// handleDiscoverSimilar serves GET /api/discover/similar?title=&author=&genre=&limit=.
func (s *Server) handleDiscoverSimilar(w http.ResponseWriter, r *http.Request) {
	if s.discoverClient == nil {
		writeJSON(w, http.StatusOK, discoverUnavailable())
		return
	}
	q := r.URL.Query()
	res := s.discoverClient.Similar(r.Context(), q.Get("title"), q.Get("author"), q.Get("genre"), discoverLimit(r))
	writeJSON(w, http.StatusOK, res)
}

func discoverUnavailable() discover.Result {
	return discover.Result{
		Status: discover.StatusUpstreamUnavailable,
		Error:  "Discover is not configured",
		Items:  []discover.Item{},
	}
}
