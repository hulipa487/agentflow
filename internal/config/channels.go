package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// LoopbackListen reports whether a listen address is reachable only from this
// host. It is deliberately strict and answers from the address text alone
// rather than resolving anything: an empty host (":8080") binds every
// interface, a bare port or an unparseable address is not vouched for, and a
// name other than "localhost" is not assumed to resolve to a loopback address.
// Anything this cannot vouch for is treated as reachable.
//
// The admin plane uses it to decide whether a per-boot token is a secret worth
// having; an openai channel with auth: none uses it to decide whether serving
// unauthenticated requests is safe.
func LoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// JournalMode is the effective audit mode for a channel. Empty means full, but
// an openai channel defaults to metadata: its traffic is coding context, which
// is large and private, and the ids and sizes are what an audit needs.
func (ch Channel) JournalMode() string {
	if m := strings.ToLower(strings.TrimSpace(ch.Journal)); m != "" {
		return m
	}
	if ch.Type == "openai" {
		return "metadata"
	}
	return "full"
}

// RegionalReplyWarnings names the channels whose answers can be lost in a
// regional cell. A channel that answers on the HTTP request it received (a
// synchronous webhook, the openai channel) holds that request in one process,
// and a reply is delivered by the instance that owns the session. When a
// message lands on one instance for a conversation another instance owns, the
// owner has nowhere to send the answer and the caller waits out its timeout.
// One-request sessions are unaffected: the instance that receives the request
// always owns them. The list is empty outside a regional topology.
func (c *Config) RegionalReplyWarnings() []string {
	if !c.IsRegional() {
		return nil
	}
	var out []string
	for i, ch := range c.Gateway.Channels {
		name := ch.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", ch.Type, i)
		}
		switch {
		case ch.Type == "openai":
			out = append(out, fmt.Sprintf("channel %q (openai) with runtime.topology: regional: a request that carries a conversation id is answered only when it reaches the instance that owns that conversation, and llm.forward cannot read a request body that arrived on another instance. Requests without a conversation id are unaffected. Pin a conversation to one instance at the load balancer, or leave conversation ids unused.", name))
		case ch.Type == "webhook" && !ch.Async:
			out = append(out, fmt.Sprintf("channel %q (synchronous webhook) with runtime.topology: regional: a reply is delivered only by the instance that received the request, so a message for a conversation another instance owns gets no answer before its timeout. Pin each sender to one instance at the load balancer, or set async: true with a deliver_url.", name))
		}
	}
	return out
}

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

// reservedChannelPath reports whether a channel path would land on a route the
// engine itself serves on the shared listener.
func reservedChannelPath(p string) bool {
	switch strings.TrimSuffix(p, "/") {
	case "", "/health", "/healthz", "/readyz", "/metrics":
		return p != "" && p != "/"
	}
	return strings.HasPrefix(p, "/admin/") || p == "/admin" || strings.HasPrefix(p, "/v1/users/") || p == "/v1/users"
}

