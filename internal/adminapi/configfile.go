package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

// sensitiveKeys are masked wherever they appear in the structured config view.
// Each of these names only ever holds a secret, so no context is needed.
// ${VAR} placeholders are kept verbatim — they reveal nothing and the operator
// needs to see which fields are env-sourced.
var sensitiveKeys = map[string]bool{
	"api_key": true, "token": true, "secret": true, "password": true, "master_key": true,
	// Missed by the original list, so these sat in cleartext in the view the
	// console labels "secrets masked": S3 keys, the Cloudflare browser token,
	// and a channel's webhook secret token.
	"secret_key": true, "access_key": true, "api_token": true, "secret_token": true,
	// A store target is a path or a DSN, and a DSN can carry a password
	// (postgres://user:pass@host/db) in every one of its four homes:
	// runtime.persistence, runtime.identity.persistence,
	// runtime.cluster.persistence and runtime.log_plane.persistence.
	"persistence": true,
}

var envPlaceholderRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`)

// maskTree returns v with sensitive string values replaced by "••••".
// Placeholders (${VAR}) pass through unchanged.
func maskTree(v any) any { return maskValue(v, nil) }

// maskValue walks v carrying the ancestor key path, because one key needs it:
// "path" is a store target in runtime.credentials.path and in a memory
// backend's config.path, but a route on the shared listener in
// gateway.channels[].path. Masking every "path" would blank the route an
// operator is auditing, in the one view they audit a config with.
func maskValue(v any, ctx []string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			lk := strings.ToLower(k)
			if sensitiveKeys[lk] ||
				(lk == "path" && isStoreTargetContext(ctx)) ||
				(lk == "url" && isMemoryBackendConfigContext(ctx)) ||
				isOperatorsContext(ctx) ||
				isTracingHeadersContext(ctx) {
				if s, ok := val.(string); ok {
					switch {
					case s == "", envPlaceholderRe.MatchString(s):
						out[k] = s
					default:
						out[k] = "••••"
					}
					continue
				}
			}
			if lk == "keys" && inChannelContext(ctx) {
				// An openai channel's shared API keys: a list where every other
				// secret is a single string, so the key-name test above does
				// not reach it.
				if list, ok := val.([]any); ok {
					masked := make([]any, len(list))
					for i, item := range list {
						s, isStr := item.(string)
						switch {
						case !isStr, s == "", envPlaceholderRe.MatchString(s):
							masked[i] = item
						default:
							masked[i] = "••••"
						}
					}
					out[k] = masked
					continue
				}
			}
			out[k] = maskValue(val, append(ctx, lk))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskValue(val, ctx)
		}
		return out
	default:
		return v
	}
}

// inChannelContext reports whether the walk is inside gateway.channels.
func inChannelContext(ctx []string) bool {
	for _, k := range ctx {
		if k == "channels" {
			return true
		}
	}
	return false
}

// isStoreTargetContext reports whether a "path" here names a store target
// rather than a route: under runtime (credentials.path) or inside a memory
// backend's config block (memory.backends.<name>.config.path).
func isStoreTargetContext(ctx []string) bool {
	for _, k := range ctx {
		if k == "runtime" || k == "config" {
			return true
		}
	}
	return false
}

// isMemoryBackendConfigContext reports whether the walk is inside
// memory.backends.<name>.config (where "url" carries a DSN password).
func isMemoryBackendConfigContext(ctx []string) bool {
	hasBackends := false
	for _, k := range ctx {
		if k == "backends" {
			hasBackends = true
		}
		if hasBackends && k == "config" {
			return true
		}
	}
	return false
}

// isOperatorsContext reports whether the walk is directly inside
// runtime.admin.operators (whose map values are bearer tokens keyed by name).
func isOperatorsContext(ctx []string) bool {
	return len(ctx) > 0 && ctx[len(ctx)-1] == "operators"
}

func isTracingHeadersContext(ctx []string) bool {
	return len(ctx) >= 2 && ctx[len(ctx)-2] == "tracing" && ctx[len(ctx)-1] == "headers"
}

// configDirFiles enumerates editable YAML fragments in a config directory in
// deterministic order: system.yaml, channels.yaml, profiles/*.yaml, triggers/*.yaml.
func (u *UI) configDirFiles() []string {
	if u.deps.ConfigDir == "" {
		return nil
	}
	var out []string
	for _, top := range []string{"system.yaml", "channels.yaml"} {
		if _, err := os.Stat(filepath.Join(u.deps.ConfigDir, top)); err == nil {
			out = append(out, top)
		}
	}
	for _, sub := range []string{"profiles", "triggers"} {
		entries, err := os.ReadDir(filepath.Join(u.deps.ConfigDir, sub))
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".yaml") {
				continue
			}
			names = append(names, sub+"/"+e.Name())
		}
		sort.Strings(names)
		out = append(out, names...)
	}
	return out
}

// resolveConfigTarget resolves the editable file path for either single-file
// mode or configdir mode (when relFile names a safe fragment inside ConfigDir).
func (u *UI) resolveConfigTarget(relFile string) (absPath, rel string, files []string, isDir bool, err error) {
	if u.deps.ConfigDir != "" {
		files = u.configDirFiles()
		if len(files) == 0 {
			return "", "", nil, true, fmt.Errorf("config directory %s has no editable YAML fragments", u.deps.ConfigDir)
		}
		rel = strings.TrimSpace(relFile)
		if rel == "" {
			rel = files[0]
		}
		// Only allow files enumerated inside the config directory (no traversal).
		allowed := false
		for _, f := range files {
			if f == rel {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", "", files, true, fmt.Errorf("unknown config fragment %q", rel)
		}
		return filepath.Join(u.deps.ConfigDir, filepath.FromSlash(rel)), rel, files, true, nil
	}
	if u.deps.ConfigPath == "" {
		return "", "", nil, false, fmt.Errorf("no config file is configured for this instance")
	}
	return u.deps.ConfigPath, filepath.Base(u.deps.ConfigPath), nil, false, nil
}

// handleConfigGet returns the raw config text (env placeholders intact — the
// operator edits this form) plus a masked structured view for browsing.
func (u *UI) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	path, rel, files, isDir, err := u.resolveConfigTarget(r.URL.Query().Get("file"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("read config: %v", err))
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("stat config: %v", err))
		return
	}
	var view any
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err == nil {
		view = maskTree(tree)
	}
	mode := "file"
	if isDir {
		mode = "configdir"
	}
	resp := map[string]any{
		"ok":    true,
		"mode":  mode,
		"path":  path,
		"file":  rel,
		"raw":   string(raw),
		"mtime": st.ModTime().Unix(),
		"view":  view,
	}
	if isDir {
		resp["files"] = files
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConfigValidate dry-runs a posted config through the exact boot
// validation path (strict fields, env expansion, cross-reference checks).
func (u *UI) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Raw  string `json:"raw"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	path, rel, _, isDir, err := u.resolveConfigTarget(req.File)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if isDir {
		err = validateConfigDirFragment(u.deps.ConfigDir, rel, req.Raw)
	} else {
		err = validateRawConfig(req.Raw, filepath.Dir(path))
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleConfigSave validates, backs up, and atomically replaces the config
// file (or selected configdir fragment). The client must send the mtime it last
// read; a mismatch means the file changed under the editor and the save is
// refused rather than clobbering it.
func (u *UI) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Raw   string `json:"raw"`
		Mtime int64  `json:"mtime"`
		File  string `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	path, rel, _, isDir, err := u.resolveConfigTarget(req.File)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("stat config: %v", err))
		return
	}
	if st.ModTime().Unix() != req.Mtime {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "config file changed on disk since you loaded it; reload before saving",
		})
		return
	}
	if isDir {
		if err := validateConfigDirFragment(u.deps.ConfigDir, rel, req.Raw); err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("does not validate: %v", err))
			return
		}
		if err := swapConfigDirFile(u.deps.ConfigDir, path, []byte(req.Raw)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := validateRawConfig(req.Raw, filepath.Dir(path)); err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("does not validate: %v", err))
			return
		}
		if err := config.ValidateAndSwap(path, []byte(req.Raw)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	st2, _ := os.Stat(path)
	var mtime int64
	if st2 != nil {
		mtime = st2.ModTime().Unix()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mtime": mtime, "file": rel,
		"note": "saved (previous file kept as .bak); restart agentflow to apply",
	})
}

// maxConfigBody bounds a posted config. A config is text, not an upload; the
// figure is generous enough for a configdir-sized deployment written as one
// document. The editor's own save path writes whatever it decoded to disk, so
// an unbounded body is an unbounded write.
const maxConfigBody = 4 << 20

// validateConfigDirFragment stages a lightweight temporary copy of the config
// directory's YAML fragments and small text assets (prompts/loops), overlays
// relFile with raw, and runs config.LoadDir against it without touching live
// files on disk.
func validateConfigDirFragment(configDir, relFile, raw string) error {
	tmpDir, err := os.MkdirTemp("", "agentflow-validate-dir-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	copyFile := func(src, dst string) error {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	}
	for _, top := range []string{"system.yaml", "channels.yaml"} {
		src := filepath.Join(configDir, top)
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, filepath.Join(tmpDir, top)); err != nil {
				return err
			}
		}
	}
	for _, sub := range []string{"profiles", "triggers", "prompts", "loops"} {
		subSrc := filepath.Join(configDir, sub)
		entries, err := os.ReadDir(subSrc)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := copyFile(filepath.Join(subSrc, e.Name()), filepath.Join(tmpDir, sub, e.Name())); err != nil {
				return err
			}
		}
	}
	dst := filepath.Join(tmpDir, filepath.FromSlash(relFile))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(raw), 0o600); err != nil {
		return err
	}
	_, err = config.LoadDir(tmpDir, nil)
	return err
}

// swapConfigDirFile backs up path to path.bak, replaces it atomically with out,
// and validates the whole config directory via config.LoadDir (restoring the
// previous file on failure).
func swapConfigDirFile(configDir, path string, out []byte) error {
	old, oldErr := os.ReadFile(path)
	if oldErr != nil && !os.IsNotExist(oldErr) {
		return oldErr
	}
	restore := func() {
		if oldErr == nil {
			_ = os.WriteFile(path, old, 0o600)
		} else {
			_ = os.Remove(path)
		}
	}
	if oldErr == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentflow-swap-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if err2 := os.WriteFile(path, out, 0o600); err2 != nil {
			return fmt.Errorf("replace %s: %w (fallback: %v)", filepath.Base(path), err, err2)
		}
	}
	_ = os.Chmod(path, 0o600)
	if _, err := config.LoadDir(configDir, nil); err != nil {
		restore()
		return fmt.Errorf("%s does not validate: %w", filepath.Base(path), err)
	}
	return nil
}

// validateRawConfig parses and validates a config text exactly as boot does,
// via a temp file so config.Load's strict decoder and env expansion apply.
//
// The temp file goes in the config's OWN directory, not the system temp dir:
// config.Load rebases relative prompt `file:` paths against the config's
// directory and refuses to boot when one cannot be read, so validating from
// somewhere else reported a false failure for a config that boots fine — and
// the Validate button disagreed with Save about the same text.
func validateRawConfig(raw, dir string) error {
	tmp, err := os.CreateTemp(dir, ".agentflow-validate-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_, err = config.Load(tmp.Name())
	return err
}
