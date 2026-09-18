package tools

import (
	"context"
	"fmt"
	"strings"

	"agentflow/internal/drivers/legal"
)

// RegisterLegalBuiltins adds the legal-database tools (builtin:legal_search and
// builtin:legal_read) backed by the configured legal engines (config.Legal).
// With no engine configured both report honest-unavailable rather than failing.
//
// The descriptions are assembled from the engines that are actually configured,
// because hklii and npc answer differently in ways a shared description would
// get wrong. hklii returns Hong Kong case law and legislation with citations,
// parties and coram, and reads a judgment by lang/abbr/year/num; npc returns
// PRC legislation with a validity status and an enacting organ, and reads one
// document by an opaque id that abbr/year/num have no meaning for. So each
// engine's facts are stated only when that engine is present, and a parameter an
// engine silently ignores is only offered when an engine that honours it is
// present — a model told about `abbr` on an npc-only deployment would use it and
// get nothing back.
//
// legal_read is named for what it does — retrieve one document's full text —
// rather than legal_fetch, which read as an HTTP fetch and collided with
// builtin:fetch.
func RegisterLegalBuiltins(r *Registry, legalSet *legal.Set) {
	names := legalSet.Names()
	configured := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	hasHKLII, hasNPC := configured("hklii"), configured("npc")

	engineProp := func(props map[string]any, desc *string, what string) {
		if legalSet.Empty() {
			*desc = what + ". Returns honest unavailable if no legal engine is configured."
			return
		}
		props["engine"] = map[string]any{
			"type":        "string",
			"enum":        names,
			"description": fmt.Sprintf("Legal engine (default %q when omitted)", legalSet.Default),
		}
		*desc = fmt.Sprintf("%s. engine selects the backend (%s); omitted uses the default (%q).",
			what, strings.Join(names, " | "), legalSet.Default)
	}

	// --- builtin:legal_search ---
	searchFacts := make([]string, 0, 2)
	if hasHKLII {
		searchFacts = append(searchFacts,
			"hklii searches Hong Kong case law and legislation; hits carry a neutral and parallel citation, court, date, parties and coram.")
	}
	if hasNPC {
		searchFacts = append(searchFacts,
			"npc searches PRC national laws, administrative regulations and judicial interpretations; hits carry the enacting organ as `court`, the promulgation date as `date`, and the legal nature, effective date and validity as `type`, `effective` and `status`. npc has no citations and no parties — those fields come back empty.")
	}
	searchWhat := "Search a legal database."
	if len(searchFacts) > 0 {
		searchWhat += " " + strings.Join(searchFacts, " ")
	}
	searchWhat += " Read a hit's full text with builtin:legal_read, passing the hit's `path`."

	fuzzyDesc := "true for exact keyword matching; false (default) allows fuzzy/concept matching"
	switch {
	case hasHKLII && hasNPC:
		fuzzyDesc += ". The two engines differ: on hklii this is keyword versus concept; on npc it matches the title only instead of the full text"
	case hasNPC:
		fuzzyDesc = "true to match the title only; false (default) searches the full text"
	}

	searchDesc := ""
	searchProps := map[string]any{
		"query": map[string]any{"type": "string", "description": "Free-text legal query: party names, a citation, or concepts (e.g. \"HKSAR v Chu\", \"offensive weapon sentencing\")"},
		"count": map[string]any{"type": "number", "description": "Max results (default 10; each engine clamps to its own ceiling)."},
	}
	if !legalSet.Empty() {
		searchProps["disable_fuzzy"] = map[string]any{"type": "boolean", "description": fuzzyDesc}
	}
	engineProp(searchProps, &searchDesc, searchWhat)
	r.Register(ToolSpec{
		Name:        "builtin:legal_search",
		Description: searchDesc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": searchProps,
			"required":   []string{"query"},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if legalSet.Empty() {
				return ResultUnavailable("builtin:legal_search", "No legal engine is configured."), nil
			}
			query, _ := args["query"].(string)
			disableFuzzy, _ := args["disable_fuzzy"].(bool)
			engine, _ := args["engine"].(string)
			used, res, err := legalSet.Search(ctx, engine, legal.Request{
				Query:        query,
				Count:        argCount(args["count"]),
				DisableFuzzy: disableFuzzy,
			})
			if err != nil {
				return nil, fmt.Errorf("builtin:legal_search: %w", err)
			}
			return map[string]any{
				"ok":           true,
				"tool":         "builtin:legal_search",
				"engine":       used,
				"query":        res.Query,
				"count":        len(res.Results),
				"total":        res.Total,
				"results":      res.Results,
				"time_cost_ms": res.TimeCostMs,
			}, nil
		},
	})

	// --- builtin:legal_read ---
	readFacts := make([]string, 0, 2)
	if hasHKLII {
		readFacts = append(readFacts,
			"hklii reads a case judgment: `path` is /{lang}/cases/{abbr}/{year}/{num}, or give `abbr`+`year`+`num` with an optional `lang`. Only case judgments can be read — a legislation hit, one whose `court` reads \"Hong Kong Ordinances\", has no readable full text here and its path is rejected.")
	}
	if hasNPC {
		readFacts = append(readFacts,
			"npc reads one law or regulation by `path`, the opaque document id a hit carries. `abbr`, `year`, `num` and `lang` have no meaning for npc, and the `neutral` it returns is that same id rather than a citation.")
	}
	readWhat := "Read one document's full text, keyed by the `path` on a legal_search hit."
	if len(readFacts) > 0 {
		readWhat += " " + strings.Join(readFacts, " ")
	}
	readWhat += " Long bodies can be read in parts with `start` and capped with `max_chars`."

	pathDesc := "The `path` from a legal_search hit."
	switch {
	case hasHKLII && hasNPC:
		pathDesc = "The `path` from a legal_search hit. hklii: /en/cases/hkcfi/2020/1808. npc: an opaque document id."
	case hasHKLII:
		pathDesc = "Case path from a legal_search hit, e.g. /en/cases/hkcfi/2020/1808."
	case hasNPC:
		pathDesc = "The `path` from a legal_search hit — npc's opaque document id."
	}

	readProps := map[string]any{
		"path":      map[string]any{"type": "string", "description": pathDesc},
		"start":     map[string]any{"type": "number", "description": "Byte offset into the text body, for reading long documents in parts (default 0). Setting it marks the result truncated."},
		"max_chars": map[string]any{"type": "number", "description": "Cap on returned text bytes (default: no cap)"},
	}
	if hasHKLII {
		// Offered only when hklii is configured. npc's Fetch reads none of
		// these, and advertising a parameter an engine discards is an
		// invitation to rely on it.
		readProps["abbr"] = map[string]any{"type": "string", "description": "hklii only: court/database abbreviation, e.g. hkcfi (with year and num, if not using path)"}
		readProps["year"] = map[string]any{"type": "string", "description": "hklii only: case year, e.g. 2020"}
		readProps["num"] = map[string]any{"type": "string", "description": "hklii only: case number within the year, e.g. 1808"}
		readProps["lang"] = map[string]any{"type": "string", "enum": []string{"en", "tc", "sc"}, "description": "hklii only: language version (default: the path's language, else en). Not every judgment has every language."}
	}

	readDesc := ""
	engineProp(readProps, &readDesc, readWhat)
	r.Register(ToolSpec{
		Name:        "builtin:legal_read",
		Description: readDesc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": readProps,
			"required":   []string{},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if legalSet.Empty() {
				return ResultUnavailable("builtin:legal_read", "No legal engine is configured."), nil
			}
			engine, _ := args["engine"].(string)
			path, _ := args["path"].(string)
			abbr, _ := args["abbr"].(string)
			year, _ := args["year"].(string)
			num, _ := args["num"].(string)
			lang, _ := args["lang"].(string)
			used, j, err := legalSet.Fetch(ctx, engine, legal.FetchRequest{
				Path:     path,
				Lang:     lang,
				Abbr:     abbr,
				Year:     year,
				Num:      num,
				Start:    argCount(args["start"]),
				MaxChars: argCount(args["max_chars"]),
			})
			if err != nil {
				return nil, fmt.Errorf("builtin:legal_read: %w", err)
			}
			out := map[string]any{
				"ok":       true,
				"tool":     "builtin:legal_read",
				"engine":   used,
				"text":     j.Text,
				"text_len": j.TextLen,
			}
			// Only the fields this engine actually fills. npc returns no court,
			// date, parallel citation or translation flag, and an empty string
			// in the result reads as "unknown" rather than "this engine does
			// not have it" — which is the wrong conclusion to invite.
			for key, val := range map[string]string{
				"title":   j.Title,
				"neutral": j.Neutral,
				"court":   j.Court,
				"date":    j.Date,
				"doc":     j.DocURL,
			} {
				if val != "" {
					out[key] = val
				}
			}
			if len(j.Parallel) > 0 {
				out["parallel_citation"] = j.Parallel
			}
			if j.Truncated {
				out["truncated"] = true
			}
			if j.HasTranslation {
				out["has_translation"] = true
			}
			return out, nil
		},
	})
}
