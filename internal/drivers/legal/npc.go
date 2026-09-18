package legal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/docparse"
)

// The National Database of Laws and Regulations (国家法律法规数据库,
// flk.npc.gov.cn) — the PRC's official source for national laws, administrative
// regulations, judicial interpretations, and local rules. Free, no key, and
// indifferent to the client User-Agent. Search is POST /law-search/search/list
// with real server-side paging; one document is GET /law-search/download/pc,
// which returns a short-lived signed URL to a .docx that docparse turns into
// text (the format param is ignored — it is always a docx). The API is
// undocumented/internal to the web app, so shapes are mapped defensively.
const (
	defaultNPCURL = "https://flk.npc.gov.cn"
	maxNPCCount   = 50 // pageSize ceiling
)

// npc is an Engine over the NPC database. Search hits are legislation; the
// fetch key (Path) is the opaque bbbs document id.
type npc struct {
	baseURL string
	http    *http.Client
}

func newNPC(cfg config.SearchEngine) (*npc, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultNPCURL
	}
	return &npc{
		baseURL: strings.TrimSuffix(base, "/"),
		http:    &http.Client{Timeout: cfg.TimeoutD()},
	}, nil
}

// npcStatus maps the 时效性 (validity) code onto its label.
var npcStatus = map[int]string{
	1: "已废止",  // repealed
	2: "已修改",  // amended
	3: "有效",   // in force
	4: "尚未生效", // not yet in force
}

type npcSearchRequest struct {
	SearchRange   int      `json:"searchRange"`
	Sxrq          []string `json:"sxrq"`
	Gbrq          []string `json:"gbrq"`
	SearchType    int      `json:"searchType"` // 1 = title, 2 = full text
	Sxx           []string `json:"sxx"`
	GbrqYear      []string `json:"gbrqYear"`
	FlfgCodeID    []string `json:"flfgCodeId"`
	ZdjgCodeID    []string `json:"zdjgCodeId"`
	SearchContent string   `json:"searchContent"`
	XgzlSearch    bool     `json:"xgzlSearch"`
	OrderByParam  struct {
		Order string `json:"order"`
		Sort  string `json:"sort"`
	} `json:"orderByParam"`
	PageNum  int `json:"pageNum"`
	PageSize int `json:"pageSize"`
}

type npcSearchResponse struct {
	Code  int    `json:"code"`
	Msg   string `json:"msg"`
	Total int    `json:"total"`
	Rows  []struct {
		BBBS     string `json:"bbbs"`     // document id (fetch key)
		Title    string `json:"title"`    // may carry <em class='highlight'> marks
		Gbrq     string `json:"gbrq"`     // 公布日期 promulgation date
		Sxrq     string `json:"sxrq"`     // 施行日期 effective date
		Sxx      int    `json:"sxx"`      // 时效性 validity code
		ZdjgName string `json:"zdjgName"` // 制定机关 enacting organ
		Flxz     string `json:"flxz"`     // 法律性质 legal nature (法律/行政法规/…)
	} `json:"rows"`
}

// Search runs one law search.
func (n *npc) Search(ctx context.Context, req Request) (*SearchResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("legal npc: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxNPCCount {
		count = maxNPCCount
	}

	body := npcSearchRequest{
		SearchRange:   1,
		Sxrq:          []string{},
		Gbrq:          []string{},
		Sxx:           []string{},
		GbrqYear:      []string{},
		FlfgCodeID:    []string{},
		ZdjgCodeID:    []string{},
		SearchContent: query,
		XgzlSearch:    false,
		PageNum:       1,
		PageSize:      count,
	}
	body.SearchType = 2 // full text (fuzzy/broad) by default
	if req.DisableFuzzy {
		body.SearchType = 1 // exact: title match only
	}
	body.OrderByParam.Order = "-1"

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("legal npc: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+"/law-search/search/list", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("legal npc: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json;charset=UTF-8")
	hreq.Header.Set("User-Agent", "agentflow")

	payload, err := n.do(hreq)
	if err != nil {
		return nil, err
	}
	var sr npcSearchResponse
	if err := json.Unmarshal(payload, &sr); err != nil {
		return nil, fmt.Errorf("legal npc: decode search response: %w", err)
	}
	if sr.Code != 200 {
		return nil, fmt.Errorf("legal npc: search failed (code %d): %s", sr.Code, sr.Msg)
	}

	out := &SearchResult{Query: query, Total: sr.Total, Results: make([]Case, 0, len(sr.Rows))}
	for _, r := range sr.Rows {
		out.Results = append(out.Results, Case{
			Title:     npcStripMarks(r.Title),
			Path:      r.BBBS,
			URL:       n.baseURL + "/detail2.html?" + url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(r.BBBS))),
			Court:     r.ZdjgName,
			Date:      r.Gbrq,
			Type:      r.Flxz,
			Effective: r.Sxrq,
			Status:    npcStatus[r.Sxx],
		})
	}
	return out, nil
}

