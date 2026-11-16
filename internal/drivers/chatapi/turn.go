package chatapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
)

// turn is one in-flight chat completion: the reply the HTTP request is waiting
// for, and the text that has already gone out. Delivery methods are called by
// the session actor (one turn at a time); the HTTP handler only reads.
type turn struct {
	driver         *Driver
	id             string // request id: also the message id and reply id
	ref            string // request registry key
	agent          string // the agent the request named
	model          string // the client-visible model id (same as agent)
	conversationID string
	stream         bool
	includeUsage   bool
	created        int64
	log            *slog.Logger

	mu        sync.Mutex
	sendText  strings.Builder // session.send content
	deltaText strings.Builder // text already streamed to the client
	reply     *gateway.Reply
	ended     bool
	endErr    error
	abandoned bool

	frames chan []byte // streaming frames; closed by finish
	gone   chan struct{}
	done   chan struct{} // closed when a buffered result is ready
	result *bufferedResult

	forgetOnce sync.Once
}

// bufferedResult is what a non-streaming request answers with, built on the
// actor's goroutine so the handler only copies bytes.
type bufferedResult struct {
	status int
	body   []byte
}

func newTurn(d *Driver, req *chatreq.Request) *turn {
	t := &turn{
		driver:         d,
		id:             req.ID,
		ref:            req.Ref,
		agent:          req.Model,
		model:          req.Model,
		conversationID: req.ConversationID,
		stream:         req.Stream,
		includeUsage:   req.IncludeUsage,
		created:        time.Now().Unix(),
		log:            d.log.With("request", req.ID),
		gone:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if req.Stream {
		t.frames = make(chan []byte, 128)
	}
	return t
}

// Deliver implements gateway.Driver: on this channel a send appends to the
// turn's content (3.4). Media replies have no expression in the OpenAI
// response shapes.
func (t *turn) Deliver(replyTo, text string, attachments []media.Part) error {
	if len(attachments) > 0 {
		return fmt.Errorf("openai channel %q does not support media replies", t.driver.name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return nil // a push after the turn finished has nowhere to go
	}
	t.sendText.WriteString(text)
	return nil
}

// DeliverDelta relays one text fragment. A non-streaming client still gets the
// text: it joins the final content, because the client never asked to watch it
// arrive.
func (t *turn) DeliverDelta(replyTo, text string) error {
	if text == "" {
		return nil
	}
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return nil
	}
	t.deltaText.WriteString(text)
	stream := t.stream
	if !stream {
		t.sendText.WriteString(text)
	}
	t.mu.Unlock()
	if !stream {
		return nil
	}
	return t.writeFrame(sseData(t.dataChunk(map[string]any{"content": text})))
}

// DeliverReply stores the structured answer and finalises the turn.
func (t *turn) DeliverReply(replyTo string, r gateway.Reply) error {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return nil
	}
	if t.reply != nil {
		t.mu.Unlock()
		return fmt.Errorf("openai channel: a reply was already delivered for this turn")
	}
	t.reply = &r
	t.mu.Unlock()
	t.finish(nil)
	return nil
}

// EndTurn finalises. It is called on a normal end, a crash and a cancellation;
// finish is single-shot, so a DeliverReply-then-EndTurn pair is harmless.
func (t *turn) EndTurn(replyTo string, err error) { t.finish(err) }

// finish builds the answer and releases the waiter.
func (t *turn) finish(err error) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	t.endErr = err
	t.mu.Unlock()

	t.driver.remove(t)

	if t.stream {
		t.finishStream(err)
		return
	}
	t.finishBuffered(err)
}

// forgetRequest drops the parsed request once, whichever path finishes the
// turn first.
func (t *turn) forgetRequest() {
	t.forgetOnce.Do(func() { t.driver.opt.Requests.Remove(t.ref) })
}

// abandon means the client stopped listening. Frames are dropped rather than
// blocking the actor; the turn itself is stopped only when it is ephemeral.
func (t *turn) abandon() {
	t.mu.Lock()
	if !t.abandoned {
		t.abandoned = true
		close(t.gone)
	}
	t.mu.Unlock()
}

// writeFrame hands one SSE frame to the handler, or drops it if the client is
// gone.
func (t *turn) writeFrame(frame []byte) error {
	t.mu.Lock()
	abandoned, ch := t.abandoned, t.frames
	t.mu.Unlock()
	if abandoned || ch == nil {
		return nil
	}
	select {
	case ch <- frame:
		return nil
	case <-t.gone:
		return nil
	}
}

