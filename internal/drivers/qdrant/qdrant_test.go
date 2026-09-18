package qdrant

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/core/memory"
)

// recorded is one request the fake Qdrant saw.
type recorded struct {
	method string
	path   string
	body   map[string]any
	apiKey string
}

// fakeQdrant stands in for the REST API. exists/size describe what the
// collection probe finds; searchHits is returned by the search endpoint.
type fakeQdrant struct {
	mu         sync.Mutex
	seen       []recorded
	exists     bool
	size       int
	searchHits []map[string]any
	getPoint   map[string]any // nil => 404
	// payloadMissing makes the payload-update endpoint answer 404, which is
	// what the real API does for a point that does not exist.
	payloadMissing bool
}

func (f *fakeQdrant) record(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, recorded{method: r.Method, path: r.URL.Path, body: body, apiKey: r.Header.Get("api-key")})
}

func (f *fakeQdrant) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.seen...)
}

func (f *fakeQdrant) last(t *testing.T) recorded {
	t.Helper()
	got := f.requests()
	if len(got) == 0 {
		t.Fatal("no request reached the fake server")
	}
	return got[len(got)-1]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeQdrant) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/agentflow":
			if !f.exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"result": map[string]any{
				"config": map[string]any{"params": map[string]any{
					"vectors": map[string]any{vectorName: map[string]any{"size": f.size}},
				}},
			}})
		case strings.HasSuffix(r.URL.Path, "/points/payload"):
			if f.payloadMissing {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			hits := f.searchHits
			if hits == nil {
				hits = []map[string]any{}
			}
			writeJSON(w, map[string]any{"result": hits})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/"):
			if f.getPoint == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"result": f.getPoint})
		default:
			writeJSON(w, map[string]any{"result": map[string]any{"status": "completed"}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func openFake(t *testing.T, f *fakeQdrant, cfg map[string]any) *Handle {
	t.Helper()
	url := f.start(t)
	cfg["url"] = url
	h, err := Provider{}.Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h.(*Handle)
}

// TestOpenRequiresURL: a backend with no url is a loud config error.
func TestOpenRequiresURL(t *testing.T) {
	if _, err := (Provider{}).Open(map[string]any{}); err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("want a url error, got %v", err)
	}
}

// TestFeaturesIsVectorOnly: the provider advertises exactly the vector feature,
// which is what the Lua path feature-detects on.
func TestFeaturesIsVectorOnly(t *testing.T) {
	got := Provider{}.Features()
	if len(got) != 1 || got[0] != "vector" {
		t.Fatalf("Features() = %v; want [vector]", got)
	}
}

// TestOpenCreatesCollection: with no collection present the provider creates
// one carrying the named vector, the configured width and the distance metric.
func TestOpenCreatesCollection(t *testing.T) {
	f := &fakeQdrant{exists: false}
	openFake(t, f, map[string]any{"dim": 8, "distance": "Dot"})

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("want a probe then a create, got %d requests: %+v", len(reqs), reqs)
	}
	if reqs[0].method != http.MethodGet {
		t.Fatalf("first call should probe the collection: %+v", reqs[0])
	}
	create := reqs[1]
	if create.method != http.MethodPut || create.path != "/collections/agentflow" {
		t.Fatalf("create call wrong: %+v", create)
	}
	vectors, _ := create.body["vectors"].(map[string]any)
	named, _ := vectors[vectorName].(map[string]any)
	if named["size"] != float64(8) || named["distance"] != "Dot" {
		t.Fatalf("collection vector config wrong: %v", vectors)
	}
}

// TestOpenChecksExistingWidth: an existing collection of a different width
// fails at boot with a message that says what to do, rather than at the first
// write with a server-side error far from the cause.
func TestOpenChecksExistingWidth(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 1536}
	_, err := Provider{}.Open(map[string]any{"url": f.start(t), "dim": 8})
	if err == nil || !strings.Contains(err.Error(), "1536") || !strings.Contains(err.Error(), "dim is 8") {
		t.Fatalf("want a width mismatch error, got %v", err)
	}
}

// TestOpenSendsAPIKey: an api_key config value rides the api-key header.
func TestOpenSendsAPIKey(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 8}
	openFake(t, f, map[string]any{"dim": 8, "api_key": "sekret"})
	if got := f.last(t).apiKey; got != "sekret" {
		t.Fatalf("api-key header = %q; want sekret", got)
	}
}

// TestOpenRejectsBadDimAndTimeout: config typos are boot errors.
func TestOpenRejectsBadDimAndTimeout(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 8}
	if _, err := (Provider{}).Open(map[string]any{"url": f.start(t), "dim": 0}); err == nil {
		t.Fatal("dim 0 must fail")
	}
	if _, err := (Provider{}).Open(map[string]any{"url": f.start(t), "timeout": "soon"}); err == nil {
		t.Fatal("a garbage timeout must fail")
	}
}

