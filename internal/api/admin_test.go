package api

import (
	"testing"

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
