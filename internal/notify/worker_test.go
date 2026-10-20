package notify

// The worker loop itself is kit/notify's and is tested there. These tests
// drive it through Tend's wiring (NewWorker: the queue over WorkerStore, the
// send step, the log attributes) with a fake clock, so the backoff, give-up,
// loop-guard and log behaviour Tend has always had stays pinned, without
// touching the network or a real database.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/core"
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

func (f *fakeWorkerStore) ClaimDueDeliveries(_ context.Context, _ time.Time, _, _ time.Duration, _ int) ([]Delivery, error) {
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

func (f *fakeWorkerStore) FailDeliveryAndEmit(_ context.Context, id int64, e core.Event) error {
	f.failed = append(f.failed, id)
	f.emitted = append(f.emitted, e)
	return nil
}

func (f *fakeWorkerStore) GetChannel(_ context.Context, _, _ int64) (Channel, string, error) {
	if f.getErr != nil {
		return Channel{}, "", f.getErr
	}
	return f.channel, f.blob, nil
}

// fakeProvider is a scripted Provider: it fails the first failN Send calls and
// succeeds thereafter, counting every call.
type fakeProvider struct {
	failN int
	calls int
	last  kitnotify.Message
}

func (p *fakeProvider) Send(_ context.Context, m kitnotify.Message) error {
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
func newTestWorker(t *testing.T, fs *fakeWorkerStore, p kitnotify.Provider) *Worker {
	t.Helper()
	box := testBox(t)
	blob, err := box.Encrypt([]byte(`{"url":"https://example/x"}`))
	if err != nil {
		t.Fatalf("encrypt blob: %v", err)
	}
	fs.blob = blob
	w := NewWorker(fs, box, func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error) { return p, nil }, "primary", discardLogger())
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
	w.Clock = clock.NewFake(now)

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
	w.Clock = clock.NewFake(now)

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

// cancelingProvider cancels the worker's context from inside Send and then
// fails with the context's error, the way a send interrupted by shutdown does.
type cancelingProvider struct {
	cancel context.CancelFunc
	calls  int
}

func (p *cancelingProvider) Send(ctx context.Context, _ kitnotify.Message) error {
	p.calls++
	p.cancel()
	return ctx.Err()
}

// TestWorkerShutdownIsNotAFailure pins that a shutdown is not a delivery
// failure. The deliveries are older than MaxAge, so a genuine send error would
// be final (FailDelivery + notification.failed); an error caused by the
// context being cancelled must instead leave the delivery exactly as claimed,
// for the lease expiry to retry after the restart.
func TestWorkerShutdownIsNotAFailure(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)

	untouched := func(t *testing.T, fs *fakeWorkerStore) {
		t.Helper()
		if len(fs.delivered) != 0 || len(fs.failed) != 0 || len(fs.rescheduled) != 0 || len(fs.emitted) != 0 {
			t.Errorf("shutdown must not finalize or reschedule anything: delivered=%v failed=%v rescheduled=%v emitted=%v",
				fs.delivered, fs.failed, fs.rescheduled, fs.emitted)
		}
	}

	t.Run("cancelled mid-send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fs := &fakeWorkerStore{due: []Delivery{
			due(1, "heartbeat.missed", "db-backup", old),
			due(2, "heartbeat.missed", "db-backup", old),
		}}
		p := &cancelingProvider{cancel: cancel}
		w := newTestWorker(t, fs, p)
		w.Clock = clock.NewFake(now)

		n, err := w.DrainOnce(ctx)
		if err != nil || n != 0 {
			t.Fatalf("DrainOnce = %d, %v; want 0, nil", n, err)
		}
		// The first send is interrupted; the rest of the batch is not attempted.
		if p.calls != 1 {
			t.Errorf("sends = %d, want 1", p.calls)
		}
		untouched(t, fs)
	})

	t.Run("cancelled before the pass", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fs := &fakeWorkerStore{due: []Delivery{due(1, "heartbeat.missed", "db-backup", old)}}
		p := &fakeProvider{failN: 1000}
		w := newTestWorker(t, fs, p)
		w.Clock = clock.NewFake(now)

		n, err := w.DrainOnce(ctx)
		if err != nil || n != 0 {
			t.Fatalf("DrainOnce = %d, %v; want 0, nil", n, err)
		}
		if p.calls != 0 || len(fs.claimed) != 0 {
			t.Errorf("a cancelled pass must not claim or send: claimed=%d sends=%d", len(fs.claimed), p.calls)
		}
		untouched(t, fs)
	})
}

