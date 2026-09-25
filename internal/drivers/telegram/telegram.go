// Package telegram is the Telegram channel driver.
//
// Three modes, selected by the "mode" channel config:
//   - "polling" (default): long-poll getUpdates in, sendMessage out.
//   - "webhook": receives POST updates at a path, sendMessage out. Requires
//     public_url to be set (the runtime must be externally reachable).
//   - "auto": health-probe the public_url at startup — setWebhook if
//     reachable, otherwise deleteWebhook and long-poll. Falls back to polling
//     without the webhook host dying silently.
//
// Webhook deliveries (webhook/auto) are authenticated: a secret_token is sent
// at setWebhook (configured per channel, or generated per boot when absent)
// and verified from X-Telegram-Bot-Api-Secret-Token on every delivery, before
// the body is parsed. Polling paths clear any stale Telegram-side webhook
// first — getUpdates 409s while one is registered.
//
// Access control (allow_users) is enforced in all modes before any Lua runs.
//
// Media (photos, documents, voice, audio, video) is ingested when the
// channel's media policy allows it: the file is downloaded via getFile and
// landed in the blob store; the message carries small part descriptors
// (handles), never the bytes. Replies carrying attachments upload them via
// multipart (sendPhoto/sendAudio/sendVideo/sendDocument).
//
// The Bot API itself is spoken through github.com/go-telegram/bot. The webhook
// *receiver* is not: handleWebhook is hand-written so its status codes (405,
// 503, 401, 403, 400, 200) and the order in which they are reached stay exactly
// what README.md promises, and the body is decoded into this package's own
// thin inbound types rather than the SDK's full models.
package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

const (
	// pollTimeout is the getUpdates long-poll window. The SDK derives the
	// `timeout` parameter from it and expects the HTTP client to outlive it, so
	// the driver's own client timeout is set well above this.
	pollTimeout = 30 * time.Second

	// callTimeout bounds one small Bot API call, uploadTimeout one that carries
	// a file. Both bound a single attempt only — a rate-limit wait happens
	// between attempts, outside the deadline.
	callTimeout   = 15 * time.Second
	uploadTimeout = 60 * time.Second

	// rateLimitRetries is how many times a 429 is slept out before the call is
	// given up on. Telegram's retry_after is its own estimate, so three waits
	// ride out a throttled burst without stalling a reply for minutes.
	rateLimitRetries = 3

	// photoCaptionLimit is Telegram's caption length limit, in characters.
	photoCaptionLimit = 1024

	// defaultMaxMediaBytes caps a single ingest when the media policy sets no
	// limit of its own.
	defaultMaxMediaBytes = 8 << 20
)

// Driver is the Telegram channel driver. It implements gateway.Driver for
// replies (sendMessage / sendPhoto / ...) and either polls or serves a
// webhook for inbound.
type Driver struct {
	name        string
	token       string
	agent       string
	allow       map[int64]bool // empty = allow all
	mode        string         // "polling" | "webhook" | "auto"
	path        string         // webhook mode only
	publicURL   string         // webhook/auto: external base that routes to this process
	secretToken string         // webhook/auto: Telegram webhook secret (verified per delivery); empty = unauthenticated
	apiBase     string         // override for the Telegram API host (tests); empty = api.telegram.org
	store       media.Store    // nil = media policy disabled
	pol         media.Policy
	sink        router.Sink
	log         *slog.Logger
	client      *http.Client

	// The SDK client is built on first use rather than in New: New must stay
	// infallible (it installs the webhook handler and mints the secret), and
	// tests construct a Driver literal with only the fields they need — so the
	// wiring inputs above stay the seam and the client is derived from them.
	botOnce sync.Once
	bot     *bot.Bot
	botErr  error
}

