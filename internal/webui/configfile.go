package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
			if sensitiveKeys[lk] || (lk == "path" && isStoreTargetContext(ctx)) {
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

// configFilePath returns the config file the console edits, or a named reason
// it cannot edit one.
//
// A -configdir instance has no single config file: it is several fragments
// (system.yaml, channels.yaml, profiles/*.yaml, triggers/*.yaml), and both the
// editor and the model-persist path assume one file to swap. Before this the
// console silently pointed at the -config default ("agentflow.yaml"), which
// under -configdir is a file that usually does not exist — so the Config tab
// 500ed, and if a stray agentflow.yaml happened to be in the working directory
// the console would have edited that instead.
func (u *UI) configFilePath() (string, error) {
	if u.deps.ConfigDir != "" {
		return "", fmt.Errorf("this instance runs from the config directory %s; edit the fragments there and restart (the console edits a single config file)", u.deps.ConfigDir)
	}
	if u.deps.ConfigPath == "" {
		return "", fmt.Errorf("no config file is configured for this instance")
	}
	return u.deps.ConfigPath, nil
}

// handleConfigGet returns the raw config text (env placeholders intact — the
// operator edits this form) plus a masked structured view for browsing.
func (u *UI) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	path, err := u.configFilePath()
	if err != nil {
		// A named reason rather than a 500: nothing is broken, this instance
		// just does not have the shape this pane edits.
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"path":  path,
		"raw":   string(raw),
		"mtime": st.ModTime().Unix(),
		"view":  view,
	})
}

// handleConfigValidate dry-runs a posted config through the exact boot
// validation path (strict fields, env expansion, cross-reference checks).
func (u *UI) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	path, err := u.configFilePath()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateRawConfig(req.Raw, filepath.Dir(path)); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleConfigSave validates, backs up, and atomically replaces the config
// file. The client must send the mtime it last read; a mismatch means the
// file changed under the editor (e.g. a hand edit) and the save is refused
// rather than clobbering it.
func (u *UI) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	path, err := u.configFilePath()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		Raw   string `json:"raw"`
		Mtime int64  `json:"mtime"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
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
	if err := validateRawConfig(req.Raw, filepath.Dir(path)); err != nil {
		writeJSON(w, http.StatusBadRequest, fmt.Sprintf("does not validate: %v", err))
		return
	}
	if err := validateThenSwap(path, []byte(req.Raw)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st2, _ := os.Stat(path)
	var mtime int64
	if st2 != nil {
		mtime = st2.ModTime().Unix()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mtime": mtime,
		"note": "saved (previous file kept as .bak); restart agentflow to apply",
	})
}

// maxConfigBody bounds a posted config. A config is text, not an upload; the
// figure is generous enough for a configdir-sized deployment written as one
// document. The editor's own save path writes whatever it decoded to disk, so
// an unbounded body is an unbounded write.
const maxConfigBody = 4 << 20

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
