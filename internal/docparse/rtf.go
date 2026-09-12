package docparse

import (
	"fmt"
	"strings"
)

// RtfToText does a best-effort plain-text extraction from RTF: groups and
// control words are stripped, \'xx hex escapes and \uN unicode are decoded,
// and \par/\line become newlines. Font tables, stylesheets, and other header
// groups are dropped.
func RtfToText(data []byte) (string, error) {
	s := string(data)
	if !strings.HasPrefix(strings.TrimSpace(s), "{\\rtf") {
		return "", fmt.Errorf("docparse rtf: not an RTF document")
	}
	var b strings.Builder
	// skipAt is the group depth where a skippable header group (fonttbl,
	// stylesheet, …) began, or -1. Nested groups inherit the skip.
	skipAt := -1
	depth := 0
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '{':
			depth++
			if skipAt < 0 {
				rest := s[i:]
				for _, kw := range []string{"{\\fonttbl", "{\\stylesheet", "{\\colortbl", "{\\info", "{\\pict", "{\\*"} {
					if strings.HasPrefix(rest, kw) {
						skipAt = depth
						break
					}
				}
			}
			i++
		case '}':
			if depth == skipAt {
				skipAt = -1
			}
			if depth > 0 {
				depth--
			}
			i++
		case '\\':
			skip := skipAt >= 0
			// Control word or escaped char.
			j := i + 1
			if j < len(s) && (s[j] == '\\' || s[j] == '{' || s[j] == '}') {
				if !skip {
					b.WriteByte(s[j])
				}
				i += 2
				continue
			}
			if j+1 < len(s) && s[j] == '\'' { // \'xx hex escape
				if j+2 < len(s) {
					var v int
					fmt.Sscanf(s[j+1:j+3], "%02x", &v)
					if !skip {
						b.WriteString(cp1252ToUTF8([]byte{byte(v)}))
					}
					i += 4
					continue
				}
			}
			// Read the control word letters.
			start := j
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
			word := s[start:j]
			// Optional signed numeric arg.
			neg := false
			if j < len(s) && s[j] == '-' {
				neg = true
				j++
			}
			numStart := j
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			num := 0
			if numStart < j {
				fmt.Sscanf(s[numStart:j], "%d", &num)
				if neg {
					num = -num
				}
			}
			if j < len(s) && s[j] == ' ' { // a space terminates a control word
				j++
			}
			if !skip {
				switch word {
				case "par", "line":
					b.WriteByte('\n')
				case "tab":
					b.WriteByte('\t')
				case "u": // \uN unicode code point
					if num < 0 {
						num += 65536
					}
					b.WriteRune(rune(num))
					if j < len(s) && s[j] == '?' { // skip the fallback char
						j++
					}
				}
			}
			i = j
		default:
			if skipAt < 0 {
				b.WriteByte(c)
			}
			i++
		}
	}
	return normalizeText(b.String()), nil
}

// HTMLToText strips markup to plain text: block-level tags become newlines,
// <script>/<style> content is dropped, and entities are decoded.
func HTMLToText(data []byte) (string, error) {
	s := stripTags(string(data))
	return normalizeText(s), nil
}
