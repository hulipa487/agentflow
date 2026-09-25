// Package qdrant implements the qdrant vector backend provider. It
// provides the "vector" feature over Qdrant's gRPC API.
//
// One collection holds every logical table (config "collection", default
// "agentflow"), matching how pgvector puts every table in one vec_kv
// table. The physical table a store resolves to is agent + "." + store table
// (see memory.ResolveStoresFor), and a "." is not legal in a Qdrant collection
// name, so tables ride in the point payload and searches filter on them rather
// than mapping each table to its own collection.
//
// Only vector queries are supported, like pgvector: any other query
// kind is an explicit error, never a silent empty iterator.
//
// The config still takes a URL, because that is what deployments (and the
// secret references in internal/config) were written against, but a URL names a
// REST endpoint and this driver speaks gRPC. So the scheme picks the transport
// and the host names the server, while the port is translated: an explicit 6333
// — the REST port every documented URL carries — becomes the gRPC default 6334,
// any other explicit port is dialled as given so a gRPC proxy still works, and a
// URL with no port gets 6334. The path is ignored (Qdrant serves gRPC at the
// root); an https URL dials with TLS.
package qdrant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/core/memory"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Provider implements memory.BackendProvider for "qdrant".
type Provider struct{}

func (Provider) Name() string { return "qdrant" }

func (Provider) Features() []string {
	return []string{"vector"}
}

const (
	// defaultDim matches the embedding size of OpenAI's text-embedding-ada-002
	// family, as pgvector does; most self-hosted models are smaller.
	defaultDim = 1536
	// defaultCollection is the collection created when config omits one.
	defaultCollection = "agentflow"
	// defaultDistance is the collection metric when config omits one.
	defaultDistance = "Cosine"
	// defaultTimeout bounds a single call, as the http.Client's Timeout did.
	defaultTimeout = 30 * time.Second
	// vectorName is the named vector every point carries. Named vectors (rather
	// than the single unnamed vector) let a point be stored without an
	// embedding, which is what a Put with no vector means.
	vectorName = "embedding"
	// tableField is the payload key searches filter on.
	tableField = "table"
	// defaultGRPCPort is the port Qdrant serves gRPC on. The REST API's port
	// (6333) is what config URLs name, so the two are translated — see
	// dialTarget.
	defaultGRPCPort = 6334
	// restPort is Qdrant's default REST port.
	restPort = 6333
)

// pointNamespace seeds the deterministic point ids. Any fixed UUID works; it
// only has to be stable across restarts so a key maps to the same point.
var pointNamespace = uuid.MustParse("1b4e28ba-2fa1-11d2-883f-0016d3cca427")

// dialOptions is a test seam, nil in production (the branch is one append). The
// in-package tests run the client against a bufconn listener, which has no
// address to dial, so the connection needs a dialer that a config URL cannot
// express.
var dialOptions []grpc.DialOption

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	rawURL, _ := config["url"].(string)
	if rawURL == "" {
		return nil, fmt.Errorf("qdrant: url is required")
	}
	host, port, useTLS, err := dialTarget(rawURL)
	if err != nil {
		return nil, err
	}
	apiKey, _ := config["api_key"].(string)

	collection, _ := config["collection"].(string)
	if collection == "" {
		collection = defaultCollection
	}

	dim := defaultDim
	switch v := config["dim"].(type) {
	case int:
		dim = v
	case int64:
		dim = int(v)
	case float64:
		dim = int(v)
	}
	if dim <= 0 {
		return nil, fmt.Errorf("qdrant: dim must be positive, got %d", dim)
	}

	distanceName, _ := config["distance"].(string)
	if distanceName == "" {
		distanceName = defaultDistance
	}
	distance, err := parseDistance(distanceName)
	if err != nil {
		return nil, fmt.Errorf("qdrant: %w", err)
	}

	timeout := defaultTimeout
	if s, ok := config["timeout"].(string); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("qdrant: invalid timeout %q (use e.g. \"30s\")", s)
		}
		timeout = d
	}

	// PoolSize 1: one handle is one connection. The client's default pool of 3
	// would round-robin a single-handle backend across three sockets.
	//
	// The compatibility check is off because it cannot work in this build: the
	// client reads its own module version out of the running binary's build
	// info, which carries no dependency list, so every open compares "Unknown"
	// against the server and logs two false-alarm warnings. The collection
	// probe below is the connectivity and shape check that matters here.
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   host,
		Port:                   port,
		APIKey:                 apiKey,
		UseTLS:                 useTLS,
		PoolSize:               1,
		SkipCompatibilityCheck: true,
		GrpcOptions:            dialOptions,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: %w", err)
	}

	h := &Handle{
		client:     client,
		collection: collection,
		dim:        dim,
		timeout:    timeout,
	}
	if err := h.ensureCollection(context.Background(), distance); err != nil {
		return nil, fmt.Errorf("qdrant: %w", err)
	}
	return h, nil
}

