package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/core"
)

// Delivery state values, stored in deliveries.state.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// Delivery is one pending notification delivery: event x channel, plus the
// retry bookkeeping the worker drives. Rows are enqueued by the store in the
// SAME transaction as the event insert, so a matched notification survives a
// crash between emit and delivery.
type Delivery struct {
	ID            int64
	OrgID         int64
	EventID       int64
	ChannelID     int64
	Attempts      int    // attempts made so far, INCLUDING the one just claimed
	State         string // DeliveryPending | DeliveryDelivered | DeliveryFailed
	NextAttemptAt time.Time
	CreatedAt     time.Time
	Event         core.Event // the originating event, joined in by the store
}

// WorkerStore is the consumer-side persistence contract the delivery worker
// needs. Defined here (not imported from store) so notify never depends on
// store: the concrete store backends satisfy it structurally.
type WorkerStore interface {
	// ClaimDueDeliveries atomically claims up to limit pending deliveries whose
	// next_attempt_at <= now: attempts is incremented and next_attempt_at is
	// pushed to now+lease, so a crashed worker's claims become due again after
	// the lease (at-least-once, no stuck 'sending' state). A pending delivery
	// holds back the later ones on its channel, so a channel receives its
	// notifications in order, one at a time, but only until it was first
	// attempted more than hold ago; hold <= 0 turns ordering off.
	ClaimDueDeliveries(ctx context.Context, now time.Time, lease, hold time.Duration, limit int) ([]Delivery, error)
	// ReleaseHeldAlerts queues the failure alerts held back by the pacing
	// (see Pacing) whose interval has ended at now. The queue calls it
	// before each claim, so the worker's poll is what sends them.
	ReleaseHeldAlerts(ctx context.Context, now time.Time) (int, error)
	// MarkDeliveryDelivered finalizes a delivery as delivered.
	MarkDeliveryDelivered(ctx context.Context, id int64) error
	// RescheduleDelivery sets the next attempt time of a still-pending delivery.
	RescheduleDelivery(ctx context.Context, id int64, nextAttemptAt time.Time) error
	// FailDelivery finalizes a delivery as permanently failed.
	FailDelivery(ctx context.Context, id int64) error
	// FailDeliveryAndEmit finalizes a still-pending delivery as failed and
	// appends ev (notification.failed, with the deliveries its rules match) in
	// one transaction, so a given-up alert is never dropped silently. A
	// delivery that is no longer pending is left alone and ev is not written.
	FailDeliveryAndEmit(ctx context.Context, id int64, ev core.Event) error
	// GetChannel returns the channel plus its encrypted config blob.
	GetChannel(ctx context.Context, orgID, id int64) (Channel, string, error)
}

// EventNotificationFailed is the event recorded when the worker gives up on a
// delivery. Its payload is the type of the event that could not be delivered.
// It is alertable, so it can be routed to another channel; giving up on the
// delivery of a notification.failed never records another one (loop guard).
const EventNotificationFailed = "notification.failed"

// Worker is kit's durable delivery worker over Tend's deliveries: it claims
// due rows, sends each through its channel's provider, and either finalizes
// it (delivered), reschedules it with capped exponential backoff, or - when
// the destination refused it for good (kitnotify.IsPermanent) or once it is
// older than MaxAge - gives it up and records notification.failed, so a
// dropped alert is never silent.
//
// Delivery is asynchronous by design: emitters only enqueue (transactionally
// with the event insert) and optionally Nudge the worker, so a slow or down
// destination can never block a runner worker, an HTTP response, or the
// heartbeat watcher.
type Worker = kitnotify.Worker[Delivery]

