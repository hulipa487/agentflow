// Package media holds the multimodal part type and the content-addressed
// blob store that backs it.
//
// Design rule: raw media bytes never cross the Lua bridge and never sit in a
// mailbox. Channels land inbound media in the Store and reference it by
// Handle ("media:<sha256>"); the llm caps layer resolves handles to base64
// just before the provider request is built. Loops only ever see the small
// JSON-safe Part descriptors.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Part is one content part of a message turn or attachment. Exactly one of
// the media sources is set for non-text parts: Data (small inline base64),
// URL (remote reference), or Handle (blob-store reference). Text parts set
// Text only.
//
// Part crosses the Lua bridge as a plain JSON table, so loops can construct
// and forward parts freely.
type Part struct {
	Type string `json:"type"` // "text" | "image" | "audio" | "video" | "file"
	Text string `json:"text,omitempty"`

	MIME   string `json:"mime,omitempty"`   // e.g. "image/png", "application/pdf"
	Data   string `json:"data,omitempty"`   // base64; small inline media
	URL    string `json:"url,omitempty"`    // remote reference (provider or core fetches)
	Handle string `json:"handle,omitempty"` // "media:<sha256hex>" blob-store ref

	Name string `json:"name,omitempty"` // filename for files (pdfs, ...)
}

// Classify maps a MIME type to a Part type. Unknown types become "file".
func Classify(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	case strings.HasPrefix(m, "video/"):
		return "video"
	default:
		return "file"
	}
}

// ErrTooLarge is returned when media exceeds the configured ceiling.
var ErrTooLarge = errors.New("media exceeds size limit")

// handleRe pins the handle format: media:<64 lowercase hex>. Anything else is
// rejected before it ever reaches the filesystem, so a forged handle cannot
// traverse paths.
var handleRe = regexp.MustCompile(`^media:[0-9a-f]{64}$`)

// ValidHandle reports whether h is a well-formed blob-store handle.
func ValidHandle(h string) bool { return handleRe.MatchString(h) }

// Policy gates media ingestion on a channel. Zero value (or empty Allow)
// means media is not ingested: captions/text still flow, media is dropped.
type Policy struct {
	MaxBytes int64    // per-file ceiling; 0 = defaultMaxBytes when Allow is set
	Allow    []string // MIME patterns ("image/png", "image/*", "application/pdf")
}

const defaultMaxBytes = 8 << 20 // 8 MiB

func (p Policy) maxBytes() int64 {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return defaultMaxBytes
}

// Enabled reports whether this policy admits any media at all.
func (p Policy) Enabled() bool { return len(p.Allow) > 0 }

// Allows reports whether mime passes the allow list. Patterns match exactly
// or by "type/*" wildcard. An empty allow list denies everything.
func (p Policy) Allows(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	for _, pat := range p.Allow {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "*" || pat == m {
			return true
		}
		if strings.HasSuffix(pat, "/*") {
			if strings.HasPrefix(m, strings.TrimSuffix(pat, "*")) {
				return true
			}
		}
	}
	return false
}

// Store is a content-addressed blob store rooted at a directory (typically
// data/media/). Writes dedupe by sha256; reads validate the handle format so
// no path built from a handle can escape the root.
type Store struct {
	dir string
}

// Open creates (if needed) and returns a Store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("media store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Ref describes a stored blob.
type Ref struct {
	Handle string
	MIME   string
	Size   int64
}

// Put streams r into the store, content-addressed by sha256, enforcing the
// policy ceiling. Returns the ref; a repeat write of the same content is a
// no-op that returns the existing ref.
func (s *Store) Put(r io.Reader, mime string, pol Policy) (*Ref, error) {
	limit := pol.maxBytes()
	tmp, err := os.CreateTemp(s.dir, ".in-*")
	if err != nil {
		return nil, fmt.Errorf("media store: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("media store: %w", err)
	}
	if size > limit {
		tmp.Close()
		return nil, fmt.Errorf("%w: %d bytes (limit %d)", ErrTooLarge, size, limit)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("media store: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	final := filepath.Join(s.dir, sum)
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpName, final); err != nil {
			return nil, fmt.Errorf("media store: %w", err)
		}
	}
	return &Ref{Handle: "media:" + sum, MIME: mime, Size: size}, nil
}

// Open returns a reader over the blob named by handle.
func (s *Store) Open(handle string) (*os.File, error) {
	if !ValidHandle(handle) {
		return nil, fmt.Errorf("media store: malformed handle %q", handle)
	}
	return os.Open(filepath.Join(s.dir, strings.TrimPrefix(handle, "media:")))
}

// ReadAll reads the whole blob, enforcing limit as a sanity ceiling.
func (s *Store) ReadAll(handle string, limit int64) ([]byte, error) {
	f, err := s.Open(handle)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("media store: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("media store: blob %s exceeds read limit", handle)
	}
	return b, nil
}
