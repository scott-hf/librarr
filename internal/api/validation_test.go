package api

import (
	"strings"
	"testing"
)

func TestSanitizeSearchQuery(t *testing.T) {
	short := "hello world"
	if got := sanitizeSearchQuery(short); got != short {
		t.Errorf("short query changed: %q", got)
	}
	long := strings.Repeat("x", 600)
	got := sanitizeSearchQuery(long)
	if len([]rune(got)) != maxSearchQueryRunes {
		t.Errorf("expected %d runes, got %d", maxSearchQueryRunes, len([]rune(got)))
	}
}

func TestSanitizeSearchQueryTrimsWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"leading spaces", "   Tolkien", "Tolkien"},
		{"trailing spaces", "Tolkien   ", "Tolkien"},
		{"both ends", "  Tolkien  ", "Tolkien"},
		{"inner whitespace preserved", "  the  hobbit  ", "the  hobbit"},
		{"empty", "", ""},
		{"spaces only", "     ", ""},
		{"tabs and newlines only", "\t\n\r \t", ""},
		{"tabs and newlines around", "\t\nDune\r\n", "Dune"},
		{"unicode space around", " 　作者测试 ", "作者测试"},
		{"unicode preserved", "作者测试", "作者测试"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSearchQuery(tc.in); got != tc.want {
				t.Errorf("sanitizeSearchQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSearchQueryTruncatesPaddedQueryToTrimmedResult(t *testing.T) {
	// Padded long query: leading whitespace is trimmed before truncation, so
	// the result is a full maxSearchQueryRunes of content with no padding.
	padded := "   " + strings.Repeat("x", 600) + "   "
	got := sanitizeSearchQuery(padded)
	if want := strings.Repeat("x", maxSearchQueryRunes); got != want {
		t.Errorf("padded long query: got %d runes (%q...), want %d x's", len([]rune(got)), got[:min(len(got), 10)], maxSearchQueryRunes)
	}

	// Truncation lands on a space: the cut must not leave a trailing space.
	cut := strings.Repeat("x", maxSearchQueryRunes-1) + " " + strings.Repeat("y", 50)
	got = sanitizeSearchQuery(cut)
	if want := strings.Repeat("x", maxSearchQueryRunes-1); got != want {
		t.Errorf("cut-at-space query not re-trimmed: got %d runes, trailing %q", len([]rune(got)), got[max(0, len(got)-3):])
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("result has surrounding whitespace: %q", got[max(0, len(got)-3):])
	}
}

func TestValidateMD5(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef"
	if !validateMD5(valid) {
		t.Error("expected valid md5")
	}
	if validateMD5("tooshort") {
		t.Error("expected invalid short md5")
	}
	if validateMD5(strings.Repeat("g", 32)) {
		t.Error("expected invalid hex md5")
	}
}
