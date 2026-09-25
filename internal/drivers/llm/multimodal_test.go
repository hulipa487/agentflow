package llm

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

// imageTurn is one user turn carrying an inline image part.
func imageTurn(dataB64 string) Message {
	return Message{
		Role: "user",
		Parts: []media.Part{
			{Type: "text", Text: "what is this?"},
			{Type: "image", MIME: "image/png", Data: dataB64},
		},
	}
}

const pngB64 = "aGVsbG8=" // "hello"

func multimodalChat(t *testing.T, provider string, msgs []Message, wantErr string) {
	t.Helper()
	srv := httptest.NewServer(simpleReply())
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		// The genai client's Gemini backend has no keyless mode, so a gemini model
		// always carries a key here; every other provider is happy without one.
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL, APIKey: geminiTestKey(provider)},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := m.Chat(context.Background(), "default", msgs, Opts{})
	if wantErr == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("want error containing %q, got %v", wantErr, err)
	}
}

// geminiTestKey is the dummy api_key the gemini test models carry: the SDK
// refuses to build a Gemini-backend client without one, and the mocks never
// check it.
func geminiTestKey(provider string) string {
	if provider == "gemini" {
		return "test-key"
	}
	return ""
}

// simpleReply serves a minimal non-streaming OK response; the assertions in
// these tests run on the request body via assertBody.
func simpleReply() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

// openaiEmptyReply serves a minimal but *valid* OpenAI Chat Completions SSE
// stream. The shared simpleReply writes only `data: [DONE]`, which the
// official SDK's SSE reader consumes without ever yielding a chunk: the SDK
// sees a 200 carrying no events at all and reports exactly that, rather than
// treating it as an empty reply the way the hand-rolled reader did. These
// tests assert on the request body, so they just need a stream the provider
// understands.
func openaiEmptyReply() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}
}

// responsesEmptyReply is openaiEmptyReply's Responses-API counterpart: one
// terminal event the SDK can decode, then the stream terminator.
func responsesEmptyReply() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}
}

// anthropicEmptyReply serves a minimal but *valid* Anthropic SSE stream. The
// shared simpleReply writes only OpenAI's `data: [DONE]`, which Anthropic never
// sends — the SDK sees a 200 carrying no events at all and reports that, rather
// than treating it as an empty reply the way the hand-rolled reader did. These
// tests assert on the request body, so they just need a stream the provider
// understands.
func anthropicEmptyReply() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n"))
	}
}

func TestAnthropicImagePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{
			`"type":"image"`,
			`"media_type":"image/png"`,
			`"data":"aGVsbG8="`,
			`"text":"what is this?"`,
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		anthropicEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "anthropic", srv, []Message{imageTurn(pngB64)})
}

func TestAnthropicAudioRejected(t *testing.T) {
	multimodalChat(t, "anthropic", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "audio", MIME: "audio/wav", Data: pngB64}},
	}}, "does not support audio")
}

func TestAnthropicPDFPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"type":"document"`) {
			t.Errorf("body missing document block\nbody: %s", b)
		}
		anthropicEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "anthropic", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "file", MIME: "application/pdf", Data: pngB64, Name: "x.pdf"}},
	}})
}

func TestOpenAIChatImagePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{
			`"type":"image_url"`,
			`"url":"data:image/png;base64,aGVsbG8="`,
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		openaiEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{imageTurn(pngB64)})
}

func TestOpenAIChatImageURLPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"url":"https://example.com/cat.png"`) {
			t.Errorf("body missing url passthrough\nbody: %s", b)
		}
		openaiEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "image", MIME: "image/png", URL: "https://example.com/cat.png"}},
	}})
}

func TestOpenAIChatAudioPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{
			`"type":"input_audio"`,
			`"format":"wav"`,
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		openaiEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "audio", MIME: "audio/wav", Data: pngB64}},
	}})
}

