// Package qqbot is the QQ Bot channel driver, built on Tencent's official QQ
// Bot Open Platform API v2 (QQ 开放平台, https://bot.q.qq.com/wiki/develop/api-v2/).
//
// Inbound events arrive in one of two modes, selected by the "mode" config:
//   - "websocket" (default): the driver connects to the platform's gateway,
//     identifies with the app access token and the intents for the configured
//     scenes, heartbeats, and resumes the session after a dropped connection.
//     Nothing has to be reachable from the internet.
//   - "webhook": the platform POSTs events to <public_url><path>. Every request
//     is verified with the Ed25519 key derived from the app secret, including
//     the console's URL-validation handshake (op 13), before anything is parsed.
//
// Scenes are where messages come from: c2c (private chat with the bot), group
// (@-mentions of the bot in a QQ group), guild (@-mentions in a guild channel)
// and dm (guild direct messages). Each inbound message becomes a session turn
// whose reply target is "<scene>:<id>"; Deliver answers there.
//
// Replies are passive when they can be: within the platform's reply window
// the latest inbound message's id goes along as msg_id with an increasing
// msg_seq, which is what the platform allows freely. Outside the window, or
// once the per-message reply count is spent, a reply goes out as a proactive
// message, which the platform rate-limits and a user can refuse.
//
// The driver speaks the HTTP API and the gateway protocol itself rather than
// through Tencent's Go SDK (github.com/tencent-connect/botgo): the SDK keeps
// its event handlers in package-level state, one slot per event type, so two
// QQ channels in one process would overwrite each other, and its built-in
// domains predate the platform's move to api.bot.qq.com.
package qqbot

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

// DefaultAPIBase is the platform's single API domain since 2026-08-10 (token,
// OpenAPI and gateway lookup all live under it).
const DefaultAPIBase = "https://api.bot.qq.com"

const (
	callTimeout   = 15 * time.Second
	uploadTimeout = 60 * time.Second

	// defaultPresignTTL is how long S3 presigned GET URLs handed to QQ stay
	// valid. QQ downloads the URL during the /files upload call (or shortly
	// after for guild image links), so 10 minutes absorbs clock skew and CDN
	// retries while keeping links short-lived.
	defaultPresignTTL = 10 * time.Minute

	// defaultMaxMediaBytes caps a single inbound download when the media policy
	// sets no limit of its own.
	defaultMaxMediaBytes = 8 << 20

	// maxOutboundMediaBytes caps inline base64 attachments staged to the blob
	// store before presigning (matches the Telegram driver ceiling).
	maxOutboundMediaBytes = 50 << 20
)

// Scene names, as configured and as the prefix of a reply target.
const (
	SceneC2C   = "c2c"
	SceneGroup = "group"
	SceneGuild = "guild"
	SceneDM    = "dm"
	// SceneGroupAll is every message in a group, not only the ones that
	// @-mention the bot. The platform sends these only to bots it has granted
	// full group-message access, so it is opt-in and a bot without the grant
	// simply keeps receiving @-mentions.
	SceneGroupAll = "group_all"
)

// DefaultScenes are the scenes a channel listens to when none are configured:
// private chats, group @-mentions and guild @-mentions. Guild direct messages
// need an intent the platform grants on application, so they are opt-in.
var DefaultScenes = []string{SceneC2C, SceneGroup, SceneGuild}

// Intent bits (https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/event-emit.html).
const (
	intentDirectMessage       = 1 << 12 // DIRECT_MESSAGE_CREATE
	intentGroupAndC2C         = 1 << 25 // C2C_MESSAGE_CREATE, GROUP_AT_MESSAGE_CREATE, GROUP_MESSAGE_CREATE
	intentPublicGuildMessages = 1 << 30 // AT_MESSAGE_CREATE
)

// Options configure a driver.
type Options struct {
	AppID  string
	Secret string // the app secret (AppSecret / clientSecret)
	Mode   string // "websocket" (default) | "webhook"
	Path   string // webhook mode: the route mounted on the shared server
	// PublicURL is only used to log the callback URL to enter in the console.
	PublicURL string
	Scenes    []string // empty = DefaultScenes
	APIBase   string   // empty = DefaultAPIBase
	// Markdown sends text replies in private chats and groups as markdown
	// messages (msg_type 2), which the platform grants per bot. A refused
	// markdown message is resent as plain text.
	Markdown bool
	// PresignTTL overrides how long presigned media URLs stay valid
	// (default 10m).
	PresignTTL time.Duration
	// Media is the centralized media service used for format negotiation
	// (e.g. WebP/PNG → JPEG), unified store lookup, and URL presigning
	// (S3 or FS HMAC-HTTP). When nil, falls back to the raw store parameter.
	Media *media.Service
}

