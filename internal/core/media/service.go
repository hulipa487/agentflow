package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ScopeMaintenance bypasses per-tenant grant checks for system/scheduler
// maintenance turns (matching memory.ModeMaintenance).
const ScopeMaintenance = "__maintenance__"

// ErrUnauthorized is returned when a scope attempts to materialize a media
// handle it does not hold a grant for.
var ErrUnauthorized = errors.New("media handle not authorized for acting scope")

// ErrNoPresigner is returned by MaterializeURL when neither an S3 Presigner
// nor an HTTP temporary URL publisher is configured.
var ErrNoPresigner = errors.New("media store cannot produce public URLs (configure media.backend: s3 or media.temp_url.enabled)")

// IngestOptions configures one media ingestion call.
type IngestOptions struct {
	MIME   string        // hint; sniffed if empty or application/octet-stream
	Name   string        // optional original filename
	Scope  string        // engine-stamped scope prefix ("user:<uuid>" or "agent:<name>")
	Origin string        // "channel" | "llm" | "mcp" | "http" | "border" | "derived"
	Policy Policy        // size ceiling & optional MIME allow-list (empty Allow = admit any)
	TTL    time.Duration // 0 = service default retention
}

// TargetSpec describes what an egress sink (channel or LLM provider) accepts.
type TargetSpec struct {
	AcceptMIME     []string      // accepted MIME types/wildcards; empty = any
	PreferMIME     string        // preferred target MIME when transcoding (e.g. "image/jpeg")
	MaxBytes       int64         // egress size cap; 0 = service default (50 MiB)
	PresignTTL     time.Duration // URL lifetime for MaterializeURL; 0 = service default (10m)
	AttachFilename bool          // include Content-Disposition filename on presigned URLs
	AllowURLFetch  bool          // allow fetching remote http(s) URLs when MaterializeBytes needs raw bytes
}

// BlobData is materialized media ready for an egress consumer.
type BlobData struct {
	Bytes  []byte
	MIME   string
	Name   string
	Handle string
}

// ServiceOptions configures a media.Service.
type ServiceOptions struct {
	Primary      Store         // centralized blob store (S3 or FS)
	Secondary    Store         // optional legacy separate files store (read fallback)
	Presigner    Presigner     // optional URL publisher (if nil, Primary is type-asserted for Presigner)
	Catalog      *Catalog      // metadata, per-scope grants, and derived-variant cache
	Transcoder   Transcoder    // defaults to ImageTranscoder
	Retention    time.Duration // default grant lifetime (0 = forever)
	DerivedTTL   time.Duration // derived transcode cache lifetime (default 24h)
	PresignTTL   time.Duration // default presigned URL lifetime (default 10m)
	HTTPClient   *http.Client  // optional netguard-protected client for AllowURLFetch
	EnforceScope bool          // enforce (scope, handle) grant checks when Catalog is enabled
}

// Service is the centralized media plane: ingest from any source into handles,
// enforce per-scope grants, transcode/cache variants, materialize for channels
// and LLMs, and sweep expired blobs.
type Service struct {
	primary      Store
	secondary    Store
	presigner    Presigner
	catalog      *Catalog
	transcoder   Transcoder
	retention    time.Duration
	derivedTTL   time.Duration
	presignTTL   time.Duration
	httpClient   *http.Client
	enforceScope bool
	now          func() time.Time
}

const (
	defaultMaterializeMax = 50 << 20
	defaultPresignTTL     = 10 * time.Minute
	defaultDerivedTTL     = 24 * time.Hour
)

