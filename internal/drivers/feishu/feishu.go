// Package feishu implements a Feishu / Lark Open Platform channel driver.
//
// Inbound events arrive over either Feishu's persistent WebSocket long-connection
// protocol (mode: "websocket", the default — no public HTTPS callback needed)
// or an HTTP Event Subscription webhook (mode: "webhook", verified with optional
// SHA-256 X-Lark-Signature, AES-256-CBC decryption, and verification token).
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

// DefaultAPIBase is the Feishu Open Platform base URL. International Lark
// tenants set api_base to "https://open.larksuite.com".
const DefaultAPIBase = "https://open.feishu.cn"

const (
	callTimeout           = 15 * time.Second
	uploadTimeout         = 60 * time.Second
	defaultMaxMediaBytes  = 8 << 20
	maxOutboundMediaBytes = 30 << 20
	tokenRefreshBefore    = 120 * time.Second
)

// Scene names matching gateway/router conventions.
const (
	SceneP2P      = "p2p"
	SceneGroup    = "group"
	SceneGroupAll = "group_all"
)

// DefaultScenes enables direct chats and @-mentions in groups. Use group_all to
// also receive unmentioned group messages.
var DefaultScenes = []string{SceneP2P, SceneGroup}

// Options configures a Feishu/Lark driver instance.
type Options struct {
	AppID      string
	Secret     string
	Token      string // optional webhook verification token
	EncryptKey string // optional webhook Encrypt Key (AES-256-CBC + X-Lark-Signature)
	Mode       string // "websocket" (default) | "webhook"
	Path       string // mounted route in webhook mode
	PublicURL  string // optional public base URL for startup log hint
	Scenes     []string
	APIBase    string
	Markdown   bool
	Media      *media.Service
}

// Driver connects AgentFlow to one Feishu or Lark bot application.
type Driver struct {
	name        string
	unknown     sync.Map
	agent       string
	appID       string
	appSecret   string
	verifyToken string
	encryptKey  string
	mode        string
	path        string
	public      string
	scenes      map[string]bool
	apiBase     string

	markdown   bool
	mdMu       sync.Mutex
	mdOffUntil time.Time

	media  *media.Service
	store  media.Store
	pol    media.Policy
	sink   router.Sink
	log    *slog.Logger
	client *http.Client

	tokens *tokenSource
	seen   *dedup

	botMu     sync.Mutex
	botOpenID string

	// wsURL overrides /callback/ws/endpoint in tests.
	wsURL     string
	retryBase time.Duration
}

// New constructs a Feishu/Lark driver and registers its webhook route when in webhook mode.
func New(name, agent string, opts Options, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
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
	if log == nil {
		log = slog.Default()
	}
	client := &http.Client{Timeout: uploadTimeout}
	d := &Driver{
		name:        name,
		agent:       agent,
		appID:       strings.TrimSpace(opts.AppID),
		appSecret:   opts.Secret,
		verifyToken: opts.Token,
		encryptKey:  opts.EncryptKey,
		mode:        mode,
		path:        opts.Path,
		public:      strings.TrimRight(opts.PublicURL, "/"),
		scenes:      scenes,
		apiBase:     base,
		markdown:    opts.Markdown,
		media:       opts.Media,
		store:       store,
		pol:         pol,
		sink:        sink,
		log:         log.With("driver", "feishu", "channel", name, "mode", mode),
		client:      client,
		tokens:      newTokenSource(strings.TrimSpace(opts.AppID), opts.Secret, base+"/open-apis/auth/v3/tenant_access_token/internal", client),
		seen:        newDedup(4096, 30*time.Minute),
		retryBase:   time.Second,
	}
	if mode == "webhook" {
		if d.path == "" {
			d.path = "/webhook/feishu/"
		}
		if srv != nil {
			srv.Handle(d.path, d.handleWebhook)
		}
	}
	return d
}

// Name returns the configured channel name.
func (d *Driver) Name() string { return d.name }

// Start begins receiving events in websocket mode or logs the callback URL in webhook mode.
func (d *Driver) Start(ctx context.Context) error {
	switch d.mode {
	case "websocket":
		go d.runGateway(ctx)
		return nil
	case "webhook":
		if d.public != "" {
			d.log.Info("feishu webhook ready; set this Request URL in the Feishu/Lark console", "url", d.public+d.path)
		} else {
			d.log.Info("feishu webhook ready; set <public https url>"+d.path+" as the Request URL in the Feishu/Lark console", "path", d.path)
		}
		return nil
	default:
		return fmt.Errorf("feishu: unknown mode %q (use websocket or webhook)", d.mode)
	}
}

// Stop fulfills the driver lifecycle contract; ctx cancellation stops the WS loop.
func (d *Driver) Stop(_ context.Context) {}

func (d *Driver) resolveBotOpenID(ctx context.Context) string {
	d.botMu.Lock()
	if d.botOpenID != "" {
		id := d.botOpenID
		d.botMu.Unlock()
		return id
	}
	d.botMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var out struct {
		Bot *struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
		Data *struct {
			Bot *struct {
				OpenID string `json:"open_id"`
			} `json:"bot"`
		} `json:"data"`
	}
	if err := d.call(ctx, http.MethodGet, "/open-apis/bot/v3/info", nil, &out); err != nil {
		return ""
	}
	id := ""
	if out.Bot != nil && out.Bot.OpenID != "" {
		id = out.Bot.OpenID
	} else if out.Data != nil && out.Data.Bot != nil {
		id = out.Data.Bot.OpenID
	}
	if id != "" {
		d.botMu.Lock()
		d.botOpenID = id
		d.botMu.Unlock()
	}
	return id
}

// ---- tenant access token ----------------------------------------------------

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

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Until(t.exp) > tokenRefreshBefore {
		return t.tok, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": t.appID, "app_secret": t.secret})
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu: tenant_access_token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int64  `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("feishu: tenant_access_token: status %d: %w", resp.StatusCode, err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu: tenant_access_token refused (status %d, code %d): %s", resp.StatusCode, out.Code, out.Msg)
	}
	secs := out.Expire
	if secs <= 0 {
		secs = 7200
	}
	t.tok = out.TenantAccessToken
	t.exp = time.Now().Add(time.Duration(secs) * time.Second)
	return t.tok, nil
}

func (t *tokenSource) invalidate() {
	t.mu.Lock()
	t.tok = ""
	t.mu.Unlock()
}

// ---- OpenAPI client ---------------------------------------------------------

type apiError struct {
	Status int
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("feishu api: status %d code %d: %s", e.Status, e.Code, e.Msg)
}

func isTokenExpiredCode(code int) bool {
	return code == 99991661 || code == 99991663 || code == 99991664
}

func isAPIError(err error) (*apiError, bool) {
	var e *apiError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

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
		req.Header.Set("Authorization", "Bearer "+tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		var envelope struct {
			Code int             `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(data, &envelope)
		if (resp.StatusCode == http.StatusUnauthorized || isTokenExpiredCode(envelope.Code)) && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 || envelope.Code != 0 {
			msg := envelope.Msg
			if msg == "" {
				msg = strings.TrimSpace(string(data))
			}
			return &apiError{Status: resp.StatusCode, Code: envelope.Code, Msg: msg}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("feishu api: %s %s: bad response: %w", method, path, err)
			}
		}
		return nil
	}
}
