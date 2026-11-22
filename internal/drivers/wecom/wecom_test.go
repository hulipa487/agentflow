package wecom

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
)

const testAESKeyStr = "jWmYm7qr5nMoCAstdJ4v4eM9cX7B1v9P4Y6k8z0q1wE" // 43 chars -> 32 bytes

type testSink struct {
	mu   sync.Mutex
	msgs []router.Inbound
	ch   chan router.Inbound
}

func newTestSink() *testSink { return &testSink{ch: make(chan router.Inbound, 16)} }

func (s *testSink) Submit(in router.Inbound) {
	s.mu.Lock()
	s.msgs = append(s.msgs, in)
	s.mu.Unlock()
	select {
	case s.ch <- in:
	default:
	}
}

func (s *testSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCryptoRoundTripAndValidation(t *testing.T) {
	key, err := DecodeAESKey(testAESKeyStr)
	if err != nil {
		t.Fatalf("DecodeAESKey: %v", err)
	}
	enc, err := Encrypt(key, "wwCorp123", []byte("<xml><Content>hi</Content></xml>"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plain, err := Decrypt(key, "wwCorp123", enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plain) != "<xml><Content>hi</Content></xml>" {
		t.Fatalf("decrypted = %q", plain)
	}
	if _, err := Decrypt(key, "wwWrong", enc); err == nil {
		t.Fatal("expected corp_id mismatch error")
	}
	sig := Signature("tok", "123", "456", enc)
	if !VerifySignature("tok", "123", "456", enc, sig) {
		t.Fatal("VerifySignature returned false for valid signature")
	}
	if VerifySignature("tok", "123", "456", enc, "bad") {
		t.Fatal("VerifySignature accepted bad signature")
	}
}

func TestWebhookURLVerificationAndInboundMessages(t *testing.T) {
	sink := newTestSink()
	var imgDownloads atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"wc-tok-1","expires_in":7200}`))
		case "/cgi-bin/media/get":
			imgDownloads.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("jpeg-data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("wc", "coder", Options{
		CorpID:     "wwCorp1",
		AgentID:    1000002,
		Secret:     "sec",
		Token:      "cb-token",
		EncryptKey: testAESKeyStr,
		APIBase:    apiSrv.URL,
	}, sink, nil, store, media.Policy{Allow: []string{"image/*"}, MaxBytes: 1 << 20}, discardLog())

	aesKey, _ := DecodeAESKey(testAESKeyStr)
	encEcho, err := Encrypt(aesKey, "wwCorp1", []byte("echo_challenge_xyz"))
	if err != nil {
		t.Fatal(err)
	}
	ts, nonce := "1700000000", "n123"
	echoSig := Signature("cb-token", ts, nonce, encEcho)

	// 1. GET URL verification.
	u := fmt.Sprintf("/webhook/wecom/?msg_signature=%s&timestamp=%s&nonce=%s&echostr=%s",
		url.QueryEscape(echoSig), ts, nonce, url.QueryEscape(encEcho))
	rec := httptest.NewRecorder()
	d.handleWebhook(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "echo_challenge_xyz" {
		t.Fatalf("GET echostr = %d %q", rec.Code, rec.Body.String())
	}

	// 2. GET bad signature rejected.
	badU := fmt.Sprintf("/webhook/wecom/?msg_signature=bad&timestamp=%s&nonce=%s&echostr=%s",
		ts, nonce, url.QueryEscape(encEcho))
	rec = httptest.NewRecorder()
	d.handleWebhook(rec, httptest.NewRequest(http.MethodGet, badU, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad echostr status = %d; want 403", rec.Code)
	}

	// 3. POST encrypted text + duplicate drop.
	innerText := `<xml><ToUserName><![CDATA[wwCorp1]]></ToUserName><FromUserName><![CDATA[ZhangSan]]></FromUserName><CreateTime>1700000001</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hello wecom]]></Content><MsgId>1001</MsgId><AgentID>1000002</AgentID></xml>`
	encText, _ := Encrypt(aesKey, "wwCorp1", []byte(innerText))
	postXML := fmt.Sprintf(`<xml><ToUserName><![CDATA[wwCorp1]]></ToUserName><Encrypt><![CDATA[%s]]></Encrypt></xml>`, encText)
	postSig := Signature("cb-token", ts, nonce, encText)
	postURL := fmt.Sprintf("/webhook/wecom/?msg_signature=%s&timestamp=%s&nonce=%s", postSig, ts, nonce)

	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		d.handleWebhook(rec, httptest.NewRequest(http.MethodPost, postURL, strings.NewReader(postXML)))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST webhook status = %d %s", rec.Code, rec.Body.String())
		}
	}
	select {
	case in := <-sink.ch:
		if in.Message.From != "user:wecom:ZhangSan" || in.Message.ReplyTo != "user:ZhangSan" || in.Message.Text != "hello wecom" {
			t.Fatalf("unexpected inbound message: %+v", in.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for wecom inbound")
	}
	time.Sleep(50 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("expected dedup count 1, got %d", sink.count())
	}

	// 4. Image message downloads attachment into media store.
	innerImg := `<xml><ToUserName><![CDATA[wwCorp1]]></ToUserName><FromUserName><![CDATA[ZhangSan]]></FromUserName><CreateTime>1700000002</CreateTime><MsgType><![CDATA[image]]></MsgType><MediaId><![CDATA[MEDIA_99]]></MediaId><MsgId>1002</MsgId><AgentID>1000002</AgentID></xml>`
	encImg, _ := Encrypt(aesKey, "wwCorp1", []byte(innerImg))
	postImg := fmt.Sprintf(`<xml><ToUserName><![CDATA[wwCorp1]]></ToUserName><Encrypt><![CDATA[%s]]></Encrypt></xml>`, encImg)
	sigImg := Signature("cb-token", ts, nonce, encImg)
	rec = httptest.NewRecorder()
	d.handleWebhook(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/webhook/wecom/?msg_signature=%s&timestamp=%s&nonce=%s", sigImg, ts, nonce), strings.NewReader(postImg)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST image status = %d", rec.Code)
	}
	select {
	case in := <-sink.ch:
		if len(in.Message.Attachments) != 1 || in.Message.Attachments[0].MIME != "image/jpeg" {
			t.Fatalf("image attachments = %+v", in.Message.Attachments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for image inbound")
	}
	if imgDownloads.Load() != 1 {
		t.Fatalf("expected 1 media download, got %d", imgDownloads.Load())
	}

	// 5. Voice recognition text + event ignored.
	d.handleInboundXML([]byte(`<xml><FromUserName>LiSi</FromUserName><MsgType>event</MsgType><Event>enter_agent</Event></xml>`))
	d.handleInboundXML([]byte(`<xml><FromUserName>LiSi</FromUserName><MsgType>voice</MsgType><Recognition>transcribed voice</Recognition><MsgId>1003</MsgId></xml>`))
	select {
	case in := <-sink.ch:
		if in.Message.Text != "transcribed voice" || in.Message.From != "user:wecom:LiSi" {
			t.Fatalf("voice inbound = %+v", in.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for voice inbound")
	}
}

func TestDeliverByteSplittingTokenRetryMarkdownFallbackAndMedia(t *testing.T) {
	var tokenIdx atomic.Int32
	var mdRejected atomic.Int32
	var sentTypes []string
	var sentContents []string
	var usedChatEndpoint bool
	var mu sync.Mutex

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			n := tokenIdx.Add(1)
			tok := "tok-1"
			if n > 1 {
				tok = "tok-2"
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"` + tok + `","expires_in":7200}`))
		case "/cgi-bin/message/send", "/cgi-bin/appchat/send":
			if r.URL.Query().Get("access_token") == "tok-1" {
				_, _ = w.Write([]byte(`{"errcode":42001,"errmsg":"access_token expired"}`))
				return
			}
			if r.URL.Path == "/cgi-bin/appchat/send" {
				usedChatEndpoint = true
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mt, _ := body["msgtype"].(string)
			if mt == "markdown" {
				mdRejected.Add(1)
				_, _ = w.Write([]byte(`{"errcode":60020,"errmsg":"markdown unsupported"}`))
				return
			}
			mu.Lock()
			sentTypes = append(sentTypes, mt)
			if mt == "text" {
				if sub, ok := body["text"].(map[string]any); ok {
					sentContents = append(sentContents, fmt.Sprint(sub["content"]))
				}
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		case "/cgi-bin/media/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.FormFile("media"); err != nil {
				t.Fatalf("missing multipart media field: %v", err)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","media_id":"MID_123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("wc", "coder", Options{
		CorpID:     "ww1",
		AgentID:    1000002,
		Secret:     "sec",
		Token:      "tok",
		EncryptKey: testAESKeyStr,
		APIBase:    apiSrv.URL,
		Markdown:   true,
	}, newTestSink(), nil, store, media.Policy{}, discardLog())

	// 1. CJK string exceeding 2048 UTF-8 bytes (1000 Chinese chars = 3000 bytes).
	longCJK := strings.Repeat("测", 1000)
	if err := d.Deliver("user:ZhangSan", longCJK, nil); err != nil {
		t.Fatalf("Deliver long CJK: %v", err)
	}
	if mdRejected.Load() != 1 || tokenIdx.Load() != 2 {
		t.Fatalf("mdRejected=%d tokenIdx=%d", mdRejected.Load(), tokenIdx.Load())
	}
	mu.Lock()
	for idx, c := range sentContents {
		if len(c) > maxMessageBytes || !utf8.ValidString(c) {
			t.Fatalf("chunk %d invalid: len=%d validUTF8=%v", idx, len(c), utf8.ValidString(c))
		}
	}
	mu.Unlock()

	// 2. Chat endpoint + image attachment upload.
	if err := d.Deliver("chat:grp1", "hi chat", []media.Part{
		{Type: "image", MIME: "image/png", Name: "pic.png", Data: base64.StdEncoding.EncodeToString([]byte("png"))},
	}); err != nil {
		t.Fatalf("Deliver chat media: %v", err)
	}
	if !usedChatEndpoint {
		t.Fatal("expected /cgi-bin/appchat/send to be called for chat: target")
	}
	mu.Lock()
	defer mu.Unlock()
	last := sentTypes[len(sentTypes)-2:]
	if strings.Join(last, ",") != "text,image" {
		t.Fatalf("last sent msgtypes = %v; want [text image]", last)
	}
}
