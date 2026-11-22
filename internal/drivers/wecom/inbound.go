package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

type inboundXML struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgId        string `xml:"MsgId"`
	AgentID      int64  `xml:"AgentID"`
	MediaId      string `xml:"MediaId"`
	PicUrl       string `xml:"PicUrl"`
	Format       string `xml:"Format"`
	Recognition  string `xml:"Recognition"`
	Title        string `xml:"Title"`
	Description  string `xml:"Description"`
	Url          string `xml:"Url"`
	LocationX    string `xml:"Location_X"`
	LocationY    string `xml:"Location_Y"`
	Label        string `xml:"Label"`
	Event        string `xml:"Event"`
	EventKey     string `xml:"EventKey"`
}

func (d *Driver) handleInboundXML(raw []byte) {
	var msg inboundXML
	if err := xml.Unmarshal(raw, &msg); err != nil {
		d.log.Warn("wecom inner xml undecodable", "err", err)
		return
	}
	msgType := strings.ToLower(strings.TrimSpace(msg.MsgType))
	if msgType == "event" {
		if msg.Event != "" {
			if _, seen := d.unknown.LoadOrStore("event:"+msg.Event, true); !seen {
				d.log.Info("wecom event ignored", "event", msg.Event)
			}
		}
		return
	}
	if msg.FromUserName == "" {
		d.log.Warn("wecom message missing FromUserName", "type", msgType)
		return
	}
	if msg.MsgId != "" && !d.seen.add(msg.MsgId) {
		return
	}

	var text string
	var atts []media.Part
	switch msgType {
	case "text":
		text = msg.Content
	case "voice":
		text = msg.Recognition
		if msg.MediaId != "" {
			mimeHint := "audio/amr"
			name := "voice.amr"
			if msg.Format != "" {
				f := strings.ToLower(strings.TrimSpace(msg.Format))
				mimeHint = "audio/" + f
				name = "voice." + f
			}
			atts = d.ingestMedia(msg.MediaId, name, mimeHint)
		}
	case "image":
		if msg.MediaId != "" {
			atts = d.ingestMedia(msg.MediaId, "image.jpg", "image/jpeg")
		}
	case "video":
		if msg.MediaId != "" {
			atts = d.ingestMedia(msg.MediaId, "video.mp4", "video/mp4")
		}
	case "location":
		if msg.Label != "" {
			text = fmt.Sprintf("[Location] %s (%s, %s)", msg.Label, msg.LocationX, msg.LocationY)
		} else {
			text = fmt.Sprintf("[Location] (%s, %s)", msg.LocationX, msg.LocationY)
		}
	case "link":
		var parts []string
		if msg.Title != "" {
			parts = append(parts, msg.Title)
		}
		if msg.Description != "" {
			parts = append(parts, msg.Description)
		}
		if msg.Url != "" {
			parts = append(parts, msg.Url)
		}
		text = strings.Join(parts, "\n")
	default:
		if msg.MediaId != "" {
			atts = d.ingestMedia(msg.MediaId, "attachment", "application/octet-stream")
		} else if _, seen := d.unknown.LoadOrStore("msg:"+msgType, true); !seen {
			d.log.Info("wecom message type not handled", "type", msgType)
		}
	}

	text = strings.TrimSpace(text)
	if text == "" && len(atts) == 0 {
		return
	}

	replyTo := "user:" + msg.FromUserName
	id := msg.MsgId
	if id == "" {
		id = fmt.Sprintf("%d-%s", msg.CreateTime, msg.FromUserName)
	}
	ts := msg.CreateTime
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	payload := map[string]any{
		"chat_id":    replyTo,
		"userid":     msg.FromUserName,
		"message_id": id,
		"msg_type":   msgType,
	}
	if msg.ToUserName != "" {
		payload["corp_id"] = msg.ToUserName
	}
	if msg.AgentID != 0 {
		payload["agent_id"] = msg.AgentID
	}

	d.log.Debug("wecom message received", "type", msgType, "from", msg.FromUserName, "text_len", len(text), "media", len(atts))
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          "wecom-" + id,
			Type:        "user",
			From:        "user:wecom:" + msg.FromUserName,
			Text:        text,
			Channel:     d.name,
			ReplyTo:     replyTo,
			Attachments: atts,
			Ts:          ts,
			Payload:     payload,
		},
	})
}

func (d *Driver) ingestMedia(mediaID, defaultName, mimeHint string) []media.Part {
	if d.store == nil || !d.pol.Enabled() || mediaID == "" {
		return nil
	}
	p, err := d.downloadMedia(mediaID, defaultName, mimeHint)
	if err != nil {
		d.log.Warn("wecom media ingest failed", "media_id", mediaID, "err", err)
		metrics.Inc("agentflow_media_unsupported")
		return nil
	}
	metrics.Inc("agentflow_media_ingested")
	return []media.Part{p}
}

func (d *Driver) downloadMedia(mediaID, defaultName, mimeHint string) (media.Part, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}

	var data []byte
	var contentType, cdHeader string
	for attempt := 0; ; attempt++ {
		tok, err := d.tokens.Token(ctx)
		if err != nil {
			return media.Part{}, err
		}
		endpoint := fmt.Sprintf("%s/cgi-bin/media/get?access_token=%s&media_id=%s",
			d.apiBase, url.QueryEscape(tok), url.QueryEscape(mediaID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return media.Part{}, err
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return media.Part{}, err
		}
		contentType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		cdHeader = resp.Header.Get("Content-Disposition")
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if readErr != nil {
			return media.Part{}, readErr
		}
		// Check whether WeCom returned an error JSON envelope instead of media bytes.
		if strings.Contains(contentType, "json") || (len(body) > 0 && body[0] == '{') {
			var apiErr apiError
			if json.Unmarshal(body, &apiErr) == nil && apiErr.ErrCode != 0 {
				if isTokenExpiredErr(apiErr.ErrCode) && attempt == 0 {
					d.tokens.invalidate()
					continue
				}
				return media.Part{}, &apiErr
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			d.tokens.invalidate()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return media.Part{}, fmt.Errorf("wecom: media %s status %d", mediaID, resp.StatusCode)
		}
		if int64(len(body)) > limit {
			return media.Part{}, fmt.Errorf("wecom: media exceeds limit %d bytes", limit)
		}
		data = body
		break
	}

	name := defaultName
	if cdHeader != "" {
		if _, params, err := mime.ParseMediaType(cdHeader); err == nil && params["filename"] != "" {
			name = params["filename"]
		}
	}
	m := contentType
	if m == "" || m == "application/octet-stream" || !strings.Contains(m, "/") {
		m = mimeHint
	}
	if !d.pol.Allows(m) {
		return media.Part{}, fmt.Errorf("wecom: mime %q not allowed by media policy", m)
	}
	ref, err := d.store.Put(bytes.NewReader(data), m, d.pol)
	if err != nil {
		return media.Part{}, err
	}
	metrics.Add("agentflow_media_bytes", ref.Size)
	return media.Part{Type: media.Classify(m), MIME: m, Handle: ref.Handle, Name: name}, nil
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
