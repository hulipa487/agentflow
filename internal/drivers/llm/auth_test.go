package llm

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentflow/internal/config"
)

// The runtime resolves credentials itself: a model's api_key is either a
// literal or a ${VAR}/cred: reference the Manager expands before the provider
// sees it. The official SDKs, though, autoload credentials from the process
// environment (openai.DefaultClientOptions reads OPENAI_API_KEY; the Anthropic
// client has the same behaviour), and the hand-rolled clients this replaced
// read nothing. That difference matters in one direction: a model entry with no
// api_key pointing at a third-party base_url would send the deployment's real
// provider key to that host. These two tests pin both halves.

// recordAuth captures the credential headers of the last request.
type recordAuth struct {
	auth string
	org  string
	key  string // x-api-key (anthropic)
}

func authRecorder(t *testing.T, provider string, seen *recordAuth) *httptest.Server {
	t.Helper()
	// The canned reply has to be a stream the provider's SDK can decode, so it
	// varies by provider. Every assertion here is on the request headers.
	reply := openaiEmptyReply()
	if provider == "anthropic" {
		reply = anthropicEmptyReply()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.auth = r.Header.Get("Authorization")
		seen.org = r.Header.Get("OpenAI-Organization")
		seen.key = r.Header.Get("x-api-key")
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// keylessManager builds a manager for one model with no api_key. It does not
// use testManager, which sets one: this test is about the absence of a key.
func keylessManager(baseURL, provider string) *Manager {
	return NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: baseURL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestProviderDoesNotInheritEnvCredentials: an empty api_key must send no
// credential at all, even when the environment carries one.
func TestProviderDoesNotInheritEnvCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")
	t.Setenv("OPENAI_ORG_ID", "org-from-the-environment")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-the-environment")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-from-the-environment")

	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var seen recordAuth
			srv := authRecorder(t, provider, &seen)
			m := keylessManager(srv.URL, provider)
			if _, err := m.Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if seen.auth != "" {
				t.Errorf("Authorization %q was sent for a model with no api_key: the process environment leaked into the request", seen.auth)
			}
			if seen.org != "" {
				t.Errorf("OpenAI-Organization %q was sent for a model with no api_key", seen.org)
			}
			if seen.key != "" {
				t.Errorf("x-api-key %q was sent for a model with no api_key: the process environment leaked into the request", seen.key)
			}
		})
	}
}

// TestProviderSendsConfiguredKey is the other half: a configured key still
// reaches the provider, and wins over whatever the environment holds.
func TestProviderSendsConfiguredKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-the-environment")

	cases := []struct {
		provider string
		apiKey   string
		wantAuth string
		wantKey  string
	}{
		{provider: "openai", apiKey: "sk-configured", wantAuth: "Bearer sk-configured"},
		{provider: "anthropic", apiKey: "sk-ant-configured", wantKey: "sk-ant-configured"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			var seen recordAuth
			srv := authRecorder(t, tc.provider, &seen)
			m := NewManager(map[string]config.Model{
				"default": {Provider: tc.provider, Model: "m", BaseURL: srv.URL, APIKey: tc.apiKey},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if _, err := m.Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if seen.auth != tc.wantAuth {
				t.Errorf("Authorization %q; want %q", seen.auth, tc.wantAuth)
			}
			if seen.key != tc.wantKey {
				t.Errorf("x-api-key %q; want %q", seen.key, tc.wantKey)
			}
		})
	}
}
