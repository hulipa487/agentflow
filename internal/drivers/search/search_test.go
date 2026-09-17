package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
)

// --- doubao ------------------------------------------------------------------

// doubaoOK is a trimmed real-shape web_search response.
const doubaoOK = `{
  "ResponseMetadata": {"RequestId": "req-1", "Action": "WebSearch", "Version": "2025-01-01"},
  "Result": {
    "ResultCount": 1,
    "WebResults": [{
      "Id": "x", "SortId": 1,
      "Title": "Beijing guide", "SiteName": "sohu", "Url": "https://example.com/a",
      "Snippet": "short", "Summary": "longer summary",
      "PublishTime": "2025-06-19T15:10:00+08:00", "RankScore": 0.95
    }],
    "SearchContext": {"OriginQuery": "beijing", "SearchType": "web"},
    "TimeCost": 372, "LogId": "req-1"
  }
}`

func TestDoubaoSearch(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, doubaoOK)
	}))
	defer srv.Close()

	s, err := newDoubao(config.SearchEngine{APIKey: "k123", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), Request{Query: "beijing", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k123" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"Query":"beijing"`) || !strings.Contains(gotBody, `"SearchType":"web"`) {
		t.Fatalf("request body: %s", gotBody)
	}
	if res.Query != "beijing" || len(res.Results) != 1 {
		t.Fatalf("result: %+v", res)
	}
	w := res.Results[0]
	if w.Title != "Beijing guide" || w.URL != "https://example.com/a" || w.Summary != "longer summary" || w.Score != 0.95 {
		t.Fatalf("web result: %+v", w)
	}
	if res.TimeCostMs != 372 {
		t.Fatalf("time cost: %d", res.TimeCostMs)
	}
}

func TestDoubaoCountClamp(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, doubaoOK)
	}))
	defer srv.Close()
	s, _ := newDoubao(config.SearchEngine{APIKey: "k", BaseURL: srv.URL})
	if _, err := s.Search(context.Background(), Request{Query: "q", Count: 999}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"Count":50`) {
		t.Fatalf("count not clamped to 50: %s", gotBody)
	}
}

func TestDoubaoAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ResponseMetadata":{"RequestId":"r","Error":{"CodeN":10406,"Code":"10406","Message":"FreeQuotaExhausted"}},"Result":null}`)
	}))
	defer srv.Close()
	s, _ := newDoubao(config.SearchEngine{APIKey: "k", BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "10406") {
		t.Fatalf("expected api error, got %v", err)
	}
}

func TestDoubaoRedactsKey(t *testing.T) {
	key := "supersecretkey"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"detail":"bad token `+key+`"}`)
	}))
	defer srv.Close()
	s, _ := newDoubao(config.SearchEngine{APIKey: key, BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaked api key: %v", err)
	}
}

func TestDoubaoRequiresKey(t *testing.T) {
	if _, err := newDoubao(config.SearchEngine{}); err == nil {
		t.Fatal("expected error for missing api_key")
	}
}

// --- ollama ------------------------------------------------------------------

const ollamaOK = `{"results":[
  {"title":"Ollama","url":"https://ollama.com/","content":"Cloud models are now available."},
  {"title":"Docs","url":"https://docs.ollama.com/","content":"API reference."}
]}`

func TestOllamaSearch(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, ollamaOK)
	}))
	defer srv.Close()

	s, err := newOllama(config.SearchEngine{APIKey: "ok123", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), Request{Query: "what is ollama?"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ok123" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"query":"what is ollama?"`) || !strings.Contains(gotBody, `"max_results":5`) {
		t.Fatalf("request body: %s", gotBody)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results: %+v", res)
	}
	w := res.Results[0]
	if w.Title != "Ollama" || w.URL != "https://ollama.com/" || w.Content != "Cloud models are now available." || w.Snippet == "" {
		t.Fatalf("result[0]: %+v", w)
	}
}

