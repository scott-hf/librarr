package search

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/models"
)

// captureLogs routes slog to a buffer for the duration of the test, then
// restores the silenced default that TestMain installs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	prev := slog.Default()
	buf := &lockedBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// blockingSearcher honors its context, like a source stuck on a dead upstream:
// it returns only when the per-source deadline (or the caller) cancels it.
// swallow mimics the old Librivox behaviour of returning (nil, nil) on cancel.
type blockingSearcher struct {
	name    string
	swallow bool
}

func (b blockingSearcher) Name() string         { return b.name }
func (b blockingSearcher) Label() string        { return b.name }
func (b blockingSearcher) Enabled() bool        { return true }
func (b blockingSearcher) SearchTab() string    { return "audiobook" }
func (b blockingSearcher) DownloadType() string { return "direct" }
func (b blockingSearcher) Search(ctx context.Context, _ string) ([]models.SearchResult, error) {
	<-ctx.Done()
	if b.swallow {
		return nil, nil
	}
	return nil, ctx.Err()
}

func fastAudiobookSource(name string) *mockSearcher {
	return &mockSearcher{
		name:    name,
		tab:     "audiobook",
		results: []models.SearchResult{{Title: "Dune", Source: name}},
	}
}

func TestSearchWithAuthor_SlowSourceCutOffAtPerSourceTimeout(t *testing.T) {
	cases := []struct {
		name string
		slow Searcher
	}{
		{"source honors ctx and returns its error", blockingSearcher{name: "slow"}},
		{"source swallows the deadline and returns nothing", blockingSearcher{name: "slow", swallow: true}},
		{"source ignores ctx entirely", &mockSearcher{name: "slow", tab: "audiobook", delay: 3 * time.Second}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			mgr := NewManager(&config.Config{}, []Searcher{fastAudiobookSource("fast"), tc.slow}, NewHealthTracker(3, 300))
			mgr.sourceTimeout = 150 * time.Millisecond

			start := time.Now()
			results, _ := mgr.SearchWithAuthor(context.Background(), "audiobook", "Dune", "")
			took := time.Since(start)

			if took > 2*time.Second {
				t.Fatalf("search took %v; the slow source should have been cut off near 150ms", took)
			}
			if len(results) != 1 || results[0].Source != "fast" {
				t.Fatalf("results = %#v, want the single result from the fast source", results)
			}

			out := logs.String()
			if got := strings.Count(out, "level=WARN"); got != 1 {
				t.Fatalf("WARN lines = %d, want exactly 1 (one per failed source):\n%s", got, out)
			}
			for _, want := range []string{"source search timed out", "source=slow", "elapsed_ms=", "error="} {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "source=fast") {
				t.Errorf("healthy source must not be logged as failing:\n%s", out)
			}
		})
	}
}

func TestSearchWithAuthor_TimeoutFailureTripsCircuitBreaker(t *testing.T) {
	health := NewHealthTracker(2, 300)
	mgr := NewManager(&config.Config{}, []Searcher{blockingSearcher{name: "dead", swallow: true}}, health)
	mgr.sourceTimeout = 50 * time.Millisecond

	for i := 0; i < 2; i++ {
		mgr.SearchWithAuthor(context.Background(), "audiobook", "Dune", "")
	}
	if health.CanSearch("dead") {
		t.Error("circuit still closed after repeated timeouts; a swallowed timeout must count as a failure")
	}
}

func TestSearchWithAuthor_ParentDeadlineStillApplies(t *testing.T) {
	logs := captureLogs(t)
	mgr := NewManager(&config.Config{}, []Searcher{blockingSearcher{name: "slow"}}, NewHealthTracker(3, 300))
	mgr.sourceTimeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	mgr.SearchWithAuthor(ctx, "audiobook", "Dune", "")
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("search took %v; the caller's 100ms deadline should bound it", took)
	}
	if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a caller-side deadline is not a source failure and must not be logged as one:\n%s", logs.String())
	}
}

func TestSearchStream_SlowSourceCutOffAtPerSourceTimeout(t *testing.T) {
	mgr := NewManager(&config.Config{}, []Searcher{fastAudiobookSource("fast"), blockingSearcher{name: "slow", swallow: true}}, NewHealthTracker(3, 300))
	mgr.sourceTimeout = 150 * time.Millisecond

	start := time.Now()
	updates := map[string]SourceSearchUpdate{}
	for u := range mgr.SearchStream(context.Background(), "audiobook", "Dune", "") {
		updates[u.Source] = u
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("stream took %v; the slow source should have been cut off near 150ms", took)
	}
	if u := updates["fast"]; u.Err != nil || len(u.Results) != 1 {
		t.Errorf("fast update = %#v, want one result and no error", u)
	}
	if u := updates["slow"]; !errors.Is(u.Err, context.DeadlineExceeded) || !u.Done {
		t.Errorf("slow update = %#v, want a Done update carrying a deadline error", u)
	}
}

func TestSearchWithAuthor_DisabledLibrivoxIsSkipped(t *testing.T) {
	srv := newRecordingServer(t, http.StatusOK, `{"books":[]}`)

	build := func(enabled bool) *Manager {
		cfg := librivoxTestConfig(enabled, srv.URL)
		return NewManager(cfg, []Searcher{
			NewLibrivox(cfg, srv.Client()),
			fastAudiobookSource("fast"),
		}, NewHealthTracker(3, 300))
	}

	results, _ := build(false).SearchWithAuthor(context.Background(), "audiobook", "Dune", "")
	if srv.hit() {
		t.Fatal("disabled Librivox still made a request upstream")
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want the one result from the other source", len(results))
	}

	build(true).SearchWithAuthor(context.Background(), "audiobook", "Dune", "")
	if !srv.hit() {
		t.Fatal("enabled Librivox made no request; the kill switch must not disable it by default")
	}
}
