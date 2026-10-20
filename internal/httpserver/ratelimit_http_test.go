package httpserver_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
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

// pingFrom sends one ping for token from addr through h.
func pingFrom(h http.Handler, method, token, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/ping/"+token, nil)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPingFleetFromOneHostIsNotThrottled: one host running many heartbeats
// that all ping on the same minute must get every ping through. The old
// per-address bucket (a burst of 20) answered the 21st with 429, which a
// client that does not retry turns into a false missed heartbeat.
func TestPingFleetFromOneHostIsNotThrottled(t *testing.T) {
	ts := newStore(t)
	const fleet = 200
	for i := 0; i < fleet; i++ {
		ts.seedHB(t, fmt.Sprintf("cron-%d", i), fmt.Sprintf("tok-fleet-%d", i), "up")
	}
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // all at the same instant
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()

	for i := 0; i < fleet; i++ {
		if rec := pingFrom(h, http.MethodPost, fmt.Sprintf("tok-fleet-%d", i), "198.51.100.3:9999"); rec.Code != http.StatusOK {
			t.Fatalf("ping %d of %d from one host: status = %d, want 200", i+1, fleet, rec.Code)
		}
	}
}

// TestPingGuessingIsCutOff: an address that keeps sending unknown tokens is
// refused once its budget of misses is spent, for real tokens too, before
// anything reaches the database; other addresses are unaffected, and the
// budget refills at one a second.
func TestPingGuessingIsCutOff(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "api", "tok-real", "up")
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()
	const guesser = "203.0.113.7:1111"

	for i := 0; i < 30; i++ {
		if rec := pingFrom(h, http.MethodGet, fmt.Sprintf("guess-%d", i), guesser); rec.Code != http.StatusNotFound {
			t.Fatalf("guess %d: status = %d, want 404 (within the miss budget)", i+1, rec.Code)
		}
	}
	for _, token := range []string{"guess-next", "tok-real"} {
		rec := pingFrom(h, http.MethodPost, token, guesser)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s after 30 misses: status = %d, want 429", token, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("429 Retry-After = %q, want 1", got)
		}
	}
	if rec := pingFrom(h, http.MethodPost, "tok-real", "192.0.2.50:2222"); rec.Code != http.StatusOK {
		t.Fatalf("a real ping from another address: status = %d, want 200", rec.Code)
	}
	fk.Advance(time.Second)
	if rec := pingFrom(h, http.MethodPost, "tok-real", guesser); rec.Code != http.StatusOK {
		t.Fatalf("a real ping after the budget refilled: status = %d, want 200", rec.Code)
	}
}

// TestPingRunawayTokenIsThrottled: one heartbeat pinged flat out is held to
// its own budget (GET and POST alike), without touching other heartbeats on
// the same host.
func TestPingRunawayTokenIsThrottled(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "loop", "tok-loop", "up")
	ts.seedHB(t, "calm", "tok-calm", "up")
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()
	const host = "198.51.100.3:9999"
	methods := []string{http.MethodGet, http.MethodPost}

	for i := 0; i < 10; i++ {
		if rec := pingFrom(h, methods[i%2], "tok-loop", host); rec.Code != http.StatusOK {
			t.Fatalf("ping %d: status = %d, want 200 (within the token's burst)", i+1, rec.Code)
		}
	}
	for _, method := range methods {
		rec := pingFrom(h, method, "tok-loop", host)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s after the burst: status = %d, want 429", method, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("%s 429 Retry-After = %q, want 1", method, got)
		}
	}
	if rec := pingFrom(h, http.MethodPost, "tok-calm", host); rec.Code != http.StatusOK {
		t.Fatalf("another heartbeat on the same host: status = %d, want 200", rec.Code)
	}
}