// TestPutWithVectorUpsertsPoint: a vectorised put upserts one point carrying
// the deterministic id, the named vector, and the table/key/value payload.
func TestPutWithVectorUpsertsPoint(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 3}
	h := openFake(t, f, map[string]any{"dim": 3})

	vec := []float32{1, 2, 3}
	if err := h.Put("writer.facts", "k1", map[string]any{"text": "hi"}, memory.PutOpts{Vector: vec}); err != nil {
		t.Fatal(err)
	}
	got := f.last(t)
	if got.method != http.MethodPut || !strings.HasSuffix(got.path, "/points") {
		t.Fatalf("upsert call wrong: %+v", got)
	}
	points, _ := got.body["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("want one point, got %v", got.body)
	}
	p, _ := points[0].(map[string]any)
	if p["id"] != pointID("writer.facts", "k1") {
		t.Fatalf("point id = %v; want the deterministic id", p["id"])
	}
	vecs, _ := p["vector"].(map[string]any)
	emb, _ := vecs[vectorName].([]any)
	if len(emb) != 3 || emb[0] != float64(1) {
		t.Fatalf("named vector wrong: %v", vecs)
	}
	payload, _ := p["payload"].(map[string]any)
	if payload[tableField] != "writer.facts" || payload["key"] != "k1" {
		t.Fatalf("payload wrong: %v", payload)
	}
}

// TestPutWithoutVectorPreservesEmbedding: a value written with no embedding
// goes through the payload endpoint so the vector a previous put stored is not
// dropped — an upsert replaces the whole point, so an unconditional upsert
// here would clear the named vector. Nothing else may be sent for a point that
// already exists.
//
// This is the regression guard for that: against a real server, following the
// payload update with an upsert carrying "vector": {} wiped the stored
// embedding, and the point stopped coming back from k-NN searches.
func TestPutWithoutVectorPreservesEmbedding(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 3}
	h := openFake(t, f, map[string]any{"dim": 3})

	if err := h.Put("t", "k", "plain", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	var payloadUpdate, upsert bool
	for _, r := range f.requests() {
		if strings.HasSuffix(r.path, "/points/payload") {
			payloadUpdate = true
			continue
		}
		if strings.HasSuffix(r.path, "/points") && r.method == http.MethodPut {
			upsert = true
		}
	}
	if !payloadUpdate {
		t.Fatalf("no payload update issued: %+v", f.requests())
	}
	if upsert {
		t.Fatalf("an existing point must not be upserted — that clears its vector: %+v", f.requests())
	}
}

// TestPutWithoutVectorCreatesAbsentPoint: when the point does not exist the
// payload endpoint answers 404 and the value is stored by creating the point
// with an empty named-vector set. A create without a vector field at all is
// rejected by the API, so {} is the only way to hold a value with no embedding.
func TestPutWithoutVectorCreatesAbsentPoint(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 3, payloadMissing: true}
	h := openFake(t, f, map[string]any{"dim": 3})

	if err := h.Put("t", "k", "plain", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	var created bool
	for _, r := range f.requests() {
		if !(strings.HasSuffix(r.path, "/points") && r.method == http.MethodPut) {
			continue
		}
		points, _ := r.body["points"].([]any)
		if len(points) != 1 {
			t.Fatalf("want one point, got %v", r.body)
		}
		p, _ := points[0].(map[string]any)
		if p["id"] != pointID("t", "k") {
			t.Fatalf("created point id = %v", p["id"])
		}
		vec, has := p["vector"]
		if !has {
			t.Fatalf("a create without a vector field is rejected by the API: %v", p)
		}
		if named, _ := vec.(map[string]any); len(named) != 0 {
			t.Fatalf("a vectorless create must carry an empty vector set: %v", p)
		}
		payload, _ := p["payload"].(map[string]any)
		if payload["key"] != "k" || payload["value"] != "plain" {
			t.Fatalf("payload wrong: %v", payload)
		}
		created = true
	}
	if !created {
		t.Fatalf("an absent point was not created: %+v", f.requests())
	}
}