// NewService constructs a centralized media Service.
func NewService(opts ServiceOptions) *Service {
	presigner := opts.Presigner
	if presigner == nil && opts.Primary != nil {
		if p, ok := opts.Primary.(Presigner); ok {
			presigner = p
		}
	}
	tc := opts.Transcoder
	if tc == nil {
		tc = ImageTranscoder{}
	}
	derivedTTL := opts.DerivedTTL
	if derivedTTL <= 0 {
		derivedTTL = defaultDerivedTTL
	}
	presignTTL := opts.PresignTTL
	if presignTTL <= 0 {
		presignTTL = defaultPresignTTL
	}
	return &Service{
		primary:      opts.Primary,
		secondary:    opts.Secondary,
		presigner:    presigner,
		catalog:      opts.Catalog,
		transcoder:   tc,
		retention:    opts.Retention,
		derivedTTL:   derivedTTL,
		presignTTL:   presignTTL,
		httpClient:   opts.HTTPClient,
		enforceScope: opts.EnforceScope,
		now:          time.Now,
	}
}

// Store returns the primary blob store.
func (s *Service) Store() Store { return s.primary }

// CanPresign reports whether this service can mint public HTTP(S) URLs for
// stored blobs (either via S3 presigning or an FS HMAC-HTTP publisher).
func (s *Service) CanPresign() bool { return s != nil && s.presigner != nil }

// IngestReader stores bytes from r under policy, records metadata and a
// per-scope grant, and returns a handle-only Part (Data is always empty).
func (s *Service) IngestReader(ctx context.Context, r io.Reader, opts IngestOptions) (Part, error) {
	if s == nil || s.primary == nil {
		return Part{}, errors.New("media service: no blob store configured")
	}
	limit := opts.Policy.MaxOrDefault()
	// Peek up to 512 bytes for MIME sniffing without loading the entire stream.
	var head bytes.Buffer
	peeked, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return Part{}, fmt.Errorf("media ingest: read header: %w", err)
	}
	head.Write(peeked)

	mime := normalizeMIME(opts.MIME)
	if mime == "" || mime == "application/octet-stream" {
		mime = SniffMIME(peeked, opts.Name)
	}
	if len(opts.Policy.Allow) > 0 && !opts.Policy.Allows(mime) {
		return Part{}, fmt.Errorf("media ingest: mime %q not allowed by policy", mime)
	}
	if int64(len(peeked)) > limit {
		return Part{}, fmt.Errorf("%w: exceeds limit %d", ErrTooLarge, limit)
	}

	combined := io.MultiReader(&head, r)
	ref, err := s.primary.Put(combined, mime, Policy{MaxBytes: limit, Allow: opts.Policy.Allow})
	if err != nil {
		return Part{}, err
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = s.retention
	}
	if s.catalog != nil {
		_ = s.catalog.RecordMeta(ctx, BlobMeta{
			Handle: ref.Handle,
			MIME:   mime,
			Size:   ref.Size,
			Origin: opts.Origin,
		}, ttl)
		if opts.Scope != "" {
			_ = s.catalog.Grant(ctx, opts.Scope, ref.Handle, opts.Origin, ttl)
		}
	}
	return Part{
		Type:   Classify(mime),
		MIME:   mime,
		Handle: ref.Handle,
		Name:   opts.Name,
		Size:   ref.Size,
	}, nil
}

// IngestBytes ingests an in-memory byte slice.
func (s *Service) IngestBytes(ctx context.Context, b []byte, opts IngestOptions) (Part, error) {
	return s.IngestReader(ctx, bytes.NewReader(b), opts)
}

// IngestBase64 decodes standard base64 (stripping surrounding whitespace) and
// ingests the bytes.
func (s *Service) IngestBase64(ctx context.Context, b64 string, opts IngestOptions) (Part, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return Part{}, fmt.Errorf("media ingest: bad base64: %w", err)
	}
	return s.IngestBytes(ctx, raw, opts)
}

// IngestDataURI parses data:<mime>;base64,<payload> and ingests the bytes.
func (s *Service) IngestDataURI(ctx context.Context, uri string, opts IngestOptions) (Part, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(uri), "data:")
	if !ok {
		return Part{}, errors.New("media ingest: not a data URI")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return Part{}, errors.New("media ingest: data URI must be data:<mime>;base64,<payload>")
	}
	if opts.MIME == "" {
		opts.MIME = strings.TrimSuffix(meta, ";base64")
	}
	return s.IngestBase64(ctx, payload, opts)
}

