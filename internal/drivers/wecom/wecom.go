// Package wecom implements a WeCom (企业微信 / WeChat Work) application channel driver.
//
// Inbound callbacks arrive via HTTPS (GET URL verification + POST encrypted XML
// with SHA-1 msg_signature and AES-256-CBC decryption using EncodingAESKey).
// Outbound replies use /cgi-bin/message/send (or /cgi-bin/appchat/send) with a
// cached access_token refreshed from /cgi-bin/gettoken.
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

// DefaultAPIBase is WeCom's official HTTP API base URL.
const DefaultAPIBase = "https://qyapi.weixin.qq.com"

const (
	callTimeout           = 15 * time.Second
	uploadTimeout         = 60 * time.Second
	defaultMaxMediaBytes  = 8 << 20
	maxOutboundMediaBytes = 20 << 20
	tokenRefreshBefore    = 120 * time.Second
)

// Options configures a WeCom channel driver.
type Options struct {
	CorpID     string
	AgentID    int64
	Secret     string // CorpSecret
	Token      string // callback verification Token
	EncryptKey string // 43-char EncodingAESKey
	Path       string // webhook route path
	PublicURL  string // optional public base URL for startup log hint
	APIBase    string // empty = DefaultAPIBase
	Markdown   bool
	Media      *media.Service
}

// Driver integrates an AgentFlow agent with one WeCom self-built app.
type Driver struct {
	name    string
	unknown sync.Map
	agent   string
	corpID  string
	agentID int64
	token   string
	aesKey  []byte
	keyErr  error
	path    string
	public  string
	apiBase string

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
}

// New constructs a WeCom channel driver and mounts its webhook callback route.
func New(name, agent string, opts Options, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	base := strings.TrimRight(opts.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	if log == nil {
		log = slog.Default()
	}
	aesKey, keyErr := DecodeAESKey(opts.EncryptKey)
	path := opts.Path
	if path == "" {
		path = "/webhook/wecom/"
	}
	client := &http.Client{Timeout: uploadTimeout}
	d := &Driver{
		name:     name,
		agent:    agent,
		corpID:   strings.TrimSpace(opts.CorpID),
		agentID:  opts.AgentID,
		token:    opts.Token,
		aesKey:   aesKey,
		keyErr:   keyErr,
		path:     path,
		public:   strings.TrimRight(opts.PublicURL, "/"),
		apiBase:  base,
		markdown: opts.Markdown,
		media:    opts.Media,
		store:    store,
		pol:      pol,
		sink:     sink,
		log:      log.With("driver", "wecom", "channel", name),
		client:   client,
		tokens:   newTokenSource(strings.TrimSpace(opts.CorpID), opts.Secret, base, client),
		seen:     newDedup(4096, 30*time.Minute),
	}
	if srv != nil {
		srv.Handle(d.path, d.handleWebhook)
	}
	return d
}

// Name returns the configured channel name.
func (d *Driver) Name() string { return d.name }

// Start validates crypto readiness and logs the callback URL to configure in WeCom Admin.
func (d *Driver) Start(_ context.Context) error {
	if d.keyErr != nil {
		return d.keyErr
	}
	if d.public != "" {
		d.log.Info("wecom webhook ready; set this Receive Message URL in WeCom Admin", "url", d.public+d.path)
	} else {
		d.log.Info("wecom webhook ready; set <public https url>"+d.path+" as the Receive Message URL in WeCom Admin", "path", d.path)
	}
	return nil
}

// Stop fulfills the driver lifecycle contract.
func (d *Driver) Stop(_ context.Context) {}

// ---- access token -----------------------------------------------------------

type tokenSource struct {
	corpID, secret, apiBase string
	client                  *http.Client

	mu  sync.Mutex
	tok string
	exp time.Time
}

func newTokenSource(corpID, secret, apiBase string, client *http.Client) *tokenSource {
	return &tokenSource{corpID: corpID, secret: secret, apiBase: apiBase, client: client}
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Until(t.exp) > tokenRefreshBefore {
		return t.tok, nil
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		t.apiBase, url.QueryEscape(t.corpID), url.QueryEscape(t.secret))
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("wecom: gettoken: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("wecom: gettoken status %d: %w", resp.StatusCode, err)
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", fmt.Errorf("wecom: gettoken refused (status %d, errcode %d): %s", resp.StatusCode, out.ErrCode, out.ErrMsg)
	}
	secs := out.ExpiresIn
	if secs <= 0 {
		secs = 7200
	}
	t.tok = out.AccessToken
	t.exp = time.Now().Add(time.Duration(secs) * time.Second)
	return t.tok, nil
}

func (t *tokenSource) invalidate() {
	t.mu.Lock()
	t.tok = ""
	t.mu.Unlock()
}

// ---- HTTP API calls ---------------------------------------------------------

type apiError struct {
	Status  int
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("wecom api: status %d errcode %d: %s", e.Status, e.ErrCode, e.ErrMsg)
}

func isTokenExpiredErr(code int) bool {
	return code == 40014 || code == 42001 || code == 42007 || code == 42009
}

func isAPIError(err error) (*apiError, bool) {
	var e *apiError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func (d *Driver) callJSON(ctx context.Context, method, path string, in, out any) error {
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
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		targetURL := d.apiBase + path + sep + "access_token=" + url.QueryEscape(tok)
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
		if err != nil {
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()

		var envelope apiError
		envelope.Status = resp.StatusCode
		_ = json.Unmarshal(data, &envelope)
		if (resp.StatusCode == http.StatusUnauthorized || isTokenExpiredErr(envelope.ErrCode)) && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 || envelope.ErrCode != 0 {
			if envelope.ErrMsg == "" {
				envelope.ErrMsg = strings.TrimSpace(string(data))
			}
			return &envelope
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("wecom api: %s %s: bad response: %w", method, path, err)
			}
		}
		return nil
	}
}
