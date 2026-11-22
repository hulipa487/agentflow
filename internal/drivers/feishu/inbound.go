package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

type eventEnvelope struct {
	Schema string `json:"schema"`
	Header struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Token     string `json:"token"`
		AppID     string `json:"app_id"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

type receiveEvent struct {
	Sender struct {
		SenderID struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id"`
			UnionID string `json:"union_id"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		RootID      string `json:"root_id"`
		ParentID    string `json:"parent_id"`
		ThreadID    string `json:"thread_id"`
		CreateTime  string `json:"create_time"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			Key string `json:"key"`
			ID  struct {
				OpenID  string `json:"open_id"`
				UserID  string `json:"user_id"`
				UnionID string `json:"union_id"`
			} `json:"id"`
			Name string `json:"name"`
		} `json:"mentions"`
	} `json:"message"`
}

func (d *Driver) handleEventJSON(raw []byte) {
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		d.log.Warn("feishu event undecodable", "err", err)
		return
	}
	if env.Header.EventType != "im.message.receive_v1" {
		if env.Header.EventType != "" {
			if _, seen := d.unknown.LoadOrStore(env.Header.EventType, true); !seen {
				d.log.Info("feishu event type not handled", "type", env.Header.EventType)
			}
		}
		return
	}
	var ev receiveEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		d.log.Warn("feishu receive event undecodable", "err", err)
		return
	}
	if ev.Sender.SenderType != "user" {
		return
	}
	msg := ev.Message
	if msg.MessageID == "" || !d.seen.add(msg.MessageID) {
		return
	}

	scene := ""
	switch strings.ToLower(msg.ChatType) {
	case "p2p":
		scene = SceneP2P
	case "group":
		scene = SceneGroup
	default:
		return
	}
	if scene == SceneP2P && !d.scenes[SceneP2P] {
		return
	}
	if scene == SceneGroup && !d.scenes[SceneGroup] && !d.scenes[SceneGroupAll] {
		return
	}

	senderOpenID := ev.Sender.SenderID.OpenID
	if senderOpenID == "" {
		senderOpenID = ev.Sender.SenderID.UserID
	}
	if senderOpenID == "" {
		d.log.Warn("feishu event missing sender id", "message_id", msg.MessageID)
		return
	}

	replyTo := "chat:" + msg.ChatID
	if msg.ChatID == "" {
		replyTo = "open_id:" + senderOpenID
	}

	text, mediaSpecs := extractContent(msg.MessageType, msg.Content)
	mentioned := false
	if scene == SceneGroup {
		botID := d.resolveBotOpenID(context.Background())
		var others bool
		text, mentioned, others = resolveGroupMentions(text, msg.Mentions, botID)
		if !mentioned && !d.scenes[SceneGroupAll] {
			return
		}
		_ = others
	}

	atts := d.ingestResources(msg.MessageID, mediaSpecs)
	if strings.TrimSpace(text) == "" && len(atts) == 0 {
		return
	}

	payload := map[string]any{
		"scene":        scene,
		"chat_id":      replyTo,
		"raw_chat_id":  msg.ChatID,
		"chat_type":    msg.ChatType,
		"message_id":   msg.MessageID,
		"message_type": msg.MessageType,
		"open_id":      senderOpenID,
	}
	if env.Header.EventID != "" {
		payload["event_id"] = env.Header.EventID
	}
	if ev.Sender.SenderID.UserID != "" {
		payload["user_id"] = ev.Sender.SenderID.UserID
	}
	if ev.Sender.SenderID.UnionID != "" {
		payload["union_id"] = ev.Sender.SenderID.UnionID
	}
	if msg.ThreadID != "" {
		payload["thread_id"] = msg.ThreadID
	}
	if scene == SceneGroup {
		payload["mentioned"] = mentioned
	}

	d.log.Debug("feishu message received", "scene", scene, "message_id", msg.MessageID, "text_len", len(text), "media", len(atts))
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          "feishu-" + msg.MessageID,
			Type:        "user",
			From:        "user:feishu:" + senderOpenID,
			Text:        strings.TrimSpace(text),
			Channel:     d.name,
			ReplyTo:     replyTo,
			Attachments: atts,
			Ts:          parseMillis(msg.CreateTime),
			Payload:     payload,
		},
	})
}

func resolveGroupMentions(text string, mentions []struct {
	Key string `json:"key"`
	ID  struct {
		OpenID  string `json:"open_id"`
		UserID  string `json:"user_id"`
		UnionID string `json:"union_id"`
	} `json:"id"`
	Name string `json:"name"`
}, botOpenID string) (out string, mentionedBot, mentionedOthers bool) {
	for _, m := range mentions {
		isBot := botOpenID != "" && m.ID.OpenID == botOpenID
		if isBot {
			mentionedBot = true
			if m.Key != "" {
				text = strings.ReplaceAll(text, m.Key, "")
			}
			continue
		}
		mentionedOthers = true
		if m.Key != "" && m.Name != "" {
			text = strings.ReplaceAll(text, m.Key, "@"+m.Name)
		}
	}
	return strings.TrimSpace(text), mentionedBot, mentionedOthers
}

type resourceSpec struct {
	key      string
	resType  string // "image" | "file"
	filename string
	hintMIME string
}