// Grant records that scope is allowed to materialize handle (e.g., when
// files.read hands a handle to a session turn).
func (s *Service) Grant(ctx context.Context, scope, handle, origin string) error {
	if s == nil || s.catalog == nil || scope == "" {
		return nil
	}
	return s.catalog.Grant(ctx, scope, handle, origin, s.retention)
}

// Authorize checks whether scope may materialize handle.
func (s *Service) Authorize(ctx context.Context, scope, handle string) error {
	if s == nil || !s.enforceScope || s.catalog == nil || !s.catalog.enabled() {
		return nil
	}
	if scope == ScopeMaintenance {
		return nil
	}
	if scope == "" {
		return fmt.Errorf("%w: no acting scope for %s", ErrUnauthorized, handle)
	}
	ok, err := s.catalog.HasGrant(ctx, scope, handle)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnauthorized, handle)
	}
	return nil
}

// NormalizeBorder ensures any Part crossing from Lua into Go has no raw inline
// base64 bytes: Data parts are ingested into the central store once and
// replaced with their handle-only descriptors, while existing handles are
// scope-checked.
func (s *Service) NormalizeBorder(ctx context.Context, scope string, parts []Part) ([]Part, error) {
	if len(parts) == 0 {
		return parts, nil
	}
	out := make([]Part, len(parts))
	for i, p := range parts {
		if p.Type == "text" {
			out[i] = p
			continue
		}
		if p.Handle != "" {
			if !ValidHandle(p.Handle) {
				return nil, fmt.Errorf("malformed media handle %q", p.Handle)
			}
			if err := s.Authorize(ctx, scope, p.Handle); err != nil {
				return nil, err
			}
			// Enrich missing MIME/Type/Size from catalog when available.
			if s != nil && s.catalog != nil {
				if meta, ok, err := s.catalog.GetMeta(ctx, p.Handle); err == nil && ok {
					if p.MIME == "" {
						p.MIME = meta.MIME
					}
					if p.Size == 0 {
						p.Size = meta.Size
					}
				}
			}
			if p.Type == "" && p.MIME != "" {
				p.Type = Classify(p.MIME)
			}
			p.Data = ""
			out[i] = p
			continue
		}
		if p.Data != "" && s != nil && s.primary != nil {
			ing, err := s.IngestBase64(ctx, p.Data, IngestOptions{
				MIME:   p.MIME,
				Name:   p.Name,
				Scope:  scope,
				Origin: "border",
				Policy: Policy{MaxBytes: defaultMaterializeMax},
			})
			if err != nil {
				return nil, err
			}
			if p.Type != "" {
				ing.Type = p.Type
			}
			out[i] = ing
			continue
		}
		out[i] = p
	}
	return out, nil
}

