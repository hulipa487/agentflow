// Package qdrant implements the builtin:qdrant vector backend provider. It
// provides the "vector" feature over Qdrant's REST API.
//
// One collection holds every logical table (config "collection", default
// "agentflow"), matching how builtin:pgvector puts every table in one vec_kv
// table. The physical table a store resolves to is agent + "." + store table
// (see memory.ResolveStoresFor), and a "." is not legal in a Qdrant collection
// name, so tables ride in the point payload and searches filter on them rather
// than mapping each table to its own collection.
//
// Only vector queries are supported, like builtin:pgvector: any other query
// kind is an explicit error, never a silent empty iterator.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/core/memory"

	"github.com/google/uuid"
)

// Provider implements memory.BackendProvider for "builtin:qdrant".
type Provider struct{}

func (Provider) Name() string { return "builtin:qdrant" }

func (Provider) Features() []string {
	return []string{"vector"}
}

const (
	// defaultDim matches the embedding size of OpenAI's text-embedding-ada-002
	// family, as builtin:pgvector does; most self-hosted models are smaller.
	defaultDim = 1536
	// defaultCollection is the collection created when config omits one.
	defaultCollection = "agentflow"
	defaultTimeout    = 30 * time.Second
	// vectorName is the named vector every point carries. Named vectors (rather
	// than the single unnamed vector) let a point be stored without an
	// embedding, which is what a Put with no vector means.
	vectorName = "embedding"
	// tableField is the payload key searches filter on.
	tableField = "table"
)

// pointNamespace seeds the deterministic point ids. Any fixed UUID works; it
// only has to be stable across restarts so a key maps to the same point.
var pointNamespace = uuid.MustParse("1b4e28ba-2fa1-11d2-883f-0016d3cca427")

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("builtin:qdrant: url is required")
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
		return nil, fmt.Errorf("builtin:qdrant: dim must be positive, got %d", dim)
	}

	distance, _ := config["distance"].(string)
	if distance == "" {
		distance = "Cosine"
	}

	timeout := defaultTimeout
	if s, ok := config["timeout"].(string); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("builtin:qdrant: invalid timeout %q (use e.g. \"30s\")", s)
		}
		timeout = d
	}

	h := &Handle{
		baseURL:    strings.TrimSuffix(url, "/"),
		apiKey:     apiKey,
		collection: collection,
		dim:        dim,
		http:       &http.Client{Timeout: timeout},
	}
	if err := h.ensureCollection(context.Background(), distance); err != nil {
		return nil, fmt.Errorf("builtin:qdrant: %w", err)
	}
	return h, nil
}

// Handle is an opened Qdrant backend.
type Handle struct {
	baseURL    string
	apiKey     string
	collection string
	dim        int
	http       *http.Client
}

// ensureCollection creates the collection when it is absent, and when it
// exists checks that its vector width matches dim. The width is fixed at
// creation time, so a changed dim would otherwise fail every later write with
// a server-side error far from the cause.
func (h *Handle) ensureCollection(ctx context.Context, distance string) error {
	ctx, cancel := context.WithTimeout(ctx, h.http.Timeout)
	defer cancel()

	var got struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors map[string]struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	status, err := h.do(ctx, http.MethodGet, "/collections/"+h.collection, nil, &got)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		if v, ok := got.Result.Config.Params.Vectors[vectorName]; ok && v.Size != h.dim {
			return fmt.Errorf("collection %q holds %d-dimensional vectors but dim is %d (set dim in the backend config to match the embedding model; changing it on an existing collection needs a manual migration)",
				h.collection, v.Size, h.dim)
		}
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("probe collection %q: unexpected status %d", h.collection, status)
	}

	body := map[string]any{
		"vectors": map[string]any{
			vectorName: map[string]any{"size": h.dim, "distance": distance},
		},
	}
	if _, err := h.do(ctx, http.MethodPut, "/collections/"+h.collection, body, nil); err != nil {
		return fmt.Errorf("create collection %q: %w", h.collection, err)
	}
	return nil
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	if len(opts.Vector) > 0 && len(opts.Vector) != h.dim {
		return fmt.Errorf("builtin:qdrant: embedding has %d dimensions, backend configured for %d (set dim in the backend config to match the embedding model)", len(opts.Vector), h.dim)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.http.Timeout)
	defer cancel()

	point := map[string]any{"id": pointID(table, key)}
	// A value stored without an embedding keeps any embedding the point
	// already has: an upsert replaces the whole point, so it would drop the
	// vector a previous Put stored. Setting only the payload leaves the vector
	// alone, matching builtin:pgvector's ON CONFLICT update.
	payload := map[string]any{tableField: table, "key": key, "value": json.RawMessage(b)}
	if len(opts.Vector) == 0 {
		if err := h.setPayload(ctx, point["id"].(string), payload); err != nil {
			return err
		}
		return nil
	}
	point["vector"] = map[string]any{vectorName: opts.Vector}
	point["payload"] = payload
	body := map[string]any{"points": []any{point}}
	_, err = h.do(ctx, http.MethodPut, "/collections/"+h.collection+"/points?wait=true", body, nil)
	return err
}

