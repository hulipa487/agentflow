package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/drivers/llm"
)

// modelNameRe keeps model names path- and YAML-safe.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// modelBody is the JSON shape for model upserts (mirrors config.Model).
type modelBody struct {
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	APIKey      string   `json:"api_key"`
	BaseURL     string   `json:"base_url"`
	Timeout     string   `json:"timeout"`
	Retry       int      `json:"retry"`
	MaxTokens   int      `json:"max_tokens"`
	Thinking    string   `json:"thinking"`
	ServerTools []string `json:"server_tools"`
}

func (b modelBody) toConfig() config.Model {
	return config.Model{
		Provider:    b.Provider,
		Model:       b.Model,
		APIKey:      b.APIKey,
		BaseURL:     b.BaseURL,
		Timeout:     b.Timeout,
		Retry:       b.Retry,
		MaxTokens:   b.MaxTokens,
		Thinking:    b.Thinking,
		ServerTools: b.ServerTools,
	}
}

func validProvider(p string) bool {
	switch p {
	case "anthropic", "openai", "openai-responses", "gemini", "rerank":
		return true
	}
	return false
}

// modelView is a masked model entry: key material is reduced to has_key plus a
// last-4 fingerprint.
type modelView struct {
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	BaseURL        string   `json:"base_url"`
	Timeout        string   `json:"timeout"`
	Retry          int      `json:"retry"`
	MaxTokens      int      `json:"max_tokens"`
	Thinking       string   `json:"thinking"`
	ServerTools    []string `json:"server_tools"`
	HasKey         bool     `json:"has_key"`
	KeyFingerprint string   `json:"key_fingerprint,omitempty"`
	InRuntime      bool     `json:"in_runtime"`
	InFile         bool     `json:"in_file"`
	// Drift is true when the live entry differs from the config file (or
	// exists on only one side) — the operator should persist or revert.
	Drift bool `json:"drift"`
}

func viewOf(name string, m config.Model) modelView {
	v := modelView{
		Name:        name,
		Provider:    m.Provider,
		Model:       m.Model,
		BaseURL:     m.BaseURL,
		Timeout:     m.Timeout,
		Retry:       m.Retry,
		MaxTokens:   m.MaxTokens,
		Thinking:    m.Thinking,
		ServerTools: m.ServerTools,
		HasKey:      m.APIKey != "",
	}
	if len(m.APIKey) >= 4 {
		v.KeyFingerprint = "…" + m.APIKey[len(m.APIKey)-4:]
	}
	return v
}

// fileModels loads the models: section from the config file (env-expanded,
// exactly as boot sees it).
func (u *UI) fileModels() (map[string]config.Model, error) {
	path, err := u.configFilePath()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg.Models == nil {
		return map[string]config.Model{}, nil
	}
	return cfg.Models, nil
}

// modelsEqual compares two model configs, treating nil and empty ServerTools
// as equal (YAML round-trips turn a nil slice into []).
func modelsEqual(a, b config.Model) bool {
	if len(a.ServerTools) == 0 {
		a.ServerTools = nil
	}
	if len(b.ServerTools) == 0 {
		b.ServerTools = nil
	}
	return reflect.DeepEqual(a, b)
}

// modelsOrUnavailable returns the live model manager, or reports a named reason
// there is none. The model routes were the only ones that dereferenced it
// without checking, so a console wired without a manager panicked instead of
// answering — and a nil manager is reachable: any test or embedder that builds
// Deps for one surface only leaves it out.
func (u *UI) modelsOrUnavailable(w http.ResponseWriter) (*llm.Manager, bool) {
	if u.deps.Models == nil {
		writeErr(w, http.StatusServiceUnavailable, "no models are configured")
		return nil, false
	}
	return u.deps.Models, true
}