// NewWorker builds the delivery worker over s with the production defaults:
// a 2s poll, a 1m lease, give-up after 24h, backoff capped at 5m and a
// kitnotify.DefaultHold (15m) per-channel hold. build makes a channel's
// provider from its kind and decrypted config (kitnotify.BuildProvider in
// production). instance names this installation in the marsad envelope of
// every notification (config.LoadInstance: TEND_INSTANCE or the hostname).
// chat shapes the text of the chat alerts (their links and output).
func NewWorker(s WorkerStore, box *secrets.Box, build func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error), instance string, chat ChatOptions, log *slog.Logger) *Worker {
	w := kitnotify.NewWorker[Delivery](queue{s}, sender(s, box, build, instance, chat), log)
	w.LogAttrs = func(d kitnotify.Delivery[Delivery]) []any {
		return []any{"channel", d.Item.ChannelID, "event", d.Item.Event.Type}
	}
	return w
}

// queue is kitnotify.Queue over Tend's deliveries table.
type queue struct{ s WorkerStore }

// Claim queues the held failure alerts that are due (ReleaseHeldAlerts),
// then claims due deliveries (see WorkerStore.ClaimDueDeliveries).
func (q queue) Claim(ctx context.Context, now time.Time, lease, hold time.Duration, limit int) ([]kitnotify.Delivery[Delivery], error) {
	if _, err := q.s.ReleaseHeldAlerts(ctx, now); err != nil {
		return nil, err
	}
	ds, err := q.s.ClaimDueDeliveries(ctx, now, lease, hold, limit)
	if err != nil {
		return nil, err
	}
	out := make([]kitnotify.Delivery[Delivery], len(ds))
	for i, d := range ds {
		out[i] = kitnotify.Delivery[Delivery]{ID: d.ID, Attempts: d.Attempts, CreatedAt: d.CreatedAt, Item: d}
	}
	return out, nil
}

// MarkSent finalizes d as delivered.
func (q queue) MarkSent(ctx context.Context, d kitnotify.Delivery[Delivery]) error {
	return q.s.MarkDeliveryDelivered(ctx, d.ID)
}

// Retry reschedules d to at.
func (q queue) Retry(ctx context.Context, d kitnotify.Delivery[Delivery], at time.Time) error {
	return q.s.RescheduleDelivery(ctx, d.ID, at)
}

// GiveUp finalizes d as failed and, in the same transaction, records
// notification.failed naming the type of the event that was not delivered
// (no secret, no output).
//
// Loop guard: a notification.failed that cannot be delivered is finalized
// and dropped. Recording another notification.failed for it could start a
// chain of them, one per failing channel and day.
func (q queue) GiveUp(ctx context.Context, d kitnotify.Delivery[Delivery], _ error) error {
	if d.Item.Event.Type == EventNotificationFailed {
		return q.s.FailDelivery(ctx, d.ID)
	}
	return q.s.FailDeliveryAndEmit(ctx, d.ID, core.Event{
		OrgID:   d.Item.OrgID,
		Type:    EventNotificationFailed,
		Source:  "notify",
		Payload: d.Item.Event.Type,
	})
}

// sender returns the worker's send step: load and decrypt the channel's
// config, build its provider, and send the rendered event. Its errors are
// logged, so they never carry the config blob or decrypted secrets (provider
// errors name only a destination's host).
func sender(s WorkerStore, box *secrets.Box, build func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error), instance string, chat ChatOptions) kitnotify.SendFunc[Delivery] {
	return func(ctx context.Context, kd kitnotify.Delivery[Delivery]) error {
		d := kd.Item
		ch, blob, err := s.GetChannel(ctx, d.OrgID, d.ChannelID)
		if err != nil {
			return fmt.Errorf("load channel %d: %w", d.ChannelID, err)
		}
		cfg, err := box.Decrypt(blob)
		if err != nil {
			// Never include the blob or decrypted config - only the channel id.
			return fmt.Errorf("decrypt channel %d config: %w", d.ChannelID, err)
		}
		p, err := build(ch.Kind, cfg)
		if err != nil {
			return fmt.Errorf("build provider for channel %d: %w", d.ChannelID, err)
		}
		return p.Send(ctx, channelMessage(ch.Kind, d.Event, instance, chat))
	}
}
