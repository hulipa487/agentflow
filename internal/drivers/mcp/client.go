// Package mcp implements an MCP (Model Context Protocol) client on top of the
// official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// Two transports, chosen by how the server is configured: a spawned child
// process over stdio (NewClient, for a `command:` server), or a Streamable
// HTTP endpoint (NewHTTPClient, for a `url:` server). The HTTP transport is
// what lets an integration live outside this repository entirely — a
// separately deployed, separately versioned service that the runtime dials
// rather than spawns.
//
// The SDK owns the wire: JSON-RPC framing, the server/discover-then-initialize
// handshake, the initialized notification, request correlation and context
// cancellation. This package keeps only what is agentflow's own policy — the
// bounded handshake that must not hang the boot, the child's stderr routed into
// the runtime log, the bearer credential for an HTTP endpoint, the
// "<server>/<tool>" namespacing the model sees, and the tool-result mapping
// (including the honest-degradation rule that a transport failure is reported
// as a result rather than an error).
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"sync/atomic"
	"time"

	"agentflow/internal/core/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handshakeTimeout bounds Connect, on both transports. A server that starts (or
// answers) but never completes the handshake would otherwise hang the boot,
// which is what the previous hand-rolled handshake guarded against and the
// reason this is not left to the caller.
const handshakeTimeout = 30 * time.Second

// clientName / clientVersion identify this runtime to the server.
const (
	clientName    = "agentflow"
	clientVersion = "0.2.0"
)

// Client is an MCP client session over either transport.
type Client struct {
	name    string
	session *mcpsdk.ClientSession
	log     *slog.Logger
	alive   atomic.Bool
}

// NewClient starts an MCP server as a child process and completes the handshake
// over stdio. It returns only once the session is usable, so a caller that gets
// an error knows the server never came up and can skip it.
func NewClient(name, command string, args []string, log *slog.Logger) (*Client, error) {
	l := log.With("mcp_server", name)
	cmd := exec.Command(command, args...)
	// The child's stderr goes to the runtime log at debug. The SDK does not
	// adopt a caller's exec.Cmd stderr, so it is set on the command before the
	// transport takes ownership of it.
	cmd.Stderr = &logWriter{l}
	return connect(l, name, &mcpsdk.CommandTransport{Command: cmd})
}

// NewHTTPClient dials an MCP server over Streamable HTTP and completes the
// handshake. endpoint is a single URL serving both the JSON-RPC POST and the
// optional event stream. token, when set, is sent as a bearer credential on
// every request.
func NewHTTPClient(name, endpoint, token string, log *slog.Logger) (*Client, error) {
	l := log.With("mcp_server", name)
	httpClient := &http.Client{}
	if token != "" {
		// StreamableClientTransport has no header field, so the credential
		// rides on the transport. Requests are cloned rather than mutated:
		// Go's client may retry, and a mutated shared request is a data race.
		httpClient.Transport = &bearerTransport{token: token, base: http.DefaultTransport}
	}
	return connect(l, name, &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	})
}

// connect performs the handshake over whichever transport was built, and
// returns a live session. It is the only place the session is created, so both
// transports share the bounded handshake and the alive flag.
//
// The timeout bounds only the handshake: a server that accepts the connection
// but never answers initialize must not hang the boot. Cancelling afterwards is
// safe on both transports — StreamableClientTransport detaches its connection
// context from this one (see its Connect), and a stdio child is independent of
// it.
func connect(l *slog.Logger, name string, transport mcpsdk.Transport) (*Client, error) {
	c := &Client{name: name, log: l}

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect %s: %w", name, err)
	}
	c.session = session
	c.alive.Store(true)
	return c, nil
}

// bearerTransport adds an Authorization header to every request, for the HTTP
// transport's per-server token.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
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

// Close ends the session. For a stdio child the transport closes stdin, waits,
// then kills — so the process is reaped either way; for HTTP it closes the
// connection, which cancels the background event stream.
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
