package notify

// These tests are in package notify (internal) so they can set the worker's
// unexported fields (now, build) directly, keeping backoff/give-up tests fast
// without touching the network or a real database.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/secrets"
)

// fakeWorkerStore is an in-memory WorkerStore. It records every call so tests
// can assert the worker's finalize/reschedule decisions.
type fakeWorkerStore struct {
	due []Delivery // returned (and cleared) by the next ClaimDueDeliveries

	channel Channel
	blob    string
	getErr  error

	claimed     []Delivery
	delivered   []int64
	rescheduled map[int64]time.Time
	failed      []int64
	emitted     []core.Event
}

func (f *fakeWorkerStore) ClaimDueDeliveries(_ context.Context, _ time.Time, _ time.Duration, _ int) ([]Delivery, error) {
	ds := f.due
	f.due = nil
	for i := range ds {
		ds[i].Attempts++ // mirror the real claim: attempts includes this one
	}
	f.claimed = append(f.claimed, ds...)
	return ds, nil
}

func (f *fakeWorkerStore) MarkDeliveryDelivered(_ context.Context, id int64) error {
	f.delivered = append(f.delivered, id)
	return nil
}

func (f *fakeWorkerStore) RescheduleDelivery(_ context.Context, id int64, at time.Time) error {
	if f.rescheduled == nil {
		f.rescheduled = map[int64]time.Time{}
	}
	f.rescheduled[id] = at
	return nil
}

func (f *fakeWorkerStore) FailDelivery(_ context.Context, id int64) error {
	f.failed = append(f.failed, id)
	return nil
}

func (f *fakeWorkerStore) GetChannel(_ context.Context, _, _ int64) (Channel, string, error) {
	if f.getErr != nil {
		return Channel{}, "", f.getErr
	}
	return f.channel, f.blob, nil
}

func (f *fakeWorkerStore) EmitEvent(_ context.Context, e core.Event) (int64, error) {
	f.emitted = append(f.emitted, e)
	return int64(len(f.emitted)), nil
}

// fakeProvider is a scripted Provider: it fails the first failN Send calls and
// succeeds thereafter, counting every call.
type fakeProvider struct {
	failN int
	calls int
	last  Message
}

func (p *fakeProvider) Send(_ context.Context, m Message) error {
	p.calls++
	p.last = m
	if p.calls <= p.failN {
		return errors.New("boom")
	}
	return nil
}

// discardLogger returns a slog.Logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testBox returns a Box keyed with an all-zero 32-byte master key (test-only).
func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// newTestWorker wires a worker over the fake store with a build func that
// always returns p. The box encrypts a dummy config so GetChannel + Decrypt
// round-trips work.
func newTestWorker(t *testing.T, fs *fakeWorkerStore, p Provider) *Worker {
	t.Helper()
	box := testBox(t)
	blob, err := box.Encrypt([]byte(`{"url":"https://example/x"}`))
	if err != nil {
		t.Fatalf("encrypt blob: %v", err)
	}
	fs.blob = blob
	w := NewWorker(fs, box, func(ChannelType, []byte) (Provider, error) { return p, nil }, discardLogger())
	return w
}

func due(id int64, evType, payload string, createdAt time.Time) Delivery {
	return Delivery{
		ID: id, OrgID: 1, EventID: id, ChannelID: 7, State: DeliveryPending,
		CreatedAt: createdAt,
		Event:     core.Event{ID: id, OrgID: 1, Type: evType, Payload: payload},
	}
}

func TestWorkerDeliversAndFinalizes(t *testing.T) {
	fs := &fakeWorkerStore{due: []Delivery{due(1, "heartbeat.missed", "db-backup", time.Now())}}
	p := &fakeProvider{}
	w := newTestWorker(t, fs, p)

	n, err := w.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 1 || p.calls != 1 {
		t.Fatalf("delivered=%d sends=%d, want 1/1", n, p.calls)
	}
	if len(fs.delivered) != 1 || fs.delivered[0] != 1 {
		t.Errorf("MarkDeliveryDelivered calls: %v", fs.delivered)
	}
	if len(fs.failed) != 0 || len(fs.rescheduled) != 0 || len(fs.emitted) != 0 {
		t.Errorf("unexpected finalizations: failed=%v rescheduled=%v emitted=%v",
			fs.failed, fs.rescheduled, fs.emitted)
	}
	if p.last.Subject != "Heartbeat missed: db-backup" {
		t.Errorf("rendered subject: %q", p.last.Subject)
	}
}

func TestWorkerReschedulesWithCappedBackoff(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "run.failed", `{"job_name":"x"}`, now)}}
	p := &fakeProvider{failN: 1000}
	w := newTestWorker(t, fs, p)
	w.now = func() time.Time { return now }

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	at, ok := fs.rescheduled[1]
	if !ok {
		t.Fatal("failed delivery was not rescheduled")
	}
	// First attempt -> 1s backoff.
	if got := at.Sub(now); got != time.Second {
		t.Errorf("first backoff: got %v want 1s", got)
	}
	if len(fs.failed) != 0 || len(fs.emitted) != 0 {
		t.Errorf("young delivery must not be finalized: failed=%v emitted=%v", fs.failed, fs.emitted)
	}

	// A high attempt count must cap at BackoffCap.
	d := due(2, "run.failed", "", now)
	d.Attempts = 40 // as if claimed 40 times already
	fs.due = []Delivery{d}
	// Fake claim adds 1; backoff(41) must equal the cap.
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if got := fs.rescheduled[2].Sub(now); got != w.BackoffCap {
		t.Errorf("capped backoff: got %v want %v", got, w.BackoffCap)
	}
}

func TestWorkerGivesUpAfterMaxAgeAndEmitsNotificationFailed(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour) // older than the 24h MaxAge
	fs := &fakeWorkerStore{due: []Delivery{due(1, "heartbeat.missed", "db-backup", old)}}
	p := &fakeProvider{failN: 1000}
	w := newTestWorker(t, fs, p)
	w.now = func() time.Time { return now }

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(fs.failed) != 1 || fs.failed[0] != 1 {
		t.Fatalf("FailDelivery calls: %v", fs.failed)
	}
	if len(fs.emitted) != 1 {
		t.Fatalf("emitted events: %d, want 1 notification.failed", len(fs.emitted))
	}
	ev := fs.emitted[0]
	if ev.Type != "notification.failed" || ev.Payload != "heartbeat.missed" || ev.OrgID != 1 {
		t.Errorf("notification.failed event: %+v", ev)
	}
	if len(fs.rescheduled) != 0 {
		t.Errorf("expired delivery must not be rescheduled: %v", fs.rescheduled)
	}
}

// TestAlertableLoopGuard pins the contract the store's transactional enqueue
// relies on: notification.* and non-terminal lifecycle events never enqueue.
func TestAlertableLoopGuard(t *testing.T) {
	for typ, want := range map[string]bool{
		"run.failed":          true,
		"heartbeat.missed":    true,
		"heartbeat.recovered": true,
		"run.succeeded":       false,
		"run.started":         false,
		"notification.failed": false,
	} {
		if got := Alertable(typ); got != want {
			t.Errorf("Alertable(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestEventJobID(t *testing.T) {
	if got := EventJobID(core.Event{Payload: `{"job_id":42}`}); got != 42 {
		t.Errorf("job_id payload: got %d want 42", got)
	}
	if got := EventJobID(core.Event{Payload: "db-backup"}); got != 0 {
		t.Errorf("plain payload: got %d want 0", got)
	}
	if got := EventJobID(core.Event{}); got != 0 {
		t.Errorf("empty payload: got %d want 0", got)
	}
}