// TestOpenAIChatPDFRejected: a base64-only file part is refused for want of a
// URL — GLM's file_url takes a URL source only.
func TestOpenAIChatPDFRejected(t *testing.T) {
	multimodalChat(t, "openai", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "file", MIME: "application/pdf", Data: pngB64}},
	}}, "requires a url source")
}

// TestOpenAIChatVideoURL: the MiniMax/Kimi/GLM video_url convention. The SDK's
// content-part union has no video_url variant, so the part is built through
// param.Override; what matters here is that it reaches the wire.
func TestOpenAIChatVideoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"type":"video_url"`) ||
			!strings.Contains(string(b), `"url":"https://cdn.example.com/v.mp4"`) {
			t.Errorf("body missing video_url\nbody: %s", b)
		}
		openaiEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "video", MIME: "video/mp4", URL: "https://cdn.example.com/v.mp4"}},
	}})
}

// TestOpenAIChatFileURL: GLM's file_url part, likewise carried by param.Override.
func TestOpenAIChatFileURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"type":"file_url"`) ||
			!strings.Contains(string(b), `"url":"https://cdn.example.com/doc.pdf"`) {
			t.Errorf("body missing file_url\nbody: %s", b)
		}
		openaiEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "file", MIME: "application/pdf", URL: "https://cdn.example.com/doc.pdf"}},
	}})
}

// TestAnthropicVideoPart: MiniMax M3's Anthropic-compatible video block. The
// SDK's writable content-block union has no video variant, so it goes out
// through param.Override; the inline base64 source and its media type are what
// this pins.
func TestAnthropicVideoPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{
			`"type":"video"`,
			`"media_type":"video/mp4"`,
			`"data":"aGVsbG8="`,
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		anthropicEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "anthropic", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "video", MIME: "video/mp4", Data: pngB64}},
	}})
}

// TestResponsesVideoURL: the Responses video item, carried by param.Override
// because the SDK's input-content union is input_text/input_image/input_file.
func TestResponsesVideoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{`"type":"input_video"`, `"video_url":"ms://file_xyz"`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		responsesEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai-responses", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "video", MIME: "video/mp4", URL: "ms://file_xyz"}},
	}})
}

// TestResponsesVideoRequiresURL: the Responses video input takes a URL or
// provider file reference, not inline base64.
func TestResponsesVideoRequiresURL(t *testing.T) {
	multimodalChat(t, "openai-responses", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "video", MIME: "video/mp4", Data: pngB64}},
	}}, "requires a url source")
}

func TestResponsesPDFRequiresSource(t *testing.T) {
	multimodalChat(t, "openai-responses", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "file", MIME: "application/pdf"}},
	}}, "requires data or url")
}

func TestResponsesImageAndPDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{
			`"type":"input_image"`,
			`"image_url":"data:image/png;base64,aGVsbG8="`,
			`"type":"input_file"`,
			`"filename":"x.pdf"`,
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("body missing %q\nbody: %s", want, b)
			}
		}
		responsesEmptyReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai-responses", srv, []Message{
		imageTurn(pngB64),
		{Role: "user", Parts: []media.Part{{Type: "file", MIME: "application/pdf", Data: pngB64, Name: "x.pdf"}}},
	})
}

// resetGeminiFileCache isolates cache-sensitive tests from each other (the
// upload cache is process-level, keyed by content sha256).
func resetGeminiFileCache() {
	geminiFileCache.Lock()
	geminiFileCache.m = map[string]geminiFileCacheEntry{}
	geminiFileCache.Unlock()
}

