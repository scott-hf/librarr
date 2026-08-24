package search

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/models"
	"github.com/JeremiahM37/librarr/internal/sources/sourcestest"
)

// abbLiveFixture returns the saved live AudioBookBay search page for
// ?s=dracula&tt=1 (captured 2026-08-24 from audiobookbay.lu). It contains
// nine div.post result blocks.
func abbLiveFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/abb_dracula_live.html")
	if err != nil {
		t.Fatalf("read live fixture: %v", err)
	}
	return string(b)
}

// TestAudioBookBaySearchParsesLiveFixture pins the parser against a real,
// unmodified ABB search page. Before commit 1b86eb1 the language gate's
// equality check rejected all nine posts on this page (each carries
// "Language: EnglishKeywords: ..." with no separator), so the source
// returned zero rows while still recording search_ok — this test fails
// loudly on that class of regression instead. The parser must surface all
// nine posts.
func TestAudioBookBaySearchParsesLiveFixture(t *testing.T) {
	rt := &abbRoundTripper{body: abbLiveFixture(t)}
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
	if len(results) != 9 {
		for i, r := range results {
			t.Logf("result[%d]: title=%q source=%q abb=%q", i, r.Title, r.Source, r.AbbURL)
		}
		t.Fatalf("got %d results from live fixture, want 9", len(results))
	}
}

// TestAudioBookBayLiveFixtureSurvivesResultPipeline pushes the parsed live
// fixture through the same post-processing the API handler applies
// (FilterResults -> FilterAndSortResults -> ScoreResults -> dedupe) and
// requires that AudioBookBay rows survive to the response.
func TestAudioBookBayLiveFixtureSurvivesResultPipeline(t *testing.T) {
	rt := &abbRoundTripper{body: abbLiveFixture(t)}
	reg, err := sourcestest.Registry()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	cfg := &config.Config{
		UserAgent:               "test",
		Sources:                 reg,
		MinTorrentSizeBytes:     10000,
		MaxTorrentSizeBytes:     20e9,
		CircuitBreakerThreshold: 3,
	}
	cfg.Sources.AudioBookBay.Mirrors = []string{"audiobookbay.lu"}
	a := &AudioBookBay{cfg: cfg, client: &http.Client{Transport: rt}}

	results, err := a.searchDomain(context.Background(), "audiobookbay.lu", "Dracula")
	if err != nil {
		t.Fatalf("searchDomain returned error: %v", err)
	}

	mgr := NewManager(cfg, []Searcher{a}, NewHealthTracker(cfg.CircuitBreakerThreshold, cfg.CircuitBreakerTimeout))
	processed := mgr.ProcessResults(results, "Dracula", "")

	var abb []models.SearchResult
	for _, r := range processed {
		if r.Indexer == "AudioBookBay" {
			abb = append(abb, r)
		}
	}
	if len(abb) == 0 {
		for i, r := range processed {
			t.Logf("processed[%d]: title=%q source=%q seeders=%d abb=%q", i, r.Title, r.Source, r.Seeders, r.AbbURL)
		}
		t.Fatalf("no AudioBookBay results survived the pipeline (started with %d)", len(results))
	}
}