func New(name, token, agent, mode string, allowUsers []int64, path, publicURL, secretToken string, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	allow := map[int64]bool{}
	for _, id := range allowUsers {
		allow[id] = true
	}
	if mode == "" {
		mode = "polling"
	}
	if (mode == "webhook" || mode == "auto") && secretToken == "" {
		// Secure by default: a webhook that verifies nothing invites update
		// injection by anyone who finds the path. Random-per-boot is safe
		// because Start re-registers the webhook (with the new secret) every
		// boot — Telegram only presents the secret it was last given.
		generated, err := generateWebhookSecret()
		if err != nil {
			// Fail closed: handleWebhook refuses every delivery without a
			// secret, so this channel is inert until a restart succeeds. Logged
			// at error rather than warning because nothing else will report it.
			log.Error("telegram webhook secret_token generation failed; webhook deliveries will be refused", "channel", name, "err", err)
		} else {
			secretToken = generated
			log.Info("telegram webhook secret_token generated (none configured)", "channel", name)
		}
	}
	d := &Driver{
		name:        name,
		token:       token,
		agent:       agent,
		allow:       allow,
		mode:        mode,
		path:        path,
		publicURL:   publicURL,
		secretToken: secretToken,
		store:       store,
		pol:         pol,
		sink:        sink,
		log:         log.With("driver", "telegram", "channel", name, "mode", mode),
		client:      &http.Client{Timeout: 40 * time.Second},
	}
	// webhook and auto both serve the webhook path, so both attach their handler
	// up front. auto may later fall back to polling, but the mounted path is
	// then simply unused (Telegram won't call it after deleteWebhook) — harmless,
	// and it lets the listener bind before the probe runs.
	if mode == "webhook" || mode == "auto" {
		if d.path == "" {
			d.path = "/webhook/telegram/"
		}
		srv.Handle(d.path, d.handleWebhook)
	}
	return d
}

func (d *Driver) Name() string { return d.name }

// Start launches the driver by mode. It must be called AFTER the shared
// httpd.Server has bound its listener (see main.go), because auto probes
// <public_url>/health — which round-trips through the public proxy back to that
// listener — and a probe before bind would always fail.
func (d *Driver) Start(ctx context.Context) error {
	switch d.mode {
	case "polling":
		d.ensureNoWebhook("polling mode")
		go d.poll(ctx)
		return nil
	case "webhook":
		if d.publicURL == "" {
			return fmt.Errorf("telegram webhook mode requires gateway.public_url")
		}
		if err := d.setWebhook(d.publicURL + strings.TrimRight(d.path, "/") + "/"); err != nil {
			return fmt.Errorf("setWebhook: %w", err)
		}
		d.log.Info("telegram webhook registered", "url", d.publicURL+d.path)
		return nil
	case "auto":
		return d.startAuto(ctx)
	default:
		return fmt.Errorf("telegram: unknown mode %q (use polling, webhook, or auto)", d.mode)
	}
}

// startAuto implements the health-gated webhook→polling fallback.
func (d *Driver) startAuto(ctx context.Context) error {
	if d.publicURL == "" {
		d.log.Info("auto: no public_url, polling")
		d.ensureNoWebhook("auto: no public_url")
		go d.poll(ctx)
		return nil
	}
	r := httpd.Probe(ctx, d.publicURL)
	d.log.Info("auto: health probe", "url", d.publicURL, "ok", r.OK, "latency_ms", r.Latency.Milliseconds(), "err", r.Err)
	if !r.OK {
		// Webhook host is unreachable: ensure Telegram isn't holding a stale
		// webhook URL against us, then long-poll. This is the documented
		// fallback — a failed GET to the domain disables webhook.
		if err := d.deleteWebhook(); err != nil {
			d.log.Warn("auto: deleteWebhook failed", "err", err)
		}
		d.log.Info("auto: webhook unreachable, falling back to polling")
		go d.poll(ctx)
		return nil
	}
	// healthy: register the webhook with Telegram. The handler is already
	// mounted (New attaches it for auto too), so no registration here.
	hookURL := d.publicURL + strings.TrimRight(d.path, "/") + "/"
	if err := d.setWebhook(hookURL); err != nil {
		d.log.Warn("auto: setWebhook failed, polling instead", "err", err)
		// The webhook may have registered on a previous boot (or partially
		// applied just now): getUpdates 409s against a stale webhook, so clear
		// it before falling back to polling.
		d.ensureNoWebhook("auto: setWebhook failed")
		go d.poll(ctx)
		return nil
	}
	d.log.Info("auto: webhook registered", "url", hookURL)
	return nil
}

// Stop is a no-op now; the shared httpd.Server owns the listener lifetime and
// polling exits via ctx cancellation. Method retained for the Driver surface.
func (d *Driver) Stop(_ context.Context) {}

