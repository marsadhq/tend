package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// poisonProvider fails every send of one event type, the way a receiver
// refuses a body it cannot read, and records what it delivered.
type poisonProvider struct {
	mu        sync.Mutex
	poison    string
	permanent bool // a 400-style refusal rather than a 500-style one
	delivered []string
}

func (p *poisonProvider) Send(_ context.Context, m notify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.Event.Type == p.poison {
		err := errors.New("webhook POST ward.example: refused")
		if p.permanent {
			return notify.Permanent(err)
		}
		return err
	}
	p.delivered = append(p.delivered, m.Event.Type)
	return nil
}

func (p *poisonProvider) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.delivered...)
}

// poisonedChannel sets up one channel with a run.failed queued ahead of a
// heartbeat.missed, and a worker whose provider is p.
func poisonedChannel(t *testing.T, ctx context.Context, s store.Store, p notify.Provider) (orgID int64, w *notify.Worker) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	box := testBox(t)
	chID, err := notify.CreateChannel(ctx, s, box, notify.Channel{OrgID: org.ID, Name: "ward", Kind: notify.Webhook}, []byte(`{"url":"https://ward.example/x"}`))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, ev := range []string{"run.failed", "heartbeat.missed"} {
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: ev, Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
	}
	for _, ev := range []core.Event{
		{OrgID: org.ID, Type: "run.failed", Source: "jobs.runner", Payload: `{"job_id":1,"job_name":"nightly"}`},
		heartbeat.MissedEvent(org.ID, "backup"),
	} {
		if _, err := s.EmitEvent(ctx, ev); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	w = notify.NewWorker(s, box, func(notify.ChannelType, []byte) (notify.Provider, error) { return p, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return org.ID, w
}

// TestRejectedHeadDoesNotBlockItsChannel: a delivery the receiver refuses for
// good (a 4xx) is given up on its first attempt, with notification.failed
// recorded, and the heartbeat.missed queued behind it goes out in the same
// pass. It used to be retried for 24 hours, holding the channel all along.
func TestRejectedHeadDoesNotBlockItsChannel(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := &poisonProvider{poison: "run.failed", permanent: true}
		orgID, w := poisonedChannel(t, ctx, s, p)

		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := p.sent(); len(got) != 1 || got[0] != "heartbeat.missed" {
			t.Fatalf("delivered %v in the first pass, want the heartbeat.missed behind the refused head", got)
		}
		failed := eventsOfType(t, ctx, s, orgID, "notification.failed")
		if len(failed) != 1 || failed[0].Payload != "run.failed" {
			t.Fatalf("notification.failed = %+v, want one for the refused run.failed", failed)
		}
		var pending int
		if err := store.RawDB(s).QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state = 'pending' AND event_id IN (SELECT id FROM events WHERE type <> 'notification.failed')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Fatalf("%d of the channel's deliveries still pending, want none", pending)
		}
	})
}
