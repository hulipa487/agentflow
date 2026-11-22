package wecom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/core/media"
)

const (
	// maxMessageBytes is WeCom's documented 2048-byte UTF-8 ceiling per message.
	maxMessageBytes = 2048
	markdownPause   = 10 * time.Minute
)

type targetSpec struct {
	endpoint string
	field    string
	id       string
	isChat   bool
}

func parseTarget(replyTo string) (targetSpec, error) {
	if replyTo == "" {
		return targetSpec{}, fmt.Errorf("wecom: empty reply target")
	}
	if prefix, id, ok := strings.Cut(replyTo, ":"); ok && id != "" {
		switch prefix {
		case "user":
			return targetSpec{endpoint: "/cgi-bin/message/send", field: "touser", id: id}, nil
		case "party":
			return targetSpec{endpoint: "/cgi-bin/message/send", field: "toparty", id: id}, nil
		case "tag":
			return targetSpec{endpoint: "/cgi-bin/message/send", field: "totag", id: id}, nil
		case "chat":
			return targetSpec{endpoint: "/cgi-bin/appchat/send", field: "chatid", id: id, isChat: true}, nil
		default:
			return targetSpec{}, fmt.Errorf("wecom: unknown reply target prefix %q", prefix)
		}
	}
	return targetSpec{endpoint: "/cgi-bin/message/send", field: "touser", id: replyTo}, nil
}

// Deliver implements gateway.Driver. Text/markdown is chunked at 2048 UTF-8
// bytes without splitting multi-byte runes; media is uploaded via /cgi-bin/media/upload.
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	spec, err := parseTarget(replyTo)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		if d.useMarkdown() {
			if err := d.deliverMarkdown(spec, text); err != nil {
				return err
			}
		} else {
			for _, chunk := range splitTextBytes(text, maxMessageBytes) {
				if err := d.sendTextChunk(spec, "text", chunk); err != nil {
					return err
				}
			}
		}
	}
	for _, att := range attachments {
		if err := d.sendMedia(spec, att); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) useMarkdown() bool {
	if !d.markdown {
		return false
	}
	d.mdMu.Lock()
	defer d.mdMu.Unlock()
	return time.Now().After(d.mdOffUntil)
}

