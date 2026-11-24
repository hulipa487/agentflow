package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/safety"
)

const streamBody = `{"model":"coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// firstToolArguments pulls choices[0].message.tool_calls[0].function.arguments.
func firstToolArguments(t *testing.T, body string) string {
	t.Helper()
	var obj struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil || len(obj.Choices) == 0 || len(obj.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("no tool call in %s (%v)", body, err)
	}
	return obj.Choices[0].Message.ToolCalls[0].Function.Arguments
}

// TestToolArgumentsAreReturnedUnchanged: the model's tool call crosses Lua on
// its way back (llm.forward returns it, session.reply sends it). A Lua table
// cannot tell [] from {}, drops nulls and rounds integers above 2^53, so the
// decoded arguments alone would come back as a different call: an empty file
// list became an object, which a coding client rejects.
func TestToolArgumentsAreReturnedUnchanged(t *testing.T) {
	const args = `{"files":[],"opt":null,"big":12345678901234567,"nested":{"a":[]}}`
	st := newStack(t, stackOpts{auth: "none", canned: canned{
		toolCalls: []gateway.ToolCall{{ID: "call_1", Name: "edit", Arguments: args}}, finish: "tool_calls",
	}})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := firstToolArguments(t, body); got != args {
		t.Fatalf("arguments changed on the way back:\n got %s\nwant %s", got, args)
	}
}

// TestFinishReasonLengthIsPassedOn: an answer cut short by the output limit
// must say so. Reported as "stop", a client takes a truncated file for a
// complete one.
func TestFinishReasonLengthIsPassedOn(t *testing.T) {
	st := newStack(t, stackOpts{auth: "none", canned: canned{text: "truncated", finish: "length"}})
	_, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("finish_reason was not passed on: %s", body)
	}
	_, sse := readAll(t, st.do(t, "POST", "/v1/chat/completions", streamBody, nil))
	if !strings.Contains(sse, `"finish_reason":"length"`) {
		t.Fatalf("streaming finish_reason was not passed on: %s", sse)
	}
}

// TestIdempotencyKeyIsScopedToTheCaller: the header is chosen by the client.
// Keyed by the header alone, a second caller using the same value attached to
// the first caller's running turn and was sent its answer.
func TestIdempotencyKeyIsScopedToTheCaller(t *testing.T) {
	users := map[string]string{"tok-a": "u_a", "tok-b": "u_b"}
	st := newStack(t, stackOpts{echo: true, auth: "user_token", delay: 300 * time.Millisecond,
		tokenUser: func(tok string) (string, bool) { u, ok := users[tok]; return u, ok }})
	first := make(chan string, 1)
	go func() {
		_, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("secret of A"),
			map[string]string{"Idempotency-Key": "k", "Authorization": "Bearer tok-a"}))
		first <- body
	}()
	time.Sleep(60 * time.Millisecond)
	_, b := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("question of B"),
		map[string]string{"Idempotency-Key": "k", "Authorization": "Bearer tok-b"}))
	a := <-first
	if strings.Contains(b, "secret of A") {
		t.Fatalf("caller B received caller A's answer: %s", b)
	}
	if !strings.Contains(b, "question of B") || !strings.Contains(a, "secret of A") {
		t.Fatalf("answers:\nA %s\nB %s", a, b)
	}
	if n := st.upstream.callCount(); n != 2 {
		t.Fatalf("provider calls = %d; two callers are two turns", n)
	}
}

// TestIdempotencyKeyWithADifferentBodyIsANewTurn: a reused key on a different
// request is not a retry.
func TestIdempotencyKeyWithADifferentBodyIsANewTurn(t *testing.T) {
	st := newStack(t, stackOpts{echo: true, auth: "none", delay: 200 * time.Millisecond})
	h := map[string]string{"Idempotency-Key": "same"}
	first := make(chan string, 1)
	go func() {
		_, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("one"), h))
		first <- body
	}()
	time.Sleep(50 * time.Millisecond)
	_, second := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("two"), h))
	if !strings.Contains(<-first, "one") || !strings.Contains(second, "two") {
		t.Fatalf("a different body under the same key was treated as a retry: %s", second)
	}
}

// TestJoinedStreamGetsEveryFrame: two requests attached to one streaming turn
// must each receive the whole stream. Reading one shared channel, each got a
// random share of the frames and only one got [DONE].
func TestJoinedStreamGetsEveryFrame(t *testing.T) {
	text := strings.Repeat("abcdefghijklmnopqrstuvwx", 20)
	st := newStack(t, stackOpts{canned: canned{text: text}, auth: "none", delay: 200 * time.Millisecond})
	h := map[string]string{"Idempotency-Key": "s"}
	first := make(chan string, 1)
	go func() {
		_, b := readAll(t, st.do(t, "POST", "/v1/chat/completions", streamBody, h))
		first <- b
	}()
	time.Sleep(50 * time.Millisecond)
	_, b := readAll(t, st.do(t, "POST", "/v1/chat/completions", streamBody, h))
	a := <-first
	for name, body := range map[string]string{"first": a, "joined": b} {
		_, chunks, done := parseSSE(t, body)
		var got strings.Builder
		for _, c := range chunks {
			choices, _ := c["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			got.WriteString(contentString(delta["content"]))
		}
		if !done {
			t.Errorf("%s request: stream has no [DONE]", name)
		}
		if got.String() != text {
			t.Errorf("%s request: got %d of %d bytes of the answer", name, got.Len(), len(text))
		}
	}
	if n := st.upstream.callCount(); n != 1 {
		t.Fatalf("provider calls = %d; want 1 for an idempotent retry", n)
	}
}

// TestSendDeltaThenReplyDoesNotDuplicate: a loop that streams fragments and
// then replies with the same text gets it sent once, whether or not the client
// asked for a stream.
func TestSendDeltaThenReplyDoesNotDuplicate(t *testing.T) {
	loop := `// af-prelude-version: 2
