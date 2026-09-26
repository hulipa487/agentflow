package router

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/session"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// routeReporting is a route handler that names itself in the log for every
// inbound it routes, so a test can tell which version of the handler answered.
func routeReporting(marker string) string {
	return `
function loop()
  while true do
    local item = session.inbox()
    log.info("` + marker + `:" .. item.agent)
  end
end
`
}

// TestReloadSwapsRouteHandler: routing is policy, so a route edit is applied to
// the running router — no process restart. The handler runs in the router's
// service state rather than in a session, so the swap is a state rebuild at the
// safe point (parked on inbox, between messages) from the new source; the next
// inbound is routed by the new handler.
func TestReloadSwapsRouteHandler(t *testing.T) {
	ch := make(chan string, 32)
	r := New(routeReporting("V1"), "", nil, slog.New(chanHandler{ch: ch}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	r.Submit(Inbound{Channel: "webhook", Agent: "main", Message: session.Message{ID: "m1", Type: "user"}})
	if line := waitLine(t, ch, "V1:"); line != "V1:main" {
		t.Fatalf("first inbound routed by the wrong handler: %q", line)
	}

	r.Reload(routeReporting("V2"))
	waitLine(t, ch, "router reloaded")

	r.Submit(Inbound{Channel: "webhook", Agent: "main", Message: session.Message{ID: "m2", Type: "user"}})
	if line := waitLine(t, ch, "V2:"); line != "V2:main" {
		t.Fatalf("inbound after the reload routed by the wrong handler: %q", line)
	}
}

// TestReloadBeforeRunIsApplied: the watcher polls on its own goroutine, so a
// reload can land before any state is parked. Nothing is lost and nothing
// blocks: the new source is what the next state is built from, so the first
// inbound is already routed by the new handler.
func TestReloadBeforeRunIsApplied(t *testing.T) {
	ch := make(chan string, 32)
	r := New(routeReporting("V1"), "", nil, slog.New(chanHandler{ch: ch}))

	// Reload before Run: nothing is listening yet.
	r.Reload(routeReporting("V2"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	r.Submit(Inbound{Channel: "webhook", Agent: "main", Message: session.Message{ID: "m1", Type: "user"}})
	if line := waitLine(t, ch, "V2:"); line != "V2:main" {
		t.Fatalf("the reload was not applied: %q", line)
	}
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
