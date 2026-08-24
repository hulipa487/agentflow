package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

// sensitiveKeys are masked in the structured config view. ${VAR} placeholders
// are kept verbatim — they reveal nothing and the operator needs to see which
// fields are env-sourced.
var sensitiveKeys = map[string]bool{
	"api_key": true, "token": true, "secret": true, "password": true, "master_key": true,
}

var envPlaceholderRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`)

// maskTree returns v with sensitive string values replaced by "••••".
// Placeholders (${VAR}) pass through unchanged.
func maskTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveKeys[strings.ToLower(k)] {
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
			out[k] = maskTree(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskTree(val)
		}
		return out
	default:
		return v
	}
}

// handleConfigGet returns the raw config text (env placeholders intact — the
// operator edits this form) plus a masked structured view for browsing.
func (u *UI) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(u.deps.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("read config: %v", err))
		return
	}
	st, err := os.Stat(u.deps.ConfigPath)
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
		"path":  u.deps.ConfigPath,
		"raw":   string(raw),
		"mtime": st.ModTime().Unix(),
		"view":  view,
	})
}

// handleConfigValidate dry-runs a posted config through the exact boot
// validation path (strict fields, env expansion, cross-reference checks).
func (u *UI) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateRawConfig(req.Raw); err != nil {
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
	var req struct {
		Raw   string `json:"raw"`
		Mtime int64  `json:"mtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	st, err := os.Stat(u.deps.ConfigPath)
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
	if err := validateRawConfig(req.Raw); err != nil {
		writeJSON(w, http.StatusBadRequest, fmt.Sprintf("does not validate: %v", err))
		return
	}
	if err := validateThenSwap(u.deps.ConfigPath, []byte(req.Raw)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st2, _ := os.Stat(u.deps.ConfigPath)
	var mtime int64
	if st2 != nil {
		mtime = st2.ModTime().Unix()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mtime": mtime,
		"note": "saved (previous file kept as .bak); restart agentflow to apply",
	})
}

// validateRawConfig parses and validates a config text exactly as boot does,
// via a temp file so config.Load's strict decoder and env expansion apply.
func validateRawConfig(raw string) error {
	tmp, err := os.CreateTemp("", "agentflow-validate-*.yaml")
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
