package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// backdate rewrites a row's created_at so retention tests can age rows without
// sleeping. The timestamp is interpolated as a literal (it comes from
// FormatTime, never user input) so the statement is portable across both
// backends' placeholder styles.
func backdate(t *testing.T, s store.Store, table string, id int64, at time.Time) {
	t.Helper()
	q := fmt.Sprintf(`UPDATE %s SET created_at = '%s' WHERE id = %d`, table, store.FormatTime(at), id)
	if _, err := store.RawDB(s).Exec(q); err != nil {
		t.Fatalf("backdate %s %d: %v", table, id, err)
	}
}

// countTable returns the total rows in a table.
func countTable(t *testing.T, s store.Store, table string) int {
	t.Helper()
	var n int
	if err := store.RawDB(s).QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestPruneEvents proves the 30-day event sweep: rows older than the cutoff go,
// newer rows stay, and an old event still referenced by a PENDING delivery is
// kept (the notify worker joins the event on claim).
func TestPruneEvents(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		emit := func(evType string) int64 {
			id, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: evType, Source: "test"})
			if err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
			return id
		}

		now := time.Now()
		cutoff := now.Add(-30 * 24 * time.Hour)

		oldPlain := emit("run.started") // old, unreferenced -> pruned
		newPlain := emit("run.started") // new -> kept
		backdate(t, s, "events", oldPlain, cutoff.Add(-time.Hour))

		// Old event with a PENDING delivery -> kept by the guard.
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: 10, EventType: "run.failed", Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
		oldPending := emit("run.failed")
		backdate(t, s, "events", oldPending, cutoff.Add(-time.Hour))

		n, err := s.PruneEvents(ctx, cutoff)
		if err != nil {
			t.Fatalf("PruneEvents: %v", err)
		}
		if n != 1 {
			t.Fatalf("PruneEvents deleted %d rows, want 1", n)
		}
		evs, err := s.ListEvents(ctx, org.ID, 100)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(evs) != 2 {
			t.Fatalf("events after prune = %d, want 2 (new + pending-guarded)", len(evs))
		}
		for _, e := range evs {
			if e.ID == oldPlain {
				t.Fatalf("old unreferenced event %d survived the prune", oldPlain)
			}
		}
		if evs[0].ID != newPlain && evs[1].ID != newPlain {
			t.Fatalf("new event %d missing after prune", newPlain)
		}
	})
}

// TestPruneJobRuns proves the 30-day run sweep: old terminal runs go, recent
// ones stay, and pending/running rows are never pruned regardless of age.
func TestPruneJobRuns(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)

		mkRun := func(finish bool, status jobs.RunStatus) int64 {
			runID, err := s.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}
			if finish {
				if _, ok, err := s.ClaimRun(ctx, "test-worker"); err != nil || !ok {
					t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
				}
				if err := s.FinishRun(ctx, runID, status, 0, "out"); err != nil {
					t.Fatalf("FinishRun: %v", err)
				}
			}
			return runID
		}

		now := time.Now()
		cutoff := now.Add(-30 * 24 * time.Hour)

		oldDone := mkRun(true, jobs.StatusSucceeded) // old terminal -> pruned
		oldFail := mkRun(true, jobs.StatusFailed)    // old terminal -> pruned
		newDone := mkRun(true, jobs.StatusSucceeded) // recent terminal -> kept
		oldPend := mkRun(false, jobs.StatusPending)  // old but pending -> kept
		for _, id := range []int64{oldDone, oldFail, oldPend} {
			backdate(t, s, "job_runs", id, cutoff.Add(-time.Hour))
		}

		n, err := s.PruneJobRuns(ctx, cutoff)
		if err != nil {
			t.Fatalf("PruneJobRuns: %v", err)
		}
		if n != 2 {
			t.Fatalf("PruneJobRuns deleted %d rows, want 2", n)
		}
		if got := countTable(t, s, "job_runs"); got != 2 {
			t.Fatalf("job_runs after prune = %d, want 2", got)
		}
		if _, err := s.GetRun(ctx, orgID, newDone); err != nil {
			t.Fatalf("recent terminal run pruned: %v", err)
		}
		if _, err := s.GetRun(ctx, orgID, oldPend); err != nil {
			t.Fatalf("old pending run pruned: %v", err)
		}
	})
}

// TestPruneDeliveries proves the 7-day delivery sweep: old delivered/failed
// rows go, recent finalized rows stay, and pending rows are never pruned.
func TestPruneDeliveries(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: 10, EventType: "run.failed", Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}

		// Each alertable emit enqueues exactly one pending delivery for channel 10.
		emitDelivery := func() int64 {
			evID, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test"})
			if err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
			var id int64
			if err := store.RawDB(s).QueryRow(
				fmt.Sprintf(`SELECT id FROM deliveries WHERE event_id = %d`, evID)).Scan(&id); err != nil {
				t.Fatalf("find delivery for event %d: %v", evID, err)
			}
			return id
		}

		oldDelivered := emitDelivery()
		oldFailed := emitDelivery()
		newDelivered := emitDelivery()
		oldPending := emitDelivery()
		if err := s.MarkDeliveryDelivered(ctx, oldDelivered); err != nil {
			t.Fatalf("MarkDeliveryDelivered: %v", err)
		}
		if err := s.FailDelivery(ctx, oldFailed); err != nil {
			t.Fatalf("FailDelivery: %v", err)
		}
		if err := s.MarkDeliveryDelivered(ctx, newDelivered); err != nil {
			t.Fatalf("MarkDeliveryDelivered: %v", err)
		}

		now := time.Now()
		cutoff := now.Add(-7 * 24 * time.Hour)
		for _, id := range []int64{oldDelivered, oldFailed, oldPending} {
			backdate(t, s, "deliveries", id, cutoff.Add(-time.Hour))
		}

		n, err := s.PruneDeliveries(ctx, cutoff)
		if err != nil {
			t.Fatalf("PruneDeliveries: %v", err)
		}
		if n != 2 {
			t.Fatalf("PruneDeliveries deleted %d rows, want 2 (old delivered + old failed)", n)
		}
		if got := countTable(t, s, "deliveries"); got != 2 {
			t.Fatalf("deliveries after prune = %d, want 2 (recent delivered + old pending)", got)
		}
		var state string
		if err := store.RawDB(s).QueryRow(
			fmt.Sprintf(`SELECT state FROM deliveries WHERE id = %d`, oldPending)).Scan(&state); err != nil {
			t.Fatalf("old pending delivery pruned: %v", err)
		}
		if state != notify.DeliveryPending {
			t.Fatalf("old pending delivery state = %q, want pending", state)
		}
	})
}
