package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memKV is an in-memory KV for catalog tests.
type memKV struct {
	mu   sync.Mutex
	rows map[string]KVEntry
	now  func() time.Time
}

func newMemKV() *memKV {
	return &memKV{rows: map[string]KVEntry{}, now: time.Now}
}

func (m *memKV) Put(_ context.Context, key, value string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = KVEntry{Key: key, Value: value, ExpiresAt: expiresAt}
	return nil
}

func (m *memKV) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[key]
	if !ok {
		return "", false, nil
	}
	if !e.ExpiresAt.IsZero() && m.now().After(e.ExpiresAt) {
		return "", false, nil
	}
	return e.Value, true, nil
}

func (m *memKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key)
	return nil
}

func (m *memKV) List(_ context.Context, prefix string) ([]KVEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []KVEntry
	for k, e := range m.rows {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	return out, nil
}

func makeTestPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Give enough spatial variation so oversized-image tests produce non-trivial PNGs.
			v := c
			v.R = uint8((int(c.R) + x*7 + y*13) % 256)
			v.G = uint8((int(c.G) + x*11 + y*3) % 256)
			img.SetNRGBA(x, y, v)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHTTPPresignerSignVerifyAndExpiry(t *testing.T) {
	fs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := fs.Put(strings.NewReader("hello-media"), "text/plain", Policy{Allow: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var srv *httptest.Server
	var p *HTTPPresigner
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()

	p, err = NewHTTPPresigner(fs, srv.URL, "/media/t/", "top-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }

	link, err := p.PresignGet(ref.Handle, 5*time.Minute, PresignOptions{
		ContentType: "text/plain",
		Filename:    "note.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, srv.URL+"/media/t/") {
		t.Fatalf("unexpected link %q", link)
	}

	// Valid GET succeeds with safe headers.
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("missing security headers: %v", resp.Header)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="note.txt"`) {
		t.Fatalf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
	}

	// Tampered token is rejected.
	tampered := link[:len(link)-1] + "X"
	if link[len(link)-1] == 'X' {
		tampered = link[:len(link)-1] + "Y"
	}
	resp2, err := http.Get(tampered)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered status = %d, want 403", resp2.StatusCode)
	}

	// Expired token is rejected.
	now = now.Add(6 * time.Minute)
	resp3, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("expired status = %d, want 403", resp3.StatusCode)
	}
}

func TestServiceScopeIsolationAndPurge(t *testing.T) {
	ctx := context.Background()
	fs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kv := newMemKV()
	svc := NewService(ServiceOptions{
		Primary:      fs,
		Catalog:      NewCatalog(kv),
		EnforceScope: true,
	})

	// Alice ingests a confidential file.
	part, err := svc.IngestBytes(ctx, []byte("%PDF-1.7 secret"), IngestOptions{
		Name:   "secret.pdf",
		Scope:  "user:alice",
		Origin: "channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if part.MIME != "application/pdf" || part.Type != "file" || part.Data != "" {
		t.Fatalf("ingested part = %+v", part)
	}

	// Alice can materialize it; Bob is refused.
	if _, err := svc.MaterializeBytes(ctx, "user:alice", part, TargetSpec{}); err != nil {
		t.Fatalf("alice materialize: %v", err)
	}
	if _, err := svc.MaterializeBytes(ctx, "user:bob", part, TargetSpec{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bob materialize = %v, want ErrUnauthorized", err)
	}
	// Maintenance scope bypasses per-tenant checks.
	if _, err := svc.MaterializeBytes(ctx, ScopeMaintenance, part, TargetSpec{}); err != nil {
		t.Fatalf("maintenance materialize: %v", err)
	}

	// Bob independently uploads the exact same bytes (deduped blob, separate grant).
	if _, err := svc.IngestBytes(ctx, []byte("%PDF-1.7 secret"), IngestOptions{
		Scope:  "user:bob",
		Origin: "channel",
	}); err != nil {
		t.Fatal(err)
	}
	// Purging Alice revokes Alice's access, but Bob's grant keeps the blob alive under GC.
	n, err := svc.PurgeScope(ctx, "user:alice")
	if err != nil || n != 1 {
		t.Fatalf("PurgeScope(alice) = (%d, %v)", n, err)
	}
	if _, err := svc.MaterializeBytes(ctx, "user:alice", part, TargetSpec{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("alice after purge = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.MaterializeBytes(ctx, "user:bob", part, TargetSpec{}); err != nil {
		t.Fatalf("bob must still access after alice purge: %v", err)
	}
	live, swept, err := svc.GC(ctx, 0)
	if err != nil || live != 1 || swept != 0 {
		t.Fatalf("GC while bob holds grant = (live=%d, swept=%d, err=%v)", live, swept, err)
	}

	// After Bob is also purged, GC sweeps the blob unless protected by files_meta.
	if _, err := svc.PurgeScope(ctx, "user:bob"); err != nil {
		t.Fatal(err)
	}
	// Protected by extraLive (files_meta):
	live, swept, err = svc.GC(ctx, 0, map[string]bool{part.Handle: true})
	if err != nil || live != 1 || swept != 0 {
		t.Fatalf("GC with files_meta ref = (live=%d, swept=%d, err=%v)", live, swept, err)
	}
	// Unreferenced everywhere:
	live, swept, err = svc.GC(ctx, 0)
	if err != nil || live != 0 || swept != 1 {
		t.Fatalf("GC unreferenced = (live=%d, swept=%d, err=%v)", live, swept, err)
	}
}

func TestServiceTranscodePNGToJPEGAndVariantCache(t *testing.T) {
	ctx := context.Background()
	fs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kv := newMemKV()
	var p *HTTPPresigner
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p, err = NewHTTPPresigner(fs, srv.URL, "/media/t/", "k")
	if err != nil {
		t.Fatal(err)
	}

	// Count transcode invocations to verify variant caching.
	transcodeCalls := 0
	countingTC := transcoderFunc(func(src []byte, srcMIME string, spec TargetSpec) ([]byte, string, error) {
		transcodeCalls++
		return ImageTranscoder{}.Transcode(src, srcMIME, spec)
	})

	svc := NewService(ServiceOptions{
		Primary:      fs,
		Presigner:    p,
		Catalog:      NewCatalog(kv),
		Transcoder:   countingTC,
		EnforceScope: true,
	})

	// Transparent PNG ingested from model output (base64).
	pngBytes := makeTestPNG(t, 120, 90, color.NRGBA{R: 10, G: 20, B: 30, A: 0})
	part, err := svc.IngestBase64(ctx, base64.StdEncoding.EncodeToString(pngBytes), IngestOptions{
		MIME:   "image/png",
		Name:   "chart.png",
		Scope:  "user:alice",
		Origin: "llm",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Target accepts JPEG only (e.g. QQ JPEG preference).
	spec := TargetSpec{
		AcceptMIME: []string{"image/jpeg"},
		PreferMIME: "image/jpeg",
		MaxBytes:   20 << 10,
	}
	link1, err := svc.MaterializeURL(ctx, "user:alice", part, spec)
	if err != nil {
		t.Fatalf("MaterializeURL #1: %v", err)
	}
	// Download the presigned URL and verify it's a valid JPEG with white background.
	resp, err := http.Get(link1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", resp.Header.Get("Content-Type"))
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatalf("decoded JPEG failed: %v", err)
	}
	r0, g0, b0, _ := img.At(0, 0).RGBA()
	if r0 < 0xf000 || g0 < 0xf000 || b0 < 0xf000 {
		t.Fatalf("transparent pixel did not composite onto white: rgb=(%x,%x,%x)", r0, g0, b0)
	}

	// Second call hits the derived-variant cache (no second transcode).
	if _, err := svc.MaterializeURL(ctx, "user:alice", part, spec); err != nil {
		t.Fatalf("MaterializeURL #2: %v", err)
	}
	if transcodeCalls != 1 {
		t.Fatalf("transcodeCalls = %d, want 1 (cached variant)", transcodeCalls)
	}
}

func TestNormalizeBorderConvertsInlineBase64ToHandle(t *testing.T) {
	ctx := context.Background()
	fs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{
		Primary:      fs,
		Catalog:      NewCatalog(newMemKV()),
		EnforceScope: true,
	})
	pngBytes := makeTestPNG(t, 8, 8, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	parts, err := svc.NormalizeBorder(ctx, "user:alice", []Part{
		{Type: "text", Text: "look"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString(pngBytes), Name: "dot.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if parts[1].Data != "" || !ValidHandle(parts[1].Handle) || parts[1].MIME != "image/png" {
		t.Fatalf("normalized part = %+v", parts[1])
	}
	// Scope grant was minted automatically.
	if err := svc.Authorize(ctx, "user:alice", parts[1].Handle); err != nil {
		t.Fatalf("authorize after normalize: %v", err)
	}
}

type transcoderFunc func(src []byte, srcMIME string, spec TargetSpec) ([]byte, string, error)

func (f transcoderFunc) Transcode(src []byte, srcMIME string, spec TargetSpec) ([]byte, string, error) {
	return f(src, srcMIME, spec)
}
