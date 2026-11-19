package qqbot

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// signingKey derives the webhook Ed25519 key from the app secret: the secret
// is repeated until it is at least 32 bytes long and the first 32 bytes are
// the seed (https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html).
func signingKey(secret string) ed25519.PrivateKey {
	seed := secret
	if seed == "" {
		// No secret: a key nobody can match, so verification fails closed.
		seed = "\x00"
	}
	for len(seed) < ed25519.SeedSize {
		seed = strings.Repeat(seed, 2)
	}
	return ed25519.NewKeyFromSeed([]byte(seed[:ed25519.SeedSize]))
}

const (
	headerSignature = "X-Signature-Ed25519"
	headerTimestamp = "X-Signature-Timestamp"
)

// verify checks a callback's signature over timestamp + body.
func (d *Driver) verify(r *http.Request, body []byte) (int, string) {
	sigHex := r.Header.Get(headerSignature)
	ts := r.Header.Get(headerTimestamp)
	if sigHex == "" || ts == "" {
		return http.StatusUnauthorized, "missing signature"
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize || sig[63]&224 != 0 {
		return http.StatusForbidden, "malformed signature"
	}
	msg := make([]byte, 0, len(ts)+len(body))
	msg = append(msg, ts...)
	msg = append(msg, body...)
	if !ed25519.Verify(d.pub, msg, sig) {
		return http.StatusForbidden, "signature mismatch"
	}
	return 0, ""
}

// payload is the envelope of every event, over the webhook and the gateway.
type payload struct {
	ID string          `json:"id,omitempty"`
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Opcodes (https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/opcode.html).
const (
	opDispatch         = 0
	opHeartbeat        = 1
	opIdentify         = 2
	opResume           = 6
	opReconnect        = 7
	opInvalidSession   = 9
	opHello            = 10
	opHeartbeatAck     = 11
	opCallbackAck      = 12
	opCallbackValidate = 13
)

// maxWebhookBodyBytes bounds one callback body.
const maxWebhookBodyBytes = 1 << 20

// handleWebhook receives one callback. The signature is checked before the
// body is parsed — the URL-validation handshake included, because answering an
// unsigned one would sign text of the caller's choosing with the bot's key.
func (d *Driver) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if status, msg := d.verify(r, body); status != 0 {
		d.log.Warn("qq webhook rejected", "reason", msg, "remote", r.RemoteAddr)
		http.Error(w, msg, status)
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch p.Op {
	case opCallbackValidate:
		var v struct {
			PlainToken string `json:"plain_token"`
			EventTS    string `json:"event_ts"`
		}
		if err := json.Unmarshal(p.D, &v); err != nil || v.PlainToken == "" {
			http.Error(w, "bad validation payload", http.StatusBadRequest)
			return
		}
		sig := ed25519.Sign(d.priv, []byte(v.EventTS+v.PlainToken))
		_ = json.NewEncoder(w).Encode(map[string]string{
			"plain_token": v.PlainToken,
			"signature":   hex.EncodeToString(sig),
		})
		d.log.Info("qq webhook callback URL validated")
	case opDispatch:
		d.handleEvent(p.T, p.ID, p.D)
		_ = json.NewEncoder(w).Encode(map[string]int{"op": opCallbackAck})
	default:
		_ = json.NewEncoder(w).Encode(map[string]int{"op": opCallbackAck})
	}
}
