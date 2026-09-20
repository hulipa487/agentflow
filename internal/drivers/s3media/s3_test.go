package s3media

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

// mockS3 is a minimal in-memory S3: path-style /{bucket}/{key} with PUT/GET/
// HEAD. It records requests so tests can assert dedupe and signing.
type mockS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	auths   []string
}

func newMockS3(t *testing.T) (*httptest.Server, *mockS3) {
	t.Helper()
	m := &mockS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.auths = append(m.auths, r.Header.Get("Authorization"))
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			m.puts++
			b, _ := io.ReadAll(r.Body)
			m.objects[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, ok := m.objects[key]; ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				// ListObjectsV2: report every object under the prefix.
				// Object keys in this mock include the bucket segment
				// ("media-bucket/agentflow/<sha>"), so qualify the prefix.
				pfx := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/") + "/" + r.URL.Query().Get("prefix")
				var contents []string
				for k := range m.objects {
					if strings.HasPrefix(k, pfx) {
						// Real S3 reports keys without the bucket segment.
						contents = append(contents, fmt.Sprintf(
							"<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-09-21T00:00:00.000Z</LastModified></Contents>",
							strings.TrimPrefix(k, pfx[:len(pfx)-len(r.URL.Query().Get("prefix"))]), len(m.objects[k])))
					}
				}
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
					strings.Join(contents, "") + `</ListBucketResult>`))
				return
			}
			if b, ok := m.objects[key]; ok {
				w.Write(b)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodDelete:
			delete(m.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, m
}

func newStore(t *testing.T, endpoint string) *Store {
	t.Helper()
	s, err := New(config.MediaS3{
		Bucket:    "media-bucket",
		Region:    "us-east-1",
		Endpoint:  endpoint,
		Prefix:    "agentflow",
		AccessKey: "AKID",
		SecretKey: "sekret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(config.MediaS3{}); err == nil {
		t.Fatal("missing bucket/region should fail")
	}
	if _, err := New(config.MediaS3{Bucket: "b", Region: "r"}); err == nil {
		t.Fatal("missing credentials should fail")
	}
}

func TestPutReadRoundTrip(t *testing.T) {
	srv, m := newMockS3(t)
	s := newStore(t, srv.URL)

	ref, err := s.Put(strings.NewReader("photo-bytes"), "image/jpeg", media.Policy{Allow: []string{"image/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref.Handle, "media:") || ref.Size != int64(len("photo-bytes")) {
		t.Fatalf("ref: %+v", ref)
	}
	// Object landed under the prefix (path-style: bucket/key), addressed by
	// content hash.
	sum := strings.TrimPrefix(ref.Handle, "media:")
	if got := string(m.objects["media-bucket/agentflow/"+sum]); got != "photo-bytes" {
		t.Fatalf("object missing at prefixed key: %v", m.objects)
	}
	// Every request carried a SigV4 Authorization header.
	for _, a := range m.auths {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 ") || !strings.Contains(a, "AKID") {
			t.Fatalf("unsigned request: %q", a)
		}
		if strings.Contains(a, "sekret") {
			t.Fatalf("secret must never appear on the wire: %q", a)
		}
	}
	// Round-trip.
	b, err := s.ReadAll(ref.Handle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "photo-bytes" {
		t.Fatalf("read: %q", b)
	}
}

func TestPutDedupes(t *testing.T) {
	srv, m := newMockS3(t)
	s := newStore(t, srv.URL)

	body := func() io.Reader { return strings.NewReader("same-content") }
	if _, err := s.Put(body(), "text/plain", media.Policy{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(body(), "text/plain", media.Policy{}); err != nil {
		t.Fatal(err)
	}
	if m.puts != 1 {
		t.Fatalf("same content should PUT once (HEAD dedupe), got %d", m.puts)
	}
}

func TestPutEnforcesLimit(t *testing.T) {
	srv, _ := newMockS3(t)
	s := newStore(t, srv.URL)
	_, err := s.Put(strings.NewReader("0123456789"), "text/plain", media.Policy{MaxBytes: 4, Allow: []string{"*"}})
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadAllMissing(t *testing.T) {
	srv, _ := newMockS3(t)
	s := newStore(t, srv.URL)
	_, err := s.ReadAll("media:"+strings.Repeat("ab", 32), 1<<20)
	if err == nil {
		t.Fatal("missing object should error")
	}
}

func TestReadAllRejectsBadHandle(t *testing.T) {
	srv, _ := newMockS3(t)
	s := newStore(t, srv.URL)
	if _, err := s.ReadAll("../etc/passwd", 1<<20); err == nil {
		t.Fatal("malformed handle should be rejected before any request")
	}
}

// TestListDelete: List enumerates blobs (handle, size, modtime) under the
// prefix and Delete removes them — the GC primitives.
func TestListDelete(t *testing.T) {
	srv, _ := newMockS3(t)
	st := newStore(t, srv.URL)

	r1, err := st.Put(strings.NewReader("alpha"), "text/plain", media.Policy{Allow: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(strings.NewReader("beta"), "text/plain", media.Policy{Allow: []string{"*"}}); err != nil {
		t.Fatal(err)
	}

	blobs, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 2 {
		t.Fatalf("List() = %d blobs, want 2", len(blobs))
	}
	seen := map[string]media.Blob{}
	for _, b := range blobs {
		seen[b.Handle] = b
		if b.ModTime.IsZero() {
			t.Fatalf("blob %s must carry LastModified", b.Handle)
		}
	}
	if seen[r1.Handle].Size != 5 {
		t.Fatalf("sizes wrong: %+v", seen)
	}

	if err := st.Delete(r1.Handle); err != nil {
		t.Fatal(err)
	}
	blobs, _ = st.List()
	if len(blobs) != 1 || blobs[0].Handle == r1.Handle {
		t.Fatalf("after delete: %v", blobs)
	}
}
