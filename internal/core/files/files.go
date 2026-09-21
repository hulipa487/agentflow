// Package files implements the user-scoped file store: named project trees
// under an engine-resolved scope (user UUID for channel-originated turns,
// agent name otherwise), engine-native snapshot versioning (commits with a
// parent chain + named refs — deliberately not a git server), and a
// per-session scratch space with TTL.
//
// Bytes live in the same content-addressed blob store family as media
// (media.Store; fs or S3), so handles stay backend-agnostic and unchanged
// files dedupe across snapshots for free. Metadata lives in the runtime
// store's files_meta table, engine-owned like the message journal.
//
// Design rule (same as media): raw bytes never cross the Lua bridge. Loops
// see paths, handles, and metadata; materialization is engine-side.
package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
)

// maxPath bounds one path segment chain; paths are metadata keys, not blobs.
const maxPath = 512

var (
	// ErrNotFound is returned for missing working-tree entries, refs, and
	// commits — and for scratch records past their TTL (lazy expiry).
	ErrNotFound = errors.New("not found")
	// ErrBadPath rejects paths that could traverse or forge keys.
	ErrBadPath = errors.New("invalid path")
)

// Entry is one working-tree or scratch record: a named blob.
type Entry struct {
	Path     string `json:"path"`
	Handle   string `json:"handle"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime,omitempty"`
	Ts       int64  `json:"ts"`
	Revision int64  `json:"revision"` // working-tree writes bump this per path
}

// Commit is one snapshot: the full tree (path → handle) plus parent/message.
// Because handles are content-addressed, a commit is metadata only — no bytes
// are copied — and unchanged files are shared with every other snapshot.
type Commit struct {
	ID      string            `json:"id"`
	Parent  string            `json:"parent,omitempty"`
	Ts      int64             `json:"ts"`
	Message string            `json:"message"`
	Tree    map[string]string `json:"tree"` // path → handle
}

// Manifest is what a checkout resolves to: the commit plus the file list a
// consumer materializes (engine-side) from.
type Manifest struct {
	Commit Commit  `json:"commit"`
	Files  []Entry `json:"files"` // tree entries with sizes resolved
}

// Manager owns the file store: blobs + metadata + scratch policy.
type Manager struct {
	blobs    media.Store
	meta     *runtime.Store
	ttl      time.Duration // 0 = scratch never expires
	maxBytes int64
	now      func() time.Time // test hook
	log      *slog.Logger
}

// New builds a Manager over the given blob store and runtime store. ttl is
// the scratch TTL (0 disables expiry); maxBytes caps one file's size.
func New(blobs media.Store, meta *runtime.Store, ttl time.Duration, maxBytes int64, log *slog.Logger) *Manager {
	if maxBytes <= 0 {
		maxBytes = media.Policy{}.MaxOrDefault()
	}
	return &Manager{
		blobs:    blobs,
		meta:     meta,
		ttl:      ttl,
		maxBytes: maxBytes,
		now:      time.Now,
		log:      log.With("module", "files"),
	}
}

// validPath enforces the path contract: a relative, normalized, slash-only
// path with no traversal. Paths become part of metadata keys, so a hostile
// path must never be able to reach another scope.
func validPath(p string) error {
	if p == "" || len(p) > maxPath || !utf8.ValidString(p) {
		return fmt.Errorf("%w: empty/oversized path", ErrBadPath)
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return fmt.Errorf("%w: %q is absolute", ErrBadPath, p)
	}
	if strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("%w: %q contains forbidden characters", ErrBadPath, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q is not normalized", ErrBadPath, p)
		}
	}
	return nil
}

// key spaces: working tree, refs, commits are (scope, project, name);
// scratch is per-owner, not per-user-scoped.
func treeKey(scope, project, p string) string      { return "t|" + scope + "|" + project + "|" + p }
func treePrefix(scope, project, pfx string) string { return "t|" + scope + "|" + project + "|" + pfx }
func refKey(scope, project, ref string) string     { return "r|" + scope + "|" + project + "|" + ref }
func commitKey(scope, project, id string) string   { return "c|" + scope + "|" + project + "|" + id }
func scratchKey(owner, name string) string         { return "s|" + owner + "|" + name }

const allTreePrefix = "t|"

