package notify_test

import (
	"testing"

	"github.com/marsadhq/tend/internal/notify"
)

// TestBuildProvider asserts each channel kind maps to the right concrete
// provider type, and that malformed/unknown configs error.
func TestBuildProvider(t *testing.T) {
	cases := []struct {
		name string
		kind notify.ChannelType
		cfg  string
		want any // concrete provider type expected (by type switch below)
	}{
		{"webhook", notify.Webhook, `{"url":"https://hooks.example/wh"}`, (*notify.WebhookProvider)(nil)},
		{"slack", notify.Slack, `{"webhook_url":"https://hooks.slack.com/x"}`, (*notify.SlackProvider)(nil)},
		{"discord", notify.Discord, `{"webhook_url":"https://discord.com/api/webhooks/x"}`, (*notify.DiscordProvider)(nil)},
		{"smtp", notify.SMTP, `{"host":"smtp.example","port":587,"username":"u","password":"p","from":"a@x","to":["b@x"]}`, (*notify.SMTPProvider)(nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := notify.BuildProvider(c.kind, []byte(c.cfg))
			if err != nil {
				t.Fatalf("BuildProvider(%s): %v", c.kind, err)
			}
			if p == nil {
				t.Fatalf("BuildProvider(%s): nil provider", c.kind)
			}
			switch c.want.(type) {
			case *notify.WebhookProvider:
				if _, ok := p.(*notify.WebhookProvider); !ok {
					t.Fatalf("kind %s: got %T want *WebhookProvider", c.kind, p)
				}
			case *notify.SlackProvider:
				if _, ok := p.(*notify.SlackProvider); !ok {
					t.Fatalf("kind %s: got %T want *SlackProvider", c.kind, p)
				}
			case *notify.DiscordProvider:
				if _, ok := p.(*notify.DiscordProvider); !ok {
					t.Fatalf("kind %s: got %T want *DiscordProvider", c.kind, p)
				}
			case *notify.SMTPProvider:
				if _, ok := p.(*notify.SMTPProvider); !ok {
					t.Fatalf("kind %s: got %T want *SMTPProvider", c.kind, p)
				}
			}
		})
	}
}

// TestBuildProviderErrors covers unknown kinds, malformed JSON, and missing
// required fields.
func TestBuildProviderErrors(t *testing.T) {
	cases := []struct {
		name string
		kind notify.ChannelType
		cfg  string
	}{
		{"unknown kind", notify.ChannelType("carrier-pigeon"), `{}`},
		{"webhook malformed json", notify.Webhook, `{"url":`},
		{"webhook missing url", notify.Webhook, `{}`},
		{"slack missing webhook_url", notify.Slack, `{}`},
		{"discord missing webhook_url", notify.Discord, `{}`},
		{"smtp missing host", notify.SMTP, `{"port":587,"from":"a@x","to":["b@x"]}`},
		{"smtp missing from", notify.SMTP, `{"host":"smtp.example","port":587,"to":["b@x"]}`},
		{"smtp no recipients", notify.SMTP, `{"host":"smtp.example","port":587,"from":"a@x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := notify.BuildProvider(c.kind, []byte(c.cfg)); err == nil {
				t.Fatalf("BuildProvider(%s, %q): expected error, got nil", c.kind, c.cfg)
			}
		})
	}
}
