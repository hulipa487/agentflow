// Package workspace keeps a directory on this host in step with a project's
// working tree in the files store, in both directions: Prepare makes the
// directory match the tree before a sandbox job, Collect writes what the job
// changed back into the tree afterwards.
//
// The files store is the truth and the directory is a cache of it. Nothing
// here is needed to recover a project: delete the directory and the next
// Prepare rebuilds it, on this instance or any other. That is what lets a
// shell job run anywhere without a handle that follows the session around.
//
// The package does no locking. Its caller runs one job at a time per project
// on this instance; two instances working on one project at once are kept
// apart by the revision rule in Collect, not by a lock.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"agentflow/internal/core/files"
)

// Limits bound what one job may write back. Zero means no limit of that kind;
// the files store's own per-file ceiling always applies.
type Limits struct {
	MaxFiles int   // changed files per job
	MaxBytes int64 // total bytes of changed files per job
}

// Skip is a path a job produced that was not written back, and why.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Changes is what a job did to its project, as recorded in the files store.
// Every list is sorted, and empty lists are non-nil so they encode as [].
type Changes struct {
	Changed []string `json:"changed"`
	Deleted []string `json:"deleted"`
	// Conflicts are paths the job changed that somebody else changed while it
	// ran. The store's version is kept; the job's is discarded.
	Conflicts []string `json:"conflicts"`
	Skipped   []Skip   `json:"skipped"`
}

// Manager owns the workspace directories under one root.
type Manager struct {
	root string
	fm   *files.Manager
	log  *slog.Logger
}

// New builds a workspace manager. root is created on first use.
func New(root string, fm *files.Manager, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{root: root, fm: fm, log: log.With("module", "workspace")}
}

// fileState is what Prepare knows about a file it put in the directory: which
// tree entry it came from, and what the file looked like on disk when written.
// Collect compares the disk against the second half to find changes cheaply,
// and the tree against the first half to find conflicts.
type fileState struct {
	Handle   string `json:"handle"`
	Revision int64  `json:"revision"`
	Exec     bool   `json:"exec,omitempty"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"` // nanoseconds
}

// Workspace is one prepared project directory, valid for one job.
type Workspace struct {
	// Dir is the absolute host directory to mount into the sandbox.
	Dir string

	m       *Manager
	scope   string
	project string
	state   map[string]fileState
	stateAt string
	ignore  Ignore
}

// Key is the stable identity of a project's workspace on this instance. The
// queue uses it to run one job per project at a time.
func Key(scope, project string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + project))
	return hex.EncodeToString(sum[:16])
}

// Prepare makes the project's directory match its working tree and returns it.
// Files already in place from an earlier job are left alone; only what differs
// is written. Paths matched by ignore are not touched in either direction.
func (m *Manager) Prepare(ctx context.Context, scope, project string, ignore Ignore) (*Workspace, error) {
	if m.fm == nil {
		return nil, errors.New("workspace: the files store is unavailable (disabled at boot)")
	}
	base, err := filepath.Abs(filepath.Join(m.root, Key(scope, project)))
	if err != nil {
		return nil, err
	}
	w := &Workspace{
		Dir:     filepath.Join(base, "tree"),
		m:       m,
		scope:   scope,
		project: project,
		stateAt: filepath.Join(base, "state.json"),
		ignore:  ignore,
	}
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	entries, err := m.fm.List(ctx, scope, project, "")
	if err != nil {
		return nil, fmt.Errorf("workspace: list %s: %w", project, err)
	}

	// State from the last job on this instance. A missing or unreadable file
	// is an empty state: every file is then rewritten, which is always right.
	old := map[string]fileState{}
	if raw, err := os.ReadFile(w.stateAt); err == nil {
		_ = json.Unmarshal(raw, &old)
	}

	// The store is a flat map of paths, so it can hold both "a" and "a/b". A
	// directory cannot. The file loses: it is left out of the directory and
	// out of the state, so the job does not see it and write-back cannot
	// mistake its absence for a deletion.
	isDir := map[string]bool{}
	for _, e := range entries {
		for d := path.Dir(e.Path); d != "."; d = path.Dir(d) {
			isDir[d] = true
		}
	}

	w.state = make(map[string]fileState, len(entries))
	wanted := make(map[string]bool, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isDir[e.Path] {
			m.log.Warn("a stored file has the same path as a directory and is left out of the workspace",
				"project", project, "path", e.Path)
			continue
		}
		if w.ignore.Match(e.Path, false) {
			// Stored before the pattern was configured. It stays in the store
			// and out of the directory: an ignored path is not the job's.
			continue
		}
		wanted[e.Path] = true
		target := filepath.Join(w.Dir, filepath.FromSlash(e.Path))
		if st, ok := old[e.Path]; ok && st.Handle == e.Handle && st.Exec == e.Exec {
			if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() &&
				info.Size() == st.Size && info.ModTime().UnixNano() == st.MTime {
				st.Revision = e.Revision
				w.state[e.Path] = st
				continue
			}
		}
		st, err := w.materialize(ctx, e, target)
		if err != nil {
			return nil, err
		}
		w.state[e.Path] = st
	}

	// Remove what the tree no longer has: files deleted from the store since
	// the last job, and anything a previous job left that was not stored.
	if err := w.walk(func(rel string, d fs.DirEntry) error {
		if !wanted[rel] {
			return os.Remove(filepath.Join(w.Dir, filepath.FromSlash(rel)))
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("workspace: clean %s: %w", project, err)
	}
	w.pruneEmptyDirs()
	if err := w.saveState(); err != nil {
		return nil, err
	}
	return w, nil
}

// materialize writes one tree entry to disk and reports what it wrote.
//
// The directory is not trusted: a job ran in it, and a job can leave a
// symbolic link anywhere it likes — where a parent directory should be, or
// where this function's own temporary file goes. Nothing here follows one.
// Parents are checked component by component and anything that is not a real
// directory is replaced; the file itself is created exclusively, through a
// handle on the workspace directory that refuses any path leading out of it.
func (w *Workspace) materialize(ctx context.Context, e files.Entry, target string) (fileState, error) {
	b, err := w.m.fm.ReadBlob(ctx, e.Handle, w.m.fm.MaxFileBytes())
	if err != nil {
		return fileState{}, fmt.Errorf("workspace: read %s: %w", e.Path, err)
	}
	fail := func(err error) (fileState, error) {
		return fileState{}, fmt.Errorf("workspace: %s: %w", e.Path, err)
	}
	if err := makeParents(w.Dir, e.Path); err != nil {
		return fail(err)
	}
	// A directory or link where the file belongs is something a job left
	// behind; the tree says a file goes here.
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return fail(err)
		}
	}
	root, err := os.OpenRoot(w.Dir)
	if err != nil {
		return fail(err)
	}
	defer root.Close()

	mode := os.FileMode(0o644)
	if e.Exec {
		mode = 0o755
	}
	rel := filepath.FromSlash(e.Path)
	tmp := rel + ".af-tmp"
	_ = root.RemoveAll(tmp)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fail(err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// The mode given at creation is filtered by the umask; the bit must
		// be exact.
		err = root.Chmod(tmp, mode)
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return fail(err)
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return fail(err)
	}
	return fileState{Handle: e.Handle, Revision: e.Revision, Exec: e.Exec, Size: info.Size(), MTime: info.ModTime().UnixNano()}, nil
}