func TestOllamaCountClampAndAuthError(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"Unauthorized"}`)
	}))
	defer srv.Close()
	s, _ := newOllama(config.SearchEngine{APIKey: "k", BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q", Count: 99})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("expected unauthorized error, got %v", err)
	}
	if !strings.Contains(gotBody, `"max_results":10`) {
		t.Fatalf("count not clamped to 10: %s", gotBody)
	}
}

func TestOllamaRequiresKey(t *testing.T) {
	if _, err := newOllama(config.SearchEngine{}); err == nil {
		t.Fatal("expected error for missing api_key")
	}
}

// --- stackoverflow -----------------------------------------------------------

// stackOK is a trimmed real-shape /2.3/search/advanced response (filter=withbody).
const stackOK = `{
  "items": [
    {
      "tags": ["memory-leaks", "go", "goroutine"],
      "owner": {"display_name": "nhooyr"},
      "is_answered": true,
      "view_count": 999,
      "answer_count": 2,
      "score": 2,
      "creation_date": 1426991752,
      "question_id": 29190333,
      "title": "Golang Goroutine leak &quot;fixed&quot;",
      "link": "https://stackoverflow.com/questions/29190333/golang-goroutine-leak",
      "body": "<pre><code>// code</code></pre> <p>The loop leaks.</p>"
    },
    {
      "tags": ["lua"],
      "owner": {"display_name": "bob"},
      "is_answered": false,
      "answer_count": 0,
      "score": 0,
      "creation_date": 1757361600,
      "title": "Plain title",
      "link": "https://stackoverflow.com/questions/2/plain",
      "body": "<p>hi</p>"
    }
  ],
  "has_more": true,
  "quota_max": 300,
  "quota_remaining": 299
}`

func TestStackOverflowSearch(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, stackOK)
	}))
	defer srv.Close()

	s, err := newStackOverflow(config.SearchEngine{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), Request{Query: "goroutine leak", Count: 2, TimeRange: "OneWeek"})
	if err != nil {
		t.Fatal(err)
	}
	// Wire params.
	if gotQuery.Get("site") != "stackoverflow" || gotQuery.Get("filter") != "withbody" {
		t.Fatalf("site/filter: %s", fmtQuery(gotQuery))
	}
	if gotQuery.Get("pagesize") != "2" {
		t.Fatalf("pagesize: %s", gotQuery.Get("pagesize"))
	}
	if gotQuery.Get("fromdate") == "" {
		t.Fatal("fromdate missing for OneWeek")
	}
	if gotQuery.Get("key") != "" {
		t.Fatalf("key should not be sent without api_key: %q", gotQuery.Get("key"))
	}
	// Mapping.
	if len(res.Results) != 2 {
		t.Fatalf("results: %+v", res)
	}
	w := res.Results[0]
	if w.Title != `Golang Goroutine leak "fixed"` { // HTML entities decoded
		t.Fatalf("title unescape: %q", w.Title)
	}
	if w.URL != "https://stackoverflow.com/questions/29190333/golang-goroutine-leak" {
		t.Fatalf("url: %q", w.URL)
	}
	if w.Site != "memory-leaks, go, goroutine" {
		t.Fatalf("site(tags): %q", w.Site)
	}
	if !strings.HasPrefix(w.Snippet, "answered (2 answers): ") {
		t.Fatalf("snippet signal: %q", w.Snippet)
	}
	if strings.Contains(w.Snippet, "<") {
		t.Fatalf("snippet not stripped: %q", w.Snippet)
	}
	if w.Published != "2015-03-22T02:35:52Z" {
		t.Fatalf("published: %q", w.Published)
	}
	if w.Score != 2 {
		t.Fatalf("score: %v", w.Score)
	}
	// Unanswered item gets the unanswered signal.
	if !strings.HasPrefix(res.Results[1].Snippet, "unanswered: ") {
		t.Fatalf("unanswered snippet: %q", res.Results[1].Snippet)
	}
}

func TestStackOverflowKeyAndSite(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, stackOK)
	}))
	defer srv.Close()
	s, err := newStackOverflow(config.SearchEngine{BaseURL: srv.URL, APIKey: "appkey", Site: "superuser"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("key") != "appkey" || gotQuery.Get("site") != "superuser" {
		t.Fatalf("key/site: %s", fmtQuery(gotQuery))
	}
}

func TestStackOverflowError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error_id":400,"error_name":"bad_parameter","error_message":"`+"`sort` was invalid"+`"}`)
	}))
	defer srv.Close()
	s, _ := newStackOverflow(config.SearchEngine{BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "bad_parameter") {
		t.Fatalf("expected api error, got %v", err)
	}
}

func TestStackOverflowBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[],"has_more":false,"backoff":2,"quota_remaining":1}`)
	}))
	defer srv.Close()
	s, _ := newStackOverflow(config.SearchEngine{BaseURL: srv.URL})
	if _, err := s.Search(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	// The response's backoff=2 must make the next call wait; a cancelled ctx
	// during that wait surfaces as an error, not a silent pass.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Search(context.Background(), Request{Query: "q"})
	_ = err // first call fine; now check notBefore is set
	if s.notBefore.IsZero() {
		t.Fatal("backoff not recorded")
	}
	_, err = s.Search(ctx, Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("expected backoff wait error on cancelled ctx, got %v", err)
	}
}

func TestStackTimeRange(t *testing.T) {
	if _, _, ok := stackTimeRange(""); ok {
		t.Fatal("empty should not map")
	}
	f, _, ok := stackTimeRange("OneDay")
	if !ok || f == 0 {
		t.Fatal("OneDay should map to fromdate")
	}
	if d := time.Since(time.Unix(f, 0)); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("OneDay fromdate off: %v", d)
	}
	f, to, ok := stackTimeRange("2024-01-01..2024-01-31")
	if !ok || f == 0 || to == 0 || to <= f {
		t.Fatalf("date range: from=%d to=%d", f, to)
	}
	if _, _, ok := stackTimeRange("bogus"); ok {
		t.Fatal("bogus should not map")
	}
}

// --- github ------------------------------------------------------------------

// githubOK is a trimmed real-shape /search/repositories response. Item[1]
// exercises the null language/description/topic branches.
const githubOK = `{
  "total_count": 2,
  "incomplete_results": false,
  "items": [
    {
      "full_name": "luau-lang/luau",
      "html_url": "https://github.com/luau-lang/luau",
      "description": "A small, fast, embeddable language based on Lua.",
      "language": "C++",
      "stargazers_count": 5848,
      "forks_count": 637,
      "topics": ["lua", "programming-language", "scripting-language", "extra"],
      "pushed_at": "2026-09-08T18:37:37Z",
      "score": 1.0
    },
    {
      "full_name": "nobody/empty",
      "html_url": "https://github.com/nobody/empty",
      "description": null,
      "language": null,
      "stargazers_count": 0,
      "forks_count": 0,
      "topics": [],
      "pushed_at": "2020-01-01T00:00:00Z",
      "score": 1.0
    }
  ]
}`

func TestGitHubSearch(t *testing.T) {
	var gotQuery url.Values
	var gotAccept, gotVersion, gotUA, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, githubOK)
	}))
	defer srv.Close()

	g, err := newGitHub(config.SearchEngine{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.Search(context.Background(), Request{Query: "luau", Count: 2, TimeRange: "OneWeek"})
	if err != nil {
		t.Fatal(err)
	}
	// Headers GitHub requires.
	if gotAccept != "application/vnd.github+json" {
		t.Fatalf("accept: %q", gotAccept)
	}
	if gotVersion != "2022-11-28" {
		t.Fatalf("api version: %q", gotVersion)
	}
	if gotUA == "" {
		t.Fatal("user-agent missing (GitHub 403s requests without one)")
	}
	if gotAuth != "" {
		t.Fatalf("auth should be empty without api_key: %q", gotAuth)
	}
	// Wire params: q carries the query plus the pushed: qualifier from TimeRange.
	if gotQuery.Get("per_page") != "2" {
		t.Fatalf("per_page: %q", gotQuery.Get("per_page"))
	}
	q := gotQuery.Get("q")
	if !strings.HasPrefix(q, "luau ") || !strings.Contains(q, "pushed:>=") {
		t.Fatalf("q missing pushed qualifier: %q", q)
	}
	// Mapping.
	if len(res.Results) != 2 {
		t.Fatalf("results: %+v", res)
	}
	w := res.Results[0]
	if w.Title != "luau-lang/luau" || w.URL != "https://github.com/luau-lang/luau" {
		t.Fatalf("title/url: %+v", w)
	}
	if w.Site != "lua, programming-language, scripting-language" { // topics clamped to 3
		t.Fatalf("site(topics): %q", w.Site)
	}
	if !strings.HasPrefix(w.Snippet, "★ 5848 · C++ · 637 forks: ") {
		t.Fatalf("snippet signal: %q", w.Snippet)
	}
	if w.Content != "A small, fast, embeddable language based on Lua." {
		t.Fatalf("content: %q", w.Content)
	}
	if w.Published != "2026-09-08T18:37:37Z" {
		t.Fatalf("published: %q", w.Published)
	}
	if w.Score != 5848 {
		t.Fatalf("score(stars): %v", w.Score)
	}
	// Null language/description/topic item: bare signal, no site, no content.
	e := res.Results[1]
	if e.Snippet != "★ 0 · 0 forks" {
		t.Fatalf("empty snippet signal: %q", e.Snippet)
	}
	if e.Site != "" || e.Content != "" {
		t.Fatalf("empty site/content should be omitted: %+v", e)
	}
}

func TestGitHubKeySent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, githubOK)
	}))
	defer srv.Close()
	g, err := newGitHub(config.SearchEngine{BaseURL: srv.URL, APIKey: "ghk"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Search(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ghk" {
		t.Fatalf("auth: %q", gotAuth)
	}
}

func TestGitHubRateLimit(t *testing.T) {
	// Primary quota exhausted: 403 + X-RateLimit-Remaining: 0.
	srvQuota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srvQuota.Close()
	g, _ := newGitHub(config.SearchEngine{BaseURL: srvQuota.URL})
	_, err := g.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "API rate limit exceeded") || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("quota error should hint at api_key: %v", err)
	}

	// Secondary/abuse limit: 403 + Retry-After.
	srvAbuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"You have exceeded a secondary rate limit"}`)
	}))
	defer srvAbuse.Close()
	g2, _ := newGitHub(config.SearchEngine{BaseURL: srvAbuse.URL})
	_, err = g2.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "retry after 60s") {
		t.Fatalf("secondary limit should surface retry-after: %v", err)
	}
}

