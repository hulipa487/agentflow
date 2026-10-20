package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ValidateChannels runs the boot-time channel checks against a candidate list.
// It is what the Studio channel editor calls before persisting, so an edit
// cannot produce a config the next boot would refuse.
func ValidateChannels(c *Config, channels []Channel) error {
	return validateChannelList("channels", c, channels)
}

// ValidateStudioChannels is the stricter Studio rule: on top of the boot
// checks, a Studio-managed channel needs a stable name and names must be
// unique. The boot path keeps accepting unnamed channels, which are addressed
// by index there (and by the drivers with a synthesized name).
func ValidateStudioChannels(c *Config, channels []Channel) error {
	if err := validateChannelList("channel", c, channels); err != nil {
		return err
	}
	seen := make(map[string]bool, len(channels))
	for i, ch := range channels {
		if ch.Name == "" {
			return fmt.Errorf("channel #%d: a name is required", i+1)
		}
		if seen[ch.Name] {
			return fmt.Errorf("channel name %q is used more than once", ch.Name)
		}
		seen[ch.Name] = true
	}
	return nil
}

// validateChannelList is the single channel validator shared by boot and the
// Studio. path is the config label used in messages.
func validateChannelList(path string, c *Config, channels []Channel) error {
	seenPaths := map[string]string{} // path -> channel name, for collision detection
	for i, ch := range channels {
		// "studio" is the synthetic channel the playground captures replies on.
		// A real channel with that name would shadow it and silently swallow
		// playground replies, so the name is reserved while the API is on.
		if ch.Name == "studio" && c.Runtime.Studio.IsEnabled() {
			return fmt.Errorf("%s: channel name \"studio\" is reserved for the Studio playground (runtime.studio.enabled)", path)
		}
		if ch.Agent == "" {
			return fmt.Errorf("%s: channel #%d (%s) has no agent", path, i, ch.Type)
		}
		if _, ok := c.Agents[ch.Agent]; !ok {
			return fmt.Errorf("%s: channel %q references unknown agent %q", path, ch.Name, ch.Agent)
		}
		switch ch.Type {
		case "webhook":
			// path defaults are applied by the driver; here we only require that
			// any explicit path ends in "/" so the subtree match covers the UUID.
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: webhook channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
			if ch.Timeout != "" {
				d, err := time.ParseDuration(ch.Timeout)
				if err != nil || d <= 0 {
					return fmt.Errorf("%s: webhook channel %q has invalid timeout %q (use e.g. \"120s\")", path, ch.Name, ch.Timeout)
				}
			}
		case "ghhook":
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: ghhook channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
			// The GitHub webhook secret is what makes an event trustworthy: the
			// signature is the only thing distinguishing a real delivery from
			// anyone who found the path. It may be a lazy reference (${VAR} /
			// cred:<service>); an unresolvable one skips the channel at
			// construction with a warning, which is safe — the alternative was
			// running it unauthenticated.
			if ch.Secret == "" {
				return fmt.Errorf("%s: ghhook channel %q has no secret; set secret (the GitHub webhook secret) — an unauthenticated ghhook accepts any event", path, ch.Name)
			}
		case "bridge":
			if ch.Path == "" || !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: bridge channel %q needs a path ending with /", path, ch.Name)
			}
			if ch.DeliverURL == "" {
				return fmt.Errorf("%s: bridge channel %q has no deliver_url", path, ch.Name)
			}
			u, err := url.Parse(ch.DeliverURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("%s: bridge channel %q deliver_url must be an absolute http(s) URL", path, ch.Name)
			}
			if ch.Secret == "" {
				return fmt.Errorf("%s: bridge channel %q has no secret (the inbound HMAC key)", path, ch.Name)
			}
		case "telegram":
			// Token presence is not validated here: the token may be a lazy
			// secret reference (${VAR} / cred:<service>) resolved at channel
			// construction, and an unresolvable one skips the channel with a
			// warning rather than failing the boot.
			if ch.Mode == "" {
				ch.Mode = "polling"
			}
			if ch.Mode != "polling" && ch.Mode != "webhook" && ch.Mode != "auto" {
				return fmt.Errorf("%s: telegram channel %q has unsupported mode %q (use polling, webhook, or auto)", path, ch.Name, ch.Mode)
			}
			if ch.Mode == "webhook" && c.Gateway.PublicURL == "" {
				return fmt.Errorf("%s: telegram channel %q webhook mode requires gateway.public_url", path, ch.Name)
			}
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: telegram channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
		default:
			return fmt.Errorf("%s: unsupported channel type %q (channel %q)", path, ch.Type, ch.Name)
		}
		// media policy sanity: max_bytes must be positive when set; an allow
		// list with no patterns is the same as media disabled (allowed, but
		// flag obvious mistakes like a negative ceiling).
		if ch.Media.MaxBytes < 0 {
			return fmt.Errorf("%s: channel %q media.max_bytes must be >= 0", path, ch.Name)
		}
		// path collision check — the shared mux panics on dupes, but a config
		// error is friendlier and names both colliding channels.
		key := ch.Path
		if key != "" {
			if prev, dup := seenPaths[key]; dup {
				return fmt.Errorf("%s: channels %q and %q mount the same path %q", path, prev, ch.Name, key)
			}
			seenPaths[key] = ch.Name
		}
	}
	return nil
}

// MarshalYAML writes only the fields a channel actually uses. The Studio
// rewrites channels.yaml after an edit; without this every save would add a
// block of empty mode/token/media keys to a hand-maintained file.
func (ch Channel) MarshalYAML() (any, error) {
	type out struct {
		Name        string        `yaml:"name"`
		Type        string        `yaml:"type"`
		Mode        string        `yaml:"mode,omitempty"`
		Agent       string        `yaml:"agent"`
		Path        string        `yaml:"path,omitempty"`
		Token       string        `yaml:"token,omitempty"`
		Secret      string        `yaml:"secret,omitempty"`
		SecretToken string        `yaml:"secret_token,omitempty"`
		AllowUsers  []int64       `yaml:"allow_users,omitempty"`
		Media       *ChannelMedia `yaml:"media,omitempty"`
		Timeout     string        `yaml:"timeout,omitempty"`
		Async       bool          `yaml:"async,omitempty"`
		DeliverURL  string        `yaml:"deliver_url,omitempty"`
	}
	o := out{
		Name: ch.Name, Type: ch.Type, Mode: ch.Mode, Agent: ch.Agent,
		Path: ch.Path, Token: ch.Token, Secret: ch.Secret, SecretToken: ch.SecretToken,
		AllowUsers: ch.AllowUsers, Timeout: ch.Timeout, Async: ch.Async,
		DeliverURL: ch.DeliverURL,
	}
	if ch.Media.MaxBytes != 0 || len(ch.Media.Allow) > 0 {
		m := ch.Media
		o.Media = &m
	}
	return o, nil
}
