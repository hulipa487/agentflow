package chatapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/builtins"
	"agentflow/internal/config"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/router"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/drivers/httpd"
	"agentflow/internal/drivers/llm"
)

// --- fixtures ---------------------------------------------------------------

type fixture struct {
	name string
	Req  struct {
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	} `json:"request"`
	Resp struct {
		Status      int                 `json:"status"`
		ContentType string              `json:"content_type"`
		Headers     map[string][]string `json:"headers"`
		Body        string              `json:"body"`
	} `json:"response"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "cmd", "agentflow", "testdata", "chatapi")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var out []fixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var f fixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		f.name = strings.TrimSuffix(strings.TrimPrefix(e.Name(), "handwritten-"), ".json")
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	if len(out) == 0 {
		t.Fatal("no fixtures")
	}
	return out
}

func (f fixture) wantsStream() bool {
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal([]byte(f.Req.Body), &body)
	return body.Stream || strings.Contains(f.Resp.ContentType, "event-stream")
}

func (f fixture) requestedModel(t *testing.T) string {
	t.Helper()
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(f.Req.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body.Model
}

// --- canned upstream reply --------------------------------------------------

type canned struct {
	text      string
	toolCalls []gateway.ToolCall
	finish    string
	usage     *gateway.Usage
	reasoning string
}

func cannedFrom(f fixture) canned {
	if strings.Contains(f.Resp.Body, "data: ") {
		return cannedFromSSE(f.Resp.Body)
	}
	var obj struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(f.Resp.Body), &obj); err != nil {
		panic(err)
	}
	c := canned{finish: "stop"}
	if len(obj.Choices) > 0 {
		if obj.Choices[0].Message.Content != nil {
			c.text = *obj.Choices[0].Message.Content
		}
		for _, tc := range obj.Choices[0].Message.ToolCalls {
			c.toolCalls = append(c.toolCalls, gateway.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if obj.Choices[0].FinishReason != "" {
			c.finish = obj.Choices[0].FinishReason
		}
	}
	if obj.Usage != nil {
		u := &gateway.Usage{Input: obj.Usage.PromptTokens, Output: obj.Usage.CompletionTokens}
		if obj.Usage.PromptTokensDetails != nil {
			u.Cached = obj.Usage.PromptTokensDetails.CachedTokens
		}
		if obj.Usage.CompletionTokensDetails != nil {
			u.Reasoning = obj.Usage.CompletionTokensDetails.ReasoningTokens
		}
		c.usage = u
	}
	return c
}

func cannedFromSSE(body string) canned {
	c := canned{finish: "stop"}
	type acc struct{ id, name, args string }
	accs := map[int]*acc{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			panic(err)
		}
		if chunk.Usage != nil {
			c.usage = &gateway.Usage{Input: chunk.Usage.PromptTokens, Output: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		c.text += ch.Delta.Content
		for _, tc := range ch.Delta.ToolCalls {
			a := accs[tc.Index]
			if a == nil {
				a = &acc{}
				accs[tc.Index] = a
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args += tc.Function.Arguments
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			c.finish = *ch.FinishReason
		}
	}
	idxs := make([]int, 0, len(accs))
	for i := range accs {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		a := accs[i]
		c.toolCalls = append(c.toolCalls, gateway.ToolCall{ID: a.id, Name: a.name, Arguments: a.args})
	}
	return c
}

// --- stub upstream model ----------------------------------------------------

type stubUpstream struct {
	mu       sync.Mutex
	bodies   []string
	calls    int
	canned   canned
	delay    time.Duration
	failCode int
	echo     bool
}

func (u *stubUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, string(body))
	u.calls++
	c := u.canned
	fail, delay, echo := u.failCode, u.delay, u.echo
	u.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if fail != 0 {
		http.Error(w, "upstream failed", fail)
		return
	}
	if echo {
		c.text = lastUserOf(body)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	base := map[string]any{"id": "chatcmpl-up", "object": "chat.completion.chunk", "created": 1, "model": "stub"}
	withChoices := func(delta map[string]any, finish any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		m["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		return m
	}
	write(withChoices(map[string]any{"role": "assistant", "content": ""}, nil))
	if c.reasoning != "" {
		write(withChoices(map[string]any{"reasoning_content": c.reasoning}, nil))
	}
	for i := 0; i < len(c.text); i += 24 {
		end := i + 24
		if end > len(c.text) {
			end = len(c.text)
		}
		write(withChoices(map[string]any{"content": c.text[i:end]}, nil))
	}
	if len(c.toolCalls) > 0 {
		tcs := make([]any, len(c.toolCalls))
		for i, tc := range c.toolCalls {
			tcs[i] = map[string]any{
				"index": i, "id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			}
		}
		write(withChoices(map[string]any{"tool_calls": tcs}, nil))
	}
	finish := c.finish
	if finish == "" {
		finish = "stop"
	}
	write(withChoices(map[string]any{}, finish))
	if c.usage != nil {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		m["choices"] = []any{}
		m["usage"] = map[string]any{
			"prompt_tokens": c.usage.Input, "completion_tokens": c.usage.Output,
			"total_tokens":              c.usage.Input + c.usage.Output,
			"prompt_tokens_details":     map[string]any{"cached_tokens": c.usage.Cached},
			"completion_tokens_details": map[string]any{"reasoning_tokens": c.usage.Reasoning},
		}
		write(m)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (u *stubUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return ""
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *stubUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func lastUserOf(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(req.Messages[i].Content, &s) == nil {
			return s
		}
	}
	return ""
}

// --- the stack --------------------------------------------------------------

type stackOpts struct {
	canned       canned
	auth         string
	keys         []string
	tokenUser    func(string) (string, bool)
	timeout      time.Duration
	ephemeralCap int
	delay        time.Duration
	failCode     int
	echo         bool
	notReady     bool
	mediaAllow   []string
	safety       *safety.Dispatcher
	stopHook     func(string)
	// loopSrc and routeSrc replace the built-in pass-through loop and per_chat
	// route; exposeReasoning turns the channel setting on.
	loopSrc         string
	routeSrc        string
	exposeReasoning bool
}

type stack struct {
	t        *testing.T
	driver   *Driver
	sup      *supervisor.Supervisor
	upstream *stubUpstream
	reqs     *chatreq.Registry
	url      string
	stopped  atomic.Int32
	stopKeys []string
	mu       sync.Mutex
}

func newStack(t *testing.T, opt stackOpts) *stack {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	up := &stubUpstream{canned: opt.canned, delay: opt.delay, failCode: opt.failCode, echo: opt.echo}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	llmMgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "stub", BaseURL: upSrv.URL},
	}, log)
	reqs := chatreq.NewRegistry()
	loopSrc, _, err := builtins.Resolve("plugin:openai_passthrough")
	if err != nil {
		t.Fatalf("resolve passthrough: %v", err)
	}
	if opt.loopSrc != "" {
		loopSrc = opt.loopSrc
	}
	handlers := caps.MeteredLLMHandlers(llmMgr, nil, caps.Metering{Agent: "coder", Requests: reqs, Log: log})
	defs := map[string]*supervisor.AgentDef{
		"coder": {
			Info:     &session.Info{Name: "coder", Model: "default", HistoryBudget: 100},
			Handlers: handlers,
			LoopSrc:  loopSrc,
			Safety:   opt.safety,
		},
	}
	gw := gateway.NewRegistry(log)
	sup := supervisor.New(defs, gw, pool.New(4), nil, log)
	sup.SetEphemeralCap(opt.ephemeralCap)
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)
	t.Cleanup(cancel)

	routeSrc, _, err := builtins.Resolve("plugin:per_chat")
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if opt.routeSrc != "" {
		routeSrc = opt.routeSrc
	}
	store, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rtr := router.New(routeSrc, "", sup, log)
	rtr.SetStateStore(store)
	go rtr.Run(ctx)

	httpSrv := httpd.New("", log)
	st := &stack{t: t, sup: sup, upstream: up, reqs: reqs}
	stopHook := opt.stopHook
	driver := New("api", Options{
		Path:            "/v1/",
		Agents:          []string{"coder"},
		Auth:            opt.auth,
		Keys:            opt.keys,
		TokenUser:       opt.tokenUser,
		Timeout:         opt.timeout,
		MaxRequestBytes: 1 << 20,
		ExposeReasoning: opt.exposeReasoning,
		Ready:           func() bool { return !opt.notReady },
		StopSession: func(key string) {
			st.stopped.Add(1)
			st.mu.Lock()
			st.stopKeys = append(st.stopKeys, key)
			st.mu.Unlock()
			if stopHook != nil {
				stopHook(key)
			}
			sup.StopSession(key)
		},
		Store:    nil,
		Policy:   media.Policy{Allow: opt.mediaAllow},
		Requests: reqs,
	}, rtr, httpSrv, log)
	gw.Register(driver)
	st.driver = driver
	ts := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(ts.Close)
	st.url = ts.URL
	return st
}

func (s *stack) do(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// --- fixture replay ---------------------------------------------------------

func TestFixturesReplay(t *testing.T) {
	for _, f := range loadFixtures(t) {
		f := f
		t.Run(f.name, func(t *testing.T) {
			st := newStack(t, stackOpts{
				canned:     cannedFrom(f),
				auth:       "none",
				mediaAllow: []string{"image/*"},
			})
			resp := st.do(t, f.Req.Method, f.Req.Path, f.Req.Body, nil)
			status, body := readAll(t, resp)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			if f.wantsStream() {
				compareStream(t, f, body)
			} else {
				compareCompletion(t, f, body)
			}
			// Per-fixture wire assertions on what reached the model.
			up := st.upstream.lastBody()
			switch f.name {
			case "image-part":
				if !strings.Contains(up, `"image_url"`) || !strings.Contains(up, "data:image/png;base64,") {
					t.Fatalf("image did not reach the provider as a data URL: %s", up)
				}
			case "tool-followup":
				if !strings.Contains(up, `"role":"tool"`) || !strings.Contains(up, `"tool_call_id":"call_hw_1"`) {
					t.Fatalf("tool result did not reach the model with its id: %s", up)
				}
			case "tools":
				if !strings.Contains(up, `"get_weather"`) || !strings.Contains(up, `"tool_choice":"auto"`) {
					t.Fatalf("tools were not forwarded: %s", up)
				}
			case "plain-chat":
				if !strings.Contains(up, "You are a careful coding assistant.") {
					t.Fatalf("system message was not forwarded: %s", up)
				}
			}
		})
	}
}

func compareCompletion(t *testing.T, f fixture, got string) {
	t.Helper()
	var want, out map[string]any
	if err := json.Unmarshal([]byte(f.Resp.Body), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, got)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v", out["object"])
	}
	if out["model"] != f.requestedModel(t) {
		t.Fatalf("model = %v; want the agent name %q", out["model"], f.requestedModel(t))
	}
	wc := want["choices"].([]any)[0].(map[string]any)
	oc := out["choices"].([]any)[0].(map[string]any)
	wm := wc["message"].(map[string]any)
	om := oc["message"].(map[string]any)
	if contentString(wm["content"]) != contentString(om["content"]) {
		t.Fatalf("content = %q; want %q", om["content"], wm["content"])
	}
	if wc["finish_reason"] != oc["finish_reason"] {
		t.Fatalf("finish_reason = %v; want %v", oc["finish_reason"], wc["finish_reason"])
	}
	compareToolCalls(t, wm["tool_calls"], om["tool_calls"])
	if wu, ok := want["usage"].(map[string]any); ok {
		ou, ok := out["usage"].(map[string]any)
		if !ok {
			t.Fatalf("usage missing; want %v", wu)
		}
		for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if num(wu[k]) != num(ou[k]) {
				t.Fatalf("usage.%s = %v; want %v", k, ou[k], wu[k])
			}
		}
	}
}

func compareStream(t *testing.T, f fixture, got string) {
	t.Helper()
	want := cannedFromSSE(f.Resp.Body)
	_, chunks, done := parseSSE(t, got)
	if !done {
		t.Fatal("stream did not end with [DONE]")
	}
	var text strings.Builder
	var toolCalls []map[string]any
	finish := ""
	usage := false
	for _, c := range chunks {
		if _, ok := c["usage"]; ok {
			usage = true
		}
		choices, _ := c["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch := choices[0].(map[string]any)
		if d, ok := ch["delta"].(map[string]any); ok {
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if tcs, ok := d["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					toolCalls = append(toolCalls, tc.(map[string]any))
				}
			}
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
	}
	if text.String() != want.text {
		t.Fatalf("stream text = %q; want %q", text.String(), want.text)
	}
	if finish != want.finish {
		t.Fatalf("finish_reason = %q; want %q", finish, want.finish)
	}
	if len(want.toolCalls) > 0 && len(toolCalls) != len(want.toolCalls) {
		t.Fatalf("tool calls = %d; want %d", len(toolCalls), len(want.toolCalls))
	}
	wantUsage := strings.Contains(f.Req.Body, `"include_usage":true`)
	if usage != wantUsage {
		t.Fatalf("usage chunk present = %v; want %v", usage, wantUsage)
	}
}

func parseSSE(t *testing.T, body string) ([]map[string]any, []map[string]any, bool) {
	t.Helper()
	var chunks []map[string]any
	done := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			continue
		}
		if payload == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("bad SSE payload %q: %v", payload, err)
		}
		chunks = append(chunks, v)
	}
	return nil, chunks, done
}

func contentString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func compareToolCalls(t *testing.T, want, got any) {
	t.Helper()
	wa, _ := want.([]any)
	ga, _ := got.([]any)
	if len(wa) != len(ga) {
		t.Fatalf("tool calls = %d; want %d (%v)", len(ga), len(wa), got)
	}
	for i := range wa {
		w := wa[i].(map[string]any)["function"].(map[string]any)
		g := ga[i].(map[string]any)["function"].(map[string]any)
		if w["name"] != g["name"] {
			t.Fatalf("tool call %d name = %v; want %v", i, g["name"], w["name"])
		}
		var wargs, gargs map[string]any
		_ = json.Unmarshal([]byte(contentString(w["arguments"])), &wargs)
		_ = json.Unmarshal([]byte(contentString(g["arguments"])), &gargs)
		if fmt.Sprint(wargs) != fmt.Sprint(gargs) {
			t.Fatalf("tool call %d args = %v; want %v", i, gargs, wargs)
		}
	}
}

// --- auth, errors, models ---------------------------------------------------

func minimalBody(prompt string) string {
	b, _ := json.Marshal(map[string]any{
		"model":    "coder",
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	return string(b)
}

func TestAuth(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		st := newStack(t, stackOpts{canned: canned{text: "ok"}, auth: "key", keys: []string{"secret-key"}})
		status, _ := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
		if status != http.StatusUnauthorized {
			t.Fatalf("missing key: status %d; want 401", status)
		}
		status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), map[string]string{"Authorization": "Bearer wrong"}))
		if status != 401 || !strings.Contains(body, "invalid_api_key") {
			t.Fatalf("wrong key: %d %s", status, body)
		}
		status, body = readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), map[string]string{"Authorization": "Bearer secret-key"}))
		if status != 200 {
			t.Fatalf("right key: %d %s", status, body)
		}
	})

	t.Run("user_token", func(t *testing.T) {
		st := newStack(t, stackOpts{
			canned: canned{text: "ok"}, auth: "user_token",
			tokenUser: func(tok string) (string, bool) {
				if tok == "good" {
					return "user-1", true
				}
				return "", false
			},
		})
		status, _ := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
		if status != 401 {
			t.Fatalf("missing token: %d; want 401", status)
		}
		status, _ = readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), map[string]string{"Authorization": "Bearer revoked"}))
		if status != 401 {
			t.Fatalf("revoked token: %d; want 401", status)
		}
		status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), map[string]string{"Authorization": "Bearer good"}))
		if status != 200 {
			t.Fatalf("good token: %d %s", status, body)
		}
	})
}

func TestRequestErrors(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "ok"}, auth: "none"})
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
		code   string
	}{
		{"bad json", "POST", "/v1/chat/completions", "{", 400, "invalid_request_error"},
		{"unknown model", "POST", "/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, 404, "model_not_found"},
		{"n > 1", "POST", "/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":"hi"}],"n":2}`, 400, "invalid_request_error"},
		{"no messages", "POST", "/v1/chat/completions", `{"model":"coder","messages":[]}`, 400, "invalid_request_error"},
		{"unknown part", "POST", "/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":[{"type":"nope"}]}]}`, 400, "invalid_request_error"},
		{"unknown endpoint", "GET", "/v1/nope", "", 404, "unknown_endpoint"},
		{"wrong method", "GET", "/v1/chat/completions", "", 405, "method_not_allowed"},
		{"models wrong method", "POST", "/v1/models", "", 405, "method_not_allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := readAll(t, st.do(t, tc.method, tc.path, tc.body, nil))
			if status != tc.want {
				t.Fatalf("status %d; want %d (%s)", status, tc.want, body)
			}
			if !strings.Contains(body, tc.code) {
				t.Fatalf("code %q missing from %s", tc.code, body)
			}
		})
	}

	t.Run("too large", func(t *testing.T) {
		st := newStack(t, stackOpts{canned: canned{text: "ok"}, auth: "none"})
		big := strings.Repeat("x", (1<<20)+10)
		body := `{"model":"coder","messages":[{"role":"user","content":"` + big + `"}]}`
		status, resp := readAll(t, st.do(t, "POST", "/v1/chat/completions", body, nil))
		if status != http.StatusRequestEntityTooLarge || !strings.Contains(resp, "request_too_large") {
			t.Fatalf("status %d: %s", status, resp)
		}
	})
}

func TestModels(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{}, auth: "none"})
	status, body := readAll(t, st.do(t, "GET", "/v1/models", "", nil))
	if status != 200 {
		t.Fatalf("models: %d %s", status, body)
	}
	if !strings.Contains(body, `"id":"coder"`) || !strings.Contains(body, `"object":"list"`) {
		t.Fatalf("models body: %s", body)
	}
	status, body = readAll(t, st.do(t, "GET", "/v1/models/coder", "", nil))
	if status != 200 || !strings.Contains(body, `"id":"coder"`) {
		t.Fatalf("model: %d %s", status, body)
	}
	status, body = readAll(t, st.do(t, "GET", "/v1/models/nope", "", nil))
	if status != 404 || !strings.Contains(body, "model_not_found") {
		t.Fatalf("unknown model: %d %s", status, body)
	}
}

// --- lifecycle and errors ---------------------------------------------------

func TestEphemeralCapMapsTo429(t *testing.T) {
	st := newStack(t, stackOpts{
		canned: canned{text: "slow"}, auth: "user_token",
		tokenUser: func(string) (string, bool) { return "user-1", true },
		delay:     500 * time.Millisecond, ephemeralCap: 1,
	})
	headers := map[string]string{"Authorization": "Bearer good"}
	done := make(chan int, 1)
	go func() {
		status, _ := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("first"), headers))
		done <- status
	}()
	// Let the first session spawn and take the one slot.
	time.Sleep(100 * time.Millisecond)
	resp := st.do(t, "POST", "/v1/chat/completions", minimalBody("second"), headers)
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("a 429 must carry Retry-After")
	}
	status, body := readAll(t, resp)
	if status != http.StatusTooManyRequests || !strings.Contains(body, "rate_limit_exceeded") {
		t.Fatalf("second request: %d %s; want 429 rate_limit_exceeded", status, body)
	}
	if status := <-done; status != 200 {
		t.Fatalf("first request: %d", status)
	}
}

func TestUpstreamErrorMapsTo502(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "x"}, auth: "none", failCode: 500, timeout: 5 * time.Second})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != http.StatusBadGateway || !strings.Contains(body, "upstream_error") {
		t.Fatalf("status %d: %s; want 502 upstream_error", status, body)
	}
	if strings.Contains(body, "upstream failed") {
		t.Fatalf("the upstream body leaked: %s", body)
	}
}

func TestTimeoutMapsTo504(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "slow"}, auth: "none", delay: time.Second, timeout: 100 * time.Millisecond})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != http.StatusGatewayTimeout || !strings.Contains(body, "timeout") {
		t.Fatalf("status %d: %s; want 504 timeout", status, body)
	}
}

type dropIngress struct{}

func (dropIngress) Name() string { return "drop" }
func (dropIngress) Ingress(context.Context, safety.IngressInput) safety.IngressResult {
	return safety.IngressResult{Drop: true, Skip: true, Reason: "test-drop"}
}
func (dropIngress) Egress(_ context.Context, in safety.EgressInput) safety.EgressResult {
	return safety.EgressResult{Text: in.Text}
}

func TestSafetyBlockedMapsTo400(t *testing.T) {
	disp := safety.New(&safety.Profile{Name: "test", Filters: []safety.Filter{dropIngress{}}})
	st := newStack(t, stackOpts{canned: canned{text: "x"}, auth: "none", safety: disp})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("blocked"), nil))
	if status != http.StatusBadRequest || !strings.Contains(body, "content_filter") {
		t.Fatalf("status %d: %s; want 400 content_filter", status, body)
	}
}

func TestClientDisconnectStopsEphemeralSession(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "slow"}, auth: "none", delay: time.Second, timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", st.url+"/v1/chat/completions", strings.NewReader(minimalBody("hi")))
	req.Header.Set("Content-Type", "application/json")
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, _ = http.DefaultClient.Do(req)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.stopped.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if st.stopped.Load() == 0 {
		t.Fatal("client disconnect did not stop the ephemeral session")
	}
	st.mu.Lock()
	keys := append([]string(nil), st.stopKeys...)
	st.mu.Unlock()
	if len(keys) == 0 || !strings.HasPrefix(keys[0], "coder|req:") {
		t.Fatalf("stop keys = %v; want coder|req:<id>", keys)
	}
}

func TestConcurrentRequestsDoNotMix(t *testing.T) {
	st := newStack(t, stackOpts{
		canned: canned{text: "x"}, auth: "user_token", echo: true, delay: 80 * time.Millisecond,
		tokenUser: func(tok string) (string, bool) {
			if tok == "a" {
				return "user-a", true
			}
			if tok == "b" {
				return "user-b", true
			}
			return "", false
		},
	})
	type result struct {
		prompt string
		status int
		body   string
	}
	results := make(chan result, 2)
	for _, tok := range []string{"a", "b"} {
		tok := tok
		go func() {
			prompt := "prompt-" + tok
			status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody(prompt), map[string]string{"Authorization": "Bearer " + tok}))
			results <- result{prompt: prompt, status: status, body: body}
		}()
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.status != 200 {
			t.Fatalf("%s: status %d: %s", r.prompt, r.status, r.body)
		}
		if !strings.Contains(r.body, r.prompt) {
			t.Fatalf("response for %s carried another request's answer: %s", r.prompt, r.body)
		}
	}
}

func TestRequestRegistryIsDrained(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "ok", usage: &gateway.Usage{Input: 1, Output: 1}}, auth: "none"})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.reqs.Len() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := st.reqs.Len(); n != 0 {
		t.Fatalf("request registry kept %d entries after the turn", n)
	}
}

// TestHeartbeatKeepsAnIdleStreamOpen: while the model is quiet the stream sends
// an SSE comment, which proxies treat as traffic without ending the response.
func TestHeartbeatKeepsAnIdleStreamOpen(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = old })

	st := newStack(t, stackOpts{canned: canned{text: "late"}, auth: "none", delay: 150 * time.Millisecond})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions",
		`{"model":"coder","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("no heartbeat in an idle stream:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream did not finish:\n%s", body)
	}
}

