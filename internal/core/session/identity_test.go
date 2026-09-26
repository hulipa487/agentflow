package session

import (
	"context"
	"encoding/json"
	"testing"
)

// This file pins the two halves of the tenant-identity change at their source:
// the three-state provenance field (§6.1) and the acting-identity struct with
// its per-role accessors (§7). Both are boundaries, so they are asserted rather
// than described.

// TestActingIdentityOfProvenanceModel is the three-state contract. The
// distinction between "nobody stamped this" and "stamped as belonging to
// nobody" is load-bearing: only the former may fall back to the sender address,
// and for an unlinked handle that address carries a stable identity id that
// must never become a scope uuid.
func TestActingIdentityOfProvenanceModel(t *testing.T) {
	stamped := func(u string) *string { return &u }
	tests := []struct {
		name    string
		msg     *Message
		inherit bool
		want    string
		known   bool
	}{
		{
			name:  "no message at all",
			msg:   nil,
			known: false,
		},
		{
			name:  "no provenance, agent sender",
			msg:   &Message{From: "agent:planner"},
			known: false,
		},
		{
			name:  "no provenance, user sender (identity layer disabled)",
			msg:   &Message{From: "user:telegram:123"},
			want:  "telegram:123",
			known: true,
		},
		{
			name:  "system provenance is unknown, not empty",
			msg:   &Message{From: "system:supervisor", Provenance: &Provenance{Kind: "system"}},
			known: false,
		},
		{
			name: "stamped tenant wins over the sender address",
			msg: &Message{From: "user:someone-else", Provenance: &Provenance{
				Kind: "channel", UserUUID: stamped("u_A"),
			}},
			want:  "u_A",
			known: true,
		},
		{
			// An unlinked handle: the sink stamps an explicit empty, and the
			// sender address is the handle's identity id.
			name: "explicit empty never falls back to the sender",
			msg: &Message{From: "user:i_abc123", Provenance: &Provenance{
				Kind: "channel", UserUUID: stamped(""),
			}},
			known: true,
		},
		{
			name: "an agent hop inherits the tenant it was handed",
			msg: &Message{From: "agent:planner", Provenance: &Provenance{
				Kind: "agent", UserUUID: stamped("u_A"),
			}},
			inherit: true,
			want:    "u_A",
			known:   true,
		},
		{
			// inherit_user: false. The person talking to the agent directly
			// still reaches it (a channel kind, above); a delegating agent's
			// tenant does not.
			name: "an opted-out agent refuses a hop tenant",
			msg: &Message{From: "agent:planner", Provenance: &Provenance{
				Kind: "agent", UserUUID: stamped("u_A"),
			}},
			inherit: false,
			known:   true,
		},
		{
			name: "the payload is not an authority",
			msg: &Message{From: "agent:planner", Payload: map[string]any{
				"user_uuid":  "u_tenant_b",
				"principal":  "u_tenant_b",
				"provenance": "u_tenant_b",
			}},
			known: false,
		},
		{
			name: "the payload cannot override a stamp",
			msg: &Message{From: "agent:planner", Payload: map[string]any{
				"user_uuid": "u_tenant_b",
			}, Provenance: &Provenance{Kind: "agent", UserUUID: stamped("u_A")}},
			inherit: true,
			want:    "u_A",
			known:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, known := actingIdentityOf(tc.msg, tc.inherit)
			if known != tc.known {
				t.Fatalf("known = %v, want %v", known, tc.known)
			}
			if got := id.ScopeUUID(); got != tc.want {
				t.Fatalf("scope uuid = %q, want %q", got, tc.want)
			}
			// The scope prefix is the resolved form the scope sites use; it must
			// agree with the uuid or group context would resolve asymmetrically.
			if tc.want == "" && id.ScopePrefix != "" {
				t.Fatalf("no scope must resolve to no prefix, got %q", id.ScopePrefix)
			}
			if tc.want != "" && id.ScopePrefix != "user:"+tc.want {
				t.Fatalf("prefix = %q, want user:%s", id.ScopePrefix, tc.want)
			}
		})
	}
}

// TestActingIdentityRoles is the §7 shape: one identity, three jobs, and group
// context separates them. A consumer that names the wrong role is the bug this
// type exists to make impossible to write by accident.
func TestActingIdentityRoles(t *testing.T) {
	personal := WithPersonalIdentity(context.Background(), "u_A")
	if got := ScopeUUIDFromCtx(personal); got != "u_A" {
		t.Fatalf("personal scope uuid = %q", got)
	}
	if got := BillingUUIDFromCtx(personal); got != "u_A" {
		t.Fatalf("personal billing uuid = %q", got)
	}
	if got := CredentialUUIDFromCtx(personal); got != "u_A" {
		t.Fatalf("personal credential uuid = %q", got)
	}
	if got := PersonalUUIDFromCtx(personal); got != "u_A" {
		t.Fatalf("personal person uuid = %q", got)
	}
	if got := ScopePrefixFromCtx(personal); got != "user:u_A" {
		t.Fatalf("personal scope prefix = %q", got)
	}

	// Group context: acting as U in group G. The membership uuid is the scope —
	// private to (person, group) — while billing, credentials and the person
	// stay the personal uuid. Billing must not fragment per group (F23): the
	// ledger is the billing mechanism.
	group := WithActingIdentity(context.Background(), ActingIdentity{
		Personal:    "u_A",
		Membership:  "m_A_G",
		ScopePrefix: "user:m_A_G",
	})
	if got := ScopeUUIDFromCtx(group); got != "m_A_G" {
		t.Fatalf("group scope uuid = %q, want the membership uuid", got)
	}
	if got := BillingUUIDFromCtx(group); got != "u_A" {
		t.Fatalf("group billing uuid = %q, want the personal uuid", got)
	}
	if got := CredentialUUIDFromCtx(group); got != "u_A" {
		t.Fatalf("group credential uuid = %q, want the personal uuid", got)
	}
	if got := PersonalUUIDFromCtx(group); got != "u_A" {
		t.Fatalf("group person uuid = %q", got)
	}
	if got := ScopePrefixFromCtx(group); got != "user:m_A_G" {
		t.Fatalf("group scope prefix = %q", got)
	}
	// The membership uuid is carried for attribution, and is not the person.
	if id, _ := ActingFromCtx(group); id.Membership != "m_A_G" {
		t.Fatalf("membership must ride the identity for attribution, got %q", id.Membership)
	}

	// An unstamped context resolves to nothing at all — no scope, no billing
	// identity, no fallbacks.
	var empty ActingIdentity
	if id, ok := ActingFromCtx(context.Background()); ok || id != empty {
		t.Fatalf("an unstamped context must read as unknown, got %+v ok=%v", id, ok)
	}
	if got := ScopePrefixFromCtx(nil); got != "" {
		t.Fatalf("nil ctx prefix = %q", got)
	}
}