// MaterializeBytes resolves part into raw bytes conforming to spec (enforcing
// scope authorization and transcoding when MIME or MaxBytes require it).
func (s *Service) MaterializeBytes(ctx context.Context, scope string, part Part, spec TargetSpec) (BlobData, error) {
	if s == nil {
		return BlobData{}, errors.New("media service: nil service")
	}
	maxBytes := spec.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaterializeMax
	}

	var raw []byte
	var handle string
	mime := normalizeMIME(part.MIME)
	name := part.Name

	switch {
	case part.Handle != "":
		if !ValidHandle(part.Handle) {
			return BlobData{}, fmt.Errorf("media: malformed handle %q", part.Handle)
		}
		if err := s.Authorize(ctx, scope, part.Handle); err != nil {
			return BlobData{}, err
		}
		handle = part.Handle
		if mime == "" && s.catalog != nil {
			if meta, ok, err := s.catalog.GetMeta(ctx, handle); err == nil && ok && meta.MIME != "" {
				mime = meta.MIME
			}
		}
		// Check derived variant cache first if transcoding may be required.
		if needsAdapt, vKey := s.variantKeyFor(mime, 0, spec); needsAdapt && s.catalog != nil {
			if v, ok, err := s.catalog.GetVariant(ctx, handle, vKey); err == nil && ok {
				if vb, rerr := s.readBlob(v.Handle, maxBytes); rerr == nil {
					return BlobData{Bytes: vb, MIME: v.MIME, Name: name, Handle: v.Handle}, nil
				}
			}
		}
		// Allow reading slightly larger source bytes if we can transcode down to spec.MaxBytes.
		readCap := maxBytes
		if s.transcoder != nil && readCap < defaultMaterializeMax {
			readCap = defaultMaterializeMax
		}
		b, err := s.readBlob(handle, readCap)
		if err != nil {
			return BlobData{}, err
		}
		raw = b

	case part.Data != "":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(part.Data))
		if err != nil {
			return BlobData{}, fmt.Errorf("media: bad base64: %w", err)
		}
		raw = b

	case (strings.HasPrefix(part.URL, "https://") || strings.HasPrefix(part.URL, "http://")) && spec.AllowURLFetch:
		b, fetchedMIME, err := s.fetchURL(ctx, part.URL, defaultMaterializeMax)
		if err != nil {
			return BlobData{}, err
		}
		raw = b
		if mime == "" {
			mime = fetchedMIME
		}

	default:
		return BlobData{}, fmt.Errorf("media: attachment %q has no materializable source", partLabel(part))
	}

	if mime == "" || mime == "application/octet-stream" {
		mime = SniffMIME(raw, name)
	}

	// Adapt format or size if required by spec.
	if needsAdapt, vKey := s.variantKeyFor(mime, int64(len(raw)), spec); needsAdapt {
		out, outMIME, err := s.transcoder.Transcode(raw, mime, spec)
		if err != nil {
			return BlobData{}, err
		}
		if out != nil {
			outHandle := handle
			if s.primary != nil {
				if ref, perr := s.primary.Put(bytes.NewReader(out), outMIME, Policy{MaxBytes: maxBytes}); perr == nil {
					outHandle = ref.Handle
					if s.catalog != nil {
						_ = s.catalog.RecordMeta(ctx, BlobMeta{
							Handle: ref.Handle, MIME: outMIME, Size: ref.Size, Origin: "derived",
						}, s.derivedTTL)
						if handle != "" {
							_ = s.catalog.PutVariant(ctx, handle, vKey, VariantRecord{
								SourceHandle: handle, Handle: ref.Handle, MIME: outMIME, Size: ref.Size,
							}, s.derivedTTL)
						}
					}
				}
			}
			return BlobData{Bytes: out, MIME: outMIME, Name: replaceExt(name, outMIME), Handle: outHandle}, nil
		}
	}
	if int64(len(raw)) > maxBytes {
		return BlobData{}, fmt.Errorf("%w: %d bytes (limit %d)", ErrTooLarge, len(raw), maxBytes)
	}
	return BlobData{Bytes: raw, MIME: mime, Name: name, Handle: handle}, nil
}

// MaterializeDataURI resolves part into a data:<mime>;base64,<payload> URI.
func (s *Service) MaterializeDataURI(ctx context.Context, scope string, part Part, spec TargetSpec) (string, error) {
	blob, err := s.MaterializeBytes(ctx, scope, part, spec)
	if err != nil {
		return "", err
	}
	mime := blob.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(blob.Bytes), nil
}

