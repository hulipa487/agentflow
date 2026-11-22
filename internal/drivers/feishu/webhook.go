package feishu

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const (
	headerTimestamp = "X-Lark-Request-Timestamp"
	headerNonce     = "X-Lark-Request-Nonce"
	headerSignature = "X-Lark-Signature"

	maxWebhookBodyBytes = 1 << 20
)

// computeSignature computes Feishu's SHA-256 webhook signature:
// sha256(timestamp + nonce + encrypt_key + body).
func computeSignature(timestamp, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (d *Driver) verifySignature(r *http.Request, body []byte) (int, string) {
	if d.encryptKey == "" {
		return 0, ""
	}
	sig := r.Header.Get(headerSignature)
	ts := r.Header.Get(headerTimestamp)
	nonce := r.Header.Get(headerNonce)
	if sig == "" || ts == "" || nonce == "" {
		return http.StatusUnauthorized, "missing signature"
	}
	expected := computeSignature(ts, nonce, d.encryptKey, body)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return http.StatusForbidden, "signature mismatch"
	}
	return 0, ""
}

// decryptPayload decrypts Feishu's base64-encoded AES-256-CBC callback body.
// Key is sha256(encryptKey); the first 16 decoded bytes are the IV.
func decryptPayload(encryptKey, b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(raw) < aes.BlockSize*2 || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("feishu: malformed ciphertext length")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	iv := raw[:aes.BlockSize]
	ct := raw[aes.BlockSize:]
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	return pkcs7Unpad(plain, aes.BlockSize)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("feishu: invalid padded length")
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > blockSize || pad > len(data) {
		return nil, errors.New("feishu: invalid padding value")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("feishu: corrupt padding bytes")
		}
	}
	return data[:len(data)-pad], nil
}

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

	var encrypted struct {
		Encrypt string `json:"encrypt"`
	}
	_ = json.Unmarshal(body, &encrypted)

	// Verify X-Lark-Signature when present, or always when encrypted.
	if encrypted.Encrypt != "" || r.Header.Get(headerSignature) != "" {
		if d.encryptKey == "" {
			http.Error(w, "encrypt_key not configured", http.StatusForbidden)
			return
		}
		if status, msg := d.verifySignature(r, body); status != 0 {
			d.log.Warn("feishu webhook rejected", "reason", msg, "remote", r.RemoteAddr)
			http.Error(w, msg, status)
			return
		}
	}

	plain := body
	if encrypted.Encrypt != "" {
		plain, err = decryptPayload(d.encryptKey, encrypted.Encrypt)
		if err != nil {
			d.log.Warn("feishu webhook decrypt failed", "err", err)
			http.Error(w, "decrypt failed", http.StatusForbidden)
			return
		}
	}

	var envelope struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Token     string `json:"token"`
		Header    struct {
			Token string `json:"token"`
		} `json:"header"`
	}
	if err := json.Unmarshal(plain, &envelope); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	tok := envelope.Token
	if tok == "" {
		tok = envelope.Header.Token
	}
	if d.verifyToken != "" {
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(d.verifyToken)) != 1 {
			d.log.Warn("feishu webhook token mismatch", "remote", r.RemoteAddr)
			http.Error(w, "token mismatch", http.StatusForbidden)
			return
		}
	} else if d.encryptKey == "" {
		// Neither encrypt_key nor verification token is configured: fail closed.
		http.Error(w, "webhook authentication not configured", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if envelope.Type == "url_verification" {
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		d.log.Info("feishu webhook url_verification succeeded")
		return
	}
	go d.handleEventJSON(plain)
	_ = json.NewEncoder(w).Encode(map[string]int{"code": 0})
}
