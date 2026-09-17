package llm

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// unroutableBaseURL fails fast (connection refused) instead of touching the
// network — these tests stop at resolution and never need a real endpoint.
const unroutableBaseURL = "http://127.0.0.1:1"

// TestChatUnresolvedAPIKey: a model whose api_key is a raw reference that
// cannot be resolved stays configured — config loads — and the first LLM
// call fails with a clear error naming the model and the credential, before
// any network I/O.
func TestChatUnresolvedAPIKey(t *testing.T) {
	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "gpt-x", APIKey: "${UNRESOLVED_MODEL_KEY}", BaseURL: unroutableBaseURL},
	}, testLogger())
	m.SetSecretResolver(func(raw string) (string, bool) {
		return (&config.Resolver{}).Resolve(context.Background(), raw)
	})

	_, _, _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err == nil {
		t.Fatal("chat with unresolvable api_key must fail")
	}
	if !strings.Contains(err.Error(), `"default"`) || !strings.Contains(err.Error(), "UNRESOLVED_MODEL_KEY") {
		t.Fatalf("error must name the model and the credential: %v", err)
	}

	// Once the credential resolves (env set), the call proceeds past
	// resolution — failing later at the transport layer, not here.
	t.Setenv("UNRESOLVED_MODEL_KEY", "sk-now-present")
	_, _, _, err = m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil && strings.Contains(err.Error(), "cannot resolve api_key") {
		t.Fatalf("resolution must succeed once the env var exists: %v", err)
	}
}

// TestChatLiteralKeyWithoutResolver: with no resolver installed, keys are
// literals — pre-existing behavior, byte-identical for the legacy path.
func TestChatLiteralKeyWithoutResolver(t *testing.T) {
	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "gpt-x", APIKey: "sk-literal", BaseURL: unroutableBaseURL},
	}, testLogger())
	_, _, _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil && strings.Contains(err.Error(), "cannot resolve api_key") {
		t.Fatalf("no resolver must never raise resolution errors: %v", err)
	}
}
