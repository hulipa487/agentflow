package qqbot

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

const testSecret = "naOC0ocQE3shWLAfffVLB1rhYPG7"

type captureSink struct {
	mu  sync.Mutex
	got []router.Inbound
}

func (s *captureSink) Submit(in router.Inbound) {
	s.mu.Lock()
	s.got = append(s.got, in)
	s.mu.Unlock()
}

func (s *captureSink) all() []router.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]router.Inbound(nil), s.got...)
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAPI is the platform's HTTP side: the token endpoint, the gateway lookup
// and the message endpoints, recording every message posted.
type fakeAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	tokens    int
	posts     []posted
	reject    func(path string, body map[string]any) (int, string)
	gatewayWS string
}

type posted struct {
	path string
	auth string
	body map[string]any
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/getAppAccessToken":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["appId"] != "1024" || in["clientSecret"] != testSecret {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 100016, "message": "invalid appid or secret"})
				return
			}
			f.mu.Lock()
			f.tokens++
			n := f.tokens
			f.mu.Unlock()
			// expires_in arrives as a string, as the docs' example shows.
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": "7200"})
		case r.URL.Path == "/gateway/bot":
			_ = json.NewEncoder(w).Encode(map[string]any{"url": f.gatewayWS, "shards": 1})
		default:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.posts = append(f.posts, posted{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
			reject := f.reject
			f.mu.Unlock()
			if reject != nil {
				if status, code := reject(r.URL.Path, body); status != 0 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, code)
					return
				}
			}
			if strings.HasSuffix(r.URL.Path, "/files") {
				_ = json.NewEncoder(w).Encode(map[string]any{"file_info": "FINFO", "ttl": 3600})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sent", "timestamp": time.Now().Format(time.RFC3339)})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) sent() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.posts...)
}

func newDriver(t *testing.T, api *fakeAPI, opts Options) (*Driver, *captureSink, *httpd.Server) {
	t.Helper()
	sink := &captureSink{}
	srv := httpd.New("127.0.0.1:0", testLog())
	if opts.AppID == "" {
		opts.AppID = "1024"
	}
	if opts.Secret == "" {
		opts.Secret = testSecret
	}
	if api != nil {
		opts.APIBase = api.srv.URL
	}
	d := New("qq", "main", opts, sink, srv, nil, media.Policy{}, testLog())
	d.retryBase = 10 * time.Millisecond
	return d, sink, srv
}

func TestSigningKeyFollowsTheDocumentedSeedRule(t *testing.T) {
	// The docs' example: the secret repeated to 32 bytes.
	want := ed25519.NewKeyFromSeed([]byte("naOC0ocQE3shWLAfffVLB1rhYPG7naOC"))
	if got := signingKey(testSecret); !bytes.Equal(got, want) {
		t.Fatal("seed derivation does not match the documented example")
	}
	short := signingKey("abc")
	if !bytes.Equal(short, ed25519.NewKeyFromSeed([]byte("abcabcabcabcabcabcabcabcabcabcab"))) {
		t.Fatal("a short secret must be repeated until it fills the seed")
	}
}

func signed(t *testing.T, priv ed25519.PrivateKey, body []byte) *http.Request {
	t.Helper()
	ts := "1725442341"
	req := httptest.NewRequest(http.MethodPost, "/webhook/qq/", bytes.NewReader(body))
	req.Header.Set(headerTimestamp, ts)
	req.Header.Set(headerSignature, hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), body...))))
	return req
}

