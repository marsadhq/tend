package httpserver

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/store"
)

// AuthConfig bundles everything the authenticated surface needs. It is supplied
// to New; a nil *AuthConfig disables login/API/dashboard (M2 public-only mode).
type AuthConfig struct {
	// Codec signs/verifies session cookies and CSRF tokens. Required.
	Codec *auth.SessionCodec
	// Secure flags the session cookie as Secure (HTTPS-only). Set it when the
	// server is reached over TLS. Reverse-proxy users that terminate TLS in
	// front of tend should set it too.
	Secure bool
}

// sessionCookieName is the name of the signed session cookie.
const sessionCookieName = "tend_session"

// --- request-scoped principal ------------------------------------------------

// ctxKey is an unexported context key type so values set here cannot collide
// with keys from other packages.
type ctxKey int

const principalKey ctxKey = 0

// authResult is what requireAuth resolves and stashes in the request context:
// the Principal plus, for cookie-authenticated requests, the Session needed to
// validate a CSRF token. byCookie distinguishes cookie auth (CSRF-required on
// mutations) from bearer auth (CSRF-exempt). csrf is the per-session CSRF token
// computed for cookie-authenticated requests (empty for bearer auth) so the
// dashboard can render it into pages and forms; it does NOT influence the auth
// decision.
type authResult struct {
	principal auth.Principal
	session   auth.Session // zero for bearer auth
	byCookie  bool
	csrf      string // session-bound CSRF token; "" for bearer/no-cookie
}

// PrincipalFrom returns the authenticated Principal stashed in ctx by
// requireAuth, and whether one was present. Downstream API/dashboard handlers
// use it to scope every store call to principal.OrgID.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	ar, ok := authResultFrom(ctx)
	if !ok {
		return auth.Principal{}, false
	}
	return ar.principal, true
}

// CSRFTokenFrom returns the session-bound CSRF token stashed in ctx for a
// cookie-authenticated request, or "" for bearer-authenticated requests, an
// unauthenticated request, or a nil context. The dashboard renders it into the
// page (a <meta> tag and hidden form fields) so cookie-auth POSTs (logout and
// the Task 7 actions) can carry a valid token. It is purely advisory: it never
// affects whether a request is authenticated.
func CSRFTokenFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ar, ok := authResultFrom(ctx)
	if !ok {
		return ""
	}
	return ar.csrf
}

// authResultFrom retrieves the authResult set by requireAuth (nil-safe).
func authResultFrom(ctx context.Context) (authResult, bool) {
	if ctx == nil {
		return authResult{}, false
	}
	ar, ok := ctx.Value(principalKey).(authResult)
	return ar, ok
}

// --- middleware --------------------------------------------------------------

