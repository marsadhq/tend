package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/httpserver"
)

// TestLoginRateLimited_HTTP drives POST /login past the 5-request burst from a
// fixed RemoteAddr and asserts the 6th attempt is rejected with 429, while
// under-burst attempts still reach the real handler (401 on bad creds).
func TestLoginRateLimited_HTTP(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	// Frozen clock: the login bucket refills 1 token/s and each attempt pays
	// for a password hash, so on a slow or busy machine five real attempts can
	// take over a second and the sixth would be let through.
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	srv := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), authConfig(false))
	h := srv.Handler()

	post := func() *httptest.ResponseRecorder {
		form := url.Values{"email": {as.email}, "password": {"wrong-password"}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "203.0.113.9:4444"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < 5; i++ {
		rec := post()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (bad creds, under burst)", i+1, rec.Code)
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: status = %d, want 429", rec.Code)
	}
	// The login bucket refills one token per second.
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("429 response Retry-After = %q, want %q", got, "1")
	}
}

// TestPingRateLimited_HTTP drives /ping/{token} past the 20-request burst from
// a fixed RemoteAddr and asserts the 21st is rejected with 429. GET and POST
// are both ping routes and draw on the same bucket, so the burst alternates
// between them and both must be refused once it is spent.
func TestPingRateLimited_HTTP(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "api", "tok-rl", "up")
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	srv := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h := srv.Handler()

	ping := func(method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/ping/tok-rl", nil)
		req.RemoteAddr = "198.51.100.3:9999"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	methods := []string{http.MethodGet, http.MethodPost}

	for i := 0; i < 20; i++ {
		rec := ping(methods[i%2])
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d (%s): status = %d, want 200 (under burst)", i+1, methods[i%2], rec.Code)
		}
	}
	for _, method := range methods {
		rec := ping(method)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s after the burst: status = %d, want 429", method, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("%s 429 response Retry-After = %q, want %q", method, got, "1")
		}
	}
}
