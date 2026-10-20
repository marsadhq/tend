// Package httpserver implements Tend's minimal HTTP surface: a liveness probe
// and the heartbeat (dead-man's-switch) ping API, compatible with the one of
// Healthchecks. External jobs ping /ping/{token} on each successful run (and
// /start, /fail, /log or /<exit-status> for more); the server records the
// ping, which flips a previously-'down' heartbeat back to 'up' and emits a
// heartbeat.recovered event, or takes it down on a failure.
//
// The server depends only on the store, clock, and core packages (no import
// cycle: the store does not import httpserver). Everything is stdlib net/http.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/httpx"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/store"
)

// Server serves the heartbeat ping and health endpoints and - when auth is
// configured - the authenticated web/API surface.
type Server struct {
	store    store.Store
	clk      clock.Clock
	dispatch func(context.Context, core.Event) // may be nil; best-effort notification sink
	log      *slog.Logger
	auth     *AuthConfig // nil disables login/API/dashboard (M2 public-only behavior)

	// loginLimiter is a per-(IP,endpoint) token bucket guarding POST /login
	// against brute force, defense-in-depth beyond argon2 cost.
	loginLimiter *httpx.RateLimiter
	// pingMissLimiter and pingTokenLimiter guard /ping/{token}; see
	// handlePing. Misses (unknown tokens) are counted per client address,
	// which is what stops token guessing; pings for a real heartbeat are
	// counted per token, never per address, so a host running many heartbeats
	// on the same minute is not throttled for it.
	pingMissLimiter  *httpx.RateLimiter
	pingTokenLimiter *httpx.RateLimiter
}

// New constructs a Server. dispatch may be nil (e.g. in tests or when no
// notifier is wired); the ping handler is nil-safe. log must be non-nil.
//
// auth bundles the session/CSRF codec and cookie policy. When auth is nil,
// Handler() registers ONLY the public routes (/healthz, /ping/{token}). When
// auth is non-nil, Handler() additionally mounts the login/logout endpoints,
// the /static/ asset prefix, and the requireAuth-gated API + dashboard surface.
func New(s store.Store, clk clock.Clock, dispatch func(context.Context, core.Event), log *slog.Logger, auth *AuthConfig) *Server {
	return &Server{
		store:    s,
		clk:      clk,
		dispatch: dispatch,
		log:      log,
		auth:     auth,
		// 1/s burst-5: slows online credential guessing hard; a human login is
		// well within a 5-request burst.
		loginLimiter: httpx.NewRateLimiter(clk, 1, 5),
		// Unknown tokens: a burst of 30, then 1/s per client address. A token
		// is 128 random bits, so this is about load, not odds: an address
		// that keeps guessing is refused before its requests reach the
		// database, whatever token it sends next, so rotating tokens does not
		// evade it.
		pingMissLimiter: httpx.NewRateLimiter(clk, pingMissRate, pingMissBurst),
		// Known tokens: a burst of 10, then 1/s per heartbeat. A heartbeat
		// pings once per period, so this only stops a runaway loop (or a
		// leaked token) from writing to the database flat out.
		pingTokenLimiter: httpx.NewRateLimiter(clk, pingTokenRate, pingTokenBurst),
	}
}

// Handler builds and returns the routing mux.
//
// Public routes (always registered, never gated by requireAuth):
//
//   - GET            /healthz   -> 200 liveness probe
//   - HEAD|GET|POST  /ping/...  -> record a heartbeat ping (handlePing)
//
// When auth is configured, Handler also registers the public auth surface
// (GET/POST /login, the /static/ asset prefix) and mounts the requireAuth-gated
// API + dashboard behind the gate. POST /logout is a cookie-auth POST and is
// registered behind the gate (not on the public mux) so it inherits the
// cookie-auth CSRF requirement. When auth is nil, ONLY the two public routes
// above are registered.
//
// Every route, in both modes, is wrapped in httpx.SecurityHeaders (CSP and
// friends; see httpx.ContentSecurityPolicy).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// The ping API: /ping/<token>[/start|/fail|/log|/<exit-status>] and the
	// slug forms, for HEAD, GET and POST (see handlePing).
	mux.HandleFunc("/ping/", s.handlePing)

	if s.auth == nil {
		return httpx.SecurityHeaders(mux)
	}

	// --- public auth surface (NOT gated; these inherently bypass requireAuth) ---
	mux.HandleFunc("GET /login", s.handleLoginForm)
	// Only the credential-checking POST is rate-limited; GET /login (the form)
	// is cheap and unauthenticated-but-harmless.
	mux.HandleFunc("POST /login", s.loginLimiter.Limit("login", clientIP, s.handleLoginSubmit))

	// /static/ serves the embedded UI assets (htmx.min.js, row-nav.js, app.css,
	// and the icons). It is NOT gated by auth: these are non-sensitive browser
	// assets needed by the login and dashboard pages alike. The FileServer is rooted at the embedded
	// static/ directory (see dashboard.go).
	mux.Handle("/static/", staticHandler())

	// --- authed surface (gated by requireAuth) ---
	// Later tasks register the API and dashboard routes on authed, which is
	// wrapped by requireAuth before being mounted at "/". The catch-all "/"
	// pattern is matched only when no more specific public route above wins, so
	// /healthz, /ping, /login, and /static stay public. POST /logout is
	// registered on this gated mux (below), NOT public.
	authed := http.NewServeMux()
	// Logout is a cookie-auth POST and must inherit the same cookie-auth CSRF
	// enforcement as every other dashboard mutation, so it is registered HERE
	// (behind requireAuth) rather than on the public mux above.
	authed.HandleFunc("POST /logout", s.handleLogout)
	// Task 4/5: REST API, org-scoped via Principal.OrgID. registerAPIRoutes
	// registers the read-only GET surface AND the Task 5 action endpoints
	// (POST run-now / enable / disable) on this requireAuth-gated mux.
	s.registerAPIRoutes(authed)
	// Task 6: server-rendered htmx dashboard pages (jobs list, job detail, run
	// detail) + the jobs polling fragment, all org-scoped via the Principal.
	s.registerDashboardRoutes(authed)
	mux.Handle("/", s.requireAuth(authed))

	return httpx.SecurityHeaders(mux)
}
