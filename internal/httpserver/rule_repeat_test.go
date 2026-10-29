package httpserver_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/notify"
)

// TestAPIRulesSayTheirRepeat: a run.failed rule in GET /api/rules carries the
// reminder interval in effect, in seconds (0: every failure), and whether it
// is its channel type's default; rules for other events carry neither. The
// fields are new; the others are as they were.
func TestAPIRulesSayTheirRepeat(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	ctx := context.Background()
	slack, err := ts.store.CreateChannel(ctx, notify.Channel{OrgID: ts.orgID, Name: "ops", Kind: kitnotify.Slack}, "blob")
	if err != nil {
		t.Fatal(err)
	}
	ward, err := ts.store.CreateChannel(ctx, notify.Channel{OrgID: ts.orgID, Name: "ward", Kind: kitnotify.Webhook}, "blob")
	if err != nil {
		t.Fatal(err)
	}
	half := 30 * time.Minute
	for _, r := range []notify.Rule{
		{ChannelID: slack, EventType: "run.failed"},
		{ChannelID: ward, EventType: "run.failed"},
		{ChannelID: slack, EventType: "run.failed", JobID: 9, Repeat: &half},
		{ChannelID: slack, EventType: "heartbeat.missed"},
	} {
		r.OrgID, r.Enabled = ts.orgID, true
		if _, err := ts.store.CreateRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()
	rec := apiGet(t, h, as, "/api/rules")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		ChannelID     int64  `json:"channel_id"`
		EventType     string `json:"event_type"`
		JobID         int64  `json:"job_id"`
		RepeatSeconds *int64 `json:"repeat_seconds"`
		RepeatDefault *bool  `json:"repeat_default"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 4 {
		t.Fatalf("got %d rules: %s", len(got), rec.Body.String())
	}
	for i, want := range []struct {
		seconds  int64
		isDflt   bool
		hasField bool
	}{{3600, true, true}, {0, true, true}, {1800, false, true}, {0, false, false}} {
		r := got[i]
		switch {
		case !want.hasField:
			if r.RepeatSeconds != nil || r.RepeatDefault != nil {
				t.Errorf("rule %d (%s) carries a repeat: %s", i, r.EventType, rec.Body.String())
			}
		case r.RepeatSeconds == nil || *r.RepeatSeconds != want.seconds:
			t.Errorf("rule %d repeat_seconds = %v, want %d", i, r.RepeatSeconds, want.seconds)
		case want.isDflt && (r.RepeatDefault == nil || !*r.RepeatDefault):
			t.Errorf("rule %d is not marked as the default: %s", i, rec.Body.String())
		case !want.isDflt && (r.RepeatDefault == nil || *r.RepeatDefault):
			t.Errorf("rule %d is not marked as set: %s", i, rec.Body.String())
		}
	}
}
