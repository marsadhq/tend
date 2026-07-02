package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// telegramAPIBase is the Telegram Bot API root. It is a package var so tests can
// point Send at an httptest server (the same seam idea as smtpSend).
var telegramAPIBase = "https://api.telegram.org"

// TelegramProvider delivers notifications to a Telegram chat via the Bot API.
// The bot token authenticates the request and lives in the URL path, so it is
// treated as a credential and never allowed into an error message.
type TelegramProvider struct {
	token  string
	chatID string
}

// NewTelegram constructs a TelegramProvider for the given bot token and chat id.
func NewTelegram(token, chatID string) *TelegramProvider {
	return &TelegramProvider{token: token, chatID: chatID}
}

// Send POSTs {chat_id, text} to the Bot API sendMessage method, where text is
// the subject and body joined by a newline (same composition as Slack/Discord).
// Any error is scrubbed of the bot token first: net/http embeds the full request
// URL (whose path contains the token) in transport errors.
func (p *TelegramProvider) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]string{
		"chat_id": p.chatID,
		"text":    m.Subject + "\n" + m.Body,
	})
	if err != nil {
		return err
	}
	rawURL := telegramAPIBase + "/bot" + p.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return p.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return p.scrub(err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram sendMessage: status %d", resp.StatusCode)
	}
	return nil
}

// scrub removes the bot token from an error so it can never reach logs.
func (p *TelegramProvider) scrub(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("telegram sendMessage: %s", strings.ReplaceAll(err.Error(), p.token, "***"))
}

// Compile-time assertion: TelegramProvider must satisfy the Provider interface.
var _ Provider = (*TelegramProvider)(nil)
