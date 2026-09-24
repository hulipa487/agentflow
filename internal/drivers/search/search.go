// Package search implements web-search backends for the builtin:web_search
// tool. A Searcher is one engine; doubao.go and ollama.go speak their
// respective APIs. The tool selects an engine per call (defaulting to config
// search.default), and with no engines configured reports honest-unavailable
// so search degrades cleanly rather than failing silently.
package search

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"agentflow/internal/config"
)

// Request is a normalized web-search request. Not every engine honors every
// field; unsupported fields are ignored rather than erroring.
type Request struct {
	Query     string // 1-100 chars, a single phrase
	Count     int    // max results (each engine clamps to its own ceiling)
	TimeRange string // OneDay|OneWeek|OneMonth|OneYear, or YYYY-MM-DD..YYYY-MM-DD
}

// WebResult is one normalized web hit. Summary/Snippet semantics are
// engine-native: doubao returns both (summary is the LLM-recommended excerpt),
// ollama fills snippet/content. Score is engine-native too — doubao is a 0..1
// relevance.
type WebResult struct {
	Title     string  `json:"title"`
	URL       string  `json:"url,omitempty"`
	Site      string  `json:"site,omitempty"`
	Summary   string  `json:"summary,omitempty"`
	Snippet   string  `json:"snippet,omitempty"`
	Content   string  `json:"content,omitempty"`
	Published string  `json:"published,omitempty"`
	Score     float64 `json:"score,omitempty"`
}

// Result is a normalized search response.
type Result struct {
	Query      string      `json:"query"`
	Engine     string      `json:"engine"`
	Results    []WebResult `json:"results"`
	TimeCostMs int64       `json:"time_cost_ms,omitempty"`
}

// Searcher runs one web search. Implementations are engine drivers.
type Searcher interface {
	Search(ctx context.Context, req Request) (*Result, error)
}

// Set is the named collection of configured engines plus the default used
// when a call omits `engine`.
type Set struct {
	Engines map[string]Searcher
	Default string
}

// Names returns the configured engine names, sorted, for tool descriptions.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Engines))
	for n := range s.Engines {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Empty reports whether no engine is configured (the tool then reports
// honest-unavailable).
func (s *Set) Empty() bool {
	return s == nil || len(s.Engines) == 0
}

// Search runs one query through the named engine, or the default when engine
// is "". It returns the engine actually used so callers can report it.
func (s *Set) Search(ctx context.Context, engine string, req Request) (string, *Result, error) {
	name := engine
	if name == "" {
		name = s.Default
	}
	e, ok := s.Engines[name]
	if !ok {
		return name, nil, fmt.Errorf("search engine %q is not configured (have: %s)", name, strings.Join(s.Names(), ", "))
	}
	res, err := e.Search(ctx, req)
	if err != nil {
		return name, nil, err
	}
	res.Engine = name
	return name, res, nil
}

// engineNames is every engine this build has, and the list the others answer
// to: NewEngine's switch must build each one, and keyedEngines marks the ones
// that need a credential. config validation accepts exactly these.
//
// They used to be three hand-kept lists in three files. Adding an engine to one
// and not the others changed behaviour without saying so — a name the validator
// accepted but NewEngine could not build, or an engine missing from
// keyedEngines that skipped the honest-degradation warning for a missing key.
var engineNames = []string{
	"doubao", "ollama", "google_search", "x_search",
	"stackoverflow", "github", "youtube",
}

// NewEngine builds one engine driver by name. The name selects the wire
// protocol; SearchEngine carries the connection config.
func NewEngine(name string, cfg config.SearchEngine) (Searcher, error) {
	switch name {
	case "doubao":
		return newDoubao(cfg)
	case "ollama":
		return newOllama(cfg)
	case "google_search":
		return newGoogleSearch(cfg)
	case "x_search":
		return newXSearch(cfg)
	case "stackoverflow":
		return newStackOverflow(cfg)
	case "github":
		return newGitHub(cfg)
	case "youtube":
		return newYouTube(cfg)
	}
	return nil, fmt.Errorf("search: unsupported engine %q", name)
}

// keyedEngines require an API key; a missing or unresolvable credential skips
// the engine with a warning (honest degradation), it never fails the boot.
// Every key here must also be in engineNames.
var keyedEngines = map[string]bool{
	"doubao": true, "ollama": true, "youtube": true,
	"google_search": true, "x_search": true,
}

// Build constructs the full engine set from config. With no engines it
// returns an empty Set (not an error) — the tool reports honest-unavailable.
// res lazily resolves api_key references (${VAR} / cred:<service>): env, then
// the credential store, then the engine is skipped with a warning naming the
// missing credential. log may be nil.
func Build(cfg config.Search, res *config.Resolver, log *slog.Logger) (*Set, error) {
	set := &Set{Engines: map[string]Searcher{}, Default: cfg.Default}
	for name, ecfg := range cfg.Engines {
		if keyedEngines[name] && ecfg.APIKey != "" {
			v, ok := res.Resolve(context.Background(), ecfg.APIKey)
			if !ok {
				warnSkip(log, name, ecfg.APIKey)
				continue
			}
			ecfg.APIKey = v
		}
		e, err := NewEngine(name, ecfg)
		if err != nil {
			if keyedEngines[name] {
				// Keyless after resolution — degrade, don't fail the boot.
				warnSkip(log, name, ecfg.APIKey)
				continue
			}
			return nil, err
		}
		set.Engines[name] = e
	}
	// If the configured default was skipped, fall back to the sole remaining
	// engine (or none — calls then name an engine explicitly).
	if set.Default != "" {
		if _, ok := set.Engines[set.Default]; !ok {
			if log != nil {
				log.Warn("search default engine unavailable after credential resolution; clearing default",
					"engine", set.Default)
			}
			set.Default = ""
			if len(set.Engines) == 1 {
				for n := range set.Engines {
					set.Default = n
				}
			}
		}
	}
	return set, nil
}

func warnSkip(log *slog.Logger, engine, raw string) {
	if log == nil {
		return
	}
	log.Warn("search engine skipped: unresolved credential",
		"engine", engine, "credential", config.CredentialName(raw))
}
