package httpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/store"
)

// --- auth test helpers -------------------------------------------------------

const testCSRFKey = "0123456789abcdef0123456789abcdef" // >=32 bytes for HMAC

// authStore is a testStore plus a seeded user + API token, ready for auth tests.
type authStore struct {
	*testStore
	userID    int64
	email     string
	password  string
	role      string
	token     string // plaintext bearer token
	tokenName string
}

// seedAuth seeds a user (with argon2id password), a membership, and an API
// token directly via the store API so the auth tests can exercise login and
// bearer auth without depending on the (not-yet-built) admin CLI.
func seedAuth(t *testing.T, ts *testStore) *authStore {
	t.Helper()
	ctx := context.Background()
	const email = "admin@example.com"
	const pw = "correct horse battery staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Insert user + membership directly via the raw handle (the public store API
	// for creating users lands in a later task).
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := ts.raw.ExecContext(ctx,
		`INSERT INTO users (org_id, email, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		ts.orgID, email, hash, created)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	if _, err := ts.raw.ExecContext(ctx,
		`INSERT INTO memberships (org_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		ts.orgID, uid, "admin", created); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ts.raw.ExecContext(ctx,
		`INSERT INTO api_tokens (org_id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		ts.orgID, "ci", auth.HashToken(tok), created); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	return &authStore{
		testStore: ts,
		userID:    uid,
		email:     email,
		password:  pw,
		role:      "admin",
		token:     tok,
		tokenName: "ci",
	}
}

func testCodec() *auth.SessionCodec { return auth.NewSessionCodec([]byte(testCSRFKey)) }

func authConfig(secure bool) *httpserver.AuthConfig {
	return &httpserver.AuthConfig{Codec: testCodec(), Secure: secure}
}

// newAuthServer builds a *Server with auth enabled and a fixed clock.
func newAuthServer(t *testing.T, s store.Store, cfg *httpserver.AuthConfig) *httpserver.Server {
	t.Helper()
	return httpserver.New(s, clock.RealClock{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
}

// validSessionCookie encodes a session for as.userID/orgID and returns the
// cookie value plus a matching CSRF token.
func validSessionCookie(t *testing.T, as *authStore, codec *auth.SessionCodec) (cookie, csrf string) {
	t.Helper()
	sess := auth.Session{UserID: as.userID, OrgID: as.orgID, Expiry: time.Now().Add(time.Hour)}
	enc, err := codec.Encode(sess)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return enc, codec.IssueCSRF(sess)
}

// stub is a handler that records the Principal it saw and writes 200.
type stub struct {
	saw    auth.Principal
	sawOK  bool
	called bool
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	s.saw, s.sawOK = httpserver.PrincipalFrom(r.Context())
	w.WriteHeader(http.StatusOK)
}

// --- requireAuth: success paths ----------------------------------------------

func TestRequireAuthValidCookie(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})

	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, _ := validSessionCookie(t, as, codec)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !st.called {
		t.Fatal("next handler not called")
	}
	if !st.sawOK {
		t.Fatal("principal not found in context")
	}
	if st.saw.OrgID != as.orgID || st.saw.UserID != as.userID || st.saw.Role != as.role {
		t.Fatalf("principal: got %+v want org=%d user=%d role=%q", st.saw, as.orgID, as.userID, as.role)
	}
}

func TestRequireAuthValidBearer(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))

	st := &stub{}
	h := srv.RequireAuthForTest(st)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+as.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !st.sawOK {
		t.Fatal("principal not found in context")
	}
	if st.saw.OrgID != as.orgID {
		t.Fatalf("principal org: got %d want %d", st.saw.OrgID, as.orgID)
	}
	if st.saw.UserID != 0 {
		t.Fatalf("token principal UserID: got %d want 0", st.saw.UserID)
	}
	if st.saw.Role != "token" {
		t.Fatalf("token principal Role: got %q want %q", st.saw.Role, "token")
	}
}

// --- requireAuth: failure paths (401 vs 302) ---------------------------------

func TestRequireAuthMissingCredentialAPI401(t *testing.T) {
	ts := newStore(t)
	srv := newAuthServer(t, ts.store, authConfig(false))
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if st.called {
		t.Fatal("next handler must NOT be called on auth failure")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: got %q want json", ct)
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("body: got %q want it to mention unauthorized", rec.Body.String())
	}
}

func TestRequireAuthMissingCredentialDashboard302(t *testing.T) {
	ts := newStore(t)
	srv := newAuthServer(t, ts.store, authConfig(false))
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("location: got %q want /login", loc)
	}
	if st.called {
		t.Fatal("next handler must NOT be called on auth failure")
	}
}

func TestRequireAuthTamperedCookieFailsClosed(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, _ := validSessionCookie(t, as, codec)
	// Flip the FIRST char (start of the base64url payload) to break the HMAC.
	// The first char always encodes significant bits, so toggling it always
	// yields different decoded bytes. (The last char of a 32-byte HMAC's
	// base64url encoding carries non-significant trailing bits, so flipping it
	// can decode back to the same tag and spuriously pass - hence the front.)
	tampered := flip(cookie[0]) + cookie[1:]
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: tampered})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (tampered cookie must fail closed)", rec.Code)
	}
	if st.called {
		t.Fatal("next handler must NOT be called with a tampered cookie")
	}
}

func TestRequireAuthWrongKeyCookieFailsClosed(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	// Server uses one key; cookie minted with a DIFFERENT key.
	srv := newAuthServer(t, ts.store, authConfig(false))
	otherCodec := auth.NewSessionCodec([]byte("ffffffffffffffffffffffffffffffff"))
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, _ := validSessionCookie(t, as, otherCodec)
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (wrong-key cookie must fail closed)", rec.Code)
	}
}

func TestRequireAuthBadBearerFailsClosed(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer tend_not-a-real-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (bad bearer must fail closed)", rec.Code)
	}
}

// --- CSRF for cookie-authenticated mutations ---------------------------------

func TestCSRFCookiePostNoTokenRejected(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, _ := validSessionCookie(t, as, codec)
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (cookie POST without CSRF)", rec.Code)
	}
	if st.called {
		t.Fatal("next must NOT run without a CSRF token")
	}
}

func TestCSRFCookiePostBadTokenRejected(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, _ := validSessionCookie(t, as, codec)
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", "bogus")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (cookie POST with bad CSRF)", rec.Code)
	}
}

func TestCSRFCookiePostValidTokenHeaderPasses(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, csrf := validSessionCookie(t, as, codec)
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (cookie POST with valid CSRF header)", rec.Code)
	}
	if !st.called {
		t.Fatal("next must run with a valid CSRF token")
	}
}

func TestCSRFCookiePostValidTokenFormFieldPasses(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	cookie, csrf := validSessionCookie(t, as, codec)
	form := url.Values{"csrf_token": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (cookie POST with valid CSRF form field)", rec.Code)
	}
	if !st.called {
		t.Fatal("next must run with a valid CSRF form field")
	}
}

func TestCSRFBearerPostExempt(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	st := &stub{}
	h := srv.RequireAuthForTest(st)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+as.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (bearer POST is CSRF-exempt)", rec.Code)
	}
	if !st.called {
		t.Fatal("next must run for a bearer POST (CSRF-exempt)")
	}
}

// --- login / logout ----------------------------------------------------------

func TestLoginGetRendersForm(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<form", "password", "email"} {
		if !strings.Contains(body, want) {
			t.Fatalf("login form missing %q; body=%q", want, body)
		}
	}
}

func TestLoginPostSuccessSetsCookieAndRedirects(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec, Secure: true})
	h := srv.Handler()

	form := url.Values{"email": {as.email}, "password": {as.password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("location: got %q want /", loc)
	}

	c := findCookie(rec.Result().Cookies(), "tend_session")
	if c == nil {
		t.Fatal("no tend_session cookie set on successful login")
	}
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite: got %v want Lax", c.SameSite)
	}
	if !c.Secure {
		t.Fatal("session cookie must be Secure when AuthConfig.Secure is set")
	}
	if c.Path != "/" {
		t.Fatalf("session cookie path: got %q want /", c.Path)
	}
	// The cookie value must decode to a valid session for this user.
	sess, err := codec.Decode(c.Value)
	if err != nil {
		t.Fatalf("decode session cookie: %v", err)
	}
	if sess.UserID != as.userID || sess.OrgID != as.orgID {
		t.Fatalf("session: got user=%d org=%d want user=%d org=%d", sess.UserID, sess.OrgID, as.userID, as.orgID)
	}
}

func TestLoginPostInsecureOmitsSecureFlag(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	form := url.Values{"email": {as.email}, "password": {as.password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c := findCookie(rec.Result().Cookies(), "tend_session")
	if c == nil {
		t.Fatal("no session cookie set")
	}
	if c.Secure {
		t.Fatal("session cookie must NOT be Secure when AuthConfig.Secure is false")
	}
}

func TestLoginPostWrongPasswordNoCookieGenericError(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	form := url.Values{"email": {as.email}, "password": {"wrong-password"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if c := findCookie(rec.Result().Cookies(), "tend_session"); c != nil && c.Value != "" {
		t.Fatal("no session cookie may be set on a failed login")
	}
	if rec.Code == http.StatusFound {
		t.Fatalf("wrong password must not redirect; got %d", rec.Code)
	}
	body := rec.Body.String()
	// Generic error - must not reveal which field was wrong.
	if strings.Contains(strings.ToLower(body), "not found") || strings.Contains(strings.ToLower(body), "no such user") {
		t.Fatalf("login error must be generic (no user enumeration); body=%q", body)
	}
}

func TestLoginPostUnknownEmailGenericErrorNoCookie(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	form := url.Values{"email": {"nobody@example.com"}, "password": {"whatever"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if c := findCookie(rec.Result().Cookies(), "tend_session"); c != nil && c.Value != "" {
		t.Fatal("no session cookie may be set for an unknown email")
	}
	if rec.Code == http.StatusFound {
		t.Fatalf("unknown email must not redirect; got %d", rec.Code)
	}
}

// TestLoginPostUnknownEmailAndWrongPasswordIdentical proves the two failure
// paths are indistinguishable to a client: same status code, same response
// body, and no session cookie on either. This is a behavior assertion (NOT a
// timing assertion); combined with the dummy-hash verification on the
// unknown-email path it closes the username-enumeration channel. A timing test
// would be flaky, so we only assert the observable response is identical.
func TestLoginPostUnknownEmailAndWrongPasswordIdentical(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	post := func(email, pw string) *httptest.ResponseRecorder {
		form := url.Values{"email": {email}, "password": {pw}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	unknown := post("nobody@example.com", "whatever")
	wrong := post(as.email, "wrong-password")

	if unknown.Code != wrong.Code {
		t.Fatalf("status differs: unknown-email=%d wrong-password=%d", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("response body differs between unknown-email and wrong-password paths")
	}
	if c := findCookie(unknown.Result().Cookies(), "tend_session"); c != nil && c.Value != "" {
		t.Fatal("no session cookie may be set for an unknown email")
	}
	if c := findCookie(wrong.Result().Cookies(), "tend_session"); c != nil && c.Value != "" {
		t.Fatal("no session cookie may be set for a wrong password")
	}
}

// TestLogoutWithoutCSRFRejected proves logout is behind the cookie-auth CSRF
// gate: a cookie-authenticated POST /logout WITHOUT a CSRF token must be
// rejected with 403 and must NOT clear the session cookie. This is the
// forced-logout (CSRF) defense - a cross-site POST cannot force-clear the
// victim's cookie.
func TestLogoutWithoutCSRFRejected(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()

	cookie, _ := validSessionCookie(t, as, codec)
	// No csrf_token form field and no X-CSRF-Token header.
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (cookie logout POST without CSRF)", rec.Code)
	}
	// The session cookie must NOT be cleared when CSRF enforcement rejects.
	if c := findCookie(rec.Result().Cookies(), "tend_session"); c != nil && c.Value == "" {
		t.Fatal("rejected logout must NOT clear the session cookie")
	}
}

// TestLogoutClearsCookie proves the happy path: a cookie-authenticated POST
// /logout WITH a valid CSRF token clears the session cookie and redirects.
func TestLogoutClearsCookie(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()

	cookie, csrf := validSessionCookie(t, as, codec)
	form := url.Values{"csrf_token": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want 302 (logout with valid CSRF redirects)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("location: got %q want /login", loc)
	}

	c := findCookie(rec.Result().Cookies(), "tend_session")
	if c == nil {
		t.Fatal("logout must emit a Set-Cookie that clears the session")
	}
	if c.Value != "" {
		t.Fatalf("logout cookie value: got %q want empty", c.Value)
	}
	if c.MaxAge >= 0 && !c.Expires.Before(time.Now()) {
		t.Fatalf("logout cookie must be expired: MaxAge=%d Expires=%v", c.MaxAge, c.Expires)
	}
}

// --- Handler() wiring: public bypass + auth==nil parity ----------------------

func TestPublicRoutesBypassAuth(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()

	cases := []struct {
		method, path string
		wantNot      int // status that would indicate auth gating
	}{
		{http.MethodGet, "/healthz", http.StatusFound},
		{http.MethodGet, "/login", http.StatusFound},
		{http.MethodGet, "/static/app.css", http.StatusFound},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusFound {
			loc := rec.Header().Get("Location")
			if loc == "/login" {
				t.Fatalf("%s %s was gated by auth (redirected to /login)", tc.method, tc.path)
			}
		}
	}
}

func TestHealthzPublicWhenAuthEnabled(t *testing.T) {
	ts := newStore(t)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status: got %d want 200", rec.Code)
	}
}

func TestStaticNotGatedByAuth(t *testing.T) {
	ts := newStore(t)
	srv := newAuthServer(t, ts.store, authConfig(false))
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/static/anything.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Whatever it resolves to (404 placeholder is fine in this task), it must
	// NOT redirect to /login.
	if rec.Code == http.StatusFound && rec.Header().Get("Location") == "/login" {
		t.Fatal("/static/ must not be gated by auth")
	}
}

func TestAuthNilLoginNotMounted(t *testing.T) {
	ts := newStore(t)
	srv := httpserver.New(ts.store, clock.RealClock{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h := srv.Handler()

	// /login must 404 when auth is disabled (M2 parity).
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/login with auth==nil: got %d want 404", rec.Code)
	}

	// /healthz must still work (M2 parity).
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/healthz with auth==nil: got %d want 200", rec2.Code)
	}
}

// --- small helpers -----------------------------------------------------------

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
