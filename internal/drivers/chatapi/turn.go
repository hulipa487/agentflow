package chatapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/session"
)

// turn is one in-flight chat completion: the reply the HTTP request is waiting
// for, and the text that has already gone out. Delivery methods are called by
// the session actor (one turn at a time); HTTP handlers only read.
//
// A turn can have more than one HTTP request attached: a retry carrying the
// same Idempotency-Key joins the turn that is still running. That is why a
// streaming turn keeps its frames in a log rather than a channel — a channel
// hands each frame to exactly one reader, so two attached clients would each
// get a random half of the stream, and a client that joined late would miss
// the beginning. Every listener reads the log from the start with its own
// cursor.
type turn struct {
	driver         *Driver
	id             string // request id: also the reply id
	ref            string // request registry key
	agent          string // the agent the request named
	model          string // the client-visible model id (same as agent)
	conversationID string
	stream         bool
	includeUsage   bool
	created        int64
	idemKey        string // the scoped Idempotency-Key hash this turn is joined by, or ""
	log            *slog.Logger

	mu        sync.Mutex
	sendText  strings.Builder // session.send content
	deltaText strings.Builder // text delivered as deltas
	reply     *gateway.Reply
	ended     bool
	endErr    error
	listeners int // HTTP requests currently attached

	// Streaming: every SSE frame produced so far, whether the log is complete,
	// and a channel that is closed (and replaced) whenever either changes.
	frames [][]byte
	closed bool
	wake   chan struct{}

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
	return &turn{
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
		wake:           make(chan struct{}),
		done:           make(chan struct{}),
	}
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
	t.mu.Unlock()
	if stream {
		t.writeFrame(sseData(t.dataChunk(map[string]any{"content": text})))
	}
	return nil
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

// attach counts one more HTTP request waiting on this turn. Callers hold the
// driver's lock, so an attach cannot race the turn being forgotten.
func (t *turn) attach() {
	t.mu.Lock()
	t.listeners++
	t.mu.Unlock()
}

// detach is one HTTP request giving up on the turn (the client went away, or
// its own deadline passed). It reports whether nobody is listening any more
// while the turn is still running — the caller then decides whether the work
// is worth finishing.
func (t *turn) detach() (orphaned bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.listeners > 0 {
		t.listeners--
	}
	return t.listeners == 0 && !t.ended
}

// writeFrame appends one SSE frame to the log and wakes every listener. It
// never blocks the session actor: a slow client costs memory bounded by the
// size of one answer, not a stalled turn.
func (t *turn) writeFrame(frame []byte) {
	t.mu.Lock()
	if !t.closed {
		t.frames = append(t.frames, frame)
		close(t.wake)
		t.wake = make(chan struct{})
	}
	t.mu.Unlock()
}

// closeFrames marks the stream complete.
func (t *turn) closeFrames() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.wake)
		t.wake = make(chan struct{})
	}
	t.mu.Unlock()
}

// framesFrom returns the frames a listener has not written yet, whether the
// stream is complete, and the channel that signals the next change.
func (t *turn) framesFrom(cursor int) (frames [][]byte, closed bool, wake <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cursor < len(t.frames) {
		frames = t.frames[cursor:len(t.frames):len(t.frames)]
	}
	return frames, t.closed, t.wake
}

// replyTail is what a structured reply adds to text the turn already delivered
// as deltas. A loop that streams and then replies with the same text (the
// pass-through does, through llm.forward) must not have it sent twice; a reply
// that does not continue the streamed text cannot retract what the client has
// already rendered, so it is not appended to it.
func replyTail(streamed, reply string) string {
	switch {
	case reply == "":
		return ""
	case strings.HasPrefix(reply, streamed):
		return reply[len(streamed):]
	case streamed == "":
		return reply
	default:
		return ""
	}
}

// blockedInput reports whether the turn ended because ingress safety refused
// the message. That is a refusal of the request, whatever refusal text the
// profile sent on the way: the contract is 400 content_filter, not an
// assistant message.
func blockedInput(err error) bool { return errors.Is(err, session.ErrSafetyBlocked) }

// finishBuffered builds a chat.completion object, or an error envelope when the
// turn failed before producing anything.
func (t *turn) finishBuffered(err error) {
	t.mu.Lock()
	send := t.sendText.String()
	streamed := t.deltaText.String()
	reply := t.reply
	t.mu.Unlock()

	// The same order a streaming client sees: deltas as they arrived, then
	// what session.send accumulated, then whatever the reply adds.
	content := streamed + send
	finish := "stop"
	var toolCalls []gateway.ToolCall
	var usage *gateway.Usage
	var reasoning string
	if reply != nil {
		content += replyTail(streamed, reply.Text)
		if reply.FinishReason != "" {
			finish = reply.FinishReason
		}
		toolCalls = reply.ToolCalls
		usage = reply.Usage
		reasoning = reply.Reasoning
	}

	if err != nil {
		status, typ, code, message := statusForError(err)
		if blockedInput(err) {
			// The profile's refusal text, when it sent one, is the most useful
			// thing the error can say.
			if send != "" {
				message = send
			}
			t.result = &bufferedResult{status: status, body: errorBody(typ, code, message)}
			close(t.done)
			return
		}
		if content == "" && len(toolCalls) == 0 {
			t.result = &bufferedResult{status: status, body: errorBody(typ, code, message)}
			close(t.done)
			return
		}
		// Output began: finish the answer rather than sending a malformed
		// half-answer. The error stays in the log.
		t.log.Warn("openai turn ended with an error after output", "err", err)
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
	t.mu.Lock()
	send := t.sendText.String()
	streamed := t.deltaText.String()
	reply := t.reply
	t.mu.Unlock()

	if err != nil {
		status, typ, code, message := statusForError(err)
		hasOutput := send != "" || streamed != "" || (reply != nil && (reply.Text != "" || len(reply.ToolCalls) > 0))
		if blockedInput(err) {
			// A refused request is an error event even when the profile sent
			// refusal text: that text is the message, not an answer.
			if send != "" {
				message = send
			}
			hasOutput = false
		}
		if !hasOutput {
			t.writeFrame(sseData(map[string]any{
				"error": errorEnvelope(typ, code, message),
			}))
			t.writeFrame([]byte("data: [DONE]\n\n"))
			t.closeFrames()
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
	remaining := send + replyTail(streamed, replyText)
	if remaining != "" {
		t.writeFrame(sseData(t.dataChunk(map[string]any{"content": remaining})))
	}
	if t.driver.opt.ExposeReasoning && reasoning != "" {
		t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning_content": reasoning}, "finish_reason": nil,
			}},
		}))
	}
	if len(toolCalls) > 0 {
		t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"tool_calls": toolCallsOut(toolCalls, true)}, "finish_reason": nil,
			}},
		}))
	}
	if finish == "" {
		finish = "stop"
	}
	t.writeFrame(sseData(map[string]any{
		"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}},
	}))
	if t.includeUsage {
		if usage == nil {
			usage = &gateway.Usage{}
		}
		t.writeFrame(sseData(map[string]any{
			"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
			"choices": []any{}, "usage": usageObject(usage),
		}))
	}
	t.writeFrame([]byte("data: [DONE]\n\n"))
	t.closeFrames()
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
