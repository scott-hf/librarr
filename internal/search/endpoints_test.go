package search

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/sources"
)

// These tests use only API that existed before the endpoints change, so they
// compile and RUN against the old code. That is deliberate: it lets them be
// shown failing there (negative control) instead of failing to build.

func metaFor(t *testing.T, cfg *config.Config, srcs ...Searcher) map[string]map[string]interface{} {
	t.Helper()
	mgr := NewManager(cfg, srcs, NewHealthTracker(3, 300))
	out := make(map[string]map[string]interface{})
	for _, e := range mgr.SourceMeta() {
		out[e["name"].(string)] = e
	}
	return out
}

// endpointsOf reads "endpoints" back the way an API client sees it: through a
// JSON round trip, so a wrong type fails here and not only in a browser.
func endpointsOf(t *testing.T, entry map[string]interface{}) ([]string, bool) {
	t.Helper()
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var decoded struct {
		Endpoints *[]string `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("endpoints is not a list of strings: %v (entry %s)", err, raw)
	}
	if decoded.Endpoints == nil {
		return nil, false
	}
	return *decoded.Endpoints, true
}

func TestSourceMetaEndpoints_AudioBookBayFollowsResolvedRegistryInOrder(t *testing.T) {
	// The registry says one thing; AUDIOBOOKBAY_MIRRORS replaces it. The endpoint
	// list must show the replacement, in the operator's order (failover order),
	// de-duplicated and host-only.
	reg := &sources.Registry{}
	reg.AudioBookBay.Mirrors = []string{"registry-a.example", "registry-b.example"}
	reg.ApplyEnvOverrides(func(k string) string {
		if k == "AUDIOBOOKBAY_MIRRORS" {
			return " Zeta.example , https://alpha.example:8443/path?x=1,zeta.example,,beta.example "
		}
		return ""
	})
	cfg := &config.Config{Sources: reg}

	meta := metaFor(t, cfg, NewAudioBookBay(cfg, &http.Client{}))
	got, ok := endpointsOf(t, meta["audiobookbay"])
	if !ok {
		t.Fatalf("audiobookbay entry has no endpoints key: %v", meta["audiobookbay"])
	}
	want := []string{"zeta.example", "alpha.example:8443", "beta.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %v, want %v (resolved override, operator order)", got, want)
	}
}

func TestSourceMetaEndpoints_AudioBookBayWithoutOverrideShowsRegistryList(t *testing.T) {
	reg := &sources.Registry{}
	reg.AudioBookBay.Mirrors = []string{"registry-b.example", "registry-a.example"}
	reg.ApplyEnvOverrides(func(string) string { return "" })
	cfg := &config.Config{Sources: reg}

	meta := metaFor(t, cfg, NewAudioBookBay(cfg, &http.Client{}))
	got, _ := endpointsOf(t, meta["audiobookbay"])
	want := []string{"registry-b.example", "registry-a.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %v, want %v", got, want)
	}
}

func TestSourceMetaEndpoints_ExistingKeysAreUnchanged(t *testing.T) {
	reg := &sources.Registry{}
	reg.AudioBookBay.Mirrors = []string{"abb.example"}
	cfg := &config.Config{Sources: reg}

	entry := metaFor(t, cfg, NewAudioBookBay(cfg, &http.Client{}))["audiobookbay"]
	checks := map[string]interface{}{
		"name":          "audiobookbay",
		"label":         "AudioBookBay",
		"enabled":       true,
		"search_tab":    "audiobook",
		"download_type": "torrent",
	}
	for k, want := range checks {
		if got, ok := entry[k]; !ok || got != want {
			t.Errorf("entry[%q] = %v (present=%v), want %v", k, got, ok, want)
		}
	}
	// Only the one additive key may differ from the old shape.
	if _, ok := entry["endpoints"]; !ok {
		t.Fatalf("expected the additive endpoints key, got %v", entry)
	}
	if len(entry) != len(checks)+1 {
		t.Errorf("entry has keys %v, want exactly the 5 original keys plus endpoints", entry)
	}
}

// Returned slices must not alias the live registry: a caller (or the JSON
// encoder's consumer) that edits the list must not rewrite the failover order.
func TestSourceMetaEndpoints_DoesNotAliasRegistry(t *testing.T) {
	reg := &sources.Registry{}
	reg.AudioBookBay.Mirrors = []string{"one.example", "two.example"}
	cfg := &config.Config{Sources: reg}
	abb := NewAudioBookBay(cfg, &http.Client{})

	first := metaFor(t, cfg, abb)["audiobookbay"]["endpoints"]
	eps, ok := first.([]string)
	if !ok {
		t.Fatalf("endpoints = %#v, want []string", first)
	}
	eps[0] = "mutated.example"

	if reg.AudioBookBay.Mirrors[0] != "one.example" {
		t.Fatalf("editing the returned endpoints rewrote the registry: %v", reg.AudioBookBay.Mirrors)
	}
	again, _ := endpointsOf(t, metaFor(t, cfg, abb)["audiobookbay"])
	if !reflect.DeepEqual(again, []string{"one.example", "two.example"}) {
		t.Errorf("second read = %v, want the original list", again)
	}
}

// Every registry-backed driver reports host-only endpoints taken from the
// resolved config it actually searches with.
func TestSourceMetaEndpoints_RegistryBackedSources(t *testing.T) {
	reg := &sources.Registry{
		ThePirateBay:   sources.URLSpec{URL: "https://apibay.example/q.php?token=SECRET"},
		Gutenberg:      sources.URLSpec{URL: "https://gutenberg.example/ebooks/search"},
		OpenLibrary:    sources.OpenLibrarySpec{SearchURL: "https://openlibrary.example/search.json", CoverURL: "https://covers.example"},
		Librivox:       sources.URLSpec{URL: "https://librivox.example/api/feed/audiobooks"},
		StandardEbooks: sources.URLSpec{URL: "https://standard.example"},
		MangaDex: sources.MangaDexSpec{
			APIURL:     "https://api.manga.example",
			UploadsURL: "https://uploads.manga.example",
			WebURL:     "https://manga.example",
		},
		Nyaa: sources.URLSpec{URL: "https://nyaa.example/?page=rss"},
		WebNovels: []sources.WebNovelSite{
			{ID: "a", URL: "https://novel-a.example/search", BaseURL: "https://novel-a.example"},
			{ID: "b", URL: "https://novel-b.example/s", BaseURL: "https://www.novel-b.example"},
		},
		ZLibraryDefault: "https://zlib-default.example/",
	}
	cfg := &config.Config{
		Sources:            reg,
		AnnasArchiveDomain: "annas.example",
		LibrivoxEnabled:    true,
	}
	client := &http.Client{}

	meta := metaFor(t, cfg,
		NewAnnasArchive(cfg, client),
		NewAnnasManga(cfg, client),
		NewThePirateBay(cfg, client, "audiobook"),
		NewGutenberg(cfg, client),
		NewOpenLibrary(cfg, client),
		NewLibrivox(cfg, client),
		NewStandardEbooks(cfg, client),
		NewMangaDex(cfg, client),
		NewNyaaManga(cfg, client),
		NewWebNovel(cfg, client),
		NewZLibrary(cfg, client),
	)

	want := map[string][]string{
		"annas":          {"annas.example"},
		"annas_manga":    {"annas.example"},
		"tpb_audiobook":  {"apibay.example"},
		"gutenberg":      {"gutenberg.example"},
		"openlibrary":    {"openlibrary.example", "covers.example"},
		"librivox":       {"librivox.example"},
		"standardebooks": {"standard.example"},
		"mangadex":       {"api.manga.example", "uploads.manga.example", "manga.example"},
		"nyaa_manga":     {"nyaa.example"},
		"webnovel":       {"novel-a.example", "novel-b.example", "www.novel-b.example"},
		"zlibrary":       {"zlib-default.example"},
	}
	for name, wantHosts := range want {
		entry, ok := meta[name]
		if !ok {
			t.Errorf("%s: no entry in SourceMeta", name)
			continue
		}
		got, present := endpointsOf(t, entry)
		if !present {
			t.Errorf("%s: no endpoints key", name)
			continue
		}
		if !reflect.DeepEqual(got, wantHosts) {
			t.Errorf("%s: endpoints = %v, want %v", name, got, wantHosts)
		}
	}
}

func TestSourceMetaEndpoints_ZLibraryUserURLWinsOverRegistryDefault(t *testing.T) {
	cfg := &config.Config{
		Sources:     &sources.Registry{ZLibraryDefault: "https://zlib-default.example"},
		ZLibraryURL: "https://zlib-mine.example/",
	}
	got, _ := endpointsOf(t, metaFor(t, cfg, NewZLibrary(cfg, &http.Client{}))["zlibrary"])
	if !reflect.DeepEqual(got, []string{"zlib-mine.example"}) {
		t.Errorf("endpoints = %v, want the user ZLIBRARY_URL host, as apiBase resolves it", got)
	}
}

// Nothing configured -> the key is absent, not an empty list.
func TestSourceMetaEndpoints_OmittedWhenNothingConfigured(t *testing.T) {
	cfg := &config.Config{Sources: &sources.Registry{}} // no domain, no mirrors, no URLs
	client := &http.Client{}

	meta := metaFor(t, cfg,
		NewAnnasArchive(cfg, client),
		NewAudioBookBay(cfg, client),
		NewGutenberg(cfg, client),
		NewWebNovel(cfg, client),
		stubSearcher{name: "plain", tab: "main"}, // not an EndpointReporter at all
	)
	for name, entry := range meta {
		if _, has := entry["endpoints"]; has {
			t.Errorf("%s: endpoints present with nothing configured: %v", name, entry["endpoints"])
		}
	}
	if len(meta) != 5 {
		t.Errorf("got %d entries, want 5", len(meta))
	}
}

// A Config built without a registry (several callers and tests do this) must
// not panic SourceMeta.
func TestSourceMetaEndpoints_NilRegistryDoesNotPanic(t *testing.T) {
	cfg := &config.Config{}
	client := &http.Client{}
	meta := metaFor(t, cfg,
		NewGutenberg(cfg, client),
		NewOpenLibrary(cfg, client),
		NewLibrivox(cfg, client),
		NewStandardEbooks(cfg, client),
		NewMangaDex(cfg, client),
		NewNyaaManga(cfg, client),
		NewWebNovel(cfg, client),
		NewThePirateBay(cfg, client, "main"),
		NewZLibrary(cfg, client),
		NewAnnasArchive(cfg, client),
		NewAnnasManga(cfg, client),
	)
	if len(meta) != 11 {
		t.Errorf("got %d entries, want 11", len(meta))
	}
}

// Whatever the config holds, nothing credential-bearing may reach the response.
// The whole SourceMeta payload is serialized and searched, so a leak through any
// key (not just endpoints) is caught.
func TestSourceMetaEndpoints_NeverLeaksCredentials(t *testing.T) {
	reg := &sources.Registry{
		ThePirateBay:    sources.URLSpec{URL: "https://tpbuser:TPBSECRET@apibay.example/q.php"},
		Gutenberg:       sources.URLSpec{URL: "https://gutenberg.example/search?api_key=QUERYSECRET#FRAGSECRET"},
		Nyaa:            sources.URLSpec{URL: "https://nyaa.example/PATHSECRET/rss"},
		ZLibraryDefault: "https://zl.example",
	}
	reg.AudioBookBay.Mirrors = []string{"abb.example", "mirroruser:MIRRORSECRET@evil.example"}
	reg.AudioBookBay.Trackers = []string{"udp://tracker.example:1337/TRACKERSECRET/announce?passkey=PASSKEYSECRET"}

	cfg := &config.Config{
		Sources:            reg,
		AnnasArchiveDomain: "annasuser:ANNASSECRET@annas.example",
		ZLibraryURL:        "https://zluser:ZLSECRET@zl-user.example/personal?remix_userkey=ZLKEYSECRET",
		ZLibraryEmail:      "reader@example.com",
		ZLibraryPassword:   "ZLPASSWORDSECRET",
		ProwlarrURL:        "http://prowlarr-internal:9696",
		ProwlarrAPIKey:     "PROWLARRKEYSECRET",
		BookTrackerURL:     "https://bt.example",
		BookTrackerUser:    "btuser",
		BookTrackerPass:    "BTPASSSECRET",
	}
	client := &http.Client{}
	mgr := NewManager(cfg, []Searcher{
		NewAnnasArchive(cfg, client),
		NewAudioBookBay(cfg, client),
		NewThePirateBay(cfg, client, "main"),
		NewGutenberg(cfg, client),
		NewNyaaManga(cfg, client),
		NewZLibrary(cfg, client),
	}, NewHealthTracker(3, 300))

	raw, err := json.Marshal(mgr.SourceMeta())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, secret := range []string{
		"TPBSECRET", "QUERYSECRET", "FRAGSECRET", "PATHSECRET", "MIRRORSECRET", "TRACKERSECRET",
		"PASSKEYSECRET", "ANNASSECRET", "ZLSECRET", "ZLKEYSECRET", "ZLPASSWORDSECRET",
		"PROWLARRKEYSECRET", "BTPASSSECRET", "btuser", "reader@example.com", "personal", "prowlarr-internal",
		"evil.example", "tpbuser", "zluser", "annasuser", "mirroruser", "passkey", "api_key",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("SourceMeta leaked %q: %s", secret, body)
		}
	}

	// The safe parts must still come through, or the test proves nothing.
	meta := map[string]map[string]interface{}{}
	for _, e := range mgr.SourceMeta() {
		meta[e["name"].(string)] = e
	}
	if got, _ := endpointsOf(t, meta["audiobookbay"]); !reflect.DeepEqual(got, []string{"abb.example"}) {
		t.Errorf("audiobookbay endpoints = %v, want only the clean mirror", got)
	}
	if got, _ := endpointsOf(t, meta["gutenberg"]); !reflect.DeepEqual(got, []string{"gutenberg.example"}) {
		t.Errorf("gutenberg endpoints = %v, want host only", got)
	}
	if got, _ := endpointsOf(t, meta["nyaa_manga"]); !reflect.DeepEqual(got, []string{"nyaa.example"}) {
		t.Errorf("nyaa endpoints = %v, want host only", got)
	}
	// Credential-bearing values are dropped, not stripped.
	for _, name := range []string{"annas", "tpb", "zlibrary"} {
		if _, has := meta[name]["endpoints"]; has {
			t.Errorf("%s: endpoints present for a value carrying userinfo: %v", name, meta[name]["endpoints"])
		}
	}
}
