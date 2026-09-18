package legal

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"agentflow/internal/config"
)

// The Hong Kong Legal Information Institute API (www.hklii.hk/api) — free, no
// key required, and indifferent to the client User-Agent. Search is
// /api/simplesearch (searchstring + disablefuzzy); it returns the full match
// set in one shot (Total in "count", the array capped server-side), so Count
// is applied client-side. One judgment is /api/getjudgment keyed by
// lang/abbr/year/num — exactly the segments of a search hit's Path. The API is
// undocumented/internal to the HKLII web app, so shapes are mapped defensively.
const (
	defaultHKLIIURL = "https://www.hklii.hk"
	maxHKLIICount   = 2500 // server returns at most this many hits
)

// hklii is an Engine over HKLII. Search covers both case law and legislation
// (the hit's Court/db names the source, e.g. "Court of Appeal" or "Hong Kong
// Ordinances"); Fetch retrieves a case judgment as plain text.
type hklii struct {
	baseURL string
	http    *http.Client
}

func newHKLII(cfg config.SearchEngine) (*hklii, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultHKLIIURL
	}
	return &hklii{
		baseURL: strings.TrimSuffix(base, "/"),
		http:    &http.Client{Timeout: cfg.TimeoutD()},
	}, nil
}

type hkliiSearchResponse struct {
	Count   int `json:"count"`
	Results []struct {
		Title    string `json:"title"`
		Path     string `json:"path"`
		PubDate  string `json:"pub_date"`
		DB       string `json:"db"`
		Act      string `json:"act"`
		Neutral  string `json:"neutral"`
		Parallel string `json:"parallel"`
		Coram    string `json:"coram"`
		Parties  string `json:"parties"`
	} `json:"results"`
}