func TestWebhookValidationHandshake(t *testing.T) {
	d, _, srv := newDriver(t, nil, Options{Mode: "webhook"})
	body := []byte(`{"op":13,"d":{"plain_token":"Arq0D5A61EgUu4OxUvOp","event_ts":"1725442341"}}`)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, signed(t, signingKey(testSecret), body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var out struct{ PlainToken, Signature string }
	_ = json.Unmarshal(w.Body.Bytes(), &struct {
		P *string `json:"plain_token"`
		S *string `json:"signature"`
	}{&out.PlainToken, &out.Signature})
	if out.PlainToken != "Arq0D5A61EgUu4OxUvOp" {
		t.Fatalf("plain_token = %q", out.PlainToken)
	}
	sig, _ := hex.DecodeString(out.Signature)
	if !ed25519.Verify(d.pub, []byte("1725442341Arq0D5A61EgUu4OxUvOp"), sig) {
		t.Fatal("the handshake signature is not event_ts+plain_token signed with the app key")
	}
}

func TestWebhookRejectsUnsignedAndForgedRequests(t *testing.T) {
	_, sink, srv := newDriver(t, nil, Options{Mode: "webhook"})
	event := []byte(`{"op":0,"t":"C2C_MESSAGE_CREATE","id":"ev1","d":{"id":"m1","content":"hi","author":{"user_openid":"U1"}}}`)

	unsigned := httptest.NewRequest(http.MethodPost, "/webhook/qq/", bytes.NewReader(event))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, unsigned)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: status %d, want 401", w.Code)
	}

	_, other, _ := ed25519.GenerateKey(nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, signed(t, other, event))
	if w.Code != http.StatusForbidden {
		t.Fatalf("forged: status %d, want 403", w.Code)
	}

	// The handshake is refused unsigned too: answering it would sign
	// attacker-chosen text with the bot's key.
	hs := httptest.NewRequest(http.MethodPost, "/webhook/qq/", strings.NewReader(`{"op":13,"d":{"plain_token":"x","event_ts":"1"}}`))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hs)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned handshake: status %d, want 401", w.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "/webhook/qq/", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, get)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d, want 405", w.Code)
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("rejected requests reached the sink: %d", n)
	}
}