// validateChannelList is the single channel validator shared by boot and the
// Studio. path is the config label used in messages.
func validateChannelList(path string, c *Config, channels []Channel) error {
	seenPaths := map[string]string{} // path -> channel name, for collision detection
	// routes collects every pattern a channel will mount, so two channels can
	// be checked for overlap even when their configured paths differ (an
	// openai channel's two endpoints live under its path).
	type routeSpec struct {
		channel string
		pattern string
		subtree bool
	}
	var routes []routeSpec

	for i, ch := range channels {
		// "studio" is the synthetic channel the playground captures replies on.
		// A real channel with that name would shadow it and silently swallow
		// playground replies, so the name is reserved while the API is on.
		if ch.Name == "studio" && c.Runtime.Studio.IsEnabled() {
			return fmt.Errorf("%s: channel name \"studio\" is reserved for the Studio playground (runtime.studio.enabled)", path)
		}
		if ch.Type == "openai" {
			// The openai channel exposes one or more agents as model ids; the
			// singular agent field is the shorthand for one.
			if ch.Agent != "" && len(ch.Agents) > 0 {
				return fmt.Errorf("%s: channel %q sets both agent and agents; use one (agents, or agent for a single agent)", path, ch.Name)
			}
			if ch.Agent == "" && len(ch.Agents) == 0 {
				return fmt.Errorf("%s: openai channel %q has no agents; list the agents clients may call as model ids under agents: [name, ...]", path, ch.Name)
			}
			names := ch.Agents
			if ch.Agent != "" {
				names = []string{ch.Agent}
			}
			for _, name := range names {
				if name == "" {
					return fmt.Errorf("%s: openai channel %q has an empty agent name", path, ch.Name)
				}
				if _, ok := c.Agents[name]; !ok {
					return fmt.Errorf("%s: openai channel %q exposes unknown agent %q", path, ch.Name, name)
				}
			}
		} else {
			if ch.Agent == "" {
				return fmt.Errorf("%s: channel #%d (%s) has no agent", path, i, ch.Type)
			}
			if _, ok := c.Agents[ch.Agent]; !ok {
				return fmt.Errorf("%s: channel %q references unknown agent %q", path, ch.Name, ch.Agent)
			}
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
		case "openai":
			if err := validateOpenAIChannel(path, c, ch); err != nil {
				return err
			}
		case "qq":
			if err := validateQQChannel(path, ch); err != nil {
				return err
			}
		case "feishu":
			if err := validateFeishuChannel(path, ch); err != nil {
				return err
			}
		case "wecom":
			if err := validateWeComChannel(path, ch); err != nil {
				return err
			}
		case "email":
			if err := validateEmailChannel(path, ch); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported channel type %q (channel %q)", path, ch.Type, ch.Name)
		}
		if ch.Markdown && ch.Type != "qq" && ch.Type != "feishu" && ch.Type != "wecom" {
			return fmt.Errorf("%s: channel %q sets markdown, which only qq, feishu and wecom channel types use", path, ch.Name)
		}
		switch ch.JournalMode() {
		case "full", "metadata", "off":
		default:
			return fmt.Errorf("%s: channel %q journal %q is not a mode (want full, metadata or off)", path, ch.Name, ch.Journal)
		}
		// media policy sanity: max_bytes must be positive when set; an allow
		// list with no patterns is the same as media disabled (allowed, but
		// flag obvious mistakes like a negative ceiling).
		if ch.Media.MaxBytes < 0 {
			return fmt.Errorf("%s: channel %q media.max_bytes must be >= 0", path, ch.Name)
		}
		key := ch.Path
		switch ch.Type {
		case "qq":
			key = ch.QQPath()
		case "feishu":
			key = ch.FeishuPath()
		case "wecom":
			key = ch.WeComPath()
		}
		if ch.Type == "openai" {
			p := key
			if p == "" {
				p = "/v1/"
			}
			// /v1/ is allowed for this type even though /v1/users/ is
			// reserved: the driver registers its own exact routes (and a
			// catch-all for OpenAI's 404 envelope), not the whole engine
			// subtree. The collision check below still compares them against
			// every other channel.
			if reservedChannelPath(p) {
				return fmt.Errorf("%s: channel %q path %q is reserved: the admin plane owns /admin/, /healthz, /readyz and /metrics, and the engine /health and /v1/users/ on the shared listener", path, ch.Name, p)
			}
			routes = append(routes,
				routeSpec{ch.Name, p, true},
				routeSpec{ch.Name, p + "chat/completions", false},
				routeSpec{ch.Name, p + "models", false},
				routeSpec{ch.Name, p + "models/", true},
			)
			continue
		}
		if key != "" {
			if prev, dup := seenPaths[key]; dup {
				return fmt.Errorf("%s: channels %q and %q mount the same path %q", path, prev, ch.Name, key)
			}
			seenPaths[key] = ch.Name
			// /v1/ is the openai channel's prefix. Another type mounting it
			// would shadow the API's 404 envelope (and any future engine route
			// under it) for a subtree that belongs to that channel type.
			if ch.Type != "openai" && key == "/v1/" {
				return fmt.Errorf("%s: channel %q path %q is reserved for the openai channel type; give this channel another path", path, ch.Name, key)
			}
			if reservedChannelPath(key) {
				return fmt.Errorf("%s: channel %q path %q is reserved: the admin plane owns /admin/, /healthz, /readyz and /metrics, and the engine /health and /v1/users/ on the shared listener", path, ch.Name, key)
			}
			routes = append(routes, routeSpec{ch.Name, key, strings.HasSuffix(key, "/")})
		}
	}
	// Two channels cannot share a route: the shared mux would shadow one
	// silently, or panic on an exact duplicate. This compares the routes each
	// channel actually mounts, so an openai channel's endpoints collide with a
	// webhook mounted over the same subtree.
	for i := range routes {
		for j := i + 1; j < len(routes); j++ {
			a, b := routes[i], routes[j]
			if a.channel == b.channel {
				continue
			}
			if patternsOverlap(a.pattern, a.subtree, b.pattern, b.subtree) {
				return fmt.Errorf("%s: channels %q and %q mount overlapping routes (%s and %s); give each channel its own path",
					path, a.channel, b.channel, a.pattern, b.pattern)
			}
		}
	}
	return nil
}

