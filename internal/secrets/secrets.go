package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// Box encrypts and decrypts secret values using AES-256-GCM with a
// caller-supplied master key. The master key must be exactly 32 bytes,
// supplied as a standard base64-encoded string (e.g. from
// `head -c 32 /dev/urandom | base64`).
//
// Each call to Encrypt uses a fresh random nonce; the nonce is prepended to
// the ciphertext before base64 encoding so Decrypt can recover it.
// No key material or plaintext is ever logged.
type Box struct{ gcm cipher.AEAD }

// NewBox creates a Box from a base64-encoded 32-byte master key.
func NewBox(masterKeyB64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("master key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Encrypt encrypts plain with AES-256-GCM and returns a base64 string
// containing [nonce || ciphertext || tag].
func (b *Box) Encrypt(plain []byte) (string, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := b.gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt decrypts an enc string produced by Encrypt. Returns an error if
// the input is malformed, too short, or authentication fails (e.g. wrong key
// or tampered ciphertext).
func (b *Box) Decrypt(enc string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	ns := b.gcm.NonceSize()
	if len(raw) < ns+b.gcm.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return b.gcm.Open(nil, raw[:ns], raw[ns:], nil)
}
