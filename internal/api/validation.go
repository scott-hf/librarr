package api

import (
	"strings"
	"unicode/utf8"
)

const maxSearchQueryRunes = 512

// sanitizeSearchQuery trims surrounding whitespace and caps search query
// length to prevent abuse. Whitespace-only input yields "" so callers'
// empty-query guards fire. Truncation can leave a trailing space, hence the
// second trim.
func sanitizeSearchQuery(q string) string {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) <= maxSearchQueryRunes {
		return q
	}
	runes := []rune(q)
	return strings.TrimSpace(string(runes[:maxSearchQueryRunes]))
}

// validateMD5 checks Anna's Archive MD5 hash format.
func validateMD5(md5 string) bool {
	if len(md5) != 32 {
		return false
	}
	for _, c := range md5 {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}
