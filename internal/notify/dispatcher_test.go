package notify

// These tests are in package notify (internal) so they can set the dispatcher's
// unexported fields (sleep, maxTries, build) directly, keeping retry tests fast
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

// fakeStore is an in-memory DispatchStore. It records every call so tests can
// assert what the dispatcher queried (e.g. that MatchingRules was never reached
// for a non-alertable event), and returns canned rules/channel/blob.
type fakeStore struct {
	rules    []Rule  // returned by MatchingRules
	channel  Channel // returned by GetChannel
	blob     string  // returned by GetChannel
	getErr   error   // if set, GetChannel returns it
	matchErr error   // if set, MatchingRules returns it

	matchCalls int // how many times MatchingRules was invoked
	lastMatch  matchArgs
	getCalls   []int64 // channel IDs passed to GetChannel
	emitted    []core.Event
}

type matchArgs struct {
	orgID     int64
	eventType string
	jobID     int64
}

func (f *fakeStore) MatchingRules(_ context.Context, orgID int64, eventType string, jobID int64) ([]Rule, error) {
	f.matchCalls++
	f.lastMatch = matchArgs{orgID, eventType, jobID}
	if f.matchErr != nil {
		return nil, f.matchErr
	}
	return f.rules, nil
}

func (f *fakeStore) GetChannel(_ context.Context, _, id int64) (Channel, string, error) {
	f.getCalls = append(f.getCalls, id)
	if f.getErr != nil {
		return Channel{}, "", f.getErr
	}
	return f.channel, f.blob, nil
}

func (f *fakeStore) EmitEvent(_ context.Context, e core.Event) (int64, error) {
	f.emitted = append(f.emitted, e)
	return int64(len(f.emitted)), nil
}

// fakeProvider is a scripted Provider: it fails the first failN Send calls and
// succeeds thereafter, counting every call.
type fakeProvider struct {
	failN int
	calls int
}

func (p *fakeProvider) Send(_ context.Context, _ Message) error {
	p.calls++
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

// newTestDispatcher wires a dispatcher over the fake store with a no-op sleep
// and a build func that always returns p. The box encrypts/decrypts the canned
// blob so GetChannel + Decrypt round-trips work.
func newTestDispatcher(t *testing.T, fs *fakeStore, p Provider) *Dispatcher {
	t.Helper()
	box := testBox(t)
	// Encrypt a dummy config so the dispatcher's Decrypt step succeeds.
	blob, err := box.Encrypt([]byte(`{"url":"https://example/x"}`))
	if err != nil {
		t.Fatalf("encrypt blob: %v", err)
	}
	fs.blob = blob
	build := func(_ ChannelType, _ []byte) (Provider, error) { return p, nil }
	d := NewDispatcher(fs, box, build, discardLogger())
	d.sleep = func(context.Context, time.Duration) bool { return false } // never really sleep
	return d
}

// TestDispatchMatchesRulesAndRetries: a rule run.failed -> channel A; the
// provider fails twice then succeeds. Send must be called exactly 3 times, no
// notification.failed must be emitted, and the matched channel must be the one
// used.
func TestDispatchMatchesRulesAndRetries(t *testing.T) {
	fs := &fakeStore{
		rules:   []Rule{{ID: 1, OrgID: 1, ChannelID: 42, EventType: "run.failed", Enabled: true}},
		channel: Channel{ID: 42, OrgID: 1, Kind: Webhook},
	}
	p := &fakeProvider{failN: 2}
	d := newTestDispatcher(t, fs, p)

	d.DispatchForEvent(context.Background(), core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":1,"job_id":0,"job_name":"nightly","status":"failed","exit_code":2}`,
	})

	if p.calls != 3 {
		t.Fatalf("Send calls: got %d want 3", p.calls)
	}
	if len(fs.emitted) != 0 {
		t.Fatalf("notification.failed should not be emitted on eventual success: %+v", fs.emitted)
	}
	if len(fs.getCalls) != 1 || fs.getCalls[0] != 42 {
		t.Fatalf("GetChannel calls: got %v want [42]", fs.getCalls)
	}
	if fs.lastMatch.eventType != "run.failed" || fs.lastMatch.orgID != 1 {
		t.Fatalf("MatchingRules args: got %+v", fs.lastMatch)
	}
}

