package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
)

type testSink struct {
	mu   sync.Mutex
	msgs []router.Inbound
	ch   chan router.Inbound
}

func newTestSink() *testSink {
	return &testSink{ch: make(chan router.Inbound, 16)}
}

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

func encryptTestPayload(t *testing.T, encryptKey string, plain []byte) string {
	t.Helper()
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	iv := []byte("0123456789abcdef")
	pad := aes.BlockSize - (len(plain) % aes.BlockSize)
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return base64.StdEncoding.EncodeToString(append(iv, ct...))
}

func TestPBBP2FrameRoundTrip(t *testing.T) {
	in := pbbp2Frame{
		SeqID:           42,
		LogID:           99,
		Service:         7,
		Method:          frameMethodData,
		PayloadEncoding: "json",
		PayloadType:     "event",
		Payload:         []byte(`{"hello":"world"}`),
		LogIDNew:        "log-42",
		Headers: []pbbp2Header{
			{Key: "type", Value: "event"},
			{Key: "message_id", Value: "msg-1"},
		},
	}
	out, err := unmarshalFrame(in.marshal())
	if err != nil {
		t.Fatalf("unmarshalFrame: %v", err)
	}
	if out.SeqID != in.SeqID || out.LogID != in.LogID || out.Service != in.Service || out.Method != in.Method {
		t.Fatalf("scalar mismatch: got %+v, want %+v", out, in)
	}
	if string(out.Payload) != string(in.Payload) || out.LogIDNew != in.LogIDNew || out.header("type") != "event" {
		t.Fatalf("body/header mismatch: got %+v", out)
	}
}

