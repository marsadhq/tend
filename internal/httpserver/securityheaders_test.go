package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/httpserver"
)

// wantSecurityHeaders asserts the four defensive headers are present with
// their exact expected values on the given response.
func wantSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q want %q", got, "nosniff")
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q want %q", got, "DENY")
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy: got %q want %q", got, "no-referrer")
	}
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy: missing")
	}
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing directive %q: %s", want, csp)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("did not expect an HSTS header (serve is plain-HTTP by default)")
	}
}

func TestSecurityHeaders_Public(t *testing.T) {
	ts := newStore(t)
	srv := startServer(t, ts.store, nil)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	wantSecurityHeaders(t, resp.Header)
}

func TestSecurityHeaders_Ping(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "api", "tok-known", "up")
	srv := startServer(t, ts.store, nil)

	resp, err := http.Get(srv.URL + "/ping/tok-known")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()
	wantSecurityHeaders(t, resp.Header)
}

func TestSecurityHeaders_AuthedPage(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	wantSecurityHeaders(t, rec.Header())
}

// TestDashboard_DisablesHTMXInlineStyles guards the strict style-src 'self'.
// htmx injects an inline <style> for its request indicator when it loads, which
// that policy blocks (a CSP violation logged on every page). The base layout
// must switch the injection off through the htmx-config meta tag.
func TestDashboard_DisablesHTMXInlineStyles(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("CSP no longer restricts styles to 'self': %s", csp)
	}
	const want = `<meta name="htmx-config" content='{"includeIndicatorStyles":false}'>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("dashboard page missing %s; htmx would inject an inline <style> the CSP blocks", want)
	}
}

func TestSecurityHeaders_LoginForm(t *testing.T) {
	ts := newStore(t)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	wantSecurityHeaders(t, rec.Header())
}
