package redisvector

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"agentflow/internal/core/memory"
)

// TestFeaturesIncludeVectorAndKV: this provider stores the value beside the
// vector, so it advertises both, and the Lua path feature-detects on "vector".
func TestFeaturesIncludeVectorAndKV(t *testing.T) {
	got := Provider{}.Features()
	want := map[string]bool{"kv": true, "prefix_scan": true, "ttl": true, "vector": true}
	if len(got) != len(want) {
		t.Fatalf("Features() = %v; want %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Fatalf("unexpected feature %q in %v", f, got)
		}
	}
}

// TestOpenRequiresURLAndGoodDim: config errors are boot errors.
func TestOpenRequiresURLAndGoodDim(t *testing.T) {
	if _, err := (Provider{}).Open(map[string]any{}); err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("want a url error, got %v", err)
	}
	// A bad dim is rejected before any connection is attempted.
	if _, err := (Provider{}).Open(map[string]any{"url": "redis://127.0.0.1:1", "dim": 0}); err == nil ||
		!strings.Contains(err.Error(), "dim must be positive") {
		t.Fatalf("want a dim error, got %v", err)
	}
}

// TestIndexArgsShape pins the FT.CREATE command: a HASH index over the table's
// key prefix, with the value field and a FLOAT32 HNSW COSINE vector field.
func TestIndexArgsShape(t *testing.T) {
	got := indexArgs("idx:writer.facts", "writer.facts:", 768)
	want := []any{
		"FT.CREATE", "idx:writer.facts",
		"ON", "HASH",
		"PREFIX", 1, "writer.facts:",
		"SCHEMA",
		"value", "TEXT",
		"embedding", "VECTOR", "HNSW", 6,
		"TYPE", "FLOAT32",
		"DIM", 768,
		"DISTANCE_METRIC", "COSINE",
	}
	if len(got) != len(want) {
		t.Fatalf("indexArgs length = %d; want %d\n got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("indexArgs[%d] = %v; want %v\n got %v", i, got[i], want[i], got)
		}
	}
}

// TestSearchArgsShape pins the k-NN query: a KNN clause over the named vector
// parameter, DIALECT 2, and NOCONTENT so the reply stays a flat key list.
func TestSearchArgsShape(t *testing.T) {
	vec := []float32{1, 2}
	got := searchArgs("idx:t", 5, vec)
	if got[0] != "FT.SEARCH" || got[1] != "idx:t" {
		t.Fatalf("search must target the table index: %v", got[:2])
	}
	if q, _ := got[2].(string); q != "(*)=>[KNN 5 @embedding $vec AS score]" {
		t.Fatalf("query string = %q", got[2])
	}
	if got[3] != "PARAMS" || got[4] != 2 || got[5] != "vec" {
		t.Fatalf("PARAMS clause wrong: %v", got[3:6])
	}
	blob, ok := got[6].([]byte)
	if !ok || len(blob) != 8 {
		t.Fatalf("vector parameter must be a float32 blob: %#v", got[6])
	}
	tail := got[7:]
	for i, want := range []any{"SORTBY", "score", "DIALECT", 2, "NOCONTENT"} {
		if tail[i] != want {
			t.Fatalf("tail[%d] = %v; want %v", i, tail[i], want)
		}
	}
}

// TestEncodeVector: the blob is little-endian float32, which is what a
// RediSearch FLOAT32 vector field expects.
func TestEncodeVector(t *testing.T) {
	got := encodeVector([]float32{1.5, -2.25})
	if len(got) != 8 {
		t.Fatalf("blob length = %d; want 8", len(got))
	}
	if v := math.Float32frombits(binary.LittleEndian.Uint32(got[0:4])); v != 1.5 {
		t.Fatalf("first float = %v; want 1.5", v)
	}
	if v := math.Float32frombits(binary.LittleEndian.Uint32(got[4:8])); v != -2.25 {
		t.Fatalf("second float = %v; want -2.25", v)
	}
	if len(encodeVector(nil)) != 0 {
		t.Fatal("an empty vector must encode to an empty blob")
	}
}

// TestSearchKeys: an FT.SEARCH ... NOCONTENT reply is [total, key, ...].
func TestSearchKeys(t *testing.T) {
	got, err := searchKeys([]any{int64(2), "t:a", "t:b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "t:a" || got[1] != "t:b" {
		t.Fatalf("searchKeys = %v; want [t:a t:b]", got)
	}

	// Zero hits still carries the total.
	got, err = searchKeys([]any{int64(0)})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty reply: %v err=%v", got, err)
	}

	// Anything unparseable is an error, not a silently empty result.
	if _, err := searchKeys("nope"); err == nil {
		t.Fatal("a non-array reply must error")
	} else if !strings.Contains(err.Error(), "RESP2") {
		t.Fatalf("the parse error should name the protocol, got %v", err)
	}
	if _, err := searchKeys([]any{int64(1), 42}); err == nil {
		t.Fatal("a non-key element must error")
	}
}

