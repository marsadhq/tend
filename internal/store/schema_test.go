package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/store"
)

// ph returns the correct SQL placeholder for position i (1-based) given the
// backend name. SQLite uses ? for all positions; Postgres uses $1, $2, ...
func ph(backend string, i int) string {
	if backend == "postgres" {
		return fmt.Sprintf("$%d", i)
	}
	return "?"
}

// phs builds a comma-separated list of placeholders for n parameters.
func phs(backend string, start, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		if i > 0 {
			result += ", "
		}
		result += ph(backend, start+i)
	}
	return result
}

// TestSchema0002 asserts that migration 0002 adds the expected heartbeat columns
// and indexes, and the notification_rules.job_id column and its unique index.
func TestSchema0002(t *testing.T) {
	ctx := context.Background()

	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := store.RawDB(b.store)
			if db == nil {
				t.Fatal("RawDB returned nil")
			}

			// Bootstrap org to get a real org_id.
			org, err := b.store.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}
			orgID := org.ID
			// Use the store's canonical fixed-width timestamp layout so test rows
			// sort consistently with store-written rows.
			now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")

			// --- 1. New heartbeat columns round-trip ---
			insertHB := fmt.Sprintf(
				`INSERT INTO heartbeats (org_id, name, last_seen_at, created_at, token, period_seconds, grace_seconds, status) VALUES (%s)`,
				phs(b.name, 1, 8),
			)
			_, err = db.ExecContext(ctx, insertHB,
				orgID, "hb-roundtrip", now, now, "tok-abc", 60, 30, "up",
			)
			if err != nil {
				t.Fatalf("insert heartbeat with new columns: %v", err)
			}

			var gotToken string
			var gotPeriod, gotGrace int
			var gotStatus string
			selectHB := fmt.Sprintf(
				`SELECT token, period_seconds, grace_seconds, status FROM heartbeats WHERE org_id = %s AND name = %s`,
				ph(b.name, 1), ph(b.name, 2),
			)
			if err := db.QueryRowContext(ctx, selectHB, orgID, "hb-roundtrip").Scan(&gotToken, &gotPeriod, &gotGrace, &gotStatus); err != nil {
				t.Fatalf("select heartbeat new columns: %v", err)
			}
			if gotToken != "tok-abc" {
				t.Errorf("token = %q, want %q", gotToken, "tok-abc")
			}
			if gotPeriod != 60 {
				t.Errorf("period_seconds = %d, want 60", gotPeriod)
			}
			if gotGrace != 30 {
				t.Errorf("grace_seconds = %d, want 30", gotGrace)
			}
			if gotStatus != "up" {
				t.Errorf("status = %q, want %q", gotStatus, "up")
			}

			// --- 2. Heartbeat token unique index: duplicate token must fail ---
			// Insert a second heartbeat with a distinct name but the SAME token.
			_, err = db.ExecContext(ctx, insertHB,
				orgID, "hb-dup-token", now, now, "tok-abc", 60, 30, "up",
			)
			if err == nil {
				t.Fatal("expected unique-index violation on duplicate heartbeat token, got nil error")
			}

			// --- 2b. Multiple NULL tokens coexist (unique index treats NULLs as distinct) ---
			for _, name := range []string{"hb-null-1", "hb-null-2"} {
				if _, err = db.ExecContext(ctx, insertHB,
					orgID, name, now, now, nil, 0, 0, "new",
				); err != nil {
					t.Fatalf("insert heartbeat with NULL token (%s) should succeed: %v", name, err)
				}
			}

			// --- 3. notification_rules.job_id column exists ---
			insertRule := fmt.Sprintf(
				`INSERT INTO notification_rules (org_id, channel_id, event_type, enabled, created_at, job_id) VALUES (%s)`,
				phs(b.name, 1, 6),
			)
			_, err = db.ExecContext(ctx, insertRule,
				orgID, 1, "run.failed", 1, now, 0,
			)
			if err != nil {
				t.Fatalf("insert notification_rule with job_id: %v", err)
			}

			// --- 4a. Duplicate (org_id, channel_id, event_type, job_id) must fail ---
			_, err = db.ExecContext(ctx, insertRule,
				orgID, 1, "run.failed", 1, now, 0,
			)
			if err == nil {
				t.Fatal("expected unique-index violation on duplicate notification_rule, got nil error")
			}

			// --- 4b. Same (org_id, channel_id, event_type) but different job_id must NOT collide ---
			_, err = db.ExecContext(ctx, insertRule,
				orgID, 1, "run.failed", 1, now, 5,
			)
			if err != nil {
				t.Fatalf("insert notification_rule with different job_id (5) should succeed: %v", err)
			}
		})
	}
}
