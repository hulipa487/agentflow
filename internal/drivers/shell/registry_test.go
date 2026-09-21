package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a registry two managers can share --------------------------------------

// memRegistry is a Registry in memory. Two managers hold one, the way two
// instances share one store.
type memRegistry struct {
	mu         sync.Mutex
	recs       map[string]Record
	saves      int
	failSave   bool
	failList   bool
	failDelete bool
}

func newMemRegistry() *memRegistry { return &memRegistry{recs: map[string]Record{}} }

var errStoreDown = errors.New("store down")

func (r *memRegistry) Save(ctx context.Context, rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failSave {
		return errStoreDown
	}
	r.saves++
	r.recs[rec.ID] = rec
	return nil
}

func (r *memRegistry) Load(ctx context.Context, id string) (Record, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[id]
	return rec, ok, nil
}

func (r *memRegistry) List(ctx context.Context, owner string) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failList {
		return nil, errStoreDown
	}
	out := []Record{}
	for _, rec := range r.recs {
		if owner != "" && rec.Owner != owner {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func (r *memRegistry) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failDelete {
		return errStoreDown
	}
	delete(r.recs, id)
	return nil
}

func (r *memRegistry) get(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[id]
	return rec, ok
}

func (r *memRegistry) put(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs[rec.ID] = rec
}

// --- providers ---------------------------------------------------------------

// baseProvider is a shell provider whose handles are addressable containers:
// Spawn mints a container, and its address rides on the handle's Meta — where
// the Docker provider publishes its container id — so the manager can record
// it. It records every call, which is how these tests tell "reached the
// existing container" from "quietly spawned a second one".
//
// name is the provider name, which is the same on every instance of a fleet
// (both are "docker" here); tag distinguishes the two instances' containers, so
// a test can tell whose container an exec landed in.
type baseProvider struct {
	name string
	tag  string

	mu       sync.Mutex
	next     int
	spawns   []string // containers created
	execs    []string
	destroys []string
	attaches []string // records rebuilt into handles
	forgets  []string // records released without a handle
	lastOpts SpawnOpts
}

func newBaseProvider(tag string) *baseProvider { return &baseProvider{name: "docker", tag: tag} }

func (p *baseProvider) Name() string { return p.name }

func (p *baseProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	p.lastOpts = opts
	id := fmt.Sprintf("%s-container-%d", p.tag, p.next)
	handleID := fmt.Sprintf("h-%s-%d", p.tag, p.next)
	p.spawns = append(p.spawns, id)
	return &Handle{
		ID:       handleID,
		State:    HandleRunning,
		Image:    opts.Image,
		Meta:     map[string]any{"container": id},
		internal: id,
	}, nil
}

func (p *baseProvider) Exec(ctx context.Context, handle *Handle, cmd string) (*ExecResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execs = append(p.execs, cmd)
	return &ExecResult{Stdout: "ok: " + cmd}, nil
}

func (p *baseProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}

func (p *baseProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	return nil
}

func (p *baseProvider) Destroy(ctx context.Context, handle *Handle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroys = append(p.destroys, handle.ID)
	handle.State = HandleDestroyed
	return nil
}

func (p *baseProvider) Alive(handle *Handle) bool { return handle.State == HandleRunning }

func (p *baseProvider) spawned() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.spawns...)
}
func (p *baseProvider) forgot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.forgets...)
}
func (p *baseProvider) attached() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.attaches...)
}

// addressableProvider can rebuild a handle from a record anywhere: the Docker
// shape.
type addressableProvider struct{ *baseProvider }

func (p addressableProvider) Attach(rec Record) (*Handle, error) {
	if rec.Container == "" {
		return nil, fmt.Errorf("no container in record %q", rec.ID)
	}
	p.mu.Lock()
	p.attaches = append(p.attaches, rec.ID)
	p.mu.Unlock()
	return &Handle{
		ID: rec.ID, Provider: rec.Provider, Image: rec.Image,
		State: HandleRunning, Meta: rec.Meta, internal: rec.Container,
	}, nil
}

func (p addressableProvider) Forget(ctx context.Context, rec Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgets = append(p.forgets, rec.ID)
	return nil
}

// unreachableProvider addresses containers but cannot find this one: the
// container is gone, and the record describes something that no longer exists.
type unreachableProvider struct{ *baseProvider }

