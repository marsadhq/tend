package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
)

// webhookCapture is a webhook receiver that records every body it is sent.
type webhookCapture struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newWebhookCapture(t *testing.T) *webhookCapture {
	t.Helper()
	c := &webhookCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// postedWebhookBody delivers ev to a webhook channel through Tend's worker
// and the real webhook provider, and returns the body the receiver got.
func postedWebhookBody(t *testing.T, ev core.Event) []byte {
	t.Helper()
	capture := newWebhookCapture(t)
	box := testBox(t)
	cfg, err := json.Marshal(map[string]string{"url": capture.srv.URL + "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := box.Encrypt(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeWorkerStore{
		due:     []Delivery{{ID: 1, OrgID: ev.OrgID, EventID: ev.ID, ChannelID: 7, State: DeliveryPending, CreatedAt: time.Now(), Event: ev}},
		channel: Channel{ID: 7, OrgID: ev.OrgID, Name: "ward", Kind: kitnotify.Webhook},
		blob:    blob,
	}
	w := NewWorker(fs, box, kitnotify.BuildProvider, "primary", discardLogger())
	w.Clock = clock.NewFake(time.Now())
	if n, err := w.DrainOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("DrainOnce = %d, %v; want the delivery sent", n, err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.bodies) != 1 {
		t.Fatalf("receiver got %d bodies, want 1", len(capture.bodies))
	}
	return capture.bodies[0]
}

// fixtureEvent is the event a kit fixture body carries, as Tend stores it.
func fixtureEvent(t *testing.T, body []byte) core.Event {
	t.Helper()
	w, err := event.ParseTendWebhook(body)
	if err != nil {
		t.Fatalf("ParseTendWebhook: %v", err)
	}
	e := w.Event
	return core.Event{ID: e.ID, OrgID: e.OrgID, Type: e.Type, Source: e.Source, Payload: e.Payload, DedupKey: e.DedupKey, CreatedAt: e.CreatedAt}
}

// TestWebhookBodyMatchesTheFixtures: for every event type a rule can route,
// the body Tend posts to a webhook is byte for byte kit's fixture with the
// marsad envelope, and without the envelope it is byte for byte the legacy
// fixture, the body every receiver of Tend's webhooks has been parsing. The
// fixtures are for an installation named "primary".
func TestWebhookBodyMatchesTheFixtures(t *testing.T) {
	for _, typ := range event.TendEventTypes() {
		t.Run(typ, func(t *testing.T) {
			legacy, err := event.LoadTendFixture(typ, false)
			if err != nil {
				t.Fatalf("LoadTendFixture: %v", err)
			}
			want, err := event.LoadTendFixture(typ, true)
			if err != nil {
				t.Fatalf("LoadTendFixture: %v", err)
			}
			ev := fixtureEvent(t, legacy)
			if got := postedWebhookBody(t, ev); !bytes.Equal(got, want) {
				t.Errorf("posted body differs from %s:\n got %s\nwant %s", event.TendFixtureName(typ, true), got, want)
			}
			w := messageFor(ev, "primary").Webhook.(event.TendWebhook)
			w.Marsad = nil
			if got, err := json.Marshal(w); err != nil || !bytes.Equal(got, legacy) {
				t.Errorf("body without the envelope differs from %s (%v):\n got %s\nwant %s", event.TendFixtureName(typ, false), err, got, legacy)
			}
		})
	}
	if got, want := event.TendEventTypes(), AlertableTypes(); !equalStrings(got, want) {
		t.Errorf("kit has fixtures for %v, Tend alerts on %v", got, want)
	}
}

// TestWebhookEnvelopeIsWhatWardReads: a receiver decoding the body with kit
// gets a valid envelope that agrees with the legacy event, with this
// installation's name, and the message carries the same envelope for the
// other providers.
func TestWebhookEnvelopeIsWhatWardReads(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 123456789, time.FixedZone("UTC+3", 3*60*60))
	for _, instance := range []string{"eu-1", ""} {
		ev := core.Event{ID: 42, OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: "db-backup", CreatedAt: at}
		m := messageFor(ev, instance)
		body, err := json.Marshal(m.Webhook)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		w, err := event.ParseTendWebhook(body)
		if err != nil || w.Marsad == nil {
			t.Fatalf("ParseTendWebhook = %+v, %v (envelope error %v)", w, err, w.EnvelopeError())
		}
		env := *w.Marsad
		if env.Source != (event.Source{Tool: "tend", Instance: instance}) {
			t.Errorf("source = %+v, want tend/%q", env.Source, instance)
		}
		if want := fmt.Sprintf("heartbeat.missed:db-backup@%d", at.UnixNano()); env.DedupKey != want {
			t.Errorf("dedup_key = %q, want %q", env.DedupKey, want)
		}
		if env.Sequence != at.UnixNano() || !env.OccurredAt.Equal(at) || env.ID != "42" {
			t.Errorf("sequence %d, occurred_at %v, id %q; want %d, %v, 42", env.Sequence, env.OccurredAt, env.ID, at.UnixNano(), at)
		}
		if m.Event == nil || m.Event.DedupKey != env.DedupKey {
			t.Errorf("Message.Event = %+v, want the envelope", m.Event)
		}
	}
}

// TestWebhookEnvelopeLeftOutWhenItCannotBeBuilt: an event that cannot make a
// valid envelope is still delivered, with the legacy body alone, exactly as
// before the envelope existed.
func TestWebhookEnvelopeLeftOutWhenItCannotBeBuilt(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	for name, ev := range map[string]core.Event{
		"run payload without job_id": {ID: 1, OrgID: 1, Type: "run.failed", Source: "jobs.runner", Payload: `{"job_name":"x"}`, CreatedAt: at},
		"run payload not JSON":       {ID: 2, OrgID: 1, Type: "run.recovered", Source: "jobs.runner", Payload: "junk", CreatedAt: at},
		"job payload not an object":  {ID: 3, OrgID: 1, Type: "job.removed", Source: "jobs", Payload: `[7]`, CreatedAt: at},
		"unnamed heartbeat":          {ID: 4, OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: "", CreatedAt: at},
		"no CreatedAt":               {ID: 5, OrgID: 1, Type: "heartbeat.recovered", Source: "heartbeat", Payload: "db-backup"},
		"not alertable":              {ID: 6, OrgID: 1, Type: "run.succeeded", Source: "jobs.runner", Payload: `{"job_id":7}`, CreatedAt: at},
	} {
		t.Run(name, func(t *testing.T) {
			m := messageFor(ev, "primary")
			w := m.Webhook.(event.TendWebhook)
			if w.Marsad != nil || m.Event != nil {
				t.Fatalf("envelope = %+v, want none", w.Marsad)
			}
			got := postedWebhookBody(t, ev)
			want, err := json.Marshal(map[string]any{"subject": m.Subject, "body": m.Body, "event": ev})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("posted body:\n got %s\nwant the legacy body %s", got, want)
			}
		})
	}
}