// dialTarget turns a config URL into the host, port and TLS flag a gRPC dial
// takes. An explicit 6333 is translated to 6334 because 6333 is the REST port,
// which is what every existing config URL names; any other port is a deliberate
// choice (a proxy, a non-default deployment) and is dialled as given.
func dialTarget(raw string) (host string, port int, useTLS bool, err error) {
	// url.Parse reads a bare "host:6333" as scheme "host" with an opaque rest,
	// so a URL without a scheme is given one first.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, false, fmt.Errorf("qdrant: invalid url %q: %w", raw, err)
	}
	if u.Hostname() == "" {
		return "", 0, false, fmt.Errorf("qdrant: url %q has no host", raw)
	}
	port = defaultGRPCPort
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false, fmt.Errorf("qdrant: url %q has an invalid port %q", raw, p)
		}
		if n == restPort {
			n = defaultGRPCPort
		}
		port = n
	}
	return u.Hostname(), port, strings.EqualFold(u.Scheme, "https"), nil
}

// parseDistance maps the config's distance name onto the enum the gRPC API
// takes. REST accepted the name as a string and left a typo to the server; gRPC
// wants the enum, so an unknown name is caught at boot, where the message can
// still name the alternatives.
func parseDistance(name string) (qdrant.Distance, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cosine":
		return qdrant.Distance_Cosine, nil
	case "euclid", "euclidean":
		return qdrant.Distance_Euclid, nil
	case "dot":
		return qdrant.Distance_Dot, nil
	case "manhattan":
		return qdrant.Distance_Manhattan, nil
	}
	return qdrant.Distance_Cosine, fmt.Errorf("unknown distance %q (want Cosine, Euclid, Dot or Manhattan)", name)
}

// Handle is an opened Qdrant backend.
type Handle struct {
	client     *qdrant.Client
	collection string
	dim        int
	timeout    time.Duration
}

// callContext bounds one call, the way the http.Client's Timeout did before the
// switch to gRPC — the dial itself has no timeout, so every RPC that must not
// hang is wrapped in one.
func (h *Handle) callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), h.timeout)
}

// ensureCollection creates the collection when it is absent, and when it
// exists checks that its vector width matches dim. The width is fixed at
// creation time, so a changed dim would otherwise fail every later write with
// a server-side error far from the cause.
func (h *Handle) ensureCollection(ctx context.Context, distance qdrant.Distance) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	collections := h.client.GetCollectionsClient()
	info, err := collections.Get(ctx, &qdrant.GetCollectionInfoRequest{CollectionName: h.collection})
	switch {
	case err == nil:
		if size, ok := namedVectorSize(info); ok && size != uint64(h.dim) {
			return fmt.Errorf("collection %q holds %d-dimensional vectors but dim is %d (set dim in the backend config to match the embedding model; changing it on an existing collection needs a manual migration)",
				h.collection, size, h.dim)
		}
		return nil
	case status.Code(err) != codes.NotFound:
		return fmt.Errorf("probe collection %q: %w", h.collection, err)
	}

	if _, err := collections.Create(ctx, &qdrant.CreateCollection{
		CollectionName: h.collection,
		VectorsConfig: qdrant.NewVectorsConfigMap(map[string]*qdrant.VectorParams{
			vectorName: {Size: uint64(h.dim), Distance: distance},
		}),
	}); err != nil {
		return fmt.Errorf("create collection %q: %w", h.collection, err)
	}
	return nil
}

