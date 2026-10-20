package httpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/store"
)

// TestPingRecoverySurvivesClientDisconnect sends a recovery ping whose client
// has already gone away. The ping must still be recorded in full: the
// heartbeat comes back up and its heartbeat.recovered is stored. The write
// used to run under the request's context, so a client that timed out could
// make the ping fail, or leave the heartbeat up with its recovery unrecorded.
func TestPingRecoverySurvivesClientDisconnect(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		id, token, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
			OrgID: org.ID, Name: "nightly", Token: "tok-gone", PeriodSeconds: 60,
		})
		if err != nil {
			t.Fatalf("CreateHeartbeat: %v", err)
		}
		if err := s.SetHeartbeatStatus(ctx, id, "down"); err != nil {
			t.Fatalf("SetHeartbeatStatus: %v", err)
		}

		var dispatched int
		srv := httpserver.New(s, clock.RealClock{}, func(context.Context, core.Event) { dispatched++ },
			slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

		gone, cancel := context.WithCancel(ctx)
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/ping/"+token, nil).WithContext(gone)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		hb, err := s.GetHeartbeat(ctx, org.ID, id)
		if err != nil {
			t.Fatalf("GetHeartbeat: %v", err)
		}
		if hb.Status != "up" {
			t.Fatalf("heartbeat status = %q, want up", hb.Status)
		}
		evs, err := s.ListEvents(ctx, org.ID, 50)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		var recovered int
		for _, e := range evs {
			if e.Type == "heartbeat.recovered" && e.Payload == "nightly" {
				recovered++
			}
		}
		if recovered != 1 {
			t.Fatalf("heartbeat.recovered events = %d, want 1", recovered)
		}
		if dispatched != 1 {
			t.Fatalf("dispatch calls = %d, want 1", dispatched)
		}
	})
}