// Driver is the QQ Bot channel driver. It implements gateway.Driver for
// replies and either holds a gateway connection or serves a webhook.
type Driver struct {
	name    string
	unknown sync.Map // event types already reported as unhandled
	agent   string
	appID   string
	mode    string
	path    string
	public  string
	scenes  map[string]bool
	apiBase string
	// markdown is the configured switch; mdOffUntil pauses it after the
	// platform refused a markdown message, so a bot without the permission
	// does not spend a passive reply on a doomed attempt every time.
	markdown   bool
	mdMu       sync.Mutex
	mdOffUntil time.Time
	// mdLimit is the longest markdown message the platform is known to
	// take, once it refused a longer one; zero until then.
	mdLimit    int
	presignTTL time.Duration
	media      *media.Service
	store      media.Store // nil = media disabled
	pol        media.Policy
	sink       router.Sink
	log        *slog.Logger
	client     *http.Client

	tokens *tokenSource
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey

	// refs remembers, per reply target, the inbound message a reply can be
	// passive to.
	refsMu sync.Mutex
	refs   map[string]*replyRef

	seen *dedup

	// gateway session, for resuming after a dropped connection.
	gwMu      sync.Mutex
	sessionID string
	seq       int64
	botID     string
	// wsURL overrides the gateway lookup (tests).
	wsURL string
	// retryBase is the first reconnect delay (tests shorten it).
	retryBase time.Duration
}