func TestGitHubTimeRange(t *testing.T) {
	if got := githubTimeRange(""); got != "" {
		t.Fatalf("empty should not map: %q", got)
	}
	if got := githubTimeRange("OneWeek"); !strings.HasPrefix(got, ">=") {
		t.Fatalf("OneWeek should be a >= date: %q", got)
	}
	if got := githubTimeRange("2024-01-01..2024-01-31"); got != "2024-01-01..2024-01-31" {
		t.Fatalf("range should pass through: %q", got)
	}
	if got := githubTimeRange("bogus"); got != "" {
		t.Fatalf("bogus should not map: %q", got)
	}
	if got := githubTimeRange("2024-13-99..nope"); got != "" {
		t.Fatalf("invalid dates should not map: %q", got)
	}
}

// fmtQuery formats url.Values for failure messages.
func fmtQuery(v url.Values) string { return v.Encode() }

// youtubeOK is a trimmed real-shape /youtube/v3/search response: a video, a
// live broadcast (HTML-escaped title), and a channel (exercises the id-kind
// URL mapping).
const youtubeOK = `{
  "kind": "youtube#searchListResponse",
  "nextPageToken": "CAUQAA",
  "regionCode": "HK",
  "pageInfo": {"totalResults": 1000000, "resultsPerPage": 3},
  "items": [
    {
      "id": {"kind": "youtube#video", "videoId": "dQw4w9WgXcQ"},
      "snippet": {
        "publishedAt": "2009-10-25T06:57:33Z",
        "channelId": "UCuAXFkgsw1L7xaCfnd5JJOw",
        "title": "Rick Astley - Never Gonna Give You Up (Official Video)",
        "description": "The official video for Never Gonna Give You Up.",
        "channelTitle": "Rick Astley",
        "liveBroadcastContent": "none"
      }
    },
    {
      "id": {"kind": "youtube#video", "videoId": "jfKfPfyJRdk"},
      "snippet": {
        "publishedAt": "2026-09-09T00:00:00Z",
        "channelId": "UCxxxxxxxx",
        "title": "lofi hip hop radio &#127925; beats to relax/study to &amp; chill",
        "description": "24/7 stream",
        "channelTitle": "Lofi Girl",
        "liveBroadcastContent": "live"
      }
    },
    {
      "id": {"kind": "youtube#channel", "channelId": "UCuAXFkgsw1L7xaCfnd5JJOw"},
      "snippet": {
        "publishedAt": "2015-10-06T00:00:00Z",
        "title": "Rick Astley",
        "description": "Official channel.",
        "channelTitle": "Rick Astley",
        "liveBroadcastContent": "none"
      }
    }
  ]
}`