// MaterializeURL turns part into a time-limited public http(s) GET URL
// conforming to spec (transcoding and caching a derived blob in Primary if the
// target MIME/size differs).
func (s *Service) MaterializeURL(ctx context.Context, scope string, part Part, spec TargetSpec) (string, error) {
	if s == nil {
		return "", errors.New("media service: nil service")
	}
	// Pass through an explicit http(s) URL when no transcoding is forced.
	if (strings.HasPrefix(part.URL, "https://") || strings.HasPrefix(part.URL, "http://")) && part.Handle == "" && part.Data == "" {
		if len(spec.AcceptMIME) == 0 || part.MIME == "" || mimeAccepted(part.MIME, spec.AcceptMIME) {
			return part.URL, nil
		}
	}
	if s.presigner == nil {
		return "", fmt.Errorf("media: attachment %q: %w", partLabel(part), ErrNoPresigner)
	}

	ttl := spec.PresignTTL
	if ttl <= 0 {
		ttl = s.presignTTL
	}

	// Stage inline base64 into Primary first so it has a handle.
	handle := part.Handle
	mime := normalizeMIME(part.MIME)
	name := part.Name
	if handle == "" {
		if part.Data == "" {
			return "", fmt.Errorf("media: attachment %q has no public URL, handle, or data", partLabel(part))
		}
		ing, err := s.IngestBase64(ctx, part.Data, IngestOptions{
			MIME:   mime,
			Name:   name,
			Scope:  scope,
			Origin: "egress",
			Policy: Policy{MaxBytes: defaultMaterializeMax},
		})
		if err != nil {
			return "", err
		}
		handle = ing.Handle
		mime = ing.MIME
	} else {
		if !ValidHandle(handle) {
			return "", fmt.Errorf("media: malformed handle %q", handle)
		}
		if err := s.Authorize(ctx, scope, handle); err != nil {
			return "", err
		}
		if mime == "" && s.catalog != nil {
			if meta, ok, err := s.catalog.GetMeta(ctx, handle); err == nil && ok {
				mime = meta.MIME
			}
		}
	}

	// Determine whether we can presign the original blob directly or need
	// transcoding / secondary→primary copy.
	var size int64
	if s.catalog != nil {
		if meta, ok, err := s.catalog.GetMeta(ctx, handle); err == nil && ok {
			size = meta.Size
		}
	}
	needsAdapt, vKey := s.variantKeyFor(mime, size, spec)
	targetHandle := handle
	targetMIME := mime
	targetName := name

	if needsAdapt {
		if s.catalog != nil {
			if v, ok, err := s.catalog.GetVariant(ctx, handle, vKey); err == nil && ok {
				targetHandle = v.Handle
				targetMIME = v.MIME
				targetName = replaceExt(name, v.MIME)
				needsAdapt = false
			}
		}
		if needsAdapt {
			blob, err := s.MaterializeBytes(ctx, scope, Part{
				Type:   part.Type,
				MIME:   mime,
				Handle: handle,
				Name:   name,
			}, spec)
			if err != nil {
				return "", err
			}
			targetHandle = blob.Handle
			targetMIME = blob.MIME
			targetName = blob.Name
		}
	} else if s.secondary != nil && s.primary != nil {
		// If the blob lives only in Secondary (legacy separate files store),
		// ensure a copy exists in Primary before presigning from Primary.
		if _, err := s.primary.ReadAll(targetHandle, 1); err != nil && !errors.Is(err, ErrExceedsLimit) {
			if b, serr := s.secondary.ReadAll(targetHandle, defaultMaterializeMax); serr == nil {
				if ref, perr := s.primary.Put(bytes.NewReader(b), targetMIME, Policy{MaxBytes: defaultMaterializeMax}); perr == nil {
					targetHandle = ref.Handle
				}
			}
		}
	}

	opts := PresignOptions{ContentType: targetMIME}
	if spec.AttachFilename && targetName != "" {
		opts.Filename = targetName
	}
	return s.presigner.PresignGet(targetHandle, ttl, opts)
}

// PurgeScope removes every media grant owned by scope.
func (s *Service) PurgeScope(ctx context.Context, scope string) (int, error) {
	if s == nil || s.catalog == nil {
		return 0, nil
	}
	return s.catalog.PurgeScope(ctx, scope)
}

// LiveHandles returns all handles kept alive by unexpired media grants or
// cached derived variants.
func (s *Service) LiveHandles(ctx context.Context) (map[string]bool, error) {
	if s == nil || s.catalog == nil {
		return map[string]bool{}, nil
	}
	return s.catalog.LiveHandles(ctx)
}

