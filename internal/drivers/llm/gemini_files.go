package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
)

// The Gemini Files API (ai.google.dev/gemini-api/docs/files): media is
// uploaded once and referenced by URI in interactions.create, instead of
// inline base64 on every call. Upload uses the resumable protocol:
//
//	POST {origin}/upload/v1beta/files   (X-Goog-Upload-Protocol: resumable,
//	                                     X-Goog-Upload-Command: start)
//	  -> x-goog-upload-url response header
//	POST {upload-url}                    (X-Goog-Upload-Command: upload, finalize,
//	                                     raw bytes)
//	  -> {"file": {"name", "uri", "state", "expirationTime"}}
//
// Files are automatically deleted by Google after 48 hours. A process-level
// cache (content sha256 -> uri, expiry) dedupes repeat uploads of the same
// bytes inside that window. The API key never appears in errors.
type geminiFiles struct {
	uploadURL string // {origin}/upload/v1beta/files
	apiBase   string // {origin}/v1beta — files.get polls go to {apiBase}/{name}
	key       string
	http      *http.Client
}

func newGeminiFiles(base, key string, client *http.Client) *geminiFiles {
	origin := base
	if i := strings.Index(base, "://"); i >= 0 {
		rest := base[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			origin = base[:i+3] + rest[:j]
		}
	}
	return &geminiFiles{
		uploadURL: origin + "/upload/v1beta/files",
		apiBase:   base,
		key:       key,
		http:      client,
	}
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
// the channel policy anyway).
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

	// 1. Start the resumable upload: metadata only, upload URL in a header.
	meta, _ := json.Marshal(map[string]any{"file": map[string]any{"display_name": name}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.uploadURL, bytes.NewReader(meta))
	if err != nil {
		return "", fmt.Errorf("gemini files: build start: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Upload-Protocol", "resumable")
	req.Header.Set("X-Goog-Upload-Command", "start")
	req.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprint(len(data)))
	req.Header.Set("X-Goog-Upload-Header-Content-Type", mime)
	f.auth(req)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini files: start upload: %w", err)
	}
	upURL := resp.Header.Get("X-Goog-Upload-Url")
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return "", geminiFilesError("start upload", resp.StatusCode, b)
	}
	resp.Body.Close()
	if upURL == "" {
		return "", fmt.Errorf("gemini files: start upload returned no upload url")
	}

	// 2. Upload + finalize the bytes.
	ureq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("gemini files: build upload: %w", err)
	}
	ureq.Header.Set("X-Goog-Upload-Offset", "0")
	ureq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	uresp, err := f.http.Do(ureq)
	if err != nil {
		return "", fmt.Errorf("gemini files: upload: %w", err)
	}
	defer uresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(uresp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("gemini files: read upload response: %w", err)
	}
	if uresp.StatusCode/100 != 2 {
		return "", geminiFilesError("upload", uresp.StatusCode, raw)
	}
	var fi struct {
		File struct {
			Name           string `json:"name"` // files/<id>
			URI            string `json:"uri"`
			State          string `json:"state"` // PROCESSING | ACTIVE | FAILED
			ExpirationTime string `json:"expirationTime"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &fi); err != nil {
		return "", fmt.Errorf("gemini files: decode upload response: %w", err)
	}
	if fi.File.URI == "" {
		return "", fmt.Errorf("gemini files: upload returned no file uri")
	}

	// 3. Large files (video especially) are processed asynchronously; poll
	// files.get until ACTIVE.
	state, err := f.awaitActive(ctx, fi.File.Name, fi.File.State)
	if err != nil {
		return "", err
	}
	if state != "ACTIVE" {
		return "", fmt.Errorf("gemini files: file %s is %s, not ACTIVE", fi.File.Name, state)
	}

	expiry := time.Now().Add(47 * time.Hour) // files auto-delete after 48h
	if fi.File.ExpirationTime != "" {
		if t, err := time.Parse(time.RFC3339, fi.File.ExpirationTime); err == nil {
			expiry = t.Add(-time.Hour) // refresh with margin
		}
	}
	geminiCachePut(cacheKey, fi.File.URI, expiry)
	return fi.File.URI, nil
}

// awaitActive polls files.get while a file is PROCESSING (bounded).
func (f *geminiFiles) awaitActive(ctx context.Context, name, state string) (string, error) {
	for i := 0; state == "PROCESSING" && i < 20; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.apiBase+"/"+name, nil)
		if err != nil {
			return "", fmt.Errorf("gemini files: build get: %w", err)
		}
		f.auth(req)
		resp, err := f.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("gemini files: poll: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("gemini files: poll read: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return "", geminiFilesError("poll file", resp.StatusCode, raw)
		}
		var g struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			return "", fmt.Errorf("gemini files: decode poll: %w", err)
		}
		state = g.State
	}
	return state, nil
}

func (f *geminiFiles) auth(req *http.Request) {
	if f.key != "" {
		req.Header.Set("x-goog-api-key", f.key)
	}
}

// geminiFilesError renders a non-2xx without leaking the api key.
func geminiFilesError(op string, status int, body []byte) error {
	return fmt.Errorf("gemini files: %s: status %d: %s", op, status, strings.TrimSpace(string(body)))
}
