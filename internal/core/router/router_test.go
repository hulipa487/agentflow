package router

import (
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/session"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Submit must stamp a message id at the ingress choke point (the audit
// item_id) and record the event to the journal with its status.
func TestSubmitStampsIDAndJournals(t *testing.T) {
	var got []string
	r := New("loop-src", "", nil, testLogger(t))
	r.Journal = func(in Inbound, status string) {
		got = append(got, in.Message.ID+"|"+status)
	}
	in := Inbound{
		Channel: "telegram",
		Agent:   "main",
		Message: session.Message{Type: "user", From: "u", Text: "hi"},
	}
	r.Submit(in)

	if len(got) != 1 {
		t.Fatalf("journal calls: %v", got)
	}
	id := got[0][:len(got[0])-len("|routed")]
	if id == "" {
		t.Fatal("message id not stamped")
	}
	if got[0] != id+"|routed" {
		t.Fatalf("status: %q", got[0])
	}
	// The queued event carries the same id and the channel default.
	queued := <-r.mailbox
	if queued.Message.ID != id {
		t.Fatalf("queued id %q != journaled id %q", queued.Message.ID, id)
	}
	if queued.Message.Channel != "telegram" {
		t.Fatalf("channel default: %q", queued.Message.Channel)
	}
}

// A channel-provided id is kept (not overwritten), and a full queue journals
// the drop.
func TestSubmitKeepsIDAndJournalsDrop(t *testing.T) {
	var statuses []string
	r := New("loop-src", "", nil, testLogger(t))
	r.Journal = func(in Inbound, status string) { statuses = append(statuses, status) }
	// Fill the queue.
	for i := 0; i < cap(r.mailbox); i++ {
		r.Submit(Inbound{Channel: "c", Message: session.Message{ID: "x"}})
	}
	r.Submit(Inbound{Channel: "c", Message: session.Message{ID: "overflow"}})
	if statuses[len(statuses)-1] != "dropped_queue" {
		t.Fatalf("last status: %v", statuses[len(statuses)-1])
	}
	first := <-r.mailbox
	if first.Message.ID != "x" {
		t.Fatalf("provided id overwritten: %q", first.Message.ID)
	}
}