func (u *UI) handleModelsList(w http.ResponseWriter, r *http.Request) {
	mgr, ok := u.modelsOrUnavailable(w)
	if !ok {
		return
	}
	live := mgr.List()
	file, fileErr := u.fileModels()

	names := map[string]bool{}
	for n := range live {
		names[n] = true
	}
	for n := range file {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	out := []modelView{}
	for _, n := range sorted {
		lm, inR := live[n]
		fm, inF := file[n]
		var v modelView
		switch {
		case inR:
			v = viewOf(n, lm)
		default:
			v = viewOf(n, fm)
		}
		v.InRuntime = inR
		v.InFile = inF
		v.Drift = inR != inF || (inR && inF && !modelsEqual(lm, fm))
		out = append(out, v)
	}
	resp := map[string]any{"ok": true, "models": out}
	if fileErr != nil {
		// The file may be mid-edit; listing still works off the runtime set.
		resp["file_error"] = fileErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (u *UI) handleModelUpsert(w http.ResponseWriter, r *http.Request) {
	mgr, ok := u.modelsOrUnavailable(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !modelNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid model name (1-64 of [A-Za-z0-9_.:-])")
		return
	}
	var b modelBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validProvider(b.Provider) {
		writeErr(w, http.StatusBadRequest, "provider must be anthropic|openai|openai-responses|gemini|rerank")
		return
	}
	if b.Model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if b.Timeout != "" {
		if _, err := time.ParseDuration(b.Timeout); err != nil {
			writeErr(w, http.StatusBadRequest, "timeout must be a Go duration (e.g. 60s)")
			return
		}
	}
	if _, err := llm.ParseThinking(b.Thinking); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m := b.toConfig()
	// The UI never round-trips secrets: an empty api_key on an existing model
	// keeps the current key rather than wiping it.
	if m.APIKey == "" {
		if cur, err := mgr.Get(name); err == nil {
			m.APIKey = cur.APIKey
		}
	}
	mgr.Upsert(name, m)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": "runtime",
		"note": "live on this instance; persist to keep across restarts, and note that other instances of a deployment keep their current models until they restart",
	})
}

func (u *UI) handleModelRemove(w http.ResponseWriter, r *http.Request) {
	mgr, ok := u.modelsOrUnavailable(w)
	if !ok {
		return
	}
	// The same name check upsert applies. Without it a delete reached the
	// manager with whatever the path held, so the two routes disagreed about
	// what a valid model name is.
	name := r.PathValue("name")
	if !modelNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid model name (1-64 of [A-Za-z0-9_.:-])")
		return
	}
	mgr.Remove(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": "runtime",
		"note": "removed on this instance; persist to keep it removed across restarts, and note that other instances of a deployment keep their current models until they restart",
	})
}

// handleModelTest exercises the live model with a minimal real call through
// the manager: a 1-token chat, or a trivial rerank for provider:"rerank". The
// verdict distinguishes the common failure classes so the UI can say whether
// the key, the endpoint, or the model name is wrong.
func (u *UI) handleModelTest(w http.ResponseWriter, r *http.Request) {
	mgr, ok := u.modelsOrUnavailable(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	cfg, err := mgr.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	var callErr error
	if cfg.Provider == "rerank" {
		_, callErr = mgr.Rerank(ctx, name, "ping", []string{"pong"}, 1)
	} else {
		_, callErr = mgr.Chat(ctx, name, []llm.Message{{Role: "user", Content: "ping"}}, llm.Opts{MaxTokens: 1})
	}
	latency := time.Since(start).Milliseconds()
	if callErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "latency_ms": latency,
			"class": classifyModelError(callErr), "error": callErr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency, "class": "ok"})
}

func classifyModelError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "403"),
		strings.Contains(msg, "unauthorized"), strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "invalid api key"), strings.Contains(msg, "incorrect api key"):
		return "auth"
	case strings.Contains(msg, "404"), strings.Contains(msg, "not found"):
		return "not_found"
	case strings.Contains(msg, "429"), strings.Contains(msg, "rate limit"), strings.Contains(msg, "quota"):
		return "rate_limited"
	case strings.Contains(msg, "refused"), strings.Contains(msg, "no such host"),
		strings.Contains(msg, "unreachable"), strings.Contains(msg, "no route"):
		return "unreachable"
	case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "timeout"),
		strings.Contains(msg, "tls handshake"):
		return "timeout"
	default:
		return "error"
	}
}

// handleModelsPersist writes the live model set into the config file's
// models: section, replacing only that mapping node so the rest of the file
// (comments included) round-trips. The result is validated exactly like boot
// before it replaces the live file; the previous file is kept as .bak.
func (u *UI) handleModelsPersist(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.modelsOrUnavailable(w); !ok {
		return
	}
	if err := u.persistModels(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "file", "note": "config.yaml updated; runtime already matches"})
}

