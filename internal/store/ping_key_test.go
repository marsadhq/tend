package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// TestOrgPingKey: an org's ping key (for slug ping URLs) is set, read back,
// resolved to its org, unique across orgs, and cleared.
func TestOrgPingKey(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		if key, err := s.GetOrgPingKey(ctx, org.ID); err != nil || key != "" {
			t.Fatalf("GetOrgPingKey before any = %q, %v; want none", key, err)
		}
		if _, err := s.OrgByPingKey(ctx, "fqOOd6-F4MMNuCEnzTU01w"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("OrgByPingKey of an unknown key: %v, want ErrNotFound", err)
		}

		if err := s.SetOrgPingKey(ctx, org.ID, "fqOOd6-F4MMNuCEnzTU01w"); err != nil {
			t.Fatalf("SetOrgPingKey: %v", err)
		}
		if key, err := s.GetOrgPingKey(ctx, org.ID); err != nil || key != "fqOOd6-F4MMNuCEnzTU01w" {
			t.Fatalf("GetOrgPingKey = %q, %v", key, err)
		}
		if id, err := s.OrgByPingKey(ctx, "fqOOd6-F4MMNuCEnzTU01w"); err != nil || id != org.ID {
			t.Fatalf("OrgByPingKey = %d, %v; want org %d", id, err, org.ID)
		}

		other := insertOrg(t, ctx, s, "other")
		if err := s.SetOrgPingKey(ctx, other, "fqOOd6-F4MMNuCEnzTU01w"); err == nil {
			t.Fatal("two orgs were given the same ping key")
		}

		if err := s.SetOrgPingKey(ctx, org.ID, ""); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if _, err := s.OrgByPingKey(ctx, "fqOOd6-F4MMNuCEnzTU01w"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("OrgByPingKey after clearing: %v, want ErrNotFound", err)
		}
		if _, err := s.OrgByPingKey(ctx, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("OrgByPingKey of the empty key: %v, want ErrNotFound", err)
		}
	})
}

// insertOrg adds an org row and returns its id.
func insertOrg(t *testing.T, ctx context.Context, s store.Store, name string) int64 {
	t.Helper()
	db := store.RawDB(s)
	b := backendName(s)
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (name, created_at) VALUES (`+phs(b, 1, 2)+`)`, name, "2026-10-20T00:00:00.000000000Z"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var id int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM orgs WHERE name = `+ph(b, 1), name).Scan(&id); err != nil {
		t.Fatalf("read org id: %v", err)
	}
	return id
}

// TestCreateHeartbeatIfAbsent: the slug ping's ?create=1 path creates a
// heartbeat only when its name is free; an existing one keeps its token,
// period and grace.
func TestCreateHeartbeatIfAbsent(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		tok, created, err := s.CreateHeartbeatIfAbsent(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "made", Token: "tok-made", PeriodSeconds: 86400, GraceSeconds: 3600})
		if err != nil || !created || tok != "tok-made" {
			t.Fatalf("first create = %q, %v, %v; want tok-made, created", tok, created, err)
		}
		if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "kept", Token: "tok-kept", PeriodSeconds: 60, GraceSeconds: 5}); err != nil {
			t.Fatal(err)
		}
		tok, created, err = s.CreateHeartbeatIfAbsent(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "kept", Token: "tok-other", PeriodSeconds: 86400, GraceSeconds: 3600})
		if err != nil || created || tok != "tok-kept" {
			t.Fatalf("create over an existing one = %q, %v, %v; want its token, not created", tok, created, err)
		}
		hb, err := s.GetHeartbeatByName(ctx, org.ID, "kept")
		if err != nil || hb.PeriodSeconds != 60 || hb.GraceSeconds != 5 {
			t.Fatalf("existing heartbeat after = %+v, %v; want period 60 and grace 5 kept", hb, err)
		}
	})
}