function loop() {
  while (true) {
    session.inbox();
    session.send_delta("Hello, ");
    session.send_delta("world");
    session.reply({ text: "Hello, world" });
  }
}`
	st := newStack(t, stackOpts{auth: "none", loopSrc: loop})
	_, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if !strings.Contains(body, `"content":"Hello, world"`) {
		t.Fatalf("buffered answer: %s", body)
	}
	_, sse := readAll(t, st.do(t, "POST", "/v1/chat/completions", streamBody, nil))
	if n := strings.Count(sse, "world"); n != 1 {
		t.Fatalf("the text went out %d times in the stream: %s", n, sse)
	}
}

// TestLoopFailureIsNotBlamedOnTheModel: a bug in the agent's own Lua is a
// server error. Reported as 502 upstream_error it sends an operator to the
// model provider's status page.
func TestLoopFailureIsNotBlamedOnTheModel(t *testing.T) {
	loop := `// af-prelude-version: 2
function loop() {
  session.inbox();
  throw new Error("a bug in the loop");
}`
	st := newStack(t, stackOpts{auth: "none", loopSrc: loop})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != http.StatusInternalServerError || !strings.Contains(body, `"code":"server_error"`) {
		t.Fatalf("status %d: %s; want 500 server_error", status, body)
	}
	if strings.Contains(body, "a bug in the loop") {
		t.Fatalf("the loop's error text leaked to the client: %s", body)
	}
}

// TestRouteThatDropsTheRequestAnswersAtOnce: a route handler that takes the
// message and delivers it nowhere used to leave the client waiting out the
// whole request timeout, and its callback registered in the router forever.
func TestRouteThatDropsTheRequestAnswersAtOnce(t *testing.T) {
	route := `// af-prelude-version: 2
function loop() {
  while (true) {
    af.op({ type: "inbox" });
  }
}`
	st := newStack(t, stackOpts{auth: "none", routeSrc: route, timeout: 30 * time.Second})
	start := time.Now()
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "server_unavailable") {
		t.Fatalf("status %d: %s; want 503 server_unavailable", status, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the refusal took %s; it must not wait for the request timeout", d)
	}
}

type refusingIngress struct{}

func (refusingIngress) Name() string { return "refuse" }
func (refusingIngress) Ingress(_ context.Context, _ safety.IngressInput) safety.IngressResult {
	return safety.IngressResult{Drop: true, Skip: true, Reason: "test", Refusal: "I can't help with that."}
}
func (refusingIngress) Egress(_ context.Context, in safety.EgressInput) safety.EgressResult {
	return safety.EgressResult{Text: in.Text}
}

// TestBlockedInputIs400EvenWithRefusalText: a profile that answers a blocked
// message with refusal text must not turn the refusal into a 200 assistant
// message. The contract is 400 content_filter; the text is the error message.
func TestBlockedInputIs400EvenWithRefusalText(t *testing.T) {
	disp := safety.New(&safety.Profile{Name: "test", Filters: []safety.Filter{refusingIngress{}}})
	st := newStack(t, stackOpts{canned: canned{text: "x"}, auth: "none", safety: disp})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("blocked"), nil))
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"content_filter"`) {
		t.Fatalf("status %d: %s; want 400 content_filter", status, body)
	}
	if !strings.Contains(body, "I can't help with that.") {
		t.Fatalf("the refusal text is not in the error message: %s", body)
	}
	if n := st.upstream.callCount(); n != 0 {
		t.Fatalf("a blocked request reached the model %d time(s)", n)
	}
}

