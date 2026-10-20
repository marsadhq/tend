package auth_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	kitauth "github.com/marsadhq/kit/auth"
	"github.com/marsadhq/tend/internal/auth"
)

// The password, token-hash, session and CSRF primitives are kit/auth's and are
// tested there. These tests cover what Tend adds: its token prefix and its
// identity types.

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

func TestHashToken_CoversThePrefix(t *testing.T) {
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	h := kitauth.HashToken(tok)
	if len(h) != 64 || h == tok {
		t.Fatalf("HashToken(%q) = %q, want 64 hex chars", tok, h)
	}
	if h == kitauth.HashToken(strings.TrimPrefix(tok, auth.TokenPrefix)) {
		t.Fatal("HashToken ignores the tend_ prefix; stored token hashes would no longer match")
	}
}

// --- Domain types ------------------------------------------------------------
// Compile-time check that the exported domain types exist with the documented
// field names/types so the store can rely on them.

func TestDomainTypes_Shape(t *testing.T) {
	now := time.Now()
	_ = auth.User{ID: 1, OrgID: 2, Email: "a@b.c", PasswordHash: "h", CreatedAt: now}
	_ = auth.APIToken{ID: 1, OrgID: 2, Name: "ci", TokenHash: "h", CreatedAt: now}
	_ = auth.Membership{ID: 1, OrgID: 2, UserID: 3, Role: "admin", CreatedAt: now}
	_ = auth.Principal{OrgID: 2, UserID: 3, Role: "admin"}
}
