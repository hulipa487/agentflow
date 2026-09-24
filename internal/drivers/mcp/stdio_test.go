package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// The package had no tests. It drives a child process over stdio, which is
// awkward to test and was the reason: everything here was written against a
// server nobody had.
//
// The child is this test binary. TestMain checks the environment and, when the
// variable is set, becomes a minimal MCP server instead of running tests — so
// NewClient, the handshake, ListTools and the tool invocation all run against a
// real process, on any platform, with no external dependency.
const helperEnv = "AF_MCP_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		serveHelper()
		return
	}
	os.Exit(m.Run())
}

// serveHelper answers initialize, tools/list and tools/call. The tool "hang"
// deliberately never answers, so a caller's context has something to time out
// against.
func serveHelper() {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			Method string          `json:"method"`
			ID     int64           `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return // stdin closed: the client is gone
		}
		reply := func(result any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}})
		case "tools/list":
			reply(map[string]any{"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "echo the text back",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
					},
				},
				map[string]any{"name": "hang", "description": "never answers"},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "hang" {
				continue // no reply, ever
			}
			reply(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "echo:" + fmt.Sprint(p.Arguments["text"])},
			}})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newHelperClient starts this test binary as an MCP server. t.Setenv is what
// makes the child a server: it is set after TestMain has already decided this
// process runs tests, and inherited by the child at spawn.
func newHelperClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv(helperEnv, "1")
	c, err := NewClient("helper", os.Args[0], nil, discardLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestNewClientCompletesTheHandshake: NewClient is only useful if the child
// answered initialize, and a server that starts but never replies must not hang
// the boot.
func TestNewClientCompletesTheHandshake(t *testing.T) {
	c := newHelperClient(t)
	if !c.alive.Load() {
		t.Fatal("the client is not alive after a successful handshake")
	}
}

func TestNewClientFailsOnABadCommand(t *testing.T) {
	if _, err := NewClient("nope", "definitely-not-a-real-binary-xyz", nil, discardLog()); err == nil {
		t.Fatal("starting a nonexistent command must fail")
	}
}

// TestListToolsNamespacesAndCarriesSchemas: ListTools is where a server's tools
// enter the registry, and the "<server>/<tool>" name is what the model sees.
func TestListToolsNamespacesAndCarriesSchemas(t *testing.T) {
	c := newHelperClient(t)

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]bool{}
	for _, spec := range got {
		byName[spec.Name] = true
	}
	if !byName["helper/echo"] || !byName["helper/hang"] {
		t.Fatalf("tools = %v; want helper/echo and helper/hang", byName)
	}

	// The schema has to survive: a tool registered with no parameters is a tool
	// the model cannot call correctly.
	for _, spec := range got {
		if spec.Name != "helper/echo" {
			continue
		}
		props, ok := spec.Parameters["properties"].(map[string]any)
		if !ok || props["text"] == nil {
			t.Fatalf("the echo tool lost its schema: %#v", spec.Parameters)
		}
		if spec.Description == "" {
			t.Error("the echo tool lost its description")
		}
	}
}

func TestInvokeReturnsTheChildsAnswer(t *testing.T) {
	c := newHelperClient(t)
	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var invoke func(context.Context, map[string]any) (any, error)
	for _, spec := range got {
		if spec.Name == "helper/echo" {
			invoke = spec.Invoke
		}
	}
	if invoke == nil {
		t.Fatal("no echo tool to invoke")
	}

	res, err := invoke(context.Background(), map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["ok"] != true || m["text"] != "echo:hi" {
		t.Fatalf("invoke returned %#v; want ok with text echo:hi", res)
	}
}

// TestInvokeHonoursTheContext: call used to take no context and select with no
// ctx case, so a child that stopped answering blocked the calling worker
// forever — and it held the response mutex across the write, which deadlocked
// the reader too. Both are covered here: the call must return on ctx, and the
// client must still work afterwards.
func TestInvokeHonoursTheContext(t *testing.T) {
	c := newHelperClient(t)
	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var hang func(context.Context, map[string]any) (any, error)
	for _, spec := range got {
		if spec.Name == "helper/hang" {
			hang = spec.Invoke
		}
	}
	if hang == nil {
		t.Fatal("no hang tool to invoke")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	// makeInvoker converts a failure into a structured unavailable result rather
	// than an error — the honest-degradation rule every tool follows — so what
	// this asserts is that the call *returns*, promptly, and says it could not
	// reach the server.
	res, err := hang(ctx, nil)
	if err != nil {
		t.Fatalf("invoke returned %v; a tool failure is reported as a result, not an error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the call took %v to give up; the context was not honoured", elapsed)
	}
	if m, ok := res.(map[string]any); !ok || m["ok"] == true {
		t.Fatalf("a timed-out call returned %#v; want a result reporting failure", res)
	}

	// The client must still work. The abandoned call held the response mutex
	// across the write, so before the fix this second call — to a tool that
	// answers immediately — would block behind a mutex the dead call never
	// released.
	var echo func(context.Context, map[string]any) (any, error)
	for _, spec := range got {
		if spec.Name == "helper/echo" {
			echo = spec.Invoke
		}
	}
	echoCtx, echoCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer echoCancel()
	done := make(chan error, 1)
	go func() {
		_, err := echo(echoCtx, map[string]any{"text": "still here"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the client did not recover after an abandoned call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a later call blocked behind the abandoned one; the client is wedged")
	}
}

func TestCallAfterCloseFails(t *testing.T) {
	t.Setenv(helperEnv, "1")
	c, err := NewClient("helper", os.Args[0], nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	// Close kills the child, so Wait reports the kill status; that is not the
	// subject here.
	_ = c.Close()
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("a call after Close must fail rather than block or panic")
	}
}
