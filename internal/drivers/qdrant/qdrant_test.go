package qdrant

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/core/memory"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// The ops the fake records: the gRPC method each REST call became. The driver
// speaks gRPC now, so what the server sees is an RPC name and a request
// message, not a method and a path.
const (
	opGetCollection    = "Collections/Get"
	opCreateCollection = "Collections/Create"
	opUpsert           = "Points/Upsert"
	opSetPayload       = "Points/SetPayload"
	opGetPoint         = "Points/Get"
	opDelete           = "Points/Delete"
	opSearch           = "Points/Search"
)

// recorded is one call the fake Qdrant saw. The request messages stand in for
// the decoded JSON bodies the REST fake kept, so an assertion still reads what
// the driver actually put on the wire.
type recorded struct {
	op         string
	apiKey     string
	collection string
	create     *qdrant.CreateCollection
	upsert     *qdrant.UpsertPoints
	setPayload *qdrant.SetPayloadPoints
	getIDs     []string
	del        *qdrant.DeletePoints
	search     *qdrant.SearchPoints
}

// fakeQdrant stands in for the gRPC API. exists/size describe what the
// collection probe finds; searchHits is returned by the search RPC; getPoint is
// the payload of the point a get returns (nil => the collection holds none).
type fakeQdrant struct {
	mu         sync.Mutex
	seen       []recorded
	exists     bool
	size       int
	searchHits []*qdrant.ScoredPoint
	getPoint   map[string]*qdrant.Value
	// payloadMissing makes the payload-update RPC answer NOT_FOUND, which is
	// what the real API does when the selector matches no point.
	payloadMissing bool
}

func (f *fakeQdrant) record(ctx context.Context, r recorded) {
	// The api_key config value rides gRPC metadata rather than a header, set by
	// the client's own interceptor — so this is where "did it reach the server"
	// is observable.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("api-key"); len(v) > 0 {
			r.apiKey = v[0]
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r)
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

// fakeCollections serves the collections service. The two services both declare
// Get and Delete with different shapes, so one Go type cannot implement both:
// each is a thin handler over the one fake's state.
type fakeCollections struct {
	qdrant.UnimplementedCollectionsServer
	f *fakeQdrant
}

func (s *fakeCollections) Get(ctx context.Context, in *qdrant.GetCollectionInfoRequest) (*qdrant.GetCollectionInfoResponse, error) {
	s.f.record(ctx, recorded{op: opGetCollection, collection: in.GetCollectionName()})
	if !s.f.exists {
		// A real Qdrant reports a collection it does not hold as NOT_FOUND.
		return nil, status.Errorf(codes.NotFound, "Collection `%s` doesn't exist!", in.GetCollectionName())
	}
	return &qdrant.GetCollectionInfoResponse{Result: &qdrant.CollectionInfo{
		Config: &qdrant.CollectionConfig{Params: &qdrant.CollectionParams{
			VectorsConfig: qdrant.NewVectorsConfigMap(map[string]*qdrant.VectorParams{
				vectorName: {Size: uint64(s.f.size)},
			}),
		}},
	}}, nil
}

func (s *fakeCollections) Create(ctx context.Context, in *qdrant.CreateCollection) (*qdrant.CollectionOperationResponse, error) {
	s.f.record(ctx, recorded{op: opCreateCollection, collection: in.GetCollectionName(), create: in})
	s.f.exists = true
	s.f.size = int(in.GetVectorsConfig().GetParamsMap().GetMap()[vectorName].GetSize())
	return &qdrant.CollectionOperationResponse{Result: true}, nil
}

// fakePoints serves the points service.
type fakePoints struct {
	qdrant.UnimplementedPointsServer
	f *fakeQdrant
}

func (s *fakePoints) Upsert(ctx context.Context, in *qdrant.UpsertPoints) (*qdrant.PointsOperationResponse, error) {
	s.f.record(ctx, recorded{op: opUpsert, collection: in.GetCollectionName(), upsert: in})
	return completed(), nil
}