// requireAuth resolves the caller's Principal (session cookie first, then
// Authorization: Bearer), stashes it in the request context, enforces CSRF on
// cookie-authenticated mutations, and only then calls next.
//
// Auth resolution FAILS CLOSED: any error decoding the cookie, loading the
// user/membership, or matching the token leaves the request unauthenticated. On
// failure it branches on the path: /api/... gets a 401 JSON body, everything
// else gets a 302 redirect to /login. No branch reveals which step failed.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar, ok := s.resolve(r)
		if !ok {
			s.rejectUnauthenticated(w, r)
			return
		}

		// CSRF: cookie-authenticated unsafe methods must carry a valid token.
		// Bearer-authenticated requests carry no ambient cookie, so they are
		// CSRF-exempt.
		if ar.byCookie && isUnsafeMethod(r.Method) {
			if !s.validCSRF(r, ar.session) {
				http.Error(w, "forbidden: invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), principalKey, ar)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolve attempts cookie auth, then bearer auth. It returns false (fail closed)
// when neither succeeds.
func (s *Server) resolve(r *http.Request) (authResult, bool) {
	if ar, ok := s.resolveCookie(r); ok {
		return ar, true
	}
	if ar, ok := s.resolveBearer(r); ok {
		return ar, true
	}
	return authResult{}, false
}

// resolveCookie validates the session cookie and loads the principal. Every
// failure (no cookie, bad signature, expiry, missing user/membership) returns
// false without distinguishing which step failed.
func (s *Server) resolveCookie(r *http.Request) (authResult, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return authResult{}, false
	}
	sess, err := s.auth.Codec.Decode(c.Value)
	if err != nil {
		return authResult{}, false
	}
	// Confirm the user and membership still exist (org-scoped), and read the
	// authoritative role from the membership rather than trusting the cookie.
	if _, err := s.store.GetUserByID(r.Context(), sess.OrgID, sess.UserID); err != nil {
		return authResult{}, false
	}
	m, err := s.store.GetMembership(r.Context(), sess.OrgID, sess.UserID)
	if err != nil {
		return authResult{}, false
	}
	return authResult{
		principal: auth.Principal{OrgID: sess.OrgID, UserID: sess.UserID, Role: m.Role},
		session:   sess,
		byCookie:  true,
		// Compute the session-bound CSRF token only on this already-authenticated
		// cookie path. It is stashed for rendering into pages/forms and never
		// gates the auth decision. Bearer auth leaves this empty (CSRF-exempt).
		csrf: s.auth.Codec.IssueCSRF(sess),
	}, true
}

// resolveBearer validates an Authorization: Bearer token. A token principal has
// UserID==0 and Role=="token". A miss returns false.
func (s *Server) resolveBearer(r *http.Request) (authResult, bool) {
	tok, ok := bearerToken(r)
	if !ok {
		return authResult{}, false
	}
	orgID, _, err := s.store.AuthenticateToken(r.Context(), auth.HashToken(tok))
	if err != nil {
		return authResult{}, false
	}
	return authResult{
		principal: auth.Principal{OrgID: orgID, UserID: 0, Role: "token"},
		byCookie:  false,
	}, true
}

// bearerToken extracts the token from an "Authorization: Bearer <tok>" header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// isUnsafeMethod reports whether method mutates state and therefore requires
// CSRF protection under cookie auth.
func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// validCSRF checks the X-CSRF-Token header first, then the csrf_token form
// field, against the session-bound CSRF token (constant-time inside auth).
func (s *Server) validCSRF(r *http.Request, sess auth.Session) bool {
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		// ParseForm reads the body for urlencoded POSTs; ignore parse errors and
		// fall through to a missing-token rejection.
		_ = r.ParseForm()
		tok = r.PostFormValue("csrf_token")
	}
	if tok == "" {
		return false
	}
	return s.auth.Codec.CheckCSRF(sess, tok)
}

// rejectUnauthenticated writes the unauthenticated response: a 401 JSON body for
// API paths, a 302 redirect to /login for everything else.
func (s *Server) rejectUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// --- login / logout ----------------------------------------------------------

// loginTmpl is a minimal login form. The rich dashboard templates land in
// Task 6; this is intentionally bare. errMsg, when non-empty, renders a generic
// error (never revealing whether the email or the password was wrong).
var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sign in · tend</title>
  <link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
  <link rel="stylesheet" href="/static/app.css">
</head>
<body class="auth-body">
  <div class="grid-bg" aria-hidden="true"></div>
  <main class="auth-wrap">
    <div class="auth-card">
      <div class="auth-brand">
        <img class="auth-logo" src="/static/logomark.svg" width="58" height="58" alt="">
        <div class="auth-wordmark">tend</div>
        <div class="auth-tag">job runner</div>
      </div>
      {{if .Error}}<p class="auth-error" role="alert">{{.Error}}</p>{{end}}
      <form class="auth-form" method="post" action="/login">
        <label class="field">
          <span class="field-label">Email</span>
          <input class="field-input" type="email" name="email" autocomplete="username" required autofocus>
        </label>
        <label class="field">
          <span class="field-label">Password</span>
          <input class="field-input" type="password" name="password" autocomplete="current-password" required>
        </label>
        <button class="auth-btn" type="submit">Sign in</button>
      </form>
    </div>
  </main>
</body>
</html>
`))

// genericLoginError is shown for BOTH an unknown email and a wrong password, so
// the form cannot be used to enumerate accounts.
const genericLoginError = "Invalid email or password."

// dummyPasswordHash is a valid argon2id PHC string verified (and discarded) on
// the unknown-email login path so that branch performs the SAME argon2id work as
// the wrong-password branch. Without this, an unknown email would return before
// VerifyPassword runs (~tens of ms cheaper), turning the login response time
// into a username-enumeration oracle despite the identical response body.
//
// It is computed ONCE at package init via auth.HashPassword so it carries the
// exact same cost parameters as real stored hashes (so the equalized timing
// genuinely matches the real verify path). If hashing fails at startup we fall
// back to a valid literal of the correct form rather than panicking; that
// literal still drives a full argon2id verification, which is all we need.
var dummyPasswordHash = mustDummyPasswordHash()

// mustDummyPasswordHash returns a valid argon2id PHC string for timing
// equalization. It never panics: on any HashPassword error it returns a
// constant valid PHC literal (a real argon2id hash with the package's default
// m=19456,t=2,p=1 parameters) so VerifyPassword still does full argon2id work.
func mustDummyPasswordHash() string {
	// A random-ish throwaway password; the plaintext is irrelevant because the
	// verification result is always ignored.
	if h, err := auth.HashPassword("tend-login-timing-equalizer"); err == nil {
		return h
	}
	// Fallback: a valid argon2id PHC literal with the correct form/parameters.
	return "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
}

// handleLoginForm renders the (empty) login form.
func (s *Server) handleLoginForm(w http.ResponseWriter, _ *http.Request) {
	s.renderLogin(w, "", http.StatusOK)
}

// handleLoginSubmit verifies the credentials and, on success, sets a signed
// session cookie and redirects to /. On any failure it re-renders the form with
// a generic error and sets no cookie.
//
// POST /login establishes the session (there is no session yet), so it is not
// subject to session-CSRF; SameSite=Lax plus the password check defend it.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, genericLoginError, http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	pw := r.PostFormValue("password")

	orgID, err := s.defaultOrgID(r.Context())
	if err != nil {
		s.log.Error("login: resolve org failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	user, err := s.store.GetUserByEmail(r.Context(), orgID, email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			// A real DB error is distinct from a bad credential; log it without
			// any secret material, but still show the user a generic message.
			s.log.Error("login: lookup user failed", "err", err)
		}
		// Equalize login timing to prevent user enumeration: run one argon2id
		// verification against a fixed dummy hash (result ignored) so the
		// unknown-email path costs the same as the wrong-password path before
		// returning the identical generic error.
		_ = auth.VerifyPassword(dummyPasswordHash, pw)
		// Unknown email: same generic error as a bad password (no enumeration).
		s.renderLogin(w, genericLoginError, http.StatusUnauthorized)
		return
	}
	if !auth.VerifyPassword(user.PasswordHash, pw) {
		s.renderLogin(w, genericLoginError, http.StatusUnauthorized)
		return
	}

	sess := auth.Session{
		UserID: user.ID,
		OrgID:  user.OrgID,
		Expiry: s.clk.Now().Add(auth.DefaultSessionTTL),
	}
	value, err := s.auth.Codec.Encode(sess)
	if err != nil {
		s.log.Error("login: encode session failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.sessionCookie(value, int(auth.DefaultSessionTTL.Seconds())))
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout clears the session cookie. It is registered on the requireAuth-
// gated mux (see Handler), so it only runs after requireAuth has validated the
// session cookie AND enforced the cookie-auth CSRF check: a cross-site logout
// POST cannot forge the session-bound CSRF token (it gets a 403), and an
// unauthenticated POST /logout is redirected to /login (there is no session to
// clear). SameSite=Lax independently blocks cross-site form posts.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessionCookie("", -1))
	http.Redirect(w, r, "/login", http.StatusFound)
}

// renderLogin writes the login form with an optional generic error and status.
func (s *Server) renderLogin(w http.ResponseWriter, errMsg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, struct{ Error string }{Error: errMsg})
}

// sessionCookie builds the session cookie with the security attributes the
// task mandates: HttpOnly, SameSite=Lax, Path=/, and Secure per AuthConfig.
// A negative maxAge (with empty value) expires the cookie (logout).
func (s *Server) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.auth.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// defaultOrgID resolves the org that interactive logins belong to. tend is
// single-org in v1; BootstrapDefaultOrg is idempotent and returns the existing
// "default" org.
func (s *Server) defaultOrgID(ctx context.Context) (int64, error) {
	org, err := s.store.BootstrapDefaultOrg(ctx)
	if err != nil {
		return 0, err
	}
	return org.ID, nil
}
