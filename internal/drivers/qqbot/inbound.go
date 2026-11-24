package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

// Event types this driver turns into messages.
const (
	evC2C   = "C2C_MESSAGE_CREATE"
	evGroup = "GROUP_AT_MESSAGE_CREATE"
	// evGroupAll is a group message that does not @-mention the bot. Only
	// bots granted full group-message access receive it.
	evGroupAll = "GROUP_MESSAGE_CREATE"
	evGuild    = "AT_MESSAGE_CREATE"
	evDirect   = "DIRECT_MESSAGE_CREATE"
)

// message is the union of the message events' d, reduced to what the driver
// uses. C2C authors carry user_openid, group authors member_openid, guild and
// DM authors a plain id.
type message struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Timestamp  string `json:"timestamp"`
	GroupID    string `json:"group_openid"`
	ChannelID  string `json:"channel_id"`
	GuildID    string `json:"guild_id"`
	SrcGuildID string `json:"src_guild_id"`
	Author     struct {
		ID           string `json:"id"`
		UserOpenID   string `json:"user_openid"`
		MemberOpenID string `json:"member_openid"`
		UnionOpenID  string `json:"union_openid"`
		Username     string `json:"username"`
		Nickname     string `json:"member_nickname"`
		Bot          bool   `json:"bot"`
	} `json:"author"`
	Attachments []attachment `json:"attachments"`
	// Mentions lists who the message @s. A group message's content keeps each
	// of them as an opaque <@id> marker.
	Mentions         []mentionee `json:"mentions"`
	MessageReference *struct {
		MessageID             string `json:"message_id"`
		IgnoreGetMessageError bool   `json:"ignore_get_message_error"`
	} `json:"message_reference"`
	MessageScene *struct {
		Source string   `json:"source"`
		Ext    []string `json:"ext"`
	} `json:"message_scene"`
}

func parseSceneExt(ext []string) (msgIdx, refMsgIdx string) {
	for _, item := range ext {
		k, v, ok := strings.Cut(item, "=")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "msg_idx":
			msgIdx = v
		case "ref_msg_idx":
			refMsgIdx = v
		}
	}
	return msgIdx, refMsgIdx
}

// mentionee is one @ in a message. IsYou marks the bot itself.
type mentionee struct {
	ID           string `json:"id"`
	UserOpenID   string `json:"user_openid"`
	MemberOpenID string `json:"member_openid"`
	Nickname     string `json:"member_nickname"`
	Username     string `json:"username"`
	Bot          bool   `json:"bot"`
	IsYou        bool   `json:"is_you"`
}

// resolveMentions rewrites the <@id> markers of a group message. The bot's
// own marker is dropped, anyone else's becomes @name when the platform gave a
// name. It reports whether the bot was @ed and whether anyone else was.
func resolveMentions(text string, list []mentionee, botID string) (out string, self, others bool) {
	for _, m := range list {
		name := m.Nickname
		if name == "" {
			name = m.Username
		}
		isSelf := m.IsYou || (botID != "" && (m.ID == botID || m.UserOpenID == botID || m.MemberOpenID == botID))
		if isSelf {
			self = true
		} else {
			others = true
		}
		ids := []string{m.ID, m.MemberOpenID, m.UserOpenID}
		if isSelf && botID != "" {
			ids = append(ids, botID)
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			for _, mark := range []string{"<@" + id + ">", "<@!" + id + ">"} {
				switch {
				case isSelf:
					text = strings.ReplaceAll(text, mark, "")
				case name != "":
					text = strings.ReplaceAll(text, mark, "@"+name)
				}
			}
		}
	}
	return strings.TrimSpace(text), self, others
}

type attachment struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	VoiceWavURL string `json:"voice_wav_url"`
	ASRText     string `json:"asr_refer_text"`
}

// mention matches a leading guild @-mention of the bot: <@!123> or <@123>.
var mention = regexp.MustCompile(`^\s*(<@!?\w+>\s*)+`)

