package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
)

// The egress journal records every reply with its outcome, correlated to the
// inbound message that prompted it — set by the supervisor, invisible to Lua.
func TestEgressJournalRecordsDelivery(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop() {
  session.inbox();
  session.send("a cat");
}
`)
	var got []EgressRecord
	a.Journal = func(r EgressRecord) { got = append(got, r) }

	a.Mailbox <- Message{ID: "msg-1", Type: "user", From: "u", Text: "what is this?", Channel: "telegram", ReplyTo: "42"}
	waitFor(t, "reply", 5*time.Second, func() bool { return len(gw.snapshot()) == 1 })
	waitFor(t, "journal", 5*time.Second, func() bool { return len(got) == 1 })

	r := got[0]
	if r.Status != "delivered" || r.Text != "a cat" {
		t.Fatalf("record: %+v", r)
	}
	if r.InReplyTo != "msg-1" {
		t.Fatalf("reply not correlated to inbound: %+v", r)
	}
	if r.Channel != "telegram" || r.ReplyTo != "42" {
		t.Fatalf("route: %+v", r)
	}
	if r.SessionID == "" || r.Agent == "" {
		t.Fatalf("identity missing: %+v", r)
	}
}

// TestJournalMetadataNeverStoresText writes a real journal row through the same
// mapping main uses: metadata keeps the ids and sizes and no text at all — not
// even truncated. The row is then read back from the store, because the point
// is what is persisted, not what the callback was handed.
func TestJournalMetadataNeverStoresText(t *testing.T) {
	store, err := runtime.OpenSQLite(t.TempDir() + "/journal.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop() {
  session.inbox();
  session.send("SUPER-SECRET-BODY");
}
`)
	a.JournalModeOf = func(string) JournalMode { return JournalMetadata }
	a.Journal = func(r EgressRecord) {
		_ = store.RecordMessage(context.Background(), runtime.JournalEntry{
			ID: r.InReplyTo, Direction: "out", Status: r.Status, Channel: r.Channel,
			Chat: r.ReplyTo, Sender: r.ReplyTo, UserUUID: r.UserUUID, Agent: r.Agent,
			SessionID: r.SessionID, Type: "agent",
			Text: r.Text, Attachments: r.Attachments,
			TextBytes: r.TextBytes, AttachmentBytes: r.AttachmentBytes,
		})
	}
	a.Mailbox <- Message{ID: "msg-1", Type: "user", From: "u", Text: "go", Channel: "api", ReplyTo: "r1"}
	waitFor(t, "reply", 5*time.Second, func() bool { return len(gw.snapshot()) == 1 })
	waitFor(t, "journal", 5*time.Second, func() bool {
		rows, err := store.ListMessages(context.Background(), runtime.JournalFilter{Limit: 10})
		return err == nil && len(rows) == 1
	})
	rows, err := store.ListMessages(context.Background(), runtime.JournalFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	if row.Text != "" {
		t.Fatalf("metadata journal kept text %q", row.Text)
	}
	if len(row.Attachments) != 0 {
		t.Fatalf("metadata journal kept %d attachments", len(row.Attachments))
	}
	if row.TextBytes != len("SUPER-SECRET-BODY") {
		t.Fatalf("metadata row text_bytes = %d; want %d", row.TextBytes, len("SUPER-SECRET-BODY"))
	}
	// The body must not survive anywhere in the row.
	all, _ := store.ListMessages(context.Background(), runtime.JournalFilter{Limit: 10})
	for _, r := range all {
		if strings.Contains(r.Text, "SUPER-SECRET-BODY") || strings.Contains(r.Err, "SUPER-SECRET-BODY") {
			t.Fatalf("metadata row leaked the body: %+v", r)
		}
	}
}

// TestJournalOffWritesNothing: off means the actor never hands the record to
// the sink.
func TestJournalOffWritesNothing(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop() {
  session.inbox();
  session.send("hello");
}
`)
	calls := 0
	a.JournalModeOf = func(string) JournalMode { return JournalOff }
	a.Journal = func(EgressRecord) { calls++ }
	a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "api", ReplyTo: "r1"}
	waitFor(t, "reply", 5*time.Second, func() bool { return len(gw.snapshot()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("journal off wrote %d records", calls)
	}
}
