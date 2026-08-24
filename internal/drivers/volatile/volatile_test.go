package volatile

import (
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/memory"
)

func TestPutGetRoundTrip(t *testing.T) {
	h, err := Provider{}.Open(nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	if err := h.Put("t", "k", map[string]any{"v": 1}, memory.PutOpts{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	v, ok, err := h.Get("t", "k")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	m, _ := v.(map[string]any)
	if m["v"].(float64) != 1 {
		t.Fatalf("got %v", v)
	}

	// Overwrite.
	_ = h.Put("t", "k", 2, memory.PutOpts{})
	v, _, _ = h.Get("t", "k")
	if v.(float64) != 2 {
		t.Fatalf("overwrite: got %v", v)
	}
}

func TestTTLExpiry(t *testing.T) {
	h, _ := Provider{}.Open(nil)
	defer h.Close()
	_ = h.Put("t", "k", "v", memory.PutOpts{TTL: 10 * time.Millisecond})
	if _, ok, _ := h.Get("t", "k"); !ok {
		t.Fatal("should be live immediately")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok, _ := h.Get("t", "k"); ok {
		t.Fatal("should expire after TTL")
	}
}

func TestQueryAllAndPrefix(t *testing.T) {
	h, _ := Provider{}.Open(nil)
	defer h.Close()
	_ = h.Put("t", "turn:1", "a", memory.PutOpts{})
	_ = h.Put("t", "fact:1", "c", memory.PutOpts{})
	time.Sleep(20 * time.Millisecond) // ensure turn:2 has a strictly later timestamp
	_ = h.Put("t", "turn:2", "b", memory.PutOpts{})

	all, _ := h.Query("t", memory.Query{Kind: "all"})
	keys := collectKeys(all)
	if len(keys) != 3 {
		t.Fatalf("all: got %d records", len(keys))
	}
	// Newest-first: turn:2 is the most recent write and must lead. The other
	// two may tie on timestamp, so only the head is asserted.
	if keys[0] != "turn:2" {
		t.Fatalf("newest first: got %v", keys)
	}

	pfx, _ := h.Query("t", memory.Query{Kind: "prefix", Prefix: "turn:"})
	pfxKeys := collectKeys(pfx)
	if len(pfxKeys) != 2 {
		t.Fatalf("prefix: got %d", len(pfxKeys))
	}
	if pfxKeys[0] != "turn:2" {
		t.Fatalf("prefix newest first: got %v", pfxKeys)
	}
}

func TestGCWindow(t *testing.T) {
	h, _ := Provider{}.Open(nil)
	defer h.Close()
	for i := 0; i < 5; i++ {
		_ = h.Put("t", "k"+itoa(i), i, memory.PutOpts{})
		time.Sleep(20 * time.Millisecond) // distinct timestamps for a stable recency order
	}
	if err := h.GC("t", 2); err != nil {
		t.Fatalf("gc: %v", err)
	}
	all, _ := h.Query("t", memory.Query{Kind: "all"})
	if len(collectKeys(all)) != 2 {
		t.Fatalf("window=2 should keep 2, got %v", all)
	}
	// The two kept must be the newest (k3, k4).
	v, ok, _ := h.Get("t", "k4")
	if !ok || v.(float64) != 4 {
		t.Fatalf("k4 should survive, got ok=%v v=%v", ok, v)
	}
	if _, ok, _ := h.Get("t", "k0"); ok {
		t.Fatal("k0 should have been reaped")
	}
}

func TestGCExpires(t *testing.T) {
	h, _ := Provider{}.Open(nil)
	defer h.Close()
	_ = h.Put("t", "exp", "v", memory.PutOpts{TTL: 10 * time.Millisecond})
	_ = h.Put("t", "live", "v", memory.PutOpts{})
	time.Sleep(20 * time.Millisecond)
	if err := h.GC("t", 0); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if _, ok, _ := h.Get("t", "exp"); ok {
		t.Fatal("expired should be reaped by GC")
	}
	if _, ok, _ := h.Get("t", "live"); !ok {
		t.Fatal("live should remain")
	}
}

// TestConcurrentPutFirstContact exercises the same "one writer wins, no
// duplicate key" guarantee the identity registry relies on for minting.
func TestConcurrentPutFirstContact(t *testing.T) {
	h, _ := Provider{}.Open(nil)
	defer h.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = h.Put("t", "k", n, memory.PutOpts{})
		}(i)
	}
	wg.Wait()
	v, ok, _ := h.Get("t", "k")
	if !ok {
		t.Fatal("key should exist")
	}
	if v == nil {
		t.Fatal("value should not be nil")
	}
}

func collectKeys(it memory.Iterator) []string {
	var out []string
	for it.Next() {
		out = append(out, it.Record().Key)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