// Put writes one file into the working tree, content-addressed; the returned
// entry's Revision is the previous revision + 1 (1 for a new path).
func (m *Manager) Put(ctx context.Context, scope, project, p string, r io.Reader, mime string) (*Entry, error) {
	if err := validPath(p); err != nil {
		return nil, err
	}
	ref, err := m.blobs.Put(r, mime, media.Policy{Allow: []string{"*"}, MaxBytes: m.maxBytes})
	if err != nil {
		return nil, err
	}
	var rev int64 = 1
	if prev, ok, err := m.getTree(ctx, scope, project, p); err != nil {
		return nil, err
	} else if ok {
		rev = prev.Revision + 1
	}
	e := &Entry{Path: p, Handle: ref.Handle, Size: ref.Size, MIME: mime, Ts: m.now().Unix(), Revision: rev}
	if err := m.putJSON(ctx, treeKey(scope, project, p), e, time.Time{}); err != nil {
		return nil, err
	}
	return e, nil
}

// PutHandle records a working-tree entry pointing at an existing blob
// handle — no bytes move (the tree IS handles: rollback restores a commit's
// tree by re-recording its handles). The handle format is validated and its
// existence probed so a typo fails here instead of at checkout
// materialization. Size is unknown without reading bytes and stays 0 —
// manifests treat size as informational and consumers read by handle.
func (m *Manager) PutHandle(ctx context.Context, scope, project, p, handle, mime string) (*Entry, error) {
	if err := validPath(p); err != nil {
		return nil, err
	}
	if err := m.checkHandle(ctx, handle); err != nil {
		return nil, err
	}
	var rev int64 = 1
	if prev, ok, err := m.getTree(ctx, scope, project, p); err != nil {
		return nil, err
	} else if ok {
		rev = prev.Revision + 1
	}
	e := &Entry{Path: p, Handle: handle, MIME: mime, Ts: m.now().Unix(), Revision: rev}
	if err := m.putJSON(ctx, treeKey(scope, project, p), e, time.Time{}); err != nil {
		return nil, err
	}
	return e, nil
}

// ScratchPutHandle is PutHandle for the per-session scratch space.
func (m *Manager) ScratchPutHandle(ctx context.Context, owner, name, handle, mime string) (*Entry, error) {
	if err := validPath(name); err != nil {
		return nil, err
	}
	if err := m.checkHandle(ctx, handle); err != nil {
		return nil, err
	}
	var rev int64 = 1
	if prev, err := m.ScratchGet(ctx, owner, name); err == nil {
		rev = prev.Revision + 1
	}
	var exp time.Time
	if m.ttl > 0 {
		exp = m.now().Add(m.ttl)
	}
	e := &Entry{Path: name, Handle: handle, MIME: mime, Ts: m.now().Unix(), Revision: rev}
	if err := m.putJSON(ctx, scratchKey(owner, name), e, exp); err != nil {
		return nil, err
	}
	return e, nil
}

// checkHandle validates the handle format and probes that the blob exists.
func (m *Manager) checkHandle(ctx context.Context, handle string) error {
	if !media.ValidHandle(handle) {
		return fmt.Errorf("%w: malformed handle %q", ErrBadPath, handle)
	}
	// Probe with a small read: nil means the blob exists; ErrExceedsLimit
	// means it exists and is larger than the probe. Anything else (not
	// found, backend failure) fails the put.
	if _, err := m.blobs.ReadAll(handle, 512); err != nil && !errors.Is(err, media.ErrExceedsLimit) {
		return fmt.Errorf("files: handle %s not found in blob store: %w", handle, err)
	}
	return nil
}

// Get returns the working-tree entry for one path (handle + metadata; never
// bytes).
func (m *Manager) Get(ctx context.Context, scope, project, p string) (*Entry, error) {
	if err := validPath(p); err != nil {
		return nil, err
	}
	e, ok, err := m.getTree(ctx, scope, project, p)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	return e, nil
}

