package identity

import (
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "identity.db"), testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestResolveMintsOnce(t *testing.T) {
	r := newTestRegistry(t)
	u1, err := r.Resolve("telegram", "user:telegram:123", "12345", map[string]any{"username": "oscar"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	u2, err := r.Resolve("telegram", "user:telegram:123", "12345", nil)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if u1 != u2 {
		t.Fatalf("same native_from must resolve to same uuid: %q vs %q", u1, u2)
	}
	if u1 == "" {
		t.Fatal("uuid empty")
	}
}

func TestResolveDistinctNativesDistinctUUIDs(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Resolve("telegram", "user:telegram:1", "1", nil)
	b, _ := r.Resolve("telegram", "user:telegram:2", "2", nil)
	if a == b {
		t.Fatal("different native_from must yield different uuids")
	}
}

func TestResolveConcurrentFirstContactMintsOne(t *testing.T) {
	r := newTestRegistry(t)
	const n = 50
	var wg sync.WaitGroup
	uuids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := r.Resolve("telegram", "user:telegram:999", "999", nil)
			if err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			uuids[i] = u
		}(i)
	}
	wg.Wait()
	first := uuids[0]
	if first == "" {
		t.Fatal("uuid empty")
	}
	for i, u := range uuids {
		if u != first {
			t.Fatalf("goroutine %d got %q, want %q — concurrent mint produced duplicate", i, u, first)
		}
	}
}

func TestRefreshUpdatesDeliveryTarget(t *testing.T) {
	r := newTestRegistry(t)
	uuid, _ := r.Resolve("telegram", "user:telegram:5", "chat-1", nil)
	ch, rt, ok := r.LookupUser(uuid)
	if !ok || ch != "telegram" || rt != "chat-1" {
		t.Fatalf("lookup after first resolve: ch=%q rt=%q ok=%v", ch, rt, ok)
	}
	// Same user, new reply target — refresh should update it.
	_, _ = r.Resolve("telegram", "user:telegram:5", "chat-2", nil)
	ch, rt, _ = r.LookupUser(uuid)
	if rt != "chat-2" {
		t.Fatalf("reply_to not refreshed: got %q", rt)
	}
}

func TestLookupUserUnknown(t *testing.T) {
	r := newTestRegistry(t)
	if _, _, ok := r.LookupUser("u_doesnotexist"); ok {
		t.Fatal("unknown uuid should not resolve")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.db")
	r1, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	uuid, err := r1.Resolve("webhook", "user:webhook:alice", "req-1", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_ = r1.Close()

	r2, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer r2.Close()
	ch, rt, ok := r2.LookupUser(uuid)
	if !ok || ch != "webhook" || rt != "req-1" {
		t.Fatalf("after reopen: ch=%q rt=%q ok=%v", ch, rt, ok)
	}
	// Same native_from resolves to the same uuid across restart.
	uuid2, err := r2.Resolve("webhook", "user:webhook:alice", "req-2", nil)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if uuid != uuid2 {
		t.Fatalf("uuid changed across reopen: %q vs %q", uuid, uuid2)
	}
}

// --- Sink ---

type captureSink struct {
	got  []router.Inbound
	mu   sync.Mutex
	done chan struct{}
}

func (c *captureSink) Submit(in router.Inbound) {
	c.mu.Lock()
	c.got = append(c.got, in)
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
}

func TestSinkRewritesEnvelope(t *testing.T) {
	r := newTestRegistry(t)
	cap := &captureSink{done: make(chan struct{}, 4)}
	sink := NewSink(cap, r, testLogger())

	in := router.Inbound{
		Channel: "telegram",
		Agent:   "anyi",
		Message: session.Message{
			ID:      "m1",
			Type:    "user",
			From:    "user:telegram:123",
			Text:    "hi",
			Channel: "telegram",
			ReplyTo: "12345",
		},
	}
	sink.Submit(in)

	if len(cap.got) != 1 {
		t.Fatalf("inner got %d events, want 1", len(cap.got))
	}
	out := cap.got[0]
	if out.Agent != "anyi" {
		t.Fatalf("agent: %q", out.Agent)
	}
	if out.Message.From == "user:telegram:123" {
		t.Fatal("From not rewritten — still native")
	}
	uuid := out.Message.Payload["user_uuid"].(string)
	if out.Message.From != "user:"+uuid {
		t.Fatalf("From=%q want user:%s", out.Message.From, uuid)
	}
	if out.Message.To != "agent:anyi" {
		t.Fatalf("To=%q want agent:anyi", out.Message.To)
	}
	if out.Message.Payload["native_from"] != "user:telegram:123" {
		t.Fatalf("native_from not stashed: %v", out.Message.Payload["native_from"])
	}
	// Reply path preserved.
	if out.Message.Channel != "telegram" || out.Message.ReplyTo != "12345" {
		t.Fatalf("reply path clobbered: channel=%q reply_to=%q", out.Message.Channel, out.Message.ReplyTo)
	}
}

func TestSinkFailOpenOnRegistryError(t *testing.T) {
	// A closed registry: Resolve will error on the DB hit.
	r := newTestRegistry(t)
	_ = r.Close()
	cap := &captureSink{done: make(chan struct{}, 4)}
	sink := NewSink(cap, r, testLogger())

	sink.Submit(router.Inbound{
		Channel: "telegram",
		Agent:   "anyi",
		Message: session.Message{From: "user:telegram:123", Type: "user", Channel: "telegram"},
	})
	if len(cap.got) != 1 {
		t.Fatalf("fail-open should still forward, got %d", len(cap.got))
	}
	if cap.got[0].Message.From != "user:telegram:123" {
		t.Fatalf("fail-open changed From to %q", cap.got[0].Message.From)
	}
}
