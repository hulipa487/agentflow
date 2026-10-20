package webui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/studio"
)

func channelAPIFixture(t *testing.T) (*UI, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("system.yaml", `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
gateway:
  listen: ":0"
runtime:
  studio: { enabled: true }
`)
	write("profiles/base.yaml", "name: base\nloop: ./base.lua\n")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	ch := studio.NewChannels(cfg, dir, "", func() []string { return []string{"base"} }, log)
	ui := New(Deps{ConfigDir: dir, Cfg: cfg, Channels: ch})
	return ui, dir
}

type channelListResponse struct {
	studio.ChannelList
	RestartRequired bool `json:"restart_required"`
}

func decodeChannels(t *testing.T, body []byte) channelListResponse {
	t.Helper()
	var out channelListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	return out
}

func TestStudioChannelsAPI(t *testing.T) {
	ui, dir := channelAPIFixture(t)

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/studio/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeChannels(t, rec.Body.Bytes())
	if !list.Editable || list.Mode != "configdir" || len(list.Channels) != 0 {
		t.Fatalf("initial list = %+v", list)
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/channels", map[string]any{
		"channel": map[string]any{
			"name": "wh", "type": "webhook", "agent": "base", "path": "/hook/",
			"token": "super-secret",
		},
		"mtime": list.MTime,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Fatalf("API leaked the inline token: %s", rec.Body.String())
	}
	list = decodeChannels(t, rec.Body.Bytes())
	if len(list.Channels) != 1 || !list.Channels[0].HasToken || !list.RestartRequired {
		t.Fatalf("created list = %+v", list)
	}
	if list.Channels[0].Live {
		t.Fatal("a newly persisted channel must not be marked live")
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/channels/validate", map[string]any{
		"index": -1,
		"channel": map[string]any{
			"name": "bad", "type": "webhook", "agent": "missing",
		},
	})
	var verr struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verr); err != nil {
		t.Fatal(err)
	}
	if verr.OK || len(verr.Errors) == 0 {
		t.Fatalf("unknown agent accepted: %s", rec.Body.String())
	}

	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/channels/0", map[string]any{
		"channel": map[string]any{
			"name": "wh", "type": "webhook", "agent": "base", "path": "/hook2/",
		},
		"mtime": list.MTime,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	list = decodeChannels(t, rec.Body.Bytes())
	if len(list.Channels) != 1 || list.Channels[0].Path != "/hook2/" {
		t.Fatalf("updated list = %+v", list)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "channels.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "super-secret") {
		t.Fatalf("blank secret field did not preserve the stored token:\n%s", raw)
	}
	if !strings.Contains(string(raw), "/hook2/") {
		t.Fatalf("update not persisted:\n%s", raw)
	}

	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/channels/0?mtime="+strconv.FormatInt(list.MTime, 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	list = decodeChannels(t, rec.Body.Bytes())
	if len(list.Channels) != 0 {
		t.Fatalf("after delete = %+v", list.Channels)
	}
}
