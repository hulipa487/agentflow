package docparse

import (
	"html"
	"regexp"
	"strings"
)

var (
	// Drop script/style blocks entirely (content is not document text).
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	// Block-level boundaries become newlines before tags are stripped.
	blockRe = regexp.MustCompile(`(?i)</(p|div|tr|table|ul|ol|li|h[1-6]|blockquote|section|article|br|hr)>\s*|<\s*(br|hr|p|div|tr|li|blockquote|h[1-6])[^>]*>`)
	tagRe   = regexp.MustCompile(`<[^>]*>`)
)

// stripTags converts HTML to rough plain text: scripts/styles removed, block
// boundaries become newlines, remaining tags dropped, entities decoded.
func stripTags(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// Collapse horizontal whitespace per line; keep the newlines.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.Join(strings.Fields(ln), " ")
	}
	return strings.Join(lines, "\n")
}
