package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestTestTokenGetMeAndPrivacyMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botpriv-on/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":9001,"is_bot":true,"username":"helper_bot","first_name":"Helper","can_join_groups":true,"can_read_all_group_messages":false}}`))
		case "/botpriv-off/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":9002,"is_bot":true,"username":"open_bot","first_name":"Open","can_join_groups":true,"can_read_all_group_messages":true}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
		}
	}))
	defer srv.Close()

	info, err := TestToken(context.Background(), "priv-on", srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("priv-on error = %v", err)
	}
	if info.BotID != 9001 || info.Username != "helper_bot" || !info.PrivacyModeEnabled || info.CanReadAllGroupMessages {
		t.Fatalf("unexpected priv-on BotInfo: %+v", info)
	}

	info2, err := TestToken(context.Background(), "priv-off", srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("priv-off error = %v", err)
	}
	if info2.PrivacyModeEnabled || !info2.CanReadAllGroupMessages {
		t.Fatalf("unexpected priv-off BotInfo: %+v", info2)
	}

	if _, err := TestToken(context.Background(), "bad-secret-tok", srv.URL, srv.Client()); err == nil || strings.Contains(err.Error(), "bad-secret-tok") {
		t.Fatalf("expected scrubbed unauthorized error, got %v", err)
	}
}