// GC performs a unified mark-sweep over the primary blob store, preserving
// every handle in media grants/variants plus any extra live sets (such as
// files.Manager.LiveHandles) and any blob newer than grace.
func (s *Service) GC(ctx context.Context, grace time.Duration, extraLive ...map[string]bool) (liveCount, swept int, err error) {
	if s == nil || s.primary == nil {
		return 0, 0, nil
	}
	live, err := s.LiveHandles(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, set := range extraLive {
		for h := range set {
			live[h] = true
		}
	}
	blobs, err := s.primary.List()
	if err != nil {
		return len(live), 0, err
	}
	cutoff := s.now().Add(-grace)
	for _, b := range blobs {
		if live[b.Handle] {
			continue
		}
		if grace > 0 && b.ModTime.After(cutoff) {
			continue
		}
		if err := s.primary.Delete(b.Handle); err != nil {
			return len(live), swept, err
		}
		swept++
	}
	return len(live), swept, nil
}

func (s *Service) readBlob(handle string, limit int64) ([]byte, error) {
	if s.primary != nil {
		b, err := s.primary.ReadAll(handle, limit)
		if err == nil {
			return b, nil
		}
		if s.secondary == nil {
			return nil, err
		}
	}
	if s.secondary != nil {
		return s.secondary.ReadAll(handle, limit)
	}
	return nil, errors.New("media: no blob store configured")
}

func (s *Service) fetchURL(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	client := s.httpClient
	if client == nil {
		return nil, "", errors.New("media: remote URL fetch disabled (no guarded HTTP client configured)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("media fetch: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > limit {
		return nil, "", fmt.Errorf("%w: remote URL exceeds %d bytes", ErrTooLarge, limit)
	}
	return b, normalizeMIME(resp.Header.Get("Content-Type")), nil
}

func (s *Service) variantKeyFor(mime string, size int64, spec TargetSpec) (bool, string) {
	mimeMismatch := len(spec.AcceptMIME) > 0 && mime != "" && !mimeAccepted(mime, spec.AcceptMIME)
	sizeMismatch := spec.MaxBytes > 0 && size > spec.MaxBytes
	if !mimeMismatch && !sizeMismatch {
		return false, ""
	}
	target := spec.PreferMIME
	if target == "" && len(spec.AcceptMIME) > 0 {
		target = spec.AcceptMIME[0]
	}
	return true, fmt.Sprintf("%s|%d", strings.ToLower(target), spec.MaxBytes)
}

func mimeAccepted(mime string, accept []string) bool {
	p := Policy{Allow: accept}
	return p.Allows(mime)
}

func normalizeMIME(m string) string {
	m = strings.TrimSpace(strings.Split(m, ";")[0])
	return strings.ToLower(m)
}

func partLabel(p Part) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Type != "" {
		return p.Type
	}
	return "attachment"
}

func replaceExt(name, mime string) string {
	if name == "" {
		return ""
	}
	var ext string
	switch strings.ToLower(mime) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	default:
		return name
	}
	if dot := strings.LastIndex(name, "."); dot > 0 {
		return name[:dot] + ext
	}
	return name + ext
}

// SniffMIME detects a MIME type from the first bytes and optional filename.
func SniffMIME(b []byte, name string) string {
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(b) >= 10 && (string(b[:9]) == "#!SILK_V3" || string(b[1:10]) == "#!SILK_V3") {
		return "audio/silk"
	}
	if len(b) >= 4 && string(b[:4]) == "%PDF" {
		return "application/pdf"
	}
	if len(b) > 0 {
		if ct := normalizeMIME(http.DetectContentType(b)); ct != "" && ct != "application/octet-stream" && ct != "text/plain" {
			return ct
		}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(lower, ".silk"), strings.HasSuffix(lower, ".slk"):
		return "audio/silk"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	}
	if len(b) > 0 {
		return normalizeMIME(http.DetectContentType(b))
	}
	return "application/octet-stream"
}
