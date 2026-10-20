package store_test

import (
	"context"
	"testing"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// deliveryStates counts a channel's deliveries by state.
func deliveryStates(t *testing.T, ctx context.Context, s store.Store, chID int64) map[string]int {
	t.Helper()
	rows, err := store.RawDB(s).QueryContext(ctx,
		`SELECT state, COUNT(*) FROM deliveries WHERE channel_id = `+ph(backendName(s), 1)+` GROUP BY state`, chID)
	if err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			t.Fatal(err)
		}
		out[st] = n
	}
	return out
}

// TestDeleteChannelTakesItsRulesAndDeliveries pins that deleting a channel
// deletes its rules and finalizes its pending deliveries in the same
// transaction, leaving other channels alone. Its rules used to survive, keep
// matching, and queue deliveries to a channel that was gone, each retried for
// 24 hours.
func TestDeleteChannelTakesItsRulesAndDeliveries(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		gone := seedAlertChannel(t, ctx, s, org.ID, "gone", "run.failed", "heartbeat.missed")
		kept := seedAlertChannel(t, ctx, s, org.ID, "kept", "run.failed")
		emit := func() {
			t.Helper()
			if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test", Payload: `{"job_id":1}`}); err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
		}
		emit()

		if err := s.DeleteChannel(ctx, org.ID, gone); err != nil {
			t.Fatalf("DeleteChannel: %v", err)
		}
		rules, err := s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListRules: %v", err)
		}
		for _, r := range rules {
			if r.ChannelID == gone {
				t.Errorf("rule %+v survived its channel", r)
			}
		}
		if len(rules) != 1 || rules[0].ChannelID != kept {
			t.Errorf("rules after delete = %+v, want only kept's run.failed rule", rules)
		}
		if got := deliveryStates(t, ctx, s, gone); got["pending"] != 0 || got["failed"] != 1 {
			t.Errorf("deleted channel's deliveries = %v, want its pending one failed", got)
		}

		emit()
		if got := deliveryStates(t, ctx, s, gone); got["pending"] != 0 {
			t.Errorf("an event after the delete queued %d deliveries to the deleted channel", got["pending"])
		}
		if got := deliveryStates(t, ctx, s, kept); got["pending"] != 2 {
			t.Errorf("kept channel's pending deliveries = %d, want 2", got["pending"])
		}
	})
}

// TestEnqueueSkipsRulesWithoutTheirChannel: a rule whose channel does not
// exist (left by an older version, or written by hand) queues nothing.
func TestEnqueueSkipsRulesWithoutTheirChannel(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: 9999, EventType: "run.failed", Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
		if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test"}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
		if n := countTable(t, s, "deliveries"); n != 0 {
			t.Fatalf("deliveries = %d, want 0 for a rule whose channel does not exist", n)
		}
	})
}

// TestMigrationClearsOrphanedRules runs migration 0006 over what an earlier
// version could leave behind: a rule and a pending delivery for a channel
// that was deleted. The rule goes and the delivery is finalized as failed;
// rules and deliveries of live channels are untouched.
func TestMigrationClearsOrphanedRules(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		live := seedAlertChannel(t, ctx, s, org.ID, "live", "run.failed")
		dead := seedAlertChannel(t, ctx, s, org.ID, "dead", "run.failed")
		if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test"}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
		db := store.RawDB(s)
		// The old DeleteChannel: only the channel row.
		if _, err := db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = `+ph(backendName(s), 1), dead); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 6`); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		rules, err := s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 1 || rules[0].ChannelID != live {
			t.Errorf("rules after the migration = %+v, want only the live channel's", rules)
		}
		if got := deliveryStates(t, ctx, s, dead); got["pending"] != 0 || got["failed"] != 1 {
			t.Errorf("dead channel's deliveries = %v, want its pending one failed", got)
		}
		if got := deliveryStates(t, ctx, s, live); got["pending"] != 1 {
			t.Errorf("live channel's deliveries = %v, want its pending one untouched", got)
		}
	})
}
