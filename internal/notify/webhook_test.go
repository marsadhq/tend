package notify_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/notify"
)

// captureServer returns a test server that captures the last request's method,
// Content-Type header, and body. The server responds with the given status code.
func captureServer(statusCode int) (srv *httptest.Server, method *string, contentType *string, body *string) {
	method = new(string)
	contentType = new(string)
	body = new(string)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*method = r.Method
		*contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		w.WriteHeader(statusCode)
	}))
	return srv, method, contentType, body
}

func TestSlackProviderPostsText(t *testing.T) {
	srv, gotMethod, gotContentType, gotBody := captureServer(200)
	defer srv.Close()

	ctx := context.Background()
	p := notify.NewSlack(srv.URL)
	err := p.Send(ctx, notify.Message{Subject: "Job failed", Body: "nightly-backup"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if *gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", *gotMethod)
	}
	if *gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", *gotContentType)
	}
	if !strings.Contains(*gotBody, `"text"`) {
		t.Errorf("body missing \"text\" key: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "nightly-backup") {
		t.Errorf("body missing body text: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "Job failed") {
		t.Errorf("body missing subject text: %s", *gotBody)
	}
}

func TestDiscordProviderPostsContent(t *testing.T) {
	srv, gotMethod, gotContentType, gotBody := captureServer(200)
	defer srv.Close()

	ctx := context.Background()
	p := notify.NewDiscord(srv.URL)
	err := p.Send(ctx, notify.Message{Subject: "Heartbeat missed", Body: "external-backup"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if *gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", *gotMethod)
	}
	if *gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", *gotContentType)
	}
	if !strings.Contains(*gotBody, `"content"`) {
		t.Errorf("body missing \"content\" key: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "external-backup") {
		t.Errorf("body missing body text: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "Heartbeat missed") {
		t.Errorf("body missing subject text: %s", *gotBody)
	}
}

func TestWebhookProviderPostsFields(t *testing.T) {
	srv, gotMethod, gotContentType, gotBody := captureServer(200)
	defer srv.Close()

	ctx := context.Background()
	p := notify.NewWebhook(srv.URL)
	err := p.Send(ctx, notify.Message{Subject: "Run failed", Body: "db-backup"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if *gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", *gotMethod)
	}
	if *gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", *gotContentType)
	}
	if !strings.Contains(*gotBody, `"subject"`) {
		t.Errorf("body missing \"subject\" key: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, `"body"`) {
		t.Errorf("body missing \"body\" key: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, `"event"`) {
		t.Errorf("body missing \"event\" key: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "Run failed") {
		t.Errorf("body missing subject value: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, "db-backup") {
		t.Errorf("body missing body value: %s", *gotBody)
	}
}

func TestWebhookProviderErrorsOnNon2xx(t *testing.T) {
	srv, _, _, _ := captureServer(500)
	defer srv.Close()

	ctx := context.Background()
	p := notify.NewWebhook(srv.URL)
	err := p.Send(ctx, notify.Message{Subject: "Test", Body: "test"})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestSlackProviderErrorsOnNon2xx(t *testing.T) {
	srv, _, _, _ := captureServer(403)
	defer srv.Close()

	ctx := context.Background()
	p := notify.NewSlack(srv.URL)
	err := p.Send(ctx, notify.Message{Subject: "Test", Body: "test"})
	if err == nil {
		t.Fatal("expected error on 403 response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status 403: %v", err)
	}
}

// TestErrorRedactsURLToken guards that a secret token embedded in the webhook
// URL path is NOT leaked into the error string (which the dispatcher logs).
func TestErrorRedactsURLToken(t *testing.T) {
	srv, _, _, _ := captureServer(500)
	defer srv.Close()

	ctx := context.Background()
	secretURL := srv.URL + "/services/T000/B000/SUPERSECRETTOKEN"
	err := notify.NewWebhook(secretURL).Send(ctx, notify.Message{Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
	if strings.Contains(err.Error(), "SUPERSECRETTOKEN") {
		t.Fatalf("error leaked secret URL token: %v", err)
	}
}

func TestWebhookProviderTransportError(t *testing.T) {
	// Start a server then immediately close it so the port is unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // close before sending

	ctx := context.Background()
	p := notify.NewWebhook(url)
	err := p.Send(ctx, notify.Message{Subject: "Test", Body: "test"})
	if err == nil {
		t.Fatal("expected transport error on closed server, got nil")
	}
}
