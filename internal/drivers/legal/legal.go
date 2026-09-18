// Package legal implements legal-database backends for the builtin:legal_search
// and builtin:legal_read tools. An Engine is one provider (hklii.go speaks the
// Hong Kong Legal Information Institute API). legal_search returns case /
// legislation metadata (citations, court, parties); legal_read retrieves one
// full judgment as plain text. With no engine configured both tools report
// honest-unavailable rather than failing.
package legal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentflow/internal/config"
)

// Request is a normalized legal-search request.
type Request struct {
	Query        string // free text; names, citations, concepts
	Count        int    // max results (the engine clamps to its own ceiling)
	DisableFuzzy bool   // true = exact keyword match; false = fuzzy/concept match
}

// FetchRequest identifies one judgment to retrieve, plus optional paging over
// the plain-text body. Path is the form a search hit carries
// (/en/cases/hkcfi/2020/1808); Lang/Abbr/Year/Num are the explicit alternative.
type FetchRequest struct {
	Path string // /{lang}/cases/{abbr}/{year}/{num} — from a search hit
	Lang string // en|tc|sc; default taken from Path, else "en"
	Abbr string // court/database abbreviation, e.g. hkcfi (with Year and Num)
	Year string
	Num  string

	Start    int // byte offset into the text body (for reading long judgments in parts)
	MaxChars int // cap on returned text bytes; 0 = no cap
}

// Case is one search hit — a judgment or a piece of legislation.
type Case struct {
	Title    string `json:"title"`
	Path     string `json:"path"` // fetch key for legal_read
	URL      string `json:"url"`  // absolute page on the provider
	Court    string `json:"court"`
	Date     string `json:"date"`
	Neutral  string `json:"neutral,omitempty"`  // neutral citation, e.g. [2020] HKCFI 1808
	Parallel string `json:"parallel,omitempty"` // parallel/report citation, e.g. [2021] 1 HKLRD 919
	Action   string `json:"action,omitempty"`   // action/proceeding number, e.g. HCMA101/2020
	Parties  string `json:"parties,omitempty"`
	Coram    string `json:"coram,omitempty"` // presiding judge(s)
	// Legislation-oriented fields (npc; empty for hklii judgments).
	Type      string `json:"type,omitempty"`      // legal nature, e.g. 法律 (Law) / 行政法规 (Administrative Regulation)
	Effective string `json:"effective,omitempty"` // date in force (施行日期)
	Status    string `json:"status,omitempty"`    // validity, e.g. 有效 / 已修改 / 已废止 / 尚未生效
}

// SearchResult is a normalized legal-search response. Total is the provider's
// full match count (Results is the returned page of it).
type SearchResult struct {
	Query      string `json:"query"`
	Engine     string `json:"engine"`
	Total      int    `json:"total"`
	Results    []Case `json:"results"`
	TimeCostMs int64  `json:"time_cost_ms,omitempty"`
}

// Judgment is one fetched document as plain text (the provider's HTML body,
// tag-stripped with paragraph breaks preserved).
type Judgment struct {
	Title          string   `json:"title"`
	Neutral        string   `json:"neutral"`
	Court          string   `json:"court"`
	Date           string   `json:"date"`
	Parallel       []string `json:"parallel_citation,omitempty"`
	DocURL         string   `json:"doc,omitempty"` // original Word/PDF source
	Text           string   `json:"text"`
	TextLen        int      `json:"text_len"`            // full body length (before any MaxChars cap)
	Truncated      bool     `json:"truncated,omitempty"` // Text was capped/paged
	HasTranslation bool     `json:"has_translation,omitempty"`
}

// Engine is one legal provider: search plus judgment fetch.
type Engine interface {
	Search(ctx context.Context, req Request) (*SearchResult, error)
	Fetch(ctx context.Context, fr FetchRequest) (*Judgment, error)
}

// Set is the named collection of configured engines plus the default used when
// a call omits `engine`.
type Set struct {
	Engines map[string]Engine
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

// Empty reports whether no engine is configured (the tools then report
// honest-unavailable).
func (s *Set) Empty() bool {
	return s == nil || len(s.Engines) == 0
}

func (s *Set) engine(name string) (string, Engine, error) {
	if name == "" {
		name = s.Default
	}
	e, ok := s.Engines[name]
	if !ok {
		return name, nil, fmt.Errorf("legal engine %q is not configured (have: %s)", name, strings.Join(s.Names(), ", "))
	}
	return name, e, nil
}

// Search runs one query through the named engine (or the default), returning
// the engine actually used so callers can report it.
func (s *Set) Search(ctx context.Context, engine string, req Request) (string, *SearchResult, error) {
	name, e, err := s.engine(engine)
	if err != nil {
		return name, nil, err
	}
	res, err := e.Search(ctx, req)
	if err != nil {
		return name, nil, err
	}
	res.Engine = name
	return name, res, nil
}

// Fetch retrieves one judgment through the named engine (or the default).
func (s *Set) Fetch(ctx context.Context, engine string, fr FetchRequest) (string, *Judgment, error) {
	name, e, err := s.engine(engine)
	if err != nil {
		return name, nil, err
	}
	j, err := e.Fetch(ctx, fr)
	if err != nil {
		return name, nil, err
	}
	return name, j, nil
}

// NewEngine builds one engine driver by name. The name selects the wire
// protocol; SearchEngine carries the connection config.
func NewEngine(name string, cfg config.SearchEngine) (Engine, error) {
	switch name {
	case "hklii":
		return newHKLII(cfg)
	case "npc":
		return newNPC(cfg)
	}
	return nil, fmt.Errorf("legal: unsupported engine %q", name)
}

// Build constructs the full engine set from config. With no engines it returns
// an empty Set (not an error) — the tools report honest-unavailable.
func Build(cfg config.LegalSearch) (*Set, error) {
	set := &Set{Engines: map[string]Engine{}, Default: cfg.Default}
	for name, ecfg := range cfg.Engines {
		e, err := NewEngine(name, ecfg)
		if err != nil {
			return nil, err
		}
		set.Engines[name] = e
	}
	return set, nil
}
