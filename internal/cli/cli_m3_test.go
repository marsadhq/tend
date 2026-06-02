package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/cli"
)

// testMasterKey returns a deterministic base64-encoded 32-byte master key.
func testMasterKey() string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// --- serve handler: both modes -------------------------------------------------

// TestServeHandlerWithMasterKeyMountsAuthSurface verifies that, with a master key
// configured, cmdServe's handler mounts the authenticated dashboard/API surface:
// /healthz is public (200), /login is served, /api/jobs requires auth (401), and
// /static/htmx.min.js is served.
func TestServeHandlerWithMasterKeyMountsAuthSurface(t *testing.T) {
	// Point the env-driven store at a temp DB so the handler builder does not
	// create a stray tend.db in the working tree.
	t.Setenv("TEND_DB", tempConfig(t).DSN)
	h, err := cli.BuildServeHandler(testMasterKey())
	if err != nil {
		t.Fatalf("build serve handler: %v", err)
	}

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{"GET", "/healthz", http.StatusOK},
		{"GET", "/login", http.StatusOK},
		{"GET", "/api/jobs", http.StatusUnauthorized}, // gated by requireAuth
		{"GET", "/static/htmx.min.js", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s = %d; want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

// TestServeHandlerWithoutMasterKeyIsPublicOnly verifies that, without a master
// key, cmdServe's handler is public-only: /healthz still works (200) but the
// authenticated surface is absent - /login is 404 (never registered).
func TestServeHandlerWithoutMasterKeyIsPublicOnly(t *testing.T) {
	t.Setenv("TEND_DB", tempConfig(t).DSN)
	h, err := cli.BuildServeHandler("")
	if err != nil {
		t.Fatalf("build serve handler: %v", err)
	}

	// Public route still works.
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d; want 200 (public-only mode)", rec.Code)
	}

	// Auth surface is NOT registered.
	req = httptest.NewRequest("GET", "/login", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /login = %d; want 404 (no master key ⇒ no auth surface)", rec.Code)
	}
}

// TestSessionKeyDeterministic verifies the derived session key is stable for a
// given master key (so a restart does not invalidate existing sessions) and
// differs for a different master key.
func TestSessionKeyDeterministic(t *testing.T) {
	mk := testMasterKey()
	k1, err := cli.DeriveSessionKey(mk)
	if err != nil {
		t.Fatalf("derive 1: %v", err)
	}
	k2, err := cli.DeriveSessionKey(mk)
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("session key is not deterministic for the same master key")
	}
	if len(k1) != 32 {
		t.Errorf("session key length = %d; want 32", len(k1))
	}

	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))
	k3, err := cli.DeriveSessionKey(other)
	if err != nil {
		t.Fatalf("derive 3: %v", err)
	}
	if bytes.Equal(k1, k3) {
		t.Error("different master keys produced the same session key")
	}
}

// --- user add + login end-to-end ----------------------------------------------

// TestUserAddCreatesUserAndAdminMembership verifies `user add` reads the password
// from STDIN (never argv), creates a user + an admin membership in the default
// org, and never echoes the password.
func TestUserAddCreatesUserAndAdminMembership(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	const pw = "correct horse battery staple"
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"user", "add", "-email", "admin@example.com"},
		strings.NewReader(pw+"\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("user add: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, pw) {
		t.Error("user add echoed the password (security violation)")
	}
	if strings.Contains(out, "$argon2id$") {
		t.Error("user add printed a password hash (security violation)")
	}
	if !strings.Contains(out, "admin@example.com") {
		t.Errorf("user add output should confirm the email, got: %q", out)
	}

	// Verify the user + admin membership exist in the store.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	u, err := st.GetUserByEmail(ctx, org.ID, "admin@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.PasswordHash == "" || u.PasswordHash == pw {
		t.Errorf("stored password hash is empty or plaintext: %q", u.PasswordHash)
	}
	m, err := st.GetMembership(ctx, org.ID, u.ID)
	if err != nil {
		t.Fatalf("get membership: %v", err)
	}
	if m.Role != "admin" {
		t.Errorf("membership role = %q; want admin", m.Role)
	}
}

// TestUserAddRejectsPasswordFlag asserts there is NO -password flag: passing one
// must fail (the password comes from stdin only).
func TestUserAddRejectsPasswordFlag(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"user", "add", "-email", "x@y.com", "-password", "leaky"},
		strings.NewReader("fromstdin123\n"), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected an error when -password flag is supplied (password must come from stdin only)")
	}
}

