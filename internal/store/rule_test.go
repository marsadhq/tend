package store_test

import (
	"context"
	"testing"

	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestRuleCRUD exercises CreateRule (upsert), ListRules, and DeleteRule against
// every backend.
func TestRuleCRUD(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		// Create a rule. channel_id values are arbitrary ints (no FK).
		id, err := s.CreateRule(ctx, notify.Rule{
			OrgID: org.ID, ChannelID: 10, EventType: "run.failed", Enabled: true, JobID: 0,
		})
		if err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
		if id == 0 {
			t.Fatal("CreateRule returned id 0")
		}

		// Upsert idempotency: re-create the SAME (org, channel, event, job) with a
		// different enabled flag -> same id, enabled updated, still one row.
		id2, err := s.CreateRule(ctx, notify.Rule{
			OrgID: org.ID, ChannelID: 10, EventType: "run.failed", Enabled: false, JobID: 0,
		})
		if err != nil {
			t.Fatalf("CreateRule upsert: %v", err)
		}
		if id2 != id {
			t.Fatalf("upsert created a new row: got id %d want %d", id2, id)
		}

		list, err := s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListRules: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("after upsert: got %d rules want 1", len(list))
		}
		if list[0].ID != id {
			t.Fatalf("list id: got %d want %d", list[0].ID, id)
		}
		if list[0].Enabled {
			t.Fatalf("upsert did not update enabled: got enabled=true want false")
		}
		if list[0].EventType != "run.failed" || list[0].ChannelID != 10 || list[0].JobID != 0 {
			t.Fatalf("list rule fields: %+v", list[0])
		}
		if list[0].CreatedAt.IsZero() {
			t.Fatal("created_at not set")
		}

		// A rule differing ONLY in job_id is a distinct row.
		idScoped, err := s.CreateRule(ctx, notify.Rule{
			OrgID: org.ID, ChannelID: 10, EventType: "run.failed", Enabled: true, JobID: 5,
		})
		if err != nil {
			t.Fatalf("CreateRule scoped: %v", err)
		}
		if idScoped == id {
			t.Fatalf("rule differing in job_id reused id %d; should be distinct", id)
		}
		list, err = s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListRules after scoped: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("got %d rules want 2", len(list))
		}

		// Org scoping: another org sees none of these rules.
		other, err := s.ListRules(ctx, org.ID+999)
		if err != nil {
			t.Fatalf("ListRules other org: %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("other org leaked %d rules", len(other))
		}

		// DeleteRule removes one rule (org-scoped); the other remains.
		if err := s.DeleteRule(ctx, org.ID, id); err != nil {
			t.Fatalf("DeleteRule: %v", err)
		}
		list, err = s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListRules after delete: %v", err)
		}
		if len(list) != 1 || list[0].ID != idScoped {
			t.Fatalf("after delete: got %+v want only idScoped %d", list, idScoped)
		}
	})
}

// TestMatchingRules verifies the job-scoping and enabled filter: org-wide rules
// (job_id 0) plus rules scoped to the event's job, never other event types or
// disabled rules.
func TestMatchingRules(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		// Seed:
		//   chanA: run.failed,        job_id 0  (all jobs)   enabled
		//   chanB: run.failed,        job_id 5  (job 5)      enabled
		//   chanC: heartbeat.missed,  job_id 0               enabled
		//   chanD: run.failed,        job_id 0               DISABLED
		mustCreate := func(channelID int64, eventType string, enabled bool, jobID int64) {
			if _, err := s.CreateRule(ctx, notify.Rule{
				OrgID: org.ID, ChannelID: channelID, EventType: eventType, Enabled: enabled, JobID: jobID,
			}); err != nil {
				t.Fatalf("CreateRule(chan=%d): %v", channelID, err)
			}
		}
		mustCreate(1, "run.failed", true, 0)       // chanA
		mustCreate(2, "run.failed", true, 5)       // chanB
		mustCreate(3, "heartbeat.missed", true, 0) // chanC
		mustCreate(4, "run.failed", false, 0)      // chanD (disabled)

		channelSet := func(rules []notify.Rule) map[int64]bool {
			out := map[int64]bool{}
			for _, r := range rules {
				out[r.ChannelID] = true
			}
			return out
		}

		// run.failed for job 5 -> chanA (job 0) + chanB (job 5); NOT chanC (other
		// event), NOT chanD (disabled).
		got, err := s.MatchingRules(ctx, org.ID, "run.failed", 5)
		if err != nil {
			t.Fatalf("MatchingRules(run.failed, 5): %v", err)
		}
		set := channelSet(got)
		if len(set) != 2 || !set[1] || !set[2] {
			t.Fatalf("MatchingRules(run.failed, 5): got channels %v want {1,2}", set)
		}

		// run.failed for job 3 (no scoped rule) -> only chanA (job 0).
		got, err = s.MatchingRules(ctx, org.ID, "run.failed", 3)
		if err != nil {
			t.Fatalf("MatchingRules(run.failed, 3): %v", err)
		}
		set = channelSet(got)
		if len(set) != 1 || !set[1] {
			t.Fatalf("MatchingRules(run.failed, 3): got channels %v want {1}", set)
		}

		// heartbeat.missed -> only chanC.
		got, err = s.MatchingRules(ctx, org.ID, "heartbeat.missed", 0)
		if err != nil {
			t.Fatalf("MatchingRules(heartbeat.missed, 0): %v", err)
		}
		set = channelSet(got)
		if len(set) != 1 || !set[3] {
			t.Fatalf("MatchingRules(heartbeat.missed, 0): got channels %v want {3}", set)
		}

		// Org scoping: a different org matches nothing.
		got, err = s.MatchingRules(ctx, org.ID+999, "run.failed", 5)
		if err != nil {
			t.Fatalf("MatchingRules other org: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("MatchingRules other org leaked %d rules", len(got))
		}
	})
}
