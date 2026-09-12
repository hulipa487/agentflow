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
	cfg, err := config.Load(u.deps.ConfigPath)
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

func (u *UI) handleModelsList(w http.ResponseWriter, r *http.Request) {
	live := u.deps.Models.List()
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
	m := b.toConfig()
	// The UI never round-trips secrets: an empty api_key on an existing model
	// keeps the current key rather than wiping it.
	if m.APIKey == "" {
		if cur, err := u.deps.Models.Get(name); err == nil {
			m.APIKey = cur.APIKey
		}
	}
	u.deps.Models.Upsert(name, m)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "runtime", "note": "live now; persist to keep across restarts"})
}

func (u *UI) handleModelRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	u.deps.Models.Remove(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "runtime", "note": "live now; persist to keep across restarts"})
}

// handleModelTest exercises the live model with a minimal real call through
// the manager: a 1-token chat, or a trivial rerank for provider:"rerank". The
// verdict distinguishes the common failure classes so the UI can say whether
// the key, the endpoint, or the model name is wrong.
func (u *UI) handleModelTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := u.deps.Models.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	var callErr error
	if cfg.Provider == "rerank" {
		_, callErr = u.deps.Models.Rerank(ctx, name, "ping", []string{"pong"}, 1)
	} else {
		_, _, _, callErr = u.deps.Models.Chat(ctx, name, []llm.Message{{Role: "user", Content: "ping"}}, llm.Opts{MaxTokens: 1})
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
	if err := u.persistModels(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "file", "note": "config.yaml updated; runtime already matches"})
}

func (u *UI) persistModels() error {
	path := u.deps.ConfigPath
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
	cfg, err := config.Load(u.deps.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("config file does not load: %v", err))
		return
	}
	u.deps.Models.SetAll(cfg.Models)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "runtime", "note": "runtime now matches the file"})
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
