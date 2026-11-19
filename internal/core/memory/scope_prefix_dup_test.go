package memory_test

import (
	"sort"
	"testing"

	"agentflow/internal/core/memory"
)

// TestScopedPrefixReturnsEachRowOnce: the prefix path adds a legacy scan with
// the loop's raw prefix. Scoped rows live in the same table under
// "user:<id>|...", so a raw prefix that the caller's scope head starts with
// matched the caller's own rows a second time, and a loop that keys its rows
// by the sender got every row back twice.
func TestScopedPrefixReturnsEachRowOnce(t *testing.T) {
	h := openOne(t)
	alice := wrap(h, "user", memory.ModeInteractive, "u_alice")
	bob := wrap(h, "user", memory.ModeInteractive, "u_bob")
	for _, k := range []string{"user:u_alice|a", "user:u_alice|b", "ui:theme"} {
		if err := alice.Put("t", k, "v", memory.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bob.Put("t", "user:u_bob|a", "v", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	// A row from before scoping existed, whose key also starts with "u".
	if err := h.Put("t", "upgrade-note", "legacy", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}

	for prefix, want := range map[string][]string{
		"user:u_alice|": {"user:u_alice|a", "user:u_alice|b"},
		"user:":         {"user:u_alice|a", "user:u_alice|b"},
		"u":             {"ui:theme", "upgrade-note", "user:u_alice|a", "user:u_alice|b"},
		"":              {"ui:theme", "upgrade-note", "user:u_alice|a", "user:u_alice|b"},
	} {
		it, err := alice.Query("t", memory.Query{Kind: "prefix", Prefix: prefix})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for it.Next() {
			got = append(got, it.Record().Key)
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		if len(got) != len(want) {
			t.Errorf("prefix %q returned %v; want each of %v once", prefix, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("prefix %q returned %v; want %v", prefix, got, want)
				break
			}
		}
	}
}
