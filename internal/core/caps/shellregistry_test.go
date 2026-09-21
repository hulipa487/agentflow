package caps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"agentflow/internal/core/runtime"
	"agentflow/internal/drivers/shell"
)

// This is the composition a fleet runs: the registry the engine installs, over
// the runtime store the engine opens, through the real manager. The manager's
// own tests use an in-memory registry, and the store's tests use fixed records;
// this is the pair together, which is where a key layout or a decode that only
// works in one of them would show up.

// handleProvider is a Docker-shaped provider: its handles are containers, and
// the container id is the address a record carries.
type handleProvider struct {
	tag      string
	next     int
	spawns   []string
	destroys []string
	forgets  []string
}

func (p *handleProvider) Name() string { return "docker" }

func (p *handleProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	p.next++
	container := p.tag + "-container-" + strconv.Itoa(p.next)
	p.spawns = append(p.spawns, container)
	return &shell.Handle{
		ID:    p.tag + "-handle-" + strconv.Itoa(p.next),
		State: shell.HandleRunning,
		Meta:  map[string]any{"container": container},
	}, nil
}

func (p *handleProvider) Exec(ctx context.Context, h *shell.Handle, cmd string) (*shell.ExecResult, error) {
	return &shell.ExecResult{Stdout: "ok: " + cmd, ExitCode: 0}, nil
}

func (p *handleProvider) Read(ctx context.Context, h *shell.Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}

func (p *handleProvider) Write(ctx context.Context, h *shell.Handle, path string, content []byte) error {
	return nil
}

func (p *handleProvider) Destroy(ctx context.Context, h *shell.Handle) error {
	if container, ok := h.Meta["container"].(string); ok {
		p.destroys = append(p.destroys, container)
	}
	h.State = shell.HandleDestroyed
	return nil
}

func (p *handleProvider) Alive(h *shell.Handle) bool { return h.State == shell.HandleRunning }

func (p *handleProvider) Attach(rec shell.Record) (*shell.Handle, error) {
	if rec.Container == "" {
		return nil, fmt.Errorf("no container in record %s", rec.ID)
	}
	return &shell.Handle{ID: rec.ID, Provider: rec.Provider, State: shell.HandleRunning, Meta: rec.Meta}, nil
}

func (p *handleProvider) Forget(ctx context.Context, rec shell.Record) error {
	p.forgets = append(p.forgets, rec.Container)
	return nil
}

// runHandleCase is the body both backends run: one session, two instances, one
// store.
func runHandleCase(t *testing.T, st runtime.Store, tag string) {
	t.Helper()
	ctx := context.Background()
	reg := ShellStore{Store: st}

	aProv := &handleProvider{tag: tag + "a"}
	bProv := &handleProvider{tag: tag + "b"}
	a := shell.NewManager([]shell.ShellProvider{aProv}, nil)
	b := shell.NewManager([]shell.ShellProvider{bProv}, nil)
	a.SetRegistry(reg, tag+"instance-a")
	b.SetRegistry(reg, tag+"instance-b")

	const owner = "bot|chat-shared"
	h, err := a.Spawn(ctx, owner, "docker", shell.SpawnOpts{Image: "alpine"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// The record is readable from the store directly, which is what a third
	// instance would do.
	rec, ok, err := reg.Load(ctx, h.ID)
	if err != nil || !ok {
		t.Fatalf("the record did not reach the store: ok=%v err=%v", ok, err)
	}
	if rec.Container != tag+"a-container-1" || rec.Owner != owner {
		t.Fatalf("record = %+v", rec)
	}

	// The session moves: the other instance reaches the container this one made.
	res, err := b.Exec(ctx, owner, h.ID, "ls")
	if err != nil {
		t.Fatalf("exec after the session moved: %v", err)
	}
	if res.Stdout != "ok: ls" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if len(bProv.spawns) != 0 {
		t.Fatalf("the second instance spawned its own container: %v", bProv.spawns)
	}

	// A shell the session spawns but never uses again stays on this instance's
	// books only: the second instance has never held it.
	second, err := a.Spawn(ctx, owner, "docker", shell.SpawnOpts{Image: "alpine"})
	if err != nil {
		t.Fatalf("second spawn: %v", err)
	}

	// When the session ends, both containers go with it — the one that was
	// adopted here through the provider, and the one that only exists as a
	// record, which is the case a killed instance leaves behind.
	b.ReapSession(ctx, owner)
	if len(bProv.destroys) != 1 || bProv.destroys[0] != tag+"a-container-1" {
		t.Fatalf("the adopted container was not destroyed: %v", bProv.destroys)
	}
	if len(bProv.forgets) != 1 || bProv.forgets[0] != tag+"a-container-2" {
		t.Fatalf("the container this instance never held was not released: %v", bProv.forgets)
	}
	for _, id := range []string{h.ID, second.ID} {
		if _, ok, _ := reg.Load(ctx, id); ok {
			t.Fatalf("the record for %s survived the reap", id)
		}
	}
}

// The default backend: a local file, one process, which is what most
// deployments run.
func TestShellHandlesAreSharedThroughTheRuntimeStore(t *testing.T) {
	st, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runHandleCase(t, st, "sqlite-")
}

// The same case on the backend a fleet actually shares, where the two managers
// stand in for two processes.
func TestPostgresShellHandlesAreShared(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	st, err := runtime.OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runHandleCase(t, st, pgTag(t))
}

// pgTag makes a per-run prefix, so a suite that runs twice against a server
// that already holds rows stays honest.
func pgTag(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "sh" + hex.EncodeToString(b) + "-"
}
