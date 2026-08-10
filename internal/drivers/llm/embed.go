package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"agentflow/internal/config"
)

// Embed computes embedding vectors for the given inputs with the named
// model. Any OpenAI-compatible endpoint works (OpenAI, Ollama /v1, vLLM,
// TEI, LiteLLM, OpenRouter): the driver POSTs {base_url}/embeddings.
// The anthropic provider has no embeddings API and returns an explicit
// unsupported error rather than a confusing transport failure.
func (m *Manager) Embed(ctx context.Context, model string, inputs []string) ([][]float32, Usage, error) {
	cfg, err := m.resolve(model)
	if err != nil {
		return nil, Usage{}, err
	}
	if len(inputs) == 0 {
		return nil, Usage{}, fmt.Errorf("embed: at least one input text is required")
	}
	var vectors [][]float32
	var usage Usage
	err = m.doWithRetry(ctx, cfg, "embed", func(ctx context.Context) (bool, error) {
		v, u, retryable, err := openEmbed(ctx, m.http, cfg, inputs)
		if err != nil {
			return retryable, err
		}
		vectors, usage = v, u
		return false, nil
	})
	if err != nil {
		return nil, usage, err
	}
	return vectors, usage, nil
}

// openEmbed routes an embeddings request to the provider implementation.
// The bool marks the error retryable (transport failures, 429, 5xx).
func openEmbed(ctx context.Context, client *http.Client, cfg config.Model, inputs []string) ([][]float32, Usage, bool, error) {
	switch cfg.Provider {
	case "openai", "openai-responses":
		return openaiEmbed(ctx, client, cfg, inputs)
	case "anthropic":
		return nil, Usage{}, false, fmt.Errorf("provider %q has no embeddings API; configure an openai-compatible embedding model (Ollama, vLLM, TEI, ...)", cfg.Provider)
	}
	return nil, Usage{}, false, fmt.Errorf("provider %q does not support embeddings", cfg.Provider)
}

// openaiEmbed implements the OpenAI Embeddings API.
// https://platform.openai.com/docs/api-reference/embeddings/create
//
// base_url should include /v1 (same convention as chat); the runtime
// appends only /embeddings.
func openaiEmbed(ctx context.Context, client *http.Client, cfg config.Model, inputs []string) ([][]float32, Usage, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/embeddings"

	body := map[string]any{
		"model": cfg.Model,
		"input": inputs,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return nil, Usage{}, false, err
	}
	req.Header.Set("content-type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, Usage{}, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryable, serr := statusError(resp.StatusCode, b)
		return nil, Usage{}, retryable, fmt.Errorf("%s -> %w", url, serr)
	}

	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&parsed); err != nil {
		return nil, Usage{}, false, fmt.Errorf("%s -> decode embeddings response: %w", url, err)
	}
	if len(parsed.Data) == 0 {
		return nil, Usage{}, false, fmt.Errorf("%s -> embeddings response carried no data", url)
	}
	// The API is allowed to return entries out of order; index is canonical.
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	vectors := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) == 0 {
			return nil, Usage{}, false, fmt.Errorf("%s -> embedding %d is empty", url, d.Index)
		}
		vectors[i] = d.Embedding
	}
	var usage Usage
	if parsed.Usage != nil {
		usage.Input = parsed.Usage.PromptTokens
		if usage.Input == 0 {
			usage.Input = parsed.Usage.TotalTokens
		}
	}
	return vectors, usage, false, nil
}

// doWithRetry runs fn with the same policy as stream establishment: retry
// retryable failures (transport, 429, 5xx) with exponential backoff, per
// attempt bounded by the model's configured timeout.
func (m *Manager) doWithRetry(ctx context.Context, cfg config.Model, what string, fn func(context.Context) (bool, error)) error {
	retry := cfg.Retry
	if retry < 0 {
		retry = 0
	}
	var last error
	for attempt := 0; attempt <= retry; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(500<<uint(attempt-1)) * time.Millisecond
			m.log.Warn("llm: retrying", "op", what, "model", cfg.Model, "attempt", attempt, "backoff", backoff, "err", last)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, cfg.TimeoutD())
		retryable, err := fn(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if !retryable {
			return err
		}
	}
	return last
}
