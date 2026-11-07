// Package bundle is the full-fidelity user migration bundle: one user's
// profile, linked handles, memory rows, files and credentials, moved between
// regional cells.
//
// The privacy export (`GET /admin/api/users/{id}/export`) answers "what do you
// hold about me?" with usage, audit and references. This is the operational
// move: it carries the bytes, and credentials are re-encrypted to a public key
// the caller supplies so the bundle itself never contains a plaintext secret.
//
// A bundle is a zip with one bundle.json inside. Blobs are base64 in that JSON
// rather than separate entries: a bundle is small (one person's working files),
// and one document makes import atomic to parse and hard to partially apply.
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/files"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/tenancy"
)

// Version is the bundle format. An importer refuses anything else: a bundle is
// data that outlives the release that wrote it, and guessing at a future
// shape is how a migration silently drops a field.
const Version = 1

// envelopeInfo domain-separates the bundle's key derivation from any other use
// of the same X25519 key pair.
const envelopeInfo = "agentflow-user-bundle-v1"

// MemoryRow is one record in a user-scoped memory store, with its scope prefix
// already removed. Agent and Table name the binding to write it back to.
//
// Vector and ExpiresAt are present when the source backend can read rows back
// whole (memory.RowExporter): without them a moved row drops out of semantic
// recall and never expires.
type MemoryRow struct {
	Agent  string    `json:"agent"`
	Table  string    `json:"table"`
	Key    string    `json:"key"`
	Value  any       `json:"value"`
	Vector []float32 `json:"vector,omitempty"`
	// ExpiresAt is unix seconds; 0 means the row has no TTL.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// MaxBundleBytes is the largest bundle an import reads. An export refuses to
// build one that would not fit, rather than produce a file the other cell
// rejects.
const MaxBundleBytes = 256 << 20

// maxFileBytes bounds the file bytes in one bundle. They travel base64-encoded
// inside the JSON document, a third larger, and the rest of the bundle needs
// room too.
const maxFileBytes = 160 << 20

// FileEntry is one working-tree file with its bytes.
type FileEntry struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	MIME    string `json:"mime,omitempty"`
	Data    []byte `json:"data"`
}

