package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
)

// TestNtfyChannelDelivers sends a heartbeat.missed to an ntfy channel through
// Tend's worker and the real ntfy provider: the topic gets Tend's rendered
// subject as the title and its body as the message, with the channel's
// priority, tags and access token.
func TestNtfyChannelDelivers(t *testing.T) {
	type got struct {
		auth string
		body map[string]any
	}
	received := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		received <- got{auth: r.Header.Get("Authorization"), body: m}
	}))
	defer srv.Close()

	box := testBox(t)
	cfg, err := json.Marshal(map[string]any{"url": srv.URL + "/tend-alerts", "token": "tk_test", "priority": 4, "tags": []string{"warning"}})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := box.Encrypt(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{
		due: []Delivery{{ID: 1, OrgID: 1, EventID: 9, ChannelID: 7, State: DeliveryPending, CreatedAt: now,
			Event: core.Event{ID: 9, OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: "db-backup", CreatedAt: now}}},
		channel: Channel{ID: 7, OrgID: 1, Name: "phone", Kind: kitnotify.Ntfy},
		blob:    blob,
	}
	w := NewWorker(fs, box, kitnotify.BuildProvider, "primary", discardLogger())
	w.Clock = clock.NewFake(now)
	if n, err := w.DrainOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("DrainOnce = %d, %v; want the delivery sent", n, err)
	}
	g := <-received
	if g.auth != "Bearer tk_test" {
		t.Errorf("Authorization = %q, want the channel's token", g.auth)
	}
	want := map[string]any{"topic": "tend-alerts", "title": "Heartbeat missed: db-backup", "message": "db-backup", "priority": float64(4), "tags": []any{"warning"}}
	for k, v := range want {
		if gv, _ := json.Marshal(g.body[k]); string(gv) != mustJSON(t, v) {
			t.Errorf("%s = %s, want %s", k, gv, mustJSON(t, v))
		}
	}
	if len(fs.delivered) != 1 {
		t.Errorf("delivered = %v, want the delivery marked sent", fs.delivered)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
