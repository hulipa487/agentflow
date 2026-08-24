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
	"agentflow/internal/core/media"
)

// Embed computes embedding vectors for the given input texts with the named
// model. Any OpenAI-compatible endpoint works (OpenAI, Ollama /v1, vLLM,
// TEI, LiteLLM, OpenRouter): the driver POSTs {base_url}/embeddings.
// The anthropic provider has no embeddings API and returns an explicit
// unsupported error rather than a confusing transport failure.
func (m *Manager) Embed(ctx context.Context, model string, texts []string) ([][]float32, Usage, error) {
	parts := make([]media.Part, len(texts))
	for i, t := range texts {
		parts[i] = media.Part{Type: "text", Text: t}
	}
	return m.EmbedParts(ctx, model, parts, EmbedOpts{})
}

// EmbedOpts carries the multimodal embedding request options from the loop:
// Task selects the provider task adapter (Jina: retrieval.query,
// retrieval.passage, text-matching, clustering, classification), Dimensions
// is a Matryoshka truncation, and Merged folds every input into ONE
// embedding (Jina MergedContentGroup) instead of one per input.
type EmbedOpts struct {
	Task       string
	Dimensions int
	Merged     bool
}

// EmbedParts embeds a list of input parts: text parts (Type "text") and
// media parts (image / video / audio / pdf). Text-only batches serialize as
// plain strings — byte-identical to the vanilla OpenAI Embeddings request.
// Batches carrying media serialize each item as a Jina-style typed doc
// ({"text"|"image"|"video"|"audio"|"pdf": "<url|base64>"}), the convention
// used by jina-embeddings-v5-omni (text+image+video+audio+pdf in one vector
// space) and jina-clip-v2. A nil/zero store on a handle part is resolved by
// the caps layer before this point; here Data must already be inline.
func (m *Manager) EmbedParts(ctx context.Context, model string, parts []media.Part, eo EmbedOpts) ([][]float32, Usage, error) {
	cfg, err := m.resolve(model)
	if err != nil {
		return nil, Usage{}, err
	}
	if len(parts) == 0 {
		return nil, Usage{}, fmt.Errorf("embed: at least one input is required")
	}
	var vectors [][]float32
	var usage Usage
	err = m.doWithRetry(ctx, cfg, "embed", func(ctx context.Context) (bool, error) {
		v, u, retryable, err := openEmbed(ctx, m.http, cfg, parts, eo)
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
func openEmbed(ctx context.Context, client *http.Client, cfg config.Model, parts []media.Part, eo EmbedOpts) ([][]float32, Usage, bool, error) {
	switch cfg.Provider {
	case "openai", "openai-responses":
		return openaiEmbed(ctx, client, cfg, parts, eo)
	case "anthropic":
		return nil, Usage{}, false, fmt.Errorf("provider %q has no embeddings API; configure an openai-compatible embedding model (Ollama, vLLM, TEI, ...)", cfg.Provider)
	}
	return nil, Usage{}, false, fmt.Errorf("provider %q does not support embeddings", cfg.Provider)
}

// openaiEmbed implements the OpenAI Embeddings API plus the Jina multimodal
// extension used by jina-embeddings-v5-omni / jina-clip-v2.
// https://platform.openai.com/docs/api-reference/embeddings/create
//
// base_url should include /v1 (same convention as chat); the runtime
// appends only /embeddings. Text-only batches send plain strings; media
// items become Jina typed docs ({"image": "<url|base64>"}, ...). With
// Merged, the whole batch folds into one {"content": [...]} group yielding
// a single embedding. task/dimensions pass through when set.
func openaiEmbed(ctx context.Context, client *http.Client, cfg config.Model, parts []media.Part, eo EmbedOpts) ([][]float32, Usage, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/embeddings"

	input, err := embedInputItems(parts, eo)
	if err != nil {
		return nil, Usage{}, false, err
	}
	body := map[string]any{
		"model": cfg.Model,
		"input": input,
	}
	if eo.Task != "" {
		body["task"] = eo.Task
	}
	if eo.Dimensions > 0 {
		body["dimensions"] = eo.Dimensions
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

// embedInputItems serializes the input parts for the /embeddings request.
//
// Text-only batches stay plain strings so vanilla OpenAI/Ollama/vLLM see the
// exact historical request. Any media part switches the batch to Jina typed
// docs: {"text": ...} / {"image"|"video"|"audio"|"pdf": "<url|base64>"} —
// the api.jina.ai convention shared by jina-embeddings-v5-omni-* (text,
// image, video, audio, pdf in one vector space) and jina-clip-v2. Media
// sources are URL or raw base64 (no data: prefix). PDFs are single-input
// only per the Jina schema. Merged folds all inputs into one
// {"content": [...]} group (MergedContentGroup) yielding a single embedding.
func embedInputItems(parts []media.Part, eo EmbedOpts) (any, error) {
	allText := true
	for _, p := range parts {
		if p.Type != "text" {
			allText = false
			break
		}
	}
	if allText && !eo.Merged {
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = p.Text
		}
		return out, nil
	}

	docs, err := embedDocs(parts)
	if err != nil {
		return nil, err
	}
	if eo.Merged {
		return map[string]any{"content": docs}, nil
	}
	return docs, nil
}

// embedDocs maps each part to one Jina typed doc object.
func embedDocs(parts []media.Part) ([]map[string]any, error) {
	hasPDF := false
	for _, p := range parts {
		if p.Type == "file" && p.MIME == "application/pdf" {
			hasPDF = true
			break
		}
	}
	if hasPDF && len(parts) > 1 {
		return nil, fmt.Errorf("embed: pdf input is single-item only (Jina PDFDoc cannot appear in a list)")
	}

	docs := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		src := p.URL
		if src == "" {
			src = p.Data // raw base64, no data: prefix (Jina convention)
		}
		switch {
		case p.Type == "text" || p.Type == "":
			if p.Text != "" {
				docs = append(docs, map[string]any{"text": p.Text})
			}
		case p.Type == "image":
			if src == "" {
				return nil, fmt.Errorf("embed: image part has neither url nor data")
			}
			docs = append(docs, map[string]any{"image": src})
		case p.Type == "video":
			if src == "" {
				return nil, fmt.Errorf("embed: video part has neither url nor data")
			}
			docs = append(docs, map[string]any{"video": src})
		case p.Type == "audio":
			if src == "" {
				return nil, fmt.Errorf("embed: audio part has neither url nor data")
			}
			docs = append(docs, map[string]any{"audio": src})
		case p.Type == "file" && p.MIME == "application/pdf":
			if src == "" {
				return nil, fmt.Errorf("embed: pdf part has neither url nor data")
			}
			docs = append(docs, map[string]any{"pdf": src})
		default:
			return nil, fmt.Errorf("embed: part type %q (mime %q) has no embeddings doc form", p.Type, p.MIME)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("embed: no embeddable content in input")
	}
	return docs, nil
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