func (s *fakePoints) SetPayload(ctx context.Context, in *qdrant.SetPayloadPoints) (*qdrant.PointsOperationResponse, error) {
	s.f.record(ctx, recorded{op: opSetPayload, collection: in.GetCollectionName(), setPayload: in})
	if s.f.payloadMissing {
		// The gRPC twin of the REST payload endpoint's 404: the selector matched
		// no point in the collection.
		return nil, status.Error(codes.NotFound, "No point with the given id exists")
	}
	return completed(), nil
}

func (s *fakePoints) Get(ctx context.Context, in *qdrant.GetPoints) (*qdrant.GetResponse, error) {
	rec := recorded{op: opGetPoint, collection: in.GetCollectionName()}
	for _, id := range in.GetIds() {
		rec.getIDs = append(rec.getIDs, id.GetUuid())
	}
	s.f.record(ctx, rec)
	// A point the collection does not hold comes back as an empty result, not
	// as an error.
	if s.f.getPoint == nil {
		return &qdrant.GetResponse{}, nil
	}
	return &qdrant.GetResponse{Result: []*qdrant.RetrievedPoint{{
		Id:      in.GetIds()[0],
		Payload: s.f.getPoint,
	}}}, nil
}

func (s *fakePoints) Delete(ctx context.Context, in *qdrant.DeletePoints) (*qdrant.PointsOperationResponse, error) {
	s.f.record(ctx, recorded{op: opDelete, collection: in.GetCollectionName(), del: in})
	return completed(), nil
}

func (s *fakePoints) Search(ctx context.Context, in *qdrant.SearchPoints) (*qdrant.SearchResponse, error) {
	s.f.record(ctx, recorded{op: opSearch, collection: in.GetCollectionName(), search: in})
	// No hits means an empty reply, as the REST fake's nil result list did.
	return &qdrant.SearchResponse{Result: s.f.searchHits}, nil
}

// completed is the "operation finished" reply the write RPCs answer with.
func completed() *qdrant.PointsOperationResponse {
	return &qdrant.PointsOperationResponse{Result: &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Completed}}
}

// start runs the fake over a bufconn listener and points the driver's dial seam
// at it. The returned URL is the ordinary REST one: a bufconn listener has no
// address to name, so the dialer ignores the host and port the driver resolves,
// but passing a real-looking URL keeps the config the tests exercise — and the
// port translation — honest.
func (f *fakeQdrant) start(t *testing.T) string {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	qdrant.RegisterPointsServer(srv, &fakePoints{f: f})
	qdrant.RegisterCollectionsServer(srv, &fakeCollections{f: f})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(func() { _ = lis.Close() })

	dialOptions = []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})}
	t.Cleanup(func() { dialOptions = nil })
	// An IP literal, so the client's resolver never needs a lookup.
	return "http://127.0.0.1:6333"
}

