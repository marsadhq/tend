package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/store"
)

// seedHeartbeat inserts a heartbeat row directly via the raw DB handle so the
// RecordPing tests do not depend on a heartbeat package (Task 7). It returns the
// org ID the heartbeat is attached to.
func seedHeartbeat(t *testing.T, ctx context.Context, s store.Store, backend, name, token, status string) int64 {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	db := store.RawDB(s)
	if db == nil {
		t.Fatal("RawDB returned nil")
	}
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	_, err = db.ExecContext(ctx,
		`INSERT INTO heartbeats (org_id, name, token, status, created_at) VALUES (`+
			ph(backend, 1)+`, `+ph(backend, 2)+`, `+ph(backend, 3)+`, `+ph(backend, 4)+`, `+ph(backend, 5)+`)`,
		org.ID, name, token, status, created)
	if err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
	return org.ID
}

// readHeartbeat returns the status and last_seen_at for a token via the raw DB.
func readHeartbeat(t *testing.T, ctx context.Context, s store.Store, backend, token string) (status string, lastSeen string) {
	t.Helper()
	db := store.RawDB(s)
	var ls *string
	if err := db.QueryRowContext(ctx,
		`SELECT status, last_seen_at FROM heartbeats WHERE token = `+ph(backend, 1), token,
	).Scan(&status, &ls); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if ls != nil {
		lastSeen = *ls
	}
	return status, lastSeen
}

func TestRecordPing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Run("new_to_up", func(t *testing.T) {
				s := b.store
				orgID := seedHeartbeat(t, ctx, s, b.name, "hb-new", "tok-new", "new")

				gotOrg, name, recovered, err := s.RecordPing(ctx, "tok-new", now)
				if err != nil {
					t.Fatalf("RecordPing: %v", err)
				}
				if recovered {
					t.Fatal("recovered=true for a new heartbeat; want false")
				}
				if gotOrg != orgID {
					t.Fatalf("orgID: got %d want %d", gotOrg, orgID)
				}
				if name != "hb-new" {
					t.Fatalf("name: got %q want %q", name, "hb-new")
				}
				status, lastSeen := readHeartbeat(t, ctx, s, b.name, "tok-new")
				if status != "up" {
					t.Fatalf("status: got %q want up", status)
				}
				if lastSeen == "" {
					t.Fatal("last_seen_at not set")
				}
			})

			t.Run("up_to_up", func(t *testing.T) {
				s := b.store
				seedHeartbeat(t, ctx, s, b.name, "hb-up", "tok-up", "up")

				_, _, recovered, err := s.RecordPing(ctx, "tok-up", now)
				if err != nil {
					t.Fatalf("RecordPing: %v", err)
				}
				if recovered {
					t.Fatal("recovered=true for an up heartbeat; want false")
				}
				status, _ := readHeartbeat(t, ctx, s, b.name, "tok-up")
				if status != "up" {
					t.Fatalf("status: got %q want up", status)
				}
			})

			t.Run("down_to_up_recovers", func(t *testing.T) {
				s := b.store
				seedHeartbeat(t, ctx, s, b.name, "hb-down", "tok-down", "down")

				_, name, recovered, err := s.RecordPing(ctx, "tok-down", now)
				if err != nil {
					t.Fatalf("RecordPing: %v", err)
				}
				if !recovered {
					t.Fatal("recovered=false for a down heartbeat; want true")
				}
				if name != "hb-down" {
					t.Fatalf("name: got %q want %q", name, "hb-down")
				}
				status, _ := readHeartbeat(t, ctx, s, b.name, "tok-down")
				if status != "up" {
					t.Fatalf("status: got %q want up", status)
				}
			})

			t.Run("unknown_token", func(t *testing.T) {
				s := b.store
				_, _, _, err := s.RecordPing(ctx, "no-such-token", now)
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("RecordPing unknown token: got %v want ErrNotFound", err)
				}
			})
		})
	}
}
