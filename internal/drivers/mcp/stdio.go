// Package mcp implements an MCP (Model Context Protocol) stdio client on top of
// the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// The SDK owns the wire: JSON-RPC framing, the server/discover-then-initialize
// handshake, the initialized notification, request correlation and context
// cancellation. This package keeps only what is agentflow's own policy — the
// bounded handshake that must not hang the boot, the child's stderr routed into
// the runtime log, the "<server>/<tool>" namespacing the model sees, and the
// tool-result mapping (including the honest-degradation rule that a transport
// failure is reported as a result rather than an error).
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"time"

	"agentflow/internal/core/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handshakeTimeout bounds Connect. A server that starts but never answers would
// otherwise hang the boot, which is what the previous hand-rolled handshake
// guarded against and the reason this is not left to the caller.
const handshakeTimeout = 30 * time.Second

// clientName / clientVersion identify this runtime to the server.
const (
	clientName    = "agentflow"
	clientVersion = "0.2.0"
)

// Client is an MCP client session over a child process's stdio.
type Client struct {
	name    string
	session *mcpsdk.ClientSession
	log     *slog.Logger
	alive   atomic.Bool
}

// NewClient starts an MCP server and completes the handshake. It returns only
// once the session is usable, so a caller that gets an error knows the server
// never came up and can skip it.
func NewClient(name, command string, args []string, log *slog.Logger) (*Client, error) {
	l := log.With("mcp_server", name)
	cmd := exec.Command(command, args...)
	// The child's stderr goes to the runtime log at debug. The SDK does not
	// adopt a caller's exec.Cmd stderr, so it is set on the command before the
	// transport takes ownership of it.
	cmd.Stderr = &logWriter{l}

	c := &Client{name: name, log: l}

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp initialize %s: %w", name, err)
	}
	c.session = session
	c.alive.Store(true)
	return c, nil
}

// ListTools discovers tools exposed by the server.
func (c *Client) ListTools(ctx context.Context) ([]tools.ToolSpec, error) {
	if !c.alive.Load() {
		return nil, fmt.Errorf("mcp server %s is not running", c.name)
	}
	res, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []tools.ToolSpec
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		// The schema stays a raw map: the registry hands it to a provider as
		// JSON Schema, so it must not be reshaped into a typed struct here.
		schema, _ := t.InputSchema.(map[string]any)
		out = append(out, tools.ToolSpec{
			Name:        c.name + "/" + t.Name,
			Description: t.Description,
			Parameters:  schema,
			Autonomous:  false,
			Invoke:      c.makeInvoker(t.Name),
		})
	}
	return out, nil
}

func (c *Client) makeInvoker(toolName string) func(context.Context, map[string]any) (any, error) {
	return func(ctx context.Context, args map[string]any) (any, error) {
		if !c.alive.Load() {
			return tools.ResultUnavailable("mcp:"+c.name+"/"+toolName,
				fmt.Sprintf("mcp server %s is not running", c.name)), nil
		}
		res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      toolName,
			Arguments: args,
		})
		if err != nil {
			// A failure to reach the server is reported as a structured result,
			// not an error: the honest-degradation rule every tool follows.
			return tools.ResultUnavailable("mcp:"+c.name+"/"+toolName, err.Error()), nil
		}
		var text string
		for _, content := range res.Content {
			if tc, ok := content.(*mcpsdk.TextContent); ok {
				text += tc.Text
			}
		}
		return map[string]any{"ok": !res.IsError, "text": text}, nil
	}
}

// Close terminates the server process. The transport closes stdin, waits, then
// kills — so the child is reaped either way.
func (c *Client) Close() error {
	c.alive.Store(false)
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}

type logWriter struct{ log *slog.Logger }

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Debug(string(p))
	return len(p), nil
}