// geminiFilesMock serves the Files API (start -> upload url header, finalize
// -> file resource) plus the interactions endpoint, recording the upload
// start count and the interactions request body.
//
// The interactions route is the path the SDK composes: {BaseURL}/{APIVersion}
// /interactions, with the API version this provider defaults to when base_url
// carries none of its own (see splitGeminiBase). The mock never set a
// Content-Type before, because the hand-rolled client never looked; the SDK
// decodes only a JSON reply and will not read a 200 it cannot classify.
func geminiFilesMock(t *testing.T, gotBody *string, starts *int) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/upload/v1beta/files" && r.Header.Get("X-Goog-Upload-Command") == "start":
			*starts++
			w.Header().Set("X-Goog-Upload-Url", srv.URL+"/upload-session/1")
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/upload-session/"):
			b, _ := io.ReadAll(r.Body)
			if string(b) != "hello" { // pngB64 decodes to "hello"
				t.Errorf("uploaded bytes: %q", b)
			}
			// The SDK's resumable upload waits for the protocol's final status
			// header before it will read the file resource (the hand-rolled client
			// ignored it), so the mock has to speak it.
			w.Header().Set("X-Goog-Upload-Status", "final")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"file":{"name":"files/abc123","uri":"` + srv.URL + `/v1beta/files/abc123","state":"ACTIVE","mimeType":"image/png"}}`))
		case r.URL.Path == "/v1beta/interactions":
			b, _ := io.ReadAll(r.Body)
			*gotBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"a cat"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestGeminiMediaParts(t *testing.T) {
	resetGeminiFileCache()
	var gotBody string
	var starts int
	srv := geminiFilesMock(t, &gotBody, &starts)
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reply, err := m.Chat(context.Background(), "default", []Message{imageTurn(pngB64)}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply.Text != "a cat" {
		t.Fatalf("text %q", reply.Text)
	}
	// The media part was uploaded to the Files API and referenced by URI —
	// never inline base64.
	if starts != 1 {
		t.Errorf("expected 1 files upload, got %d", starts)
	}
	for _, want := range []string{
		`"type":"image"`,
		`"mime_type":"image/png"`,
		`"uri":"` + srv.URL + `/v1beta/files/abc123"`,
		`"type":"text"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q\nbody: %s", want, gotBody)
		}
	}
	if strings.Contains(gotBody, "inline_data") || strings.Contains(gotBody, "aGVsbG8=") {
		t.Errorf("media must not be inline base64\nbody: %s", gotBody)
	}
}

func TestGeminiUploadDeduped(t *testing.T) {
	resetGeminiFileCache()
	var gotBody string
	var starts int
	srv := geminiFilesMock(t, &gotBody, &starts)
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Two chats carrying the same bytes upload once (48h cache window).
	for i := 0; i < 2; i++ {
		if _, err := m.Chat(context.Background(), "default", []Message{imageTurn(pngB64)}, Opts{}); err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
	}
	if starts != 1 {
		t.Fatalf("same content should upload once, got %d uploads", starts)
	}
}

func TestGeminiURLPartDownloaded(t *testing.T) {
	var gotBody string
	var starts int
	srv := geminiFilesMock(t, &gotBody, &starts)
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A url part is downloaded then uploaded to the Files API.
	_, err := m.Chat(context.Background(), "default", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "image", MIME: "image/png", URL: srv.URL + "/img.png"}},
	}}, Opts{})
	// The mock 404s /img.png; the point is the URL path is attempted, not
	// rejected outright.
	if err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("url part should be downloaded (here failing on the mock 404), got %v", err)
	}
}

func TestGeminiPartNoSource(t *testing.T) {
	multimodalChat(t, "gemini", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "image", MIME: "image/png"}},
	}}, "no resolvable source")
}

func TestGeminiTextOnlyStaysString(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// The SDK decodes only a reply it can classify by content type.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"steps":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(gotBody, `"input":"user: hi"`) {
		t.Errorf("text-only input should stay the legacy flattened string\nbody: %s", gotBody)
	}
}

// multimodalChatOn runs a Chat against a caller-provided server (for
// request-body assertions).
func multimodalChatOn(t *testing.T, provider string, srv *httptest.Server, msgs []Message) {
	t.Helper()
	m := NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Chat(context.Background(), "default", msgs, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}
