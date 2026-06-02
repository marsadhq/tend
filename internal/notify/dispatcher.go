package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/secrets"
)

// alertable is the set of event types that may trigger notifications.
// notification.* is deliberately excluded so a delivery failure (which emits
// notification.failed) can never feed back into the dispatcher and start a loop.
// Non-alert lifecycle events (run.started, run.succeeded) are likewise absent,
// so the runner can fire on every terminal event and let this filter drop the
// non-failures.
var alertable = map[string]bool{
	"run.failed":          true, // timeouts surface as run.failed (status in payload)
	"heartbeat.missed":    true,
	"heartbeat.recovered": true,
}

// DispatchStore is the consumer-side persistence contract the dispatcher needs.
// Defined here (not imported from store) so notify never depends on store: the
// concrete store backends satisfy it structurally.
type DispatchStore interface {
	// MatchingRules returns the enabled rules for orgID where event_type matches
	// and job_id is either 0 (all jobs) or equal to jobID.
	MatchingRules(ctx context.Context, orgID int64, eventType string, jobID int64) ([]Rule, error)
	// GetChannel returns the channel plus its encrypted config blob.
	GetChannel(ctx context.Context, orgID, id int64) (Channel, string, error)
	// EmitEvent appends an event (used for notification.failed on exhaustion).
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
}

// Dispatcher consumes an alertable event, matches notification rules, and fans
// the alert out to each rule's channel with bounded retry. It holds the
// secrets.Box so it can decrypt channel config (decryption lives in notify, not
// the store) and a build func so providers and the sleep schedule are injectable
// in tests.
type Dispatcher struct {
	store    DispatchStore
	box      *secrets.Box
	build    func(ChannelType, []byte) (Provider, error)
	maxTries int
	sleep    func(context.Context, time.Duration) bool // injectable so retry tests don't really sleep; returns true if ctx cancelled
	log      *slog.Logger
}

// sleepCtx waits for d or until ctx is done. Returns true if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// NewDispatcher builds a Dispatcher with the production defaults: 4 delivery
// attempts and ctx-aware backoff.
func NewDispatcher(s DispatchStore, box *secrets.Box, build func(ChannelType, []byte) (Provider, error), log *slog.Logger) *Dispatcher {
	return &Dispatcher{store: s, box: box, build: build, maxTries: 4, sleep: sleepCtx, log: log}
}

// DispatchForEvent matches rules for ev and delivers the alert to each channel.
// It is safe to call on any event: non-alertable types (including all
// notification.* events) are dropped BEFORE any store query, which is the loop
// guard. Errors are logged, not returned, so a single bad channel never blocks
// the others.
func (d *Dispatcher) DispatchForEvent(ctx context.Context, ev core.Event) {
	if ctx.Err() != nil {
		return // shutting down: skip dispatch (and its store query) cleanly
	}
	if !alertable[ev.Type] {
		return // loop guard + ignore non-alert events, before any store access
	}

	rules, err := d.store.MatchingRules(ctx, ev.OrgID, ev.Type, jobIDFromPayload(ev))
	if err != nil {
		d.log.Error("notify: load rules", "err", err, "event", ev.Type)
		return
	}

	msg := messageFor(ev)
	for _, r := range rules {
		ch, blob, err := d.store.GetChannel(ctx, ev.OrgID, r.ChannelID)
		if err != nil {
			d.log.Info("notify: channel unavailable, skipping rule", "channel", r.ChannelID, "err", err)
			continue
		}
		cfg, err := d.box.Decrypt(blob)
		if err != nil {
			// Never log the blob or decrypted config - only the channel id.
			d.log.Error("notify: decrypt channel config", "err", err, "channel", ch.ID)
			continue
		}
		p, err := d.build(ch.Kind, cfg)
		if err != nil {
			d.log.Error("notify: build provider", "err", err, "channel", ch.ID)
			continue
		}
		d.deliver(ctx, p, msg, ch.ID)
	}
}

// deliver sends m via p, retrying up to maxTries with quadratic backoff
// (1s, 4s, 9s between attempts). On exhaustion it logs and emits a
// notification.failed event so a dropped alert is never silent. The emitted
// payload is only the failed event TYPE - no secret, no output.
// If ctx is cancelled before or during a backoff, deliver returns immediately
// without emitting notification.failed (a shutdown is not a delivery failure).
func (d *Dispatcher) deliver(ctx context.Context, p Provider, m Message, channelID int64) {
	tries := d.maxTries
	if tries < 1 {
		tries = 1
	}
	var err error
	for attempt := 1; attempt <= tries; attempt++ {
		if ctx.Err() != nil {
			return // context cancelled/deadline exceeded - not a delivery failure
		}
		if err = p.Send(ctx, m); err == nil {
			return
		}
		if attempt < tries {
			if d.sleep(ctx, time.Duration(attempt*attempt)*time.Second) {
				return // cancelled during backoff - not a delivery failure
			}
		}
	}
	d.log.Error("notify: delivery failed", "channel", channelID, "event", m.Event.Type, "err", err)
	_, _ = d.store.EmitEvent(ctx, core.Event{
		OrgID:   m.Event.OrgID,
		Type:    "notification.failed",
		Source:  "notify",
		Payload: m.Event.Type,
	})
}

// jobIDFromPayload extracts the job_id from a run.* event's JSON payload, or 0
// when the payload is absent, not JSON, or carries no job_id (e.g. heartbeat
// events whose payload is a plain name). A 0 job_id matches org-wide rules only.
func jobIDFromPayload(ev core.Event) int64 {
	if ev.Payload == "" {
		return 0
	}
	var p struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
		return 0
	}
	return p.JobID
}

// messageFor renders a Subject/Body for ev, tailored per event type and robust
// to odd payloads (no panic). The originating event is always carried on
// Message.Event for providers that want structured access.
func messageFor(ev core.Event) Message {
	switch ev.Type {
	case "run.failed":
		var p struct {
			JobName  string `json:"job_name"`
			Status   string `json:"status"`
			ExitCode int    `json:"exit_code"`
		}
		// Best-effort: an unparseable payload just yields the generic subject.
		_ = json.Unmarshal([]byte(ev.Payload), &p)
		subject := "Job failed"
		if p.JobName != "" {
			subject = "Job failed: " + p.JobName
		}
		status := p.Status
		if status == "" {
			status = "failed"
		}
		return Message{
			Subject: subject,
			Body:    fmt.Sprintf("status=%s exit_code=%d", status, p.ExitCode),
			Event:   ev,
		}

	case "heartbeat.missed":
		return Message{
			Subject: "Heartbeat missed: " + ev.Payload,
			Body:    ev.Payload,
			Event:   ev,
		}

	case "heartbeat.recovered":
		return Message{
			Subject: "Heartbeat recovered: " + ev.Payload,
			Body:    ev.Payload,
			Event:   ev,
		}

	default:
		return Message{Subject: ev.Type, Body: ev.Payload, Event: ev}
	}
}
