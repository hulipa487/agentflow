package feishu

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

	"agentflow/internal/core/media"
)

const (
	maxTextRunes  = 4000
	markdownPause = 10 * time.Minute
)

func parseTarget(replyTo string) (receiveType, receiveID string, err error) {
	if k, v, ok := strings.Cut(replyTo, ":"); ok && v != "" {
		switch k {
		case "chat", "chat_id":
			return "chat_id", v, nil
		case "open_id", "user", "p2p":
			return "open_id", v, nil
		case "user_id":
			return "user_id", v, nil
		case "union_id":
			return "union_id", v, nil
		case "email":
			return "email", v, nil
		}
		return "", "", fmt.Errorf("feishu: unknown reply target prefix %q", k)
	}
	switch {
	case strings.HasPrefix(replyTo, "oc_"):
		return "chat_id", replyTo, nil
	case strings.HasPrefix(replyTo, "ou_"):
		return "open_id", replyTo, nil
	case strings.HasPrefix(replyTo, "on_"):
		return "union_id", replyTo, nil
	case replyTo != "":
		return "chat_id", replyTo, nil
	default:
		return "", "", fmt.Errorf("feishu: empty reply target")
	}
}

// Deliver implements gateway.Driver. Text is split to stay within platform
// limits; markdown replies send interactive markdown cards with plain-text fallback.
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	idType, id, err := parseTarget(replyTo)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		if d.useMarkdown() {
			if err := d.deliverMarkdown(idType, id, text); err != nil {
				return err
			}
		} else {
			for _, chunk := range splitText(text, maxTextRunes) {
				if err := d.sendMessage(idType, id, "text", map[string]string{"text": chunk}); err != nil {
					return err
				}
			}
		}
	}
	for _, att := range attachments {
		if err := d.sendMedia(idType, id, att); err != nil {
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

func (d *Driver) deliverMarkdown(idType, id, text string) error {
	for _, chunk := range splitMarkdown(text, maxTextRunes) {
		sendPlain := func() error {
			for _, plainChunk := range splitText(chunk, maxTextRunes) {
				if err := d.sendMessage(idType, id, "text", map[string]string{"text": plainChunk}); err != nil {
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
		card := map[string]any{
			"config": map[string]bool{"wide_screen_mode": true},
			"elements": []map[string]string{
				{"tag": "markdown", "content": chunk},
			},
		}
		err := d.sendMessage(idType, id, "interactive", card)
		if err == nil {
			continue
		}
		if _, ok := isAPIError(err); !ok {
			return err
		}
		d.mdMu.Lock()
		d.mdOffUntil = time.Now().Add(markdownPause)
		d.mdMu.Unlock()
		d.log.Warn("feishu refused interactive markdown card; falling back to plain text", "err", err)
		if err := sendPlain(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) sendMessage(idType, id, msgType string, content any) error {
	encoded, err := json.Marshal(content)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	path := "/open-apis/im/v1/messages?receive_id_type=" + url.QueryEscape(idType)
	return d.call(ctx, http.MethodPost, path, map[string]any{
		"receive_id": id,
		"msg_type":   msgType,
		"content":    string(encoded),
	}, nil)
}

func (d *Driver) sendMedia(idType, id string, att media.Part) error {
	partType := att.Type
	if partType == "" && att.MIME != "" {
		partType = media.Classify(att.MIME)
	}
	raw, filename, err := d.materializeBytes(att, partType)
	if err != nil {
		return err
	}
	if partType == "image" {
		key, err := d.uploadMultipart("/open-apis/im/v1/images", map[string]string{"image_type": "message"}, "image", filename, raw)
		if err != nil {
			return err
		}
		return d.sendMessage(idType, id, "image", map[string]string{"image_key": key})
	}
	key, err := d.uploadMultipart("/open-apis/im/v1/files", map[string]string{
		"file_type": "stream",
		"file_name": filename,
	}, "file", filename, raw)
	if err != nil {
		return err
	}
	return d.sendMessage(idType, id, "file", map[string]string{"file_key": key})
}

func (d *Driver) materializeBytes(att media.Part, partType string) ([]byte, string, error) {
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
			spec.AcceptMIME = []string{"image/jpeg", "image/png", "image/webp", "image/gif"}
			spec.PreferMIME = "image/jpeg"
			spec.MaxBytes = 10 << 20
		}
		ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
		defer cancel()
		blob, err := d.media.MaterializeBytes(ctx, media.ScopeMaintenance, att, spec)
		if err != nil {
			return nil, "", fmt.Errorf("feishu: media %q: %w", name, err)
		}
		if blob.Name != "" {
			name = blob.Name
		}
		return blob.Bytes, name, nil
	}
	switch {
	case att.Handle != "" && d.store != nil:
		b, err := d.store.ReadAll(att.Handle, maxOutboundMediaBytes)
		if err != nil {
			return nil, "", fmt.Errorf("feishu: read handle %q: %w", att.Handle, err)
		}
		return b, name, nil
	case att.Data != "":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(att.Data))
		if err != nil {
			return nil, "", fmt.Errorf("feishu: decode base64: %w", err)
		}
		return b, name, nil
	default:
		return nil, "", fmt.Errorf("feishu: attachment %q has no resolvable content (need handle or data)", name)
	}
}

func (d *Driver) uploadMultipart(path string, fields map[string]string, fileField, filename string, payload []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return "", err
		}
	}
	fw, err := mw.CreateFormFile(fileField, filename)
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
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiBase+path, bytes.NewReader(rawBody))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", ct)
		resp, err := d.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("feishu upload %s: %w", path, err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				ImageKey string `json:"image_key"`
				FileKey  string `json:"file_key"`
			} `json:"data"`
		}
		_ = json.Unmarshal(data, &out)
		if (resp.StatusCode == http.StatusUnauthorized || isTokenExpiredCode(out.Code)) && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 || out.Code != 0 {
			return "", &apiError{Status: resp.StatusCode, Code: out.Code, Msg: out.Msg}
		}
		if out.Data.ImageKey != "" {
			return out.Data.ImageKey, nil
		}
		if out.Data.FileKey != "" {
			return out.Data.FileKey, nil
		}
		return "", fmt.Errorf("feishu upload %s returned no media key", path)
	}
}

func splitMarkdown(text string, n int) []string {
	chunks := splitText(text, n-16)
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

func splitText(text string, n int) []string {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	r := []rune(text)
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut == n {
			for i := n; i > n/2; i-- {
				if r[i-1] == ' ' {
					cut = i
					break
				}
			}
		}
		out = append(out, strings.TrimRight(string(r[:cut]), "\n "))
		r = r[cut:]
	}
	if s := strings.TrimSpace(string(r)); s != "" {
		out = append(out, string(r))
	}
	return out
}
