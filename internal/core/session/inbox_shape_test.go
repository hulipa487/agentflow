package session

import (
	"testing"
	"time"

	"agentflow/internal/core/media"
)

// This file pins the shape of what a loop is handed, because vm.PreludeVersion
// covers that shape and not only the list of ops a chunk may call.
//
// The gate answers "which ops exist". It says nothing on its own about the
// shape of the values those ops hand back — and the inbox shape moved under
// loops once already: the tenant used to ride msg.payload.user_uuid, and now it
// is msg.provenance.user_uuid, a three-state field (absent, "", or a uuid). A
// loop that still reads the old key declares the version it was written for and
// passes the gate cleanly, so the gate cannot see the break. That is the class
// of change most likely to take out proprietary Lua silently, which is why the
// shape is pinned rather than described.
//
// The seam pinned is the one a loop actually meets: dispatchInline's "inbox"
// case in actor.go marshals the Message with jsonString and resumes the
// coroutine with those bytes, which the prelude decodes into the table
// session.inbox() returns. The two tests below are the two ends of that seam —
// the exact bytes (TestInboxWireShape) and what a running loop decodes them
// into (TestInboxShapeReachesLua). Both are change detectors, and that is the
// point: a failure here is a version decision, not a bug. If the change is
// intended, bump vm.PreludeVersion and update these expectations in the same
// commit, so a chunk that declares the old number is refused at load instead of
// misreading the new shape at run time.
//
// What this does NOT pin: the payload map, which is a free-form bag the core
// copies from a caller's Lua table and so has no shape to hold still. See the
// comment on Provenance in actor.go for why the tenant must never live there.

// inboxStr stamps a tenant the way the identity sink does: a pointer, so that
// "known empty" and "unknown" stay distinct on the wire.
func inboxStr(s string) *string { return &s }

// inboxMessageFixture is a Message with every field set, so the whole wire
// shape is legible in one place rather than inferred from the struct tags. The
// userUUID argument is the three-state tenant: nil is unknown (nothing stamped
// it), a pointer to "" is known-empty, and a pointer to a uuid is the tenant.
func inboxMessageFixture(userUUID *string) Message {
	return Message{
		ID:      "m1",
		Type:    "user",
		From:    "user:telegram:42",
		To:      "agent:main",
		Text:    "go",
		Channel: "webhook",
		ReplyTo: "r1",
		Payload: map[string]any{"k": "v"},
		Attachments: []media.Part{
			{Type: "text", Text: "hi"},
		},
		Provenance: &Provenance{
			Kind:      "channel",
			Principal: "user:telegram:42",
			Parent:    "main",
			RequestID: "req-1",
			UserUUID:  userUUID,
		},
		Ts: 1700000000,
	}
}

