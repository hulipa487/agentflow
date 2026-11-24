package qqbot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/core/media"
)

// replyRef is the latest inbound message of one conversation, which replies
// can reference to be passive.
type replyRef struct {
	msgID string
	at    time.Time
	used  int
}

// replyWindow and replyLimit are the platform's passive-reply rules per scene
// (https://bot.q.qq.com/wiki/develop/api-v2/server-inter/message/overview.html):
// a reply must follow the message within the window, and one message takes at
// most replyLimit replies (0 = no limit published). A reply the platform still
// refuses as expired is retried as a proactive message.
var (
	replyWindow = map[string]time.Duration{
		SceneC2C:   60 * time.Minute,
		SceneGroup: 5 * time.Minute,
		SceneGuild: 5 * time.Minute,
		SceneDM:    5 * time.Minute,
	}
	replyLimit = map[string]int{
		SceneC2C:   4,
		SceneGroup: 5,
	}
)

// Error codes that mean a passive reply is no longer possible: the msg_id
// expired, its reply count is spent, or it was not accepted.
var passiveExpired = []int{304103, 40034005, 40034024, 40034128}

// maxTextRunes is the length a text reply is split at.
const maxTextRunes = 1800

func (d *Driver) rememberRef(target, msgID string) {
	d.refsMu.Lock()
	d.refs[target] = &replyRef{msgID: msgID, at: time.Now()}
	d.refsMu.Unlock()
}

// claimRef returns the msg_id and msg_seq the next reply to target can use,
// or ok=false when it has to be proactive.
func (d *Driver) claimRef(scene, target string) (string, int, bool) {
	d.refsMu.Lock()
	defer d.refsMu.Unlock()
	r := d.refs[target]
	if r == nil {
		return "", 0, false
	}
	if time.Since(r.at) > replyWindow[scene] {
		delete(d.refs, target)
		return "", 0, false
	}
	if limit := replyLimit[scene]; limit > 0 && r.used >= limit {
		return "", 0, false
	}
	r.used++
	return r.msgID, r.used, true
}

func (d *Driver) dropRef(target string) {
	d.refsMu.Lock()
	delete(d.refs, target)
	d.refsMu.Unlock()
}

// parseTarget splits a reply target "<scene>:<id>".
func parseTarget(replyTo string) (scene, id string, err error) {
	scene, id, ok := strings.Cut(replyTo, ":")
	if !ok || id == "" {
		return "", "", fmt.Errorf("qq: reply target %q is not <scene>:<id>", replyTo)
	}
	switch scene {
	case SceneC2C, SceneGroup, SceneGuild, SceneDM:
		return scene, id, nil
	}
	return "", "", fmt.Errorf("qq: reply target %q names an unknown scene (want c2c, group, guild or dm)", replyTo)
}

func messagesPath(scene, id string) string {
	e := url.PathEscape(id)
	switch scene {
	case SceneC2C:
		return "/v2/users/" + e + "/messages"
	case SceneGroup:
		return "/v2/groups/" + e + "/messages"
	case SceneGuild:
		return "/channels/" + e + "/messages"
	default:
		return "/dms/" + e + "/messages"
	}
}

func filesPath(scene, id string) string {
	e := url.PathEscape(id)
	if scene == SceneC2C {
		return "/v2/users/" + e + "/files"
	}
	return "/v2/groups/" + e + "/files"
}

// Deliver implements gateway.Driver. Text is sent as plain text, split into
// chunks the platform accepts; each attachment is its own message. QQ fetches
// media by URL: an attachment with an http(s) URL is sent directly, while a
// blob-store handle or inline base64 payload is resolved via a presigned GET
// URL when the media store implements media.Presigner (e.g. media.backend: s3).
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	scene, id, err := parseTarget(replyTo)
	if err != nil {
		return err
	}
	if d.useMarkdown(scene) {
		if err := d.deliverMarkdown(scene, id, replyTo, text); err != nil {
			return err
		}
	} else {
		for _, chunk := range splitText(text, maxTextRunes) {
			if err := d.send(scene, id, replyTo, map[string]any{"content": chunk, "msg_type": 0}); err != nil {
				return err
			}
		}
	}
	for _, att := range attachments {
		if err := d.sendMedia(scene, id, replyTo, att); err != nil {
			return err
		}
	}
	return nil
}

// maxMarkdownRunes is the longest markdown message the driver tries to send
// in one piece. The platform's real limit is not documented, so the driver
// starts here and learns it: see deliverMarkdown.
const maxMarkdownRunes = 12000

