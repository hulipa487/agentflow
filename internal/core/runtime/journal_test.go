package runtime

import (
	"context"
	"testing"
	"time"

	"agentflow/internal/core/media"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/rt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJournalRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	in := JournalEntry{
		ID:        "msg-1",
		Ts:        time.Now().Unix(),
		Direction: "in",
		Status:    "routed",
		Channel:   "telegram",
		Chat:      "42",
		Sender:    "user:abc",
		Agent:     "main",
		Type:      "user",
		Text:      "look at this",
		Attachments: []media.Part{
			{Type: "image", MIME: "image/jpeg", Handle: "media:deadbeef", Name: "photo.jpg"},
		},
		Provenance: map[string]any{"kind": "channel", "principal": "telegram"},
	}
	if err := s.RecordMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	out := JournalEntry{
		ID:        "msg-1", // reply correlates to the inbound id
		Direction: "out",
		Status:    "delivered",
		Channel:   "telegram",
		Chat:      "42",
		Sender:    "42",
		Agent:     "main",
		SessionID: "main|telegram:42",
		Type:      "agent",
		Text:      "a cat",
	}
	if err := s.RecordMessage(ctx, out); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListMessages(ctx, JournalFilter{Channel: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	// Newest first: the egress (same ts, later rowid) leads.
	if rows[0].Direction != "out" || rows[0].SessionID != "main|telegram:42" || rows[0].Status != "delivered" {
		t.Fatalf("out row: %+v", rows[0])
	}
	got := rows[1]
	if got.Direction != "in" || got.Text != "look at this" || got.Chat != "42" {
		t.Fatalf("in row: %+v", got)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Type != "image" || got.Attachments[0].Name != "photo.jpg" {
		t.Fatalf("attachments: %+v", got.Attachments)
	}
	if got.Provenance["kind"] != "channel" {
		t.Fatalf("provenance: %+v", got.Provenance)
	}

	// Filter by id correlation: both directions share msg-1.
	if n, err := s.journalRowCount(ctx); err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
}

func TestJournalPrune(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -100).Unix()
	for _, e := range []JournalEntry{
		{ID: "old", Ts: old, Direction: "in", Status: "routed"},
		{ID: "new", Ts: time.Now().Unix(), Direction: "in", Status: "routed"},
	} {
		if err := s.RecordMessage(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneMessages(ctx, time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned: %d", n)
	}
	rows, _ := s.ListMessages(ctx, JournalFilter{})
	if len(rows) != 1 || rows[0].ID != "new" {
		t.Fatalf("remaining: %+v", rows)
	}
}

func TestJournalFilters(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Unix()
	for _, e := range []JournalEntry{
		{ID: "1", Ts: now, Direction: "in", Status: "routed", Channel: "telegram", SessionID: "s1"},
		{ID: "2", Ts: now, Direction: "in", Status: "routed", Channel: "webhook", SessionID: "s2"},
	} {
		if err := s.RecordMessage(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := s.ListMessages(ctx, JournalFilter{SessionID: "s2"})
	if len(rows) != 1 || rows[0].Channel != "webhook" {
		t.Fatalf("session filter: %+v", rows)
	}
	rows, _ = s.ListMessages(ctx, JournalFilter{Since: now + 100})
	if len(rows) != 0 {
		t.Fatalf("since filter: %+v", rows)
	}
}
