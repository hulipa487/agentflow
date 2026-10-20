package studio

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

// ErrChannelConflict means the channels file changed since the caller read it.
var ErrChannelConflict = errors.New("the channels file changed on disk since it was read; reload before saving")

// ChannelDraft is the editable shape of one channel. Secret fields are write
// only: an empty value keeps the stored one, and the Clear flags remove it.
type ChannelDraft struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Agent          string   `json:"agent"`
	Mode           string   `json:"mode,omitempty"`
	Path           string   `json:"path,omitempty"`
	Token          string   `json:"token,omitempty"`
	Secret         string   `json:"secret,omitempty"`
	SecretToken    string   `json:"secret_token,omitempty"`
	ClearToken     bool     `json:"clear_token,omitempty"`
	ClearSecret    bool     `json:"clear_secret,omitempty"`
	ClearSecretTok bool     `json:"clear_secret_token,omitempty"`
	AllowUsers     []int64  `json:"allow_users,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	Async          bool     `json:"async,omitempty"`
	DeliverURL     string   `json:"deliver_url,omitempty"`
	MediaAllow     []string `json:"media_allow,omitempty"`
	MediaMaxBytes  int      `json:"media_max_bytes,omitempty"`
}

// ChannelView is a channel as the editor sees it. Secrets are never included;
// Has* says whether one is stored, and *Ref carries an env/credential
// placeholder verbatim because it reveals nothing and is useful to audit.
type ChannelView struct {
	Index         int      `json:"index"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Agent         string   `json:"agent"`
	Mode          string   `json:"mode,omitempty"`
	Path          string   `json:"path,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`
	Async         bool     `json:"async,omitempty"`
	DeliverURL    string   `json:"deliver_url,omitempty"`
	AllowUsers    []int64  `json:"allow_users,omitempty"`
	MediaAllow    []string `json:"media_allow,omitempty"`
	MediaMaxBytes int      `json:"media_max_bytes,omitempty"`
	HasToken      bool     `json:"has_token"`
	HasSecret     bool     `json:"has_secret"`
	HasSecretTok  bool     `json:"has_secret_token"`
	TokenRef      string   `json:"token_ref,omitempty"`
	SecretRef     string   `json:"secret_ref,omitempty"`
	SecretTokRef  string   `json:"secret_token_ref,omitempty"`
	Live          bool     `json:"live"`
}

// ChannelList is the list response: the current source and the channels in it.
type ChannelList struct {
	Mode     string        `json:"mode"` // configdir | file
	Path     string        `json:"path,omitempty"`
	Editable bool          `json:"editable"`
	MTime    int64         `json:"mtime,omitempty"`
	Channels []ChannelView `json:"channels"`
	Note     string        `json:"note"`
}

// Channels persists the deployment's gateway channels. Channels bind at boot,
// so a save is durable but does not change the running listeners: the API
// reports restart_required, and the desktop app says so.
type Channels struct {
	mu         sync.Mutex
	cfg        *config.Config
	configDir  string
	configPath string
	agentNames func() []string
	log        *slog.Logger
}

// NewChannels builds the store. agentNames should return the live agent names
// (not spawn templates), so a channel can target an agent created in Studio
// since boot; nil falls back to the boot config's agents.
func NewChannels(cfg *config.Config, configDir, configPath string, agentNames func() []string, log *slog.Logger) *Channels {
	if log == nil {
		log = slog.Default()
	}
	if abs, err := filepath.Abs(configDir); err == nil && configDir != "" {
		configDir = abs
	}
	if abs, err := filepath.Abs(configPath); err == nil && configPath != "" {
		configPath = abs
	}
	return &Channels{cfg: cfg, configDir: configDir, configPath: configPath, agentNames: agentNames, log: log}
}

// sourcePath returns the file the store edits, or "" when the instance has no
// editable config source.
func (c *Channels) sourcePath() string {
	if c.configDir != "" {
		return filepath.Join(c.configDir, "channels.yaml")
	}
	return c.configPath
}

// List reads the channels currently persisted in the source.
func (c *Channels) List() (ChannelList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listLocked()
}

func (c *Channels) listLocked() (ChannelList, error) {
	path := c.sourcePath()
	out := ChannelList{
		Mode:     "file",
		Path:     path,
		Editable: path != "",
		Note:     "Channels bind when agentflow starts; saved changes apply after a restart.",
	}
	if c.configDir != "" {
		out.Mode = "configdir"
	}
	if path == "" {
		out.Editable = false
		return out, nil
	}
	raw, err := c.readRaw()
	if err != nil {
		return out, err
	}
	if st, err := os.Stat(path); err == nil {
		out.MTime = st.ModTime().UnixNano()
	}
	out.Channels = make([]ChannelView, 0, len(raw))
	for i, ch := range raw {
		out.Channels = append(out.Channels, c.view(i, ch))
	}
	return out, nil
}

