package execguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	maxRedactDepth = 6
	maxRedactKeys  = 32
	maxRedactItems = 32
	maxRedactRunes = 256
)

var secretKeyRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|authorization|cookie|private[_-]?key|credential)`)

// CanonicalJSON deterministically serializes v with sorted map keys and returns
// both the canonical bytes and their SHA-256 hex digest.
func CanonicalJSON(v any) ([]byte, string) {
	norm := canonicalizeValue(v)
	b, err := json.Marshal(norm)
	if err != nil {
		b = []byte("null")
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func canonicalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = canonicalizeValue(val)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = val
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = canonicalizeValue(item)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = item
		}
		return out
	case bool, int, int64, uint64, float64, string, json.Number:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		var decoded any
		if err := json.Unmarshal(b, &decoded); err != nil {
			return string(b)
		}
		return canonicalizeValue(decoded)
	}
}

// RedactArgs clones args for human-facing approval previews, masking secret-like
// keys and bounding depth, width, and string lengths so untrusted payloads
// stay readable and safe to render.
func RedactArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out, _ := redactValue(args, "", 0).(map[string]any)
	return out
}

func redactValue(v any, keyName string, depth int) any {
	if keyName != "" && secretKeyRe.MatchString(keyName) {
		return "[REDACTED]"
	}
	if depth > maxRedactDepth {
		return "[TRUNCATED]"
	}
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return truncateRunes(x, maxRedactRunes)
	case map[string]any:
		return redactMap(x, depth)
	case map[string]string:
		conv := make(map[string]any, len(x))
		for k, val := range x {
			conv[k] = val
		}
		return redactMap(conv, depth)
	case []any:
		limit := len(x)
		if limit > maxRedactItems {
			limit = maxRedactItems
		}
		out := make([]any, 0, limit)
		for i := 0; i < limit; i++ {
			out = append(out, redactValue(x[i], "", depth+1))
		}
		return out
	case []string:
		limit := len(x)
		if limit > maxRedactItems {
			limit = maxRedactItems
		}
		out := make([]any, 0, limit)
		for i := 0; i < limit; i++ {
			out = append(out, truncateRunes(x[i], maxRedactRunes))
		}
		return out
	default:
		return x
	}
}

func redactMap(m map[string]any, depth int) map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, min(len(keys), maxRedactKeys))
	for i, k := range keys {
		if i >= maxRedactKeys {
			out["_truncated_keys"] = len(keys) - maxRedactKeys
			break
		}
		out[k] = redactValue(m[k], k, depth+1)
	}
	return out
}

func truncateRunes(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
