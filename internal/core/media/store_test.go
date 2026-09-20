package media

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestStorePutOpenDedupe(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pol := Policy{Allow: []string{"image/*"}}

	ref, err := s.Put(strings.NewReader("hello image"), "image/png", pol)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidHandle(ref.Handle) {
		t.Fatalf("bad handle %q", ref.Handle)
	}
	if ref.Size != int64(len("hello image")) {
		t.Fatalf("size %d", ref.Size)
	}

	// Same content dedupes to the same handle without error.
	ref2, err := s.Put(strings.NewReader("hello image"), "image/png", pol)
	if err != nil || ref2.Handle != ref.Handle {
		t.Fatalf("dedupe failed: %v %v", ref2, err)
	}

	b, err := s.ReadAll(ref.Handle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello image" {
		t.Fatalf("content %q", b)
	}
}

func TestStoreRejectsMalformedHandle(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"../../etc/passwd", "media:abc", "media:ZZ" + strings.Repeat("0", 61), ""} {
		if _, err := s.OpenFile(h); err == nil {
			t.Fatalf("expected error for handle %q", h)
		}
	}
	// The store dir must not be reachable through a "valid-looking" prefix.
	if _, err := s.OpenFile("media:" + strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestStoreEnforcesLimit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pol := Policy{MaxBytes: 4, Allow: []string{"*"}}
	_, err = s.Put(bytes.NewReader(make([]byte, 5)), "application/pdf", pol)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	// No temp leftovers.
	ents, _ := os.ReadDir(s.dir)
	if len(ents) != 0 {
		t.Fatalf("expected clean dir, got %d entries", len(ents))
	}
}

func TestPolicyAllows(t *testing.T) {
	p := Policy{Allow: []string{"image/*", "application/pdf"}}
	cases := []struct {
		mime string
		want bool
	}{
		{"image/png", true},
		{"IMAGE/JPEG", true},
		{"application/pdf", true},
		{"video/mp4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := p.Allows(c.mime); got != c.want {
			t.Fatalf("Allows(%q) = %v, want %v", c.mime, got, c.want)
		}
	}
	if (Policy{}).Allows("image/png") {
		t.Fatal("zero policy must deny")
	}
	if !(Policy{Allow: []string{"*"}}).Allows("video/mp4") {
		t.Fatal("* must allow everything")
	}
}

func TestClassify(t *testing.T) {
	for mime, want := range map[string]string{
		"image/png":       "image",
		"audio/ogg":       "audio",
		"video/mp4":       "video",
		"application/pdf": "file",
		"":                "file",
	} {
		if got := Classify(mime); got != want {
			t.Fatalf("Classify(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestFSListDelete: List reports every blob with size and mtime; Delete
// removes a blob and treats a missing one as success (idempotent GC).
func TestFSListDelete(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.Put(strings.NewReader("alpha"), "text/plain", Policy{Allow: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Put(strings.NewReader("beta"), "text/plain", Policy{Allow: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 2 {
		t.Fatalf("List() = %d blobs, want 2", len(blobs))
	}
	byHandle := map[string]Blob{}
	for _, b := range blobs {
		byHandle[b.Handle] = b
		if b.ModTime.IsZero() {
			t.Fatalf("blob %s must report an mtime", b.Handle)
		}
	}
	if byHandle[r1.Handle].Size != 5 || byHandle[r2.Handle].Size != 4 {
		t.Fatalf("sizes wrong: %+v", byHandle)
	}
	if err := s.Delete(r1.Handle); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(r1.Handle); err != nil {
		t.Fatalf("deleting a missing blob must be a no-op: %v", err)
	}
	blobs, _ = s.List()
	if len(blobs) != 1 || blobs[0].Handle != r2.Handle {
		t.Fatalf("after delete: %v", blobs)
	}
	if err := s.Delete("bogus"); err == nil {
		t.Fatal("malformed handle must fail delete")
	}
}
