package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"agentflow/internal/core/media"

	"google.golang.org/genai"
)

// The Gemini Files API (ai.google.dev/gemini-api/docs/files): media is
// uploaded once and referenced by URI in interactions.create, instead of
// inline base64 on every call.
//
// The upload itself is the official SDK's (genai.Client.Files.Upload), which
// speaks the same resumable protocol the hand-rolled client did:
//
//	POST {BaseURL}/upload/v1beta/files   (X-Goog-Upload-Protocol: resumable,
//	                                      X-Goog-Upload-Command: start)
//	  -> x-goog-upload-url response header
//	POST {upload-url}                     (X-Goog-Upload-Command: upload, finalize,
//	                                      raw bytes)
//	  -> {"file": {"name", "uri", "state", "expirationTime"}}
//
// and files.get polling goes through genai.Client.Files.Get. What stays here is
// the part the SDK does not offer: downloading a url source, the content-hash
// dedupe and the wait for a file to become ACTIVE. Files are automatically
// deleted by Google after 48 hours, which is the dedupe window, and the key
// rides in the client the SDK was given rather than in any error this file
// builds.
type geminiFiles struct {
	client *genai.Client
	key    string       // cache-key namespace: uploads are per-api-key
	http   *http.Client // url-source downloads; the SDK never fetches those
}

func newGeminiFiles(client *genai.Client, key string, hc *http.Client) *geminiFiles {
	return &geminiFiles{client: client, key: key, http: hc}
}

// geminiFileCacheEntry is one uploaded file's URI plus its server expiry.
type geminiFileCacheEntry struct {
	uri    string
	expiry time.Time
}

var geminiFileCache = struct {
	sync.Mutex
	m map[string]geminiFileCacheEntry
}{m: map[string]geminiFileCacheEntry{}}

func geminiCacheGet(k string) (string, bool) {
	geminiFileCache.Lock()
	defer geminiFileCache.Unlock()
	e, ok := geminiFileCache.m[k]
	if !ok || time.Now().After(e.expiry) {
		return "", false
	}
	return e.uri, true
}

func geminiCachePut(k, uri string, expiry time.Time) {
	geminiFileCache.Lock()
	defer geminiFileCache.Unlock()
	geminiFileCache.m[k] = geminiFileCacheEntry{uri: uri, expiry: expiry}
}

// reference resolves a media part to a Files API URI: bytes come from inline
// base64 (data) or are downloaded (url), then uploaded (deduped by content).
func (f *geminiFiles) reference(ctx context.Context, p media.Part, mime string) (string, error) {
	var data []byte
	switch {
	case p.Data != "":
		b, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return "", fmt.Errorf("gemini: %s part has invalid base64 data: %w", p.Type, err)
		}
		data = b
	case p.URL != "":
		b, err := f.download(ctx, p.URL)
		if err != nil {
			return "", fmt.Errorf("gemini: %s part download failed: %w", p.Type, err)
		}
		data = b
	default:
		return "", fmt.Errorf("gemini: %s part has no resolvable source (need data or url)", p.Type)
	}
	return f.upload(ctx, data, mime, p.Name)
}

// download fetches a URL source for upload, capped at the Files API's 2 GB
// per-file ceiling (kept far lower here: media over the bridge is bounded by
// the channel policy anyway). The SDK only uploads readers it is handed, so the
// fetch is ours.
func (f *geminiFiles) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// upload sends bytes to the Files API (or returns the cached URI for content
// uploaded within the last 48 hours) and waits for the file to be ACTIVE.
func (f *geminiFiles) upload(ctx context.Context, data []byte, mime, name string) (string, error) {
	sum := sha256.Sum256(data)
	cacheKey := f.key + ":" + hex.EncodeToString(sum[:])
	if uri, ok := geminiCacheGet(cacheKey); ok {
		return uri, nil
	}
	if name == "" {
		name = hex.EncodeToString(sum[:8])
	}

	file, err := f.client.Files.Upload(ctx, bytes.NewReader(data), &genai.UploadFileConfig{
		MIMEType:    mime,
		DisplayName: name,
	})
	if err != nil {
		return "", fmt.Errorf("gemini files: upload: %w", err)
	}
	if file.URI == "" {
		return "", fmt.Errorf("gemini files: upload returned no file uri")
	}

	// Large files (video especially) are processed asynchronously; poll
	// files.get until ACTIVE.
	state, err := f.awaitActive(ctx, file.Name, file.State)
	if err != nil {
		return "", err
	}
	if state != genai.FileStateActive {
		return "", fmt.Errorf("gemini files: file %s is %s, not ACTIVE", file.Name, state)
	}

	expiry := time.Now().Add(47 * time.Hour) // files auto-delete after 48h
	if !file.ExpirationTime.IsZero() {
		expiry = file.ExpirationTime.Add(-time.Hour) // refresh with margin
	}
	geminiCachePut(cacheKey, file.URI, expiry)
	return file.URI, nil
}

// awaitActive polls files.get while a file is PROCESSING (bounded).
func (f *geminiFiles) awaitActive(ctx context.Context, name string, state genai.FileState) (genai.FileState, error) {
	for i := 0; state == genai.FileStateProcessing && i < 20; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		file, err := f.client.Files.Get(ctx, name, nil)
		if err != nil {
			return "", fmt.Errorf("gemini files: poll: %w", err)
		}
		state = file.State
	}
	return state, nil
}
