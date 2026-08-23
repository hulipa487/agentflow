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
// Access control (allow_users) is enforced in all modes before any Lua runs.
//
// Media (photos, documents, voice, audio, video) is ingested when the
// channel's media policy allows it: the file is downloaded via getFile and
// landed in the blob store; the message carries small part descriptors
// (handles), never the bytes. Replies carrying attachments upload them via
// multipart (sendPhoto/sendAudio/sendVideo/sendDocument).
package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

// Driver is the Telegram channel driver. It implements gateway.Driver for
// replies (sendMessage / sendPhoto / ...) and either polls or serves a
// webhook for inbound.
type Driver struct {
	name      string
	token     string
	agent     string
	allow     map[int64]bool // empty = allow all
	mode      string         // "polling" | "webhook" | "auto"
	path      string         // webhook mode only
	publicURL string         // webhook/auto: external base that routes to this process
	apiBase   string         // override for the Telegram API host (tests); empty = api.telegram.org
	store     *media.Store   // nil = media policy disabled
	pol       media.Policy
	sink      router.Sink
	log       *slog.Logger
	client    *http.Client
	seq       int64
}

func New(name, token, agent, mode string, allowUsers []int64, path, publicURL string, sink router.Sink, srv *httpd.Server, store *media.Store, pol media.Policy, log *slog.Logger) *Driver {
	allow := map[int64]bool{}
	for _, id := range allowUsers {
		allow[id] = true
	}
	if mode == "" {
		mode = "polling"
	}
	d := &Driver{
		name:      name,
		token:     token,
		agent:     agent,
		allow:     allow,
		mode:      mode,
		path:      path,
		publicURL: publicURL,
		store:     store,
		pol:       pol,
		sink:      sink,
		log:       log.With("driver", "telegram", "channel", name, "mode", mode),
		client:    &http.Client{Timeout: 40 * time.Second},
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
		go d.poll(ctx)
		return nil
	}
	d.log.Info("auto: webhook registered", "url", hookURL)
	return nil
}

// Stop is a no-op now; the shared httpd.Server owns the listener lifetime and
// polling exits via ctx cancellation. Method retained for the Driver surface.
func (d *Driver) Stop(_ context.Context) {}

func (d *Driver) apiURL(method string) string {
	base := d.apiBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	return base + "/bot" + d.token + "/" + method
}

// setWebhook registers hookURL with Telegram so updates are pushed there.
// An empty URL clears the webhook (see deleteWebhook). drain is false so we
// don't drop in-flight updates on the switch.
func (d *Driver) setWebhook(hookURL string) error {
	body, _ := json.Marshal(map[string]any{
		"url":             hookURL,
		"allowed_updates": []string{"message"},
	})
	return d.postAPI("setWebhook", body)
}

// deleteWebhook clears any registered webhook URL so Telegram stops pushing.
func (d *Driver) deleteWebhook() error {
	body, _ := json.Marshal(map[string]any{"drop_pending_updates": false})
	return d.postAPI("deleteWebhook", body)
}