// deliverMarkdown sends text as markdown in as few messages as the platform
// takes. A long reply (a block of code, say) reads badly cut into pieces, so
// a piece longer than maxTextRunes is tried whole first. When the platform
// refuses it, the size the driver tries from then on is halved and the piece
// is cut again; at maxTextRunes it is the ordinary path, with its plain-text
// fallback.
func (d *Driver) deliverMarkdown(scene, id, target, text string) error {
	d.mdMu.Lock()
	limit := d.mdLimit
	d.mdMu.Unlock()
	if limit < maxTextRunes {
		limit = maxMarkdownRunes
	}
	for _, chunk := range splitMarkdown(text, limit) {
		n := utf8.RuneCountInString(chunk)
		if n <= maxTextRunes {
			if err := d.sendMarkdown(scene, id, target, chunk); err != nil {
				return err
			}
			continue
		}
		err := d.send(scene, id, target, map[string]any{"msg_type": 2, "markdown": map[string]string{"content": chunk}})
		var refused *apiError
		if err == nil {
			continue
		}
		if !errors.As(err, &refused) {
			return err
		}
		smaller := max(n/2, maxTextRunes)
		d.mdMu.Lock()
		d.mdLimit = smaller
		d.mdMu.Unlock()
		d.log.Info("qq refused a long markdown message; cutting it smaller",
			"runes", n, "next_limit", smaller, "code", refused.code(), "message", refused.Message)
		if err := d.deliverMarkdown(scene, id, target, chunk); err != nil {
			return err
		}
	}
	return nil
}

// markdownPause is how long markdown stays off after the platform refused it.
const markdownPause = 10 * time.Minute

// useMarkdown reports whether text to scene goes out as markdown: switched on,
// a private chat or group (where the platform documents markdown), and not
// paused after a refusal.
func (d *Driver) useMarkdown(scene string) bool {
	if !d.markdown || (scene != SceneC2C && scene != SceneGroup) {
		return false
	}
	d.mdMu.Lock()
	defer d.mdMu.Unlock()
	return time.Now().After(d.mdOffUntil)
}

// sendMarkdown sends one markdown message. When the platform refuses it, the
// same text goes again as plain text and markdown pauses, so nothing is lost
// while the bot lacks the permission.
func (d *Driver) sendMarkdown(scene, id, target, text string) error {
	err := d.send(scene, id, target, map[string]any{"msg_type": 2, "markdown": map[string]string{"content": text}})
	var refused *apiError
	if err == nil || !errors.As(err, &refused) {
		return err
	}
	d.mdMu.Lock()
	d.mdOffUntil = time.Now().Add(markdownPause)
	d.mdMu.Unlock()
	d.log.Warn("qq refused a markdown message; sending plain text and pausing markdown",
		"code", refused.code(), "message", refused.Message, "pause", markdownPause)
	return d.send(scene, id, target, map[string]any{"content": text, "msg_type": 0})
}

