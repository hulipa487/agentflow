package legal

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// npcSearchOK is a real-shape /law-search/search/list response (two rows; the
// second carries the <em class='highlight'> marks the API wraps around matched
// title terms).
const npcSearchOK = `{
  "code": 200,
  "msg": "查询成功",
  "total": 2971,
  "rows": [
    {"bbbs":"ff8081819ff54a6401a032af29bf2887","title":"中华人民共和国安全生产法","gbrq":"2021-06-10","sxrq":"2021-09-01","sxx":3,"zdjgName":"全国人民代表大会常务委员会","flxz":"法律","score":9.9},
    {"bbbs":"bbbbb2","title":"<em class='highlight'>安全生产</em>条例","gbrq":"2019-01-01","sxrq":"2019-03-01","sxx":2,"zdjgName":"国务院","flxz":"行政法规","score":7.1}
  ]
}`

func TestNPCSearch(t *testing.T) {
	var gotBody npcSearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, npcSearchOK)
	}))
	defer srv.Close()

	n, err := newNPC(config.SearchEngine{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := n.Search(context.Background(), Request{Query: "安全生产法", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.SearchContent != "安全生产法" {
		t.Fatalf("searchContent: %q", gotBody.SearchContent)
	}
	if gotBody.SearchType != 2 { // full-text (fuzzy) by default
		t.Fatalf("searchType default: %d", gotBody.SearchType)
	}
	if gotBody.PageNum != 1 || gotBody.PageSize != 5 {
		t.Fatalf("paging: %d/%d", gotBody.PageNum, gotBody.PageSize)
	}
	if res.Total != 2971 {
		t.Fatalf("total: %d", res.Total)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results: %d", len(res.Results))
	}
	c := res.Results[0]
	if c.Title != "中华人民共和国安全生产法" || c.Path != "ff8081819ff54a6401a032af29bf2887" {
		t.Fatalf("case: %+v", c)
	}
	if c.Court != "全国人民代表大会常务委员会" || c.Date != "2021-06-10" {
		t.Fatalf("organ/date: %+v", c)
	}
	if c.Type != "法律" || c.Effective != "2021-09-01" || c.Status != "有效" {
		t.Fatalf("legislation fields: %+v", c)
	}
	if !strings.HasPrefix(c.URL, srv.URL+"/detail2.html?") {
		t.Fatalf("detail url: %q", c.URL)
	}
	// Row 2: <em> highlight marks stripped, status code 2 → 已修改.
	if res.Results[1].Title != "安全生产条例" {
		t.Fatalf("highlight strip: %q", res.Results[1].Title)
	}
	if res.Results[1].Status != "已修改" {
		t.Fatalf("status map: %q", res.Results[1].Status)
	}
}

func TestNPCSearchDisableFuzzy(t *testing.T) {
	var gotBody npcSearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, npcSearchOK)
	}))
	defer srv.Close()
	n, _ := newNPC(config.SearchEngine{BaseURL: srv.URL})
	if _, err := n.Search(context.Background(), Request{Query: "q", DisableFuzzy: true}); err != nil {
		t.Fatal(err)
	}
	if gotBody.SearchType != 1 { // exact → title match only
		t.Fatalf("searchType: %d", gotBody.SearchType)
	}
}

// buildNPCDocx builds a minimal .docx (zip holding word/document.xml) with the
// given paragraphs.
func buildNPCDocx(t *testing.T, paras ...string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?><w:document xmlns:w="w"><w:body>`)
	for _, p := range paras {
		sb.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestNPCFetch exercises the two-step fetch: download/pc returns a signed URL,
// then the docx bytes are fetched and run through docparse.
func TestNPCFetch(t *testing.T) {
	docx := buildNPCDocx(t, "中华人民共和国安全生产法", "第一章 总则", "第一条 为了加强安全生产工作。")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/law-search/download/pc") {
			// Echo a self-referential signed URL at /object.
			dl, _ := json.Marshal(map[string]any{
				"code": 200, "msg": "操作成功",
				"data": map[string]string{"url": "http://" + r.Host + "/object/law.docx", "urlIn": ""},
			})
			w.Write(dl)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/object/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(docx)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	n, _ := newNPC(config.SearchEngine{BaseURL: srv.URL})
	j, err := n.Fetch(context.Background(), FetchRequest{Path: "ff8081819ff54a6401a032af29bf2887"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "中华人民共和国安全生产法" {
		t.Fatalf("title from first line: %q", j.Title)
	}
	if j.Neutral != "ff8081819ff54a6401a032af29bf2887" {
		t.Fatalf("neutral = bbbs: %q", j.Neutral)
	}
	if !strings.Contains(j.Text, "第一章 总则") || !strings.Contains(j.Text, "第一条") {
		t.Fatalf("docx text: %q", j.Text)
	}
	if j.TextLen != len(j.Text) || j.Truncated {
		t.Fatalf("unpaged fetch should be full: len=%d textlen=%d truncated=%v", len(j.Text), j.TextLen, j.Truncated)
	}
}

func TestNPCFetchPaging(t *testing.T) {
	docx := buildNPCDocx(t, "中华人民共和国安全生产法", "第一章 总则")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/law-search/download/pc") {
			dl, _ := json.Marshal(map[string]any{"code": 200, "data": map[string]string{"url": "http://" + r.Host + "/object/law.docx"}})
			w.Write(dl)
			return
		}
		w.Write(docx)
	}))
	defer srv.Close()
	n, _ := newNPC(config.SearchEngine{BaseURL: srv.URL})
	full, err := n.Fetch(context.Background(), FetchRequest{Path: "x"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := n.Fetch(context.Background(), FetchRequest{Path: "x", MaxChars: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Text) != 20 || !j.Truncated || j.TextLen != full.TextLen {
		t.Fatalf("max_chars: len=%d truncated=%v textlen=%d", len(j.Text), j.Truncated, j.TextLen)
	}
}

func TestNPCFetchDownloadFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":500,"msg":"操作失败","data":{}}`)
	}))
	defer srv.Close()
	n, _ := newNPC(config.SearchEngine{BaseURL: srv.URL})
	if _, err := n.Fetch(context.Background(), FetchRequest{Path: "bad"}); err == nil {
		t.Fatal("expected download failure to surface")
	}
}

func TestNPCNewEngine(t *testing.T) {
	if _, err := NewEngine("npc", config.SearchEngine{}); err != nil {
		t.Fatalf("npc needs no key: %v", err)
	}
}