// dropEgressFilter passes ingress and refuses every outbound text.
type dropEgressFilter struct{}

func (dropEgressFilter) Name() string { return "drop-egress" }
func (dropEgressFilter) Ingress(_ context.Context, in safety.IngressInput) safety.IngressResult {
	return safety.IngressResult{Text: in.Message}
}
func (dropEgressFilter) Egress(_ context.Context, in safety.EgressInput) safety.EgressResult {
	return safety.EgressResult{Drop: true, Reason: "test-egress"}
}

// TestOutputBlockedIsContentFilter: the model answered, the safety profile
// refused to show it, and the HTTP contract's answer is a 200 with
// finish_reason content_filter — not a dropped connection and not a 500.
func TestOutputBlockedIsContentFilter(t *testing.T) {
	disp := safety.New(&safety.Profile{Name: "test", Filters: []safety.Filter{dropEgressFilter{}}})
	st := newStack(t, stackOpts{canned: canned{text: "secret"}, auth: "none", safety: disp})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if !strings.Contains(body, `"finish_reason":"content_filter"`) {
		t.Fatalf("finish_reason is not content_filter: %s", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("blocked text leaked: %s", body)
	}
}

// TestEmptyTurnIsAnEmptyAssistantMessage: a turn that produced nothing is an
// empty answer, not a timeout.
func TestEmptyTurnIsAnEmptyAssistantMessage(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{}, auth: "none"})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if !strings.Contains(body, `"content":""`) {
		t.Fatalf("empty turn body: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("empty turn finish: %s", body)
	}
}