// finishBuffered builds a chat.completion object, or an error envelope when the
// turn failed before producing anything.
func (t *turn) finishBuffered(err error) {
	t.mu.Lock()
	send := t.sendText.String()
	reply := t.reply
	t.mu.Unlock()

	content := send
	finish := "stop"
	var toolCalls []gateway.ToolCall
	var usage *gateway.Usage
	var reasoning string
	if reply != nil {
		content += reply.Text
		if reply.FinishReason != "" {
			finish = reply.FinishReason
		}
		toolCalls = reply.ToolCalls
		usage = reply.Usage
		reasoning = reply.Reasoning
	}

	if err != nil {
		status, typ, code, message := statusForError(err)
		if content == "" && len(toolCalls) == 0 {
			t.result = &bufferedResult{status: status, body: errorBody(typ, code, message)}
			close(t.done)
			return
		}
		// Output began: finish the stream rather than sending a malformed
		// half-answer. The error stays in the log.
		t.log.Warn("openai turn ended with an error after output", "err", err)
		finish = "stop"
	}
	if reply == nil && err == nil {
		finish = "stop"
	}

	msg := map[string]any{"role": "assistant"}
	if toolCalls == nil {
		msg["content"] = content
	} else if content != "" {
		msg["content"] = content
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCallsOut(toolCalls, false)
	}
	if t.driver.opt.ExposeReasoning && reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	obj := map[string]any{
		"id":      "chatcmpl-" + t.id,
		"object":  "chat.completion",
		"created": t.createdUnix(),
		"model":   t.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
			"logprobs":      nil,
		}},
	}
	if usage != nil {
		obj["usage"] = usageObject(usage)
	}
	body, _ := json.Marshal(obj)
	t.result = &bufferedResult{status: 200, body: body}
	close(t.done)
}

// finishStream writes the tail of the SSE stream: any text not yet relayed,
// the complete tool calls, the finish reason, usage when asked for, and [DONE].
func (t *turn) finishStream(err error) {
	ch := t.frames
	t.mu.Lock()
	send := t.sendText.String()
	streamed := t.deltaText.String()
	reply := t.reply
	t.mu.Unlock()

	if err != nil {
		status, typ, code, message := statusForError(err)
		hasOutput := send != "" || streamed != "" || (reply != nil && (reply.Text != "" || len(reply.ToolCalls) > 0))
		if !hasOutput {
			_ = t.writeFrame(sseData(map[string]any{
				"error": errorEnvelope(typ, code, message),
			}))
			_ = t.writeFrame([]byte("data: [DONE]\n\n"))
			close(ch)
			return
		}
		if status >= 400 {
			t.log.Warn("openai stream ended with an error after output", "err", err)
		}
	}

	finish := "stop"
	var toolCalls []gateway.ToolCall
	var usage *gateway.Usage
	var reasoning string
	replyText := ""
	if reply != nil {
		replyText = reply.Text
		finish = reply.FinishReason
		toolCalls = reply.ToolCalls
		usage = reply.Usage
		reasoning = reply.Reasoning
	}
	// Remaining text: what send() accumulated (never streamed) plus the part of
	// a reply the deltas did not already carry.
	remaining := send
	if replyText != "" {
		if strings.HasPrefix(replyText, streamed) {
			remaining += replyText[len(streamed):]
		} else if streamed == "" {
			remaining += replyText
		}
	}
	if remaining != "" {
		_ = t.writeFrame(sseData(t.dataChunk(map[string]any{"content": remaining})))
	}
	if t.driver.opt.ExposeReasoning && reasoning != "" {
		_ = t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning_content": reasoning}, "finish_reason": nil,
			}},
		}))
	}
	if len(toolCalls) > 0 {
		_ = t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"tool_calls": toolCallsOut(toolCalls, true)}, "finish_reason": nil,
			}},
		}))
	}
	if finish == "" {
		finish = "stop"
	}
	_ = t.writeFrame(sseData(map[string]any{
		"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}},
	}))
	if t.includeUsage {
		if usage == nil {
			usage = &gateway.Usage{}
		}
		_ = t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{}, "usage": usageObject(usage),
		}))
	}
	_ = t.writeFrame([]byte("data: [DONE]\n\n"))
	close(ch)
}

// createdUnix is the turn's timestamp: one value for the whole answer, so every
// chunk agrees and no two goroutines write the field.
func (t *turn) createdUnix() int64 { return t.created }

// errorBody encodes the HTTP error envelope.
func errorBody(typ, code, message string) []byte {
	b, _ := json.Marshal(map[string]any{"error": errorEnvelope(typ, code, message)})
	return b
}

func errorEnvelope(typ, code, message string) map[string]any {
	return map[string]any{"message": message, "type": typ, "code": code}
}

// usageObject maps the runtime's token counts to OpenAI's names.
func usageObject(u *gateway.Usage) map[string]any {
	total := u.Input + u.Output
	out := map[string]any{
		"prompt_tokens":     u.Input,
		"completion_tokens": u.Output,
		"total_tokens":      total,
	}
	if u.Cached != 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	if u.Reasoning != 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.Reasoning}
	}
	return out
}

// toolCallsOut renders the complete tool calls. A streaming client receives
// them in one chunk; the protocol allows one fragment carrying the whole
// arguments string.
func toolCallsOut(calls []gateway.ToolCall, stream bool) []map[string]any {
	out := make([]map[string]any, len(calls))
	for i, c := range calls {
		m := map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		}
		// index is a streaming-chunk field; a chat.completion message does not
		// carry it.
		if stream {
			m["index"] = i
		}
		out[i] = m
	}
	return out
}

// dataChunk wraps one delta in the chat.completion.chunk envelope.
func (t *turn) dataChunk(delta map[string]any) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + t.id,
		"object":  "chat.completion.chunk",
		"created": t.createdUnix(),
		"model":   t.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
}

// sseData frames one JSON payload as a Server-Sent Event data line.
func sseData(v any) []byte {
	b, _ := json.Marshal(v)
	return []byte("data: " + string(b) + "\n\n")
}