// TestWebhookBodyIsTheLegacyEncoding: the webhook body is exactly what Tend's
// earlier encoding, a map around its event struct, produced, also for values
// the fixtures do not exercise (characters JSON escapes, invalid UTF-8, the
// zero time, a time zone, odd payloads).
func TestWebhookBodyIsTheLegacyEncoding(t *testing.T) {
	loc := time.FixedZone("UTC+3", 3*60*60)
	events := []core.Event{
		{},
		{ID: 1, OrgID: 1, Type: "run.failed", Source: "jobs.runner", Payload: `{"job_name":"<b>&\"quoted\"</b>","status":"timeout","exit_code":-1}`, DedupKey: "run:1:run.failed", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, loc)},
		{ID: -5, OrgID: 99, Type: "heartbeat.missed", Source: "heartbeat", Payload: "naïve \u2028 \x00 \xff name", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{ID: 1 << 60, OrgID: 2, Type: "job.removed", Source: "jobs", Payload: "not json", DedupKey: "job:7:removed"},
		{ID: 3, OrgID: 1, Type: "notification.failed", Source: "notify", Payload: ""},
		{ID: 4, OrgID: 1, Type: "weird.type", Source: "x", Payload: "<script>"},
	}
	for _, ev := range events {
		m := messageFor(ev, "primary")
		w, ok := m.Webhook.(event.TendWebhook)
		if !ok {
			t.Fatalf("messageFor(%s).Webhook is %T, want event.TendWebhook", ev.Type, m.Webhook)
		}
		w.Marsad = nil // the envelope is an addition; the rest is the legacy body
		got, err := json.Marshal(w)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want, err := json.Marshal(map[string]any{"subject": m.Subject, "body": m.Body, "event": ev})
		if err != nil {
			t.Fatalf("marshal legacy: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q:\n got %s\nwant %s", ev.Type, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