// setPayload updates an existing point's payload without touching its vector,
// and creates the point when it is absent. The create carries no vector: the
// collection uses a named vector, so a point may be stored without one, which
// is what a value with no embedding is.
func (h *Handle) setPayload(ctx context.Context, id string, payload map[string]any) error {
	status, err := h.do(ctx, http.MethodPost, "/collections/"+h.collection+"/points/payload?wait=true",
		map[string]any{"payload": payload, "points": []string{id}}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("update payload: unexpected status %d", status)
	}
	// Whether the point existed is not reported, so ensure it does: the
	// upsert is a no-op for an existing point with no vector, and creates one
	// otherwise.
	body := map[string]any{"points": []any{map[string]any{
		"id":      id,
		"vector":  map[string]any{},
		"payload": payload,
	}}}
	_, err = h.do(ctx, http.MethodPut, "/collections/"+h.collection+"/points?wait=true", body, nil)
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), h.http.Timeout)
	defer cancel()

	var got struct {
		Result struct {
			Payload struct {
				Table string          `json:"table"`
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			} `json:"payload"`
		} `json:"result"`
	}
	status, err := h.do(ctx, http.MethodGet, "/collections/"+h.collection+"/points/"+pointID(table, key), nil, &got)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("builtin:qdrant: get: unexpected status %d", status)
	}
	if got.Result.Payload.Table != table || got.Result.Payload.Key != key {
		return nil, false, nil
	}
	var v any
	if len(got.Result.Payload.Value) > 0 {
		if err := json.Unmarshal(got.Result.Payload.Value, &v); err != nil {
			return nil, true, err
		}
	}
	return v, true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.http.Timeout)
	defer cancel()
	body := map[string]any{"points": []string{pointID(table, key)}}
	_, err := h.do(ctx, http.MethodPost, "/collections/"+h.collection+"/points/delete?wait=true", body, nil)
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	if q.Kind != "vector" {
		return nil, fmt.Errorf("builtin:qdrant: only vector queries are supported")
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("builtin:qdrant: vector query requires a query vector")
	}
	if len(q.Vector) != h.dim {
		return nil, fmt.Errorf("builtin:qdrant: query vector has %d dimensions, backend configured for %d", len(q.Vector), h.dim)
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.http.Timeout)
	defer cancel()

	body := map[string]any{
		"vector":       map[string]any{"name": vectorName, "vector": q.Vector},
		"limit":        k,
		"with_payload": true,
	}
	if table != "" {
		body["filter"] = map[string]any{
			"must": []any{map[string]any{"key": tableField, "match": map[string]any{"value": table}}},
		}
	}
	var got struct {
		Result []struct {
			Payload struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			} `json:"payload"`
		} `json:"result"`
	}
	if _, err := h.do(ctx, http.MethodPost, "/collections/"+h.collection+"/points/search", body, &got); err != nil {
		return nil, err
	}
	recs := make([]memory.Record, 0, len(got.Result))
	for _, p := range got.Result {
		var v any
		if len(p.Payload.Value) > 0 {
			if err := json.Unmarshal(p.Payload.Value, &v); err != nil {
				return nil, err
			}
		}
		recs = append(recs, memory.Record{Key: p.Payload.Key, Value: v})
	}
	return &sliceIter{recs: recs}, nil
}

// GC is a no-op: records carry no window of their own, and a vector store is
// not the recency store.
func (h *Handle) GC(table string, window int) error { return nil }

func (h *Handle) Close() error { return nil }

// do issues one REST call and decodes the JSON reply into out (when non-nil).
// It returns the HTTP status so callers can treat 404 as "absent" rather than
// an error. A non-2xx status other than 404 is an error carrying the server's
// own message, which is the fastest way to diagnose a shape mismatch here.
func (h *Handle) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.apiKey != "" {
		req.Header.Set("api-key", h.apiKey)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, serverError(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode reply: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// serverError pulls Qdrant's {"status":{"error":"..."}} message out of a
// failure body, falling back to the raw body.
func serverError(raw []byte) string {
	var e struct {
		Status struct {
			Error string `json:"error"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Status.Error != "" {
		return e.Status.Error
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
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
