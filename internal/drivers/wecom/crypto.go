package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const pkcs7BlockSize = 32

// DecodeAESKey decodes WeCom's 43-character EncodingAESKey (base64 without the
// trailing '=') into a 32-byte AES-256 key.
func DecodeAESKey(encodingAESKey string) ([]byte, error) {
	k := strings.TrimSpace(encodingAESKey)
	if len(k) != 43 {
		return nil, fmt.Errorf("wecom: EncodingAESKey must be 43 characters (got %d)", len(k))
	}
	raw, err := base64.StdEncoding.DecodeString(k + "=")
	if err != nil {
		return nil, fmt.Errorf("wecom: invalid EncodingAESKey: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("wecom: decoded AES key must be 32 bytes (got %d)", len(raw))
	}
	return raw, nil
}

// Signature computes WeCom's SHA-1 callback signature:
// hex(sha1(sort(token, timestamp, nonce, encrypted))).
func Signature(token, timestamp, nonce, encrypted string) string {
	parts := []string{token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// VerifySignature verifies msg_signature using constant-time comparison.
func VerifySignature(token, timestamp, nonce, encrypted, msgSig string) bool {
	if msgSig == "" || token == "" {
		return false
	}
	expected := Signature(token, timestamp, nonce, encrypted)
	return subtle.ConstantTimeCompare([]byte(msgSig), []byte(expected)) == 1
}

// Decrypt decodes and decrypts a WeCom base64 ciphertext, validates PKCS#7
// padding and the trailing ReceiveId against corpID, and returns the payload.
func Decrypt(aesKey []byte, corpID, b64Cipher string) ([]byte, error) {
	if len(aesKey) != 32 {
		return nil, errors.New("wecom: AES key must be 32 bytes")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Cipher))
	if err != nil {
		return nil, fmt.Errorf("wecom: base64 decode: %w", err)
	}
	if len(ciphertext) < aes.BlockSize || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: malformed ciphertext block length")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	iv := aesKey[:aes.BlockSize]
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)

	unpadded, err := pkcs7Unpad32(plain)
	if err != nil {
		return nil, err
	}
	if len(unpadded) < 20 {
		return nil, errors.New("wecom: decrypted envelope too short")
	}
	msgLen := int(binary.BigEndian.Uint32(unpadded[16:20]))
	if msgLen < 0 || 20+msgLen > len(unpadded) {
		return nil, errors.New("wecom: invalid inner message length")
	}
	msg := append([]byte(nil), unpadded[20:20+msgLen]...)
	receiveID := unpadded[20+msgLen:]
	if corpID != "" && subtle.ConstantTimeCompare(receiveID, []byte(corpID)) != 1 {
		return nil, fmt.Errorf("wecom: corp_id mismatch (got %q)", string(receiveID))
	}
	return msg, nil
}

// Encrypt encrypts plaintext into WeCom's base64 AES-256-CBC envelope format.
func Encrypt(aesKey []byte, corpID string, plaintext []byte) (string, error) {
	if len(aesKey) != 32 {
		return "", errors.New("wecom: AES key must be 32 bytes")
	}
	var buf bytes.Buffer
	rnd := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, rnd); err != nil {
		return "", err
	}
	buf.Write(rnd)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(plaintext)))
	buf.Write(lenBuf[:])
	buf.Write(plaintext)
	buf.WriteString(corpID)

	pad := pkcs7BlockSize - (buf.Len() % pkcs7BlockSize)
	if pad == 0 {
		pad = pkcs7BlockSize
	}
	buf.Write(bytes.Repeat([]byte{byte(pad)}, pad))

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	iv := aesKey[:aes.BlockSize]
	ct := make([]byte, buf.Len())
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, buf.Bytes())
	return base64.StdEncoding.EncodeToString(ct), nil
}

func pkcs7Unpad32(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: invalid padded buffer size")
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > pkcs7BlockSize || pad > len(data) {
		return nil, errors.New("wecom: invalid PKCS#7 padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("wecom: corrupt PKCS#7 padding bytes")
		}
	}
	return data[:len(data)-pad], nil
}