func (c *Channels) view(i int, ch config.Channel) ChannelView {
	v := ChannelView{
		Index: i, Name: ch.Name, Type: ch.Type, Agent: ch.Agent,
		Mode: ch.Mode, Path: ch.Path, Timeout: ch.Timeout, Async: ch.Async,
		DeliverURL: ch.DeliverURL,
		AllowUsers: ch.AllowUsers, MediaAllow: ch.Media.Allow, MediaMaxBytes: ch.Media.MaxBytes,
		HasToken: ch.Token != "", HasSecret: ch.Secret != "", HasSecretTok: ch.SecretToken != "",
		TokenRef: secretRef(ch.Token), SecretRef: secretRef(ch.Secret), SecretTokRef: secretRef(ch.SecretToken),
	}
	v.Live = c.live(i, ch)
	return v
}

// live reports whether the boot-time channel at this index still matches the
// persisted definition. Secrets cannot be compared (the boot config has them
// resolved), so a secret-only edit is covered by the list's restart note.
func (c *Channels) live(i int, raw config.Channel) bool {
	if c.cfg == nil || i >= len(c.cfg.Gateway.Channels) {
		return false
	}
	l := c.cfg.Gateway.Channels[i]
	return l.Name == raw.Name && l.Type == raw.Type && l.Agent == raw.Agent &&
		l.Mode == raw.Mode && l.Path == raw.Path && l.Timeout == raw.Timeout &&
		l.Async == raw.Async && l.DeliverURL == raw.DeliverURL &&
		equalInts(l.AllowUsers, raw.AllowUsers) &&
		equalStrings(l.Media.Allow, raw.Media.Allow)
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// secretRef returns a raw value only when it is a reference, never an inline
// secret.
func secretRef(raw string) string {
	if strings.HasPrefix(raw, "cred:") {
		return raw
	}
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		return raw
	}
	return ""
}

// Validate checks one draft against the deployment without writing.
func (c *Channels) Validate(index int, d ChannelDraft) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := c.readRaw()
	if err != nil {
		return []string{err.Error()}
	}
	list, err := c.merged(raw, index, d)
	if err != nil {
		return []string{err.Error()}
	}
	if err := config.ValidateStudioChannels(c.validationConfig(), list); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// Save replaces (index >= 0) or appends (index < 0) a channel and persists the
// list. expectMTime guards against clobbering a hand edit.
func (c *Channels) Save(index int, d ChannelDraft, expectMTime int64) (ChannelList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.sourcePath()
	if path == "" {
		return ChannelList{}, errors.New("this instance has no editable config file")
	}
	if expectMTime > 0 {
		if st, err := os.Stat(path); err == nil && st.ModTime().UnixNano() != expectMTime {
			return ChannelList{}, ErrChannelConflict
		} else if err != nil && !os.IsNotExist(err) {
			return ChannelList{}, err
		}
	}
	raw, err := c.readRaw()
	if err != nil {
		return ChannelList{}, err
	}
	list, err := c.merged(raw, index, d)
	if err != nil {
		return ChannelList{}, err
	}
	if err := config.ValidateStudioChannels(c.validationConfig(), list); err != nil {
		return ChannelList{}, err
	}
	if err := c.write(list); err != nil {
		return ChannelList{}, err
	}
	return c.listLocked()
}

// Delete removes a channel by index and persists the remaining list.
func (c *Channels) Delete(index int, expectMTime int64) (ChannelList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.sourcePath()
	if path == "" {
		return ChannelList{}, errors.New("this instance has no editable config file")
	}
	if expectMTime > 0 {
		if st, err := os.Stat(path); err == nil && st.ModTime().UnixNano() != expectMTime {
			return ChannelList{}, ErrChannelConflict
		} else if err != nil && !os.IsNotExist(err) {
			return ChannelList{}, err
		}
	}
	raw, err := c.readRaw()
	if err != nil {
		return ChannelList{}, err
	}
	if index < 0 || index >= len(raw) {
		return ChannelList{}, fmt.Errorf("channel #%d does not exist", index+1)
	}
	list := append(append([]config.Channel(nil), raw[:index]...), raw[index+1:]...)
	if err := config.ValidateStudioChannels(c.validationConfig(), list); err != nil {
		return ChannelList{}, err
	}
	if err := c.write(list); err != nil {
		return ChannelList{}, err
	}
	return c.listLocked()
}

// merged applies the draft to the raw list, preserving secrets that the draft
// leaves untouched.
func (c *Channels) merged(raw []config.Channel, index int, d ChannelDraft) ([]config.Channel, error) {
	list := append([]config.Channel(nil), raw...)
	var existing *config.Channel
	if index >= 0 {
		if index >= len(list) {
			return nil, fmt.Errorf("channel #%d does not exist", index+1)
		}
		existing = &list[index]
	}
	ch := channelFromDraft(d, existing)
	if index >= 0 {
		list[index] = ch
	} else {
		list = append(list, ch)
	}
	return list, nil
}