// FeishuPath is the route a feishu channel in webhook mode mounts: its path,
// or /webhook/feishu/<name>/ so multiple Feishu bots never collide. Empty in
// websocket mode.
func (ch Channel) FeishuPath() string {
	if ch.Mode != "webhook" {
		return ""
	}
	return defaultWebhookPath("/webhook/feishu/", ch)
}

// WeComPath is the route a wecom channel mounts: its path, or
// /webhook/wecom/<name>/ so multiple WeCom apps never collide.
func (ch Channel) WeComPath() string {
	return defaultWebhookPath("/webhook/wecom/", ch)
}

func defaultWebhookPath(prefix string, ch Channel) string {
	if ch.Path != "" {
		return ch.Path
	}
	if ch.Name != "" {
		return prefix + ch.Name + "/"
	}
	return prefix
}

// QQPath is the route a qq channel in webhook mode mounts: its path, or
// /webhook/qq/<name>/ so two QQ bots never share one. Empty in websocket mode.
func (ch Channel) QQPath() string {
	if ch.Mode != "webhook" {
		return ""
	}
	return defaultWebhookPath("/webhook/qq/", ch)
}

func validateOptionalAPIBase(path, kind string, ch Channel) error {
	if ch.APIBase == "" {
		return nil
	}
	u, err := url.Parse(ch.APIBase)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: %s channel %q api_base must be an absolute http(s) URL", path, kind, ch.Name)
	}
	return nil
}

// validateFeishuChannel checks a Feishu / Lark channel. Secrets may be lazy
// references (${VAR} / cred:<service>), so only presence is checked here.
func validateFeishuChannel(path string, ch Channel) error {
	if strings.TrimSpace(ch.AppID) == "" {
		return fmt.Errorf("%s: feishu channel %q has no app_id; copy the App ID from the Feishu/Lark Developer Console", path, ch.Name)
	}
	if ch.Secret == "" {
		return fmt.Errorf("%s: feishu channel %q has no secret; set secret to the App Secret, e.g. ${FEISHU_APP_SECRET}", path, ch.Name)
	}
	switch ch.Mode {
	case "", "websocket":
		if ch.Path != "" {
			return fmt.Errorf("%s: feishu channel %q sets path, which only webhook mode uses; remove it or set mode: webhook", path, ch.Name)
		}
	case "webhook":
		if !strings.HasSuffix(ch.FeishuPath(), "/") {
			return fmt.Errorf("%s: feishu channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
		}
		if ch.Token == "" && ch.EncryptKey == "" {
			return fmt.Errorf("%s: feishu channel %q webhook mode requires token (Verification Token) and/or encrypt_key so callbacks can be authenticated", path, ch.Name)
		}
	default:
		return fmt.Errorf("%s: feishu channel %q has unsupported mode %q (use websocket or webhook)", path, ch.Name, ch.Mode)
	}
	for _, s := range ch.Scenes {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "p2p", "group", "group_all":
		default:
			return fmt.Errorf("%s: feishu channel %q has unknown scene %q (use p2p, group or group_all)", path, ch.Name, s)
		}
	}
	if err := validateOptionalAPIBase(path, "feishu", ch); err != nil {
		return err
	}
	if len(ch.AllowUsers) > 0 {
		return fmt.Errorf("%s: feishu channel %q sets allow_users, which only telegram uses", path, ch.Name)
	}
	return nil
}