// handleEvent turns one dispatched event into a session turn. Event types the
// driver does not use, and scenes the channel does not listen to, are dropped.
func (d *Driver) handleEvent(t, eventID string, raw json.RawMessage) {
	scene := ""
	mentioned := true
	switch t {
	case evC2C:
		scene = SceneC2C
	case evGroup:
		scene = SceneGroup
	case evGroupAll:
		scene = SceneGroup
		mentioned = false
	case evGuild:
		scene = SceneGuild
	case evDirect:
		scene = SceneDM
	default:
		// Said once per type: what the platform actually pushes to this bot
		// is otherwise invisible, and "why does it not hear X" starts here.
		if _, seen := d.unknown.LoadOrStore(t, true); !seen {
			d.log.Info("qq event type not handled", "type", t)
		}
		return
	}
	switch {
	case t == evGroupAll || t == evGroup:
		if !d.scenes[SceneGroup] && !d.scenes[SceneGroupAll] {
			if _, seen := d.unknown.LoadOrStore("scene:"+t, true); !seen {
				d.log.Info("qq event ignored because its scene is not enabled", "type", t)
			}
			return
		}
	case !d.scenes[scene]:
		if _, seen := d.unknown.LoadOrStore("scene:"+t, true); !seen {
			d.log.Info("qq event ignored because its scene is not enabled", "type", t)
		}
		return
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		d.log.Warn("qq event undecodable", "type", t, "err", err)
		return
	}
	if m.ID == "" || m.Author.Bot {
		return
	}

	var msgIdx, refMsgIdx, refMsgID string
	if m.MessageScene != nil {
		msgIdx, refMsgIdx = parseSceneExt(m.MessageScene.Ext)
	}
	if m.MessageReference != nil {
		refMsgID = strings.TrimSpace(m.MessageReference.MessageID)
	}
	refEntry, hasRef := d.lookupMsg(refMsgIdx, refMsgID)
	isReplyToBot := hasRef && refEntry.isBot

	d.gwMu.Lock()
	botID, botName := d.botID, d.botName
	d.gwMu.Unlock()

	var target, from string
	payload := map[string]any{"scene": scene, "message_id": m.ID}
	if eventID != "" {
		payload["event_id"] = eventID
	}
	if m.Author.UnionOpenID != "" {
		payload["union_openid"] = m.Author.UnionOpenID
	}
	text := strings.TrimSpace(m.Content)
	if scene == SceneGroup {
		// With full group access the platform pushes an @ of the bot as a
		// GROUP_MESSAGE_CREATE event too; the mentions list tells it apart
		// from an ordinary group message.
		var self, others bool
		text, self, others = resolveMentions(text, m.Mentions, botID)
		if self {
			mentioned = true
		}
		if t == evGroupAll && !d.scenes[SceneGroupAll] && !mentioned && !isReplyToBot {
			if _, seen := d.unknown.LoadOrStore("drop:"+t, true); !seen {
				d.log.Info("qq group message without @-mention ignored; add scene group_all to receive all group messages", "type", t)
			}
			return
		}
		// Whether the bot was addressed with an @. A loop that listens to a
		// whole group uses it to tell a message for it from one it overheard.
		payload["mentioned"] = mentioned
		if others || strings.Contains(text, "<@") {
			payload["mentions_others"] = true
		}
		if botName != "" {
			payload["bot_username"] = botName
		}
	}
	refID := refMsgIdx
	if refID == "" {
		refID = refMsgID
	}
	if refID != "" {
		payload["reply_to_message_id"] = refID
	}
	if hasRef {
		if refEntry.text != "" {
			payload["reply_to_text"] = refEntry.text
		}
		if refEntry.author != "" {
			payload["reply_to_author"] = refEntry.author
		}
		if refEntry.isBot {
			payload["is_reply_to_bot"] = true
		}
	}
	// The platform may push one message more than once; a webhook retry and a
	// gateway replay after resume both look like this. Check after the scene
	// and mention gate so a dropped event does not suppress a companion event
	// carrying the same message id.
	if !d.seen.add(m.ID) {
		return
	}
	switch scene {
	case SceneC2C:
		target = SceneC2C + ":" + m.Author.UserOpenID
		from = m.Author.UserOpenID
		payload["user_openid"] = m.Author.UserOpenID
	case SceneGroup:
		target = SceneGroup + ":" + m.GroupID
		from = m.Author.MemberOpenID
		if from == "" {
			from = m.Author.ID
		}
		payload["group_openid"] = m.GroupID
		payload["member_openid"] = from
		if m.Author.Nickname != "" {
			payload["name"] = m.Author.Nickname
		}
	case SceneGuild:
		target = SceneGuild + ":" + m.ChannelID
		from = m.Author.ID
		payload["guild_id"] = m.GuildID
		payload["channel_id"] = m.ChannelID
		text = strings.TrimSpace(mention.ReplaceAllString(m.Content, ""))
		if botName != "" {
			payload["bot_username"] = botName
		}
	case SceneDM:
		target = SceneDM + ":" + m.GuildID
		from = m.Author.ID
		payload["guild_id"] = m.GuildID
		payload["channel_id"] = m.ChannelID
		if m.SrcGuildID != "" {
			payload["src_guild_id"] = m.SrcGuildID
		}
	}
	if from == "" || strings.HasSuffix(target, ":") {
		d.log.Warn("qq event without a sender or conversation id", "type", t, "message_id", m.ID)
		return
	}
	if m.Author.Username != "" {
		payload["username"] = m.Author.Username
		if payload["name"] == nil {
			payload["name"] = m.Author.Username
		}
	}
	payload["chat_id"] = target

	// A voice message's text is what the platform's speech recognition heard.
	for _, a := range m.Attachments {
		if text == "" && a.ASRText != "" {
			text = a.ASRText
		}
	}
	atts := d.ingestMedia(m.Attachments)
	if text == "" && len(atts) == 0 {
		return
	}
	who, _ := payload["name"].(string)
	if who == "" {
		who = from
	}
	d.rememberMsg([]string{m.ID, msgIdx}, indexedMsg{
		author: who,
		text:   text,
		isBot:  false,
		at:     time.Now(),
	})
	d.rememberRef(target, m.ID)
	d.log.Debug("qq message received", "scene", scene, "message_id", m.ID, "text_len", len(text), "media", len(atts))
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          "qq-" + m.ID,
			Type:        "user",
			From:        "user:qq:" + from,
			Text:        text,
			Channel:     d.name,
			ReplyTo:     target,
			Attachments: atts,
			Ts:          eventTime(m.Timestamp),
			Payload:     payload,
		},
	})
}

