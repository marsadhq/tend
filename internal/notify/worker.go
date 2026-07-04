package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/secrets"
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
	// the lease (at-least-once, no stuck 'sending' state).
	ClaimDueDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Delivery, error)
	// MarkDeliveryDelivered finalizes a delivery as delivered.
	MarkDeliveryDelivered(ctx context.Context, id int64) error
	// RescheduleDelivery sets the next attempt time of a still-pending delivery.
	RescheduleDelivery(ctx context.Context, id int64, nextAttemptAt time.Time) error
	// FailDelivery finalizes a delivery as permanently failed.
	FailDelivery(ctx context.Context, id int64) error
	// GetChannel returns the channel plus its encrypted config blob.
	GetChannel(ctx context.Context, orgID, id int64) (Channel, string, error)
	// EmitEvent appends an event (used for notification.failed on exhaustion).
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
}

// claimBatch bounds how many due deliveries one drain pass claims at a time.
const claimBatch = 32

// Worker drains the durable deliveries queue: claim due rows, send each via
// its channel's provider, and either finalize (delivered), reschedule with
// capped exponential backoff, or - once a delivery is older than MaxAge -
// give up and emit notification.failed so a dropped alert is never silent.
//
// Delivery is asynchronous by design: emitters only enqueue (transactionally
// with the event insert) and optionally Nudge the worker, so a slow or down
// destination can never block a runner worker, an HTTP response, or the
// heartbeat watcher.
type Worker struct {
	store WorkerStore
	box   *secrets.Box
	build func(ChannelType, []byte) (Provider, error)
	log   *slog.Logger
	now   func() time.Time // injectable so backoff/give-up tests don't sleep

	// Interval is the poll cadence between drain passes; default 2s.
	Interval time.Duration
	// Lease is how long a claim holds before the delivery becomes due again;
	// it must exceed the slowest single send (HTTP timeout is 10s). Default 1m.
	Lease time.Duration
	// MaxAge is how long a delivery may keep failing before the worker gives
	// up and emits notification.failed. Default 24h.
	MaxAge time.Duration
	// BackoffCap caps the exponential retry backoff. Default 5m.
	BackoffCap time.Duration

	nudge chan struct{}
}

// NewWorker builds a delivery worker with the production defaults.
func NewWorker(s WorkerStore, box *secrets.Box, build func(ChannelType, []byte) (Provider, error), log *slog.Logger) *Worker {
	return &Worker{
		store:      s,
		box:        box,
		build:      build,
		log:        log,
		now:        time.Now,
		Interval:   2 * time.Second,
		Lease:      time.Minute,
		MaxAge:     24 * time.Hour,
		BackoffCap: 5 * time.Minute,
		nudge:      make(chan struct{}, 1),
	}
}

// Nudge wakes the worker for a prompt drain pass after an emitter enqueued a
// delivery. It never blocks: a nudge while one is already pending is a no-op
// (the pending pass will see the new row anyway).
func (w *Worker) Nudge() {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Run drains the queue until ctx is cancelled, waking every Interval or on a
// Nudge. Drain errors are logged, never fatal: the next pass retries.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if _, err := w.DrainOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("notify: drain deliveries", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.nudge:
		}
	}
}

// DrainOnce claims and processes every currently-due delivery, returning the
// number successfully delivered. It stops early (without error) when ctx is
// cancelled mid-batch; claimed-but-unsent deliveries simply become due again
// after the lease.
func (w *Worker) DrainOnce(ctx context.Context) (int, error) {
	delivered := 0
	for {
		if ctx.Err() != nil {
			return delivered, nil
		}
		ds, err := w.store.ClaimDueDeliveries(ctx, w.now(), w.Lease, claimBatch)
		if err != nil {
			return delivered, err
		}
		if len(ds) == 0 {
			return delivered, nil
		}
		for _, d := range ds {
			if ctx.Err() != nil {
				return delivered, nil
			}
			if w.deliverOne(ctx, d) {
				delivered++
			}
		}
	}
}

// deliverOne attempts a single claimed delivery and finalizes/reschedules it.
// It returns true only when the send succeeded.
func (w *Worker) deliverOne(ctx context.Context, d Delivery) bool {
	err := w.send(ctx, d)
	if err == nil {
		if merr := w.store.MarkDeliveryDelivered(ctx, d.ID); merr != nil {
			// The send DID happen; a lease-expiry redelivery is possible
			// (at-least-once). Log so the duplicate is explicable.
			w.log.Error("notify: mark delivered", "delivery", d.ID, "err", merr)
		}
		return true
	}
	if ctx.Err() != nil {
		return false // shutdown mid-send: the lease expiry retries, not a failure
	}

	if w.now().Sub(d.CreatedAt) >= w.MaxAge {
		// Errors from send never include channel secrets (postJSON redacts the
		// URL; decrypt errors carry no blob), so logging err here is safe.
		w.log.Error("notify: delivery failed permanently",
			"delivery", d.ID, "channel", d.ChannelID, "event", d.Event.Type, "attempts", d.Attempts, "err", err)
		if ferr := w.store.FailDelivery(ctx, d.ID); ferr != nil {
			w.log.Error("notify: fail delivery", "delivery", d.ID, "err", ferr)
			return false
		}
		// The emitted payload is only the failed event TYPE - no secret, no
		// output. notification.failed is not Alertable, so this can never
		// enqueue a delivery of its own (loop guard).
		_, _ = w.store.EmitEvent(ctx, core.Event{
			OrgID:   d.OrgID,
			Type:    "notification.failed",
			Source:  "notify",
			Payload: d.Event.Type,
		})
		return false
	}

	w.log.Warn("notify: delivery attempt failed",
		"delivery", d.ID, "channel", d.ChannelID, "event", d.Event.Type, "attempt", d.Attempts, "err", err)
	if rerr := w.store.RescheduleDelivery(ctx, d.ID, w.now().Add(w.backoff(d.Attempts))); rerr != nil {
		w.log.Error("notify: reschedule delivery", "delivery", d.ID, "err", rerr)
	}
	return false
}

// send loads + decrypts the channel config, builds the provider, and performs
// one send attempt. Returned errors are safe to log: they never carry the
// config blob or decrypted secrets.
func (w *Worker) send(ctx context.Context, d Delivery) error {
	ch, blob, err := w.store.GetChannel(ctx, d.OrgID, d.ChannelID)
	if err != nil {
		return fmt.Errorf("load channel %d: %w", d.ChannelID, err)
	}
	cfg, err := w.box.Decrypt(blob)
	if err != nil {
		// Never include the blob or decrypted config - only the channel id.
		return fmt.Errorf("decrypt channel %d config: %w", d.ChannelID, err)
	}
	p, err := w.build(ch.Kind, cfg)
	if err != nil {
		return fmt.Errorf("build provider for channel %d: %w", d.ChannelID, err)
	}
	return p.Send(ctx, messageFor(d.Event))
}

// backoff returns the wait before the next attempt after `attempts` tries:
// 1s, 2s, 4s, ... doubling up to BackoffCap.
func (w *Worker) backoff(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < w.BackoffCap; i++ {
		d *= 2
	}
	if d > w.BackoffCap {
		d = w.BackoffCap
	}
	return d
}