func (p unreachableProvider) Attach(rec Record) (*Handle, error) {
	return nil, fmt.Errorf("container %s is not reachable", rec.Container)
}

func (p unreachableProvider) Forget(ctx context.Context, rec Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgets = append(p.forgets, rec.ID)
	return nil
}

// opaqueProvider is a provider whose handles cannot be reached from another
// instance at all: no Attach, no Forget. The manager has to degrade rather than
// fail — the record is still dropped, because nothing here outlives the process.
type opaqueProvider struct{ *baseProvider }

// --- helpers -----------------------------------------------------------------

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// twoInstances builds the shape every test here needs: a shared registry and two
// managers, each with its own provider, standing in for two instances.
func twoInstances(t *testing.T, makeProvider func(name string) ShellProvider) (*memRegistry, *Manager, *baseProvider, *Manager, *baseProvider) {
	t.Helper()
	reg := newMemRegistry()
	ap, bp := makeProvider("a"), makeProvider("b")
	a := NewManager([]ShellProvider{ap}, testLog())
	b := NewManager([]ShellProvider{bp}, testLog())
	a.SetRegistry(reg, "instance-a")
	b.SetRegistry(reg, "instance-b")
	return reg, a, baseOf(ap), b, baseOf(bp)
}

// baseOf digs the call-recording base out of a provider wrapper.
func baseOf(p ShellProvider) *baseProvider {
	switch v := p.(type) {
	case addressableProvider:
		return v.baseProvider
	case unreachableProvider:
		return v.baseProvider
	case opaqueProvider:
		return v.baseProvider
	}
	return nil
}

// --- the properties ----------------------------------------------------------

const testOwner = "bot|chat-1"

// The point of the whole plane: a session that moves to another instance finds
// the shell it was using, because the handle is an address on a host rather
// than a structure in the process that created it.
func TestAnotherInstanceAdoptsTheSessionsShell(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()

	h, err := a.Spawn(ctx, testOwner, "docker", SpawnOpts{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reg.get(h.ID)
	if !ok {
		t.Fatal("a spawned handle must be recorded")
	}
	if rec.Owner != testOwner || rec.Instance != "instance-a" || rec.Container != "a-container-1" {
		t.Fatalf("record does not describe the handle: %+v", rec)
	}

	// b never saw the spawn: the session moved here.
	res, err := b.Exec(ctx, testOwner, h.ID, "ls")
	if err != nil {
		t.Fatalf("exec on the adopting instance: %v", err)
	}
	if res.Stdout != "ok: ls" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if got := len(bProv.spawned()); got != 0 {
		t.Fatalf("the adopting instance spawned %d handles; it must reach the existing container", got)
	}
	if got := bProv.attached(); len(got) != 1 || got[0] != h.ID {
		t.Fatalf("attach calls = %v", got)
	}
	// The adoption refreshes the record's clock: the session is live here now,
	// so the reclaim pass must not read this handle as abandoned.
	if after, _ := reg.get(h.ID); after.LiveAt < rec.LiveAt {
		t.Fatalf("adoption went backwards in time: %d → %d", rec.LiveAt, after.LiveAt)
	}
}

// Adoption is not a way around the ownership rule: a loop still cannot reach
// another session's shell, whichever instance created it.
func TestAdoptionStillEnforcesOwnership(t *testing.T) {
	_, a, _, b, _ := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})

	if _, err := b.Exec(ctx, "someone-else", h.ID, "ls"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("another owner reached the handle: err=%v", err)
	}
}

// "Ensure" is the lazy-spawn path every shell tool uses. After a failover it
// must return the container the session already has: spawning a second one
// would silently hand the loop an empty filesystem.
func TestEnsureAdoptsRatherThanSpawningASecondShell(t *testing.T) {
	_, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{Image: "alpine"})

	got, err := b.Ensure(ctx, testOwner, "docker", SpawnOpts{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != h.ID {
		t.Fatalf("ensure returned %q, want the session's existing handle %q", got.ID, h.ID)
	}
	if n := len(bProv.spawned()); n != 0 {
		t.Fatalf("ensure spawned %d containers", n)
	}
}

// A handle that cannot be reached from here is not insisted on: the record is
// released and a fresh shell spawned, because the alternative is a session that
// can never run a command again.
func TestEnsureReplacesAHandleItCannotReach(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		if name == "a" {
			return addressableProvider{newBaseProvider(name)}
		}
		return unreachableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})

	got, err := b.Ensure(ctx, testOwner, "docker", SpawnOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == h.ID {
		t.Fatal("ensure returned the handle it cannot reach")
	}
	if _, still := reg.get(h.ID); still {
		t.Fatal("the unreachable record should have been released")
	}
	if n := len(bProv.spawned()); n != 1 {
		t.Fatalf("spawned %d containers, want 1", n)
	}
	if rec, ok := reg.get(got.ID); !ok || rec.Owner != testOwner || rec.Instance != "instance-b" {
		t.Fatalf("the replacement is not recorded for this session: %+v ok=%v", rec, ok)
	}
}