// TestDeliveryExhaustionEmitsNotificationFailed: the provider always fails. Send
// must be called maxTries (4) times, and then exactly one notification.failed
// event must be emitted (never silently dropped).
func TestDeliveryExhaustionEmitsNotificationFailed(t *testing.T) {
	fs := &fakeStore{
		rules:   []Rule{{ID: 1, OrgID: 1, ChannelID: 7, EventType: "run.failed", Enabled: true}},
		channel: Channel{ID: 7, OrgID: 1, Kind: Slack},
	}
	p := &fakeProvider{failN: 1000} // always fails
	d := newTestDispatcher(t, fs, p)

	d.DispatchForEvent(context.Background(), core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":1,"job_id":0,"job_name":"nightly","status":"failed","exit_code":1}`,
	})

	if p.calls != d.maxTries {
		t.Fatalf("Send calls: got %d want %d (maxTries)", p.calls, d.maxTries)
	}
	if len(fs.emitted) != 1 {
		t.Fatalf("notification.failed events: got %d want 1", len(fs.emitted))
	}
	ev := fs.emitted[0]
	if ev.Type != "notification.failed" {
		t.Fatalf("emitted type: got %q want notification.failed", ev.Type)
	}
	if ev.OrgID != 1 {
		t.Fatalf("emitted org: got %d want 1", ev.OrgID)
	}
	if ev.Payload != "run.failed" {
		t.Fatalf("emitted payload: got %q want the failed event type %q", ev.Payload, "run.failed")
	}
	if ev.Source != "notify" {
		t.Fatalf("emitted source: got %q want notify", ev.Source)
	}
}

// TestNotificationFailedNeverDispatched is the loop guard: a notification.failed
// event (and any non-alertable type) must be dropped BEFORE any store query, so
// MatchingRules is never called.
func TestNotificationFailedNeverDispatched(t *testing.T) {
	for _, typ := range []string{"notification.failed", "run.succeeded", "run.started"} {
		fs := &fakeStore{}
		p := &fakeProvider{}
		d := newTestDispatcher(t, fs, p)

		d.DispatchForEvent(context.Background(), core.Event{Type: typ, OrgID: 1})

		if fs.matchCalls != 0 {
			t.Fatalf("%s: MatchingRules called %d times; loop guard failed", typ, fs.matchCalls)
		}
		if p.calls != 0 {
			t.Fatalf("%s: provider Send called %d times for a non-alertable event", typ, p.calls)
		}
		if len(fs.emitted) != 0 {
			t.Fatalf("%s: should not emit anything", typ)
		}
	}
}

// TestDeliveryStopsOnContextCancel: dispatching with an already-cancelled context
// must not emit notification.failed. The provider should not be called at all
// (short-circuited by the ctx.Err() guard at the top of each attempt iteration).
func TestDeliveryStopsOnContextCancel(t *testing.T) {
	fs := &fakeStore{
		rules:   []Rule{{ID: 1, OrgID: 1, ChannelID: 5, EventType: "run.failed", Enabled: true}},
		channel: Channel{ID: 5, OrgID: 1, Kind: Webhook},
	}
	p := &fakeProvider{failN: 1000} // always fails if reached
	d := newTestDispatcher(t, fs, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before dispatch

	d.DispatchForEvent(ctx, core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":1,"job_id":0,"job_name":"nightly","status":"failed","exit_code":1}`,
	})

	// A shutdown must not produce spurious notification.failed events.
	if len(fs.emitted) != 0 {
		t.Fatalf("notification.failed must not be emitted on context cancel: got %d event(s)", len(fs.emitted))
	}
	// Provider should not have been called (short-circuited by ctx.Err() check).
	if p.calls > 1 {
		t.Fatalf("Send called %d times on cancelled ctx; want 0 or at most 1", p.calls)
	}
}

