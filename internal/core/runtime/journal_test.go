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

func TestJournalFilterByChannelAgentChatStatus(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Unix()

	rows := []JournalEntry{
		{ID: "j1", Ts: now - 30, Direction: "in", Status: "routed", Channel: "telegram", Chat: "-1001", Agent: "support", Text: "hi"},
		{ID: "j2", Ts: now - 20, Direction: "out", Status: "ignored", Channel: "telegram", Chat: "-1001", Agent: "support", Text: "cooldown", Err: "cooldown"},
		{ID: "j3", Ts: now - 10, Direction: "in", Status: "dropped_allowlist", Channel: "telegram", Chat: "-1002", Agent: "sales", Text: "blocked", Err: "allow_users"},
		{ID: "j4", Ts: now - 5, Direction: "in", Status: "routed", Channel: "webhook", Chat: "w1", Agent: "support", Text: "hook"},
	}
	for _, r := range rows {
		if err := s.RecordMessage(ctx, r); err != nil {
			t.Fatalf("record %s: %v", r.ID, err)
		}
	}

	byChan, err := s.ListMessages(ctx, JournalFilter{Channel: "telegram"})
	if err != nil || len(byChan) != 3 {
		t.Fatalf("by Channel = %+v, err=%v", byChan, err)
	}
	byAgent, err := s.ListMessages(ctx, JournalFilter{Agent: "sales"})
	if err != nil || len(byAgent) != 1 || byAgent[0].ID != "j3" {
		t.Fatalf("by Agent = %+v, err=%v", byAgent, err)
	}
	byChat, err := s.ListMessages(ctx, JournalFilter{Chat: "-1001"})
	if err != nil || len(byChat) != 2 {
		t.Fatalf("by Chat = %+v, err=%v", byChat, err)
	}
	byStatus, err := s.ListMessages(ctx, JournalFilter{Status: "ignored"})
	if err != nil || len(byStatus) != 1 || byStatus[0].ID != "j2" {
		t.Fatalf("by Status = %+v, err=%v", byStatus, err)
	}
}