// An SSH-shaped provider has nothing a record can rebuild, so a takeover
// degrades to a fresh handle rather than failing — and the record it leaves
// behind is dropped rather than re-attached forever.
func TestEnsureReplacesAHandleItsProviderCannotAttach(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		if name == "a" {
			return addressableProvider{newBaseProvider(name)}
		}
		return opaqueProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})

	got, err := b.Ensure(ctx, testOwner, "docker", SpawnOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == h.ID {
		t.Fatal("a handle nothing can attach to must not be handed back")
	}
	if _, still := reg.get(h.ID); still {
		t.Fatal("the unadoptable record should have been released")
	}
	if n := len(bProv.spawned()); n != 1 {
		t.Fatalf("spawned %d containers, want 1", n)
	}
}

// A session that ends takes its shells with it — including the ones created on
// an instance that is no longer with us. That is the fleet's cleanup path: the
// killed instance never runs its own reap.
func TestReapReachesHandlesThisInstanceNeverHeld(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})

	b.ReapSession(ctx, testOwner)
	if got := bProv.forgot(); len(got) != 1 || got[0] != h.ID {
		t.Fatalf("forget calls = %v, want the abandoned handle", got)
	}
	if _, still := reg.get(h.ID); still {
		t.Fatal("the record survived the reap")
	}
}

// The same, for an explicit destroy: a loop asking for its shell to go means
// the container goes, whoever created it.
func TestDestroyReleasesAContainerAnotherInstanceCreated(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})

	if err := b.Destroy(ctx, testOwner, h.ID); err != nil {
		t.Fatal(err)
	}
	if got := bProv.forgot(); len(got) != 1 {
		t.Fatalf("forget calls = %v", got)
	}
	if _, still := reg.get(h.ID); still {
		t.Fatal("the record survived the destroy")
	}
	// A second destroy is a destroy: the resource is already gone.
	if err := b.Destroy(ctx, testOwner, h.ID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}

// A release that fails must keep the record: the container is still out there,
// and the next reclaim pass is what will get it.
func TestAFailedReleaseKeepsTheRecord(t *testing.T) {
	reg, a, _, b, _ := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, _ := a.Spawn(ctx, testOwner, "docker", SpawnOpts{})
	reg.mu.Lock()
	reg.failDelete = true
	reg.mu.Unlock()

	if err := b.Destroy(ctx, testOwner, h.ID); err == nil {
		t.Fatal("a release whose record could not be deleted should report it")
	}
	if _, still := reg.get(h.ID); !still {
		t.Fatal("the record must survive so the next pass can retry")
	}
}