func TestWebhookEncryptedChallengeAndInboundMessage(t *testing.T) {
	sink := newTestSink()
	var tokenCalls atomic.Int32
	var downloaded atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-1","expire":7200}`))
		case "/open-apis/bot/v3/info":
			_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_bot"}}`))
		case "/open-apis/im/v1/messages/om_img/resources/img_key_1":
			downloaded.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("fs", "coder", Options{
		AppID:      "cli_a",
		Secret:     "sec_a",
		Token:      "vtoken",
		EncryptKey: "enc-key-123",
		Mode:       "webhook",
		APIBase:    apiSrv.URL,
	}, sink, nil, store, media.Policy{Allow: []string{"image/*"}, MaxBytes: 1 << 20}, discardLog())

	// 1. Reject missing signature when encrypt_key is configured.
	req := httptest.NewRequest(http.MethodPost, "/webhook/feishu/", strings.NewReader(`{"encrypt":"abc"}`))
	rec := httptest.NewRecorder()
	d.handleWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook status = %d; want 401", rec.Code)
	}

	// 2. Encrypted URL verification challenge.
	challengePlain, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"challenge": "ch_xyz",
		"token":     "vtoken",
	})
	encBody, _ := json.Marshal(map[string]string{
		"encrypt": encryptTestPayload(t, "enc-key-123", challengePlain),
	})
	ts, nonce := "1700000000", "nonce1"
	req = httptest.NewRequest(http.MethodPost, "/webhook/feishu/", bytes.NewReader(encBody))
	req.Header.Set(headerTimestamp, ts)
	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSignature, computeSignature(ts, nonce, "enc-key-123", encBody))
	rec = httptest.NewRecorder()
	d.handleWebhook(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ch_xyz") {
		t.Fatalf("challenge response = %d %s", rec.Code, rec.Body.String())
	}

	// 3. Encrypted group @mention event + duplicate suppression.
	groupEvent := map[string]any{
		"schema": "2.0",
		"header": map[string]string{
			"event_id":   "ev-1",
			"event_type": "im.message.receive_v1",
			"token":      "vtoken",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_type": "user",
				"sender_id":   map[string]string{"open_id": "ou_alice", "user_id": "u1"},
			},
			"message": map[string]any{
				"message_id":   "om_1",
				"chat_id":      "oc_grp",
				"chat_type":    "group",
				"message_type": "text",
				"content":      `{"text":"@_user_1 hello @_user_2"}`,
				"mentions": []map[string]any{
					{"key": "@_user_1", "id": map[string]string{"open_id": "ou_bot"}, "name": "Bot"},
					{"key": "@_user_2", "id": map[string]string{"open_id": "ou_bob"}, "name": "Bob"},
				},
			},
		},
	}
	evPlain, _ := json.Marshal(groupEvent)
	evBody, _ := json.Marshal(map[string]string{
		"encrypt": encryptTestPayload(t, "enc-key-123", evPlain),
	})
	for i := 0; i < 2; i++ {
		req = httptest.NewRequest(http.MethodPost, "/webhook/feishu/", bytes.NewReader(evBody))
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerNonce, nonce)
		req.Header.Set(headerSignature, computeSignature(ts, nonce, "enc-key-123", evBody))
		rec = httptest.NewRecorder()
		d.handleWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("event webhook status = %d", rec.Code)
		}
	}
	select {
	case in := <-sink.ch:
		if in.Message.From != "user:feishu:ou_alice" || in.Message.ReplyTo != "chat:oc_grp" {
			t.Fatalf("inbound addressing = %+v", in.Message)
		}
		if in.Message.Text != "hello @Bob" {
			t.Fatalf("resolved text = %q; want %q", in.Message.Text, "hello @Bob")
		}
		if in.Message.Payload["mentioned"] != true {
			t.Fatalf("expected mentioned=true, payload=%+v", in.Message.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
	time.Sleep(50 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("expected dedup count 1, got %d", sink.count())
	}

	// 4. Non-mentioned group message dropped unless group_all is configured.
	d.handleEventJSON([]byte(`{
		"schema":"2.0",
		"header":{"event_id":"ev-2","event_type":"im.message.receive_v1"},
		"event":{
			"sender":{"sender_type":"user","sender_id":{"open_id":"ou_alice"}},
			"message":{"message_id":"om_ignored","chat_id":"oc_grp","chat_type":"group","message_type":"text","content":"{\"text\":\"overheard\"}"}
		}
	}`))
	if sink.count() != 1 {
		t.Fatalf("unmentioned group message should be ignored; count=%d", sink.count())
	}

	// 5. Image resource download into media store.
	d.handleEventJSON([]byte(`{
		"schema":"2.0",
		"header":{"event_id":"ev-3","event_type":"im.message.receive_v1"},
		"event":{
			"sender":{"sender_type":"user","sender_id":{"open_id":"ou_alice"}},
			"message":{"message_id":"om_img","chat_id":"oc_p2p","chat_type":"p2p","message_type":"image","content":"{\"image_key\":\"img_key_1\"}"}
		}
	}`))
	select {
	case in := <-sink.ch:
		if len(in.Message.Attachments) != 1 || in.Message.Attachments[0].MIME != "image/png" {
			t.Fatalf("attachments = %+v", in.Message.Attachments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for image inbound")
	}
	if downloaded.Load() != 1 || tokenCalls.Load() != 1 {
		t.Fatalf("downloaded=%d tokenCalls=%d", downloaded.Load(), tokenCalls.Load())
	}
}

func TestWebSocketGatewayReassemblesChunksAndACKs(t *testing.T) {
	sink := newTestSink()
	ackCh := make(chan pbbp2Frame, 2)
	pingCh := make(chan pbbp2Frame, 2)
	upgrader := websocket.Upgrader{}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback/ws/endpoint":
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?service_id=9"
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"URL":"` + wsURL + `","ClientConfig":{"PingInterval":30}}}`))
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var wsWriteMu sync.Mutex
			writeBin := func(b []byte) error {
				wsWriteMu.Lock()
				defer wsWriteMu.Unlock()
				return conn.WriteMessage(websocket.BinaryMessage, b)
			}
			go func() {
				for {
					mt, raw, err := conn.ReadMessage()
					if err != nil || mt != websocket.BinaryMessage {
						return
					}
					f, err := unmarshalFrame(raw)
					if err != nil {
						return
					}
					switch f.Method {
					case frameMethodControl:
						pingCh <- f
						_ = writeBin(pbbp2Frame{
							Service: f.Service,
							Method:  frameMethodControl,
							Headers: []pbbp2Header{{Key: "type", Value: "pong"}},
							Payload: []byte(`{"PingInterval":60}`),
						}.marshal())
					case frameMethodData:
						ackCh <- f
					}
				}
			}()
			ev := `{"schema":"2.0","header":{"event_id":"ev_ws","event_type":"im.message.receive_v1"},"event":{"sender":{"sender_type":"user","sender_id":{"open_id":"ou_ws"}},"message":{"message_id":"om_ws1","chat_id":"oc_ws1","chat_type":"p2p","message_type":"text","content":"{\"text\":\"ws hello\"}"}}}`
			mid := len(ev) / 2
			parts := [][]byte{[]byte(ev[:mid]), []byte(ev[mid:])}
			for idx, part := range parts {
				_ = writeBin(pbbp2Frame{
					SeqID:   uint64(10 + idx),
					LogID:   500,
					Service: 9,
					Method:  frameMethodData,
					Headers: []pbbp2Header{
						{Key: "type", Value: "event"},
						{Key: "message_id", Value: "chunk_1"},
						{Key: "sum", Value: "2"},
						{Key: "seq", Value: string(rune('0' + idx))},
					},
					Payload: part,
				}.marshal())
			}
			time.Sleep(400 * time.Millisecond)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := New("fs-ws", "coder", Options{
		AppID:   "cli_ws",
		Secret:  "sec_ws",
		APIBase: srv.URL,
	}, sink, nil, nil, media.Policy{}, discardLog())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case pf := <-pingCh:
		if pf.Service != 9 || pf.header("type") != "ping" {
			t.Fatalf("unexpected ping frame: %+v", pf)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected control ping frame")
	}

	select {
	case af := <-ackCh:
		if string(af.Payload) != `{"code":200}` || af.header("biz_rt") == "" {
			t.Fatalf("unexpected ACK frame: payload=%q biz_rt=%q", af.Payload, af.header("biz_rt"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected binary ACK frame")
	}

	select {
	case in := <-sink.ch:
		if in.Message.Text != "ws hello" || in.Message.From != "user:feishu:ou_ws" {
			t.Fatalf("unexpected ws inbound: %+v", in.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected assembled ws message")
	}
}

func TestDeliverTokenRetryMarkdownFallbackAndMediaUpload(t *testing.T) {
	var tokenIdx atomic.Int32
	var cardRejected atomic.Int32
	var sentTypes []string
	var mu sync.Mutex
	var imageUploaded, fileUploaded bool

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			n := tokenIdx.Add(1)
			tok := "tok-old"
			if n > 1 {
				tok = "tok-fresh"
			}
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"` + tok + `","expire":7200}`))
		case r.URL.Path == "/open-apis/im/v1/messages":
			if r.Header.Get("Authorization") == "Bearer tok-old" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":99991663,"msg":"token expired"}`))
				return
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
				Content   string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.MsgType == "interactive" {
				cardRejected.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":230001,"msg":"card unsupported"}`))
				return
			}
			mu.Lock()
			sentTypes = append(sentTypes, body.MsgType)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_out"}}`))
		case r.URL.Path == "/open-apis/im/v1/images":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("image_type") != "message" {
				t.Fatalf("unexpected image_type %q", r.FormValue("image_type"))
			}
			imageUploaded = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"image_key":"img_out_1"}}`))
		case r.URL.Path == "/open-apis/im/v1/files":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("file_type") != "stream" {
				t.Fatalf("unexpected file_type %q", r.FormValue("file_type"))
			}
			fileUploaded = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"file_key":"file_out_1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("fs", "coder", Options{
		AppID:    "cli_1",
		Secret:   "sec_1",
		APIBase:  apiSrv.URL,
		Markdown: true,
	}, newTestSink(), nil, store, media.Policy{}, discardLog())

	err = d.Deliver("chat:oc_chat1", "hello markdown", []media.Part{
		{Type: "image", MIME: "image/png", Name: "shot.png", Data: base64.StdEncoding.EncodeToString([]byte("img"))},
		{Type: "file", MIME: "application/pdf", Name: "doc.pdf", Data: base64.StdEncoding.EncodeToString([]byte("pdf"))},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if cardRejected.Load() != 1 || tokenIdx.Load() != 2 || !imageUploaded || !fileUploaded {
		t.Fatalf("cardRejected=%d tokenIdx=%d image=%v file=%v", cardRejected.Load(), tokenIdx.Load(), imageUploaded, fileUploaded)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(sentTypes, ",") != "text,image,file" {
		t.Fatalf("sent msg_types = %v; want [text image file]", sentTypes)
	}
}