// tg returns the SDK client, building it on first use. Construction is deferred
// so that a Driver literal (the tests) can be handed to any method, and so that
// a token the operator is still editing does not fail channel setup before the
// first call.
func (d *Driver) tg() (*bot.Bot, error) {
	d.botOnce.Do(func() {
		opts := []bot.Option{
			// New calls getMe to validate the token unless told not to. The
			// driver never needs the bot's own user, and a dead network at boot
			// is not a reason to refuse to build the channel.
			bot.WithSkipGetMe(),
			// The long-poll window the SDK asks for comes from this timeout; the
			// HTTP client handed alongside it must outlive it, so the two travel
			// together and the tests' short-timeout client stays in charge.
			bot.WithHTTPClient(pollTimeout, d.httpClient()),
			// Nothing but messages carries anything this driver can act on, so
			// getUpdates asks Telegram for nothing else.
			bot.WithAllowedUpdates(bot.AllowedUpdates{"message"}),
			// Handle updates inline, on the polling goroutine. The default is a
			// goroutine per update, which would reorder a chat's messages under
			// the router and let a flood of updates spawn unbounded goroutines.
			bot.WithNotAsyncHandlers(),
			// Polling failures (429, 5xx, network) belong in the channel's log
			// with its driver/channel fields, not in the SDK's package-level one.
			bot.WithErrorsHandler(func(err error) { d.log.Warn("telegram api error", "err", err) }),
			// A polling update arrives as the SDK's model; handleUpdate speaks
			// this package's own shape, which is also what handleWebhook decodes.
			bot.WithDefaultHandler(func(_ context.Context, _ *bot.Bot, u *models.Update) {
				d.handleUpdate(fromSDK(u))
			}),
		}
		if base := d.apiBase; base != "" {
			opts = append(opts, bot.WithServerURL(base))
		}
		d.bot, d.botErr = bot.New(d.token, opts...)
	})
	if d.botErr != nil {
		return nil, fmt.Errorf("telegram: bot client: %w", d.botErr)
	}
	return d.bot, nil
}

// httpClient is the client every Telegram request goes through, defaulting when
// a driver was built without one.
func (d *Driver) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	return &http.Client{Timeout: callTimeout}
}

// call runs one outbound Bot API call, sleeping out a 429 and trying again
// while it lasts. The SDK parses retry_after into *bot.TooManyRequestsError but
// only acts on it in its own polling loop, so without this a throttled channel
// silently drops replies. errors.As is the wrapped-tolerant form of the SDK's
// bot.IsTooManyRequestsError. timeout bounds one attempt; the wait between
// attempts is deliberately outside it, since retry_after is usually longer than
// one call's budget.
func (d *Driver) call(method string, timeout time.Duration, fn func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := fn(ctx)
		cancel()
		if err == nil {
			return nil
		}
		var limited *bot.TooManyRequestsError
		if !errors.As(err, &limited) || attempt >= rateLimitRetries {
			return err
		}
		wait := time.Duration(limited.RetryAfter) * time.Second
		if wait <= 0 {
			// "Too many requests" with no window to respect; a second keeps the
			// retry from becoming a spin.
			wait = time.Second
		}
		d.log.Warn("telegram rate limited, backing off",
			"method", method, "retry_after_s", int(wait.Seconds()), "attempt", attempt+1)
		time.Sleep(wait)
	}
}

// ensureNoWebhook clears any Telegram-side webhook before polling begins. A
// stale webhook — left over from a webhook-mode deployment or an auto
// setWebhook that failed after applying — makes every getUpdates fail with a
// 409 "Conflict: can't use getUpdates method while webhook is active", so
// polling must not start while one is registered. Best-effort: a network
// blip here shouldn't block polling, but the 409 then persists until the
// next boot, so a failure is a loud WARN rather than a swallowed error.
func (d *Driver) ensureNoWebhook(why string) {
	if err := d.deleteWebhook(); err != nil {
		d.log.Warn("deleteWebhook before polling failed; getUpdates may 409 against a stale webhook", "why", why, "err", err)
	}
}

// generateWebhookSecret makes a per-instance webhook secret. 32 random bytes
// in base64url give 43 chars from [A-Za-z0-9_-] — inside Telegram's 1-256
// char secret alphabet.
func generateWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setWebhook registers hookURL with Telegram so updates are pushed there.
// An empty URL clears the webhook (see deleteWebhook). drop_pending_updates is
// left false — the SDK omits the field, which is the same thing to Telegram —
// so a mode switch does not discard updates that were already in flight. When a
// secretToken is set it is registered too, and Telegram then presents it on
// every delivery as the X-Telegram-Bot-Api-Secret-Token header (verified in
// handleWebhook). A 429 is slept out rather than failing the channel, since
// webhook registration failing means no inbound at all.
func (d *Driver) setWebhook(hookURL string) error {
	b, err := d.tg()
	if err != nil {
		return err
	}
	params := &bot.SetWebhookParams{
		URL:            hookURL,
		AllowedUpdates: []string{"message"},
	}
	if d.secretToken != "" {
		params.SecretToken = d.secretToken
	}
	return d.call("setWebhook", callTimeout, func(ctx context.Context) error {
		_, err := b.SetWebhook(ctx, params)
		return err
	})
}

