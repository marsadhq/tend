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
}

// New constructs a Server. dispatch may be nil (e.g. in tests or when no
// notifier is wired); the ping handler is nil-safe. log must be non-nil.
//
// auth bundles the session/CSRF codec and cookie policy. When auth is nil,
// Handler() registers ONLY the public M2 routes (/healthz, /ping/{token}) and
// behaves byte-identically to M2. When auth is non-nil, Handler() additionally
// mounts the login/logout endpoints, the /static/ asset prefix, and the
// requireAuth-gated API + dashboard surface.
func New(s store.Store, clk clock.Clock, dispatch func(context.Context, core.Event), log *slog.Logger, auth *AuthConfig) *Server {
	return &Server{store: s, clk: clk, dispatch: dispatch, log: log, auth: auth}
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
// cookie-auth CSRF requirement. When auth is nil, ONLY the two public M2 routes
// above are registered (byte-identical M2 behavior).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /ping/{token}", s.handlePing)
	mux.HandleFunc("GET /ping/{token}", s.handlePing)

	if s.auth == nil {
		return mux
	}

	// --- public auth surface (NOT gated; these inherently bypass requireAuth) ---
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)

	// /static/ serves the embedded UI assets (htmx.min.js + app.css). It is NOT
	// gated by auth: these are non-sensitive browser assets needed by the login
	// and dashboard pages alike. The FileServer is rooted at the embedded
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

	return mux
}

// handlePing records a dead-man's-switch ping. On a down->up recovery it emits a
// heartbeat.recovered event and then dispatches it best-effort.
//
// Ordering caveat: the status flip + event emit happen as two store calls and
// the dispatch happens afterwards in-process. A crash between RecordPing's
// commit and EmitEvent would lose the recovered event (the heartbeat would still
// be correctly 'up'). This is acceptable for v1 and mirrors the heartbeat.missed
// path in the Task 7 watcher.
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
		// Payload is the plain heartbeat NAME: the dispatcher's jobIDFromPayload
		// treats non-JSON as job 0, and messageFor uses it for the subject.
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
