package legal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// hkliiSearchOK is a real-shape simplesearch response: two cases and one piece
// of legislation (count is the total; results is the returned array).
const hkliiSearchOK = `{
  "count": 3,
  "results": [
    {"title":"HKSAR V. YUNG WAI SHING AND ANOTHER","path":"/en/cases/hkcfi/2020/1808","pub_date":"2020-08-06T00:00:00+08:00","db":"Court of First Instance","act":"HCMA101/2020","neutral":"[2020] HKCFI 1808","parallel":"[2021] 1 HKLRD 919","coram":"Before: High Court Judge Albert Wong in Court","parties":"BETWEEN HKSAR Respondent and Yung Wai Shing (D1)"},
    {"title":"HKSAR V. CHAN CHUN KIT","path":"/en/cases/hkca/2021/1493","pub_date":"2021-10-11T00:00:00+08:00","db":"Court of Appeal","act":"HCMA242/2020","neutral":"[2021] HKCA 1493","parallel":"[2022] 3 HKLRD 588","coram":"Before: Poon CJHC","parties":"HKSAR v CHAN CHUN KIT"},
    {"title":"Prevention of Bribery Ordinance","path":"/en/legis/ord/201/","pub_date":"2024-01-01T00:00:00+08:00","db":"Hong Kong Ordinances","act":"","neutral":"","parallel":"","coram":"","parties":""}
  ],
  "classification": {"type":"ENTITY","entities":[]},
  "recommendation": {"message":"Not concept search","data":[]}
}`

// hkliiJudgmentOK is a trimmed real-shape getjudgment response (HTML body).
const hkliiJudgmentOK = `{
  "date":"2020-08-06T00:00:00+08:00",
  "db":"Court of First Instance",
  "neutral":"[2020] HKCFI 1808",
  "content":"<html><body><p>HCMA 101/2020</p><p><a class=\"para\" id=\"p1\">1.</a> The appellants were convicted.</p><p><a class=\"para\" id=\"p2\">2.</a> Second &amp; final paragraph.</p></body></html>",
  "doc":"https://legalref.judiciary.hk/doc/judg/word/vetted/other/ch/2020/HCMA000101_2020.doc",
  "cases":[{"title":"HKSAR v Yung Wai Shing and Another","act":"HCMA101/2020"}],
  "corrs":[],
  "parallel_citation":["[2021] 1 HKLRD 919"],
  "is_translation":false,
  "has_translation":true
}`

func TestHKLIISearch(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, hkliiSearchOK)
	}))
	defer srv.Close()

	h, err := newHKLII(config.SearchEngine{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Count 2 < the 3 returned: applied client-side; Total stays the API count.
	res, err := h.Search(context.Background(), Request{Query: "chu tsz wai", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("searchstring") != "chu tsz wai" {
		t.Fatalf("searchstring: %s", gotQuery.Encode())
	}
	if gotQuery.Get("disablefuzzy") != "0" { // fuzzy on by default
		t.Fatalf("disablefuzzy default: %q", gotQuery.Get("disablefuzzy"))
	}
	if res.Total != 3 {
		t.Fatalf("total: %d", res.Total)
	}
	if len(res.Results) != 2 {
		t.Fatalf("client-side count truncation: %d results", len(res.Results))
	}
	c := res.Results[0]
	if c.Title != "HKSAR V. YUNG WAI SHING AND ANOTHER" || c.Path != "/en/cases/hkcfi/2020/1808" {
		t.Fatalf("case: %+v", c)
	}
	if c.URL != srv.URL+"/en/cases/hkcfi/2020/1808" {
		t.Fatalf("url: %q", c.URL)
	}
	if c.Court != "Court of First Instance" || c.Date != "2020-08-06T00:00:00+08:00" {
		t.Fatalf("court/date: %+v", c)
	}
	if c.Neutral != "[2020] HKCFI 1808" || c.Parallel != "[2021] 1 HKLRD 919" || c.Action != "HCMA101/2020" {
		t.Fatalf("citations/action: %+v", c)
	}
	if c.Parties == "" || c.Coram == "" {
		t.Fatalf("parties/coram: %+v", c)
	}
}

func TestHKLIISearchDisableFuzzy(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, hkliiSearchOK)
	}))
	defer srv.Close()
	h, _ := newHKLII(config.SearchEngine{BaseURL: srv.URL})
	if _, err := h.Search(context.Background(), Request{Query: "q", DisableFuzzy: true}); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("disablefuzzy") != "1" {
		t.Fatalf("disablefuzzy: %q", gotQuery.Get("disablefuzzy"))
	}
}

func TestHKLIIFetch(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, hkliiJudgmentOK)
	}))
	defer srv.Close()
	h, _ := newHKLII(config.SearchEngine{BaseURL: srv.URL})
	j, err := h.Fetch(context.Background(), FetchRequest{Path: "/en/cases/hkcfi/2020/1808"})
	if err != nil {
		t.Fatal(err)
	}
	// The path decomposes into the getjudgment params.
	if gotQuery.Get("lang") != "en" || gotQuery.Get("abbr") != "hkcfi" || gotQuery.Get("year") != "2020" || gotQuery.Get("num") != "1808" {
		t.Fatalf("params: %s", gotQuery.Encode())
	}
	if j.Title != "HKSAR v Yung Wai Shing and Another" || j.Neutral != "[2020] HKCFI 1808" {
		t.Fatalf("title/neutral: %+v", j)
	}
	if len(j.Parallel) != 1 || j.Parallel[0] != "[2021] 1 HKLRD 919" {
		t.Fatalf("parallel: %+v", j.Parallel)
	}
	if !j.HasTranslation || j.DocURL == "" {
		t.Fatalf("translation/doc: %+v", j)
	}
	// HTML stripped, paragraph breaks + numbers preserved, entities decoded.
	if strings.ContainsAny(j.Text, "<>") {
		t.Fatalf("text not stripped: %q", j.Text)
	}
	if !strings.Contains(j.Text, "1. The appellants were convicted.") {
		t.Fatalf("paragraph 1: %q", j.Text)
	}
	if !strings.Contains(j.Text, "2. Second & final paragraph.") {
		t.Fatalf("paragraph 2 (entity decode): %q", j.Text)
	}
	if j.TextLen != len(j.Text) || j.Truncated {
		t.Fatalf("unpaged fetch should be full length: len=%d textlen=%d truncated=%v", len(j.Text), j.TextLen, j.Truncated)
	}
}

func TestHKLIIFetchPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, hkliiJudgmentOK)
	}))
	defer srv.Close()
	h, _ := newHKLII(config.SearchEngine{BaseURL: srv.URL})
	full, err := h.Fetch(context.Background(), FetchRequest{Path: "/en/cases/hkcfi/2020/1808"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := h.Fetch(context.Background(), FetchRequest{Path: "/en/cases/hkcfi/2020/1808", MaxChars: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Text) != 20 || !j.Truncated || j.TextLen != full.TextLen {
		t.Fatalf("max_chars: len=%d truncated=%v textlen=%d", len(j.Text), j.Truncated, j.TextLen)
	}
	j2, err := h.Fetch(context.Background(), FetchRequest{Path: "/en/cases/hkcfi/2020/1808", Start: 10, MaxChars: 20})
	if err != nil {
		t.Fatal(err)
	}
	if j2.Text != full.Text[10:30] {
		t.Fatalf("start paging: %q want %q", j2.Text, full.Text[10:30])
	}
}

func TestHKLIIFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h, _ := newHKLII(config.SearchEngine{BaseURL: srv.URL})
	_, err := h.Fetch(context.Background(), FetchRequest{Path: "/en/cases/hkcfi/2020/1808"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestFetchIdentity(t *testing.T) {
	// From a path; lang taken from the path.
	l, a, y, n, err := FetchRequest{Path: "/en/cases/hkcfi/2020/1808"}.identity()
	if err != nil || l != "en" || a != "hkcfi" || y != "2020" || n != "1808" {
		t.Fatalf("path identity: %v %q %q %q %q", err, l, a, y, n)
	}
	// Explicit lang overrides the path's.
	l, _, _, _, err = FetchRequest{Path: "/en/cases/hkcfi/2020/1808", Lang: "tc"}.identity()
	if err != nil || l != "tc" {
		t.Fatalf("lang override: %v %q", err, l)
	}
	// Explicit abbr/year/num without a path; lang defaults to en.
	l, a, y, n, err = FetchRequest{Abbr: "hkca", Year: "2021", Num: "1493"}.identity()
	if err != nil || l != "en" || a != "hkca" || y != "2021" || n != "1493" {
		t.Fatalf("explicit identity: %v %q %q %q %q", err, l, a, y, n)
	}
	// A legislation path is not a case judgment.
	if _, _, _, _, err = (FetchRequest{Path: "/en/legis/ord/201/"}).identity(); err == nil {
		t.Fatal("legislation path should error")
	}
	// Nothing to identify the judgment.
	if _, _, _, _, err = (FetchRequest{}).identity(); err == nil {
		t.Fatal("empty identity should error")
	}
}

func TestJudgmentText(t *testing.T) {
	out := judgmentText(`<p>Intro</p><p><a class="para" id="p1">1.</a> Body &amp; more.</p>`)
	if strings.ContainsAny(out, "<>") {
		t.Fatalf("tags remain: %q", out)
	}
	if !strings.Contains(out, "Intro") || !strings.Contains(out, "1. Body & more.") {
		t.Fatalf("content: %q", out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatalf("expected paragraph breaks: %q", out)
	}
}

func TestBuildAndDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/getjudgment") {
			io.WriteString(w, hkliiJudgmentOK)
			return
		}
		io.WriteString(w, hkliiSearchOK)
	}))
	defer srv.Close()
	set, err := Build(config.LegalSearch{
		Default: "hklii",
		Engines: map[string]config.SearchEngine{"hklii": {BaseURL: srv.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := set.Names(); len(names) != 1 || names[0] != "hklii" {
		t.Fatalf("names: %v", names)
	}
	used, res, err := set.Search(context.Background(), "", Request{Query: "q"})
	if err != nil || used != "hklii" || res.Engine != "hklii" {
		t.Fatalf("search dispatch: %v %v", used, err)
	}
	used, j, err := set.Fetch(context.Background(), "", FetchRequest{Path: "/en/cases/hkcfi/2020/1808"})
	if err != nil || used != "hklii" || j.Neutral == "" {
		t.Fatalf("fetch dispatch: %v %v", used, err)
	}
	if _, _, err = set.Search(context.Background(), "westlaw", Request{Query: "q"}); err == nil {
		t.Fatal("unknown engine should error")
	}
}

func TestNewEngineGuard(t *testing.T) {
	if _, err := NewEngine("hklii", config.SearchEngine{}); err != nil {
		t.Fatalf("hklii needs no key: %v", err)
	}
	if _, err := NewEngine("westlaw", config.SearchEngine{}); err == nil {
		t.Fatal("unsupported engine should fail")
	}
	if set, _ := Build(config.LegalSearch{}); !set.Empty() {
		t.Fatal("empty config should build an empty set")
	}
}
