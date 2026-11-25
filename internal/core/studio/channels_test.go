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

// TestChannelSaveKeepsTheAuditModeAndDropsUnusedKeys: the audit mode exists on
// every channel type, but an editor may show it only for some. A draft that
// does not mention it must keep the stored value — saving a webhook from a form
// with no journal field used to turn its journal back to full. And shared keys
// that no longer apply (the channel moved off auth: key) are removed rather
// than left in the file as secrets nothing reads.
func TestChannelSaveKeepsTheAuditModeAndDropsUnusedKeys(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "resolved-secret")
	dir := writeFiles(t, map[string]string{
		"system.yaml":           channelSystem + "  identity: { enabled: true }\n",
		"profiles/greeter.yaml": greeterProfile,
		"channels.yaml": `
channels:
  - { name: wh, type: webhook, agent: greeter, path: /hook/, journal: off }
  - { name: api, type: openai, agents: [greeter], auth: key, keys: ["${HOOK_TOKEN}"] }
`,
	})
	cfg, err := config.LoadDir(dir, testLog())
	if err != nil {
		t.Fatal(err)
	}
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}

	// A webhook saved by a form that has no journal field.
	saved, err := store.Save(0, ChannelDraft{Name: "wh", Type: "webhook", Agent: "greeter", Path: "/hook/", Timeout: "45s"}, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Channels[0].Journal; got != "off" {
		t.Fatalf("journal after an unrelated edit = %q; want off", got)
	}
	// An explicit value still changes it.
	saved, err = store.Save(0, ChannelDraft{Name: "wh", Type: "webhook", Agent: "greeter", Path: "/hook/", Journal: "full"}, saved.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Channels[0].Journal; got != "full" {
		t.Fatalf("journal after an explicit change = %q; want full", got)
	}

	// The openai channel moves to per-user tokens: its shared keys go.
	saved, err = store.Save(1, ChannelDraft{Name: "api", Type: "openai", Agents: []string{"greeter"}, Auth: "user_token"}, saved.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Channels[1].HasKeys {
		t.Fatal("shared keys were kept on a channel that no longer uses auth: key")
	}
	if raw := string(mustRead(t, filepath.Join(dir, "channels.yaml"))); strings.Contains(raw, "HOOK_TOKEN") {
		t.Fatalf("the unused key reference is still in channels.yaml:\n%s", raw)
	}
}

func TestChannelsFeishuAndWeComRoundTrip(t *testing.T) {
	t.Setenv("FS_SECRET", "fs-secret")
	t.Setenv("WC_SECRET", "wc-secret")
	t.Setenv("WC_TOKEN", "wc-token")
	t.Setenv("WC_AES", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG")
	dir := writeFiles(t, map[string]string{
		"system.yaml":           channelSystem,
		"profiles/greeter.yaml": greeterProfile,
		"channels.yaml": `
channels:
  - { name: fs, type: feishu, agent: greeter, app_id: cli_1, secret: "${FS_SECRET}", encrypt_key: "${WC_AES}" }
  - { name: wc, type: wecom, agent: greeter, corp_id: ww1, agent_id: 1000002, secret: "${WC_SECRET}", token: "${WC_TOKEN}", encrypt_key: "${WC_AES}" }
`,
	})
	cfg, err := config.LoadDir(dir, testLog())
	if err != nil {
		t.Fatal(err)
	}
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 2 {
		t.Fatalf("channels = %d; want 2", len(list.Channels))
	}
	wc := list.Channels[1]
	if wc.CorpID != "ww1" || wc.AgentID != 1000002 || !wc.HasEncryptKey || wc.EncryptKeyRef != "${WC_AES}" {
		t.Fatalf("wecom view = %+v", wc)
	}
	saved, err := store.Save(1, ChannelDraft{
		Name: "wc", Type: "wecom", Agent: "greeter", CorpID: "ww1", AgentID: 1000002, Markdown: true,
	}, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Channels[1].HasEncryptKey || saved.Channels[1].EncryptKeyRef != "${WC_AES}" {
		t.Fatalf("encrypt_key lost on save: %+v", saved.Channels[1])
	}
}

func TestChannelsSetHooksSaveLiveDeleteLiveAndRawToken(t *testing.T) {
	t.Setenv("TG_TOK", "123456:real-token")
	dir := writeFiles(t, map[string]string{
		"system.yaml":           channelSystem,
		"profiles/greeter.yaml": greeterProfile,
		"channels.yaml":         "channels: []\n",
	})
	cfg, err := config.LoadDir(dir, testLog())
	if err != nil {
		t.Fatal(err)
	}
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())

	liveSet := map[string]config.Channel{}
	var deletedSeen []string
	store.SetHooks(ChannelHooks{
		Apply: func(saved []config.Channel, changed *config.Channel, deletedName string) (bool, error) {
			if deletedName != "" {
				deletedSeen = append(deletedSeen, deletedName)
				delete(liveSet, deletedName)
				return true, nil
			}
			if changed != nil && changed.Type == "telegram" {
				liveSet[changed.Name] = *changed
				return true, nil
			}
			return false, nil
		},
		IsLive: func(ch config.Channel) (bool, bool) {
			if ch.Type != "telegram" {
				return false, false
			}
			_, ok := liveSet[ch.Name]
			return ok, true
		},
	})

	initList, err := store.List()
	if err != nil {
		t.Fatal(err)
	}

	saved, outcome, err := store.SaveLive(-1, ChannelDraft{
		Name: "tg-main", Type: "telegram", Agent: "greeter", Mode: "polling", Token: "${TG_TOK}", APIBase: "https://api.example.test",
	}, initList.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Applied || outcome.Note == "" {
		t.Fatalf("expected live apply outcome, got %+v", outcome)
	}
	if len(saved.Channels) != 1 || !saved.Channels[0].Live {
		t.Fatalf("expected Live=true via IsLive hook, got %+v", saved.Channels)
	}

	tok, base, err := store.RawToken(0)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "${TG_TOK}" || base != "https://api.example.test" {
		t.Fatalf("RawToken = (%q, %q)", tok, base)
	}

	afterDel, delOutcome, err := store.DeleteLive(0, saved.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if !delOutcome.Applied || len(afterDel.Channels) != 0 {
		t.Fatalf("unexpected DeleteLive result: outcome=%+v channels=%+v", delOutcome, afterDel.Channels)
	}
	if len(deletedSeen) != 1 || deletedSeen[0] != "tg-main" {
		t.Fatalf("deletedSeen = %v; want [tg-main]", deletedSeen)
	}
}

func TestChannelsEmailRoundTripAndSecrets(t *testing.T) {
	t.Setenv("EMAIL_PW", "imap-secret")
	t.Setenv("SMTP_PW", "smtp-secret")
	dir := writeFiles(t, map[string]string{
		"system.yaml":           channelSystem,
		"profiles/greeter.yaml": greeterProfile,
		"channels.yaml":         "channels: []\n",
	})
	cfg, err := config.LoadDir(dir, testLog())
	if err != nil {
		t.Fatal(err)
	}
	store := NewChannels(cfg, dir, "", agentsFor("greeter"), testLog())

	list0, err := store.List()
	if err != nil {
		t.Fatal(err)
	}

	markSeenFalse := false
	saved, err := store.Save(-1, ChannelDraft{
		Name:         "support-mail",
		Type:         "email",
		Agent:        "greeter",
		Mode:         "polling",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		SMTPHost:     "smtp.example.com",
		SMTPPort:     465,
		Username:     "support@example.com",
		Password:     "${EMAIL_PW}",
		SMTPUsername: "smtp-user@example.com",
		SMTPPassword: "${SMTP_PW}",
		FromAddress:  "Support <support@example.com>",
		Mailbox:      "SupportBox",
		PollInterval: "45s",
		MarkSeen:     &markSeenFalse,
		AllowFrom:    []string{"vip@partner.org"},
		AllowDomains: []string{"example.com"},
		MediaAllow:   []string{"text/*", "application/pdf"},
	}, list0.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(saved.Channels))
	}
	v := saved.Channels[0]
	if !v.HasPassword || v.PasswordRef != "${EMAIL_PW}" {
		t.Fatalf("password flags = (%v, %q); want (true, ${EMAIL_PW})", v.HasPassword, v.PasswordRef)
	}
	if !v.HasSMTPPassword || v.SMTPPasswordRef != "${SMTP_PW}" {
		t.Fatalf("smtp password flags = (%v, %q); want (true, ${SMTP_PW})", v.HasSMTPPassword, v.SMTPPasswordRef)
	}
	if v.MarkSeen == nil || *v.MarkSeen {
		t.Fatalf("MarkSeen = %v; want false", v.MarkSeen)
	}
	if v.IMAPHost != "imap.example.com" || v.SMTPHost != "smtp.example.com" || v.Mailbox != "SupportBox" {
		t.Fatalf("unexpected email view fields: %+v", v)
	}

	creds, err := store.RawEmailCredentials(0)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Password != "${EMAIL_PW}" || creds.SMTPPassword != "${SMTP_PW}" || creds.IMAPHost != "imap.example.com" {
		t.Fatalf("RawEmailCredentials = %+v", creds)
	}

	// Editing without touching secrets preserves password and smtp_password, while ClearSMTPPassword clears it.
	markSeenTrue := true
	updated, err := store.Save(0, ChannelDraft{
		Name:              "support-mail",
		Type:              "email",
		Agent:             "greeter",
		Mode:              "polling",
		IMAPHost:          "imap.example.com",
		IMAPPort:          993,
		SMTPHost:          "smtp.example.com",
		SMTPPort:          465,
		Username:          "support@example.com",
		SMTPUsername:      "smtp-user@example.com",
		FromAddress:       "Support <support@example.com>",
		Mailbox:           "INBOX",
		PollInterval:      "30s",
		MarkSeen:          &markSeenTrue,
		ClearSMTPPassword: true,
		AllowFrom:         []string{"vip@partner.org"},
		AllowDomains:      []string{"example.com"},
		MediaAllow:        []string{"text/*"},
	}, saved.MTime)
	if err != nil {
		t.Fatal(err)
	}
	uv := updated.Channels[0]
	if !uv.HasPassword || uv.PasswordRef != "${EMAIL_PW}" {
		t.Fatalf("password lost on save: %+v", uv)
	}
	if uv.HasSMTPPassword || uv.SMTPPasswordRef != "" {
		t.Fatalf("smtp_password not cleared: %+v", uv)
	}
	if uv.MarkSeen == nil || !*uv.MarkSeen {
		t.Fatalf("MarkSeen = %v; want true", uv.MarkSeen)
	}
}
