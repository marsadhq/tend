package cli_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	kitauth "github.com/marsadhq/kit/auth"
	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/cli"
)

// Credentials issued by Tend v0.3.0, before its auth code moved to kit/auth:
// produced by that release's HashPassword, GenerateToken and HashToken, and by
// its session codec under the key serve derives from testMasterKey(), for user
// 1 of org 1 with an expiry of 2100-01-01. They pin that an upgrade keeps
// every stored password hash and API token, and every session cookie and
// CSRF token a browser holds, working.
const (
	v030Password     = "correct horse battery staple"
	v030PasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$uGO0U9ZuAEW+Wb1sznDH6Q$rKKi5vnBLyN0KSeak8oH9FAb1b2J9/pEYVYTLctrcLY"
	v030Token        = "tend_N8FQKVHhzCDQTU_FCSc0zw3x86FCuyGd-cESq03r07o"
	v030TokenHash    = "d5f1e81afd9e9dabb16790de288749aee0c8afcb8b64264175a282a1dc18f864"
	v030Cookie       = "AAAAAAAAAAEAAAAAAAAAAQAAAAD0hlcA.whzI49CTKVXH2Du-Q2D7OO1uJ1TSTndaMN1S2hZT358"
	v030CSRF         = "e1dS4Slk74H_j6GTGhEsFZVnEp8uQZN7Q-c4bOEZZBc"
)

// TestCredentialsFromV030StillWork stores the v0.3.0 password hash and token
// hash as that release would have, then drives serve's real handler: the
// password logs in, the old session cookie is accepted, its CSRF token passes
// the check on a cookie-authenticated POST, and the old bearer token
// authenticates the API.
func TestCredentialsFromV030StillWork(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	if got := kitauth.HashToken(v030Token); got != v030TokenHash {
		t.Fatalf("HashToken(v0.3.0 token) = %s, want the hash v0.3.0 stored (%s)", got, v030TokenHash)
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap default org: %v", err)
	}
	userID, err := st.CreateUser(ctx, auth.User{OrgID: org.ID, Email: "compat@example.com", PasswordHash: v030PasswordHash})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if org.ID != 1 || userID != 1 {
		t.Fatalf("org %d, user %d: the v0.3.0 cookie is for user 1 of org 1", org.ID, userID)
	}
	if _, err := st.CreateMembership(ctx, auth.Membership{OrgID: org.ID, UserID: userID, Role: "admin"}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}
	if _, err := st.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "v030", TokenHash: v030TokenHash}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	t.Setenv("TEND_DB", cfg.DSN)
	h, err := cli.BuildServeHandler(cfg.MasterKey)
	if err != nil {
		t.Fatalf("build serve handler: %v", err)
	}
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	session := &http.Cookie{Name: "tend_session", Value: v030Cookie}

	// The password hash v0.3.0 stored still verifies.
	form := url.Values{"email": {"compat@example.com"}, "password": {v030Password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(req); rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "tend_session=") {
		t.Fatalf("POST /login with the v0.3.0 password hash = %d (Set-Cookie %q), want 302 with a session", rec.Code, rec.Header().Get("Set-Cookie"))
	}

	// The session cookie v0.3.0 issued still authenticates.
	req = httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(session)
	if rec := serve(req); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs with the v0.3.0 session cookie = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Its CSRF token still passes; a wrong one is still refused.
	for token, want := range map[string]int{v030CSRF: http.StatusFound, "f1dS4Slk74H_j6GTGhEsFZVnEp8uQZN7Q-c4bOEZZBc": http.StatusForbidden} {
		req = httptest.NewRequest(http.MethodPost, "/logout", nil)
		req.AddCookie(session)
		req.Header.Set("X-CSRF-Token", token)
		if rec := serve(req); rec.Code != want {
			t.Errorf("POST /logout with CSRF token %q = %d, want %d", token, rec.Code, want)
		}
	}

	// The API token v0.3.0 issued still authenticates.
	req = httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+v030Token)
	if rec := serve(req); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs with the v0.3.0 bearer token = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