func (d *Driver) postAPI(method string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %d: %s", method, resp.StatusCode, b)
	}
	return nil
}

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
	Doc   *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MIME     string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"document"`
	Voice *struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
		MIME     string `json:"mime_type"`
	} `json:"voice"`
	Audio *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MIME     string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"audio"`
	Video *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MIME     string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"video"`
}

type photoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type updatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []update `json:"result"`
	Description string   `json:"description"`
}

// --- polling mode ---

func (d *Driver) poll(ctx context.Context) {
	d.log.Info("telegram polling started")
	var offset int64
	for ctx.Err() == nil {
		updates, err := d.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Warn("getUpdates failed", "err", err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			d.handleUpdate(u)
		}
	}
}

func (d *Driver) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	url := d.apiURL("getUpdates") +
		"?timeout=30&offset=" + strconv.FormatInt(offset, 10) +
		`&allowed_updates=["message"]`
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var ur updatesResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return nil, fmt.Errorf("bad getUpdates response: %w", err)
	}
	if !ur.OK {
		return nil, fmt.Errorf("telegram api: %s", ur.Description)
	}
	return ur.Result, nil
}

// --- webhook mode ---

func (d *Driver) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
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
	atts := d.ingestMedia(m)
	if text == "" && len(atts) == 0 {
		return
	}
	if len(d.allow) > 0 && !d.allow[m.From.ID] {
		d.log.Warn("dropping message from non-allowed user", "user_id", m.From.ID)
		return
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	d.seq++
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
// and lands it in the blob store under the channel policy.
func (d *Driver) downloadFile(fileID, mime, name string, size int64) (media.Part, error) {
	if mime == "" {
		mime = "application/octet-stream"
	}
	if !d.pol.Allows(mime) {
		return media.Part{}, fmt.Errorf("telegram: mime %q not allowed by channel media policy", mime)
	}
	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	if size > 0 && size > limit {
		return media.Part{}, fmt.Errorf("telegram: file %d bytes exceeds limit %d", size, limit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// getFile
	furl := d.apiURL("getFile") + "?file_id=" + fileID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, furl, nil)
	if err != nil {
		return media.Part{}, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return media.Part{}, err
	}
	var fr struct {
		OK     bool `json:"ok"`
		Result *struct {
			FileID       string `json:"file_id"`
			FileUniqueID string `json:"file_unique_id"`
			FileSize     int64  `json:"file_size"`
			FilePath     string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&fr)
	resp.Body.Close()
	if err != nil {
		return media.Part{}, err
	}
	if !fr.OK || fr.Result == nil || fr.Result.FilePath == "" {
		return media.Part{}, fmt.Errorf("getFile: %s", fr.Description)
	}

	// file download: {apiBase}/file/bot{token}/{file_path}
	base := d.apiBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	dlURL := base + "/file/bot" + d.token + "/" + fr.Result.FilePath
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return media.Part{}, err
	}
	resp, err = d.client.Do(req)
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
	if text != "" && attachments[0].Type != "image" {
		if err := d.sendText(replyTo, text); err != nil {
			return err
		}
		text = ""
	}
	for i, att := range attachments {
		method, field := sendMethodFor(att.Type)
		caption := ""
		if i == 0 && text != "" && method == "sendPhoto" {
			caption = text
			if len(caption) > 1024 {
				caption = caption[:1024]
			}
		}
		if err := d.uploadMedia(replyTo, method, field, att, caption); err != nil {
			return err
		}
	}
	return nil
}

func sendMethodFor(partType string) (method, fileField string) {
	switch partType {
	case "image":
		return "sendPhoto", "photo"
	case "audio":
		return "sendAudio", "audio"
	case "video":
		return "sendVideo", "video"
	default:
		return "sendDocument", "document"
	}
}

func (d *Driver) sendText(replyTo, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id": replyTo,
		"text":    text,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("sendMessage: %d: %s", resp.StatusCode, b)
	}
	return nil
}

// uploadMedia POSTs one attachment as multipart/form-data. Bytes come from
// the blob store (handle) or inline base64 (data); URL-only parts are an
// error — resolve or download them first.
func (d *Driver) uploadMedia(replyTo, method, field string, att media.Part, caption string) error {
	var payload []byte
	switch {
	case att.Handle != "" && d.store != nil:
		b, err := d.store.ReadAll(att.Handle, 50<<20)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		payload = b
	case att.Data != "":
		b, err := base64Decode(att.Data)
		if err != nil {
			return fmt.Errorf("%s: bad base64: %w", method, err)
		}
		payload = b
	default:
		return fmt.Errorf("%s: attachment has no resolvable source (need handle or data)", method)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", replyTo)
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	name := att.Name
	if name == "" {
		name = "file"
		if att.MIME != "" {
			name += mimeExtension(att.MIME)
		}
	}
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(payload); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL(method), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %d: %s", method, resp.StatusCode, b)
	}
	return nil
}

// base64Decode is std base64; used for inline data parts.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
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
