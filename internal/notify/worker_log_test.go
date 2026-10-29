package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
)

// logStore is a WorkerStore whose writes fail on demand.
type logStore struct {
	due                                  []Delivery
	blob                                 string
	getErr, markErr, reschedErr, failErr error
}

func (f *logStore) ReleaseHeldAlerts(context.Context, time.Time) (int, error) { return 0, nil }

func (f *logStore) ClaimDueDeliveries(_ context.Context, _ time.Time, _, _ time.Duration, _ int) ([]Delivery, error) {
	ds := f.due
	f.due = nil
	for i := range ds {
		ds[i].Attempts++
	}
	return ds, nil
}
func (f *logStore) MarkDeliveryDelivered(context.Context, int64) error         { return f.markErr }
func (f *logStore) RescheduleDelivery(context.Context, int64, time.Time) error { return f.reschedErr }
func (f *logStore) FailDelivery(context.Context, int64) error                  { return f.failErr }
func (f *logStore) FailDeliveryAndEmit(context.Context, int64, core.Event) error {
	return f.failErr
}
func (f *logStore) GetChannel(context.Context, int64, int64) (Channel, string, error) {
	return Channel{ID: 7, OrgID: 1, Kind: kitnotify.Webhook}, f.blob, f.getErr
}

// errProvider fails every send with err (nil: succeeds).
type errProvider struct{ err error }

func (p errProvider) Send(context.Context, kitnotify.Message) error { return p.err }

