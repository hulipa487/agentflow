package studio

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const channelSystem = `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
gateway:
  listen: ":0"
runtime:
  studio: { enabled: true }
`

const greeterProfile = `
name: greeter
loop: ./loops/greeter.lua
model: default
`

func configDirFixture(t *testing.T) (string, *config.Config) {
	t.Helper()
	t.Setenv("HOOK_TOKEN", "resolved-secret")
	dir := writeFiles(t, map[string]string{
		"system.yaml":           channelSystem,
		"profiles/greeter.yaml": greeterProfile,
		"channels.yaml": `
channels:
  - { name: wh, type: webhook, agent: greeter, path: /hook/, token: "${HOOK_TOKEN}", timeout: 30s }
`,
	})
	cfg, err := config.LoadDir(dir, testLog())
	if err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

func agentsFor(names ...string) func() []string {
	return func() []string { return names }
}

func TestChannelsListAndPreserveSecrets(t *testing.T) {
	dir, cfg := configDirFixture(t)
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || list.Mode != "configdir" || !list.Editable {
		t.Fatalf("list = %+v", list)
	}
	ch := list.Channels[0]
	if !ch.HasToken || ch.TokenRef != "${HOOK_TOKEN}" {
		t.Fatalf("token view = %+v; want a reference", ch)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(dir, "channels.yaml"))), "resolved-secret") {
		t.Fatal("fixture unexpectedly contains the resolved secret")
	}

	saved, err := store.Save(0, ChannelDraft{
		Name: "wh", Type: "webhook", Agent: "greeter", Path: "/hook2/", Timeout: "45s",
	}, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Channels[0].Path != "/hook2/" || saved.Channels[0].Live {
		t.Fatalf("saved view = %+v", saved.Channels[0])
	}
	raw := string(mustRead(t, filepath.Join(dir, "channels.yaml")))
	if !strings.Contains(raw, "${HOOK_TOKEN}") || !strings.Contains(raw, "/hook2/") {
		t.Fatalf("persisted channels.yaml lost the placeholder or the edit:\n%s", raw)
	}
	if strings.Contains(raw, "resolved-secret") {
		t.Fatalf("resolved secret leaked into channels.yaml:\n%s", raw)
	}
}

func TestChannelsValidateAndDelete(t *testing.T) {
	dir, cfg := configDirFixture(t)
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())

	if errs := store.Validate(-1, ChannelDraft{Name: "x", Type: "webhook", Agent: "nope"}); len(errs) == 0 {
		t.Fatal("unknown agent accepted")
	}
	if errs := store.Validate(-1, ChannelDraft{Name: "x", Type: "webhook", Agent: "greeter"}); len(errs) != 0 {
		t.Fatalf("valid channel rejected: %v", errs)
	}
	if errs := store.Validate(-1, ChannelDraft{Name: "studio", Type: "webhook", Agent: "greeter"}); len(errs) == 0 {
		t.Fatal("reserved name accepted")
	}

	list, err := store.Save(-1, ChannelDraft{Name: "gh", Type: "ghhook", Agent: "greeter", Path: "/gh/", Secret: "${GH_SECRET}"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 2 || !list.Channels[1].HasSecret {
		t.Fatalf("created channel = %+v", list.Channels)
	}
	list, err = store.Delete(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || list.Channels[0].Name != "gh" {
		t.Fatalf("after delete = %+v", list.Channels)
	}
}

func TestChannelsConflict(t *testing.T) {
	dir, cfg := configDirFixture(t)
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())
	if _, err := store.Save(0, ChannelDraft{Name: "wh", Type: "webhook", Agent: "greeter"}, 12345); err == nil || err != ErrChannelConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestChannelsSingleFile(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "resolved-secret")
	dir := writeFiles(t, map[string]string{
		"agentflow.yaml": `
# hand-maintained deployment
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
agents:
  greeter:
    loop: ./loops/greeter.lua
runtime:
  studio: { enabled: true }
gateway:
  listen: ":0"
  channels:
    - { name: wh, type: webhook, agent: greeter, path: /hook/, token: "${HOOK_TOKEN}" }
`,
	})
	path := filepath.Join(dir, "agentflow.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewChannels(cfg, "", path, agentsFor("greeter"), testLog())
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || !list.Channels[0].HasToken {
		t.Fatalf("list = %+v", list)
	}
	saved, err := store.Save(0, ChannelDraft{
		Name: "wh", Type: "webhook", Agent: "greeter", Path: "/hook/", Timeout: "60s",
	}, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Channels[0].Timeout != "60s" {
		t.Fatalf("saved = %+v", saved.Channels[0])
	}
	raw := string(mustRead(t, path))
	for _, want := range []string{"# hand-maintained deployment", "${HOOK_TOKEN}", "timeout: 60s"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("config lost %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "resolved-secret") {
		t.Fatal("resolved secret leaked into the config")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChannelsBridge(t *testing.T) {
	dir, cfg := configDirFixture(t)
	t.Setenv("BRIDGE_SECRET", "resolved-bridge")
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())
	list, err := store.Save(-1, ChannelDraft{
		Name: "slack", Type: "bridge", Agent: "greeter", Path: "/bridge/",
		DeliverURL: "https://adapter.example.com/deliver", Secret: "${BRIDGE_SECRET}", Timeout: "15s",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got *ChannelView
	for i := range list.Channels {
		if list.Channels[i].Name == "slack" {
			got = &list.Channels[i]
		}
	}
	if got == nil || got.DeliverURL != "https://adapter.example.com/deliver" || !got.HasSecret {
		t.Fatalf("bridge view = %+v", got)
	}
	raw := string(mustRead(t, filepath.Join(dir, "channels.yaml")))
	for _, want := range []string{"type: bridge", "deliver_url: https://adapter.example.com/deliver", "${BRIDGE_SECRET}", "timeout: 15s"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("channels.yaml lost %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "resolved-bridge") {
		t.Fatal("resolved bridge secret leaked into channels.yaml")
	}
	// A blank secret on update preserves the stored HMAC key.
	if _, err := store.Save(got.Index, ChannelDraft{
		Name: "slack", Type: "bridge", Agent: "greeter", Path: "/bridge/",
		DeliverURL: "https://adapter.example.com/v2",
	}, list.MTime); err != nil {
		t.Fatal(err)
	}
	raw = string(mustRead(t, filepath.Join(dir, "channels.yaml")))
	if !strings.Contains(raw, "${BRIDGE_SECRET}") || !strings.Contains(raw, "/v2") {
		t.Fatalf("bridge update lost data:\n%s", raw)
	}
}
