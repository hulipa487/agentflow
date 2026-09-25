package llm

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/media"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

// Embed computes embedding vectors for the given input texts with the named
// model. Any OpenAI-compatible endpoint works (OpenAI, Ollama /v1, vLLM,
// TEI, LiteLLM, OpenRouter): the driver calls {base_url}/embeddings.
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
	cfg, err := m.resolveModel(model)
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
// base_url should include /v1 (same convention as chat); the runtime calls the
// SDK's embeddings path relative to it. Text-only batches send plain strings;
// media items become Jina typed docs ({"image": "<url|base64>"}, ...). With
// Merged, the whole batch folds into one {"content": [...]} group yielding
// a single embedding. task/dimensions pass through when set.
//
// The wire is owned by the official SDK (request encoding, the retry knobs,
// response decoding and its error typing). The Jina extension is the one part
// the SDK cannot model: EmbeddingNewParamsInputUnion covers strings and token
// arrays only, and there is no `task` field. So the vanilla OpenAI case goes
// through the typed fields, and the extension's `input`/`task` ride in the
// params' extra-fields — which is how the SDK documents escaping its own
// schema. The SDK's own retries are disabled: the Manager's doWithRetry owns
// that policy.
func openaiEmbed(ctx context.Context, client *http.Client, cfg config.Model, parts []media.Part, eo EmbedOpts) ([][]float32, Usage, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/embeddings"

	input, err := embedInputItems(parts, eo)
	if err != nil {
		return nil, Usage{}, false, err
	}
	params := openai.EmbeddingNewParams{Model: openai.EmbeddingModel(cfg.Model)}
	extras := map[string]any{}
	if texts, ok := input.([]string); ok {
		params.Input = openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts}
	} else {
		extras["input"] = input
	}
	if eo.Task != "" {
		extras["task"] = eo.Task
	}
	if eo.Dimensions > 0 {
		params.Dimensions = param.NewOpt(int64(eo.Dimensions))
	}
	if len(extras) > 0 {
		params.SetExtraFields(extras)
	}

	sdkOpts := []option.RequestOption{
		option.WithBaseURL(base),
		option.WithHTTPClient(client),
		option.WithMaxRetries(0),
	}
	sdkOpts = append(sdkOpts, openaiAuthOptions(cfg.APIKey)...)
	api := openai.NewClient(sdkOpts...)

	resp, err := api.Embeddings.New(ctx, params)
	if err != nil {
		return nil, Usage{}, openaiRetryable(err), fmt.Errorf("%s -> %w", url, err)
	}
	if len(resp.Data) == 0 {
		return nil, Usage{}, false, fmt.Errorf("%s -> embeddings response carried no data", url)
	}
	// The API is allowed to return entries out of order; index is canonical.
	sort.Slice(resp.Data, func(i, j int) bool { return resp.Data[i].Index < resp.Data[j].Index })
	vectors := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		if len(d.Embedding) == 0 {
			return nil, Usage{}, false, fmt.Errorf("%s -> embedding %d is empty", url, d.Index)
		}
		// The SDK models a vector as []float64 (that is what the API sends);
		// the driver's contract is []float32, so it is narrowed here.
		v := make([]float32, len(d.Embedding))
		for j, f := range d.Embedding {
			v[j] = float32(f)
		}
		vectors[i] = v
	}
	var usage Usage
	if resp.Usage.PromptTokens > 0 {
		usage.Input = int(resp.Usage.PromptTokens)
	} else {
		usage.Input = int(resp.Usage.TotalTokens)
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
		m.log.Debug("llm: request", "op", what, "provider", cfg.Provider, "model", cfg.Model, "attempt", attempt)
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