// TestBlockedSendIsContentFilter: a loop that answers with session.send and
// has its text refused ends the turn with finish_reason content_filter, the
// same as a refused session.reply.
func TestBlockedSendIsContentFilter(t *testing.T) {
	loop := `// af-prelude-version: 2
function loop() {
  while (true) {
    session.inbox();
    try { session.send("secret"); } catch (e) {}
  }
}`
	disp := safety.New(&safety.Profile{Name: "test", Filters: []safety.Filter{dropEgressFilter{}}})
	st := newStack(t, stackOpts{auth: "none", safety: disp, loopSrc: loop})
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if status != 200 || !strings.Contains(body, `"finish_reason":"content_filter"`) {
		t.Fatalf("status %d: %s; want 200 with finish_reason content_filter", status, body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("blocked text leaked: %s", body)
	}
}

// TestExposeReasoningWithThePassthroughLoop: the setting has to work with the
// loop the channel ships. llm.forward calls the model's reasoning "thinking";
// session.reply must carry it as the reply's reasoning.
func TestExposeReasoningWithThePassthroughLoop(t *testing.T) {
	c := canned{text: "answer", reasoning: "because"}
	on := newStack(t, stackOpts{auth: "none", canned: c, exposeReasoning: true})
	_, body := readAll(t, on.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if !strings.Contains(body, `"reasoning_content":"because"`) {
		t.Fatalf("expose_reasoning is on but the answer has no reasoning_content: %s", body)
	}
	off := newStack(t, stackOpts{auth: "none", canned: c})
	_, body = readAll(t, off.do(t, "POST", "/v1/chat/completions", minimalBody("hi"), nil))
	if strings.Contains(body, "because") {
		t.Fatalf("reasoning reached the client with expose_reasoning off: %s", body)
	}
}

// TestClientOptionsReachTheModel is the end-to-end form of the pass-through
// promise, through the real route, loop and Lua bridge.
func TestClientOptionsReachTheModel(t *testing.T) {
	st := newStack(t, stackOpts{auth: "none", canned: canned{text: "ok"}})
	req := `{"model":"coder","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}],
		"temperature":0.2,"top_p":0.5,"seed":42,"stop":"END","max_completion_tokens":64,
		"parallel_tool_calls":false,"tool_choice":"required","response_format":{"type":"json_object"},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", req, nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	up := st.upstream.lastBody()
	for _, want := range []string{
		`"temperature":0.2`, `"top_p":0.5`, `"seed":42`, `"stop":["END"]`, `"parallel_tool_calls":false`,
		`"tool_choice":"required"`, `"response_format":{"type":"json_object"}`, `"name":"lookup"`,
		`"role":"developer"`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("the model's request is missing %s\nbody: %s", want, up)
		}
	}
}

// TestForwardOptionSpellings: a named tool choice written as a table and a
// single stop string are accepted from Lua.
func TestForwardOptionSpellings(t *testing.T) {
	loop := `// af-prelude-version: 2
function loop() {
  while (true) {
    session.inbox();
    const reply = llm.forward({ params: { tool_choice: { name: "lookup" }, stop: "END" } });
    session.reply(reply);
  }
}`
	st := newStack(t, stackOpts{auth: "none", canned: canned{text: "ok"}, loopSrc: loop})
	req := `{"model":"coder","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`
	status, body := readAll(t, st.do(t, "POST", "/v1/chat/completions", req, nil))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	up := st.upstream.lastBody()
	if !strings.Contains(up, `"tool_choice":{"function":{"name":"lookup"},"type":"function"}`) || !strings.Contains(up, `"stop":["END"]`) {
		t.Fatalf("the option spellings did not reach the model: %s", up)
	}
}

// TestFilePartsGoThroughTheMediaPolicy: an inline file is media like an inline
// image. It used to skip the channel's policy and was handed to the provider
// as a data URL where base64 was expected.
func TestFilePartsGoThroughTheMediaPolicy(t *testing.T) {
	req := `{"model":"coder","messages":[{"role":"user","content":[
		{"type":"text","text":"read this"},
		{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`
	closed := newStack(t, stackOpts{auth: "none", canned: canned{text: "ok"}})
	status, body := readAll(t, closed.do(t, "POST", "/v1/chat/completions", req, nil))
	if status != http.StatusBadRequest || !strings.Contains(body, "application/pdf") {
		t.Fatalf("status %d: %s; a channel with no media policy must refuse the file", status, body)
	}
}
