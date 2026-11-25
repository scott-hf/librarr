package search

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/sources"
)

// librivoxTestConfig returns a config whose Librivox endpoint is url.
func librivoxTestConfig(enabled bool, url string) *config.Config {
	reg := &sources.Registry{}
	reg.Librivox.URL = url
	return &config.Config{LibrivoxEnabled: enabled, UserAgent: "test", Sources: reg}
}

func TestLibrivoxEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		url     string
		want    bool
	}{
		{"enabled with endpoint", true, "https://librivox.example/api/feed/audiobooks", true},
		{"disabled by config", false, "https://librivox.example/api/feed/audiobooks", false},
		{"enabled but no endpoint", true, "", false},
	}
	t.Run("enabled but no registry loaded", func(t *testing.T) {
		if NewLibrivox(&config.Config{LibrivoxEnabled: true}, &http.Client{}).Enabled() {
			t.Error("Enabled() = true with a nil source registry, want false")
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := librivoxTestConfig(tt.enabled, tt.url)
			if got := NewLibrivox(cfg, &http.Client{}).Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLibrivoxSearch_UpstreamDownIsAnError(t *testing.T) {
	// A Cloudflare 522 on every request must surface as a failure, not as an
	// empty result set, so it is logged and counted by the circuit breaker.
	srv := newRecordingServer(t, 522, "origin unreachable")
	cfg := librivoxTestConfig(true, srv.URL)

	results, err := NewLibrivox(cfg, &http.Client{Timeout: 5 * time.Second}).Search(context.Background(), "dune")
	if err == nil {
		t.Fatalf("Search returned nil error with results=%v, want an error", results)
	}
	if !strings.Contains(err.Error(), "522") {
		t.Errorf("error %q should name the upstream status", err)
	}
}

func TestLibrivoxSearch_EmptyAnswerIsNotAnError(t *testing.T) {
	srv := newRecordingServer(t, http.StatusOK, `{"books":[]}`)
	cfg := librivoxTestConfig(true, srv.URL)

	results, err := NewLibrivox(cfg, &http.Client{Timeout: 5 * time.Second}).Search(context.Background(), "zzqx")
	if err != nil {
		t.Fatalf("a healthy upstream with no matches returned error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none", results)
	}
}