func TestYouTubeSearch(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, youtubeOK)
	}))
	defer srv.Close()

	y, err := newYouTube(config.SearchEngine{BaseURL: srv.URL, APIKey: "yk"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := y.Search(context.Background(), Request{Query: "rick astley", Count: 3, TimeRange: "OneYear"})
	if err != nil {
		t.Fatal(err)
	}
	// Wire params per the spec.
	if gotQuery.Get("part") != "snippet" || gotQuery.Get("type") != "video" || gotQuery.Get("order") != "relevance" {
		t.Fatalf("params: %s", gotQuery.Encode())
	}
	if gotQuery.Get("q") != "rick astley" || gotQuery.Get("maxResults") != "3" {
		t.Fatalf("query/count: %s", gotQuery.Encode())
	}
	if gotQuery.Get("key") != "yk" {
		t.Fatalf("key param: %q", gotQuery.Get("key"))
	}
	if gotQuery.Get("publishedAfter") == "" {
		t.Fatal("TimeRange should map to publishedAfter")
	}
	// Mapping.
	if len(res.Results) != 3 {
		t.Fatalf("results: %+v", res)
	}
	w := res.Results[0]
	if w.Title != "Rick Astley - Never Gonna Give You Up (Official Video)" {
		t.Fatalf("title: %q", w.Title)
	}
	if w.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Fatalf("video url: %q", w.URL)
	}
	if w.Site != "Rick Astley" || w.Content != "The official video for Never Gonna Give You Up." {
		t.Fatalf("site/content: %+v", w)
	}
	if w.Published != "2009-10-25T06:57:33Z" {
		t.Fatalf("published: %q", w.Published)
	}
	// Live broadcast: entities decoded, LIVE prefix on the snippet.
	l := res.Results[1]
	if l.Title != "lofi hip hop radio 🎵 beats to relax/study to & chill" {
		t.Fatalf("title not entity-decoded: %q", l.Title)
	}
	if !strings.HasPrefix(l.Snippet, "LIVE: ") {
		t.Fatalf("live snippet: %q", l.Snippet)
	}
	// Channel id-kind maps to the channel URL.
	if res.Results[2].URL != "https://www.youtube.com/channel/UCuAXFkgsw1L7xaCfnd5JJOw" {
		t.Fatalf("channel url: %q", res.Results[2].URL)
	}
}

func TestYouTubeQuotaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"message":"The request cannot be completed because you have exceeded your quota.","errors":[{"reason":"quotaExceeded"}]}}`)
	}))
	defer srv.Close()
	y, err := newYouTube(config.SearchEngine{BaseURL: srv.URL, APIKey: "sekret-yt-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = y.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "quotaExceeded") || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("quota error should surface the reason: %v", err)
	}
	if strings.Contains(err.Error(), "sekret-yt-key") {
		t.Fatalf("error must not leak the api key: %v", err)
	}
}

func TestYouTubeTimeRange(t *testing.T) {
	if _, _, ok := youtubeTimeRange(""); ok {
		t.Fatal("empty should not map")
	}
	after, before, ok := youtubeTimeRange("OneWeek")
	if !ok || after == "" || before != "" {
		t.Fatalf("OneWeek should set only publishedAfter: %q %q", after, before)
	}
	after, before, ok = youtubeTimeRange("2024-01-01..2024-01-31")
	if !ok || after != "2024-01-01T00:00:00Z" || before != "2024-02-01T00:00:00Z" {
		t.Fatalf("range: %q..%q", after, before)
	}
	if _, _, ok := youtubeTimeRange("bogus"); ok {
		t.Fatal("bogus should not map")
	}
}

func TestYouTubeRequiresKey(t *testing.T) {
	if _, err := newYouTube(config.SearchEngine{}); err == nil {
		t.Fatal("youtube without api_key should fail")
	}
}

// --- Set / Build --------------------------------------------------------------

// bothEnginesServer answers doubao (PascalCase body) and ollama requests from
// one endpoint, so multi-engine dispatch can be tested against one mock.
func bothEnginesServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "SearchType") {
			io.WriteString(w, doubaoOK)
			return
		}
		io.WriteString(w, ollamaOK)
	}))
}

func TestBuildAndSetDispatch(t *testing.T) {
	srv := bothEnginesServer(t)
	defer srv.Close()

	set, err := Build(config.Search{
		Default: "ollama",
		Engines: map[string]config.SearchEngine{
			"doubao": {APIKey: "dk", BaseURL: srv.URL},
			"ollama": {APIKey: "ok", BaseURL: srv.URL},
		},
	}, &config.Resolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := set.Names(); strings.Join(names, ",") != "doubao,ollama" {
		t.Fatalf("names: %v", names)
	}

	// Explicit engine dispatches correctly.
	used, res, err := set.Search(context.Background(), "doubao", Request{Query: "beijing"})
	if err != nil || used != "doubao" || res.Engine != "doubao" || len(res.Results) != 1 {
		t.Fatalf("doubao dispatch: %v %v", used, err)
	}
	// Omitted engine uses the default.
	used, res, err = set.Search(context.Background(), "", Request{Query: "ollama"})
	if err != nil || used != "ollama" || len(res.Results) != 2 {
		t.Fatalf("default dispatch: %v %v", used, err)
	}
	// Unknown engine names the configured set.
	_, _, err = set.Search(context.Background(), "reddit", Request{Query: "go"})
	if err == nil || !strings.Contains(err.Error(), "doubao, ollama") {
		t.Fatalf("unknown engine error: %v", err)
	}
}

func TestBuildEmptyConfig(t *testing.T) {
	set, err := Build(config.Search{}, &config.Resolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Empty() {
		t.Fatal("empty config should build an empty set")
	}
}

func TestBuildRequiresKeys(t *testing.T) {
	if _, err := NewEngine("doubao", config.SearchEngine{}); err == nil {
		t.Fatal("doubao without key should fail")
	}
	if _, err := NewEngine("ollama", config.SearchEngine{}); err == nil {
		t.Fatal("ollama without key should fail")
	}
	if _, err := NewEngine("youtube", config.SearchEngine{}); err == nil {
		t.Fatal("youtube without key should fail")
	}
	if _, err := NewEngine("reddit", config.SearchEngine{}); err == nil {
		t.Fatal("removed engine should fail (reddit was dropped: public .json deprecated, OAuth is commercial-only)")
	}
	if _, err := NewEngine("brave", config.SearchEngine{}); err == nil {
		t.Fatal("unsupported engine should fail")
	}
}