// splitMarkdown splits like splitText, and keeps a fenced code block intact
// across a cut: the chunk that ends inside a fence closes it, and the next
// chunk reopens it with the same fence line.
func splitMarkdown(text string, n int) []string {
	// Leave room for the closing and reopening fence lines.
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

type sendResp struct {
	ID      string `json:"id"`
	ExtInfo struct {
		RefIdx string `json:"ref_idx"`
	} `json:"ext_info"`
}

func bodyText(body map[string]any) string {
	if s, _ := body["content"].(string); s != "" {
		return strings.TrimSpace(s)
	}
	if md, _ := body["markdown"].(map[string]string); md != nil {
		if s := md["content"]; s != "" {
			return strings.TrimSpace(s)
		}
	}
	if md, _ := body["markdown"].(map[string]any); md != nil {
		if s, _ := md["content"].(string); s != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (d *Driver) recordSent(out sendResp, body map[string]any) {
	if out.ID == "" && out.ExtInfo.RefIdx == "" {
		return
	}
	d.gwMu.Lock()
	botName := d.botName
	d.gwMu.Unlock()
	d.rememberMsg([]string{out.ID, out.ExtInfo.RefIdx}, indexedMsg{
		author: botName,
		text:   bodyText(body),
		isBot:  true,
		at:     time.Now(),
	})
}

// send posts one message, passive when the conversation allows it. A passive
// reply refused as expired goes again as a proactive one.
func (d *Driver) send(scene, id, target string, body map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var out sendResp
	if msgID, seq, ok := d.claimRef(scene, target); ok {
		body["msg_id"] = msgID
		if scene == SceneC2C || scene == SceneGroup {
			body["msg_seq"] = seq
		}
		err := d.call(ctx, "POST", messagesPath(scene, id), body, &out)
		if err == nil {
			d.recordSent(out, body)
			return nil
		}
		if !isCode(err, passiveExpired...) {
			return err
		}
		d.log.Info("qq passive reply window closed; sending proactively", "scene", scene, "err", err)
		d.dropRef(target)
		delete(body, "msg_id")
		delete(body, "msg_seq")
		out = sendResp{}
	}
	err := d.call(ctx, "POST", messagesPath(scene, id), body, &out)
	if err == nil {
		d.recordSent(out, body)
	}
	return err
}

// sendMedia sends one attachment by URL: uploaded as rich media for private
// chats and groups, as an image link for guild channels and DMs. Blob-store
// handles and inline base64 attachments are resolved to time-limited presigned
// GET URLs when the configured media store implements media.Presigner (S3).
func (d *Driver) sendMedia(scene, id, target string, att media.Part) error {
	partType := att.Type
	if partType == "" && att.MIME != "" {
		partType = media.Classify(att.MIME)
	}
	if scene == SceneGuild || scene == SceneDM {
		if partType != "image" {
			return fmt.Errorf("qq: guild channels take images only, not %s", partType)
		}
	}
	link, err := d.resolveMediaURL(att, partType)
	if err != nil {
		return err
	}
	if scene == SceneGuild || scene == SceneDM {
		return d.send(scene, id, target, map[string]any{"image": link})
	}
	fileType := 4 // file
	switch partType {
	case "image":
		fileType = 1
	case "video":
		fileType = 2
	case "audio":
		fileType = 3 // the platform plays silk; other audio may show as a file
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	var up struct {
		FileInfo string `json:"file_info"`
	}
	err = d.call(ctx, "POST", filesPath(scene, id), map[string]any{
		"file_type":    fileType,
		"url":          link,
		"srv_send_msg": false,
	}, &up)
	if err != nil {
		return fmt.Errorf("qq: media upload: %w", err)
	}
	if up.FileInfo == "" {
		return fmt.Errorf("qq: media upload returned no file_info")
	}
	return d.send(scene, id, target, map[string]any{
		"msg_type": 7,
		"content":  " ",
		"media":    map[string]string{"file_info": up.FileInfo},
	})
}

// resolveMediaURL turns an attachment into an http(s) URL QQ's servers can
// fetch. When d.media is wired, it uses the centralized media.Service to
// transcode images to QQ-compatible JPEG/PNG, fit within size limits, cache
// derived variants, and mint an S3 presigned or FS HMAC-signed URL.
func (d *Driver) resolveMediaURL(att media.Part, partType string) (string, error) {
	if d.media != nil {
		spec := qqTargetSpec(partType, d.presignTTL)
		ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
		defer cancel()
		return d.media.MaterializeURL(ctx, media.ScopeMaintenance, att, spec)
	}
	if strings.HasPrefix(att.URL, "https://") || strings.HasPrefix(att.URL, "http://") {
		return att.URL, nil
	}
	name := att.Name
	if name == "" {
		name = partType
	}
	if att.Handle == "" && att.Data == "" {
		return "", fmt.Errorf("qq: attachment %q has no public URL, handle, or data", name)
	}
	presigner, ok := d.store.(media.Presigner)
	if !ok {
		return "", fmt.Errorf("qq: attachment %q has no public URL and the configured media store cannot presign URLs; set url or configure media.backend: s3", name)
	}
	handle := att.Handle
	if handle == "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(att.Data))
		if err != nil {
			return "", fmt.Errorf("qq: attachment %q bad base64: %w", name, err)
		}
		ref, err := d.store.Put(bytes.NewReader(raw), att.MIME, media.Policy{MaxBytes: maxOutboundMediaBytes})
		if err != nil {
			return "", fmt.Errorf("qq: stage attachment %q for presign: %w", name, err)
		}
		handle = ref.Handle
	}
	opts := media.PresignOptions{ContentType: att.MIME}
	if partType == "file" && att.Name != "" {
		opts.Filename = att.Name
	}
	link, err := presigner.PresignGet(handle, d.presignTTL, opts)
	if err != nil {
		return "", fmt.Errorf("qq: presign attachment %q: %w", name, err)
	}
	return link, nil
}

// qqTargetSpec declares QQ's media format requirements: images prefer JPEG
// (accepting JPEG/PNG up to 10 MiB, auto-transcoding WebP/GIF/oversized PNG);
// files carry an RFC 6266 attachment filename.
func qqTargetSpec(partType string, ttl time.Duration) media.TargetSpec {
	spec := media.TargetSpec{
		MaxBytes:   maxOutboundMediaBytes,
		PresignTTL: ttl,
	}
	switch partType {
	case "image":
		spec.AcceptMIME = []string{"image/jpeg", "image/png"}
		spec.PreferMIME = "image/jpeg"
		spec.MaxBytes = 10 << 20
	case "file":
		spec.AttachFilename = true
	}
	return spec
}

// splitText cuts text into chunks of at most n runes, preferring line breaks
// and then spaces. Empty text is no chunks.
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
