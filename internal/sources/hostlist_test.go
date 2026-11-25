package sources_test

import (
	"slices"
	"testing"

	"github.com/JeremiahM37/librarr/internal/sources"
)

// HostList is the gate between configured endpoints and GET /api/sources, so
// the credential cases matter more than the tidy-up ones.
func TestHostList(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing in, nothing out", nil, nil},
		{"blank and whitespace are skipped", []string{"", "   "}, nil},
		{"hostname passes through lower-cased", []string{"Example.COM"}, []string{"example.com"}},
		{"url is reduced to its host", []string{"https://example.com/a/b?c=d#e"}, []string{"example.com"}},
		{"port is kept, it is part of the endpoint", []string{"https://example.com:8443/x"}, []string{"example.com:8443"}},
		{"order is preserved", []string{"b.test", "a.test", "c.test"}, []string{"b.test", "a.test", "c.test"}},
		{"duplicates collapse to the first position", []string{"a.test", "b.test", "https://A.test/x", "b.test"}, []string{"a.test", "b.test"}},
		{"query-string secret never survives", []string{"https://example.com/search?api_key=SECRET"}, []string{"example.com"}},
		{"userinfo with password is dropped, not stripped", []string{"https://user:SECRET@example.com/"}, nil},
		{"userinfo without scheme is dropped", []string{"user:SECRET@example.com"}, nil},
		{"username only is dropped", []string{"https://token@example.com"}, nil},
		{"a bad entry does not hide the good ones", []string{"user:SECRET@bad.test", "good.test"}, []string{"good.test"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sources.HostList(tc.in...)
			if !slices.Equal(got, tc.want) {
				t.Errorf("HostList(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
