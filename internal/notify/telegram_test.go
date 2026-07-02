package notify

// Internal test: it flips the unexported telegramAPIBase seam to point Send at
// an httptest server. These tests mutate that package global and must not run in
// parallel with each other.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTelegramSendsChatIDAndText verifies Send POSTs JSON {chat_id, text} to the
// Bot API sendMessage method, where text is Subject + "\n" + Body (the same
// composition Slack/Discord use).
func TestTelegramSendsChatIDAndText(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	p := NewTelegram("123456:ABC-DEF", "-1001234567890")
	if err := p.Send(context.Background(), Message{Subject: "Job failed: backup", Body: "status=failed exit_code=3"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/bot123456:ABC-DEF/sendMessage" {
		t.Errorf("path = %q, want /bot123456:ABC-DEF/sendMessage", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, gotBody)
	}
	if payload["chat_id"] != "-1001234567890" {
		t.Errorf("chat_id = %q, want -1001234567890", payload["chat_id"])
	}
	if payload["text"] != "Job failed: backup\nstatus=failed exit_code=3" {
		t.Errorf("text = %q, want %q", payload["text"], "Job failed: backup\nstatus=failed exit_code=3")
	}
}

// TestTelegramErrorNeverLeaksToken guards the credential: neither a non-2xx
// response nor a transport error (net/http embeds the full URL, which contains
// the token in its path) may surface the bot token.
func TestTelegramErrorNeverLeaksToken(t *testing.T) {
	const token = "7777777:SUPERSECRETBOTTOKEN"
	old := telegramAPIBase
	defer func() { telegramAPIBase = old }()
	p := NewTelegram(token, "1")

	// (a) non-2xx response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	telegramAPIBase = srv.URL
	err := p.Send(context.Background(), Message{Subject: "s", Body: "b"})
	srv.Close()
	if err == nil {
		t.Fatal("expected an error on a non-2xx response")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("non-2xx error leaked the bot token: %v", err)
	}

	// (b) transport error: an unreachable base makes net/http embed the full URL
	// (path includes the token) in its error.
	telegramAPIBase = "http://127.0.0.1:1"
	err = p.Send(context.Background(), Message{Subject: "s", Body: "b"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("transport error leaked the bot token: %v", err)
	}
}
