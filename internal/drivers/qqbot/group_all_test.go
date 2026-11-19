package qqbot

import (
	"encoding/json"
	"testing"
)

// A group message that does not @-mention the bot arrives as its own event,
// and only for bots the platform has granted full group-message access. The
// driver takes it when the channel opts in with the group_all scene, and
// marks every group message with whether the bot was addressed.
func TestGroupAllScene(t *testing.T) {
	plain := json.RawMessage(`{"id":"p1","content":"anyone up for a game?","group_openid":"G9","author":{"member_openid":"M1","member_nickname":"Ann"}}`)
	at := json.RawMessage(`{"id":"a1","content":" you in? ","group_openid":"G9","author":{"member_openid":"M2"}}`)

	// Without the opt-in the plain message is dropped; the @-mention is not.
	d, sink, _ := newDriver(t, nil, Options{Scenes: []string{"group"}})
	d.handleEvent(evGroupAll, "e1", plain)
	d.handleEvent(evGroup, "e2", at)
	got := sink.all()
	if len(got) != 1 || got[0].Message.Payload["mentioned"] != true {
		t.Fatalf("scenes [group]: %+v", got)
	}

	// With it both arrive, in the same group conversation, told apart.
	d, sink, _ = newDriver(t, nil, Options{Scenes: []string{"group_all"}})
	d.handleEvent(evGroupAll, "e1", plain)
	d.handleEvent(evGroup, "e2", at)
	got = sink.all()
	if len(got) != 2 {
		t.Fatalf("scenes [group_all]: %d messages", len(got))
	}
	p, a := got[0].Message, got[1].Message
	if p.Payload["mentioned"] != false || p.Payload["scene"] != "group" || p.ReplyTo != "group:G9" || p.Text != "anyone up for a game?" {
		t.Fatalf("plain message = %+v", p)
	}
	if a.Payload["mentioned"] != true || a.ReplyTo != "group:G9" {
		t.Fatalf("@-mention = %+v", a)
	}
	if d.intents()&intentGroupAndC2C == 0 {
		t.Fatal("group_all must subscribe to the group intent")
	}

	// A message pushed as both events is one message.
	d.handleEvent(evGroup, "e3", plain)
	if n := len(sink.all()); n != 2 {
		t.Fatalf("a message delivered under both events made %d messages", n)
	}
}

// With full group access an @ of the bot arrives as a plain group message;
// the mentions list says whether it was the bot or someone else.
func TestGroupAllMentions(t *testing.T) {
	d, sink, _ := newDriver(t, nil, Options{Scenes: []string{"group_all"}})
	d.handleEvent(evGroupAll, "e1", json.RawMessage(`{"id":"m1","content":"<@BOT1> still alive?","group_openid":"G9","author":{"member_openid":"M1"},"mentions":[{"member_openid":"BOT1","bot":true,"is_you":true}]}`))
	d.handleEvent(evGroupAll, "e2", json.RawMessage(`{"id":"m2","content":"<@BOT2> where is your owner","group_openid":"G9","author":{"member_openid":"M1"},"mentions":[{"member_openid":"BOT2","member_nickname":"Other","username":"other","bot":true,"is_you":false}]}`))
	d.handleEvent(evGroupAll, "e3", json.RawMessage(`{"id":"m3","content":"<@X9> hi","group_openid":"G9","author":{"member_openid":"M1"}}`))
	got := sink.all()
	if len(got) != 3 {
		t.Fatalf("%d messages", len(got))
	}
	me, other, bare := got[0].Message, got[1].Message, got[2].Message
	if me.Payload["mentioned"] != true || me.Payload["mentions_others"] != nil || me.Text != "still alive?" {
		t.Fatalf("@ of the bot = %q %+v", me.Text, me.Payload)
	}
	if other.Payload["mentioned"] != false || other.Payload["mentions_others"] != true || other.Text != "@Other where is your owner" {
		t.Fatalf("@ of another = %q %+v", other.Text, other.Payload)
	}
	if bare.Payload["mentioned"] != false || bare.Payload["mentions_others"] != true {
		t.Fatalf("unlisted @ = %q %+v", bare.Text, bare.Payload)
	}
}
