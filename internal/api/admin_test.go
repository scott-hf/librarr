package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/search"
)

// The dashboard read a "state" key that HealthTracker.Snapshot has never
// emitted, so every source showed as "ok" even with its circuit open. These
// cases are keyed to what Snapshot actually publishes; the circuit-open case
// fails against the old "state" lookup.
func TestSourceStatus(t *testing.T) {
	tests := []struct {
		name string
		src  map[string]interface{}
		want string
	}{
		{
			name: "circuit open reports degraded",
			src:  map[string]interface{}{"health": map[string]interface{}{"circuit_open": true}},
			want: "degraded",
		},
		{
			name: "circuit closed reports ok",
			src:  map[string]interface{}{"health": map[string]interface{}{"circuit_open": false}},
			want: "ok",
		},
		{
			name: "no health entry reports ok",
			src:  map[string]interface{}{"name": "audiobookbay"},
			want: "ok",
		},
		{
			name: "health of the wrong type reports ok rather than panicking",
			src:  map[string]interface{}{"health": "not-a-map"},
			want: "ok",
		},
		{
			name: "circuit_open of the wrong type reports ok rather than panicking",
			src:  map[string]interface{}{"health": map[string]interface{}{"circuit_open": "true"}},
			want: "ok",
		},
		{
			name: "a stale state key alone does not make a healthy source degraded",
			src:  map[string]interface{}{"health": map[string]interface{}{"state": "open", "circuit_open": false}},
			want: "ok",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceStatus(tc.src); got != tc.want {
				t.Errorf("sourceStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Guards the contract between the two packages: if Snapshot ever renames
// circuit_open, this fails instead of the dashboard silently going quiet again.
func TestSourceStatusMatchesHealthTrackerKeys(t *testing.T) {
	tracker := search.NewHealthTracker(1, 300)
	tracker.RecordFailure("audiobookbay", "all AudioBookBay domains failed", "search")

	snapshot := tracker.Snapshot()
	entry, ok := snapshot["audiobookbay"]
	if !ok {
		t.Fatalf("expected a health entry for audiobookbay, got %v", snapshot)
	}
	if _, ok := entry["circuit_open"]; !ok {
		t.Fatalf("HealthTracker.Snapshot no longer publishes circuit_open: %v", entry)
	}

	if got := sourceStatus(map[string]interface{}{"health": entry}); got != "degraded" {
		t.Errorf("a source with an open circuit reported %q, want \"degraded\"", got)
	}
}

// GET /api/sources must report the endpoints the running process resolved, not
// the registry as published. This drives the real config.Load (registry file ->
// ApplyEnvOverrides -> Config) into real drivers and reads the handler's JSON,
// so a break anywhere along that chain shows up here. It exists because the
// only way to confirm AUDIOBOOKBAY_MIRRORS took effect used to be a throwaway
// container and its logs.
func TestHandleSourcesReportsResolvedEndpoints(t *testing.T) {
	const registryJSON = `{
  "version": 1,
  "annas": {"domain": "annas-registry.example"},
  "audiobookbay": {
    "mirrors": ["registry-first.example", "registry-second.example"],
    "trackers": ["udp://tracker.example:1337/TRACKERSECRET/announce?passkey=PASSKEYSECRET"]
  }
}`

	cases := []struct {
		name          string
		mirrorsEnv    string
		annasEnv      string
		wantMirrors   []string
		wantAnnasHost string
	}{
		{
			name:          "registry values when no env override is set",
			wantMirrors:   []string{"registry-first.example", "registry-second.example"},
			wantAnnasHost: "annas-registry.example",
		},
		{
			name:          "AUDIOBOOKBAY_MIRRORS replaces the list and keeps its order",
			mirrorsEnv:    "z-last-alphabetically.example,a-first-alphabetically.example",
			wantMirrors:   []string{"z-last-alphabetically.example", "a-first-alphabetically.example"},
			wantAnnasHost: "annas-registry.example",
		},
		{
			name:          "ANNAS_ARCHIVE_DOMAIN wins over the registry domain",
			annasEnv:      "https://annas-env.example/search",
			wantMirrors:   []string{"registry-first.example", "registry-second.example"},
			wantAnnasHost: "annas-env.example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registryPath := filepath.Join(dir, "sources.json")
			if err := os.WriteFile(registryPath, []byte(registryJSON), 0o600); err != nil {
				t.Fatalf("write registry: %v", err)
			}
			t.Setenv("LIBRARR_SOURCES_PATH", registryPath)
			t.Setenv("LIBRARR_SOURCES_URL", "")
			t.Setenv("LIBRARR_DB_PATH", filepath.Join(dir, "librarr.db"))
			t.Setenv("SETTINGS_FILE", filepath.Join(dir, "settings.json"))
			t.Setenv("AUDIOBOOKBAY_MIRRORS", tc.mirrorsEnv)
			t.Setenv("ANNAS_ARCHIVE_DOMAIN", tc.annasEnv)

			cfg := config.Load()
			client := &http.Client{}
			mgr := search.NewManager(cfg, []search.Searcher{
				search.NewAnnasArchive(cfg, client),
				search.NewAudioBookBay(cfg, client),
			}, search.NewHealthTracker(3, 300))
			s := &Server{cfg: cfg, searchMgr: mgr}

			rr := httptest.NewRecorder()
			s.handleSources(rr, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
			}

			var entries []struct {
				Name         string   `json:"name"`
				Label        string   `json:"label"`
				Enabled      bool     `json:"enabled"`
				SearchTab    string   `json:"search_tab"`
				DownloadType string   `json:"download_type"`
				Endpoints    []string `json:"endpoints"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
				t.Fatalf("response is not the expected array: %v\n%s", err, rr.Body.String())
			}
			byName := map[string][]string{}
			for _, e := range entries {
				byName[e.Name] = e.Endpoints
			}

			if got := byName["audiobookbay"]; !reflect.DeepEqual(got, tc.wantMirrors) {
				t.Errorf("audiobookbay endpoints = %v, want %v", got, tc.wantMirrors)
			}
			if got := byName["annas"]; !reflect.DeepEqual(got, []string{tc.wantAnnasHost}) {
				t.Errorf("annas endpoints = %v, want [%s]", got, tc.wantAnnasHost)
			}

			// A tracker passkey lives next to the mirrors in the registry; it must
			// not appear anywhere in the response.
			for _, secret := range []string{"PASSKEYSECRET", "TRACKERSECRET", "passkey", "tracker.example"} {
				if strings.Contains(rr.Body.String(), secret) {
					t.Errorf("response leaks %q: %s", secret, rr.Body.String())
				}
			}
		})
	}
}
