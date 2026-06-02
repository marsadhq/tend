package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// httpClient is the shared HTTP client for all HTTP-based providers. The
// 10-second timeout prevents hung goroutines when an upstream is slow.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// postJSON marshals payload as JSON, POSTs it to rawURL with Content-Type:
// application/json, and returns a non-nil error if the response status is
// outside the 2xx range so callers can retry. Errors report only the host, not
// the full URL: webhook/Slack/Discord URLs embed a secret token in the path,
// and this error is surfaced to logs by the dispatcher.
func postJSON(ctx context.Context, rawURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain the body so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook POST %s: status %d", safeHost(rawURL), resp.StatusCode)
	}
	return nil
}

// safeHost returns the host of rawURL for use in error messages, so a secret
// token embedded in the URL path is never logged. Falls back to "<url>" if the
// URL cannot be parsed (never the raw URL itself).
func safeHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "<url>"
}

// SlackProvider delivers notifications to a Slack incoming webhook URL. The
// message is encoded as a Slack-compatible {"text": "…"} payload.
type SlackProvider struct{ url string }

// NewSlack constructs a SlackProvider that posts to the given incoming webhook URL.
func NewSlack(url string) *SlackProvider { return &SlackProvider{url} }

// Send posts the message subject and body joined by a newline to the Slack
// webhook. It returns an error when the HTTP response is non-2xx.
func (p *SlackProvider) Send(ctx context.Context, m Message) error {
	return postJSON(ctx, p.url, map[string]string{"text": m.Subject + "\n" + m.Body})
}

// DiscordProvider delivers notifications to a Discord webhook URL. The
// message is encoded as a Discord-compatible {"content": "…"} payload.
type DiscordProvider struct{ url string }

// NewDiscord constructs a DiscordProvider that posts to the given Discord webhook URL.
func NewDiscord(url string) *DiscordProvider { return &DiscordProvider{url} }

// Send posts the message subject and body joined by a newline to the Discord
// webhook. It returns an error when the HTTP response is non-2xx.
func (p *DiscordProvider) Send(ctx context.Context, m Message) error {
	return postJSON(ctx, p.url, map[string]string{"content": m.Subject + "\n" + m.Body})
}

// WebhookProvider delivers notifications to a generic HTTP endpoint. The
// message is encoded as a structured JSON payload with "subject", "body", and
// "event" fields, giving the receiver full access to the originating event.
type WebhookProvider struct{ url string }

// NewWebhook constructs a WebhookProvider that posts to the given URL.
func NewWebhook(url string) *WebhookProvider { return &WebhookProvider{url} }

// Send posts a structured JSON payload containing the subject, body, and
// originating event to the webhook URL. It returns an error when the HTTP
// response is non-2xx.
func (p *WebhookProvider) Send(ctx context.Context, m Message) error {
	return postJSON(ctx, p.url, map[string]any{"subject": m.Subject, "body": m.Body, "event": m.Event})
}

// Compile-time assertions: all three providers must satisfy the Provider interface.
var _ Provider = (*SlackProvider)(nil)
var _ Provider = (*DiscordProvider)(nil)
var _ Provider = (*WebhookProvider)(nil)