// Fetch downloads one document and extracts its text via docparse (the format
// is auto-detected, so a future non-docx response is still handled).
func (n *npc) Fetch(ctx context.Context, fr FetchRequest) (*Judgment, error) {
	bbbs := strings.TrimSpace(fr.Path)
	if bbbs == "" {
		return nil, fmt.Errorf("legal npc: need the document id (path from a legal_search hit) to fetch")
	}
	// A bbbs id is an opaque token with no separators. A slash means this is a
	// path shaped for another engine, so say so rather than spend a request on
	// something that could only fail — and let Set.Fetch route it to the engine
	// that can read it.
	if strings.Contains(bbbs, "/") {
		return nil, fmt.Errorf("legal npc: %q is not a document id (npc ids contain no separators): %w", bbbs, ErrPathNotForEngine)
	}
	dlURL := fmt.Sprintf("%s/law-search/download/pc?format=docx&bbbs=%s", n.baseURL, url.QueryEscape(bbbs))
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("legal npc: build request: %w", err)
	}
	hreq.Header.Set("User-Agent", "agentflow")
	payload, err := n.do(hreq)
	if err != nil {
		return nil, err
	}
	var dr struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &dr); err != nil {
		return nil, fmt.Errorf("legal npc: decode download response: %w", err)
	}
	if dr.Code != 200 || dr.Data.URL == "" {
		return nil, fmt.Errorf("legal npc: download failed (code %d): %s", dr.Code, dr.Msg)
	}

	// The signed URL points at object storage; fetch the document bytes.
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, dr.Data.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("legal npc: build download request: %w", err)
	}
	dreq.Header.Set("User-Agent", "agentflow")
	docBytes, err := n.do(dreq)
	if err != nil {
		return nil, err
	}
	text, format, err := docparse.Text(docBytes)
	if err != nil {
		return nil, fmt.Errorf("legal npc: extract text: %w", err)
	}

	full := len(text)
	truncated := false
	if fr.Start > 0 {
		if fr.Start < len(text) {
			text = text[fr.Start:]
		} else {
			text = ""
		}
		truncated = true
	}
	if fr.MaxChars > 0 && len(text) > fr.MaxChars {
		text = text[:fr.MaxChars]
		truncated = true
	}

	return &Judgment{
		Title:     npcFirstLine(text),
		Neutral:   bbbs,
		Text:      text,
		TextLen:   full,
		Truncated: truncated,
		DocURL:    fmt.Sprintf("%s/law-search/download/pc?format=%s&bbbs=%s", n.baseURL, format, url.QueryEscape(bbbs)),
	}, nil
}

// do performs one request and returns the body, mapping non-200s onto honest
// errors.
func (n *npc) do(hreq *http.Request) ([]byte, error) {
	resp, err := n.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("legal npc: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("legal npc: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legal npc: status %d: %s", resp.StatusCode, npcTruncate(payload, 512))
	}
	return payload, nil
}

var npcEmRe = regexp.MustCompile(`<[^>]*>`)

// npcStripMarks removes the <em class='highlight'> marks the API wraps around
// matched terms in titles.
func npcStripMarks(s string) string {
	return npcEmRe.ReplaceAllString(s, "")
}

// npcFirstLine returns the first non-empty line (the document title) of an
// extracted text body.
func npcFirstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func npcTruncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
