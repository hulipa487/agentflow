package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/lease"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/storedb"
)

// testSupervisor builds a one-agent supervisor whose loop source is loopSrc,
// with a gateway driver that hands replies to the test.
func testSupervisor(t *testing.T, loopSrc string) (*Supervisor, *benchDriver) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	drv := &benchDriver{replies: make(chan string, 64)}
	gw.Register(drv)
	defs := map[string]*AgentDef{
		"echo": {
			Info:     &session.Info{Name: "echo", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{},
			LoopSrc:  directive() + loopSrc,
		},
	}
	sup := New(defs, gw, pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)
	t.Cleanup(func() {
		cancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer waitCancel()
		sup.Wait(waitCtx)
	})
	return sup, drv
}

const echoLoop = `
function loop() {
  while (true) {
    const m = session.inbox();
    session.send(m.text);
  }
}`

func ephemeralMsg(t *testing.T, id, key, tenant string) session.Message {
	t.Helper()
	p := session.Provenance{Kind: "channel"}
	if tenant != "" {
		u := tenant
		p.UserUUID = &u
	}
	return session.Message{
		ID: id, Type: "user", From: "user:test", Text: "hi",
		Channel: "bench", ReplyTo: key, Provenance: &p,
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An ephemeral session serves exactly one turn and then leaves nothing behind:
// no session, no retired id, no tenant accounting. The retired map is the one
// that would otherwise grow by one entry per request forever.
func TestEphemeralSessionServesOneTurnAndGrowsNoMap(t *testing.T) {
	sup, drv := testSupervisor(t, echoLoop)
	const requests = 1000
	for i := 0; i < requests; i++ {
		key := fmt.Sprintf("req-%d", i)
		if err := sup.DeliverEphemeral("echo", key, ephemeralMsg(t, "m-"+key, key, "")); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		select {
		case <-drv.replies:
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d: no reply", i)
		}
	}
	waitFor(t, "ephemeral sessions to exit", 10*time.Second, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.sessions) == 0
	})
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if len(sup.retired) != 0 {
		t.Fatalf("retired grew to %d after ephemeral requests; it must stay empty", len(sup.retired))
	}
	if len(sup.ephemeralTenants) != 0 || len(sup.ephemeralTenant) != 0 {
		t.Fatalf("ephemeral tenant accounting leaked: %v %v", sup.ephemeralTenants, sup.ephemeralTenant)
	}
}

// countingHub fails the test if ephemeral delivery asks it to route anything,
// and counts calls otherwise.
type countingHub struct {
	mu       sync.Mutex
	routed   int
	released []string
}

func (h *countingHub) Route(context.Context, string, string, session.Message) error {
	h.mu.Lock()
	h.routed++
	h.mu.Unlock()
	return nil
}
func (h *countingHub) Claim(context.Context, string) (bool, error) { return true, nil }
func (h *countingHub) Live(context.Context, string) (bool, error)  { return false, nil }
func (h *countingHub) ReleaseSession(_ context.Context, sessKey string) error {
	h.mu.Lock()
	h.released = append(h.released, sessKey)
	h.mu.Unlock()
	return nil
}

func (h *countingHub) routedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.routed
}

func (h *countingHub) releasedKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.released...)
}

// DeliverEphemeral never takes a lease and never writes the inbox: the key is
// unique per request, so there is no owner to elect and no backlog to hand
// over. Assert the hub is not called and both tables stay empty.
func TestEphemeralSessionWritesNoLeaseOrInboxRow(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/cluster.db"
	lm, err := lease.Open(target, "test-owner", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()
	// Closed before the test ends: Windows cannot delete a file that is still
	// open, so a leaked handle fails the temporary directory's cleanup.
	closeInbox, err := inboxOpenForTest(target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInbox()

	sup, drv := testSupervisor(t, echoLoop)
	hub := &countingHub{}
	sup.SetHub(hub)
	if err := sup.DeliverEphemeral("echo", "req-1", ephemeralMsg(t, "m1", "req-1", "tenant-a")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drv.replies:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	if hub.routedCount() != 0 {
		t.Fatalf("ephemeral delivery called the hub %d times", hub.routedCount())
	}

	st, err := storedb.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, table := range []string{"leases", "session_inbox"} {
		var n int
		if err := st.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows after an ephemeral turn; want 0", table, n)
		}
	}
}

// The ephemeral bound is its own budget: it refuses with ErrTenantBusy, and
// the slot comes back when the turn ends. The loop holds its turn so the
// session is genuinely in flight when the second request arrives.
func TestEphemeralCapRefusesAndRecovers(t *testing.T) {
	sup, drv := testSupervisor(t, `
function loop() {
  const m = session.inbox();
  time.sleep(0.3);
  session.send(m.text);
}`)
	sup.SetEphemeralCap(1)

	if err := sup.DeliverEphemeral("echo", "req-1", ephemeralMsg(t, "m1", "req-1", "tenant-a")); err != nil {
		t.Fatal(err)
	}
	// Wait until the one slot is actually held.
	waitFor(t, "the first ephemeral session", time.Second, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return sup.ephemeralTenants["tenant-a"] == 1
	})
	err := sup.DeliverEphemeral("echo", "req-2", ephemeralMsg(t, "m2", "req-2", "tenant-a"))
	if !errors.Is(err, ErrTenantBusy) {
		t.Fatalf("second request = %v; want ErrTenantBusy", err)
	}
	select {
	case <-drv.replies:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}
	waitFor(t, "the slot to return", 5*time.Second, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return sup.ephemeralTenants["tenant-a"] == 0
	})
	if err := sup.DeliverEphemeral("echo", "req-3", ephemeralMsg(t, "m3", "req-3", "tenant-a")); err != nil {
		t.Fatalf("request after recovery: %v", err)
	}
	select {
	case <-drv.replies:
	case <-time.After(5 * time.Second):
		t.Fatal("third request did not finish")
	}
}

// A crashing ephemeral loop is not restarted: the session exists for one
// request, so a retry would serve it a second time and the waiting channel
// would never be told the turn failed.
func TestCrashingEphemeralLoopIsNotRestarted(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	sup, _ := testSupervisor(t, `
function loop() {
  const m = session.inbox();
  af.op({ type: "count" });
  throw new Error("boom");
}`)
	sup.defs["echo"].Handlers["count"] = func(context.Context, session.Op) (string, bool) {
		mu.Lock()
		runs++
		mu.Unlock()
		return "true", true
	}
	if err := sup.DeliverEphemeral("echo", "req-1", ephemeralMsg(t, "m1", "req-1", "")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the loop to run once", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return runs == 1
	})
	time.Sleep(1300 * time.Millisecond) // longer than the restart pause
	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 1 {
		t.Fatalf("crashed ephemeral loop ran %d times; want 1 (no restart)", got)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if len(sup.sessions) != 0 || len(sup.retired) != 0 {
		t.Fatalf("crashed ephemeral session left state: sessions=%d retired=%d", len(sup.sessions), len(sup.retired))
	}
}

// inboxOpenForTest creates the inbox schema on the same target without the
// supervisor ever touching it.
func inboxOpenForTest(target string) (func() error, error) {
	st, err := storedb.Open(target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.ExecDDL(ctx, `CREATE TABLE IF NOT EXISTS session_inbox (
		session_key TEXT NOT NULL,
		msg_id      TEXT NOT NULL,
		ts          BIGINT NOT NULL,
		body        TEXT NOT NULL,
		claimed_by  TEXT NOT NULL DEFAULT '',
		claimed_at  BIGINT NOT NULL DEFAULT 0,
		done        INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (session_key, msg_id)
	)`); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st.Close, nil
}