// GroupRef is a group membership. The group must already exist in the
// destination cell; a bundle cannot recreate a group without also recreating
// every other member's grants.
type GroupRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// Credential is one secret, sealed to the caller's public key. Redacted means
// the exporter was not given a key and sent metadata only.
type Credential struct {
	Service  string `json:"service"`
	Kind     string `json:"kind"`
	Header   string `json:"header,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Envelope string `json:"envelope,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// Bundle is the document inside the zip.
type Bundle struct {
	Version     int              `json:"version"`
	ExportedAt  string           `json:"exported_at"`
	Region      string           `json:"region,omitempty"`
	Profile     identity.Profile `json:"profile"`
	Groups      []GroupRef       `json:"groups,omitempty"`
	Memory      []MemoryRow      `json:"memory,omitempty"`
	Files       []FileEntry      `json:"files,omitempty"`
	Credentials []Credential     `json:"credentials,omitempty"`
}

// Service reads and writes bundles against one cell's stores.
type Service struct {
	Identity *identity.Registry
	Memory   *memory.Manager
	Memories []memory.AgentMemory
	Files    *files.Manager
	Creds    *credentials.Store
	Tenancy  *tenancy.Registry
	Region   string
	Log      *slog.Logger
}

// Report is what an import did and what it could not do.
type Report struct {
	Profile    bool `json:"profile"`
	Identities int  `json:"identities"`
	MemoryRows int  `json:"memory_rows"`
	// VectorsDropped counts memory rows imported without their embedding
	// because the destination store cannot hold it (no vector support, or a
	// different dimension). They need re-embedding to appear in vector recall.
	VectorsDropped  int      `json:"vectors_dropped,omitempty"`
	Files           int      `json:"files"`
	Credentials     int      `json:"credentials"`
	GroupsAdded     int      `json:"groups_added"`
	GroupsSkipped   []string `json:"groups_skipped,omitempty"`
	CredentialsSkip int      `json:"credentials_skipped,omitempty"`
}

// Export reads one user's data. publicKey is a raw X25519 public key (32
// bytes); nil exports credential metadata only.
func (s *Service) Export(ctx context.Context, userID string, publicKey []byte) (*Bundle, error) {
	if s.Identity == nil {
		return nil, fmt.Errorf("bundle: identity is not enabled")
	}
	p, ok, err := s.Identity.Get(userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bundle: no such profile %q", userID)
	}
	b := &Bundle{
		Version:    Version,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Region:     s.Region,
		Profile:    p,
	}
	if s.Tenancy != nil {
		groups, err := s.Tenancy.GroupsOf(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			b.Groups = append(b.Groups, GroupRef{UUID: g.UUID, Name: g.Name})
		}
	}
	b.Memory, err = s.exportMemory(userID)
	if err != nil {
		return nil, err
	}
	b.Files, err = s.exportFiles(ctx, userID)
	if err != nil {
		return nil, err
	}
	b.Credentials, err = s.exportCredentials(ctx, userID, publicKey)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Service) exportMemory(userID string) ([]MemoryRow, error) {
	if s.Memory == nil {
		return nil, nil
	}
	var out []MemoryRow
	seen := map[string]bool{}
	for _, am := range s.Memories {
		// Tables is keyed by the profile's logical table name; Stores carries
		// the physical (agent-prefixed) table. BindForTable takes the logical
		// key, so iterate Tables.
		for logical, bind := range am.Tables {
			if bind.Scoping != "user" {
				continue // pool and agent-wide stores are not one person's data
			}
			key := bind.Agent + "|" + bind.Table
			if seen[key] {
				continue
			}
			seen[key] = true
			h, b, err := s.Memory.BindForTable(am, logical)
			if err != nil {
				continue // a table this agent's profile does not expose
			}
			if ex, ok := memory.AsRowExporter(h); ok {
				recs, err := ex.ExportRows(b.Table)
				if err != nil {
					return nil, err
				}
				for _, rec := range recs {
					k, ok := userScopedKey(rec.Key, userID)
					if !ok {
						continue
					}
					row := MemoryRow{Agent: b.Agent, Table: b.Table, Key: k, Value: rec.Value, Vector: rec.Vector}
					if !rec.ExpiresAt.IsZero() {
						row.ExpiresAt = rec.ExpiresAt.Unix()
					}
					out = append(out, row)
				}
				continue
			}
			it, err := h.Query(b.Table, memory.Query{Kind: "all"})
			if err != nil {
				return nil, err
			}
			for it.Next() {
				rec := it.Record()
				k, ok := userScopedKey(rec.Key, userID)
				if !ok {
					continue
				}
				out = append(out, MemoryRow{Agent: b.Agent, Table: b.Table, Key: k, Value: rec.Value})
			}
			if err := it.Err(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// userScopedKey reports whether a raw store key is this user's, and returns it
// with the scope prefix removed. The scope may carry a generation suffix
// ("user:<uuid>@3|key"); the suffix is dropped because the importing cell's
// ownership generation is its own.
func userScopedKey(key, userID string) (string, bool) {
	head, rest, ok := strings.Cut(key, "|")
	if !ok {
		return "", false
	}
	base := head
	if i := strings.LastIndexByte(head, '@'); i > 0 {
		digits := true
		for _, ch := range head[i+1:] {
			if ch < '0' || ch > '9' {
				digits = false
				break
			}
		}
		if digits {
			base = head[:i]
		}
	}
	if base != "user:"+userID {
		return "", false
	}
	return rest, true
}

func (s *Service) exportFiles(ctx context.Context, userID string) ([]FileEntry, error) {
	if s.Files == nil {
		return nil, nil
	}
	scope := "user:" + userID
	projects, err := s.Files.Projects(ctx, scope)
	if err != nil {
		return nil, err
	}
	var out []FileEntry
	var total int64
	for _, project := range projects {
		entries, err := s.Files.List(ctx, scope, project, "")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			data, err := s.Files.ReadBlob(ctx, e.Handle, s.Files.MaxFileBytes())
			if err != nil {
				return nil, fmt.Errorf("bundle: read %s/%s: %w", project, e.Path, err)
			}
			total += int64(len(data))
			if total > maxFileBytes {
				return nil, fmt.Errorf("bundle: the user's files exceed %d MiB, more than one bundle can carry; move the largest ones separately and export again", maxFileBytes>>20)
			}
			out = append(out, FileEntry{Project: project, Path: e.Path, MIME: e.MIME, Data: data})
		}
	}
	return out, nil
}

func (s *Service) exportCredentials(ctx context.Context, userID string, publicKey []byte) ([]Credential, error) {
	if s.Creds == nil {
		return nil, nil
	}
	refs, err := s.Creds.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []Credential
	for _, ref := range refs {
		c := Credential{Service: ref.Service, Kind: ref.Kind, Header: ref.Header, Scheme: ref.Scheme}
		if len(publicKey) == 0 {
			c.Redacted = true
			out = append(out, c)
			continue
		}
		secret, ok, err := s.Creds.Get(ctx, userID, ref.Service)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		env, err := Seal(publicKey, []byte(secret.Value))
		if err != nil {
			return nil, err
		}
		c.Envelope = env
		out = append(out, c)
	}
	return out, nil
}

// Import writes a bundle into this cell. It is additive: a profile that
// already exists is updated, memory rows are upserted, and files are written
// into the same projects. privateKey is required only when the bundle carries
// sealed credentials.
func (s *Service) Import(ctx context.Context, b *Bundle, privateKey []byte) (Report, error) {
	var rep Report
	if b.Version != Version {
		return rep, fmt.Errorf("bundle: version %d, this runtime reads %d", b.Version, Version)
	}
	if s.Identity == nil {
		return rep, fmt.Errorf("bundle: identity is not enabled")
	}
	linked, err := s.Identity.ImportProfile(b.Profile)
	if err != nil {
		return rep, err
	}
	rep.Profile = true
	rep.Identities = linked

	n, err := s.importMemory(b, &rep.VectorsDropped)
	if err != nil {
		return rep, err
	}
	rep.MemoryRows = n
	if s.Files != nil {
		for _, f := range b.Files {
			if _, err := s.Files.Put(ctx, "user:"+b.Profile.UserID, f.Project, f.Path, bytes.NewReader(f.Data), f.MIME); err != nil {
				return rep, fmt.Errorf("bundle: import file %s/%s: %w", f.Project, f.Path, err)
			}
			rep.Files++
		}
	}
	for _, c := range b.Credentials {
		if c.Redacted || c.Envelope == "" {
			rep.CredentialsSkip++
			continue
		}
		if s.Creds == nil {
			return rep, fmt.Errorf("bundle: credentials are not enabled in this cell")
		}
		if len(privateKey) == 0 {
			return rep, fmt.Errorf("bundle: credential %q is sealed and no private key was supplied", c.Service)
		}
		plain, err := Open(privateKey, c.Envelope)
		if err != nil {
			return rep, fmt.Errorf("bundle: credential %q: %w", c.Service, err)
		}
		if err := s.Creds.Put(ctx, b.Profile.UserID, c.Service, c.Kind, string(plain), c.Header, c.Scheme); err != nil {
			return rep, err
		}
		rep.Credentials++
	}
	if s.Tenancy != nil {
		for _, g := range b.Groups {
			if _, ok, err := s.Tenancy.Group(ctx, g.UUID); err != nil {
				return rep, err
			} else if !ok {
				rep.GroupsSkipped = append(rep.GroupsSkipped, g.Name+" ("+g.UUID+")")
				continue
			}
			if _, err := s.Tenancy.AddMember(ctx, g.UUID, b.Profile.UserID); err != nil {
				// Already a member is not a failure; the membership is there.
				if strings.Contains(err.Error(), "already") {
					rep.GroupsAdded++
					continue
				}
				return rep, fmt.Errorf("bundle: group %s: %w", g.Name, err)
			}
			rep.GroupsAdded++
		}
	}
	return rep, nil
}

func (s *Service) importMemory(b *Bundle, dropped *int) (int, error) {
	if s.Memory == nil || len(b.Memory) == 0 {
		return 0, nil
	}
	type target struct {
		am      memory.AgentMemory
		logical string
	}
	byBinding := map[string]target{}
	for _, am := range s.Memories {
		for logical, bind := range am.Tables {
			if bind.Scoping != "user" {
				continue
			}
			byBinding[bind.Agent+"|"+bind.Table] = target{am: am, logical: logical}
		}
	}
	imported := 0
	now := time.Now()
	for _, row := range b.Memory {
		tgt, ok := byBinding[row.Agent+"|"+row.Table]
		if !ok {
			continue // this cell has no such agent/table; the rest still import
		}
		h, bnd, err := s.Memory.BindForTable(tgt.am, tgt.logical)
		if err != nil {
			continue
		}
		scoped := memory.WrapScoped(h, bnd.Scoping, memory.ModeInteractive,
			memory.Caller{ScopeUUID: b.Profile.UserID, PersonalUUID: b.Profile.UserID, Locality: bnd.Locality},
			memory.PoolScope{})
		var opts memory.PutOpts
		if row.ExpiresAt > 0 {
			opts.TTL = time.Unix(row.ExpiresAt, 0).Sub(now)
			if opts.TTL <= 0 {
				continue // expired in transit
			}
		}
		if len(row.Vector) > 0 {
			if hasFeature(s.Memory.Features(bnd.Backend), "vector") {
				opts.Vector = row.Vector
			} else {
				*dropped++
			}
		}
		err = scoped.Put(bnd.Table, row.Key, row.Value, opts)
		if err != nil && opts.Vector != nil {
			// A vector-capable store can still refuse this one, typically a
			// different dimension because the cells embed with different
			// models. The value is what the person owns; keep it, and report
			// the row as needing a re-embed.
			opts.Vector = nil
			if err = scoped.Put(bnd.Table, row.Key, row.Value, opts); err == nil {
				*dropped++
			}
		}
		if err != nil {
			return imported, fmt.Errorf("bundle: memory %s/%s %q: %w", row.Agent, row.Table, row.Key, err)
		}
		imported++
	}
	return imported, nil
}

func hasFeature(features []string, want string) bool {
	for _, f := range features {
		if f == want {
			return true
		}
	}
	return false
}

// Zip renders the bundle as the downloadable document.
func (b *Bundle) Zip() ([]byte, error) {
	doc, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("bundle.json")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(doc); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadZip parses a bundle document.
func ReadZip(data []byte) (*Bundle, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("bundle: not a zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "bundle.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		doc, err := io.ReadAll(io.LimitReader(rc, MaxBundleBytes+1))
		if err != nil {
			return nil, err
		}
		if len(doc) > MaxBundleBytes {
			return nil, fmt.Errorf("bundle: bundle.json is larger than %d MiB", MaxBundleBytes>>20)
		}
		var b Bundle
		if err := json.Unmarshal(doc, &b); err != nil {
			return nil, fmt.Errorf("bundle: parse bundle.json: %w", err)
		}
		return &b, nil
	}
	return nil, fmt.Errorf("bundle: zip has no bundle.json")
}

// Seal encrypts plaintext to an X25519 public key: an ephemeral key pair, the
// ECDH secret through HKDF-SHA256, then AES-GCM. The envelope is the ephemeral
// public key, the nonce and the ciphertext, base64.
func Seal(publicKey, plaintext []byte) (string, error) {
	curve := ecdh.X25519()
	remote, err := curve.NewPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("bundle: public key: %w", err)
	}
	eph, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	secret, err := eph.ECDH(remote)
	if err != nil {
		return "", err
	}
	gcm, err := gcmFor(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := append(eph.PublicKey().Bytes(), nonce...)
	out = append(out, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts an envelope produced by Seal.
func Open(privateKey []byte, envelope string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(envelope)
	if err != nil {
		return nil, fmt.Errorf("bundle: envelope is not base64: %w", err)
	}
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("bundle: private key: %w", err)
	}
	if len(raw) < 32 {
		return nil, fmt.Errorf("bundle: envelope is truncated")
	}
	remote, err := curve.NewPublicKey(raw[:32])
	if err != nil {
		return nil, err
	}
	secret, err := priv.ECDH(remote)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(secret)
	if err != nil {
		return nil, err
	}
	rest := raw[32:]
	if len(rest) < gcm.NonceSize() {
		return nil, fmt.Errorf("bundle: envelope is truncated")
	}
	return gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], nil)
}

func gcmFor(secret []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, secret, nil, envelopeInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
