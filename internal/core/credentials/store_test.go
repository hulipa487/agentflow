package credentials

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func tempDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.db")
	t.Cleanup(func() { os.Remove(p) })
	return p
}

func TestPutGetRoundTrip(t *testing.T) {
	s, err := Open(tempDB(t), "master-key", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, "u_1", "github", "api_key", "ghp_secret", "", ""); err != nil {
		t.Fatal(err)
	}
	sec, ok, err := s.Get(ctx, "u_1", "github")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected credential found")
	}
	if sec.Value != "ghp_secret" {
		t.Fatalf("value = %q", sec.Value)
	}
	if sec.Header != "Authorization" || sec.Scheme != "Bearer" {
		t.Fatalf("defaults not applied: header=%q scheme=%q", sec.Header, sec.Scheme)
	}
}

func TestEncryptionAtRest(t *testing.T) {
	p := tempDB(t)
	s, err := Open(p, "master-key", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "u_1", "svc", "api_key", "sk-plaintext-visible", "", ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Read the raw DB file: the plaintext secret must not appear anywhere.
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-plaintext-visible") {
		t.Fatal("plaintext secret found in db file — not encrypted at rest")
	}
}

func TestWrongMasterKeyErrors(t *testing.T) {
	p := tempDB(t)
	s, err := Open(p, "correct-key", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "u_1", "svc", "api_key", "secret-123", "", ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(p, "wrong-key", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err := s2.Get(ctx, "u_1", "svc"); err == nil {
		t.Fatal("expected decrypt error with wrong master key")
	}
}

func TestDeleteRemoves(t *testing.T) {
	s, err := Open(tempDB(t), "k", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, "u_1", "svc", "api_key", "secret", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "u_1", "svc"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(ctx, "u_1", "svc"); err != nil || ok {
		t.Fatalf("expected gone after delete: ok=%v err=%v", ok, err)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, err := Open(tempDB(t), "k", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	_ = s.Put(ctx, "u_a", "svc", "api_key", "a-secret", "", "")
	if _, ok, _ := s.Get(ctx, "u_b", "svc"); ok {
		t.Fatal("user b must not see user a's credential")
	}
}

func TestListReturnsNamesOnly(t *testing.T) {
	s, err := Open(tempDB(t), "k", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	_ = s.Put(ctx, "u_1", "github", "api_key", "ghp_x", "", "")
	_ = s.Put(ctx, "u_1", "openai", "api_key", "sk_y", "", "")

	refs, err := s.List(ctx, "u_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	// Values must never surface.
	for _, r := range refs {
		if strings.Contains(r.Service, "ghp_x") || strings.Contains(r.Service, "sk_y") {
			t.Fatalf("service field carries a secret: %q", r.Service)
		}
	}
}