// deleteWebhook clears any registered webhook URL so Telegram stops pushing.
// drop_pending_updates is explicitly false (and the SDK then omits it, which
// Telegram reads the same way): nothing already queued should be lost.
func (d *Driver) deleteWebhook() error {
	b, err := d.tg()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err = b.DeleteWebhook(ctx, &bot.DeleteWebhookParams{DropPendingUpdates: false})
	return err
}

// --- inbound ---

// update is one Telegram update, reduced to the parts this driver acts on. It
// is decoded from the webhook request body and, for polling, converted from the
// SDK's model by fromSDK.
type update struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

// tgMessage is the message portion of a Telegram update, including media.
type tgMessage struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	From      struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Photo []photoSize `json:"photo"`
	Doc   *document   `json:"document"`
	Voice *voice      `json:"voice"`
	Audio *audio      `json:"audio"`
	Video *video      `json:"video"`
}

// The attachment shapes are named types rather than inline structs because both
// inbound paths have to build one: the webhook decodes them and fromSDK maps
// the SDK's models into them.
type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIME     string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MIME     string `json:"mime_type"`
}

type audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIME     string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type video struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIME     string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type photoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// fromSDK maps one polling update onto the driver's own inbound shape. The
// webhook path decodes that shape straight from the request body (its handler
// is hand-written, the SDK's is not used), so polling converts here rather than
// the driver growing a second inbound type — and with it a second path to the
// allow-list check, media ingest and text extraction.
func fromSDK(u *models.Update) update {
	out := update{UpdateID: u.ID}
	m := u.Message
	if m == nil {
		return out
	}
	msg := &tgMessage{
		MessageID: int64(m.ID),
		Text:      m.Text,
		Caption:   m.Caption,
	}
	out.Message = msg
	if m.From != nil {
		msg.From.ID = m.From.ID
		msg.From.Username = m.From.Username
		msg.From.FirstName = m.From.FirstName
	}
	msg.Chat.ID = m.Chat.ID
	msg.Chat.Type = string(m.Chat.Type)
	for _, p := range m.Photo {
		msg.Photo = append(msg.Photo, photoSize{
			FileID:   p.FileID,
			Width:    p.Width,
			Height:   p.Height,
			FileSize: int64(p.FileSize),
		})
	}
	if doc := m.Document; doc != nil {
		msg.Doc = &document{FileID: doc.FileID, FileName: doc.FileName, MIME: doc.MimeType, FileSize: doc.FileSize}
	}
	if v := m.Voice; v != nil {
		msg.Voice = &voice{FileID: v.FileID, Duration: v.Duration, MIME: v.MimeType}
	}
	if a := m.Audio; a != nil {
		msg.Audio = &audio{FileID: a.FileID, FileName: a.FileName, MIME: a.MimeType, FileSize: a.FileSize}
	}
	if v := m.Video; v != nil {
		msg.Video = &video{FileID: v.FileID, FileName: v.FileName, MIME: v.MimeType, FileSize: v.FileSize}
	}
	return out
}

// --- polling mode ---

// poll hands the getUpdates loop to the SDK: offset tracking, the long-poll
// window and the 429 backoff all live in there now. It blocks until ctx is
// done, which is why every caller runs it on its own goroutine.
func (d *Driver) poll(ctx context.Context) {
	b, err := d.tg()
	if err != nil {
		d.log.Error("telegram polling cannot start", "err", err)
		return
	}
	d.log.Info("telegram polling started")
	b.Start(ctx)
	d.log.Info("telegram polling stopped")
}

// --- webhook mode ---

// secretTokenHeader is the header Telegram presents the registered webhook
// secret in on every delivery.
const secretTokenHeader = "X-Telegram-Bot-Api-Secret-Token"

