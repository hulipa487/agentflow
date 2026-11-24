package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

// benchDriver is a gateway.Driver that hands every reply to the benchmark
// goroutine. Replies are keyed by replyTo so the router benchmark and the spawn
// benchmark can share the type without sharing a channel.
type benchDriver struct {
	replies chan string
}

func (d *benchDriver) Name() string { return "bench" }

func (d *benchDriver) Deliver(replyTo, text string, _ []media.Part) error {
	select {
	case d.replies <- text:
	default:
	}
	return nil
}

// BenchmarkSessionSpawn measures the cold path that decides phase 4.3: from the
// first DeliverLocal to the echo loop's reply, including the Luau state, the
// prelude, the support chunks and the loop chunk. Each iteration uses a fresh
// session key, so every spawn is cold. Cleanup — stopping the actor and waiting
// for it to leave the map — is excluded from the measurement but still runs, or
// the benchmark would hold b.N Luau states at once.
func BenchmarkSessionSpawn(b *testing.B) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	drv := &benchDriver{replies: make(chan string, 1)}
	gw.Register(drv)

	defs := map[string]*AgentDef{
		"echo": {
			Info:     &session.Info{Name: "echo", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{},
			LoopSrc: directive() + `
function loop() {
  while (true) {
    const m = session.inbox();
    session.send(m.text);
  }
}`,
		},
	}
	sup := New(defs, gw, pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	var measured time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("spawn-%d", i)
		start := time.Now()
		if err := sup.DeliverLocal("echo", key, session.Message{
			ID:      fmt.Sprintf("m-%d", i),
			Type:    "user",
			From:    "user:bench",
			Text:    "hi",
			Channel: "bench",
			ReplyTo: key,
		}); err != nil {
			b.Fatalf("deliver: %v", err)
		}
		select {
		case <-drv.replies:
		case <-time.After(30 * time.Second):
			b.Fatal("echo loop did not reply")
		}
		measured += time.Since(start)

		b.StopTimer()
		sup.StopSession("echo|" + key)
		for {
			sup.mu.Lock()
			_, live := sup.sessions["echo|"+key]
			sup.mu.Unlock()
			if !live {
				break
			}
			time.Sleep(100 * time.Microsecond)
		}
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(measured.Nanoseconds())/float64(b.N), "ns/spawn")
}
