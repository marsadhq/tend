package store_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// volatile matches what differs between two installations' webhook bodies for
// the same events: timestamps first, then every run of digits (row ids, the
// org, run and job ids, unix-nanosecond sequences and dedup suffixes).
var (
	volatileTime   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	volatileDigits = regexp.MustCompile(`\d+`)
)

func maskVolatile(b []byte) string {
	return volatileDigits.ReplaceAllString(volatileTime.ReplaceAllString(string(b), "<time>"), "N")
}

// TestWebhookBodiesFromRealEventsMatchTheFixtures produces every alertable
// event the way Tend does in production (a failing then succeeding job run by
// the runner, the watcher marking a heartbeat down, a ping bringing it back,
// deleting the heartbeat and the job, and a destination refusing a
// notification), delivers them through the worker and the real webhook
// provider, and checks each body against kit's fixture for its type. Ids and
// times differ from the fixtures' and are masked; everything else, the keys
// and their order, sources, payloads, dedup keys, subjects, bodies and the
// envelope's mapping, must be identical.
func TestWebhookBodiesFromRealEventsMatchTheFixtures(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		var mu sync.Mutex
		bodies := map[string][]byte{} // event type -> body posted to the ward channel
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/refuses" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(r.Body)
			wh, err := event.ParseTendWebhook(b)
			if err != nil {
				t.Errorf("receiver could not parse %s: %v", b, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, dup := bodies[wh.Event.Type]; dup {
				t.Errorf("second %s body: %s", wh.Event.Type, b)
			}
			bodies[wh.Event.Type] = b
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		box := testBox(t)
		mkChannel := func(name, url string, events ...string) {
			t.Helper()
			cfg, _ := json.Marshal(map[string]string{"url": url})
			id, err := notify.CreateChannel(ctx, s, box, notify.Channel{OrgID: org.ID, Name: name, Kind: kitnotify.Webhook}, cfg)
			if err != nil {
				t.Fatalf("CreateChannel %s: %v", name, err)
			}
			for _, ev := range events {
				if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: id, EventType: ev, Enabled: true}); err != nil {
					t.Fatalf("CreateRule: %v", err)
				}
			}
		}
		mkChannel("ward", srv.URL+"/ward", notify.AlertableTypes()...)
		mkChannel("refuses", srv.URL+"/refuses", "heartbeat.missed") // its give-up is the notification.failed

		w := notify.NewWorker(s, box, kitnotify.BuildProvider, "primary", notify.ChatOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.Hold = 0
		drain := func() {
			t.Helper()
			if _, err := w.DrainOnce(ctx); err != nil {
				t.Fatalf("DrainOnce: %v", err)
			}
		}

		// run.failed, then run.recovered: the job "backup" fails (exit 1)
		// until its flag file exists.
		flag := filepath.Join(t.TempDir(), "ok")
		jobID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "backup", Type: jobs.Shell, Command: "test -f " + flag, Enabled: true})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		runner := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now()))
		run := func() {
			t.Helper()
			if _, err := s.EnqueueRun(ctx, org.ID, jobID); err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}
			if err := runner.DrainOnce(ctx); err != nil {
				t.Fatalf("runner DrainOnce: %v", err)
			}
		}
		run()
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		run()

		// heartbeat.missed (and, through the refusing channel,
		// notification.failed), heartbeat.recovered, heartbeat.removed.
		token, err := heartbeat.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		hbID, token, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "db-backup", Token: token, PeriodSeconds: 60})
		if err != nil {
			t.Fatalf("CreateHeartbeat: %v", err)
		}
		later := clock.NewFake(time.Now().Add(time.Hour))
		if err := heartbeat.NewWatcher(s, later, nil, nil).Check(ctx); err != nil {
			t.Fatalf("watcher Check: %v", err)
		}
		drain() // delivers the miss, and records notification.failed for the refused one
		if _, _, recovered, err := s.RecordPing(ctx, token, later.Now()); err != nil || !recovered {
			t.Fatalf("RecordPing = recovered %v, %v; want a recovery", recovered, err)
		}
		if err := s.DeleteHeartbeat(ctx, org.ID, hbID); err != nil {
			t.Fatalf("DeleteHeartbeat: %v", err)
		}

		// job.removed.
		if err := s.DeleteJob(ctx, org.ID, jobID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		drain()

		mu.Lock()
		defer mu.Unlock()
		for _, typ := range event.TendEventTypes() {
			got, ok := bodies[typ]
			if !ok {
				t.Errorf("no %s was delivered", typ)
				continue
			}
			want, err := event.LoadTendFixture(typ, true)
			if err != nil {
				t.Fatalf("LoadTendFixture: %v", err)
			}
			if g, w := maskVolatile(got), maskVolatile(want); g != w {
				t.Errorf("%s body, ids and times masked, differs from %s:\n got %s\nwant %s", typ, event.TendFixtureName(typ, true), g, w)
			}
			wh, _ := event.ParseTendWebhook(got)
			if wh.Marsad == nil {
				t.Errorf("%s: no envelope (%v)", typ, wh.EnvelopeError())
			}
		}
	})
}