// TestWorkerLogLinesUnchanged pins the worker's log lines, which operators
// alert and search on, for every path a delivery can take. The expected lines
// are what Tend v0.3.0's own worker logged for the same deliveries.
func TestWorkerLogLinesUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	box := testBox(t)
	goodBlob, err := box.Encrypt([]byte(`{"url":"https://example/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id int64, typ string, created time.Time) Delivery {
		return Delivery{ID: id, OrgID: 1, EventID: id, ChannelID: 7, State: DeliveryPending, CreatedAt: created,
			Event: core.Event{ID: id, OrgID: 1, Type: typ, Payload: "x"}}
	}
	boom := errors.New("boom")
	dbGone := errors.New("db gone")
	cases := []struct {
		name      string
		st        *logStore
		sendErr   error
		buildErr  error
		delivered int
		want      string
	}{
		{"retry", &logStore{due: []Delivery{mk(1, "run.failed", now)}}, boom, nil, 0,
			`level=WARN msg="notify: delivery attempt failed" delivery=1 channel=7 event=run.failed attempt=1 err=boom` + "\n"},
		{"given up after MaxAge", &logStore{due: []Delivery{mk(2, "heartbeat.missed", old)}}, boom, nil, 0,
			`level=ERROR msg="notify: delivery failed permanently" reason="retried for too long" delivery=2 channel=7 event=heartbeat.missed attempts=1 err=boom` + "\n"},
		{"rejected", &logStore{due: []Delivery{mk(3, "run.failed", now)}}, kitnotify.Permanent(errors.New("webhook POST ward.example: status 400")), nil, 0,
			`level=ERROR msg="notify: delivery failed permanently" reason="rejected by the destination" delivery=3 channel=7 event=run.failed attempts=1 err="webhook POST ward.example: status 400"` + "\n"},
		{"loop guard", &logStore{due: []Delivery{mk(4, "notification.failed", old)}}, boom, nil, 0,
			`level=ERROR msg="notify: delivery failed permanently" reason="retried for too long" delivery=4 channel=7 event=notification.failed attempts=1 err=boom` + "\n"},
		{"channel not loaded", &logStore{due: []Delivery{mk(5, "run.failed", now)}, getErr: errors.New("store: not found")}, nil, nil, 0,
			`level=WARN msg="notify: delivery attempt failed" delivery=5 channel=7 event=run.failed attempt=1 err="load channel 7: store: not found"` + "\n"},
		{"config not decrypted", &logStore{due: []Delivery{mk(6, "run.failed", now)}, blob: "not-a-blob"}, nil, nil, 0,
			`level=WARN msg="notify: delivery attempt failed" delivery=6 channel=7 event=run.failed attempt=1 err="decrypt channel 7 config: illegal base64 data at input byte 3"` + "\n"},
		{"provider not built", &logStore{due: []Delivery{mk(7, "run.failed", now)}}, nil, errors.New(`notify: unknown channel kind "x"`), 0,
			`level=WARN msg="notify: delivery attempt failed" delivery=7 channel=7 event=run.failed attempt=1 err="build provider for channel 7: notify: unknown channel kind \"x\""` + "\n"},
		{"mark delivered fails", &logStore{due: []Delivery{mk(8, "run.failed", now)}, markErr: dbGone}, nil, nil, 1,
			`level=ERROR msg="notify: mark delivered" delivery=8 err="db gone"` + "\n"},
		{"reschedule fails", &logStore{due: []Delivery{mk(9, "run.failed", now)}, reschedErr: dbGone}, boom, nil, 0,
			`level=WARN msg="notify: delivery attempt failed" delivery=9 channel=7 event=run.failed attempt=1 err=boom` + "\n" +
				`level=ERROR msg="notify: reschedule delivery" delivery=9 err="db gone"` + "\n"},
		{"give-up fails", &logStore{due: []Delivery{mk(10, "run.failed", old)}, failErr: dbGone}, boom, nil, 0,
			`level=ERROR msg="notify: delivery failed permanently" reason="retried for too long" delivery=10 channel=7 event=run.failed attempts=1 err=boom` + "\n" +
				`level=ERROR msg="notify: fail delivery" delivery=10 err="db gone"` + "\n"},
		{"loop-guard give-up fails", &logStore{due: []Delivery{mk(11, "notification.failed", old)}, failErr: dbGone}, boom, nil, 0,
			`level=ERROR msg="notify: delivery failed permanently" reason="retried for too long" delivery=11 channel=7 event=notification.failed attempts=1 err=boom` + "\n" +
				`level=ERROR msg="notify: fail delivery" delivery=11 err="db gone"` + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.st.blob == "" {
				c.st.blob = goodBlob
			}
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			}}))
			w := NewWorker(c.st, box, func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error) {
				if c.buildErr != nil {
					return nil, c.buildErr
				}
				return errProvider{c.sendErr}, nil
			}, "primary", ChatOptions{}, log)
			w.Clock = clock.NewFake(now)
			n, err := w.DrainOnce(context.Background())
			if err != nil || n != c.delivered {
				t.Fatalf("DrainOnce = %d, %v; want %d, nil", n, err, c.delivered)
			}
			if got := buf.String(); got != c.want {
				t.Errorf("log:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestNewWorkerDefaults pins the delivery policy serve runs with.
func TestNewWorkerDefaults(t *testing.T) {
	w := NewWorker(&logStore{}, testBox(t), kitnotify.BuildProvider, "primary", ChatOptions{}, discardLogger())
	for name, c := range map[string]struct{ got, want time.Duration }{
		"Interval":   {w.Interval, 2 * time.Second},
		"Lease":      {w.Lease, time.Minute},
		"MaxAge":     {w.MaxAge, 24 * time.Hour},
		"BackoffCap": {w.BackoffCap, 5 * time.Minute},
		"Hold":       {w.Hold, 15 * time.Minute},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if fmt.Sprintf("%T", w.Clock) != "clock.RealClock" {
		t.Errorf("Clock = %T, want the system clock", w.Clock)
	}
}

// TestWorkerBackoffSequence pins the retry schedule: 1s after the first
// attempt, doubling, capped at 5 minutes.
func TestWorkerBackoffSequence(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, wantDelay := range want {
		d := due(1, "run.failed", "", now)
		d.Attempts = i // the claim makes it i+1
		fs := &fakeWorkerStore{due: []Delivery{d}}
		w := newTestWorker(t, fs, &fakeProvider{failN: 1000})
		w.Clock = clock.NewFake(now)
		if _, err := w.DrainOnce(context.Background()); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := fs.rescheduled[1].Sub(now); got != wantDelay {
			t.Errorf("after attempt %d: next attempt in %v, want %v", i+1, got, wantDelay)
		}
	}
}
