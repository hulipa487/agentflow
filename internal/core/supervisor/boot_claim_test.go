package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

// fakeRouter stands in for the session hub: it answers Claim with a canned
// verdict and records what it was asked for.
type fakeRouter struct {
	claim     bool
	claimErr  error
	live      bool
	liveErr   error
	asked     []string
	routed    int
	lastAgent string
	lastKey   string
	lastMsg   session.Message
}

func (f *fakeRouter) Route(ctx context.Context, agent, key string, msg session.Message) error {
	f.routed++
	f.lastAgent, f.lastKey, f.lastMsg = agent, key, msg
	return nil
}

func (f *fakeRouter) Claim(ctx context.Context, sessKey string) (bool, error) {
	f.asked = append(f.asked, sessKey)
	return f.claim, f.claimErr
}

func (f *fakeRouter) Live(ctx context.Context, sessKey string) (bool, error) {
	return f.live, f.liveErr
}

func (f *fakeRouter) ReleaseSession(ctx context.Context, sessKey string) error { return nil }

// A daemon is one session, so a fleet boots it once. The instance that cannot
// claim the session must not spawn the actor at all — a second copy of a
// conversation is the failure the hub exists to prevent, and a boot push is the
// one delivery that does not go through the hub to be arbitrated.
func TestDaemonBootsOnlyOnTheInstanceThatClaimsIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		claim      bool
		claimErr   error
		wantBooted bool
	}{
		{name: "claimed here", claim: true, wantBooted: true},
		{name: "claimed elsewhere", claim: false, wantBooted: false},
		{name: "cannot arbitrate", claimErr: errors.New("store down"), wantBooted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			recorded := make(chan string, 2)
			defs := map[string]*AgentDef{
				"daemon": {
					Info:       &session.Info{Name: "daemon", HistoryBudget: 100},
					Handlers:   map[string]session.OpHandler{"probe.record": probeHandler(recorded)},
					Persistent: true,
					LoopSrc: directive() + `function loop()
  local msg = session.inbox()
  af.op({ type = "probe.record", text = msg.type })
end`,
				},
			}
			ctx := context.Background()
			sup := New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
			sup.Start(ctx)
			router := &fakeRouter{claim: tc.claim, claimErr: tc.claimErr}
			sup.SetHub(router)

			sup.BootPersistent(ctx)

			if len(router.asked) != 1 || router.asked[0] != "daemon|boot" {
				t.Fatalf("claim asked for %v, want the daemon's session key", router.asked)
			}
			if !tc.wantBooted {
				// The refusal is synchronous: no actor may exist afterwards.
				if rows, _, _ := sup.Snapshot(); len(rows) != 0 {
					t.Fatalf("a daemon this instance does not own must not spawn: %v", rows)
				}
				return
			}
			if got := awaitProbe(t, recorded, "daemon boot turn"); got != "boot" {
				t.Fatalf("daemon first message type = %q, want boot", got)
			}
			if router.routed != 0 {
				t.Fatalf("the boot turn must be delivered locally once the session is ours, not routed: %d", router.routed)
			}
		})
	}
}

// TestDaemonFailoverBootsAnUnownedDaemon: BootPersistent runs at startup only,
// so a daemon whose instance was killed stays down until some instance checks
// again. The failover pass asks the lease table whether anyone owns the daemon
// and claims it when nobody does; a live owner or an unreadable store leaves it
// alone.
func TestDaemonFailoverBootsAnUnownedDaemon(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       bool
		liveErr    error
		claim      bool
		wantBooted bool
		wantAsked  bool
	}{
		{name: "owner alive", live: true, claim: false, wantBooted: false, wantAsked: false},
		{name: "owner gone, claim won", live: false, claim: true, wantBooted: true, wantAsked: true},
		{name: "owner gone, lost the race", live: false, claim: false, wantBooted: false, wantAsked: true},
		{name: "lease store unreadable", live: false, liveErr: errors.New("store down"), claim: true, wantBooted: false, wantAsked: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			recorded := make(chan string, 2)
			defs := map[string]*AgentDef{
				"daemon": {
					Info:       &session.Info{Name: "daemon", HistoryBudget: 100},
					Handlers:   map[string]session.OpHandler{"probe.record": probeHandler(recorded)},
					Persistent: true,
					LoopSrc: directive() + `function loop()
  local msg = session.inbox()
  af.op({ type = "probe.record", text = msg.type })
end`,
				},
			}
			ctx := context.Background()
			sup := New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
			sup.Start(ctx)
			router := &fakeRouter{claim: tc.claim, live: tc.live, liveErr: tc.liveErr}
			sup.SetHub(router)

			sup.FailoverPersistent(ctx)

			if got := len(router.asked) > 0; got != tc.wantAsked {
				t.Fatalf("claim asked=%v, want %v (asked %v)", got, tc.wantAsked, router.asked)
			}
			if tc.wantAsked && router.asked[0] != "daemon|boot" {
				t.Fatalf("claim asked for %q, want daemon|boot", router.asked[0])
			}
			if !tc.wantBooted {
				if rows, _, _ := sup.Snapshot(); len(rows) != 0 {
					t.Fatalf("daemon must not spawn without a claim: %v", rows)
				}
				return
			}
			if got := awaitProbe(t, recorded, "failed-over daemon boot turn"); got != "boot" {
				t.Fatalf("daemon first message type = %q, want boot", got)
			}
			if router.routed != 0 {
				t.Fatalf("the boot turn must be delivered locally, not routed: %d", router.routed)
			}
		})
	}
}
