package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type fitBook struct {
	title string
	keys  []string
}

// sciFiPage is a trimmed copy of live Open Library data (2026-11-16,
// subject_key:"science_fiction" sorted by readinglog): every doc carries the
// bare science_fiction key; only some carry a corroborating one.
var sciFiPage = []fitBook{
	{"A Game of Thrones", []string{"epic_fantasy", "fantasy", "fiction_fantasy_epic", "high_fantasy", "science_fiction"}},
	{"Shatter Me", []string{"romance_fiction", "science_fiction"}},
	{"Nineteen Eighty-Four", []string{"dystopias", "english_science_fiction", "science_fiction"}},
	{"The Eye of the World", []string{"fantasy", "fiction_fantasy_general", "science_fiction"}},
	{"Dune", []string{"american_science_fiction", "fiction_science_fiction_general", "science_fiction"}},
	{"The Hunger Games", []string{"dystopian_fiction", "young_adult_fiction", "science_fiction"}},
	{"Fahrenheit 451", []string{"fiction_dystopian", "science_fiction"}},
	{"No Keys Book", nil},
}

var sciFiKept = []string{"Nineteen Eighty-Four", "Dune", "The Hunger Games", "Fahrenheit 451", "No Keys Book"}

func sciFiPageJSON() string {
	var docs []string
	for i, b := range sciFiPage {
		keys, _ := json.Marshal(b.keys)
		docs = append(docs, fmt.Sprintf(
			`{"key":"/works/OL%dW","title":%q,"author_name":["A"],"first_publish_year":2000,"cover_i":%d,"readinglog_count":%d,"subject_key":%s}`,
			i, b.title, 1000+i, 9000-i, keys))
	}
	return fmt.Sprintf(`{"numFound":5000,"docs":[%s]}`, strings.Join(docs, ","))
}

func titlesOf(items []Item) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	return strings.Join(out, "|")
}

// A bare science_fiction key does not make a book sci-fi: epic fantasy and YA
// romance drop out of the sci-fi Popular browse page, real sci-fi and docs
// without key data stay, and paging still follows the raw page.
func TestBrowseDropsBareScienceFictionTag(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, sciFiPageJSON() })
	c, _ := newTestClient(f)
	res := browse(c, CatalogPopular, "Science fiction", 0)
	if res.Status != StatusOK {
		t.Fatalf("status %q", res.Status)
	}
	if got, want := titlesOf(res.Items), strings.Join(sciFiKept, "|"); got != want {
		t.Errorf("titles %s, want %s", got, want)
	}
	if res.NextOffset == nil || *res.NextOffset != 20 {
		t.Errorf("next_offset %v, want 20 (paging follows the raw page)", res.NextOffset)
	}
}

func TestPopularShelfDropsBareScienceFictionTag(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, sciFiPageJSON() })
	c, _ := newTestClient(f)
	res := c.Popular(context.Background(), "Science Fiction", 20)
	if res.Status != StatusOK {
		t.Fatalf("status %q", res.Status)
	}
	if got, want := titlesOf(res.Items), strings.Join(sciFiKept, "|"); got != want {
		t.Errorf("titles %s, want %s", got, want)
	}
}

// Other subjects are never filtered, whatever keys their docs carry.
func TestBrowseOtherSubjectsKeepEveryDoc(t *testing.T) {
	f := newFakeOL(t, func(url.Values) (int, string) { return 200, sciFiPageJSON() })
	c, _ := newTestClient(f)
	res := browse(c, CatalogPopular, "fantasy", 0)
	if len(res.Items) != len(sciFiPage) {
		t.Errorf("fantasy browse kept %d of %d docs", len(res.Items), len(sciFiPage))
	}
}
