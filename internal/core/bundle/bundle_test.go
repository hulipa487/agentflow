package bundle

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"agentflow/internal/core/identity"
	"agentflow/internal/core/memory"
	"agentflow/internal/drivers/volatile"
)

func testKey(t *testing.T) (pub, priv []byte) {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().Bytes(), k.Bytes()
}

func TestSealOpenRoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	env, err := Seal(pub, []byte("sk-live-secret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(priv, env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sk-live-secret" {
		t.Fatalf("round trip = %q", got)
	}

	otherPub, otherPriv := testKey(t)
	_ = otherPub
	if _, err := Open(otherPriv, env); err == nil {
		t.Fatal("the wrong private key decrypted the envelope")
	}
}

func TestZipRoundTrip(t *testing.T) {
	pub, _ := testKey(t)
	env, err := Seal(pub, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		Version: Version,
		Profile: identity.Profile{UserID: "u_1", DisplayName: "Oscar"},
		Memory:  []MemoryRow{{Agent: "bot", Table: "bot.dialogue", Key: "turn:1", Value: "hi"}},
		Files:   []FileEntry{{Project: "p", Path: "a.txt", Data: []byte("body")}},
		Credentials: []Credential{
			{Service: "github", Kind: "api_key", Envelope: env},
		},
	}
	data, err := b.Zip()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadZip(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Profile.UserID != "u_1" || len(back.Memory) != 1 || len(back.Files) != 1 || len(back.Credentials) != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if string(back.Files[0].Data) != "body" {
		t.Fatalf("file bytes = %q", back.Files[0].Data)
	}
}

func TestUserScopedKey(t *testing.T) {
	cases := []struct {
		key, user, want string
		ok              bool
	}{
		{"user:u_1|turn:1", "u_1", "turn:1", true},
		{"user:u_1@3|turn:1", "u_1", "turn:1", true},
		{"user:u_2|turn:1", "u_1", "", false},
		{"service|turn:1", "u_1", "", false},
		{"turn:1", "u_1", "", false},
	}
	for _, c := range cases {
		got, ok := userScopedKey(c.key, c.user)
		if got != c.want || ok != c.ok {
			t.Errorf("userScopedKey(%q, %q) = %q, %v; want %q, %v", c.key, c.user, got, ok, c.want, c.ok)
		}
	}
}

// TestExportImportIdentity is the round trip that needs no other store: a
// profile and its handles move to a second cell with the same uuid.
func TestExportImportIdentity(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src, err := identity.Open(filepath.Join(t.TempDir(), "src.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	p, err := src.CreateProfile("Oscar", "oscar@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Resolve("telegram", "tg-1", "chat-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Link(p.UserID, "tg-1"); err != nil {
		t.Fatal(err)
	}

	exporter := &Service{Identity: src}
	b, err := exporter.Export(ctx, p.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Profile.Identities) != 1 {
		t.Fatalf("exported identities = %d, want 1", len(b.Profile.Identities))
	}

	dst, err := identity.Open(filepath.Join(t.TempDir(), "dst.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	rep, err := (&Service{Identity: dst}).Import(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Profile || rep.Identities != 1 {
		t.Fatalf("import report = %+v", rep)
	}
	got, ok, err := dst.Get(p.UserID)
	if err != nil || !ok {
		t.Fatalf("destination profile: ok=%v err=%v", ok, err)
	}
	if got.Email != "oscar@example.com" || len(got.Identities) != 1 || got.Identities[0].NativeFrom != "tg-1" {
		t.Fatalf("destination profile = %+v", got)
	}
}

// memoryFixture opens one agent binding on a volatile backend and returns the
// manager, the resolved memory, and a scoped handle for a user.
func memoryFixture(t *testing.T, userID string) (*memory.Manager, memory.AgentMemory, memory.BackendHandle, memory.StoreBinding) {
	t.Helper()
	reg := memory.NewRegistry()
	reg.RegisterProvider(volatile.Provider{})
	reg.AddBackend("v", "volatile", map[string]any{"max_entries": 100})
	if err := reg.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	mgr := memory.NewManager(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	am, err := reg.ResolveStoresFor("bot", map[string]memory.Store{
		"dialogue": {Backend: "v", Table: "dialogue", Scope: "user"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, bind, err := mgr.BindForTable(am, "dialogue")
	if err != nil {
		t.Fatal(err)
	}
	scoped := memory.WrapScoped(h, bind.Scoping, memory.ModeInteractive,
		memory.Caller{ScopeUUID: userID, PersonalUUID: userID, Locality: bind.Locality}, memory.PoolScope{})
	return mgr, am, scoped, bind
}

// TestMemoryRoundTrip: the export reads the user's scope through the binding's
// logical table and the import writes it back through the destination's own
// binding.
func TestMemoryRoundTrip(t *testing.T) {
	srcMgr, srcAM, srcScoped, srcBind := memoryFixture(t, "u_1")
	if err := srcScoped.Put(srcBind.Table, "turn:1", "hello", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	src := &Service{Memory: srcMgr, Memories: []memory.AgentMemory{srcAM}}
	rows, err := src.exportMemory("u_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Agent != "bot" || rows[0].Key != "turn:1" || rows[0].Table != "bot.dialogue" {
		t.Fatalf("exported rows = %+v", rows)
	}

	dstMgr, dstAM, dstScoped, dstBind := memoryFixture(t, "u_1")
	dst := &Service{Memory: dstMgr, Memories: []memory.AgentMemory{dstAM}}
	n, err := dst.importMemory(&Bundle{Profile: identity.Profile{UserID: "u_1"}, Memory: rows})
	if err != nil || n != 1 {
		t.Fatalf("importMemory = %d, %v", n, err)
	}
	v, ok, err := dstScoped.Get(dstBind.Table, "turn:1")
	if err != nil || !ok || v != "hello" {
		t.Fatalf("imported row = %v, %v, %v", v, ok, err)
	}
}