func channelFromDraft(d ChannelDraft, existing *config.Channel) config.Channel {
	ch := config.Channel{
		Name: strings.TrimSpace(d.Name), Type: strings.TrimSpace(d.Type),
		Agent: strings.TrimSpace(d.Agent), Mode: strings.TrimSpace(d.Mode),
		Path: strings.TrimSpace(d.Path), Timeout: strings.TrimSpace(d.Timeout),
		Async: d.Async, DeliverURL: strings.TrimSpace(d.DeliverURL), AllowUsers: d.AllowUsers,
		Media: config.ChannelMedia{Allow: d.MediaAllow, MaxBytes: d.MediaMaxBytes},
	}
	if existing != nil {
		ch.Token, ch.Secret, ch.SecretToken = existing.Token, existing.Secret, existing.SecretToken
	}
	if d.Token != "" {
		ch.Token = d.Token
	} else if d.ClearToken {
		ch.Token = ""
	}
	if d.Secret != "" {
		ch.Secret = d.Secret
	} else if d.ClearSecret {
		ch.Secret = ""
	}
	if d.SecretToken != "" {
		ch.SecretToken = d.SecretToken
	} else if d.ClearSecretTok {
		ch.SecretToken = ""
	}
	return ch
}

// validationConfig replaces the boot agent set with the live one, so a channel
// can reference an agent created in Studio since startup.
func (c *Channels) validationConfig() *config.Config {
	if c.cfg == nil {
		return &config.Config{}
	}
	clone := *c.cfg
	agents := map[string]config.Agent{}
	for name := range c.cfg.Agents {
		agents[name] = config.Agent{}
	}
	if c.agentNames != nil {
		for _, name := range c.agentNames() {
			agents[name] = config.Agent{}
		}
	}
	clone.Agents = agents
	return &clone
}

func (c *Channels) readRaw() ([]config.Channel, error) {
	if c.configDir != "" {
		b, err := os.ReadFile(filepath.Join(c.configDir, "channels.yaml"))
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var frag struct {
			Channels []config.Channel `yaml:"channels"`
		}
		if err := yaml.Unmarshal(b, &frag); err != nil {
			return nil, fmt.Errorf("channels.yaml: %w", err)
		}
		return frag.Channels, nil
	}
	if c.configPath == "" {
		return nil, nil
	}
	b, err := os.ReadFile(c.configPath)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	root, err := config.MappingRoot(&doc)
	if err != nil {
		return nil, err
	}
	gw := config.MapValue(root, "gateway")
	if gw == nil {
		return nil, nil
	}
	chNode := config.MapValue(gw, "channels")
	if chNode == nil {
		return nil, nil
	}
	var out []config.Channel
	if err := chNode.Decode(&out); err != nil {
		return nil, fmt.Errorf("gateway.channels: %w", err)
	}
	return out, nil
}

func (c *Channels) write(list []config.Channel) error {
	if c.configDir != "" {
		return c.writeDir(list)
	}
	return c.writeFile(list)
}

func marshalChannels(list []config.Channel) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	frag := struct {
		Channels []config.Channel `yaml:"channels"`
	}{Channels: list}
	if err := enc.Encode(frag); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Channels) writeDir(list []config.Channel) error {
	path := filepath.Join(c.configDir, "channels.yaml")
	out, err := marshalChannels(list)
	if err != nil {
		return err
	}
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
	tmp, err := os.CreateTemp(c.configDir, ".channels-*.yaml")
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
		// Windows rename-over-existing can fail; fall back to a direct write.
		if err2 := os.WriteFile(path, out, 0o600); err2 != nil {
			return fmt.Errorf("replace channels.yaml: %w (fallback: %v)", err, err2)
		}
	}
	// Best effort on platforms without POSIX modes.
	_ = os.Chmod(path, 0o600)
	// The whole directory must still load: this catches a channel pointing at
	// an agent that no longer exists, a malformed sibling fragment, and so on.
	if _, err := config.LoadDir(c.configDir, c.log); err != nil {
		restore()
		return fmt.Errorf("channels.yaml does not validate: %w", err)
	}
	return nil
}

func (c *Channels) writeFile(list []config.Channel) error {
	b, err := os.ReadFile(c.configPath)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	root, err := config.MappingRoot(&doc)
	if err != nil {
		return err
	}
	frag, err := marshalChannels(list)
	if err != nil {
		return err
	}
	var wrap yaml.Node
	if err := yaml.Unmarshal(frag, &wrap); err != nil {
		return err
	}
	if len(wrap.Content) == 0 || len(wrap.Content[0].Content) < 2 {
		return fmt.Errorf("internal: encoded channels fragment is malformed")
	}
	seq := config.MapValue(wrap.Content[0], "channels")
	if seq == nil {
		return fmt.Errorf("internal: encoded channels fragment has no channels key")
	}
	gw := config.MapValue(root, "gateway")
	if gw == nil {
		gw = &yaml.Node{Kind: yaml.MappingNode}
		config.SetMapKey(root, "gateway", gw)
	}
	config.SetMapKey(gw, "channels", seq)
	candidate, err := config.MarshalDocument(&doc)
	if err != nil {
		return err
	}
	return config.ValidateAndSwap(c.configPath, candidate)
}