// namedVectorSize reads the width of the named vector out of a collection
// probe. It reports false for a collection carrying no such named vector — an
// unnamed-vector collection, or one created by something other than this
// driver — which is not the width mismatch the caller is looking for.
func namedVectorSize(info *qdrant.GetCollectionInfoResponse) (uint64, bool) {
	params := info.GetResult().GetConfig().GetParams()
	v, ok := params.GetVectorsConfig().GetParamsMap().GetMap()[vectorName]
	if !ok {
		return 0, false
	}
	return v.GetSize(), true
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	if len(opts.Vector) > 0 && len(opts.Vector) != h.dim {
		return fmt.Errorf("qdrant: embedding has %d dimensions, backend configured for %d (set dim in the backend config to match the embedding model)", len(opts.Vector), h.dim)
	}
	payload, err := pointPayload(table, key, value)
	if err != nil {
		return err
	}
	ctx, cancel := h.callContext()
	defer cancel()

	id := pointID(table, key)
	// A value stored without an embedding keeps any embedding the point
	// already has: an upsert replaces the whole point, so it would drop the
	// vector a previous Put stored. Setting only the payload leaves the vector
	// alone, matching pgvector's ON CONFLICT update.
	if len(opts.Vector) == 0 {
		return h.setPayload(ctx, id, payload)
	}
	_, err = h.client.GetPointsClient().Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: h.collection,
		Wait:           qdrant.PtrOf(true),
		Points: []*qdrant.PointStruct{{
			Id:      qdrant.NewID(id),
			Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{vectorName: qdrant.NewVectorDense(opts.Vector)}),
			Payload: payload,
		}},
	})
	return err
}

// setPayload updates an existing point's payload without touching its vector,
// and creates the point when it is absent.
//
// The create is not unconditional: an upsert replaces the whole point, so
// following every payload update with one would clear the named vector of a
// point that already had one. Qdrant reports the missing point instead — the
// payload RPC fails with NOT_FOUND, the gRPC twin of the REST endpoint's 404 —
// so the upsert runs only in that case. A point cannot be created without a
// vector field at all (the API rejects the body), so the create carries an empty
// named-vector set, which is the only way to store a value that has no
// embedding.
func (h *Handle) setPayload(ctx context.Context, id string, payload map[string]*qdrant.Value) error {
	points := h.client.GetPointsClient()
	_, err := points.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: h.collection,
		Wait:           qdrant.PtrOf(true),
		Payload:        payload,
		PointsSelector: qdrant.NewPointsSelector(qdrant.NewID(id)),
	})
	if err == nil {
		return nil // updated in place; the stored vector is untouched
	}
	if status.Code(err) != codes.NotFound {
		return fmt.Errorf("update payload: %w", err)
	}
	_, err = points.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: h.collection,
		Wait:           qdrant.PtrOf(true),
		Points: []*qdrant.PointStruct{{
			Id:      qdrant.NewID(id),
			Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{}),
			Payload: payload,
		}},
	})
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := h.callContext()
	defer cancel()

	got, err := h.client.GetPointsClient().Get(ctx, &qdrant.GetPoints{
		CollectionName: h.collection,
		Ids:            []*qdrant.PointId{qdrant.NewID(pointID(table, key))},
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, false, fmt.Errorf("qdrant: get: %w", err)
	}
	// A point the collection does not hold comes back as an empty result, not
	// as an error.
	result := got.GetResult()
	if len(result) == 0 {
		return nil, false, nil
	}
	// The id is a hash of (table, key), so the payload is the authority on what
	// the point holds: a point that answers to this id but describes another
	// logical row reads as absent rather than as a hit, and a later write to
	// the same table/key pair overwrites it.
	payload := result[0].GetPayload()
	if stringField(payload, tableField) != table || stringField(payload, "key") != key {
		return nil, false, nil
	}
	return valueAny(payload["value"]), true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := h.callContext()
	defer cancel()
	_, err := h.client.GetPointsClient().Delete(ctx, &qdrant.DeletePoints{
		CollectionName: h.collection,
		Wait:           qdrant.PtrOf(true),
		Points:         qdrant.NewPointsSelector(qdrant.NewID(pointID(table, key))),
	})
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	if q.Kind != "vector" {
		return nil, fmt.Errorf("qdrant: only vector queries are supported")
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("qdrant: vector query requires a query vector")
	}
	if len(q.Vector) != h.dim {
		return nil, fmt.Errorf("qdrant: query vector has %d dimensions, backend configured for %d", len(q.Vector), h.dim)
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := h.callContext()
	defer cancel()

	// Search is the gRPC counterpart of the REST points/search endpoint this
	// driver used, and the older of the two RPCs Qdrant offers for it (Query is
	// its replacement). It is kept because every Qdrant 1.x serves it and its
	// request maps one-for-one onto what the REST body carried: the named query
	// vector, the limit, the payload, and the table filter.
	req := &qdrant.SearchPoints{
		CollectionName: h.collection,
		Vector:         q.Vector,
		VectorName:     qdrant.PtrOf(vectorName),
		Limit:          uint64(k),
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if table != "" {
		// The table is a payload field, so the filter is a keyword match on it —
		// the same clause the REST body's must/match carried.
		req.Filter = &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword(tableField, table)}}
	}
	got, err := h.client.GetPointsClient().Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("qdrant: search: %w", err)
	}
	hits := got.GetResult()
	recs := make([]memory.Record, 0, len(hits))
	for _, p := range hits {
		recs = append(recs, memory.Record{
			Key:   stringField(p.GetPayload(), "key"),
			Value: valueAny(p.GetPayload()["value"]),
		})
	}
	return &sliceIter{recs: recs}, nil
}