func TestWebhookDeliversAnEventAndAcks(t *testing.T) {
	_, sink, srv := newDriver(t, nil, Options{Mode: "webhook"})
	event := []byte(`{"op":0,"t":"C2C_MESSAGE_CREATE","id":"C2C_MESSAGE_CREATE:abc","d":{"id":"m1","content":" hello ","timestamp":"2026-11-19T10:00:00+08:00","author":{"id":"A","user_openid":"U1","union_openid":"UN1"}}}`)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, signed(t, signingKey(testSecret), event))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"op":12`) {
		t.Fatalf("ack: %d %s", w.Code, w.Body)
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	m := got[0].Message
	if m.Text != "hello" || m.From != "user:qq:U1" || m.ReplyTo != "c2c:U1" || m.Payload["chat_id"] != "c2c:U1" {
		t.Fatalf("message = %+v", m)
	}
	if m.Payload["event_id"] != "C2C_MESSAGE_CREATE:abc" || m.Payload["union_openid"] != "UN1" {
		t.Fatalf("payload = %+v", m.Payload)
	}
	// The same message pushed again is dropped.
	srv.Handler().ServeHTTP(httptest.NewRecorder(), signed(t, signingKey(testSecret), event))
	if n := len(sink.all()); n != 1 {
		t.Fatalf("a duplicate delivery made %d messages", n)
	}
}

func TestEventMappingPerScene(t *testing.T) {
	d, sink, _ := newDriver(t, nil, Options{Scenes: []string{"c2c", "group", "guild", "dm"}})
	d.handleEvent(evGroup, "e1", json.RawMessage(`{"id":"g1","content":" status? ","group_openid":"G9","author":{"member_openid":"M1","member_nickname":"Ann"}}`))
	d.handleEvent(evGuild, "e2", json.RawMessage(`{"id":"h1","content":"<@!4242> ping","channel_id":"C7","guild_id":"GU1","author":{"id":"777","username":"bob"}}`))
	d.handleEvent(evDirect, "e3", json.RawMessage(`{"id":"d1","content":"secret","channel_id":"C8","guild_id":"DMG","src_guild_id":"GU1","author":{"id":"778","username":"cy"}}`))
	d.handleEvent(evC2C, "e4", json.RawMessage(`{"id":"b1","content":"from a bot","author":{"user_openid":"U2","bot":true}}`))
	d.handleEvent("GUILD_MEMBER_ADD", "e5", json.RawMessage(`{}`))
	got := sink.all()
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3 (bot author and unknown events dropped)", len(got))
	}
	want := []struct{ text, from, reply string }{
		{"status?", "user:qq:M1", "group:G9"},
		{"ping", "user:qq:777", "guild:C7"},
		{"secret", "user:qq:778", "dm:DMG"},
	}
	for i, w := range want {
		m := got[i].Message
		if m.Text != w.text || m.From != w.from || m.ReplyTo != w.reply {
			t.Fatalf("message %d = %q from %q to %q, want %+v", i, m.Text, m.From, m.ReplyTo, w)
		}
	}
	if got[0].Message.Payload["name"] != "Ann" {
		t.Fatalf("group nickname missing: %+v", got[0].Message.Payload)
	}
}

func TestScenesNotConfiguredAreDropped(t *testing.T) {
	d, sink, _ := newDriver(t, nil, Options{Scenes: []string{"c2c"}})
	d.handleEvent(evGroup, "", json.RawMessage(`{"id":"g1","content":"hi","group_openid":"G","author":{"member_openid":"M"}}`))
	if len(sink.all()) != 0 {
		t.Fatal("a group message reached a c2c-only channel")
	}
	if got := d.intents(); got != intentGroupAndC2C {
		t.Fatalf("intents = %d, want only GROUP_AND_C2C_EVENT", got)
	}
	all, _, _ := newDriver(t, nil, Options{})
	if got, want := all.intents(), intentGroupAndC2C|intentPublicGuildMessages; got != want {
		t.Fatalf("default intents = %d, want %d", got, want)
	}
}

func TestVoiceTextComesFromSpeechRecognition(t *testing.T) {
	d, sink, _ := newDriver(t, nil, Options{})
	d.handleEvent(evC2C, "", json.RawMessage(`{"id":"v1","content":"","author":{"user_openid":"U"},"attachments":[{"content_type":"voice","url":"https://x/v.silk","asr_refer_text":"turn on the light"}]}`))
	got := sink.all()
	if len(got) != 1 || got[0].Message.Text != "turn on the light" {
		t.Fatalf("voice message = %+v", got)
	}
}

func TestTokenSourceCachesAndRefreshes(t *testing.T) {
	api := newFakeAPI(t)
	ts := newTokenSource("1024", testSecret, api.srv.URL+"/app/getAppAccessToken", api.srv.Client())
	a, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ts.Token(context.Background())
	if a != b || api.tokens != 1 {
		t.Fatalf("token not cached: %q %q after %d fetches", a, b, api.tokens)
	}
	// Inside the last minute a new token is fetched.
	ts.exp = time.Now().Add(30 * time.Second)
	c, _ := ts.Token(context.Background())
	if c == a || api.tokens != 2 {
		t.Fatalf("token not refreshed near expiry: %q after %d fetches", c, api.tokens)
	}
	bad := newTokenSource("1024", "wrong", api.srv.URL+"/app/getAppAccessToken", api.srv.Client())
	_, err = bad.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "wrong") || !strings.Contains(err.Error(), "invalid appid") {
		t.Fatalf("refused token error = %v (must explain, must not echo the secret)", err)
	}
}

func TestDeliverPassiveRepliesThenProactive(t *testing.T) {
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.handleEvent(evC2C, "", json.RawMessage(`{"id":"MSG1","content":"hi","author":{"user_openid":"U1"}}`))
	for i := 0; i < 5; i++ {
		if err := d.Deliver("c2c:U1", "answer", nil); err != nil {
			t.Fatal(err)
		}
	}
	posts := api.sent()
	if len(posts) != 5 {
		t.Fatalf("posts = %d, want 5", len(posts))
	}
	for i := 0; i < 4; i++ {
		p := posts[i]
		if p.path != "/v2/users/U1/messages" || p.body["msg_id"] != "MSG1" || p.body["msg_seq"] != float64(i+1) {
			t.Fatalf("reply %d not passive with msg_seq %d: %+v", i+1, i+1, p)
		}
		if p.auth != "QQBot tok1" {
			t.Fatalf("authorization = %q", p.auth)
		}
	}
	if _, ok := posts[4].body["msg_id"]; ok {
		t.Fatalf("the fifth C2C reply should be proactive: %+v", posts[4].body)
	}
}

func TestDeliverRetriesProactivelyWhenTheWindowClosed(t *testing.T) {
	api := newFakeAPI(t)
	api.reject = func(path string, body map[string]any) (int, string) {
		if body["msg_id"] != nil {
			return http.StatusBadRequest, `{"code":40034005,"message":"msg_id expired"}`
		}
		return 0, ""
	}
	d, _, _ := newDriver(t, api, Options{})
	d.handleEvent(evGroup, "", json.RawMessage(`{"id":"GM","content":"hi","group_openid":"G1","author":{"member_openid":"M"}}`))
	if err := d.Deliver("group:G1", "late answer", nil); err != nil {
		t.Fatal(err)
	}
	posts := api.sent()
	if len(posts) != 2 || posts[0].body["msg_id"] != "GM" || posts[1].body["msg_id"] != nil {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestDeliverGuildAndErrors(t *testing.T) {
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.handleEvent(evGuild, "", json.RawMessage(`{"id":"H1","content":"<@!1> hi","channel_id":"C7","guild_id":"G","author":{"id":"9"}}`))
	if err := d.Deliver("guild:C7", "pong", nil); err != nil {
		t.Fatal(err)
	}
	p := api.sent()[0]
	if p.path != "/channels/C7/messages" || p.body["msg_id"] != "H1" || p.body["msg_seq"] != nil {
		t.Fatalf("guild reply = %+v", p)
	}
	if err := d.Deliver("telegram:1", "x", nil); err == nil {
		t.Fatal("a target with an unknown scene was accepted")
	}
	api.reject = func(string, map[string]any) (int, string) {
		return http.StatusBadRequest, `{"code":40054013,"message":"user refused"}`
	}
	if err := d.Deliver("c2c:U9", "x", nil); err == nil || !strings.Contains(err.Error(), "40054013") {
		t.Fatalf("an API refusal must surface: %v", err)
	}
}

func TestDeliverMediaByURL(t *testing.T) {
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	err := d.Deliver("group:G1", "", []media.Part{{Type: "image", MIME: "image/png", URL: "https://cdn.example.com/a.png"}})
	if err != nil {
		t.Fatal(err)
	}
	posts := api.sent()
	if len(posts) != 2 || posts[0].path != "/v2/groups/G1/files" || posts[0].body["file_type"] != float64(1) || posts[0].body["url"] != "https://cdn.example.com/a.png" {
		t.Fatalf("upload = %+v", posts)
	}
	media7 := posts[1].body
	if media7["msg_type"] != float64(7) || media7["media"].(map[string]any)["file_info"] != "FINFO" {
		t.Fatalf("media message = %+v", media7)
	}
	if err := d.Deliver("c2c:U1", "", []media.Part{{Type: "image", Handle: "media:abc", Name: "chart.png"}}); err == nil || !strings.Contains(err.Error(), "public URL") {
		t.Fatalf("a handle-only attachment must be refused with a reason: %v", err)
	}
	if err := d.Deliver("guild:C1", "", []media.Part{{Type: "video", URL: "https://x/v.mp4"}}); err == nil {
		t.Fatal("a video to a guild channel was accepted")
	}
}

func TestSplitText(t *testing.T) {
	if got := splitText("  \n", 10); got != nil {
		t.Fatalf("blank text = %q", got)
	}
	long := strings.Repeat("word ", 50) + "\n" + strings.Repeat("x", 30)
	chunks := splitText(long, 100)
	joined := 0
	for _, c := range chunks {
		if n := len([]rune(c)); n > 100 {
			t.Fatalf("chunk of %d runes", n)
		}
		joined += len(c)
	}
	if len(chunks) < 3 || joined < len(strings.TrimSpace(long))-len(chunks)*2 {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestDedupForgetsOldIDs(t *testing.T) {
	s := newDedup(2, time.Hour)
	if !s.add("a") || s.add("a") {
		t.Fatal("a repeat was not caught")
	}
	s.add("b")
	s.add("c") // evicts a
	if !s.add("a") {
		t.Fatal("an evicted id still counted as seen")
	}
}

// ---- gateway ------------------------------------------------------------------

// fakeGateway scripts one websocket connection at a time.
type fakeGateway struct {
	t     *testing.T
	srv   *httptest.Server
	conns chan *websocket.Conn
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{t: t, conns: make(chan *websocket.Conn, 4)}
	up := websocket.Upgrader{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		g.conns <- c
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) url() string { return "ws" + strings.TrimPrefix(g.srv.URL, "http") }

func (g *fakeGateway) next() *websocket.Conn {
	g.t.Helper()
	select {
	case c := <-g.conns:
		g.t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(5 * time.Second):
		g.t.Fatal("the driver did not connect")
		return nil
	}
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}

// expect reads frames until one with op arrives (heartbeats in between are
// acknowledged), and returns its d.
func expect(t *testing.T, c *websocket.Conn, op int) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var p struct {
			Op int `json:"op"`
			D  any `json:"d"`
		}
		if err := c.ReadJSON(&p); err != nil {
			t.Fatalf("waiting for op %d: %v", op, err)
		}
		if p.Op == op {
			m, _ := p.D.(map[string]any)
			if m == nil {
				m = map[string]any{"value": p.D}
			}
			return m
		}
		if p.Op == opHeartbeat {
			send(t, c, map[string]any{"op": opHeartbeatAck})
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGatewayIdentifiesHeartbeatsAndResumes(t *testing.T) {
	api := newFakeAPI(t)
	gw := newFakeGateway(t)
	api.gatewayWS = gw.url() // found through GET /gateway/bot
	d, sink, _ := newDriver(t, api, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.runGateway(ctx)

	c := gw.next()
	send(t, c, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 40}})
	id := expect(t, c, opIdentify)
	if id["token"] != "QQBot tok1" || id["intents"] != float64(intentGroupAndC2C|intentPublicGuildMessages) {
		t.Fatalf("identify = %+v", id)
	}
	if shard, _ := id["shard"].([]any); len(shard) != 2 || shard[1] != float64(1) {
		t.Fatalf("shard = %+v", id["shard"])
	}
	send(t, c, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "S1", "user": map[string]any{"id": "B", "username": "bot"}}})
	send(t, c, map[string]any{"op": 0, "s": 2, "t": evC2C, "id": "ev", "d": map[string]any{"id": "m1", "content": "first", "author": map[string]any{"user_openid": "U"}}})
	// The next heartbeat carries the last sequence number.
	hb := expect(t, c, opHeartbeat)
	if hb["value"] != float64(2) {
		t.Fatalf("heartbeat d = %+v, want 2", hb)
	}
	send(t, c, map[string]any{"op": opHeartbeatAck})
	waitFor(t, "the first message", func() bool { return len(sink.all()) == 1 })

	// The server asks for a reconnect: the driver resumes the session.
	send(t, c, map[string]any{"op": opReconnect})
	c2 := gw.next()
	send(t, c2, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 40}})
	res := expect(t, c2, opResume)
	if res["session_id"] != "S1" || res["seq"] != float64(2) || res["token"] != "QQBot tok1" {
		t.Fatalf("resume = %+v", res)
	}
	send(t, c2, map[string]any{"op": 0, "s": 3, "t": "RESUMED", "d": ""})
	send(t, c2, map[string]any{"op": 0, "s": 4, "t": evC2C, "d": map[string]any{"id": "m2", "content": "second", "author": map[string]any{"user_openid": "U"}}})
	// A replayed duplicate after the resume is dropped.
	send(t, c2, map[string]any{"op": 0, "s": 5, "t": evC2C, "d": map[string]any{"id": "m1", "content": "first", "author": map[string]any{"user_openid": "U"}}})
	waitFor(t, "the second message", func() bool { return len(sink.all()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := len(sink.all()); n != 2 {
		t.Fatalf("messages = %d after a replayed duplicate, want 2", n)
	}
}

func TestGatewayInvalidSessionIdentifiesAgain(t *testing.T) {
	gw := newFakeGateway(t)
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.wsURL = gw.url()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.runGateway(ctx)

	c := gw.next()
	send(t, c, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 1000}})
	expect(t, c, opIdentify)
	send(t, c, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "S1"}})
	send(t, c, map[string]any{"op": opInvalidSession, "d": false})
	c2 := gw.next()
	send(t, c2, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 1000}})
	expect(t, c2, opIdentify) // not a resume: the session was invalidated
}

func TestGatewayUnacknowledgedHeartbeatReconnects(t *testing.T) {
	gw := newFakeGateway(t)
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.wsURL = gw.url()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.runGateway(ctx)

	c := gw.next()
	send(t, c, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 30}})
	expect(t, c, opIdentify)
	send(t, c, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "S1"}})
	// Never acknowledge: the driver must give up on this socket and resume.
	c2 := gw.next()
	send(t, c2, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 1000}})
	if res := expect(t, c2, opResume); res["session_id"] != "S1" {
		t.Fatalf("resume = %+v", res)
	}
}

func TestGatewayFatalCloseStops(t *testing.T) {
	gw := newFakeGateway(t)
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.wsURL = gw.url()
	done := make(chan struct{})
	go func() {
		d.runGateway(context.Background())
		close(done)
	}()
	c := gw.next()
	send(t, c, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 1000}})
	expect(t, c, opIdentify)
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4014, "disallowed intents"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a disallowed-intents close should stop the gateway loop")
	}
}

func TestGatewayStopsWithContext(t *testing.T) {
	gw := newFakeGateway(t)
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{})
	d.wsURL = gw.url()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.runGateway(ctx)
		close(done)
	}()
	c := gw.next()
	send(t, c, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 1000}})
	expect(t, c, opIdentify)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway loop outlived its context")
	}
}

func TestDeliverMarkdownInPrivateChatsAndGroups(t *testing.T) {
	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{Markdown: true})
	d.handleEvent(evC2C, "", json.RawMessage(`{"id":"M1","content":"hi","author":{"user_openid":"U1"}}`))
	if err := d.Deliver("c2c:U1", "## Title\n\n**bold**", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver("group:G1", "*hi*", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver("guild:C1", "**plain here**", nil); err != nil {
		t.Fatal(err)
	}
	posts := api.sent()
	if len(posts) != 3 {
		t.Fatalf("posts = %d", len(posts))
	}
	c2c := posts[0].body
	md, _ := c2c["markdown"].(map[string]any)
	if c2c["msg_type"] != float64(2) || md["content"] != "## Title\n\n**bold**" || c2c["content"] != nil {
		t.Fatalf("c2c markdown body = %+v", c2c)
	}
	if c2c["msg_id"] != "M1" || c2c["msg_seq"] != float64(1) {
		t.Fatalf("a markdown reply should still be passive: %+v", c2c)
	}
	if posts[1].body["msg_type"] != float64(2) {
		t.Fatalf("group reply not markdown: %+v", posts[1].body)
	}
	if posts[2].body["msg_type"] != float64(0) || posts[2].body["content"] != "**plain here**" {
		t.Fatalf("guild replies stay plain text: %+v", posts[2].body)
	}
}

func TestMarkdownRefusalFallsBackToTextAndPauses(t *testing.T) {
	api := newFakeAPI(t)
	api.reject = func(path string, body map[string]any) (int, string) {
		if body["msg_type"] == float64(2) {
			return http.StatusBadRequest, `{"code":304023,"message":"markdown not allowed"}`
		}
		return 0, ""
	}
	d, _, _ := newDriver(t, api, Options{Markdown: true})
	if err := d.Deliver("c2c:U1", "**one**", nil); err != nil {
		t.Fatalf("the plain-text fallback should have delivered: %v", err)
	}
	if err := d.Deliver("c2c:U1", "**two**", nil); err != nil {
		t.Fatal(err)
	}
	posts := api.sent()
	if len(posts) != 3 {
		t.Fatalf("posts = %d, want markdown + text fallback + text (paused)", len(posts))
	}
	if posts[0].body["msg_type"] != float64(2) || posts[1].body["content"] != "**one**" || posts[1].body["msg_type"] != float64(0) {
		t.Fatalf("fallback = %+v / %+v", posts[0].body, posts[1].body)
	}
	if posts[2].body["msg_type"] != float64(0) {
		t.Fatalf("markdown should pause after a refusal: %+v", posts[2].body)
	}
}

// A long reply goes out whole when the platform takes it, and is cut smaller
// only as far as the platform makes necessary.
func TestLongMarkdownIsSentWholeWhenThePlatformTakesIt(t *testing.T) {
	long := "here:\n\n```svg\n" + strings.Repeat("<stop offset='0' stop-color='#ffe9a8'/>\n", 150) + "```"
	size := func(body map[string]any) int {
		md, _ := body["markdown"].(map[string]any)
		c, _ := md["content"].(string)
		return len([]rune(c))
	}

	api := newFakeAPI(t)
	d, _, _ := newDriver(t, api, Options{Markdown: true})
	if err := d.Deliver("group:G1", long, nil); err != nil {
		t.Fatal(err)
	}
	if posts := api.sent(); len(posts) != 1 || posts[0].body["msg_type"] != float64(2) {
		t.Fatalf("posts = %d, want the whole reply in one markdown message", len(posts))
	}

	// A platform that takes at most 4000 runes: the whole reply is refused
	// once, then it goes in pieces that fit, each a closed code block.
	api = newFakeAPI(t)
	api.reject = func(path string, body map[string]any) (int, string) {
		if size(body) > 4000 {
			return http.StatusBadRequest, `{"code":40034099,"message":"content too long"}`
		}
		return 0, ""
	}
	d, _, _ = newDriver(t, api, Options{Markdown: true})
	if err := d.Deliver("group:G1", long, nil); err != nil {
		t.Fatal(err)
	}
	posts := api.sent()
	if len(posts) != 4 || size(posts[0].body) <= 4000 {
		t.Fatalf("posts = %d, want one refused attempt and three pieces", len(posts))
	}
	for _, p := range posts[1:] {
		md, _ := p.body["markdown"].(map[string]any)
		c, _ := md["content"].(string)
		if p.body["msg_type"] != float64(2) || size(p.body) > 4000 || strings.Count(c, "```")%2 != 0 {
			t.Fatalf("piece = %d runes, msg_type %v, fences %d", size(p.body), p.body["msg_type"], strings.Count(c, "```"))
		}
	}
	// The limit is remembered: the next long reply is not tried whole again.
	if err := d.Deliver("group:G1", long, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(api.sent()); n != 7 {
		t.Fatalf("posts after the second reply = %d, want 7", n)
	}
}

func TestSplitMarkdownKeepsCodeFencesClosed(t *testing.T) {
	body := strings.Repeat("line of code\n", 40)
	text := "intro\n```lua\n" + body + "```\nafter"
	chunks := splitMarkdown(text, 200)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 200 {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
		fences := 0
		for _, line := range strings.Split(c, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fences++
			}
		}
		if fences%2 != 0 {
			t.Fatalf("chunk %d leaves a fence open:\n%s", i, c)
		}
		if i > 0 && i < len(chunks)-1 && !strings.HasPrefix(c, "```lua") {
			t.Fatalf("chunk %d does not reopen the fence with its language:\n%s", i, c)
		}
	}
}