// TestUserCanLoginEndToEnd creates a user via cmdUser, then POSTs /login against
// cmdServe's handler with that password and asserts a session cookie is set.
func TestUserCanLoginEndToEnd(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	const email = "login@example.com"
	const pw = "s3cret-passphrase-xyz"
	if err := cli.Run(ctx, cfg, []string{"user", "add", "-email", email},
		strings.NewReader(pw+"\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("user add: %v", err)
	}

	// Build the serve handler against the SAME store (env-driven DSN) and POST
	// /login with the new credentials.
	t.Setenv("TEND_DB", cfg.DSN)
	h, err := cli.BuildServeHandler(cfg.MasterKey)
	if err != nil {
		t.Fatalf("build serve handler: %v", err)
	}

	form := url.Values{"email": {email}, "password": {pw}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("POST /login = %d; want 302 (successful login redirect). body=%s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Result().Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "tend_session=") {
		t.Errorf("login did not set a tend_session cookie; Set-Cookie=%q", setCookie)
	}
}

// --- token create / list / revoke ---------------------------------------------

// TestTokenCreatePrintsOnceAndStoresHashOnly verifies `token create` prints the
// plaintext token EXACTLY once with a "won't be shown again" note, stores only
// the hash, and that `token list` shows the token WITHOUT any hash.
func TestTokenCreatePrintsOnceAndStoresHashOnly(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"token", "create", "-name", "ci"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("token create: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()

	plaintext := extractToken(out)
	if plaintext == "" {
		t.Fatalf("token create did not print a tend_ token; out=%q", out)
	}
	// Printed exactly once.
	if strings.Count(out, plaintext) != 1 {
		t.Errorf("plaintext token printed %d times; want exactly 1\nout=%q", strings.Count(out, plaintext), out)
	}
	// A "won't be shown again" note is present.
	low := strings.ToLower(out)
	if !strings.Contains(low, "won't be shown") && !strings.Contains(low, "not be shown") {
		t.Errorf("token create output lacks a one-time warning note; out=%q", out)
	}

	// The stored value must NOT be the plaintext - only the hash is stored.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap default org: %v", err)
	}
	// The stored token_hash must equal HashToken(plaintext): looking up by the
	// hash of the plaintext must succeed and resolve to this org/name. (If the
	// impl wrongly stored the plaintext in token_hash, this lookup-by-hash would
	// miss and ErrNotFound here would fail the test.)
	gotOrg, gotName, err := st.AuthenticateToken(ctx, auth.HashToken(plaintext))
	if err != nil {
		t.Fatalf("AuthenticateToken(HashToken(plaintext)) failed; hash not stored correctly: %v", err)
	}
	if gotOrg != org.ID {
		t.Errorf("AuthenticateToken returned orgID=%d; want %d", gotOrg, org.ID)
	}
	if gotName != "ci" {
		t.Errorf("AuthenticateToken returned name=%q; want %q", gotName, "ci")
	}

	// token list shows the token name but never a hash.
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"token", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("token list: %v\nstderr: %s", err, stderr.String())
	}
	listOut := stdout.String()
	if !strings.Contains(listOut, "ci") {
		t.Errorf("token list missing token name; out=%q", listOut)
	}
	if strings.Contains(listOut, plaintext) {
		t.Error("token list leaked the plaintext token")
	}
	if containsHexHash(listOut) {
		t.Errorf("token list output appears to contain a token hash; out=%q", listOut)
	}
}

// TestTokenAuthenticatesAPI verifies that a created token can authenticate an
// /api/... request (Authorization: Bearer) against cmdServe's handler.
func TestTokenAuthenticatesAPI(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"token", "create", "-name", "api"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("token create: %v", err)
	}
	plaintext := extractToken(stdout.String())
	if plaintext == "" {
		t.Fatalf("no token printed; out=%q", stdout.String())
	}

	t.Setenv("TEND_DB", cfg.DSN)
	h, err := cli.BuildServeHandler(cfg.MasterKey)
	if err != nil {
		t.Fatalf("build serve handler: %v", err)
	}

	// Without the token: 401.
	req := httptest.NewRequest("GET", "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/jobs unauth = %d; want 401", rec.Code)
	}

	// With the bearer token: authenticated (200).
	req = httptest.NewRequest("GET", "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/jobs with bearer token = %d; want 200. body=%s", rec.Code, rec.Body.String())
	}
}

// TestTokenRevokeDeletes verifies `token revoke` deletes the token by id.
func TestTokenRevokeDeletes(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MasterKey = testMasterKey()
	ctx := context.Background()

	if err := cli.Run(ctx, cfg, []string{"token", "create", "-name", "temp"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("token create: %v", err)
	}

	// Resolve the token id via the store.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	toks, err := st.ListTokens(ctx, org.ID)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("expected 1 token, got %d", len(toks))
	}
	id := toks[0].ID

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"token", "revoke", "-id", strconv.FormatInt(id, 10)}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("token revoke: %v\nstderr: %s", err, stderr.String())
	}

	toks, err = st.ListTokens(ctx, org.ID)
	if err != nil {
		t.Fatalf("list tokens after revoke: %v", err)
	}
	if len(toks) != 0 {
		t.Errorf("expected 0 tokens after revoke, got %d", len(toks))
	}
}

// --- helpers -------------------------------------------------------------------

// extractToken returns the first whitespace-delimited field beginning with the
// tend_ token prefix, or "" if none is present.
func extractToken(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "tend_") {
			return f
		}
	}
	return ""
}

// containsHexHash reports whether s contains a run of >=64 hex characters (a
// sha256 hex digest), which would indicate a leaked token hash.
func containsHexHash(s string) bool {
	run := 0
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if isHex {
			run++
			if run >= 64 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}
