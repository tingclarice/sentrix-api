// Package pushtest is a test helper that plays the browser's part of Web Push: it
// creates a subscription and decrypts what the server sends (RFC 8291, aes128gcm).
// It is only imported by tests.
package pushtest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// Browser holds the keys a real browser creates in pushManager.subscribe().
type Browser struct {
	priv   *ecdh.PrivateKey
	secret []byte
	P256dh string // base64url, what the browser sends to the server
	Auth   string
}

func NewBrowser(t testing.TB) *Browser {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return &Browser{priv: priv, secret: secret, P256dh: enc(priv.PublicKey().Bytes()), Auth: enc(secret)}
}

func expand(prk, info []byte, n int) ([]byte, error) {
	out := make([]byte, n)
	_, err := io.ReadFull(hkdf.Expand(sha256.New, prk, info), out)
	return out, err
}

// Decrypt returns the plaintext of an aes128gcm Web Push request body.
func (b *Browser) Decrypt(body []byte) ([]byte, error) {
	// header: salt(16) | record size(4) | key id length(1) | key id (the sender's public key)
	if len(body) < 21 {
		return nil, errors.New("body too short")
	}
	salt := body[:16]
	idLen := int(body[20])
	if len(body) < 21+idLen {
		return nil, errors.New("body shorter than its header")
	}
	senderPub := body[21 : 21+idLen]
	ciphertext := body[21+idLen:]
	_ = binary.BigEndian.Uint32(body[16:20]) // record size, unused for a single record

	sender, err := ecdh.P256().NewPublicKey(senderPub)
	if err != nil {
		return nil, err
	}
	shared, err := b.priv.ECDH(sender)
	if err != nil {
		return nil, err
	}

	keyInfo := append([]byte("WebPush: info\x00"), b.priv.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, senderPub...)
	ikm, err := expand(hkdf.Extract(sha256.New, shared, b.secret), keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek, err := expand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	if err != nil {
		return nil, err
	}
	nonce, err := expand(prk, []byte("Content-Encoding: nonce\x00"), 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	// Strip the padding: zeros, preceded by a 0x02 delimiter on the last record.
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, errors.New("missing padding delimiter")
	}
	return plain[:len(plain)-1], nil
}
