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
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, _, _, err := m.Chat(context.Background(), "default", msgs, Opts{})
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

// simpleReply serves a minimal non-streaming OK response; the assertions in
// these tests run on the request body via assertBody.
func simpleReply() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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
		simpleReply()(w, r)
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
		simpleReply()(w, r)
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
		simpleReply()(w, r)
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
		simpleReply()(w, r)
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
		simpleReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai", srv, []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "audio", MIME: "audio/wav", Data: pngB64}},
	}})
}

func TestOpenAIChatPDFRejected(t *testing.T) {
	multimodalChat(t, "openai", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "file", MIME: "application/pdf", Data: pngB64}},
	}}, "does not support file parts")
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
		simpleReply()(w, r)
	}))
	defer srv.Close()
	multimodalChatOn(t, "openai-responses", srv, []Message{
		imageTurn(pngB64),
		{Role: "user", Parts: []media.Part{{Type: "file", MIME: "application/pdf", Data: pngB64, Name: "x.pdf"}}},
	})
}

func TestResponsesVideoRejected(t *testing.T) {
	multimodalChat(t, "openai-responses", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "video", MIME: "video/mp4", Data: pngB64}},
	}}, "does not support video parts")
}

func TestGeminiMediaParts(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"a cat"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text, _, _, err := m.Chat(context.Background(), "default", []Message{imageTurn(pngB64)}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if text != "a cat" {
		t.Fatalf("text %q", text)
	}
	for _, want := range []string{
		`"type":"inline_data"`,
		`"mime_type":"image/png"`,
		`"data":"aGVsbG8="`,
		`"type":"text"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q\nbody: %s", want, gotBody)
		}
	}
}

func TestGeminiTextOnlyStaysString(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"steps":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(gotBody, `"input":"user: hi"`) {
		t.Errorf("text-only input should stay the legacy flattened string\nbody: %s", gotBody)
	}
}

func TestGeminiURLPartRejected(t *testing.T) {
	multimodalChat(t, "gemini", []Message{{
		Role:  "user",
		Parts: []media.Part{{Type: "image", MIME: "image/png", URL: "https://example.com/x.png"}},
	}}, "requires inline base64 data")
}

// multimodalChatOn runs a Chat against a caller-provided server (for
// request-body assertions).
func multimodalChatOn(t *testing.T, provider string, srv *httptest.Server, msgs []Message) {
	t.Helper()
	m := NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, _, err := m.Chat(context.Background(), "default", msgs, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}
