package memory

import (
	"log/slog"
)

// RebindSkipped remaps store bindings that point at a backend which was
// skipped at boot (its url/password secret reference could not be resolved)
// onto a surviving backend, so a deployment that loses e.g. its pgvector
// backend still boots with the rest of its memory intact — the same
// degradation the downstream config generator used to perform by hand.
//
// Rules:
//   - A store bound to a skipped backend rebinds to a survivor, preferring
//     one whose provider features include text_search (a vector store
//     degrades to full-text search; loops feature-detect from the binding's
//     Features). Its `requires` degrades to what the fallback offers:
//     [kv, text_search] when the fallback has text_search, else [kv].
//   - No survivor at all drops the store entirely; the agent's memory
//     resolves without it.
//   - Stores on backends that were not skipped pass through untouched, and
//     with an empty skipped set the input map is returned as-is (so a config
//     with nothing skipped behaves exactly as before).
//
// Only credential-skipped backends are remapped: a genuinely unknown backend
// name (a config typo) is never in `skipped` and still fails loudly at
// ResolveStoresFor, and the loader's validation has already rejected it.
//
// feats reports a backend's provider features (memory.Registry.Features);
// survivors is the set of backend names that opened successfully. Both the
// rewritten store set and the names of stores that were dropped are returned;
// every remap and drop is logged.
func RebindSkipped(stores map[string]Store, skipped map[string]bool, survivors []string, feats func(string) []string, log *slog.Logger) (map[string]Store, []string) {
	if len(skipped) == 0 {
		return stores, nil
	}
	if log == nil {
		log = slog.Default()
	}
	out := make(map[string]Store, len(stores))
	var dropped []string
	for sname, s := range stores {
		if !skipped[s.Backend] {
			out[sname] = s
			continue
		}
		fallback, ok := pickFallback(survivors, feats)
		if !ok {
			dropped = append(dropped, sname)
			log.Warn("memory store dropped: its backend was skipped and no backend survives",
				"store", sname, "backend", s.Backend)
			continue
		}
		old := s.Backend
		s.Backend = fallback
		s.Requires = degradeRequires(feats(fallback))
		out[sname] = s
		log.Warn("memory store rebound to a surviving backend",
			"store", sname, "backend", old, "fallback", fallback, "requires", s.Requires)
	}
	return out, dropped
}

// pickFallback chooses the fallback backend for a rebound store: the first
// survivor whose provider offers text_search (so a vector store degrades to
// FTS rather than to a kv-only sink), else the first survivor.
func pickFallback(survivors []string, feats func(string) []string) (string, bool) {
	if len(survivors) == 0 {
		return "", false
	}
	for _, b := range survivors {
		for _, f := range feats(b) {
			if f == "text_search" {
				return b, true
			}
		}
	}
	return survivors[0], true
}

// degradeRequires lowers a rebound store's feature requirements to what a
// fallback backend can offer: full-text search when it has it, otherwise
// plain key/value.
func degradeRequires(features []string) []string {
	for _, f := range features {
		if f == "text_search" {
			return []string{"kv", "text_search"}
		}
	}
	return []string{"kv"}
}
