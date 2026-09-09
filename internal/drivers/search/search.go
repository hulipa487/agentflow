// Package search implements web-search backends for the builtin:web_search
// tool. A Searcher is one engine; doubao.go and ollama.go speak their
// respective APIs. The tool selects an engine per call (defaulting to config
// search.default), and with no engines configured reports honest-unavailable
// so search degrades cleanly rather than failing silently.
package search

import (
	"context"
	"fmt"
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

// NewEngine builds one engine driver by name. The name selects the wire
// protocol; SearchEngine carries the connection config.
func NewEngine(name string, cfg config.SearchEngine) (Searcher, error) {
	switch name {
	case "doubao":
		return newDoubao(cfg)
	case "ollama":
		return newOllama(cfg)
	case "stackoverflow":
		return newStackOverflow(cfg)
	case "github":
		return newGitHub(cfg)
	}
	return nil, fmt.Errorf("search: unsupported engine %q", name)
}

// Build constructs the full engine set from config. With no engines it
// returns an empty Set (not an error) — the tool reports honest-unavailable.
func Build(cfg config.Search) (*Set, error) {
	set := &Set{Engines: map[string]Searcher{}, Default: cfg.Default}
	for name, ecfg := range cfg.Engines {
		e, err := NewEngine(name, ecfg)
		if err != nil {
			return nil, err
		}
		set.Engines[name] = e
	}
	return set, nil
}
