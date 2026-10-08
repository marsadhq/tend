// Package httpserver implements Tend's minimal HTTP surface: a liveness probe
// and the heartbeat (dead-man's-switch) ping endpoint. External jobs POST (or
// GET) /ping/{token} on each successful run; the server records the ping, which
// flips a previously-'down' heartbeat back to 'up' and emits a
// heartbeat.recovered event.
//
// The server depends only on the store, clock, and core packages (no import
// cycle: the store does not import httpserver). Everything is stdlib net/http.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/marsadhq/tend/internal/clock"
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

	// loginLimiter and pingLimiter are per-(IP,endpoint) token buckets guarding
	// POST /login and /ping/{token} against brute-force/flood, defense-in-depth
	// beyond argon2 cost / token secrecy. See ratelimit.go.
	loginLimiter *rateLimiter
	pingLimiter  *rateLimiter
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
		loginLimiter: newRateLimiter(clk, 1, 5),
		// 10/s burst-20: many heartbeats can legitimately share one source IP
		// (NAT / one host running many cron jobs), so this is generous
		// defense-in-depth against a flood, not per-token fairness - keying on
		// the token itself would let an attacker rotate tokens to evade it and
		// would penalize the exact identifier we want to protect.
		pingLimiter: newRateLimiter(clk, 10, 20),
	}
}

// Handler builds and returns the routing mux.
//
// Public routes (always registered, never gated by requireAuth):
//
//   - GET       /healthz       -> 200 liveness probe
//   - POST|GET  /ping/{token}  -> record a heartbeat ping
//
// When auth is configured, Handler also registers the public auth surface
// (GET/POST /login, the /static/ asset prefix) and mounts the requireAuth-gated
// API + dashboard behind the gate. POST /logout is a cookie-auth POST and is
// registered behind the gate (not on the public mux) so it inherits the
// cookie-auth CSRF requirement. When auth is nil, ONLY the two public routes
// above are registered.
//
// Every route, in both modes, is wrapped in securityHeaders.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /ping/{token}", s.rateLimit(s.pingLimiter, "ping", s.handlePing))
	mux.HandleFunc("GET /ping/{token}", s.rateLimit(s.pingLimiter, "ping", s.handlePing))

	if s.auth == nil {
		return securityHeaders(mux)
	}

	// --- public auth surface (NOT gated; these inherently bypass requireAuth) ---
	mux.HandleFunc("GET /login", s.handleLoginForm)
	// Only the credential-checking POST is rate-limited; GET /login (the form)
	// is cheap and unauthenticated-but-harmless.
	mux.HandleFunc("POST /login", s.rateLimit(s.loginLimiter, "login", s.handleLoginSubmit))

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

	return securityHeaders(mux)
}

// securityHeaders sets defensive response headers on every route. The CSP is
// deliberately strict: the dashboard loads only same-origin /static/ assets
// (htmx.min.js, row-nav.js, app.css) and uses no inline scripts, so 'self'
// suffices with no 'unsafe-inline' for scripts. htmx attributes are HTML
// attributes, not inline <script>, so they are unaffected by script-src.
// style-src is 'self' as well: htmx would otherwise inject an inline <style>
// for its request indicator at load, so the base template turns that off
// (htmx-config includeIndicatorStyles=false; the dashboard has no indicator).
//
// HSTS is intentionally omitted: serve speaks plain HTTP by default (TLS is
// terminated at a reverse proxy per README), and emitting HSTS over plain
// HTTP is either ignored or, once cached, can lock a browser to HTTPS against
// an HTTP-only deploy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// handlePing records a dead-man's-switch ping. On a down->up recovery it emits
// a heartbeat.recovered event; EmitEvent enqueues the matched notification
// deliveries transactionally, and the dispatch func only nudges the delivery
// worker. Nothing here blocks on a slow destination, so the ping response is
// never held hostage by a down webhook receiver, and a client that times out
// and cancels r.Context() can no longer make the recovered notification vanish
// (F3): the enqueued delivery survives and is drained asynchronously.
//
// Ordering caveat: the status flip + event emit happen as two store calls. A
// crash between RecordPing's commit and EmitEvent would lose the recovered
// event (the heartbeat would still be correctly 'up'). This is acceptable for
// v1 and mirrors the heartbeat.missed path in the watcher.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	orgID, name, recovered, err := s.store.RecordPing(r.Context(), token, s.clk.Now())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		// A real DB error must not masquerade as a 404. The token is a
		// credential, so log only a truncated hint, never the full value.
		s.log.Error("record ping failed", "token_hint", tokenHint(token), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if recovered {
		// Payload is the plain heartbeat NAME: notify.EventJobID treats non-JSON
		// as job 0, and messageFor uses it for the subject.
		ev := core.Event{
			OrgID:   orgID,
			Type:    "heartbeat.recovered",
			Source:  "heartbeat",
			Payload: name,
		}
		if _, err := s.store.EmitEvent(r.Context(), ev); err != nil {
			// Log the heartbeat name (not the token credential).
			s.log.Error("emit heartbeat.recovered failed", "heartbeat", name, "err", err)
		}
		if s.dispatch != nil {
			s.dispatch(r.Context(), ev)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// tokenHint returns a short, non-sensitive prefix of a ping token for logs, so
// the full token (a credential) is never written to a log aggregator.
func tokenHint(token string) string {
	if len(token) <= 8 {
		return "********"
	}
	return token[:8] + "..."
}