func (d *Driver) deliverMarkdown(spec targetSpec, text string) error {
	for _, chunk := range splitMarkdownBytes(text, maxMessageBytes) {
		sendPlain := func() error {
			for _, plainChunk := range splitTextBytes(chunk, maxMessageBytes) {
				if err := d.sendTextChunk(spec, "text", plainChunk); err != nil {
					return err
				}
			}
			return nil
		}
		if !d.useMarkdown() {
			if err := sendPlain(); err != nil {
				return err
			}
			continue
		}
		err := d.sendTextChunk(spec, "markdown", chunk)
		if err == nil {
			continue
		}
		if _, ok := isAPIError(err); !ok {
			return err
		}
		d.mdMu.Lock()
		d.mdOffUntil = time.Now().Add(markdownPause)
		d.mdMu.Unlock()
		d.log.Warn("wecom refused markdown message; falling back to plain text", "err", err)
		if err := sendPlain(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) sendTextChunk(spec targetSpec, msgType, chunk string) error {
	body := map[string]any{
		spec.field: spec.id,
		"msgtype":  msgType,
		msgType:    map[string]string{"content": chunk},
	}
	if !spec.isChat {
		body["agentid"] = d.agentID
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return d.callJSON(ctx, http.MethodPost, spec.endpoint, body, nil)
}

func (d *Driver) sendMedia(spec targetSpec, att media.Part) error {
	partType := att.Type
	if partType == "" && att.MIME != "" {
		partType = media.Classify(att.MIME)
	}
	raw, filename, resolvedMIME, err := d.materializeBytes(att, partType)
	if err != nil {
		return err
	}
	wecomType := "file"
	switch partType {
	case "image":
		wecomType = "image"
	case "video":
		wecomType = "video"
	case "audio":
		if strings.Contains(strings.ToLower(resolvedMIME), "amr") || strings.HasSuffix(strings.ToLower(filename), ".amr") {
			wecomType = "voice"
		}
	}
	mediaID, err := d.uploadMedia(wecomType, filename, raw)
	if err != nil {
		return err
	}
	body := map[string]any{
		spec.field: spec.id,
		"msgtype":  wecomType,
		wecomType:  map[string]string{"media_id": mediaID},
	}
	if !spec.isChat {
		body["agentid"] = d.agentID
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return d.callJSON(ctx, http.MethodPost, spec.endpoint, body, nil)
}

func (d *Driver) materializeBytes(att media.Part, partType string) ([]byte, string, string, error) {
	name := att.Name
	if name == "" {
		if partType == "image" {
			name = "image.jpg"
		} else {
			name = "attachment.bin"
		}
	}
	if d.media != nil {
		spec := media.TargetSpec{MaxBytes: maxOutboundMediaBytes, AllowURLFetch: true}
		if partType == "image" {
			spec.AcceptMIME = []string{"image/jpeg", "image/png"}
			spec.PreferMIME = "image/jpeg"
			spec.MaxBytes = 10 << 20
		}
		ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
		defer cancel()
		blob, err := d.media.MaterializeBytes(ctx, media.ScopeMaintenance, att, spec)
		if err != nil {
			return nil, "", "", fmt.Errorf("wecom: media %q: %w", name, err)
		}
		if blob.Name != "" {
			name = blob.Name
		}
		return blob.Bytes, name, blob.MIME, nil
	}
	switch {
	case att.Handle != "" && d.store != nil:
		b, err := d.store.ReadAll(att.Handle, maxOutboundMediaBytes)
		if err != nil {
			return nil, "", "", fmt.Errorf("wecom: read handle %q: %w", att.Handle, err)
		}
		return b, name, att.MIME, nil
	case att.Data != "":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(att.Data))
		if err != nil {
			return nil, "", "", fmt.Errorf("wecom: decode base64: %w", err)
		}
		return b, name, att.MIME, nil
	default:
		return nil, "", "", fmt.Errorf("wecom: attachment %q has no resolvable content (need handle or data)", name)
	}
}

func (d *Driver) uploadMedia(wecomType, filename string, payload []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("media", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(payload); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	rawBody := body.Bytes()
	ct := mw.FormDataContentType()

	for attempt := 0; ; attempt++ {
		tok, err := d.tokens.Token(ctx)
		if err != nil {
			return "", err
		}
		endpoint := fmt.Sprintf("%s/cgi-bin/media/upload?access_token=%s&type=%s",
			d.apiBase, url.QueryEscape(tok), url.QueryEscape(wecomType))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", ct)
		resp, err := d.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("wecom media upload: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var out struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
			MediaID string `json:"media_id"`
		}
		_ = json.Unmarshal(data, &out)
		if (resp.StatusCode == http.StatusUnauthorized || isTokenExpiredErr(out.ErrCode)) && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 || out.ErrCode != 0 {
			return "", &apiError{Status: resp.StatusCode, ErrCode: out.ErrCode, ErrMsg: out.ErrMsg}
		}
		if out.MediaID == "" {
			return "", errorsNew("wecom media upload returned no media_id")
		}
		return out.MediaID, nil
	}
}

func errorsNew(msg string) error { return fmt.Errorf("%s", msg) }

// splitTextBytes cuts text into chunks whose UTF-8 byte length is at most
// maxBytes, never splitting inside a UTF-8 rune and preferring newline/space breaks.
func splitTextBytes(text string, maxBytes int) []string {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if maxBytes <= 4 {
		maxBytes = 4
	}
	var out []string
	rem := text
	for len(rem) > maxBytes {
		// Walk backward from maxBytes to a valid rune start.
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(rem[cut]) {
			cut--
		}
		if cut == 0 {
			// Single rune exceeded maxBytes? Advance by one full rune so we never loop.
			_, size := utf8.DecodeRuneInString(rem)
			cut = size
		}
		half := cut / 2
		best := cut
		if nl := strings.LastIndexByte(rem[:cut], '\n'); nl >= half {
			best = nl + 1
		} else if sp := strings.LastIndexByte(rem[:cut], ' '); sp >= half {
			best = sp + 1
		}
		if piece := strings.TrimRight(rem[:best], "\n "); piece != "" {
			out = append(out, piece)
		}
		rem = rem[best:]
	}
	if strings.TrimSpace(rem) != "" {
		out = append(out, rem)
	}
	return out
}

func splitMarkdownBytes(text string, maxBytes int) []string {
	headroom := maxBytes - 32
	if headroom < 64 {
		headroom = maxBytes
	}
	chunks := splitTextBytes(text, headroom)
	open := ""
	for i, c := range chunks {
		if open != "" {
			c = open + "\n" + c
		}
		open = ""
		for _, line := range strings.Split(c, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				if open == "" {
					open = t
				} else {
					open = ""
				}
			}
		}
		if open != "" {
			c += "\n" + open[:3]
		}
		chunks[i] = c
	}
	return chunks
}
