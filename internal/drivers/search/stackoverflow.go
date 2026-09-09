package search

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/config"
)

// The StackExchange API (api.stackexchange.com/2.3) — searched on
// site=stackoverflow by default. No key is required: the anonymous quota is
// 300 requests/day per IP, and a free app key (stackapps.com, app key not a
// secret) lifts it to 10,000/day. The API mandates client backoff via the
// "backoff" response field (seconds); the engine honors it process-wide.
const (
	defaultStackURL  = "https://api.stackexchange.com"
	defaultStackSite = "stackoverflow"
	maxStackCount    = 100 // pagesize ceiling
)

// stackoverflow is a Searcher over /2.3/search/advanced. Each hit is a
// question: URL is the question link, Content is the question body (HTML),
// Snippet is an answered-signal prefix plus a text-only excerpt, and Score is
// the raw upvote count (engine-native, not 0..1).
type stackoverflow struct {
	baseURL string
	site    string
	key     string
	http    *http.Client

	mu        sync.Mutex
	notBefore time.Time // server-mandated backoff deadline
}

func newStackOverflow(cfg config.SearchEngine) (*stackoverflow, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultStackURL
	}
	site := cfg.Site
	if site == "" {
		site = defaultStackSite
	}
	return &stackoverflow{
		baseURL: strings.TrimSuffix(base, "/"),
		site:    site,
		key:     cfg.APIKey,
		http:    &http.Client{Timeout: cfg.TimeoutD()},
	}, nil
}

type stackResponse struct {
	Items []struct {
		Title        string   `json:"title"`
		Link         string   `json:"link"`
		Score        int      `json:"score"`
		AnswerCount  int      `json:"answer_count"`
		IsAnswered   bool     `json:"is_answered"`
		CreationDate int64    `json:"creation_date"`
		Tags         []string `json:"tags"`
		Body         string   `json:"body"`
		Owner        struct {
			DisplayName string `json:"display_name"`
		} `json:"owner"`
	} `json:"items"`
	HasMore        bool `json:"has_more"`
	QuotaMax       int  `json:"quota_max"`
	QuotaRemaining int  `json:"quota_remaining"`
	Backoff        int  `json:"backoff"`
	// Error envelope (present on failures, with a non-200 status).
	ErrorID      int    `json:"error_id"`
	ErrorName    string `json:"error_name"`
	ErrorMessage string `json:"error_message"`
}

// Search runs one search over StackOverflow questions.
func (s *stackoverflow) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search stackoverflow: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxStackCount {
		count = maxStackCount
	}

	q := url.Values{}
	q.Set("order", "desc")
	q.Set("sort", "relevance")
	q.Set("site", s.site)
	q.Set("pagesize", strconv.Itoa(count))
	q.Set("filter", "withbody") // include the question body
	q.Set("q", query)
	if s.key != "" {
		q.Set("key", s.key)
	}
	if from, to, ok := stackTimeRange(req.TimeRange); ok {
		q.Set("fromdate", strconv.FormatInt(from, 10))
		if to > 0 {
			q.Set("todate", strconv.FormatInt(to, 10))
		}
	}

	if err := s.waitBackoff(ctx); err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/2.3/search/advanced?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search stackoverflow: build request: %w", err)
	}
	hreq.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search stackoverflow: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search stackoverflow: read response: %w", err)
	}
	var sr stackResponse
	if err := json.Unmarshal(payload, &sr); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("search stackoverflow: status %d: %s", resp.StatusCode, truncate(payload, 512))
		}
		return nil, fmt.Errorf("search stackoverflow: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search stackoverflow: status %d %s: %s", resp.StatusCode, sr.ErrorName, sr.ErrorMessage)
	}
	// Honor a mandated backoff for subsequent requests.
	if sr.Backoff > 0 {
		s.mu.Lock()
		s.notBefore = time.Now().Add(time.Duration(sr.Backoff) * time.Second)
		s.mu.Unlock()
	}

	out := &Result{Query: query, Results: make([]WebResult, 0, len(sr.Items))}
	for _, it := range sr.Items {
		w := WebResult{
			Title:     html.UnescapeString(it.Title), // titles arrive HTML-escaped
			URL:       it.Link,
			Content:   it.Body,
			Score:     float64(it.Score),
			Published: time.Unix(it.CreationDate, 0).UTC().Format(time.RFC3339),
		}
		if len(it.Tags) > 0 {
			tags := it.Tags
			if len(tags) > 3 {
				tags = tags[:3]
			}
			w.Site = strings.Join(tags, ", ")
		}
		signal := "unanswered"
		if it.IsAnswered {
			signal = fmt.Sprintf("answered (%d answers)", it.AnswerCount)
		}
		w.Snippet = signal + ": " + truncate([]byte(stripHTML(it.Body)), 240-len(signal)-2)
		out.Results = append(out.Results, w)
	}
	return out, nil
}

// waitBackoff sleeps until the server-mandated backoff deadline (if any).
func (s *stackoverflow) waitBackoff(ctx context.Context) error {
	s.mu.Lock()
	nb := s.notBefore
	s.mu.Unlock()
	if d := time.Until(nb); d > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("search stackoverflow: cancelled during backoff: %w", ctx.Err())
		case <-time.After(d):
		}
	}
	return nil
}

// stackTimeRange maps the shared TimeRange enum (and the YYYY-MM-DD..YYYY-MM-DD
// range form) onto fromdate/todate unix seconds.
func stackTimeRange(tr string) (from, to int64, ok bool) {
	if tr == "" {
		return 0, 0, false
	}
	switch tr {
	case "OneDay":
		return time.Now().Add(-24 * time.Hour).Unix(), 0, true
	case "OneWeek":
		return time.Now().Add(-7 * 24 * time.Hour).Unix(), 0, true
	case "OneMonth":
		return time.Now().Add(-30 * 24 * time.Hour).Unix(), 0, true
	case "OneYear":
		return time.Now().Add(-365 * 24 * time.Hour).Unix(), 0, true
	}
	parts := strings.SplitN(tr, "..", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	f, err1 := time.ParseInLocation("2006-01-02", parts[0], time.UTC)
	t, err2 := time.ParseInLocation("2006-01-02", parts[1], time.UTC)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return f.Unix(), t.Add(24 * time.Hour).Unix(), true // todate is inclusive midnight + 1 day
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var wsRe = regexp.MustCompile(`\s+`)

// stripHTML turns a StackExchange HTML body into plain text (titles, code
// blocks, and tags removed; entities decoded).
func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}