func extractContent(msgType, rawContent string) (string, []resourceSpec) {
	if rawContent == "" {
		return "", nil
	}
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(rawContent), &c) == nil {
			return c.Text, nil
		}
	case "post":
		var p struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag      string `json:"tag"`
				Text     string `json:"text"`
				Href     string `json:"href"`
				UserName string `json:"user_name"`
				ImageKey string `json:"image_key"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(rawContent), &p) == nil {
			var lines []string
			if p.Title != "" {
				lines = append(lines, p.Title)
			}
			var specs []resourceSpec
			for _, para := range p.Content {
				var sb strings.Builder
				for _, el := range para {
					switch el.Tag {
					case "text":
						sb.WriteString(el.Text)
					case "a":
						if el.Href != "" {
							sb.WriteString(el.Text + " (" + el.Href + ")")
						} else {
							sb.WriteString(el.Text)
						}
					case "at":
						if el.UserName != "" {
							sb.WriteString("@" + el.UserName)
						}
					case "img":
						if el.ImageKey != "" {
							specs = append(specs, resourceSpec{key: el.ImageKey, resType: "image", filename: el.ImageKey + ".png", hintMIME: "image/png"})
						}
					}
				}
				if s := strings.TrimSpace(sb.String()); s != "" {
					lines = append(lines, s)
				}
			}
			return strings.Join(lines, "\n"), specs
		}
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if json.Unmarshal([]byte(rawContent), &c) == nil && c.ImageKey != "" {
			return "", []resourceSpec{{key: c.ImageKey, resType: "image", filename: c.ImageKey + ".png", hintMIME: "image/png"}}
		}
	case "file":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if json.Unmarshal([]byte(rawContent), &c) == nil && c.FileKey != "" {
			return "", []resourceSpec{{key: c.FileKey, resType: "file", filename: c.FileName}}
		}
	case "audio":
		var c struct {
			FileKey string `json:"file_key"`
		}
		if json.Unmarshal([]byte(rawContent), &c) == nil && c.FileKey != "" {
			return "", []resourceSpec{{key: c.FileKey, resType: "file", filename: c.FileKey + ".opus", hintMIME: "audio/opus"}}
		}
	case "media":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if json.Unmarshal([]byte(rawContent), &c) == nil && c.FileKey != "" {
			name := c.FileName
			if name == "" {
				name = c.FileKey + ".mp4"
			}
			return "", []resourceSpec{{key: c.FileKey, resType: "file", filename: name, hintMIME: "video/mp4"}}
		}
	}
	return "", nil
}

func (d *Driver) ingestResources(messageID string, specs []resourceSpec) []media.Part {
	if d.store == nil || !d.pol.Enabled() || len(specs) == 0 {
		return nil
	}
	var out []media.Part
	for _, s := range specs {
		p, err := d.downloadResource(messageID, s)
		if err != nil {
			d.log.Warn("feishu media ingest failed", "key", s.key, "err", err)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		out = append(out, p)
	}
	if len(out) > 0 {
		metrics.Inc("agentflow_media_ingested")
	}
	return out
}

func (d *Driver) downloadResource(messageID string, spec resourceSpec) (media.Part, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	path := fmt.Sprintf("/open-apis/im/v1/messages/%s/resources/%s?type=%s",
		url.PathEscape(messageID), url.PathEscape(spec.key), url.QueryEscape(spec.resType))

	var resp *http.Response
	for attempt := 0; ; attempt++ {
		tok, err := d.tokens.Token(ctx)
		if err != nil {
			return media.Part{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.apiBase+path, nil)
		if err != nil {
			return media.Part{}, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = d.client.Do(req)
		if err != nil {
			return media.Part{}, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			d.tokens.invalidate()
			continue
		}
		break
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Part{}, fmt.Errorf("feishu: resource %s status %d", spec.key, resp.StatusCode)
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mime == "" || mime == "application/octet-stream" || !strings.Contains(mime, "/") {
		mime = mimeFromName(spec.filename, spec.hintMIME)
	}
	if !d.pol.Allows(mime) {
		return media.Part{}, fmt.Errorf("feishu: mime %q not allowed by media policy", mime)
	}
	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}
	if resp.ContentLength > limit {
		return media.Part{}, fmt.Errorf("feishu: resource %d bytes exceeds limit %d", resp.ContentLength, limit)
	}
	ref, err := d.store.Put(io.LimitReader(resp.Body, limit+1), mime, d.pol)
	if err != nil {
		return media.Part{}, err
	}
	metrics.Add("agentflow_media_bytes", ref.Size)
	name := spec.filename
	if name == "" {
		name = "attachment"
	}
	return media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle, Name: name}, nil
}

func mimeFromName(name, hint string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".opus"):
		return "audio/opus"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	}
	if hint != "" {
		return hint
	}
	return "application/octet-stream"
}

func parseMillis(raw string) int64 {
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
		return ms / 1000
	}
	return time.Now().Unix()
}

type dedup struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	at    map[string]time.Time
	order []seenEntry
}

type seenEntry struct {
	id string
	at time.Time
}

func newDedup(max int, ttl time.Duration) *dedup {
	return &dedup{max: max, ttl: ttl, at: map[string]time.Time{}}
}

func (s *dedup) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if t, ok := s.at[id]; ok && now.Sub(t) < s.ttl {
		return false
	}
	s.at[id] = now
	s.order = append(s.order, seenEntry{id, now})
	for len(s.order) > 0 && (len(s.order) > s.max || now.Sub(s.order[0].at) >= s.ttl) {
		old := s.order[0]
		s.order = s.order[1:]
		if s.at[old.id].Equal(old.at) {
			delete(s.at, old.id)
		}
	}
	return true
}