// makeParents creates the directories above a workspace path. Each component
// is examined without following it: a file or a link where a directory
// belongs is removed and replaced by a directory.
func makeParents(root, rel string) error {
	cur := root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			if err := os.Remove(cur); err != nil {
				return err
			}
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// walk visits every entry in the directory that is not a directory and not
// ignored, with its slash path. Ignored directories are not entered.
func (w *Workspace) walk(visit func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(w.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == w.Dir {
			return nil
		}
		rel, err := filepath.Rel(w.Dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if w.ignore.Match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if w.ignore.Match(rel, false) {
			return nil
		}
		return visit(rel, d)
	})
}

// pruneEmptyDirs removes directories that hold nothing, deepest first. The
// files store has no directories, so an empty one cannot be reproduced on
// another instance and is not kept on this one.
func (w *Workspace) pruneEmptyDirs() {
	var dirs []string
	_ = filepath.WalkDir(w.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == w.Dir {
			return nil
		}
		if rel, rerr := filepath.Rel(w.Dir, p); rerr == nil && w.ignore.Match(filepath.ToSlash(rel), true) {
			return filepath.SkipDir
		}
		dirs = append(dirs, p)
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails, as intended, when not empty
	}
}

func (w *Workspace) saveState() error {
	raw, err := json.Marshal(w.state)
	if err != nil {
		return err
	}
	tmp := w.stateAt + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("workspace: save state: %w", err)
	}
	if err := os.Rename(tmp, w.stateAt); err != nil {
		return fmt.Errorf("workspace: save state: %w", err)
	}
	return nil
}

// Has reports whether a directory exists in the workspace. The caller checks a
// job's working directory with it before handing the workspace to a sandbox.
func (w *Workspace) Has(dir string) bool {
	if dir == "" {
		return true
	}
	clean := path.Clean(dir)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	info, err := os.Stat(filepath.Join(w.Dir, filepath.FromSlash(clean)))
	return err == nil && info.IsDir()
}