// validateWeComChannel checks a WeCom (企业微信) callback channel.
func validateWeComChannel(path string, ch Channel) error {
	if strings.TrimSpace(ch.CorpID) == "" {
		return fmt.Errorf("%s: wecom channel %q has no corp_id; copy CorpID from WeCom Admin", path, ch.Name)
	}
	if ch.AgentID <= 0 {
		return fmt.Errorf("%s: wecom channel %q needs agent_id > 0", path, ch.Name)
	}
	if ch.Secret == "" {
		return fmt.Errorf("%s: wecom channel %q has no secret; set secret to the app CorpSecret, e.g. ${WECOM_SECRET}", path, ch.Name)
	}
	if ch.Token == "" {
		return fmt.Errorf("%s: wecom channel %q has no token; set token to the callback Token", path, ch.Name)
	}
	if ch.EncryptKey == "" {
		return fmt.Errorf("%s: wecom channel %q has no encrypt_key; set encrypt_key to the 43-character EncodingAESKey", path, ch.Name)
	}
	ref := ParseSecretRef(ch.EncryptKey)
	if ref.Env == "" && ref.Cred == "" {
		raw, err := base64.StdEncoding.DecodeString(ref.Literal + "=")
		if len(ref.Literal) != 43 || err != nil || len(raw) != 32 {
			return fmt.Errorf("%s: wecom channel %q encrypt_key must be a 43-character base64 EncodingAESKey", path, ch.Name)
		}
	}
	switch ch.Mode {
	case "", "webhook":
	default:
		return fmt.Errorf("%s: wecom channel %q has unsupported mode %q (only webhook is supported)", path, ch.Name, ch.Mode)
	}
	if !strings.HasSuffix(ch.WeComPath(), "/") {
		return fmt.Errorf("%s: wecom channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
	}
	if err := validateOptionalAPIBase(path, "wecom", ch); err != nil {
		return err
	}
	if len(ch.Scenes) > 0 {
		return fmt.Errorf("%s: wecom channel %q sets scenes, which wecom does not use", path, ch.Name)
	}
	if len(ch.AllowUsers) > 0 {
		return fmt.Errorf("%s: wecom channel %q sets allow_users, which only telegram uses", path, ch.Name)
	}
	return nil
}

// validateQQChannel checks a QQ Bot channel. The secret may be a lazy
// reference (${VAR} / cred:<service>), so only its presence is checked here.
func validateQQChannel(path string, ch Channel) error {
	if strings.TrimSpace(ch.AppID) == "" {
		return fmt.Errorf("%s: qq channel %q has no app_id; copy the bot's AppID from the QQ Open Platform console (q.qq.com)", path, ch.Name)
	}
	if ch.Secret == "" {
		return fmt.Errorf("%s: qq channel %q has no secret; set secret to the bot's AppSecret, e.g. ${QQ_BOT_SECRET}", path, ch.Name)
	}
	switch ch.Mode {
	case "", "websocket":
		if ch.Path != "" {
			return fmt.Errorf("%s: qq channel %q sets path, which only webhook mode uses; remove it or set mode: webhook", path, ch.Name)
		}
	case "webhook":
		if !strings.HasSuffix(ch.QQPath(), "/") {
			return fmt.Errorf("%s: qq channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
		}
	default:
		return fmt.Errorf("%s: qq channel %q has unsupported mode %q (use websocket or webhook)", path, ch.Name, ch.Mode)
	}
	for _, s := range ch.Scenes {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "c2c", "group", "group_all", "guild", "dm":
		default:
			return fmt.Errorf("%s: qq channel %q has unknown scene %q (use c2c, group, group_all, guild or dm)", path, ch.Name, s)
		}
	}
	if err := validateOptionalAPIBase(path, "qq", ch); err != nil {
		return err
	}
	if len(ch.AllowUsers) > 0 {
		return fmt.Errorf("%s: qq channel %q sets allow_users, which only telegram uses (QQ ids are openids, not numbers)", path, ch.Name)
	}
	return nil
}

// validateEmailChannel checks an IMAP/SMTP email channel. Passwords may be
// lazy references (${VAR} / cred:<service>), so only presence is checked here.
func validateEmailChannel(path string, ch Channel) error {
	if strings.TrimSpace(ch.IMAPHost) == "" {
		return fmt.Errorf("%s: email channel %q has no imap_host", path, ch.Name)
	}
	if ch.IMAPPort < 0 || ch.IMAPPort > 65535 {
		return fmt.Errorf("%s: email channel %q imap_port must be 1..65535 (got %d)", path, ch.Name, ch.IMAPPort)
	}
	if strings.TrimSpace(ch.SMTPHost) == "" {
		return fmt.Errorf("%s: email channel %q has no smtp_host", path, ch.Name)
	}
	if ch.SMTPPort < 0 || ch.SMTPPort > 65535 {
		return fmt.Errorf("%s: email channel %q smtp_port must be 1..65535 (got %d)", path, ch.Name, ch.SMTPPort)
	}
	if strings.TrimSpace(ch.Username) == "" {
		return fmt.Errorf("%s: email channel %q has no username", path, ch.Name)
	}
	if ch.Password == "" {
		return fmt.Errorf("%s: email channel %q has no password; set password, e.g. ${EMAIL_PASSWORD} or cred:email", path, ch.Name)
	}
	switch ch.Mode {
	case "", "idle", "polling":
	default:
		return fmt.Errorf("%s: email channel %q has unsupported mode %q (use idle or polling)", path, ch.Name, ch.Mode)
	}
	if ch.Path != "" {
		return fmt.Errorf("%s: email channel %q sets path, which email does not use", path, ch.Name)
	}
	if len(ch.AllowUsers) > 0 {
		return fmt.Errorf("%s: email channel %q sets allow_users, which only telegram uses; use allow_from or allow_domains for email", path, ch.Name)
	}
	if pi := strings.TrimSpace(ch.PollInterval); pi != "" {
		d, err := time.ParseDuration(pi)
		if err != nil || d < time.Second {
			return fmt.Errorf("%s: email channel %q poll_interval %q must be a duration >= 1s (use e.g. \"30s\")", path, ch.Name, ch.PollInterval)
		}
	}
	if from := strings.TrimSpace(ch.FromAddress); from != "" {
		addr, err := mail.ParseAddress(from)
		if err != nil || strings.TrimSpace(addr.Address) == "" {
			return fmt.Errorf("%s: email channel %q has invalid from_address %q", path, ch.Name, ch.FromAddress)
		}
	}
	for _, raw := range ch.AllowFrom {
		s := strings.TrimSpace(raw)
		local, domain, ok := strings.Cut(s, "@")
		if s == "" || !ok || strings.ContainsAny(s, " \t\r\n") || local == "" || domain == "" || strings.Contains(domain, "@") {
			return fmt.Errorf("%s: email channel %q allow_from entry %q must be a valid email address", path, ch.Name, raw)
		}
	}
	for _, raw := range ch.AllowDomains {
		d := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		if d == "" || strings.ContainsAny(d, "@ \t\r\n") {
			return fmt.Errorf("%s: email channel %q allow_domains entry %q must be a domain name (e.g. example.com)", path, ch.Name, raw)
		}
	}
	return nil
}

// patternsOverlap reports whether two ServeMux patterns can match the same
// request. A pattern ending in "/" is a subtree; a request matches the longest
// pattern, so a subtree and a path under it overlap.
func patternsOverlap(a string, aSubtree bool, b string, bSubtree bool) bool {
	switch {
	case aSubtree && bSubtree:
		return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
	case aSubtree:
		return strings.HasPrefix(b, a)
	case bSubtree:
		return strings.HasPrefix(a, b)
	default:
		return a == b
	}
}

// validateOpenAIChannel is the openai type's own rules. Each refusal names what
// to do, because the alternative — a channel that starts and then misbehaves —
// is harder to diagnose than a boot failure.
func validateOpenAIChannel(path string, c *Config, ch Channel) error {
	p := ch.Path
	if p == "" {
		p = "/v1/"
	}
	if !strings.HasSuffix(p, "/") {
		return fmt.Errorf("%s: openai channel %q path must end with / (got %q): it is the prefix the driver appends chat/completions and models to", path, ch.Name, p)
	}
	switch ch.Auth {
	case "user_token":
		if !c.Runtime.Identity.Enabled {
			return fmt.Errorf("%s: openai channel %q uses auth: user_token but runtime.identity.enabled is false; enable identity and issue per-user tokens, or use auth: key", path, ch.Name)
		}
	case "key":
		if len(ch.Keys) == 0 {
			return fmt.Errorf("%s: openai channel %q uses auth: key but has no keys; set keys: [\"${COMPAT_API_KEY}\"]", path, ch.Name)
		}
	case "none":
		listen := c.Gateway.Listen
		if listen == "" {
			listen = ":8080"
		}
		if !LoopbackListen(listen) {
			return fmt.Errorf("%s: openai channel %q uses auth: none but gateway.listen %q is not a loopback address; anyone who can reach it could spend this deployment's models — set auth: key or auth: user_token", path, ch.Name, listen)
		}
	case "":
		return fmt.Errorf("%s: openai channel %q needs an auth mode: user_token (per-user bearer tokens), key (a shared key from keys:), or none (loopback only)", path, ch.Name)
	default:
		return fmt.Errorf("%s: openai channel %q has unsupported auth %q (use user_token, key, or none)", path, ch.Name, ch.Auth)
	}
	if ch.Timeout != "" {
		d, err := time.ParseDuration(ch.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s: openai channel %q has invalid timeout %q (use e.g. \"600s\")", path, ch.Name, ch.Timeout)
		}
	}
	if ch.MaxRequestBytes < 0 {
		return fmt.Errorf("%s: openai channel %q max_request_bytes must be >= 0", path, ch.Name)
	}
	// An agent whose model points back at this listener and this channel's path
	// would answer itself forever: the request becomes a turn, the turn calls
	// the model, the model is this channel.
	for name, m := range c.Models {
		if m.BaseURL == "" {
			continue
		}
		if reachesOwnListener(m.BaseURL, c.Gateway.Listen, p) {
			return fmt.Errorf("%s: openai channel %q: model %q has base_url %q, which reaches this instance's own listener on %s; an agent would call itself forever — point the model at the upstream provider, or move the channel to another path",
				path, ch.Name, name, m.BaseURL, p)
		}
	}
	return nil
}

// reachesOwnListener reports whether baseURL addresses this process's own
// gateway listener under the channel's path.
func reachesOwnListener(baseURL, listen, channelPath string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return false
	}
	uport := u.Port()
	if uport == "" {
		switch u.Scheme {
		case "http":
			uport = "80"
		case "https":
			uport = "443"
		default:
			return false
		}
	}
	if port != uport {
		return false
	}
	if !hostsOverlap(host, u.Hostname()) {
		return false
	}
	base := u.Path
	if base == "" {
		base = "/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return strings.HasPrefix(base, channelPath)
}

// hostsOverlap reports whether a listen host and a URL host name the same
// interface. An empty or unspecified listen host binds every interface, so any
// local address reaches it; loopback names are interchangeable (localhost,
// 127.0.0.1, ::1).
func hostsOverlap(listen, target string) bool {
	if listen == "" || listen == "0.0.0.0" || listen == "::" {
		return hostIsLocal(target)
	}
	if strings.EqualFold(listen, target) {
		return true
	}
	return hostIsLocal(listen) && hostIsLocal(target)
}

func hostIsLocal(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// MarshalYAML writes only the fields a channel actually uses. The Studio
// rewrites channels.yaml after an edit; without this every save would add a
// block of empty mode/token/media keys to a hand-maintained file.
func (ch Channel) MarshalYAML() (any, error) {
	type out struct {
		Name            string        `yaml:"name"`
		Type            string        `yaml:"type"`
		Mode            string        `yaml:"mode,omitempty"`
		Agent           string        `yaml:"agent,omitempty"`
		Agents          []string      `yaml:"agents,omitempty"`
		Path            string        `yaml:"path,omitempty"`
		Token           string        `yaml:"token,omitempty"`
		Secret          string        `yaml:"secret,omitempty"`
		AppID           string        `yaml:"app_id,omitempty"`
		CorpID          string        `yaml:"corp_id,omitempty"`
		AgentID         int64         `yaml:"agent_id,omitempty"`
		EncryptKey      string        `yaml:"encrypt_key,omitempty"`
		Scenes          []string      `yaml:"scenes,omitempty"`
		APIBase         string        `yaml:"api_base,omitempty"`
		Markdown        bool          `yaml:"markdown,omitempty"`
		SecretToken     string        `yaml:"secret_token,omitempty"`
		Auth            string        `yaml:"auth,omitempty"`
		Keys            []string      `yaml:"keys,omitempty"`
		MaxRequestBytes int           `yaml:"max_request_bytes,omitempty"`
		ExposeReasoning bool          `yaml:"expose_reasoning,omitempty"`
		Journal         string        `yaml:"journal,omitempty"`
		AllowUsers      []int64       `yaml:"allow_users,omitempty"`
		IMAPHost        string        `yaml:"imap_host,omitempty"`
		IMAPPort        int           `yaml:"imap_port,omitempty"`
		SMTPHost        string        `yaml:"smtp_host,omitempty"`
		SMTPPort        int           `yaml:"smtp_port,omitempty"`
		Username        string        `yaml:"username,omitempty"`
		Password        string        `yaml:"password,omitempty"`
		SMTPUsername    string        `yaml:"smtp_username,omitempty"`
		SMTPPassword    string        `yaml:"smtp_password,omitempty"`
		FromAddress     string        `yaml:"from_address,omitempty"`
		Mailbox         string        `yaml:"mailbox,omitempty"`
		PollInterval    string        `yaml:"poll_interval,omitempty"`
		MarkSeen        *bool         `yaml:"mark_seen,omitempty"`
		AllowFrom       []string      `yaml:"allow_from,omitempty"`
		AllowDomains    []string      `yaml:"allow_domains,omitempty"`
		Media           *ChannelMedia `yaml:"media,omitempty"`
		Timeout         string        `yaml:"timeout,omitempty"`
		Async           bool          `yaml:"async,omitempty"`
		DeliverURL      string        `yaml:"deliver_url,omitempty"`
	}
	o := out{
		Name: ch.Name, Type: ch.Type, Mode: ch.Mode, Agent: ch.Agent, Agents: ch.Agents,
		Path: ch.Path, Token: ch.Token, Secret: ch.Secret, SecretToken: ch.SecretToken,
		AppID: ch.AppID, CorpID: ch.CorpID, AgentID: ch.AgentID, EncryptKey: ch.EncryptKey,
		Scenes: ch.Scenes, APIBase: ch.APIBase, Markdown: ch.Markdown,
		Auth: ch.Auth, Keys: ch.Keys, MaxRequestBytes: ch.MaxRequestBytes,
		ExposeReasoning: ch.ExposeReasoning, Journal: ch.Journal,
		AllowUsers: ch.AllowUsers,
		IMAPHost:   ch.IMAPHost, IMAPPort: ch.IMAPPort, SMTPHost: ch.SMTPHost, SMTPPort: ch.SMTPPort,
		Username: ch.Username, Password: ch.Password,
		SMTPUsername: ch.SMTPUsername, SMTPPassword: ch.SMTPPassword,
		FromAddress: ch.FromAddress, Mailbox: ch.Mailbox,
		PollInterval: ch.PollInterval, MarkSeen: ch.MarkSeen,
		AllowFrom: ch.AllowFrom, AllowDomains: ch.AllowDomains,
		Timeout: ch.Timeout, Async: ch.Async,
		DeliverURL: ch.DeliverURL,
	}
	if ch.Media.MaxBytes != 0 || len(ch.Media.Allow) > 0 {
		m := ch.Media
		o.Media = &m
	}
	return o, nil
}