// TestKeysAndIndexNaming: "<table>:<key>" is builtin:redis's convention, the
// index is per table, and a record key round-trips through stripTable.
func TestKeysAndIndexNaming(t *testing.T) {
	h := &Handle{}
	if got := h.fullKey("writer.facts", "k1"); got != "writer.facts:k1" {
		t.Fatalf("fullKey = %q", got)
	}
	if got := h.fullKey("", "k1"); got != "k1" {
		t.Fatalf("an empty table must leave the key bare, got %q", got)
	}
	if got := h.stripTable("writer.facts:k1", "writer.facts"); got != "k1" {
		t.Fatalf("stripTable = %q", got)
	}
	if got := indexFor("writer.facts"); got != "idx:writer.facts" {
		t.Fatalf("indexFor = %q", got)
	}
}

// TestQueryRejectsUnsupportedAndMisconfigured: the error paths that must fire
// before any server contact.
func TestQueryRejectsUnsupportedAndMisconfigured(t *testing.T) {
	h := &Handle{dim: 4}

	if _, err := h.Query("t", memory.Query{Kind: "text"}); err == nil ||
		!strings.Contains(err.Error(), "unsupported query kind") {
		t.Fatalf("want an unsupported-kind error, got %v", err)
	}
	if _, err := h.Query("t", memory.Query{Kind: "vector"}); err == nil {
		t.Fatal("a vector query with no vector must fail")
	}
	if _, err := h.Query("t", memory.Query{Kind: "vector", Vector: []float32{1, 2}}); err == nil ||
		!strings.Contains(err.Error(), "configured for 4") {
		t.Fatalf("want a dimension error, got %v", err)
	}
	// A table is required: the index is per table, so an unscoped vector query
	// cannot be answered.
	if _, err := h.Query("", memory.Query{Kind: "vector", Vector: []float32{1, 2, 3, 4}}); err == nil ||
		!strings.Contains(err.Error(), "need a table") {
		t.Fatalf("want a table error, got %v", err)
	}
	if err := h.ensureIndex(""); err == nil {
		t.Fatal("ensureIndex with no table must fail")
	}
}

// TestDimMismatchOnPut: a vector of the wrong width is refused with both
// numbers named, as builtin:pgvector does.
func TestDimMismatchOnPut(t *testing.T) {
	h := &Handle{dim: 3}
	err := h.Put("t", "k", "v", memory.PutOpts{Vector: []float32{1, 2}})
	if err == nil || !strings.Contains(err.Error(), "2 dimensions") || !strings.Contains(err.Error(), "configured for 3") {
		t.Fatalf("want a dimension error, got %v", err)
	}
}

// TestIsIndexExists: a pre-existing index is not an error, anything else is.
func TestIsIndexExists(t *testing.T) {
	if isIndexExists(nil) {
		t.Fatal("nil is not an existing index")
	}
	if !isIndexExists(errString("ERR Index already exists")) {
		t.Fatal("the RediSearch duplicate-index reply must be tolerated")
	}
	if isIndexExists(errString("ERR unknown command 'FT.CREATE'")) {
		t.Fatal("a missing module must not be mistaken for an existing index")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestParseOptionsPinsRESP2: go-redis v9 negotiates RESP3 by default, and
// RediSearch replies to FT.SEARCH as a map of named fields under RESP3 rather
// than the flat [total, key, ...] array searchKeys reads. Without this pin
// every vector query fails to parse — which is exactly what happened against a
// live Redis Stack, before the KNN query string was ever reached.
func TestParseOptionsPinsRESP2(t *testing.T) {
	opts, err := parseOptions("redis://localhost:6379")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Protocol != 2 {
		t.Fatalf("Protocol = %d; want 2 (RESP2). RESP3 makes FT.SEARCH reply as a map and breaks searchKeys", opts.Protocol)
	}
	// An explicit protocol in the URL must not win: the reply parsing below
	// only understands the RESP2 shape.
	if opts, err := parseOptions("redis://localhost:6379?protocol=3"); err != nil || opts.Protocol != 2 {
		t.Fatalf("a URL asking for RESP3 must still be pinned: protocol=%d err=%v", opts.Protocol, err)
	}
	if _, err := parseOptions("://not-a-url"); err == nil {
		t.Fatal("a malformed url must error")
	}
}
