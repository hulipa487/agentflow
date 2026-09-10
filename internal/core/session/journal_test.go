package session

import (
	"testing"
	"time"
)

// The egress journal records every reply with its outcome, correlated to the
// inbound message that prompted it — set by the supervisor, invisible to Lua.
func TestEgressJournalRecordsDelivery(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop()
  local msg = session.inbox()
  session.send("a cat")
end
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
