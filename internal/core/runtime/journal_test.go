package runtime

import (
	"context"
	"testing"
	"time"

	"agentflow/internal/core/media"
)

func openTemp(t *testing.T) Store {
	t.Helper()
	s, err := OpenSQLite(t.TempDir() + "/rt.db")
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

	// Count via the ops helper: both directions share msg-1.
	if n, err := s.JournalRowCount(ctx); err != nil || n != 2 {
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
	if n, err := s.JournalRowCount(ctx); err != nil || n != 1 {
		t.Fatalf("remaining: %d %v", n, err)
	}
}