// TestQueryVectorFiltersOnTable: the search carries the named query vector, the
// limit and a table filter, and results come back in server order.
func TestQueryVectorFiltersOnTable(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 2, searchHits: []map[string]any{
		{"id": "a", "score": 0.9, "payload": map[string]any{"key": "k1", "value": map[string]any{"text": "one"}}},
		{"id": "b", "score": 0.5, "payload": map[string]any{"key": "k2", "value": "two"}},
	}}
	h := openFake(t, f, map[string]any{"dim": 2})

	it, err := h.Query("writer.facts", memory.Query{Kind: "vector", Vector: []float32{1, 0}, K: 2})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	var vals []any
	for it.Next() {
		rec := it.Record()
		keys = append(keys, rec.Key)
		vals = append(vals, rec.Value)
	}
	if it.Err() != nil {
		t.Fatal(it.Err())
	}
	if len(keys) != 2 || keys[0] != "k1" || keys[1] != "k2" {
		t.Fatalf("keys = %v; want [k1 k2] in server order", keys)
	}
	if m, ok := vals[0].(map[string]any); !ok || m["text"] != "one" {
		t.Fatalf("value not decoded: %#v", vals[0])
	}
	if vals[1] != "two" {
		t.Fatalf("scalar value not decoded: %#v", vals[1])
	}

	body := f.last(t).body
	if body["limit"] != float64(2) {
		t.Fatalf("limit wrong: %v", body["limit"])
	}
	qvec, _ := body["vector"].(map[string]any)
	if qvec["name"] != vectorName {
		t.Fatalf("query vector not named: %v", qvec)
	}
	filter, _ := body["filter"].(map[string]any)
	must, _ := filter["must"].([]any)
	if len(must) != 1 {
		t.Fatalf("table filter missing: %v", body["filter"])
	}
	clause, _ := must[0].(map[string]any)
	if clause["key"] != tableField {
		t.Fatalf("filter key wrong: %v", clause)
	}
	match, _ := clause["match"].(map[string]any)
	if match["value"] != "writer.facts" {
		t.Fatalf("filter value wrong: %v", match)
	}
}

// TestQueryRejectsNonVectorKinds: this provider is vector-only, like
// pgvector — anything else is an explicit error, never a silent empty.
func TestQueryRejectsNonVectorKinds(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 2}
	h := openFake(t, f, map[string]any{"dim": 2})
	for _, kind := range []string{"prefix", "text", "all", "time_range", ""} {
		if _, err := h.Query("t", memory.Query{Kind: kind}); err == nil ||
			!strings.Contains(err.Error(), "only vector queries are supported") {
			t.Fatalf("kind %q: want an explicit error, got %v", kind, err)
		}
	}
}

// TestDimMismatchIsLoud: a vector of the wrong width is refused before any
// network call, naming both numbers.
func TestDimMismatchIsLoud(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 2}
	h := openFake(t, f, map[string]any{"dim": 2})

	err := h.Put("t", "k", "v", memory.PutOpts{Vector: []float32{1, 2, 3}})
	if err == nil || !strings.Contains(err.Error(), "3 dimensions") || !strings.Contains(err.Error(), "configured for 2") {
		t.Fatalf("put: want a dimension error, got %v", err)
	}
	if _, err := h.Query("t", memory.Query{Kind: "vector", Vector: []float32{1}}); err == nil {
		t.Fatal("query: want a dimension error")
	}
	if _, err := h.Query("t", memory.Query{Kind: "vector"}); err == nil {
		t.Fatal("query with no vector must fail")
	}
}

// TestGetRoundTrip: a present point returns its stored value, an absent one
// reports not-found rather than an error.
func TestGetRoundTrip(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 2, getPoint: map[string]any{
		"id":      "a",
		"payload": map[string]any{"table": "t", "key": "k", "value": map[string]any{"n": float64(1)}},
	}}
	h := openFake(t, f, map[string]any{"dim": 2})

	v, ok, err := h.Get("t", "k")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if m, _ := v.(map[string]any); m["n"] != float64(1) {
		t.Fatalf("value wrong: %#v", v)
	}

	// A point that exists under a different key must not be mistaken for this
	// one (the id is a hash, so the payload is the authority).
	f.getPoint = map[string]any{"payload": map[string]any{"table": "t", "key": "other", "value": "x"}}
	if _, ok, err := h.Get("t", "k"); ok || err != nil {
		t.Fatalf("a mismatched payload must read as absent: ok=%v err=%v", ok, err)
	}

	f.getPoint = nil
	if _, ok, err := h.Get("t", "k"); ok || err != nil {
		t.Fatalf("404 must read as absent: ok=%v err=%v", ok, err)
	}
}

// TestDeleteAndGC: delete targets the deterministic id; GC is a no-op that
// never errors (a vector store carries no recency window).
func TestDeleteAndGC(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 2}
	h := openFake(t, f, map[string]any{"dim": 2})

	if err := h.Delete("t", "k"); err != nil {
		t.Fatal(err)
	}
	got := f.last(t)
	if got.method != http.MethodPost || !strings.HasSuffix(got.path, "/points/delete") {
		t.Fatalf("delete call wrong: %+v", got)
	}
	points, _ := got.body["points"].([]any)
	if len(points) != 1 || points[0] != pointID("t", "k") {
		t.Fatalf("delete targeted the wrong point: %v", got.body)
	}
	if err := h.GC("t", 100); err != nil {
		t.Fatalf("GC must be a no-op: %v", err)
	}
}

// TestPointIDIsStableAndTableScoped: ids are deterministic across calls, and
// the same key in two tables is two points.
func TestPointIDIsStableAndTableScoped(t *testing.T) {
	a := pointID("t1", "k")
	if a != pointID("t1", "k") {
		t.Fatal("point id is not stable")
	}
	if a == pointID("t2", "k") {
		t.Fatal("the same key in two tables must not share a point")
	}
	if a == pointID("t1", "k2") {
		t.Fatal("distinct keys must not share a point")
	}
}