// GC is a no-op: records carry no window of their own, and a vector store is
// not the recency store.
func (h *Handle) GC(table string, window int) error { return nil }

// Close is a no-op, as it was for the REST client. The handle holds one gRPC
// connection, and the engine opens backends once at boot and closes them only
// as the process goes down, so the connection is left to exit with the process
// rather than torn down here.
func (h *Handle) Close() error { return nil }

// pointPayload is the payload every point carries: the logical table and key,
// which are what Get checks and Query filters on, plus the caller's value as
// arbitrary JSON.
func pointPayload(table, key string, value any) (map[string]*qdrant.Value, error) {
	encoded, err := payloadValue(value)
	if err != nil {
		return nil, err
	}
	return map[string]*qdrant.Value{
		tableField: qdrant.NewValueString(table),
		"key":      qdrant.NewValueString(key),
		"value":    encoded,
	}, nil
}

// payloadValue converts a stored Go value into Qdrant's payload Value type.
//
// The conversion runs through JSON, which is how the value travelled over REST
// before: a caller hands in whatever a store wrote (a map, a slice, a struct, a
// json.RawMessage), and the point has to hold it as the nested struct the API
// takes. It also normalizes the value the way the old wire format did, so a
// round trip hands back the same shapes JSON decoding produced.
func payloadValue(value any) (*qdrant.Value, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal value: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, fmt.Errorf("marshal value: %w", err)
	}
	out, err := qdrant.NewValue(decoded)
	if err != nil {
		return nil, fmt.Errorf("marshal value: %w", err)
	}
	return out, nil
}

// valueAny is the inverse of payloadValue: it hands a stored payload value back
// as the plain Go value the stores read. Numbers come back as float64 even when
// the point holds an integer, because that is what decoding the REST reply
// produced and what callers were written against.
func valueAny(v *qdrant.Value) any {
	switch kind := v.GetKind().(type) {
	case *qdrant.Value_NullValue:
		return nil
	case *qdrant.Value_BoolValue:
		return kind.BoolValue
	case *qdrant.Value_StringValue:
		return kind.StringValue
	case *qdrant.Value_DoubleValue:
		return kind.DoubleValue
	case *qdrant.Value_IntegerValue:
		return float64(kind.IntegerValue)
	case *qdrant.Value_StructValue:
		fields := kind.StructValue.GetFields()
		out := make(map[string]any, len(fields))
		for name, field := range fields {
			out[name] = valueAny(field)
		}
		return out
	case *qdrant.Value_ListValue:
		items := kind.ListValue.GetValues()
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = valueAny(item)
		}
		return out
	}
	// Absent: a point whose payload has no value field holds nothing.
	return nil
}

// stringField reads a string out of a payload, treating an absent or
// non-string field as "" so a malformed point reads as a mismatch rather than
// as a hit.
func stringField(payload map[string]*qdrant.Value, name string) string {
	return payload[name].GetStringValue()
}

// pointID is the deterministic Qdrant point id for one (table, key): stable
// across restarts, so Get/Delete/Upsert need no filter round-trip. The table
// is part of the input, so the same key in two tables is two points.
func pointID(table, key string) string {
	return uuid.NewSHA1(pointNamespace, []byte(table+"\x00"+key)).String()
}

type sliceIter struct {
	recs []memory.Record
	i    int
}

func (it *sliceIter) Next() bool {
	if it.i >= len(it.recs) {
		return false
	}
	it.i++
	return true
}

func (it *sliceIter) Record() memory.Record { return it.recs[it.i-1] }
func (it *sliceIter) Err() error            { return nil }