// TestProvenanceTenantSurvivesJSONRoundTrip: a fleet delivers a session's mail
// through the durable inbox, which stores a message as JSON. The three states
// must survive that trip, or an instance boundary would silently turn "no
// tenant" into "unknown" and hand a downstream turn a scope built from the
// sender address instead.
func TestProvenanceTenantSurvivesJSONRoundTrip(t *testing.T) {
	stamped := func(u string) *string { return &u }
	tests := []struct {
		name  string
		in    *string
		known bool
		want  string
	}{
		{name: "unknown stays unknown", in: nil, known: false},
		{name: "explicit empty stays empty", in: stamped(""), known: true},
		{name: "a tenant stays itself", in: stamped("u_A"), known: true, want: "u_A"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(Message{
				ID: "m1", From: "agent:planner",
				Provenance: &Provenance{Kind: "agent", UserUUID: tc.in},
			})
			if err != nil {
				t.Fatal(err)
			}
			var out Message
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			id, known := actingIdentityOf(&out, true)
			if known != tc.known {
				t.Fatalf("after the round trip known = %v, want %v (%s)", known, tc.known, b)
			}
			if got := id.ScopeUUID(); got != tc.want {
				t.Fatalf("after the round trip scope = %q, want %q (%s)", got, tc.want, b)
			}
		})
	}
}

// TestInheritsUserDefault pins the opt-out's polarity: the zero value inherits.
// An Info built without the field (tests, builtin loops, any future caller that
// forgets it) must keep propagation on — the failure mode of the other default
// is silent tenant mixing, which is the bug this whole change removes.
func TestInheritsUserDefault(t *testing.T) {
	var nilInfo *Info
	if !nilInfo.InheritsUser() {
		t.Fatal("a nil Info must inherit")
	}
	if !(&Info{}).InheritsUser() {
		t.Fatal("an unset inherit_user must inherit")
	}
	yes, no := true, false
	if !(&Info{InheritUser: &yes}).InheritsUser() {
		t.Fatal("inherit_user: true must inherit")
	}
	if (&Info{InheritUser: &no}).InheritsUser() {
		t.Fatal("inherit_user: false must not inherit")
	}
}

// TestActingIdentityGroupContext pins the group-context half of §7: the
// engine-stamped membership uuid becomes the scope uuid and the scope prefix,
// while the personal uuid stays billing and credentials. The stamp is engine
// arithmetic — the deliver boundary derived it after validating membership —
// so from this layer down it is trusted input, exactly like UserUUID.
func TestActingIdentityGroupContext(t *testing.T) {
	personal, membership := "person-1", "membership-1"
	stamped := &Message{
		From: "webhook:x",
		Provenance: &Provenance{
			Kind:           "channel",
			UserUUID:       &personal,
			MembershipUUID: &membership,
		},
	}
	id, known := actingIdentityOf(stamped, true)
	if !known {
		t.Fatal("a stamped group message resolved unknown")
	}
	if id.Personal != personal {
		t.Fatalf("personal = %q; the person never changes", id.Personal)
	}
	if id.Membership != membership {
		t.Fatalf("membership = %q; want the stamped uuid", id.Membership)
	}
	if id.ScopePrefix != "user:"+membership {
		t.Fatalf("scope prefix = %q; want user:<membership>", id.ScopePrefix)
	}
	if id.ScopeUUID() != membership {
		t.Fatalf("scope uuid = %q; want the membership uuid", id.ScopeUUID())
	}

	// Billing follows the person even in group context — the ledger never
	// fragments per group (F23).
	ctx := WithActingIdentity(context.Background(), id)
	if got := BillingUUIDFromCtx(ctx); got != personal {
		t.Fatalf("billing uuid = %q; want the personal uuid", got)
	}
	if got := ScopePrefixFromCtx(ctx); got != "user:"+membership {
		t.Fatalf("scope prefix from ctx = %q", got)
	}

	// An explicit empty membership stamp is known-none: personal context.
	empty := ""
	if id2, _ := actingIdentityOf(&Message{Provenance: &Provenance{Kind: "channel", UserUUID: &personal, MembershipUUID: &empty}}, true); id2.Membership != "" {
		t.Fatalf("empty stamp produced membership %q", id2.Membership)
	}
}
