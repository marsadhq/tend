package auth_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/auth"
)

// --- Passwords (argon2id) ----------------------------------------------------

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !auth.VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("VerifyPassword returned false for the correct password")
	}
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if auth.VerifyPassword(hash, "Tr0ub4dor&3") {
		t.Fatal("VerifyPassword returned true for the wrong password")
	}
}

func TestHashPassword_IsPHCString(t *testing.T) {
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=") {
		t.Fatalf("hash is not a PHC argon2id string: %q", hash)
	}
}

func TestHashPassword_RandomSalt(t *testing.T) {
	h1, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword 1: %v", err)
	}
	h2, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword 2: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical (salt not random)")
	}
	// Both must still verify.
	if !auth.VerifyPassword(h1, "same password") || !auth.VerifyPassword(h2, "same password") {
		t.Fatal("one of the distinct hashes failed to verify")
	}
}

func TestVerifyPassword_MalformedInput(t *testing.T) {
	// VerifyPassword must never panic and must return false for garbage input.
	cases := []string{
		"",
		"not-a-phc-string",
		"$argon2id$v=19$m=19456,t=2,p=1", // missing salt/hash segments
		"$argon2id$v=19$m=19456,t=2,p=1$!!!notbase64!!!$alsoNotBase64", // bad base64
		"$bcrypt$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",                   // wrong algorithm
		"$argon2id$v=19$m=bad,t=2,p=1$c2FsdA$aGFzaA",                   // non-numeric param
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$aGFzaA",                 // wrong version
		"$$$$$",
	}
	for _, c := range cases {
		if auth.VerifyPassword(c, "anything") {
			t.Fatalf("VerifyPassword returned true for malformed input %q", c)
		}
	}
}

// --- API tokens --------------------------------------------------------------

func TestGenerateToken_DistinctHighEntropy(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		tok, err := auth.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if !strings.HasPrefix(tok, "tend_") {
			t.Fatalf("token missing tend_ prefix: %q", tok)
		}
		body := strings.TrimPrefix(tok, "tend_")
		raw, err := base64.RawURLEncoding.DecodeString(body)
		if err != nil {
			t.Fatalf("token body is not base64url: %q (%v)", tok, err)
		}
		if len(raw) != 32 {
			t.Fatalf("token entropy = %d bytes, want 32", len(raw))
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	h1 := auth.HashToken(tok)
	h2 := auth.HashToken(tok)
	if h1 != h2 {
		t.Fatal("HashToken is not deterministic")
	}
	// sha256 hex is 64 chars.
	if len(h1) != 64 {
		t.Fatalf("HashToken length = %d, want 64 hex chars", len(h1))
	}
	if h1 == tok {
		t.Fatal("HashToken returned the plaintext token")
	}
}

func TestHashToken_DistinctTokensDistinctHashes(t *testing.T) {
	t1, _ := auth.GenerateToken()
	t2, _ := auth.GenerateToken()
	if auth.HashToken(t1) == auth.HashToken(t2) {
		t.Fatal("different tokens hashed to the same value")
	}
}

// --- Sessions ----------------------------------------------------------------

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestSessionCodec_RoundTrip(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	want := auth.Session{UserID: 42, OrgID: 7, Expiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	enc, err := c.Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := c.Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.UserID != want.UserID || got.OrgID != want.OrgID {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) {
		t.Fatalf("expiry mismatch: got %v, want %v", got.Expiry, want.Expiry)
	}
}

func TestSessionCodec_Tampered(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	enc, err := c.Encode(auth.Session{UserID: 1, OrgID: 1, Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Flip a byte in the payload portion (before the '.').
	dot := strings.IndexByte(enc, '.')
	if dot <= 0 {
		t.Fatalf("encoded session has no payload.mac separator: %q", enc)
	}
	b := []byte(enc)
	// Mutate the first payload char to a different valid base64url char.
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, err := c.Decode(string(b)); err == nil {
		t.Fatal("Decode accepted a tampered session")
	}
}

func TestSessionCodec_WrongKeyRejected(t *testing.T) {
	c1 := auth.NewSessionCodec(testKey())
	other := make([]byte, 32)
	for i := range other {
		other[i] = 0xAB
	}
	c2 := auth.NewSessionCodec(other)
	enc, err := c1.Encode(auth.Session{UserID: 1, OrgID: 1, Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := c2.Decode(enc); err == nil {
		t.Fatal("Decode with a different key accepted the session")
	}
}

func TestSessionCodec_Expired(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	enc, err := c.Encode(auth.Session{UserID: 1, OrgID: 1, Expiry: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := c.Decode(enc); err == nil {
		t.Fatal("Decode accepted an expired session")
	}
}

func TestSessionCodec_MalformedRejected(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	cases := []string{
		"",
		"no-separator",
		".",
		"a.b.c",
		"!!!.???",
		strings.Repeat("A", 10) + ".",
	}
	for _, in := range cases {
		if _, err := c.Decode(in); err == nil {
			t.Fatalf("Decode accepted malformed input %q", in)
		}
	}
}

// --- CSRF --------------------------------------------------------------------

func TestCSRF_IssueAndCheck(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	s := auth.Session{UserID: 5, OrgID: 3, Expiry: time.Now().Add(time.Hour)}
	tok := c.IssueCSRF(s)
	if tok == "" {
		t.Fatal("IssueCSRF returned empty token")
	}
	if !c.CheckCSRF(s, tok) {
		t.Fatal("CheckCSRF rejected a freshly issued token")
	}
}

func TestCSRF_ForgedRejected(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	s := auth.Session{UserID: 5, OrgID: 3, Expiry: time.Now().Add(time.Hour)}
	if c.CheckCSRF(s, "forged-token") {
		t.Fatal("CheckCSRF accepted a forged token")
	}
	if c.CheckCSRF(s, "") {
		t.Fatal("CheckCSRF accepted an empty token")
	}
}

func TestCSRF_BoundToSession(t *testing.T) {
	c := auth.NewSessionCodec(testKey())
	exp := time.Now().Add(time.Hour)
	a := auth.Session{UserID: 5, OrgID: 3, Expiry: exp}
	b := auth.Session{UserID: 6, OrgID: 3, Expiry: exp} // different user
	tokA := c.IssueCSRF(a)
	if c.CheckCSRF(b, tokA) {
		t.Fatal("CSRF token issued for session A validated for session B")
	}
}

func TestCSRF_BoundToKey(t *testing.T) {
	c1 := auth.NewSessionCodec(testKey())
	other := make([]byte, 32)
	for i := range other {
		other[i] = 0x11
	}
	c2 := auth.NewSessionCodec(other)
	s := auth.Session{UserID: 5, OrgID: 3, Expiry: time.Now().Add(time.Hour)}
	tok := c1.IssueCSRF(s)
	if c2.CheckCSRF(s, tok) {
		t.Fatal("CSRF token validated under a different codec key")
	}
}

// --- Domain types ------------------------------------------------------------
// Compile-time check that the exported domain types exist with the documented
// field names/types so later tasks (store) can rely on them.

func TestDomainTypes_Shape(t *testing.T) {
	now := time.Now()
	_ = auth.User{ID: 1, OrgID: 2, Email: "a@b.c", PasswordHash: "h", CreatedAt: now}
	_ = auth.APIToken{ID: 1, OrgID: 2, Name: "ci", TokenHash: "h", CreatedAt: now}
	_ = auth.Membership{ID: 1, OrgID: 2, UserID: 3, Role: "admin", CreatedAt: now}
	_ = auth.Principal{OrgID: 2, UserID: 3, Role: "admin"}
}