// The reclaim pass, which is the only thing that ever cleans up after an
// instance that was killed. It must be exact in both directions: a handle whose
// session is gone goes, and a handle whose session is alive stays — however old
// it is.
func TestSweepReclaimsOnlyAbandonedHandles(t *testing.T) {
	reg, _, aProv, b, _ := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	now := time.Now().Unix()

	// A session that is alive right now, with a container created long ago.
	reg.put(Record{ID: "h-live", Owner: "bot|alive", Provider: "b", State: HandleRunning,
		Container: "c-live", CreatedAt: now - 86400, LiveAt: now - int64(12*time.Hour)})
	// A session that is gone, and has been for longer than the grace window.
	reg.put(Record{ID: "h-abandoned", Owner: "bot|gone", Provider: "b", State: HandleRunning,
		Container: "c-abandoned", CreatedAt: now - 86400, LiveAt: now - int64(48*time.Hour)})
	// A session that is gone, but only just: a failover may still claim it.
	reg.put(Record{ID: "h-fresh", Owner: "bot|recent", Provider: "b", State: HandleRunning,
		Container: "c-fresh", CreatedAt: now - 60, LiveAt: now - 60})
	// A session whose liveness cannot be established.
	reg.put(Record{ID: "h-unknown", Owner: "bot|unknown", Provider: "b", State: HandleRunning,
		Container: "c-unknown", CreatedAt: now - 86400, LiveAt: now - 86400})

	live := func(ctx context.Context, owner string) (bool, error) {
		switch owner {
		case "bot|alive":
			return true, nil
		case "bot|unknown":
			return false, errStoreDown
		default:
			return false, nil
		}
	}
	n, err := b.Sweep(ctx, ReclaimGrace, live)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d handles, want 1", n)
	}

	if got := aProv.forgot(); len(got) != 0 {
		t.Fatalf("the wrong instance released containers: %v", got)
	}
	if _, still := reg.get("h-abandoned"); still {
		t.Fatal("an abandoned handle survived the sweep")
	}
	for _, id := range []string{"h-live", "h-fresh", "h-unknown"} {
		if _, ok := reg.get(id); !ok {
			t.Fatalf("%s was reclaimed; its session may still be alive", id)
		}
	}
	// The live handle's clock was moved forward, which is what keeps a
	// long-lived session's shell out of the next sweep too.
	liveRec, _ := reg.get("h-live")
	if liveRec.LiveAt < now-60 {
		t.Fatalf("a live handle's clock was not refreshed: %d (now %d)", liveRec.LiveAt, now)
	}
}

// A deployment with no registry — the single-instance default — behaves exactly
// as it did before there was one: the maps are the whole story.
func TestNoRegistryKeepsTheManagerLocal(t *testing.T) {
	prov := addressableProvider{newBaseProvider("docker")}
	mgr := NewManager([]ShellProvider{prov}, testLog())
	ctx := context.Background()

	h, err := mgr.Spawn(ctx, testOwner, "docker", SpawnOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := mgr.Current(ctx, testOwner); err != nil || cur == nil || cur.ID != h.ID {
		t.Fatalf("current = %v err=%v", cur, err)
	}
	if n, err := mgr.Sweep(ctx, ReclaimGrace, func(context.Context, string) (bool, error) { return false, nil }); n != 0 || err != nil {
		t.Fatalf("a sweep without a registry = %d, %v", n, err)
	}
	if _, err := mgr.Exec(ctx, testOwner, "h-never", "ls"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown handle error = %v", err)
	}
	if cur, err := mgr.Current(ctx, "bot|nobody"); err != nil || cur != nil {
		t.Fatalf("current for a session with no shell = %v, %v", cur, err)
	}
}

// A record describes a resource; it is never a credential. A password or key
// cannot reach the store, which is also why an SSH handle is not adoptable.
func TestARecordCarriesNoSecret(t *testing.T) {
	reg, a, aProv, _, _ := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	h, err := a.Spawn(ctx, testOwner, "docker", SpawnOpts{
		Host: "box.internal:22", User: "deploy", Password: "hunter2", KeyFile: "/keys/id_ed25519",
	})
	if err != nil {
		t.Fatal(err)
	}
	if aProv.lastOpts.Password != "hunter2" {
		t.Fatal("the provider did not receive the spawn options")
	}
	rec, ok := reg.get(h.ID)
	if !ok {
		t.Fatal("no record")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "/keys/id_ed25519"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("the record carries %q: %s", secret, b)
		}
	}
}

// An unreadable registry must not turn "ensure" into "spawn another container":
// a duplicate shell is worse than an error the loop can retry.
func TestEnsureFailsRatherThanDuplicatingWhenTheRegistryIsUnreadable(t *testing.T) {
	reg, a, _, b, bProv := twoInstances(t, func(name string) ShellProvider {
		return addressableProvider{newBaseProvider(name)}
	})
	ctx := context.Background()
	a.Spawn(ctx, testOwner, "docker", SpawnOpts{})
	reg.mu.Lock()
	reg.failList = true
	reg.mu.Unlock()

	if _, err := b.Ensure(ctx, testOwner, "docker", SpawnOpts{}); err == nil {
		t.Fatal("ensure should fail when it cannot read the registry")
	}
	if n := len(bProv.spawned()); n != 0 {
		t.Fatalf("ensure spawned %d containers on an unreadable registry", n)
	}
}