func (d *Driver) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Authenticate before parsing: an unauthenticated POST must never reach
	// update handling. Missing is 401, mismatched is 403; both are checked
	// in constant time.
	//
	// A driver with no secret refuses everything. It used to fall through, which
	// meant a generation failure at construction — only warned about — left the
	// webhook path open to anyone who found it.
	if d.secretToken == "" {
		http.Error(w, "webhook authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	got := r.Header.Get(secretTokenHeader)
	if got == "" {
		http.Error(w, "missing secret token", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(d.secretToken)) != 1 {
		http.Error(w, "secret token mismatch", http.StatusForbidden)
		return
	}
	var u update
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&u); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	d.handleUpdate(u)
	w.WriteHeader(http.StatusOK)
}

// --- shared ---

func (d *Driver) handleUpdate(u update) {
	m := u.Message
	if m == nil {
		return
	}
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	// The allow-list is checked before media is ingested. ingestMedia downloads
	// the file from Telegram and writes it to the blob store, so checking the
	// sender afterwards let a non-allowed user make the runtime do both.
	if len(d.allow) > 0 && !d.allow[m.From.ID] {
		d.log.Warn("dropping message from non-allowed user", "user_id", m.From.ID)
		return
	}
	atts := d.ingestMedia(m)
	if text == "" && len(atts) == 0 {
		return
	}
	d.log.Debug("telegram update received", "update_id", u.UpdateID, "chat_id", m.Chat.ID, "user_id", m.From.ID, "text_len", len(text), "media", len(atts))
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          fmt.Sprintf("tg-%d", u.UpdateID),
			Type:        "user",
			From:        "user:telegram:" + strconv.FormatInt(m.From.ID, 10),
			Text:        text,
			Channel:     d.name,
			ReplyTo:     chatID,
			Attachments: atts,
			Ts:          time.Now().Unix(),
			Payload: map[string]any{
				"chat_id":  chatID,
				"user_id":  m.From.ID,
				"username": m.From.Username,
				"name":     m.From.FirstName,
			},
		},
	})
}

// ingestMedia downloads the update's media (photo/document/voice/audio/video)
// into the blob store and returns part descriptors. With media disabled
// (nil store / empty policy) every part is dropped and nil returned — the
// message still flows with its caption text.
func (d *Driver) ingestMedia(m *tgMessage) []media.Part {
	if d.store == nil || !d.pol.Enabled() {
		return nil
	}
	var parts []media.Part
	add := func(fileID, mime, name string, size int64) {
		p, err := d.downloadFile(fileID, mime, name, size)
		if err != nil {
			d.log.Warn("media ingest failed", "err", err)
			metrics.Inc("agentflow_media_unsupported")
			return
		}
		parts = append(parts, p)
	}
	switch {
	case len(m.Photo) > 0:
		// Telegram sends several sizes; the largest is last.
		best := m.Photo[len(m.Photo)-1]
		add(best.FileID, "image/jpeg", "photo.jpg", best.FileSize)
	case m.Doc != nil:
		add(m.Doc.FileID, m.Doc.MIME, m.Doc.FileName, m.Doc.FileSize)
	case m.Voice != nil:
		add(m.Voice.FileID, orDefault(m.Voice.MIME, "audio/ogg"), "voice.ogg", 0)
	case m.Audio != nil:
		add(m.Audio.FileID, orDefault(m.Audio.MIME, "audio/mpeg"), orDefault(m.Audio.FileName, "audio"), m.Audio.FileSize)
	case m.Video != nil:
		add(m.Video.FileID, orDefault(m.Video.MIME, "video/mp4"), orDefault(m.Video.FileName, "video.mp4"), m.Video.FileSize)
	}
	if len(parts) > 0 {
		metrics.Inc("agentflow_media_ingested")
	}
	return parts
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// downloadFile fetches a Telegram file by id (getFile → file download URL)
// and lands it in the blob store under the channel policy. getFile goes through
// the SDK; the bytes themselves do not, since the SDK exposes the download link
// and nothing else.
func (d *Driver) downloadFile(fileID, mime, name string, size int64) (media.Part, error) {
	if mime == "" {
		mime = "application/octet-stream"
	}
	if !d.pol.Allows(mime) {
		return media.Part{}, fmt.Errorf("telegram: mime %q not allowed by channel media policy", mime)
	}
	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}
	if size > 0 && size > limit {
		return media.Part{}, fmt.Errorf("telegram: file %d bytes exceeds limit %d", size, limit)
	}

	b, err := d.tg()
	if err != nil {
		return media.Part{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return media.Part{}, err
	}
	if f.FilePath == "" {
		return media.Part{}, fmt.Errorf("getFile: no file_path for %s", fileID)
	}
	// The link is {apiBase}/file/bot{token}/{file_path}, so this follows the
	// same server override getFile did.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.FileDownloadLink(f), nil)
	if err != nil {
		return media.Part{}, err
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return media.Part{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Part{}, fmt.Errorf("file download: %d", resp.StatusCode)
	}

	ref, err := d.store.Put(resp.Body, mime, d.pol)
	if err != nil {
		return media.Part{}, err
	}
	metrics.Add("agentflow_media_bytes", ref.Size)
	return media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle, Name: name}, nil
}

// Deliver implements gateway.Driver: sendMessage to the chat, uploading any
// attachments via the matching send* method (multipart). The first image
// attachment's caption carries the text; text alongside non-image attachments
// is sent as a separate sendMessage first.
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	if len(attachments) == 0 {
		return d.sendText(replyTo, text)
	}
	// Text with non-leading image attachments: send it as its own message
	// first so nothing is silently lost (only sendPhoto takes a caption).
	if text != "" && attachments[0].Type != partImage {
		if err := d.sendText(replyTo, text); err != nil {
			return err
		}
		text = ""
	}
	for i, att := range attachments {
		caption := ""
		if i == 0 && text != "" && att.Type == partImage {
			caption = text
			if len(caption) > photoCaptionLimit {
				caption = caption[:photoCaptionLimit]
			}
		}
		if err := d.uploadMedia(replyTo, att, caption); err != nil {
			return err
		}
	}
	return nil
}