func eventTime(ts string) int64 {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
}

// ingestMedia downloads attachments into the blob store when the channel's
// media policy allows them. With media disabled every attachment is dropped
// and the text still flows.
func (d *Driver) ingestMedia(list []attachment) []media.Part {
	if d.store == nil || !d.pol.Enabled() || len(list) == 0 {
		return nil
	}
	var parts []media.Part
	for _, a := range list {
		p, err := d.download(a)
		if err != nil {
			d.log.Warn("qq media ingest failed", "err", err)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) > 0 {
		metrics.Inc("agentflow_media_ingested")
	}
	return parts
}

func (d *Driver) download(a attachment) (media.Part, error) {
	link := a.URL
	if link == "" {
		link = a.VoiceWavURL
	}
	if link == "" {
		return media.Part{}, fmt.Errorf("qq: attachment without a url")
	}
	// Guild attachments come without a scheme.
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		link = "https://" + strings.TrimPrefix(link, "//")
	}
	mime := strings.TrimSpace(strings.Split(a.ContentType, ";")[0])
	if mime == "" || !strings.Contains(mime, "/") {
		mime = mimeFromName(a.Filename, a.ContentType)
	}
	if !d.pol.Allows(mime) {
		return media.Part{}, fmt.Errorf("qq: mime %q not allowed by channel media policy", mime)
	}
	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}
	if a.Size > limit {
		return media.Part{}, fmt.Errorf("qq: file %d bytes exceeds limit %d", a.Size, limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return media.Part{}, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return media.Part{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Part{}, fmt.Errorf("qq: attachment download: status %d", resp.StatusCode)
	}
	ref, err := d.store.Put(resp.Body, mime, d.pol)
	if err != nil {
		return media.Part{}, err
	}
	metrics.Add("agentflow_media_bytes", ref.Size)
	name := a.Filename
	if name == "" {
		name = "attachment"
	}
	return media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle, Name: name}, nil
}

// mimeFromName guesses a MIME type from a filename or a bare content type
// such as "image" or "voice".
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
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(lower, ".amr"):
		return "audio/amr"
	case strings.HasSuffix(lower, ".silk"), strings.HasSuffix(lower, ".slk"):
		return "audio/silk"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	}
	switch strings.ToLower(hint) {
	case "image":
		return "image/jpeg"
	case "voice", "audio":
		return "audio/wav"
	case "video":
		return "video/mp4"
	}
	return "application/octet-stream"
}

// ---- duplicate suppression -----------------------------------------------------

// dedup remembers recently seen message ids, bounded in count and age.
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

// add reports whether id is new (or last seen longer ago than the TTL), and
// remembers it.
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
		// Only forget the id if this entry is its latest sighting.
		if s.at[old.id].Equal(old.at) {
			delete(s.at, old.id)
		}
	}
	return true
}