// New builds a driver. In webhook mode it mounts the callback route on srv.
func New(name, agent string, opts Options, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	mode := opts.Mode
	if mode == "" {
		mode = "websocket"
	}
	base := strings.TrimRight(opts.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	scenes := map[string]bool{}
	list := opts.Scenes
	if len(list) == 0 {
		list = DefaultScenes
	}
	for _, s := range list {
		scenes[strings.ToLower(strings.TrimSpace(s))] = true
	}
	presignTTL := opts.PresignTTL
	if presignTTL <= 0 {
		presignTTL = defaultPresignTTL
	}
	client := &http.Client{Timeout: uploadTimeout}
	priv := signingKey(opts.Secret)
	d := &Driver{
		name:       name,
		agent:      agent,
		appID:      opts.AppID,
		mode:       mode,
		path:       opts.Path,
		public:     strings.TrimRight(opts.PublicURL, "/"),
		scenes:     scenes,
		apiBase:    base,
		markdown:   opts.Markdown,
		presignTTL: presignTTL,
		media:      opts.Media,
		store:      store,
		pol:        pol,
		sink:       sink,
		log:        log.With("driver", "qq", "channel", name, "mode", mode),
		client:     client,
		tokens:     newTokenSource(opts.AppID, opts.Secret, base+"/app/getAppAccessToken", client),
		priv:       priv,
		pub:        priv.Public().(ed25519.PublicKey),
		refs:       map[string]*replyRef{},
		seen:       newDedup(4096, 30*time.Minute),
		retryBase:  time.Second,
	}
	if mode == "webhook" {
		if d.path == "" {
			d.path = "/webhook/qq/"
		}
		srv.Handle(d.path, d.handleWebhook)
	}
	return d
}

func (d *Driver) Name() string { return d.name }

// Start opens the gateway connection in websocket mode. In webhook mode the
// route is already mounted and the callback URL is set in the console, so
// there is nothing to register.
func (d *Driver) Start(ctx context.Context) error {
	switch d.mode {
	case "websocket":
		go d.runGateway(ctx)
		return nil
	case "webhook":
		if d.public != "" {
			d.log.Info("qq webhook ready; set this callback URL in the QQ Open Platform console", "url", d.public+d.path)
		} else {
			d.log.Info("qq webhook ready; set <public https url>"+d.path+" as the callback URL in the QQ Open Platform console", "path", d.path)
		}
		return nil
	default:
		return fmt.Errorf("qq: unknown mode %q (use websocket or webhook)", d.mode)
	}
}

// Stop is a no-op: the gateway goroutine exits with ctx and the shared
// httpd.Server owns the listener.
func (d *Driver) Stop(_ context.Context) {}

// intents is the gateway intent mask for the configured scenes.
func (d *Driver) intents() int {
	n := 0
	if d.scenes[SceneC2C] || d.scenes[SceneGroup] || d.scenes[SceneGroupAll] {
		n |= intentGroupAndC2C
	}
	if d.scenes[SceneGuild] {
		n |= intentPublicGuildMessages
	}
	if d.scenes[SceneDM] {
		n |= intentDirectMessage
	}
	return n
}

// ---- access token -----------------------------------------------------------

// tokenSource fetches and caches the app access token. The platform does not
// extend a token: a fresh one is requested in the last minute of the old one's
// life, during which both stay valid.
type tokenSource struct {
	appID, secret, url string
	client             *http.Client

	mu  sync.Mutex
	tok string
	exp time.Time
}

func newTokenSource(appID, secret, url string, client *http.Client) *tokenSource {
	return &tokenSource{appID: appID, secret: secret, url: url, client: client}
}

// refreshBefore is how long before expiry a new token is fetched.
const refreshBefore = 60 * time.Second

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Until(t.exp) > refreshBefore {
		return t.tok, nil
	}
	body, _ := json.Marshal(map[string]string{"appId": t.appID, "clientSecret": t.secret})
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("qq: access token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"` // documented as a number, sent as a string
		Code        int             `json:"code"`
		Message     string          `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("qq: access token: status %d: %w", resp.StatusCode, err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("qq: access token refused (status %d, code %d): %s — check app_id and secret", resp.StatusCode, out.Code, out.Message)
	}
	secs := parseSeconds(out.ExpiresIn)
	if secs <= 0 {
		secs = 7200
	}
	t.tok = out.AccessToken
	t.exp = time.Now().Add(time.Duration(secs) * time.Second)
	return t.tok, nil
}

// invalidate drops the cached token after the platform rejected it.
func (t *tokenSource) invalidate() {
	t.mu.Lock()
	t.tok = ""
	t.mu.Unlock()
}

// parseSeconds reads a JSON number or a numeric string.
func parseSeconds(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ---- OpenAPI calls ------------------------------------------------------------

// apiError is a non-2xx answer from the OpenAPI.
type apiError struct {
	Status  int
	Code    int    `json:"code"`
	ErrCode int    `json:"err_code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

func (e *apiError) Error() string {
	code := e.Code
	if code == 0 {
		code = e.ErrCode
	}
	return fmt.Sprintf("qq api: status %d code %d: %s", e.Status, code, e.Message)
}

// code is the platform's error code, from whichever field carried it.
func (e *apiError) code() int {
	if e.Code != 0 {
		return e.Code
	}
	return e.ErrCode
}

// call makes one authenticated OpenAPI request. A 401 drops the cached token
// and retries once with a fresh one.
func (d *Driver) call(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		tok, err := d.tokens.Token(ctx)
		if err != nil {
			return err
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, d.apiBase+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "QQBot "+tok)
		req.Header.Set("X-Union-Appid", d.appID)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := &apiError{Status: resp.StatusCode}
			_ = json.Unmarshal(data, e)
			if e.Message == "" {
				e.Message = strings.TrimSpace(string(data))
			}
			return e
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("qq api: %s %s: bad response: %w", method, path, err)
			}
		}
		return nil
	}
}

// BotInfo describes a verified QQ Bot application and its gateway metadata.
type BotInfo struct {
	AppID      string `json:"app_id"`
	BotID      string `json:"bot_id,omitempty"`
	Username   string `json:"username,omitempty"`
	GatewayURL string `json:"gateway_url,omitempty"`
	Shards     int    `json:"shards,omitempty"`
	APIBase    string `json:"api_base,omitempty"`
}

// TestCredentials exchanges AppID + AppSecret for an access token, verifies
// OpenAPI access via GET /gateway/bot, and best-effort fetches bot profile
// metadata from GET /users/@me. Secrets and tokens are scrubbed from errors.
func TestCredentials(ctx context.Context, appID, secret, apiBase string, client *http.Client) (BotInfo, error) {
	appID = strings.TrimSpace(appID)
	secret = strings.TrimSpace(secret)
	if appID == "" {
		return BotInfo{}, errors.New("qq app_id is required")
	}
	if secret == "" {
		return BotInfo{}, errors.New("qq app secret is required")
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = DefaultAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	var tok string
	scrub := func(err error) error {
		if err == nil {
			return nil
		}
		msg := strings.ReplaceAll(err.Error(), secret, "***")
		if tok != "" {
			msg = strings.ReplaceAll(msg, tok, "***")
		}
		return errors.New(msg)
	}

	ts := newTokenSource(appID, secret, base+"/app/getAppAccessToken", client)
	var err error
	tok, err = ts.Token(ctx)
	if err != nil {
		return BotInfo{}, scrub(err)
	}

	doGET := func(path string, out any) error {
		reqCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+path, nil)
		if err != nil {
			return scrub(err)
		}
		req.Header.Set("Authorization", "QQBot "+tok)
		req.Header.Set("X-Union-Appid", appID)
		resp, err := client.Do(req)
		if err != nil {
			return scrub(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := &apiError{Status: resp.StatusCode}
			_ = json.Unmarshal(data, e)
			if e.Message == "" {
				e.Message = strings.TrimSpace(string(data))
			}
			return scrub(e)
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return scrub(fmt.Errorf("qq api: GET %s: bad response: %w", path, err))
			}
		}
		return nil
	}

	var gw struct {
		URL    string `json:"url"`
		Shards int    `json:"shards"`
	}
	if err := doGET("/gateway/bot", &gw); err != nil {
		return BotInfo{}, err
	}
	info := BotInfo{
		AppID:      appID,
		GatewayURL: gw.URL,
		Shards:     gw.Shards,
		APIBase:    base,
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := doGET("/users/@me", &me); err == nil {
		info.BotID = me.ID
		info.Username = me.Username
	}
	return info, nil
}

// isCode reports whether err is an API error with one of codes.
func isCode(err error, codes ...int) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range codes {
		if e.code() == c {
			return true
		}
	}
	return false
}