// Collect writes what a job changed back into the project's working tree and
// brings the directory back in line with it.
//
// A path is written back only if the tree still holds the revision the job
// started from. If someone else wrote or deleted it meanwhile, the store's
// version stands and the path is reported as a conflict: a job that ran for a
// minute must not silently undo what the user did during that minute.
//
// Whatever is not written back — too large, over the job's limits, not a
// regular file — is removed from the directory and reported, so the directory
// never holds something the store does not: the next job, here or on another
// instance, sees exactly the tree.
func (w *Workspace) Collect(ctx context.Context, lim Limits) (*Changes, error) {
	ch := &Changes{Changed: []string{}, Deleted: []string{}, Conflicts: []string{}, Skipped: []Skip{}}
	fm := w.m.fm
	seen := make(map[string]bool, len(w.state))
	var nFiles, nBytes int64
	maxFile := fm.MaxFileBytes()

	skip := func(rel, reason string) {
		ch.Skipped = append(ch.Skipped, Skip{Path: rel, Reason: reason})
		_ = os.Remove(filepath.Join(w.Dir, filepath.FromSlash(rel)))
		delete(w.state, rel)
	}

	err := w.walk(func(rel string, d fs.DirEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seen[rel] = true
		full := filepath.Join(w.Dir, filepath.FromSlash(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// Over a tracked file this also forgets the file, so the next
			// Prepare puts the stored version back.
			skip(rel, "not a regular file (links and special files are not stored)")
			return nil
		}
		exec := info.Mode()&0o111 != 0
		st, tracked := w.state[rel]
		if tracked && info.Size() == st.Size && info.ModTime().UnixNano() == st.MTime && exec == st.Exec {
			return nil
		}
		if reason := unstorable(rel); reason != "" {
			skip(rel, reason)
			return nil
		}
		if info.Size() > maxFile {
			skip(rel, fmt.Sprintf("larger than the %d MiB per-file limit", maxFile>>20))
			return nil
		}
		handle, err := hashFile(full)
		if err != nil {
			return err
		}
		if tracked && handle == st.Handle && exec == st.Exec {
			// Rewritten with the same content: nothing to store, but remember
			// the new timestamp so the next job does not hash it again.
			st.Size, st.MTime = info.Size(), info.ModTime().UnixNano()
			w.state[rel] = st
			return nil
		}
		if (lim.MaxFiles > 0 && nFiles+1 > int64(lim.MaxFiles)) || (lim.MaxBytes > 0 && nBytes+info.Size() > lim.MaxBytes) {
			skip(rel, "over the job's write-back limit")
			return nil
		}

		cur, err := fm.Get(ctx, w.scope, w.project, rel)
		switch {
		case errors.Is(err, files.ErrNotFound):
			cur = nil
		case err != nil:
			return fmt.Errorf("workspace: check %s: %w", rel, err)
		}
		if (tracked && (cur == nil || cur.Revision != st.Revision)) || (!tracked && cur != nil) {
			ch.Conflicts = append(ch.Conflicts, rel)
			// Forget the file so the next Prepare restores the store's version.
			delete(w.state, rel)
			return nil
		}

		f, err := os.Open(full)
		if err != nil {
			return err
		}
		e, err := fm.PutFile(ctx, w.scope, w.project, rel, f, mimeOf(rel, full), exec)
		f.Close()
		if err != nil {
			return fmt.Errorf("workspace: store %s: %w", rel, err)
		}
		nFiles++
		nBytes += info.Size()
		ch.Changed = append(ch.Changed, rel)
		w.state[rel] = fileState{Handle: e.Handle, Revision: e.Revision, Exec: exec, Size: info.Size(), MTime: info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Tracked files the job removed.
	for rel, st := range w.state {
		if seen[rel] {
			continue
		}
		cur, err := fm.Get(ctx, w.scope, w.project, rel)
		switch {
		case errors.Is(err, files.ErrNotFound):
			// Already gone from the store: the job and someone else agree.
		case err != nil:
			return nil, fmt.Errorf("workspace: check %s: %w", rel, err)
		case cur.Revision != st.Revision:
			ch.Conflicts = append(ch.Conflicts, rel)
		default:
			if err := fm.Delete(ctx, w.scope, w.project, rel); err != nil {
				return nil, fmt.Errorf("workspace: delete %s: %w", rel, err)
			}
			ch.Deleted = append(ch.Deleted, rel)
		}
		delete(w.state, rel)
	}

	sort.Strings(ch.Changed)
	sort.Strings(ch.Deleted)
	sort.Strings(ch.Conflicts)
	sort.Slice(ch.Skipped, func(i, j int) bool { return ch.Skipped[i].Path < ch.Skipped[j].Path })
	w.pruneEmptyDirs()
	if err := w.saveState(); err != nil {
		return nil, err
	}
	return ch, nil
}

// unstorable says why a path cannot be a files-store path, or "". The store
// has its own check; this one gives the job's author a reason instead of an
// error that fails the whole write-back.
func unstorable(rel string) string {
	switch {
	case !utf8.ValidString(rel):
		return "the name is not valid UTF-8"
	case len(rel) > 512:
		return "the path is longer than 512 bytes"
	case strings.ContainsAny(rel, "\\\x00|"):
		return "the name contains a character the files store does not allow"
	}
	return ""
}

// hashFile computes the files-store handle a file's content would get.
func hashFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return "media:" + hex.EncodeToString(sum.Sum(nil)), nil
}

// mimeOf guesses a file's type from its name, then from its first bytes.
func mimeOf(rel, full string) string {
	if t := mime.TypeByExtension(path.Ext(rel)); t != "" {
		return t
	}
	f, err := os.Open(full)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return http.DetectContentType(head[:n])
}
