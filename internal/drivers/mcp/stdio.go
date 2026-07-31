// Package mcp implements a minimal MCP (Model Context Protocol) stdio client.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"

	"agentflow/internal/core/tools"
)

// Client is a JSON-RPC client over a child process's stdio.
type Client struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	log    *slog.Logger

	mu      sync.Mutex
	pending map[int64]chan response
	alive   atomic.Bool
	nextID  atomic.Int64
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int64  `json:"id"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// NewClient starts an MCP server and performs the initialize handshake.
func NewClient(name, command string, args []string, log *slog.Logger) (*Client, error) {
	c := &Client{
		name:    name,
		cmd:     exec.Command(command, args...),
		log:     log.With("mcp_server", name),
		pending: map[int64]chan response{},
	}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	c.stdin = stdin
	c.stdout = bufio.NewReader(stdout)
	c.cmd.Stderr = &logWriter{c.log}

	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mcp server %s: %w", name, err)
	}
	c.alive.Store(true)
	go c.readLoop()

	if _, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentflow", "version": "0.2.0"},
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp initialize %s: %w", name, err)
	}
	return c, nil
}

func (c *Client) call(method string, params any) (json.RawMessage, error) {
	if !c.alive.Load() {
		return nil, fmt.Errorf("mcp server %s is not running", c.name)
	}
	id := c.nextID.Add(1)
	req := request{JSONRPC: "2.0", Method: method, Params: params, ID: id}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')

	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	c.mu.Lock()
	_, err = c.stdin.Write(b)
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp server %s closed", c.name)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) readLoop() {
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			c.alive.Store(false)
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var resp response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			c.log.Warn("bad mcp json", "line", line, "err", err)
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// ListTools discovers tools exposed by the server.
func (c *Client) ListTools(ctx context.Context) ([]tools.ToolSpec, error) {
	res, err := c.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		return nil, err
	}
	var out []tools.ToolSpec
	for _, t := range payload.Tools {
		var schema map[string]any
		_ = json.Unmarshal(t.InputSchema, &schema)
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
		res, err := c.call("tools/call", map[string]any{
			"name":      toolName,
			"arguments": args,
		})
		if err != nil {
			return tools.ResultUnavailable("mcp:"+c.name+"/"+toolName, err.Error()), nil
		}
		var payload struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(res, &payload); err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		var text string
		for _, c := range payload.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		return map[string]any{"ok": !payload.IsError, "text": text}, nil
	}
}

// Close terminates the server process.
func (c *Client) Close() error {
	c.alive.Store(false)
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}

type logWriter struct{ log *slog.Logger }

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Debug(string(p))
	return len(p), nil
}