func openFake(t *testing.T, f *fakeQdrant, cfg map[string]any) *Handle {
	t.Helper()
	cfg["url"] = f.start(t)
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

// TestDialTargetTranslatesTheURL: the config stays a REST URL, so a port that
// names REST has to become the gRPC one — every existing deployment's URL says
// :6333, and dialling that would hang. An explicit port that is not the REST
// default is somebody's gRPC proxy and is left alone.
func TestDialTargetTranslatesTheURL(t *testing.T) {
	for _, tc := range []struct {
		url    string
		host   string
		port   int
		useTLS bool
	}{
		{"http://localhost:6333", "localhost", 6334, false},
		{"http://qdrant.internal", "qdrant.internal", 6334, false},
		{"https://qdrant.cloud:6333", "qdrant.cloud", 6334, true},
		{"http://127.0.0.1:7777", "127.0.0.1", 7777, false},
		{"localhost:6333", "localhost", 6334, false},
	} {
		host, port, useTLS, err := dialTarget(tc.url)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if host != tc.host || port != tc.port || useTLS != tc.useTLS {
			t.Fatalf("%s: got %s:%d tls=%v; want %s:%d tls=%v", tc.url, host, port, useTLS, tc.host, tc.port, tc.useTLS)
		}
	}
	if _, _, _, err := dialTarget("http://:6333"); err == nil {
		t.Fatal("a url with no host must fail")
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
	if reqs[0].op != opGetCollection {
		t.Fatalf("first call should probe the collection: %+v", reqs[0])
	}
	if reqs[0].collection != "agentflow" {
		t.Fatalf("probe named the wrong collection: %+v", reqs[0])
	}
	create := reqs[1]
	if create.op != opCreateCollection || create.collection != "agentflow" {
		t.Fatalf("create call wrong: %+v", create)
	}
	named := create.create.GetVectorsConfig().GetParamsMap().GetMap()[vectorName]
	if named == nil {
		t.Fatalf("collection created without the named vector: %v", create.create.GetVectorsConfig())
	}
	if named.GetSize() != 8 || named.GetDistance() != qdrant.Distance_Dot {
		t.Fatalf("collection vector config wrong: size=%d distance=%v", named.GetSize(), named.GetDistance())
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

// TestOpenSendsAPIKey: an api_key config value reaches the server, which over
// gRPC means the api-key metadata entry the client's interceptor sets from
// Config.APIKey.
func TestOpenSendsAPIKey(t *testing.T) {
	f := &fakeQdrant{exists: true, size: 8}
	openFake(t, f, map[string]any{"dim": 8, "api_key": "sekret"})
	if got := f.last(t).apiKey; got != "sekret" {
		t.Fatalf("api-key metadata = %q; want sekret", got)
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
	if _, err := (Provider{}).Open(map[string]any{"url": f.start(t), "distance": "Hamming"}); err == nil {
		t.Fatal("an unknown distance must fail")
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
	if got.op != opUpsert || got.collection != "agentflow" {
		t.Fatalf("upsert call wrong: %+v", got)
	}
	// The REST call's ?wait=true: without it a read issued right after the
	// write can still miss the point.
	if !got.upsert.GetWait() {
		t.Fatalf("upsert must wait: %+v", got.upsert)
	}
	points := got.upsert.GetPoints()
	if len(points) != 1 {
		t.Fatalf("want one point, got %v", points)
	}
	p := points[0]
	if p.GetId().GetUuid() != pointID("writer.facts", "k1") {
		t.Fatalf("point id = %v; want the deterministic id", p.GetId())
	}
	emb := p.GetVectors().GetVectors().GetVectors()[vectorName].GetDense().GetData()
	if len(emb) != 3 || emb[0] != 1 {
		t.Fatalf("named vector wrong: %v", p.GetVectors())
	}
	payload := p.GetPayload()
	if payload[tableField].GetStringValue() != "writer.facts" || payload["key"].GetStringValue() != "k1" {
		t.Fatalf("payload wrong: %v", payload)
	}
	// The value is arbitrary JSON and must travel as a structure, not as a
	// string that only this driver knows how to unwrap.
	if text := payload["value"].GetStructValue().GetFields()["text"].GetStringValue(); text != "hi" {
		t.Fatalf("value not stored as a structure: %v", payload["value"])
	}
}

// TestPutWithoutVectorPreservesEmbedding: a value written with no embedding
// goes through the payload RPC so the vector a previous put stored is not
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
		switch r.op {
		case opSetPayload:
			payloadUpdate = true
			if v := r.setPayload.GetPayload()["value"].GetStringValue(); v != "plain" {
				t.Fatalf("payload update carried %q; want plain: %+v", v, r.setPayload)
			}
			if ids := r.setPayload.GetPointsSelector().GetPoints().GetIds(); len(ids) != 1 || ids[0].GetUuid() != pointID("t", "k") {
				t.Fatalf("payload update targeted the wrong point: %v", ids)
			}
		case opUpsert:
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
// payload RPC answers NOT_FOUND and the value is stored by creating the point
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
		if r.op != opUpsert {
			continue
		}
		points := r.upsert.GetPoints()
		if len(points) != 1 {
			t.Fatalf("want one point, got %v", points)
		}
		p := points[0]
		if p.GetId().GetUuid() != pointID("t", "k") {
			t.Fatalf("created point id = %v", p.GetId())
		}
		if p.GetVectors() == nil {
			t.Fatalf("a create without a vector field is rejected by the API: %v", p)
		}
		if named := p.GetVectors().GetVectors(); named == nil || len(named.GetVectors()) != 0 {
			t.Fatalf("a vectorless create must carry an empty vector set: %v", p.GetVectors())
		}
		payload := p.GetPayload()
		if payload["key"].GetStringValue() != "k" || payload["value"].GetStringValue() != "plain" {
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
	f := &fakeQdrant{exists: true, size: 2, searchHits: []*qdrant.ScoredPoint{
		{Id: qdrant.NewID("a"), Score: 0.9, Payload: qdrant.NewValueMap(map[string]any{"key": "k1", "value": map[string]any{"text": "one"}})},
		{Id: qdrant.NewID("b"), Score: 0.5, Payload: qdrant.NewValueMap(map[string]any{"key": "k2", "value": "two"})},
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

	req := f.last(t).search
	if req.GetLimit() != 2 {
		t.Fatalf("limit wrong: %v", req.GetLimit())
	}
	// Without the payload the hits carry no key and no value to return.
	if !req.GetWithPayload().GetEnable() {
		t.Fatal("search must ask for the payload")
	}
	qv := req.GetVector()
	if len(qv) != 2 || qv[0] != 1 || qv[1] != 0 {
		t.Fatalf("query vector wrong: %v", qv)
	}
	if req.GetVectorName() != vectorName {
		t.Fatalf("query vector not named: %q", req.GetVectorName())
	}
	must := req.GetFilter().GetMust()
	if len(must) != 1 {
		t.Fatalf("table filter missing: %v", req.GetFilter())
	}
	field := must[0].GetField()
	if field.GetKey() != tableField {
		t.Fatalf("filter key wrong: %v", field)
	}
	if field.GetMatch().GetKeyword() != "writer.facts" {
		t.Fatalf("filter value wrong: %v", field.GetMatch())
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
	point := qdrant.NewValueMap(map[string]any{
		"table": "t", "key": "k", "value": map[string]any{"n": 1},
	})
	f := &fakeQdrant{exists: true, size: 2, getPoint: point}
	h := openFake(t, f, map[string]any{"dim": 2})

	v, ok, err := h.Get("t", "k")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if m, _ := v.(map[string]any); m["n"] != float64(1) {
		t.Fatalf("value wrong: %#v", v)
	}
	got := f.last(t)
	if got.op != opGetPoint || len(got.getIDs) != 1 || got.getIDs[0] != pointID("t", "k") {
		t.Fatalf("get call wrong: %+v", got)
	}

	// A point that exists under a different key must not be mistaken for this
	// one (the id is a hash, so the payload is the authority).
	f.getPoint = qdrant.NewValueMap(map[string]any{"table": "t", "key": "other", "value": "x"})
	if _, ok, err := h.Get("t", "k"); ok || err != nil {
		t.Fatalf("a mismatched payload must read as absent: ok=%v err=%v", ok, err)
	}

	// A point the collection does not hold at all: the API answers an empty
	// result rather than an error, and that must read as absent too.
	f.getPoint = nil
	if _, ok, err := h.Get("t", "k"); ok || err != nil {
		t.Fatalf("an absent point must read as absent: ok=%v err=%v", ok, err)
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
	if got.op != opDelete || got.collection != "agentflow" {
		t.Fatalf("delete call wrong: %+v", got)
	}
	ids := got.del.GetPoints().GetPoints().GetIds()
	if len(ids) != 1 || ids[0].GetUuid() != pointID("t", "k") {
		t.Fatalf("delete targeted the wrong point: %v", got.del.GetPoints())
	}
	// The REST call's ?wait=true, for the same reason the upsert carries it.
	if !got.del.GetWait() {
		t.Fatalf("delete must wait: %+v", got.del)
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
