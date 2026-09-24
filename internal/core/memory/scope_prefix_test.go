package memory_test

import (
	"testing"

	"agentflow/internal/core/memory"
)

// TestScopedPrefixCannotBePointedAtAnotherScope: the prefix path runs one range
// query per allowed stratum and then one more for the RAW prefix — and a loop
// chooses that prefix. Passing "user:<someone-else>|" made the extra scan match
// the other user's rows, and those results were stripped but never re-checked,
// so the one path that left isolation to the backend's prefix semantics could
// return a row the caller may not read. Every path re-checks now.
func TestScopedPrefixCannotBePointedAtAnotherScope(t *testing.T) {
	h := openOne(t)
	alice := wrap(h, "user", memory.ModeInteractive, "u_alice")
	bob := wrap(h, "user", memory.ModeInteractive, "u_bob")

	if err := alice.Put("t", "secret", "alice-only", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := bob.Put("t", "secret", "bob-only", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}

	// Bob asks for a prefix naming Alice's scope outright.
	it, err := bob.Query("t", memory.Query{Kind: "prefix", Prefix: "user:u_alice|"})
	if err != nil {
		t.Fatal(err)
	}
	for it.Next() {
		t.Fatalf("bob read alice's row through a crafted prefix: %+v", it.Record())
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}

	// The ordinary case still works: each user sees their own row, and only it.
	for name, handle := range map[string]memory.BackendHandle{"alice": alice, "bob": bob} {
		it, err := handle.Query("t", memory.Query{Kind: "prefix", Prefix: "sec"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for it.Next() {
			got = append(got, it.Record().Key)
		}
		if err := it.Err(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0] != "secret" {
			t.Errorf("%s saw %v; want exactly the own-scope key \"secret\"", name, got)
		}
	}
}

// TestScopedPrefixStillSeesLegacyRows: unscoped rows written before scoping
// existed stay readable — that is the whole reason the raw prefix is in the
// query list, and the filter must not cost it.
func TestScopedPrefixStillSeesLegacyRows(t *testing.T) {
	h := openOne(t)
	// Written straight to the store, as a pre-isolation release would have.
	if err := h.Put("t", "old", "legacy", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	alice := wrap(h, "user", memory.ModeInteractive, "u_alice")

	it, err := alice.Query("t", memory.Query{Kind: "prefix", Prefix: "ol"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for it.Next() {
		got = append(got, it.Record().Key)
	}
	if len(got) != 1 || got[0] != "old" {
		t.Fatalf("alice saw %v; a legacy row must stay readable", got)
	}
}