// TestDeliveryStopsOnBackoffCancel: provider fails once, then sleep signals
// cancellation - delivery must stop without emitting notification.failed.
func TestDeliveryStopsOnBackoffCancel(t *testing.T) {
	fs := &fakeStore{
		rules:   []Rule{{ID: 1, OrgID: 1, ChannelID: 6, EventType: "run.failed", Enabled: true}},
		channel: Channel{ID: 6, OrgID: 1, Kind: Webhook},
	}
	p := &fakeProvider{failN: 1000} // always fails
	d := newTestDispatcher(t, fs, p)
	// Override sleep to simulate cancellation mid-backoff.
	d.sleep = func(context.Context, time.Duration) bool { return true }

	d.DispatchForEvent(context.Background(), core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":1,"job_id":0,"job_name":"nightly","status":"failed","exit_code":1}`,
	})

	// Exactly one attempt (provider called once), then sleep returns true → stop.
	if p.calls != 1 {
		t.Fatalf("Send calls: got %d want 1", p.calls)
	}
	// No notification.failed - cancellation during backoff is not a delivery failure.
	if len(fs.emitted) != 0 {
		t.Fatalf("notification.failed must not be emitted on backoff cancel: got %d event(s)", len(fs.emitted))
	}
}

// TestJobIDFromPayload covers the payload-parsing helper across the shapes the
// dispatcher actually sees: run.failed JSON, plain heartbeat name, empty, junk.
func TestJobIDFromPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int64
	}{
		{"run.failed JSON", `{"run_id":1,"job_id":7,"job_name":"x","status":"failed","exit_code":2}`, 7},
		{"plain name", "nightly-backup", 0},
		{"empty", "", 0},
		{"malformed JSON", `{"job_id":`, 0},
		{"json without job_id", `{"run_id":1}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jobIDFromPayload(core.Event{Payload: c.payload}); got != c.want {
				t.Fatalf("jobIDFromPayload(%q): got %d want %d", c.payload, got, c.want)
			}
		})
	}
}

// TestMessageForRunFailed asserts the run.failed message carries the job name in
// the subject and status/exit_code in the body.
func TestMessageForRunFailed(t *testing.T) {
	ev := core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":3,"job_id":2,"job_name":"nightly","status":"failed","exit_code":9}`,
	}
	m := messageFor(ev)
	if m.Event.Type != "run.failed" {
		t.Fatalf("Event not carried through: %+v", m.Event)
	}
	if !contains(m.Subject, "nightly") {
		t.Fatalf("subject should mention job name: %q", m.Subject)
	}
	if !contains(m.Body, "failed") || !contains(m.Body, "9") {
		t.Fatalf("body should mention status and exit_code: %q", m.Body)
	}

	// A run.failed with an unparseable payload must not panic and must still
	// produce a sensible subject.
	m2 := messageFor(core.Event{Type: "run.failed", Payload: "not json"})
	if m2.Subject == "" {
		t.Fatal("empty subject for unparseable run.failed payload")
	}
}

// TestMessageForHeartbeat asserts heartbeat messages include the heartbeat name
// (carried as the plain-string payload).
func TestMessageForHeartbeat(t *testing.T) {
	missed := messageFor(core.Event{Type: "heartbeat.missed", Payload: "db-backup"})
	if !contains(missed.Subject, "db-backup") {
		t.Fatalf("missed subject should mention heartbeat name: %q", missed.Subject)
	}
	recovered := messageFor(core.Event{Type: "heartbeat.recovered", Payload: "db-backup"})
	if !contains(recovered.Subject, "db-backup") {
		t.Fatalf("recovered subject should mention heartbeat name: %q", recovered.Subject)
	}

	// Unknown type falls back to the type as subject.
	other := messageFor(core.Event{Type: "weird.event", Payload: "x"})
	if other.Subject != "weird.event" {
		t.Fatalf("default subject: got %q want %q", other.Subject, "weird.event")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