// TestDrainingReturns503: the readiness flag the admin plane flips during a
// shutdown is honoured by the channel before any work is done.
func TestDrainingReturns503(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "x"}, auth: "none", notReady: true})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "server_unavailable") {
		t.Fatalf("status %d: %s; want 503 server_unavailable", status, body)
	}
}

// TestIdempotencyJoinsTheRunningTurn: two requests with the same key while the
// first is still running produce one model call and one answer.
func TestIdempotencyJoinsTheRunningTurn(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "one answer"}, auth: "none", delay: 200 * time.Millisecond})
	headers := map[string]string{"Idempotency-Key": "retry-1"}
	first := make(chan string, 1)
	go func() {
		_, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), headers))
		first <- body
	}()
	time.Sleep(50 * time.Millisecond)
	_, second := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), headers))
	bodyA := <-first
	if bodyA != second {
		t.Fatalf("joined request got a different answer:\nA %s\nB %s", bodyA, second)
	}
	if n := st.upstream.callCount(); n != 1 {
		t.Fatalf("provider calls = %d; want 1 for an idempotent retry", n)
	}
}

// TestIdempotencyAfterFinishIsANewTurn: the key joins only a running turn.
func TestIdempotencyAfterFinishIsANewTurn(t *testing.T) {
	st := newStack(t, stackOpts{canned: canned{text: "answer"}, auth: "none"})
	headers := map[string]string{"Idempotency-Key": "retry-2"}
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), headers))
	if status != 200 {
		t.Fatalf("first: %d %s", status, body)
	}
	// Let the turn finish and deregister before retrying.
	time.Sleep(50 * time.Millisecond)
	status, body = readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), headers))
	if status != 200 {
		t.Fatalf("second: %d %s", status, body)
	}
	if n := st.upstream.callCount(); n != 2 {
		t.Fatalf("provider calls = %d; a retry after the turn finished is a new turn", n)
	}
}