// TestInboxWireShape pins the bytes session.inbox() hands to Lua, field order
// included: jsonString is the function the inbox op calls, and encoding/json
// emits struct fields in declaration order, so a reordered struct is a wire
// change and is treated as one. The literals below are the contract — each is
// what a loop sees for one of the three tenant states, plus the shape for a
// producer that stamped no provenance at all.
func TestInboxWireShape(t *testing.T) {
	noProvenance := inboxMessageFixture(nil)
	noProvenance.Provenance = nil

	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{
			// A loop may read the tenant: user_uuid is present and carries the uuid.
			name: "tenant stamped",
			msg:  inboxMessageFixture(inboxStr("u_A")),
			want: `{"id":"m1","type":"user","from":"user:telegram:42","to":"agent:main","text":"go","channel":"webhook","reply_to":"r1","payload":{"k":"v"},"attachments":[{"type":"text","text":"hi"}],"provenance":{"kind":"channel","principal":"user:telegram:42","parent":"main","request_id":"req-1","user_uuid":"u_A"},"ts":1700000000}`,
		},
		{
			// A loop must be able to tell "belongs to no tenant" from "nobody said".
			// The key is PRESENT and empty; omitempty drops a nil pointer but not a
			// pointer to "", which is exactly what keeps the two states apart here.
			name: "known empty",
			msg:  inboxMessageFixture(inboxStr("")),
			want: `{"id":"m1","type":"user","from":"user:telegram:42","to":"agent:main","text":"go","channel":"webhook","reply_to":"r1","payload":{"k":"v"},"attachments":[{"type":"text","text":"hi"}],"provenance":{"kind":"channel","principal":"user:telegram:42","parent":"main","request_id":"req-1","user_uuid":""},"ts":1700000000}`,
		},
		{
			// Unknown: the key is absent, not empty. A loop that reads it gets nil.
			name: "unknown",
			msg:  inboxMessageFixture(nil),
			want: `{"id":"m1","type":"user","from":"user:telegram:42","to":"agent:main","text":"go","channel":"webhook","reply_to":"r1","payload":{"k":"v"},"attachments":[{"type":"text","text":"hi"}],"provenance":{"kind":"channel","principal":"user:telegram:42","parent":"main","request_id":"req-1"},"ts":1700000000}`,
		},
		{
			// A producer with no provenance in scope: the key is absent entirely, so
			// msg.provenance is nil in Lua and msg.provenance.user_uuid would raise.
			name: "no provenance",
			msg:  noProvenance,
			want: `{"id":"m1","type":"user","from":"user:telegram:42","to":"agent:main","text":"go","channel":"webhook","reply_to":"r1","payload":{"k":"v"},"attachments":[{"type":"text","text":"hi"}],"ts":1700000000}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jsonString(tc.msg)
			if err != nil {
				t.Fatalf("marshalling the inbox message: %v", err)
			}
			if got != tc.want {
				t.Fatalf("the inbox wire shape changed.\n got: %s\nwant: %s\n\n"+
					"This shape is part of the prelude API version contract (vm.PreludeVersion): "+
					"a loop reads these keys off session.inbox(). If the change is intended, bump "+
					"vm.PreludeVersion and update this literal in the same commit, so a chunk that "+
					"declares the old number is refused at load rather than misreading the new shape.",
					got, tc.want)
			}
		})
	}
}

// TestInboxShapeReachesLua is the other end of the same seam: the bytes above,
// decoded by a running loop through the real inbox op. It reports the key set
// it was handed (sorted, so the comparison is order-free) and the state of the
// tenant as the loop itself can tell it apart — the point of the three-state
// field is that Lua can distinguish all three, so the test asserts it the way a
// chunk would.
//
// Asserting on the Go struct instead would have missed the decode: a field
// dropped between marshal and the table, or an omitempty that swallowed a
// known-empty, shows up here and nowhere else.
func TestInboxShapeReachesLua(t *testing.T) {
	const loopSrc = `
function loop()
  local msg = session.inbox()
  local mk = {}
  for k in pairs(msg) do mk[#mk+1] = k end
  table.sort(mk)
  local pk = {}
  local tenant = "ABSENT"
  if type(msg.provenance) == "table" then
    for k in pairs(msg.provenance) do pk[#pk+1] = k end
    table.sort(pk)
    local uu = msg.provenance.user_uuid
    if uu == nil then tenant = "ABSENT"
    elseif uu == "" then tenant = "EMPTY"
    else tenant = uu end
  end
  session.send("msg[" .. table.concat(mk, " ") .. "] provenance[" .. table.concat(pk, " ") ..
               "] user_uuid=" .. tenant)
end
`

	fullMsgKeys := "attachments channel from id payload provenance reply_to text to ts type"
	fullProvKeys := "kind parent principal request_id user_uuid"
	// Unknown: the key is absent from the wire, so it is absent from the table a
	// loop can iterate. That asymmetry is the three-state field's whole point and
	// is worth stating in the expectation rather than in a comment on the code.
	unknownProvKeys := "kind parent principal request_id"

	noProvenance := inboxMessageFixture(nil)
	noProvenance.Provenance = nil

	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "tenant stamped",
			msg:  inboxMessageFixture(inboxStr("u_A")),
			want: "msg[" + fullMsgKeys + "] provenance[" + fullProvKeys + "] user_uuid=u_A",
		},
		{
			name: "known empty",
			msg:  inboxMessageFixture(inboxStr("")),
			want: "msg[" + fullMsgKeys + "] provenance[" + fullProvKeys + "] user_uuid=EMPTY",
		},
		{
			name: "unknown",
			msg:  inboxMessageFixture(nil),
			want: "msg[" + fullMsgKeys + "] provenance[" + unknownProvKeys + "] user_uuid=ABSENT",
		},
		{
			name: "no provenance",
			msg:  noProvenance,
			want: "msg[attachments channel from id payload reply_to text to ts type] provenance[] user_uuid=ABSENT",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeGW{}
			a, _ := newTestActor(t, map[string]bool{}, gw, loopSrc)
			a.Mailbox <- tc.msg

			waitFor(t, "the loop's report on the inbox shape", 5*time.Second, func() bool {
				return len(gw.snapshot()) > 0
			})
			sends := gw.snapshot()
			if len(sends) != 1 {
				t.Fatalf("expected exactly one report, got %d: %+v", len(sends), sends)
			}
			if sends[0].text != tc.want {
				t.Fatalf("the shape a loop receives from session.inbox() changed.\n got: %s\nwant: %s\n\n"+
					"This shape is part of the prelude API version contract (vm.PreludeVersion). If the "+
					"change is intended, bump vm.PreludeVersion and update this expectation in the same "+
					"commit, so a chunk that declares the old number is refused at load.",
					sends[0].text, tc.want)
			}
		})
	}
}
