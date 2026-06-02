package store_test

import (
	"context"
	"testing"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// TestCreateHeartbeatUpsert verifies that CreateHeartbeat inserts a new
// heartbeat with the provided token (returning a non-zero id + that token), and
// that re-creating by the same (org, name) preserves the original token while
// updating period/grace. The effective token is always returned so the CLI can
// print a stable ping URL across re-syncs.
func TestCreateHeartbeatUpsert(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		// --- insert ---
		id, tok, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
			OrgID:         org.ID,
			Name:          "external-backup",
			Token:         "tok-original",
			PeriodSeconds: 86400,
			GraceSeconds:  3600,
		})
		if err != nil {
			t.Fatalf("CreateHeartbeat (insert): %v", err)
		}
		if id == 0 {
			t.Fatal("CreateHeartbeat (insert): id is 0")
		}
		if tok != "tok-original" {
			t.Errorf("CreateHeartbeat (insert): token = %q, want tok-original", tok)
		}

		// --- upsert with a DIFFERENT token: token must be preserved, period/grace updated ---
		id2, tok2, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
			OrgID:         org.ID,
			Name:          "external-backup",
			Token:         "tok-second", // must be ignored on conflict
			PeriodSeconds: 120,
			GraceSeconds:  30,
		})
		if err != nil {
			t.Fatalf("CreateHeartbeat (upsert): %v", err)
		}
		if id2 != id {
			t.Errorf("CreateHeartbeat (upsert): id = %d, want %d (same row)", id2, id)
		}
		if tok2 != "tok-original" {
			t.Errorf("CreateHeartbeat (upsert): token = %q, want tok-original (preserved)", tok2)
		}

		// Verify the row reflects the updated period/grace and preserved token+status.
		hbs, err := s.ListHeartbeats(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListHeartbeats: %v", err)
		}
		if len(hbs) != 1 {
			t.Fatalf("ListHeartbeats returned %d, want 1", len(hbs))
		}
		got := hbs[0]
		if got.Name != "external-backup" {
			t.Errorf("Name = %q, want external-backup", got.Name)
		}
		if got.Token != "tok-original" {
			t.Errorf("Token = %q, want tok-original", got.Token)
		}
		if got.PeriodSeconds != 120 {
			t.Errorf("PeriodSeconds = %d, want 120 (updated)", got.PeriodSeconds)
		}
		if got.GraceSeconds != 30 {
			t.Errorf("GraceSeconds = %d, want 30 (updated)", got.GraceSeconds)
		}
		if got.Status != "new" {
			t.Errorf("Status = %q, want new (preserved on conflict)", got.Status)
		}

		// --- status preservation: upsert must NOT reset a non-default status ---
		// Transition the heartbeat to 'up' so the next upsert can prove it is
		// preserved rather than re-defaulted to 'new'.
		if err := s.SetHeartbeatStatus(ctx, id, "up"); err != nil {
			t.Fatalf("SetHeartbeatStatus -> up: %v", err)
		}
		// Re-create with the same name but new period/grace; token must stay the
		// same and status must remain 'up' (not be reset to 'new').
		id3, tok3, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
			OrgID:         org.ID,
			Name:          "external-backup",
			Token:         "tok-third", // ignored on conflict
			PeriodSeconds: 300,
			GraceSeconds:  60,
		})
		if err != nil {
			t.Fatalf("CreateHeartbeat (upsert after up): %v", err)
		}
		if id3 != id {
			t.Errorf("CreateHeartbeat (upsert after up): id = %d, want %d (same row)", id3, id)
		}
		if tok3 != "tok-original" {
			t.Errorf("CreateHeartbeat (upsert after up): token = %q, want tok-original (preserved)", tok3)
		}
		hbs2, err := s.ListHeartbeats(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListHeartbeats (after up upsert): %v", err)
		}
		if len(hbs2) != 1 {
			t.Fatalf("ListHeartbeats (after up upsert) = %d, want 1", len(hbs2))
		}
		got2 := hbs2[0]
		if got2.Status != "up" {
			t.Errorf("Status = %q, want up (must be preserved, not reset to 'new')", got2.Status)
		}
		if got2.Token != "tok-original" {
			t.Errorf("Token = %q, want tok-original after up upsert", got2.Token)
		}
		if got2.PeriodSeconds != 300 {
			t.Errorf("PeriodSeconds = %d, want 300 (updated)", got2.PeriodSeconds)
		}
		if got2.GraceSeconds != 60 {
			t.Errorf("GraceSeconds = %d, want 60 (updated)", got2.GraceSeconds)
		}
	})
}

// TestListHeartbeats verifies org-scoping and ordering.
func TestListHeartbeats(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		empty, err := s.ListHeartbeats(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListHeartbeats (empty): %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("ListHeartbeats (empty) = %d, want 0", len(empty))
		}

		for _, n := range []string{"hb-1", "hb-2"} {
			if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
				OrgID: org.ID, Name: n, Token: "tok-" + n, PeriodSeconds: 60, GraceSeconds: 10,
			}); err != nil {
				t.Fatalf("CreateHeartbeat %q: %v", n, err)
			}
		}

		hbs, err := s.ListHeartbeats(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListHeartbeats: %v", err)
		}
		if len(hbs) != 2 {
			t.Fatalf("ListHeartbeats = %d, want 2", len(hbs))
		}
		if hbs[0].Name != "hb-1" || hbs[1].Name != "hb-2" {
			t.Errorf("order = [%q, %q], want [hb-1, hb-2]", hbs[0].Name, hbs[1].Name)
		}
	})
}
