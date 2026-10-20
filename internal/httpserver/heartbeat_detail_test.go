package httpserver_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
)

// seedPings records a start and a failure with a body for the heartbeat that
// owns token, and returns the heartbeat.
func seedPings(t *testing.T, ts *testStore, name, token string) heartbeat.Heartbeat {
	t.Helper()
	ctx := context.Background()
	ts.seedHB(t, name, token, "up")
	t0 := time.Now().UTC().Add(-time.Hour)
	three := 3
	for _, p := range []struct {
		in heartbeat.PingInput
		at time.Time
	}{
		{heartbeat.PingInput{Kind: heartbeat.PingStart, RID: "728b3763-ea80-4113-9fc0-f49b3adf226a"}, t0},
		{heartbeat.PingInput{Kind: heartbeat.PingFail, ExitStatus: &three, RID: "728b3763-ea80-4113-9fc0-f49b3adf226a",
			Body: "<script>alert(1)</script>\nconnection refused\n"}, t0.Add(90 * time.Second)},
	} {
		if _, err := ts.store.RecordHeartbeatPing(ctx, token, p.in, p.at); err != nil {
			t.Fatalf("RecordHeartbeatPing: %v", err)
		}
	}
	hb, err := ts.store.GetHeartbeatByName(ctx, ts.orgID, name)
	if err != nil {
		t.Fatalf("GetHeartbeatByName: %v", err)
	}
	return hb
}

// TestDashboardHeartbeatDetailShowsPings: the heartbeat page lists its pings
// with kind, exit status, duration and the logged body, escaped, and never the
// ping token; the list links to it.
func TestDashboardHeartbeatDetailShowsPings(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const secret = "SECRET-PING-TOKEN-detail-page-0123456789"
	hb := seedPings(t, ts, "db-backup", secret)
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	list := dashGet(t, h, cookie, "/heartbeats")
	if !strings.Contains(list.Body.String(), `href="/heartbeats/`+itoa(hb.ID)+`"`) {
		t.Fatalf("heartbeats list does not link to the heartbeat page; body=%q", list.Body.String())
	}

	rec := dashGet(t, h, cookie, "/heartbeats/"+itoa(hb.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"db-backup", "down", "fail", "start", "exit 3", "1m30s", "728b3763",
		"&lt;script&gt;alert(1)&lt;/script&gt;", "connection refused", "heartbeat.missed"} {
		if !strings.Contains(body, want) {
			t.Errorf("heartbeat page missing %q; body=%q", want, body)
		}
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("a logged body was rendered as markup")
	}
	if strings.Contains(body, secret) {
		t.Fatal("the ping token leaked into the heartbeat page")
	}
}

// TestDashboardHeartbeatDetailOrgScoped: another org's heartbeat, and an id
// that does not exist, are 404.
func TestDashboardHeartbeatDetailOrgScoped(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	orgB := newOrg(t, ts, "org-b")
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO heartbeats (org_id, name, token, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		orgB, "theirs", "tok-theirs-detail", "up", created)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := res.LastInsertId()
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)
	for _, path := range []string{"/heartbeats/" + itoa(theirs), "/heartbeats/9999", "/heartbeats/x"} {
		if rec := dashGet(t, h, cookie, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

// TestAPIHeartbeatPings: GET /api/heartbeats/{id}/pings returns the ping log,
// newest first, with what each ping reported and its body, but no token; the
// heartbeat and event DTOs carry the run being timed and the event detail.
func TestAPIHeartbeatPings(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	hb := seedPings(t, ts, "db-backup", "api-ping-secret")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/heartbeats/"+itoa(hb.ID)+"/pings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api-ping-secret") {
		t.Fatal("ping log leaked the token")
	}
	var got []struct {
		Kind       string `json:"kind"`
		ExitStatus *int   `json:"exit_status"`
		RID        string `json:"rid"`
		DurationMS *int64 `json:"duration_ms"`
		Body       string `json:"body"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 2 || got[0].Kind != "fail" || got[1].Kind != "start" {
		t.Fatalf("pings = %+v, want [fail, start]", got)
	}
	if got[0].ExitStatus == nil || *got[0].ExitStatus != 3 || got[0].DurationMS == nil || *got[0].DurationMS != 90000 ||
		!strings.Contains(got[0].Body, "connection refused") || got[0].RID == "" {
		t.Fatalf("failure ping = %+v", got[0])
	}
	if got[1].ExitStatus != nil || got[1].DurationMS != nil || got[1].Body != "" {
		t.Fatalf("start ping = %+v", got[1])
	}
	if r := apiGet(t, h, as, "/api/heartbeats/9999/pings"); r.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", r.Code)
	}

	hist := apiGet(t, h, as, "/api/heartbeats/"+itoa(hb.ID)+"/history")
	var evs []struct {
		Type   string `json:"type"`
		Detail struct {
			Reason     string `json:"reason"`
			ExitStatus int    `json:"exit_status"`
		} `json:"detail"`
	}
	decodeJSON(t, hist, &evs)
	if len(evs) != 1 || evs[0].Type != "heartbeat.missed" || evs[0].Detail.Reason != "failed" || evs[0].Detail.ExitStatus != 3 {
		t.Fatalf("history = %+v, want the heartbeat.missed with its detail", evs)
	}
}
