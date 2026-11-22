package wecom

import (
	"encoding/xml"
	"io"
	"net/http"
)

const maxWebhookBodyBytes = 1 << 20

type xmlEnvelope struct {
	ToUserName string `xml:"ToUserName"`
	Encrypt    string `xml:"Encrypt"`
	AgentID    string `xml:"AgentID"`
}

func (d *Driver) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if d.keyErr != nil {
		http.Error(w, "wecom AES key misconfigured", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	msgSig := q.Get("msg_signature")
	ts := q.Get("timestamp")
	nonce := q.Get("nonce")

	switch r.Method {
	case http.MethodGet:
		echostr := q.Get("echostr")
		if echostr == "" {
			http.Error(w, "missing echostr", http.StatusBadRequest)
			return
		}
		if !VerifySignature(d.token, ts, nonce, echostr, msgSig) {
			d.log.Warn("wecom echostr signature mismatch", "remote", r.RemoteAddr)
			http.Error(w, "signature mismatch", http.StatusForbidden)
			return
		}
		plain, err := Decrypt(d.aesKey, d.corpID, echostr)
		if err != nil {
			d.log.Warn("wecom echostr decrypt failed", "err", err)
			http.Error(w, "decrypt failed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(plain)
		d.log.Info("wecom callback URL verified")

	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if len(body) > maxWebhookBodyBytes {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var env xmlEnvelope
		if err := xml.Unmarshal(body, &env); err != nil || env.Encrypt == "" {
			http.Error(w, "bad xml envelope", http.StatusBadRequest)
			return
		}
		if !VerifySignature(d.token, ts, nonce, env.Encrypt, msgSig) {
			d.log.Warn("wecom callback signature mismatch", "remote", r.RemoteAddr)
			http.Error(w, "signature mismatch", http.StatusForbidden)
			return
		}
		plain, err := Decrypt(d.aesKey, d.corpID, env.Encrypt)
		if err != nil {
			d.log.Warn("wecom callback decrypt failed", "err", err)
			http.Error(w, "decrypt failed", http.StatusForbidden)
			return
		}
		go d.handleInboundXML(plain)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}