func TestGroupPayloadEnrichmentUTF16AndOnDrop(t *testing.T) {
	sink := &captureSink{}
	d := &Driver{
		name:  "tg",
		agent: "helper",
		allow: map[int64]bool{100: true},
		sink:  sink,
		log:   testLogger(),
	}
	d.SetBotInfo(BotInfo{
		BotID:                   999,
		Username:                "helper_bot",
		CanReadAllGroupMessages: true,
	})

	var dropped []string
	d.OnDrop = func(channel, agent, chatID, sender, text, reason string) {
		dropped = append(dropped, channel+"|"+agent+"|"+chatID+"|"+sender+"|"+reason)
	}

	// Non-allowed user triggers OnDrop.
	d.handleUpdate(update{
		UpdateID: 1,
		Message: &tgMessage{
			MessageID: 10,
			Text:      "hi",
			From:      struct{ ID int64 `json:"id"`; IsBot bool `json:"is_bot"`; Username string `json:"username"`; FirstName string `json:"first_name"` }{ID: 777},
			Chat:      struct{ ID int64 `json:"id"`; Type string `json:"type"`; Title string `json:"title"` }{ID: -10055, Type: "supergroup"},
		},
	})
	if len(sink.inbs) != 0 {
		t.Fatalf("disallowed sender must not be routed, got %+v", sink.inbs)
	}
	if len(dropped) != 1 || dropped[0] != "tg|helper|-10055|user:telegram:777|allow_users" {
		t.Fatalf("dropped = %v", dropped)
	}

	// SDK update with leading surrogate-pair emoji tests UTF-16 entity slicing + reply + thread.
	sdkUpd := &models.Update{
		ID: 2,
		Message: &models.Message{
			ID:              22,
			MessageThreadID: 77,
			Text:            "😀 @helper_bot check status",
			Entities: []models.MessageEntity{
				{Type: models.MessageEntityTypeMention, Offset: 3, Length: 11},
			},
			From: &models.User{ID: 100, Username: "alice", FirstName: "Alice"},
			Chat: models.Chat{ID: -10055, Type: models.ChatTypeSupergroup, Title: "Dev Group"},
			ReplyToMessage: &models.Message{
				ID:   21,
				Text: "previous bot answer",
				From: &models.User{ID: 999, IsBot: true, Username: "helper_bot"},
			},
		},
	}
	d.handleUpdate(fromSDK(sdkUpd))
	if len(sink.inbs) != 1 {
		t.Fatalf("expected 1 routed message, got %d", len(sink.inbs))
	}
	p := sink.inbs[0].Message.Payload
	if p["chat_type"] != "supergroup" || p["chat_title"] != "Dev Group" {
		t.Fatalf("chat metadata = %+v", p)
	}
	if p["message_thread_id"] != int64(77) {
		t.Fatalf("message_thread_id = %v", p["message_thread_id"])
	}
	if p["mentioned"] != true || p["is_reply_to_bot"] != true {
		t.Fatalf("expected mentioned=true and is_reply_to_bot=true, got %+v", p)
	}
	if _, hasOthers := p["mentions_others"]; hasOthers {
		t.Fatalf("did not expect mentions_others when only bot was mentioned: %+v", p)
	}
	if p["reply_to_text"] != "previous bot answer" || p["reply_to_author"] != "helper_bot" || p["bot_username"] != "helper_bot" {
		t.Fatalf("reply/bot fields = %+v", p)
	}

	// Plain non-mention group message must set mentioned=false and is_reply_to_bot=false.
	sdkOther := &models.Update{
		ID: 3,
		Message: &models.Message{
			ID:   23,
			Text: "email me at a@helper_bot.example.com please",
			From: &models.User{ID: 100, Username: "alice", FirstName: "Alice"},
			Chat: models.Chat{ID: -10055, Type: models.ChatTypeGroup},
		},
	}
	d.handleUpdate(fromSDK(sdkOther))
	if len(sink.inbs) != 2 {
		t.Fatalf("expected 2 routed messages, got %d", len(sink.inbs))
	}
	p2 := sink.inbs[1].Message.Payload
	if p2["mentioned"] != false || p2["is_reply_to_bot"] != false {
		t.Fatalf("unrelated message must not trigger mention/reply flags, got %+v", p2)
	}
	if _, hasOthers := p2["mentions_others"]; hasOthers {
		t.Fatalf("plain message without mention entity must not set mentions_others: %+v", p2)
	}

	// Mentioning another member (username @mention) and quoting a user with FirstName.
	sdkMentionOther := &models.Update{
		ID: 4,
		Message: &models.Message{
			ID:   24,
			Text: "@bob can you check that?",
			Entities: []models.MessageEntity{
				{Type: models.MessageEntityTypeMention, Offset: 0, Length: 4},
			},
			From: &models.User{ID: 100, Username: "alice", FirstName: "Alice"},
			Chat: models.Chat{ID: -10055, Type: models.ChatTypeSupergroup},
			ReplyToMessage: &models.Message{
				ID:   23,
				Text: "original note",
				From: &models.User{ID: 200, Username: "bob_u", FirstName: "Bob"},
			},
		},
	}
	d.handleUpdate(fromSDK(sdkMentionOther))
	if len(sink.inbs) != 3 {
		t.Fatalf("expected 3 routed messages, got %d", len(sink.inbs))
	}
	p3 := sink.inbs[2].Message.Payload
	if p3["mentioned"] != false || p3["mentions_others"] != true || p3["is_reply_to_bot"] != false {
		t.Fatalf("expected mentions_others=true without bot mention/reply, got %+v", p3)
	}
	if p3["reply_to_author"] != "Bob" {
		t.Fatalf("expected reply_to_author=Bob from FirstName, got %+v", p3["reply_to_author"])
	}

	// Text-mentioning another user (without @username) must also set mentions_others=true.
	sdkTextMentionOther := &models.Update{
		ID: 5,
		Message: &models.Message{
			ID:   25,
			Text: "Carol what do you think?",
			Entities: []models.MessageEntity{
				{
					Type:   models.MessageEntityTypeTextMention,
					Offset: 0,
					Length: 5,
					User:   &models.User{ID: 300, FirstName: "Carol"},
				},
			},
			From: &models.User{ID: 100, Username: "alice", FirstName: "Alice"},
			Chat: models.Chat{ID: -10055, Type: models.ChatTypeSupergroup},
		},
	}
	d.handleUpdate(fromSDK(sdkTextMentionOther))
	if len(sink.inbs) != 4 {
		t.Fatalf("expected 4 routed messages, got %d", len(sink.inbs))
	}
	p4 := sink.inbs[3].Message.Payload
	if p4["mentioned"] != false || p4["mentions_others"] != true {
		t.Fatalf("expected mentions_others=true for text_mention of another user, got %+v", p4)
	}
}
