package tools

import (
	"context"
	"fmt"
	"strings"

	"agentflow/internal/drivers/legal"
)

// RegisterLegalBuiltins adds the legal-database tools (builtin:legal_search and
// builtin:legal_fetch) backed by the configured legal engines (config.Legal).
// With no engine configured both report honest-unavailable rather than failing.
func RegisterLegalBuiltins(r *Registry, legalSet *legal.Set) {
	engineProp := func(props map[string]any, desc *string, what string) {
		if legalSet.Empty() {
			*desc = what + ". Returns honest unavailable if no legal engine is configured."
			return
		}
		names := legalSet.Names()
		props["engine"] = map[string]any{
			"type":        "string",
			"enum":        names,
			"description": fmt.Sprintf("Legal engine (default %q when omitted)", legalSet.Default),
		}
		*desc = fmt.Sprintf("%s. engine selects the backend (%s); omitted uses the default (%q).",
			what, strings.Join(names, " | "), legalSet.Default)
	}

	// --- builtin:legal_search ---
	searchDesc := ""
	searchProps := map[string]any{
		"query":         map[string]any{"type": "string", "description": "Free-text legal query: party names, a citation, or concepts (e.g. \"HKSAR v Chu\", \"offensive weapon sentencing\")"},
		"count":         map[string]any{"type": "number", "description": "Max results (default 10)"},
		"disable_fuzzy": map[string]any{"type": "boolean", "description": "true for exact keyword match; false (default) allows fuzzy/concept match"},
	}
	engineProp(searchProps, &searchDesc,
		"Search a legal database (HKLII: Hong Kong case law and legislation). Returns matching cases/legislation with citations, court, date, and parties — fetch the full text of a hit with builtin:legal_fetch using its `path`")
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

	// --- builtin:legal_fetch ---
	fetchDesc := ""
	fetchProps := map[string]any{
		"path":      map[string]any{"type": "string", "description": "Case path from a legal_search hit, e.g. /en/cases/hkcfi/2020/1808 (preferred)"},
		"abbr":      map[string]any{"type": "string", "description": "Court/database abbreviation, e.g. hkcfi (with year and num, if not using path)"},
		"year":      map[string]any{"type": "string", "description": "Case year, e.g. 2020"},
		"num":       map[string]any{"type": "string", "description": "Case number within the year, e.g. 1808"},
		"lang":      map[string]any{"type": "string", "enum": []string{"en", "tc", "sc"}, "description": "Language version (default: the path's language, else en). Not every judgment has every language."},
		"start":     map[string]any{"type": "number", "description": "Byte offset into the text body, for reading long judgments in parts (default 0)"},
		"max_chars": map[string]any{"type": "number", "description": "Cap on returned text bytes (default: no cap)"},
	}
	engineProp(fetchProps, &fetchDesc,
		"Fetch one legal judgment's full text by its path (from legal_search) or abbr/year/num")
	r.Register(ToolSpec{
		Name:        "builtin:legal_fetch",
		Description: fetchDesc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": fetchProps,
			"required":   []string{},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if legalSet.Empty() {
				return ResultUnavailable("builtin:legal_fetch", "No legal engine is configured."), nil
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
				return nil, fmt.Errorf("builtin:legal_fetch: %w", err)
			}
			return map[string]any{
				"ok":                true,
				"tool":              "builtin:legal_fetch",
				"engine":            used,
				"title":             j.Title,
				"neutral":           j.Neutral,
				"court":             j.Court,
				"date":              j.Date,
				"parallel_citation": j.Parallel,
				"doc":               j.DocURL,
				"text":              j.Text,
				"text_len":          j.TextLen,
				"truncated":         j.Truncated,
				"has_translation":   j.HasTranslation,
			}, nil
		},
	})
}