// Projects lists the project names present in a scope. A project is not a
// registry entity — it comes into existence when something is written under its
// name — so enumerating is a key scan with the project segment extracted. The
// scope is the caller's ("user:<uuid>" or "agent:<name>"); a caller can never
// discover another scope's projects.
func (m *Manager) Projects(ctx context.Context, scope string) ([]string, error) {
	if scope == "" {
		return nil, errors.New("files: scope is required")
	}
	prefix := allTreePrefix + scope + "|"
	rows, err := m.meta.ListRows(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("files: list projects: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		rest := strings.TrimPrefix(r.Key, prefix)
		// A tree key is "<project>|<path>": everything up to the first
		// separator is the project.
		if i := strings.Index(rest, "|"); i > 0 {
			seen[rest[:i]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// List returns working-tree entries under a path prefix ("" = the whole
// tree), sorted by path. A prefix is a path fragment, not a stored path:
// trailing slashes are tolerated ("src/" ≡ "src").
func (m *Manager) List(ctx context.Context, scope, project, prefix string) ([]Entry, error) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" {
		if err := validPath(prefix); err != nil {
			return nil, err
		}
	}
	rows, err := m.meta.ListRows(ctx, treePrefix(scope, project, prefix))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		var e Entry
		if err := json.Unmarshal([]byte(r.Value), &e); err != nil {
			return nil, fmt.Errorf("files: corrupt tree record %q: %w", r.Key, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// Delete removes one working-tree entry (the blob itself is content-addressed
// and may be shared — it is never deleted here).
func (m *Manager) Delete(ctx context.Context, scope, project, p string) error {
	if err := validPath(p); err != nil {
		return err
	}
	return m.meta.DeleteRow(ctx, treeKey(scope, project, p))
}

// Commit snapshots the current working tree as a new commit and moves ref to
// it. The parent is the commit ref currently points at (empty for the first
// commit) — the parent chain is the history. Returns the new commit.
func (m *Manager) Commit(ctx context.Context, scope, project, ref, message string) (*Commit, error) {
	if ref == "" || strings.ContainsAny(ref, "|\x00") {
		return nil, fmt.Errorf("%w: invalid ref %q", ErrBadPath, ref)
	}
	entries, err := m.List(ctx, scope, project, "")
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("cannot commit an empty tree")
	}
	c := &Commit{ID: newID(), Ts: m.now().Unix(), Message: message, Tree: make(map[string]string, len(entries))}
	for _, e := range entries {
		c.Tree[e.Path] = e.Handle
	}
	if prev, ok, err := m.meta.GetRow(ctx, refKey(scope, project, ref)); err != nil {
		return nil, err
	} else if ok {
		c.Parent = jsonString(prev.Value)
	}
	if err := m.putJSON(ctx, commitKey(scope, project, c.ID), c, time.Time{}); err != nil {
		return nil, err
	}
	if err := m.putJSON(ctx, refKey(scope, project, ref), c.ID, time.Time{}); err != nil {
		return nil, err
	}
	return c, nil
}

// Checkout resolves a ref name or a commit id to a manifest. A ref resolves
// through the ref record; anything else must be a commit id. Missing targets
// are ErrNotFound, so a loop can distinguish "no such ref" from empty trees.
func (m *Manager) Checkout(ctx context.Context, scope, project, target string) (*Manifest, error) {
	id := target
	if row, ok, err := m.meta.GetRow(ctx, refKey(scope, project, target)); err != nil {
		return nil, err
	} else if ok {
		id = jsonString(row.Value)
	}
	row, ok, err := m.meta.GetRow(ctx, commitKey(scope, project, id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: ref/commit %q", ErrNotFound, target)
	}
	var c Commit
	if err := json.Unmarshal([]byte(row.Value), &c); err != nil {
		return nil, fmt.Errorf("files: corrupt commit %q: %w", id, err)
	}
	// Sizes come from the working tree when the path still exists; a file
	// deleted since the commit reports size 0 (the handle is still valid —
	// content-addressed blobs are never removed).
	paths := make([]string, 0, len(c.Tree))
	for p := range c.Tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := make([]Entry, 0, len(paths))
	for _, p := range paths {
		e := Entry{Path: p, Handle: c.Tree[p]}
		if cur, ok, err := m.getTree(ctx, scope, project, p); err == nil && ok && cur.Handle == e.Handle {
			e.Size = cur.Size
			e.MIME = cur.MIME
		}
		files = append(files, e)
	}
	return &Manifest{Commit: c, Files: files}, nil
}

// ScratchPut writes one named scratch record for an owner session.
func (m *Manager) ScratchPut(ctx context.Context, owner, name string, r io.Reader, mime string) (*Entry, error) {
	if err := validPath(name); err != nil {
		return nil, err
	}
	ref, err := m.blobs.Put(r, mime, media.Policy{Allow: []string{"*"}, MaxBytes: m.maxBytes})
	if err != nil {
		return nil, err
	}
	e := &Entry{Path: name, Handle: ref.Handle, Size: ref.Size, MIME: mime, Ts: m.now().Unix(), Revision: 1}
	var exp time.Time
	if m.ttl > 0 {
		exp = m.now().Add(m.ttl)
	}
	if err := m.putJSON(ctx, scratchKey(owner, name), e, exp); err != nil {
		return nil, err
	}
	return e, nil
}

// ScratchGet returns a scratch record, honoring TTL lazily: an expired record
// is deleted and reported as ErrNotFound.
func (m *Manager) ScratchGet(ctx context.Context, owner, name string) (*Entry, error) {
	if err := validPath(name); err != nil {
		return nil, err
	}
	row, ok, err := m.meta.GetRow(ctx, scratchKey(owner, name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: scratch %q", ErrNotFound, name)
	}
	var e Entry
	if err := json.Unmarshal([]byte(row.Value), &e); err != nil {
		return nil, fmt.Errorf("files: corrupt scratch record: %w", err)
	}
	return &e, nil
}

// ScratchList returns one owner's live scratch records, sorted by name.
func (m *Manager) ScratchList(ctx context.Context, owner string) ([]Entry, error) {
	rows, err := m.meta.ListRows(ctx, "s|"+owner+"|")
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		var e Entry
		if err := json.Unmarshal([]byte(r.Value), &e); err != nil {
			return nil, fmt.Errorf("files: corrupt scratch record %q: %w", r.Key, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// SweepScratch deletes expired scratch rows (all owners) and returns the count.
func (m *Manager) SweepScratch(ctx context.Context) (int, error) {
	return m.meta.SweepExpired(ctx)
}

// MaxFileBytes returns the per-file ceiling engine consumers (checkout
// materialization, scratch mounts) apply when reading blobs back.
func (m *Manager) MaxFileBytes() int64 { return m.maxBytes }

// GC is a mark-sweep over the blob store. The live set is every handle
// referenced by files_meta: working-tree records, every commit's tree
// (rollback must keep old commits' blobs alive — sweeping them would defeat
// checkout), and scratch records. Unreferenced blobs older than grace are
// deleted; a grace of zero sweeps regardless of age (the boot wiring only
// ever passes positive values; zero/negative is for tests and forced
// operator sweeps). The put write order is blob-then-metadata, so anything
// younger than the grace is referenced or mid-transaction — the grace window
// is what makes marking safe without locking. Returns (live, swept).
func (m *Manager) GC(ctx context.Context, grace time.Duration) (int, int, error) {
	live, err := m.liveHandles(ctx)
	if err != nil {
		return 0, 0, err
	}
	blobs, err := m.blobs.List()
	if err != nil {
		return 0, 0, err
	}
	swept := 0
	cutoff := m.now().Add(-grace)
	for _, b := range blobs {
		if live[b.Handle] {
			continue
		}
		if grace > 0 && b.ModTime.After(cutoff) {
			continue // too young: referenced or mid-transaction
		}
		if err := m.blobs.Delete(b.Handle); err != nil {
			return len(live), swept, err
		}
		swept++
	}
	return len(live), swept, nil
}

// liveHandles collects every blob handle referenced by files_meta: tree
// records (t|), commit trees (c|), and scratch records (s|).
func (m *Manager) liveHandles(ctx context.Context) (map[string]bool, error) {
	live := map[string]bool{}
	for _, prefix := range []string{"t|", "c|", "s|"} {
		rows, err := m.meta.ListRows(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			switch r.Key[0] {
			case 't', 's':
				var e Entry
				if err := json.Unmarshal([]byte(r.Value), &e); err != nil {
					return nil, fmt.Errorf("files: corrupt record %q: %w", r.Key, err)
				}
				live[e.Handle] = true
			case 'c':
				var c Commit
				if err := json.Unmarshal([]byte(r.Value), &c); err != nil {
					return nil, fmt.Errorf("files: corrupt commit %q: %w", r.Key, err)
				}
				for _, h := range c.Tree {
					live[h] = true
				}
			}
		}
	}
	return live, nil
}

// ReadBlob materializes blob bytes engine-side (checkout, shell mounts,
// fetch save_to). Loops never see this — it exists for engine consumers.
func (m *Manager) ReadBlob(ctx context.Context, handle string, limit int64) ([]byte, error) {
	return m.blobs.ReadAll(handle, limit)
}

// --- helpers -----------------------------------------------------------------

func (m *Manager) getTree(ctx context.Context, scope, project, p string) (*Entry, bool, error) {
	row, ok, err := m.meta.GetRow(ctx, treeKey(scope, project, p))
	if err != nil || !ok {
		return nil, ok, err
	}
	var e Entry
	if err := json.Unmarshal([]byte(row.Value), &e); err != nil {
		return nil, false, fmt.Errorf("files: corrupt tree record %q: %w", row.Key, err)
	}
	return &e, true, nil
}

func (m *Manager) putJSON(ctx context.Context, key string, v any, exp time.Time) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.meta.PutRow(ctx, key, string(b), exp)
}

// newID mints a commit id: nanos + random suffix, unique per process run and
// collision-safe across restarts for any realistic commit rate.
func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// jsonString unmarshals a stored JSON string value (ref records hold a bare
// commit id string).
func jsonString(s string) string {
	var v string
	_ = json.Unmarshal([]byte(s), &v)
	return v
}