// Search runs one simplesearch query.
func (h *hklii) Search(ctx context.Context, req Request) (*SearchResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("legal hklii: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxHKLIICount {
		count = maxHKLIICount
	}

	q := url.Values{}
	q.Set("searchstring", query)
	if req.DisableFuzzy {
		q.Set("disablefuzzy", "1")
	} else {
		q.Set("disablefuzzy", "0")
	}
	payload, err := h.get(ctx, "/api/simplesearch?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var sr hkliiSearchResponse
	if err := json.Unmarshal(payload, &sr); err != nil {
		return nil, fmt.Errorf("legal hklii: decode search response: %w", err)
	}

	out := &SearchResult{Query: query, Total: sr.Count, Results: make([]Case, 0, min(count, len(sr.Results)))}
	for i, it := range sr.Results {
		if i >= count {
			break // Count applied client-side; the server returns the whole set
		}
		out.Results = append(out.Results, Case{
			Title:    it.Title,
			Path:     it.Path,
			URL:      h.baseURL + it.Path,
			Court:    it.DB,
			Date:     it.PubDate,
			Neutral:  it.Neutral,
			Parallel: it.Parallel,
			Action:   it.Act,
			Parties:  it.Parties,
			Coram:    it.Coram,
		})
	}
	return out, nil
}

type hkliiJudgmentResponse struct {
	Date    string `json:"date"`
	DB      string `json:"db"`
	Neutral string `json:"neutral"`
	Content string `json:"content"` // full HTML body
	Doc     string `json:"doc"`     // original Word/PDF
	Cases   []struct {
		Title string `json:"title"`
		Act   string `json:"act"`
	} `json:"cases"`
	ParallelCitation []string `json:"parallel_citation"`
	IsTranslation    bool     `json:"is_translation"`
	HasTranslation   bool     `json:"has_translation"`
}

// Fetch retrieves one judgment as plain text.
func (h *hklii) Fetch(ctx context.Context, fr FetchRequest) (*Judgment, error) {
	lang, abbr, year, num, err := fr.identity()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("lang", lang)
	q.Set("abbr", abbr)
	q.Set("year", year)
	q.Set("num", num)
	payload, err := h.get(ctx, "/api/getjudgment?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var jr hkliiJudgmentResponse
	if err := json.Unmarshal(payload, &jr); err != nil {
		return nil, fmt.Errorf("legal hklii: decode judgment response: %w", err)
	}

	text := judgmentText(jr.Content)
	full := len(text)
	if fr.Start > 0 {
		if fr.Start < len(text) {
			text = text[fr.Start:]
		} else {
			text = ""
		}
	}
	truncated := fr.Start > 0
	if fr.MaxChars > 0 && len(text) > fr.MaxChars {
		text = text[:fr.MaxChars]
		truncated = true
	}

	title := jr.Neutral
	if len(jr.Cases) > 0 && jr.Cases[0].Title != "" {
		title = jr.Cases[0].Title
	}
	return &Judgment{
		Title:          title,
		Neutral:        jr.Neutral,
		Court:          jr.DB,
		Date:           jr.Date,
		Parallel:       jr.ParallelCitation,
		DocURL:         jr.Doc,
		Text:           text,
		TextLen:        full,
		Truncated:      truncated,
		HasTranslation: jr.HasTranslation,
	}, nil
}

// get performs one GET and returns the body, mapping non-200s (a missing
// judgment or unavailable language comes back as 404) onto honest errors.
func (h *hklii) get(ctx context.Context, path string) ([]byte, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("legal hklii: build request: %w", err)
	}
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", "agentflow") // polite client identification
	resp, err := h.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("legal hklii: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("legal hklii: read response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("legal hklii: not found (no such judgment, or that language version does not exist)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legal hklii: status %d: %s", resp.StatusCode, truncateBytes(payload, 512))
	}
	return payload, nil
}

// identity resolves the lang/abbr/year/num quadruple from either Path
// (/{lang}/cases/{abbr}/{year}/{num}) or the explicit fields.
func (fr FetchRequest) identity() (lang, abbr, year, num string, err error) {
	lang, abbr, year, num = fr.Lang, fr.Abbr, fr.Year, fr.Num
	if fr.Path != "" {
		parts := strings.Split(strings.Trim(fr.Path, "/"), "/")
		// {lang}/cases/{abbr}/{year}/{num}
		if len(parts) != 5 || parts[1] != "cases" {
			// Wrapped so Set.Fetch can hand the path to another engine: it is
			// not malformed, it belongs to a backend that shapes paths
			// differently.
			return "", "", "", "", fmt.Errorf("legal hklii: path %q is not a case judgment path (want /{lang}/cases/{abbr}/{year}/{num}): %w", fr.Path, ErrPathNotForEngine)
		}
		if lang == "" {
			lang = parts[0]
		}
		abbr, year, num = parts[2], parts[3], parts[4]
	}
	if lang == "" {
		lang = "en"
	}
	if abbr == "" || year == "" || num == "" {
		return "", "", "", "", fmt.Errorf("legal hklii: need a case path or abbr+year+num to fetch a judgment")
	}
	return lang, abbr, year, num, nil
}

var (
	// Block-level boundaries become newlines before tags are stripped, so the
	// judgment keeps its paragraph structure (paragraph numbers live in anchor
	// text like <a class="para" id="p1">1.</a> and survive stripping).
	blockRe = regexp.MustCompile(`(?i)</(p|tr|table|div|blockquote|li|h[1-6])>\s*|<\s*(br|p|tr|li|blockquote)[^>]*>`)
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	blankRe = regexp.MustCompile(`\n{3,}`)
	spaceRe = regexp.MustCompile(`[^\S\n]+`)
)

// judgmentText converts a judgment's HTML body to readable plain text,
// preserving paragraph/line breaks and decoding entities.
func judgmentText(s string) string {
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// Collapse horizontal whitespace per line, then blank-line runs.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(spaceRe.ReplaceAllString(ln, " "))
	}
	s = strings.Join(lines, "\n")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
