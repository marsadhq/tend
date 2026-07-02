// Package notify defines the notification domain: channel types, the provider
// contract that delivers messages to external destinations (webhook, Slack,
// Discord, SMTP), and the encryption helpers for channel configuration.
//
// Layering: notify depends only on core (+ stdlib + secrets). It MUST NOT import
// store or jobs. The store package imports notify (for notify.Channel), and the
// channel persistence helpers below talk to the store through the consumer-side
// ChannelStore interface, never by importing store - so there is no cycle.
package notify

import (
	"context"

	"github.com/marsadhq/tend/internal/core"
)

// ChannelType identifies the kind of destination a notification channel
// delivers to. It is stored in the notification_channels.kind column.
type ChannelType string

const (
	// Webhook posts JSON to an arbitrary HTTP endpoint.
	Webhook ChannelType = "webhook"
	// Slack posts to a Slack incoming webhook.
	Slack ChannelType = "slack"
	// Discord posts to a Discord webhook.
	Discord ChannelType = "discord"
	// SMTP sends email.
	SMTP ChannelType = "smtp"
	// Telegram posts to a chat via the Telegram Bot API.
	Telegram ChannelType = "telegram"
)

// Message is a notification ready to be delivered by a Provider. It carries a
// rendered subject/body plus the originating event for providers that want
// structured access to it.
type Message struct {
	Subject string
	Body    string
	Event   core.Event
}

// Provider delivers a Message to a single destination. Implementations should
// return a non-nil error on any non-success so the dispatcher can retry.
type Provider interface {
	Send(ctx context.Context, m Message) error
}