func (u *UI) persistModels() error {
	if u.deps.Models == nil {
		return fmt.Errorf("no models are configured")
	}
	path, err := u.configFilePath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	root := doc.Content[0]

	var modelsNode yaml.Node
	if err := modelsNode.Encode(u.deps.Models.List()); err != nil {
		return fmt.Errorf("encode models: %w", err)
	}
	// config.Load expands ${VAR} across the whole document before decoding, so
	// a model the file sources from a placeholder is held by the manager as the
	// resolved secret. Encoding the live set verbatim would write that secret
	// into the config file — and lose the placeholder the operator chose on
	// purpose to keep it out. It would also move the config epoch, which hashes
	// the file before expansion precisely so rotating a secret does not move it.
	preserveFileAPIKeys(&modelsNode, fileAPIKeyNodes(root), u.deps.Models.List())
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "models" {
			root.Content[i+1] = &modelsNode
			replaced = true
			break
		}
	}
	if !replaced {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "models"},
			&modelsNode)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	_ = enc.Close()
	return validateThenSwap(path, buf.Bytes())
}

// handleModelsRevert re-applies the config file's models: section to the live
// manager, discarding unpersisted runtime edits.
func (u *UI) handleModelsRevert(w http.ResponseWriter, r *http.Request) {
	mgr, ok := u.modelsOrUnavailable(w)
	if !ok {
		return
	}
	path, err := u.configFilePath()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("config file does not load: %v", err))
		return
	}
	mgr.SetAll(cfg.Models)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "runtime", "note": "runtime now matches the file"})
}

// fileAPIKeyNodes returns each model's api_key node exactly as the file writes
// it — before ${VAR} expansion. The node is kept whole rather than its value so
// the original quoting style survives the round trip.
func fileAPIKeyNodes(root *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if root.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "models" {
			continue
		}
		models := root.Content[i+1]
		if models.Kind != yaml.MappingNode {
			return out
		}
		for j := 0; j+1 < len(models.Content); j += 2 {
			body := models.Content[j+1]
			if body.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k+1 < len(body.Content); k += 2 {
				if body.Content[k].Value == "api_key" {
					out[models.Content[j].Value] = body.Content[k+1]
				}
			}
		}
	}
	return out
}

// preserveFileAPIKeys restores the file's own api_key text for every model
// whose file text is a reference — an ${VAR} placeholder or a cred:<service>
// lookup — and still resolves to what the manager holds.
//
// The second half of that test is what keeps a console edit from being lost. If
// the operator typed a new key for a model the file sources from ${VAR}, the
// manager no longer matches the placeholder's expansion, so the new value is
// written instead: the placeholder was replaced, not merely re-rendered. A
// model the file does not mention (one added through the console) has no
// original text and is written as the manager holds it.
func preserveFileAPIKeys(models *yaml.Node, fileKeys map[string]*yaml.Node, live map[string]config.Model) {
	if models.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(models.Content); i += 2 {
		orig, ok := fileKeys[models.Content[i].Value]
		if !ok || !isSecretReference(orig.Value) {
			continue
		}
		if string(config.ExpandEnv([]byte(orig.Value))) != live[models.Content[i].Value].APIKey {
			continue // the operator replaced it; write what they set
		}
		body := models.Content[i+1]
		if body.Kind != yaml.MappingNode {
			continue
		}
		for k := 0; k+1 < len(body.Content); k += 2 {
			if body.Content[k].Value == "api_key" {
				body.Content[k+1] = orig
			}
		}
	}
}

// isSecretReference reports whether s names a config-level indirection rather
// than a literal secret: an ${VAR} placeholder (optionally ${VAR:-default}) or
// a cred:<service> lookup.
func isSecretReference(s string) bool {
	return envPlaceholderRe.MatchString(s) || strings.HasPrefix(s, "cred:")
}

// validateThenSwap validates candidate as a full config (same code path as
// boot), backs the current file up to .bak, and atomically replaces it.
func validateThenSwap(path string, candidate []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agentflow-*.yaml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(candidate); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if _, err := config.Load(tmpName); err != nil {
		return fmt.Errorf("result does not validate: %w", err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	if err := os.WriteFile(path+".bak", cur, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Windows rename-over-existing can fail on some filesystems; the
		// backup already exists, so a direct write is a safe fallback.
		if err2 := os.WriteFile(path, candidate, 0o600); err2 != nil {
			return fmt.Errorf("replace config: %w (fallback: %v)", err, err2)
		}
	}
	return nil
}
