package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

func mediaPolicy() media.Policy { return media.Policy{} }

type captureSink struct{ got router.Inbound }

func (s *captureSink) Submit(in router.Inbound) { s.got = in }

func TestBridgeVerifiesAndSubmits(t *testing.T) {
	sink := &captureSink{}
	srv := httpd.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	d := New("slack", "support", Options{
		Path: "/bridge/", DeliverURL: "http://127.0.0.1:1/deliver", Secret: "s3cret",
	}, sink, srv, nil, mediaPolicy(), slog.Default())
	_ = d

	body := `{"from":"U1","chat_id":"C1","text":"hello"}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/bridge/", strings.NewReader(body))
	req.Header.Set("X-Agentflow-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sink.got.Channel != "slack" || sink.got.Agent != "support" {
		t.Fatalf("inbound = %+v", sink.got)
	}
	if sink.got.Message.From != "user:bridge:U1" || sink.got.Message.ReplyTo != "C1" || sink.got.Message.Text != "hello" {
		t.Fatalf("message = %+v", sink.got.Message)
	}

	// A body without the matching HMAC is refused.
	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/bridge/", strings.NewReader(body))
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d", badResp.StatusCode)
	}
}

func TestBridgeDelivers(t *testing.T) {
	var got outbound
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	srv := httpd.New("", slog.Default())
	d := New("slack", "support", Options{
		Path: "/bridge/", DeliverURL: ts.URL, Secret: "s3cret", Token: "out-token",
	}, &captureSink{}, srv, nil, mediaPolicy(), slog.Default())
	if err := d.Deliver("C1", "reply", nil); err != nil {
		t.Fatal(err)
	}
	if got.To != "C1" || got.Text != "reply" {
		t.Fatalf("outbound = %+v", got)
	}
	if auth != "Bearer out-token" {
		t.Fatalf("auth = %q", auth)
	}
}
