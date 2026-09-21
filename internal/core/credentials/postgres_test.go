package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// postgresDSN points the shared-store case at a server:
//
//	AGENTFLOW_TEST_POSTGRES='postgres://user:pass@localhost:5432/agentflow?sslmode=disable' \
//	  go test ./internal/core/credentials/
//
// Unset, it skips. Rows are tagged per run, so a server that already holds
// credentials is safe to test against.
var postgresDSN = os.Getenv("AGENTFLOW_TEST_POSTGRES")

// The credential statements on the backend a fleet runs on: the ciphertext
// column is text, the upsert is a conflict on (user_uuid, service), and a key
// stored on one instance resolves on another.
func TestPostgresSharedCredentials(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(b)
	user, service := "u_"+tag, "svc-"+tag

	a, err := Open(postgresDSN, "master-key", testLogger())
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	other, err := Open(postgresDSN, "master-key", testLogger())
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer other.Close()
	ctx := context.Background()

	if err := a.Put(ctx, user, service, "api", "s3cret-value", "", ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	sec, ok, err := other.Get(ctx, user, service)
	if err != nil || !ok || sec.Value != "s3cret-value" {
		t.Fatalf("get on another instance: %+v ok=%v err=%v", sec, ok, err)
	}
	refs, err := other.List(ctx, user)
	if err != nil || len(refs) == 0 {
		t.Fatalf("list: %+v err=%v", refs, err)
	}
	if refs[0].Fingerprint == "" {
		t.Fatalf("fingerprint should decrypt for the listing: %+v", refs[0])
	}
	// The upsert replaces rather than duplicating.
	if err := other.Put(ctx, user, service, "api", "rotated-value", "", ""); err != nil {
		t.Fatalf("put again: %v", err)
	}
	if sec, _, _ := a.Get(ctx, user, service); sec.Value != "rotated-value" {
		t.Fatalf("rotation did not take effect: %q", sec.Value)
	}
	if refs, _ := a.List(ctx, user); len(refs) != 1 {
		t.Fatalf("the upsert duplicated a row: %+v", refs)
	}
	if err := a.Delete(ctx, user, service); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := other.Get(ctx, user, service); err != nil || ok {
		t.Fatalf("a delete must hold everywhere: ok=%v err=%v", ok, err)
	}
}