// TestAlertableSet pins the event types the store's transactional enqueue
// turns into deliveries: non-terminal and non-alert lifecycle events never
// enqueue. notification.failed does; the worker's loop guard (below) keeps it
// from feeding itself.
func TestAlertableSet(t *testing.T) {
	for typ, want := range map[string]bool{
		"run.failed":          true,
		"run.recovered":       true,
		"heartbeat.missed":    true,
		"heartbeat.recovered": true,
		"job.removed":         true,
		"heartbeat.removed":   true,
		"notification.failed": true,
		"run.succeeded":       false,
		"run.started":         false,
		"notification.other":  false,
	} {
		if got := Alertable(typ); got != want {
			t.Errorf("Alertable(%q) = %v, want %v", typ, got, want)
		}
	}
}

// TestWorkerNeverReportsAFailedFailureReport pins the loop guard: when the
// delivery being given up is itself a notification.failed, it is finalized as
// failed and no further notification.failed is recorded.
func TestWorkerNeverReportsAFailedFailureReport(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "notification.failed", "run.failed", old)}}
	w := newTestWorker(t, fs, &fakeProvider{failN: 1000})
	w.Clock = clock.NewFake(now)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(fs.failed) != 1 || fs.failed[0] != 1 {
		t.Fatalf("failed deliveries = %v, want [1]", fs.failed)
	}
	if len(fs.emitted) != 0 {
		t.Fatalf("emitted %+v, want nothing for a failed notification.failed", fs.emitted)
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

// rejectingProvider refuses every send for good, the way a receiver that
// answers 400 to this notification does.
type rejectingProvider struct{ calls int }

func (p *rejectingProvider) Send(context.Context, kitnotify.Message) error {
	p.calls++
	return kitnotify.Permanent(errors.New("webhook POST ward.example: status 400"))
}

// TestWorkerGivesUpARejectedDeliveryAtOnce: a delivery refused for good is
// finalized on its first attempt, long before MaxAge, with notification.failed
// recorded; it is not rescheduled.
func TestWorkerGivesUpARejectedDeliveryAtOnce(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "run.failed", `{"job_id":1}`, now)}}
	p := &rejectingProvider{}
	w := newTestWorker(t, fs, p)
	w.Clock = clock.NewFake(now)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("sends = %d, want 1", p.calls)
	}
	if len(fs.failed) != 1 || fs.failed[0] != 1 {
		t.Fatalf("failed deliveries = %v, want [1]", fs.failed)
	}
	if len(fs.emitted) != 1 || fs.emitted[0].Type != "notification.failed" || fs.emitted[0].Payload != "run.failed" {
		t.Fatalf("emitted = %+v, want one notification.failed for run.failed", fs.emitted)
	}
	if fs.emitted[0].Source != "notify" || fs.emitted[0].OrgID != 1 {
		t.Errorf("notification.failed = %+v, want source notify in org 1", fs.emitted[0])
	}
	if len(fs.rescheduled) != 0 {
		t.Errorf("a rejected delivery was rescheduled: %v", fs.rescheduled)
	}
}

// TestWorkerRejectedFailureReportIsNotReported keeps the loop guard for the
// immediate give-up too.
func TestWorkerRejectedFailureReportIsNotReported(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "notification.failed", "run.failed", now)}}
	w := newTestWorker(t, fs, &rejectingProvider{})
	w.Clock = clock.NewFake(now)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(fs.failed) != 1 || len(fs.emitted) != 0 {
		t.Fatalf("failed=%v emitted=%+v, want the delivery failed and nothing recorded", fs.failed, fs.emitted)
	}
}
