package notify

import (
	"encoding/json"
	"fmt"
)

// BuildProvider constructs the concrete Provider for a channel kind from its
// decrypted JSON configuration. It is the production wiring used by the
// dispatcher (a field on Dispatcher, injectable for tests).
//
// It returns an error on malformed JSON, an unknown kind, or a missing required
// field, so a misconfigured channel is skipped (and logged) rather than panicking
// or sending to an empty destination.
func BuildProvider(kind ChannelType, cfg []byte) (Provider, error) {
	switch kind {
	case Webhook:
		var c struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("notify: webhook config: %w", err)
		}
		if c.URL == "" {
			return nil, fmt.Errorf("notify: webhook config: missing url")
		}
		return NewWebhook(c.URL), nil

	case Slack:
		var c struct {
			WebhookURL string `json:"webhook_url"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("notify: slack config: %w", err)
		}
		if c.WebhookURL == "" {
			return nil, fmt.Errorf("notify: slack config: missing webhook_url")
		}
		return NewSlack(c.WebhookURL), nil

	case Discord:
		var c struct {
			WebhookURL string `json:"webhook_url"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("notify: discord config: %w", err)
		}
		if c.WebhookURL == "" {
			return nil, fmt.Errorf("notify: discord config: missing webhook_url")
		}
		return NewDiscord(c.WebhookURL), nil

	case SMTP:
		var c SMTPConfig
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("notify: smtp config: %w", err)
		}
		if c.Host == "" {
			return nil, fmt.Errorf("notify: smtp config: missing host")
		}
		if c.From == "" {
			return nil, fmt.Errorf("notify: smtp config: missing from")
		}
		if len(c.To) == 0 {
			return nil, fmt.Errorf("notify: smtp config: missing recipients")
		}
		return NewSMTP(c), nil

	case Telegram:
		var c struct {
			BotToken string `json:"bot_token"`
			ChatID   string `json:"chat_id"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("notify: telegram config: %w", err)
		}
		if c.BotToken == "" {
			return nil, fmt.Errorf("notify: telegram config: missing bot_token")
		}
		if c.ChatID == "" {
			return nil, fmt.Errorf("notify: telegram config: missing chat_id")
		}
		return NewTelegram(c.BotToken, c.ChatID), nil

	default:
		return nil, fmt.Errorf("notify: unknown channel kind %q", kind)
	}
}
