package chatapi

import (
	"net/http"
	"time"
)

// serveBuffered waits for the turn and writes one chat.completion object. The
// wait is bounded by the channel timeout, which is the whole request, not one
// model call.
func (d *Driver) serveBuffered(w http.ResponseWriter, r *http.Request, t *turn) {
	timer := time.NewTimer(d.opt.Timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		res := t.result
		if res.status == http.StatusTooManyRequests {
			// A refusal the router produced (the ephemeral or session cap) ends
			// the turn rather than the handler's own path, so it needs the
			// Retry-After header here too.
			w.Header().Set("Retry-After", "1")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.status)
		_, _ = w.Write(res.body)
	case <-r.Context().Done():
		// The client went away. The answer has nowhere to go, and an ephemeral
		// session has no reason to keep spending tokens for it.
		d.leave(t)
	case <-timer.C:
		d.leave(t)
		writeError(w, http.StatusGatewayTimeout, "timeout", "timeout", "the agent did not answer within the request timeout")
	}
}

// serveStream speaks SSE: the role chunk first, a comment heartbeat while the
// model is quiet, then the frames the turn has logged — from the beginning, so
// a retry that joined a running turn receives the whole answer — and the tail
// finish, usage and [DONE] frames.
func (d *Driver) serveStream(w http.ResponseWriter, r *http.Request, t *turn) {
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The role chunk goes out before the model says anything, which is what
	// lets a client render an assistant bubble immediately.
	_, _ = w.Write(sseData(t.dataChunk(map[string]any{"role": "assistant", "content": ""})))
	flush()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	timer := time.NewTimer(d.opt.Timeout)
	defer timer.Stop()

	cursor := 0
	for {
		frames, closed, wake := t.framesFrom(cursor)
		for _, frame := range frames {
			_, _ = w.Write(frame)
		}
		if len(frames) > 0 {
			cursor += len(frames)
			flush()
		}
		if closed {
			return
		}
		select {
		case <-wake:
		case <-heartbeat.C:
			// A comment line is valid SSE and keeps proxies from closing an
			// idle connection.
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flush()
		case <-r.Context().Done():
			d.leave(t)
			return
		case <-timer.C:
			d.leave(t)
			if cursor > 0 {
				// Output already went out: finish the stream cleanly rather
				// than sending a malformed one.
				_, _ = w.Write(sseData(map[string]any{
					"id": "chatcmpl-" + t.id, "object": "chat.completion.chunk", "created": t.createdUnix(), "model": t.model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				}))
			} else {
				_, typ, code, message := statusForError(&turnTimeout{})
				_, _ = w.Write(sseData(map[string]any{"error": errorEnvelope(typ, code, message)}))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flush()
			return
		}
	}
}

// turnTimeout is the error the handler's own deadline maps to (504 timeout).
type turnTimeout struct{}

func (*turnTimeout) Error() string { return "the agent did not answer within the request timeout" }
