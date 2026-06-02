package secrets_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/secrets"
)

// testKeyB64 returns a base64-encoded 32-byte key (all zeros, valid for tests).
func testKeyB64() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

// altKeyB64 returns a different 32-byte key (all ones).
func altKeyB64() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xff
	}
	return base64.StdEncoding.EncodeToString(key)
}

// --- 1. Round-trip -----------------------------------------------------------

func TestRoundTrip_NonEmptyPlaintext(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	plain := []byte("super secret value 🔒")
	enc, err := box.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := box.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plain)
	}
}

func TestRoundTrip_EmptyPlaintext(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	plain := []byte{}
	enc, err := box.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := box.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	// Decrypt of empty may return nil or empty slice; both are acceptable.
	if len(got) != 0 {
		t.Fatalf("expected empty plaintext back, got %q", got)
	}
}

// --- 2. Nonce randomization --------------------------------------------------

func TestNonceRandomization(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	plain := []byte("same plaintext, different nonce each time")
	enc1, err := box.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	enc2, err := box.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if enc1 == enc2 {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext (nonce not random)")
	}
	// Both must still decrypt correctly.
	got1, err := box.Decrypt(enc1)
	if err != nil {
		t.Fatalf("Decrypt 1: %v", err)
	}
	got2, err := box.Decrypt(enc2)
	if err != nil {
		t.Fatalf("Decrypt 2: %v", err)
	}
	if !bytes.Equal(got1, plain) || !bytes.Equal(got2, plain) {
		t.Fatal("decrypted values did not match original plaintext")
	}
}

// --- 3. Wrong key fails ------------------------------------------------------

func TestWrongKeyFails(t *testing.T) {
	boxA, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox A: %v", err)
	}
	boxB, err := secrets.NewBox(altKeyB64())
	if err != nil {
		t.Fatalf("NewBox B: %v", err)
	}
	enc, err := boxA.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := boxB.Decrypt(enc); err == nil {
		t.Fatal("expected error decrypting with wrong key, got nil")
	}
}

// --- 4. Tampered ciphertext fails --------------------------------------------

func TestTamperedCiphertextFails(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	enc, err := box.Encrypt([]byte("tamper me"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	// Flip the last byte (in the ciphertext/tag region, past the nonce).
	raw[len(raw)-1] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(raw)

	got, err := box.Decrypt(tampered)
	if err == nil {
		t.Fatalf("expected error decrypting tampered ciphertext, got: %q", got)
	}
}

// --- 5. Short ciphertext fails -----------------------------------------------

func TestShortCiphertextFails(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	// A base64 of just 3 bytes - shorter than any nonce size (GCM nonce = 12).
	short := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03})
	_, err = box.Decrypt(short)
	if err == nil {
		t.Fatal("expected error for short ciphertext, got nil")
	}
	if !strings.Contains(err.Error(), "ciphertext too short") {
		t.Fatalf("expected 'ciphertext too short' error, got: %v", err)
	}
}

// TestBoundaryNonceSizedCiphertext checks the length guard at exactly the nonce
// size (no room for ciphertext+tag), which must still report "ciphertext too short"
// rather than falling through to an opaque stdlib auth error.
func TestBoundaryNonceSizedCiphertext(t *testing.T) {
	box, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	// GCM standard nonce size is 12 bytes; supply exactly that, no tag.
	exactlyNonce := base64.StdEncoding.EncodeToString(make([]byte, 12))
	_, err = box.Decrypt(exactlyNonce)
	if err == nil {
		t.Fatal("expected error for nonce-sized ciphertext, got nil")
	}
	if !strings.Contains(err.Error(), "ciphertext too short") {
		t.Fatalf("expected 'ciphertext too short' error, got: %v", err)
	}
}

// --- 6. NewBox key validation -------------------------------------------------

func TestNewBox_InvalidBase64(t *testing.T) {
	_, err := secrets.NewBox("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
}

func TestNewBox_WrongKeyLength(t *testing.T) {
	// 16-byte key (AES-128), but we require exactly 32.
	key16 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, err := secrets.NewBox(key16)
	if err == nil {
		t.Fatal("expected error for 16-byte key, got nil")
	}
}

func TestNewBox_ValidKey(t *testing.T) {
	_, err := secrets.NewBox(testKeyB64())
	if err != nil {
		t.Fatalf("expected no error for valid 32-byte key, got: %v", err)
	}
}
