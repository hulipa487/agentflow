package main

import (
	"strings"
	"testing"
)

func TestRedactRemovesCredentials(t *testing.T) {
	body := `{
		"model": "coder",
		"api_key": "sk-live-abcdefghijklmnop",
		"messages": [{"role":"user","content":"use sk-proj-abcdefghijklmnopqrstuvwxyz please"}],
		"nested": {"authorization": "Bearer abcdefghijklmnop"}
	}`
	out := redact([]byte(body))
	for _, leaked := range []string{"sk-live", "sk-proj", "abcdefghijklmnop"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("redacted body still contains %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, `"model":"coder"`) {
		t.Fatalf("redaction damaged the request: %s", out)
	}
}

func TestRedactTextHandlesSSE(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"key sk-proj-abcdefghijklmnop\"}}]}\n\ndata: [DONE]\n\n"
	out := redactText(in)
	if strings.Contains(out, "sk-proj") {
		t.Fatalf("SSE body kept a key: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("SSE framing was damaged: %s", out)
	}
}

func TestSlug(t *testing.T) {
	if got := slug("POST /v1/chat/completions?x=1"); got != "post-v1-chat-completions-x-1" {
		t.Fatalf("slug = %q", got)
	}
	if got := slug("///"); got != "request" {
		t.Fatalf("slug empty case = %q", got)
	}
}
