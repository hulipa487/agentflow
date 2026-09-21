package credentials

import (
	"context"
	"path/filepath"
	"testing"
)

// Two stores over one target are two instances of the engine sharing a
// credential database: a key an operator adds on one resolves on the other,
// which is the whole point of pointing a fleet at a server.
func TestSharedStoreServesBothInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.db")
	a, err := Open(path, "master-key", testLogger())
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := Open(path, "master-key", testLogger())
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()
	ctx := context.Background()

	if err := a.Put(ctx, "u_1", "github", "api_key", "ghp_secret", "", ""); err != nil {
		t.Fatalf("put on a: %v", err)
	}
	sec, ok, err := b.Get(ctx, "u_1", "github")
	if err != nil {
		t.Fatalf("get on b: %v", err)
	}
	if !ok || sec.Value != "ghp_secret" {
		t.Fatalf("a key stored on one instance must resolve on another: ok=%v value=%q", ok, sec.Value)
	}
	// A revocation on one instance is a revocation everywhere.
	if err := a.Delete(ctx, "u_1", "github"); err != nil {
		t.Fatalf("delete on a: %v", err)
	}
	if _, ok, err := b.Get(ctx, "u_1", "github"); err != nil || ok {
		t.Fatalf("a delete on one instance must hold on another: ok=%v err=%v", ok, err)
	}
	// And the enumeration the admin console uses agrees.
	if err := b.Put(ctx, "u_2", "openai", "api", "sk-x", "", ""); err != nil {
		t.Fatalf("put on b: %v", err)
	}
	users, err := a.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users on a: %v", err)
	}
	found := false
	for _, u := range users {
		if u == "u_2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a write on one instance must show up in the other's listing, got %v", users)
	}
	// The ciphertext is encrypted under the deployment's master key, so a store
	// opened with a different one must fail loudly rather than return nonsense.
	c, err := Open(path, "different-key", testLogger())
	if err != nil {
		t.Fatalf("open c: %v", err)
	}
	defer c.Close()
	if _, ok, err := c.Get(ctx, "u_2", "openai"); err == nil || ok {
		t.Fatalf("a wrong master key must surface as an error: ok=%v err=%v", ok, err)
	}
}
