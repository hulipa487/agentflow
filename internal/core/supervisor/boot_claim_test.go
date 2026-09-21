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
	claim    bool
	claimErr error
	asked    []string
	routed   int
}

func (f *fakeRouter) Route(ctx context.Context, agent, key string, msg session.Message) error {
	f.routed++
	return nil
}

func (f *fakeRouter) Claim(ctx context.Context, sessKey string) (bool, error) {
	f.asked = append(f.asked, sessKey)
	return f.claim, f.claimErr
}

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
					LoopSrc: `function loop()
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
