package router

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	_ "agentflow/internal/builtins"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/luacode"
	"agentflow/internal/vm"
)

// memRows is a minimal in-memory runtime.Rows. The default route handler asks
// route state for a chat's mode on every message, so the benchmark needs a
// store: without one the handler reports the op as unavailable and crashes.
type memRows struct {
	mu   sync.Mutex
	rows map[string]runtime.Row
}

func newMemRows() *memRows { return &memRows{rows: map[string]runtime.Row{}} }

func (m *memRows) PutRow(_ context.Context, key, value string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = runtime.Row{Key: key, Value: value, UpdatedAt: time.Now(), ExpiresAt: expiresAt}
	return nil
}

func (m *memRows) GetRow(_ context.Context, key string) (runtime.Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key]
	return r, ok, nil
}

func (m *memRows) DeleteRow(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key)
	return nil
}

func (m *memRows) ListRows(_ context.Context, prefix string) ([]runtime.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]runtime.Row, 0, len(m.rows))
	for k, r := range m.rows {
		if strings.HasPrefix(k, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRows) SweepExpired(context.Context) (int, error) { return 0, nil }

// benchDriver hands each reply to the benchmark goroutine.
type benchDriver struct{ replies chan string }

func (d *benchDriver) Name() string { return "bench" }

func (d *benchDriver) Deliver(replyTo, text string, _ []media.Part) error {
	select {
	case d.replies <- text:
	default:
	}
	return nil
}

// BenchmarkRouterHop measures one message through the default route handler:
// Submit → router's Luau state → deliver op → session. The payload sizes are the
// two cases that decide D6's design: a small message and one large enough that
// crossing the Lua bridge twice is visible. The session echoes the reply so the
// benchmark waits for the hop to finish rather than for a queue write.
func BenchmarkRouterHop(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size/1024), func(b *testing.B) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			gw := gateway.NewRegistry(log)
			drv := &benchDriver{replies: make(chan string, 1)}
			gw.Register(drv)

			defs := map[string]*supervisor.AgentDef{
				"echo": {
					Info:     &session.Info{Name: "echo", HistoryBudget: 100},
					Handlers: map[string]session.OpHandler{},
					LoopSrc: fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) + `
function loop()
  while true do
    local m = session.inbox()
    session.send(m.text)
  end
end`,
				},
			}
			sup := supervisor.New(defs, gw, pool.New(2), nil, log)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sup.Start(ctx)

			routeSrc, _, err := luacode.Resolve("plugin:per_chat")
			if err != nil {
				b.Fatalf("resolve route: %v", err)
			}
			rtr := New(routeSrc, "", sup, log)
			rtr.SetStateStore(newMemRows())
			go rtr.Run(ctx)

			blob := strings.Repeat("x", size)
			var measured time.Duration
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := fmt.Sprintf("hop-%d", i)
				msg := session.Message{
					ID:      id,
					Type:    "user",
					From:    "user:bench",
					Text:    "hi",
					Channel: "bench",
					ReplyTo: id,
					Payload: map[string]any{"chat_id": "bench-chat", "blob": blob},
				}
				start := time.Now()
				rtr.Submit(Inbound{Channel: "bench", Agent: "echo", Message: msg})
				select {
				case <-drv.replies:
				case <-time.After(30 * time.Second):
					b.Fatal("router hop did not complete")
				}
				measured += time.Since(start)
			}
			b.StopTimer()
			b.ReportMetric(float64(measured.Nanoseconds())/float64(b.N), "ns/hop")
			cancel()
			sup.StopSession("echo|bench:bench-chat")
		})
	}
}