// partImage is the media class that maps to sendPhoto, the one method that
// carries a caption.
const partImage = "image"

func (d *Driver) sendText(replyTo, text string) error {
	return d.call("sendMessage", callTimeout, func(ctx context.Context) error {
		b, err := d.tg()
		if err != nil {
			return err
		}
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: replyTo, Text: text})
		return err
	})
}

// uploadMedia POSTs one attachment as multipart/form-data, through the send*
// method that carries that class of part. Bytes come from the blob store
// (handle) or inline base64 (data); URL-only parts are an error — resolve or
// download them first.
//
// There is no file-field name to carry around any more: an SDK params struct
// *is* one method and names its own file field, so sendMethodFor picks the
// method and the switch below picks the struct that matches it.
func (d *Driver) uploadMedia(replyTo string, att media.Part, caption string) error {
	method := sendMethodFor(att.Type)
	var payload []byte
	switch {
	case att.Handle != "" && d.store != nil:
		b, err := d.store.ReadAll(att.Handle, 50<<20)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		payload = b
	case att.Data != "":
		b, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			return fmt.Errorf("%s: bad base64: %w", method, err)
		}
		payload = b
	default:
		return fmt.Errorf("%s: attachment has no resolvable source (need handle or data)", method)
	}

	name := att.Name
	if name == "" {
		name = "file"
		if att.MIME != "" {
			name += mimeExtension(att.MIME)
		}
	}
	return d.call(method, uploadTimeout, func(ctx context.Context) error {
		b, err := d.tg()
		if err != nil {
			return err
		}
		upload := &models.InputFileUpload{Filename: name, Data: bytes.NewReader(payload)}
		switch method {
		case "sendPhoto":
			_, err = b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: replyTo, Photo: upload, Caption: caption})
		case "sendAudio":
			_, err = b.SendAudio(ctx, &bot.SendAudioParams{ChatID: replyTo, Audio: upload, Caption: caption})
		case "sendVideo":
			_, err = b.SendVideo(ctx, &bot.SendVideoParams{ChatID: replyTo, Video: upload, Caption: caption})
		default:
			_, err = b.SendDocument(ctx, &bot.SendDocumentParams{ChatID: replyTo, Document: upload, Caption: caption})
		}
		return err
	})
}

// sendMethodFor names the Bot API method a media class is sent with. The call
// is dispatched on the returned name; this remains the one place the mapping is
// written down, and the caption rule in Deliver turns on it.
func sendMethodFor(partType string) string {
	switch partType {
	case partImage:
		return "sendPhoto"
	case "audio":
		return "sendAudio"
	case "video":
		return "sendVideo"
	default:
		return "sendDocument"
	}
}

// mimeExtension maps common MIME types to a sane fallback filename extension.
func mimeExtension(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}
