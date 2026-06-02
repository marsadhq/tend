package store_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// newTestBox returns a secrets.Box keyed with an all-zero 32-byte master key.
// Deterministic and fine for tests: we only assert that the at-rest blob is not
// the plaintext and that it decrypts back to the original.
func newTestBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// TestChannelRoundTripEncryptsConfig exercises the channel CRUD + encryption
// helpers against every backend: create with a config blob, read it back
// decrypted, and confirm the raw config column is ciphertext (no plaintext URL).
func TestChannelRoundTripEncryptsConfig(t *testing.T) {
	ctx := context.Background()
	box := newTestBox(t)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}

			cfg := []byte(`{"webhook_url":"https://hooks.example/abc"}`)
			id, err := notify.CreateChannel(ctx, s, box,
				notify.Channel{OrgID: org.ID, Kind: notify.Slack, Name: "ops"}, cfg)
			if err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			if id == 0 {
				t.Fatal("CreateChannel returned id 0")
			}

			// Decrypted round-trip.
			ch, got, err := notify.GetChannelDecrypted(ctx, s, box, org.ID, id)
			if err != nil {
				t.Fatalf("GetChannelDecrypted: %v", err)
			}
			if ch.Name != "ops" {
				t.Fatalf("name: got %q want %q", ch.Name, "ops")
			}
			if ch.Kind != notify.Slack {
				t.Fatalf("kind: got %q want %q", ch.Kind, notify.Slack)
			}
			if ch.ID != id {
				t.Fatalf("id: got %d want %d", ch.ID, id)
			}
			if ch.OrgID != org.ID {
				t.Fatalf("org: got %d want %d", ch.OrgID, org.ID)
			}
			if string(got) != string(cfg) {
				t.Fatalf("config: got %q want %q", got, cfg)
			}
			if ch.CreatedAt.IsZero() {
				t.Fatal("created_at not set")
			}

			// At rest the config column must be ciphertext, not plaintext.
			db := store.RawDB(s)
			if db == nil {
				t.Fatal("RawDB returned nil")
			}
			var raw string
			if err := db.QueryRowContext(ctx,
				`SELECT config FROM notification_channels WHERE id = `+ph(b.name, 1), id,
			).Scan(&raw); err != nil {
				t.Fatalf("read raw config: %v", err)
			}
			if strings.Contains(raw, "hooks.example") {
				t.Fatalf("config stored in plaintext: %q", raw)
			}
			if raw == string(cfg) {
				t.Fatal("config stored unencrypted")
			}

			// ListChannels returns the channel metadata.
			list, err := s.ListChannels(ctx, org.ID)
			if err != nil {
				t.Fatalf("ListChannels: %v", err)
			}
			if len(list) != 1 || list[0].ID != id || list[0].Name != "ops" || list[0].Kind != notify.Slack {
				t.Fatalf("ListChannels: %+v", list)
			}

			// GetChannel with a wrong org -> ErrNotFound.
			if _, _, err := s.GetChannel(ctx, org.ID+999, id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetChannel wrong org: got %v want ErrNotFound", err)
			}
			// GetChannel with an unknown id -> ErrNotFound.
			if _, _, err := s.GetChannel(ctx, org.ID, id+999); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetChannel unknown id: got %v want ErrNotFound", err)
			}

			// Upsert by (org, name): same name, new kind/config, same id.
			cfg2 := []byte(`{"webhook_url":"https://hooks.example/xyz","other":true}`)
			id2, err := notify.CreateChannel(ctx, s, box,
				notify.Channel{OrgID: org.ID, Kind: notify.Webhook, Name: "ops"}, cfg2)
			if err != nil {
				t.Fatalf("CreateChannel upsert: %v", err)
			}
			if id2 != id {
				t.Fatalf("upsert created new row: got id %d want %d", id2, id)
			}
			ch2, got2, err := notify.GetChannelDecrypted(ctx, s, box, org.ID, id)
			if err != nil {
				t.Fatalf("GetChannelDecrypted after upsert: %v", err)
			}
			if ch2.Kind != notify.Webhook {
				t.Fatalf("upsert kind: got %q want %q", ch2.Kind, notify.Webhook)
			}
			if string(got2) != string(cfg2) {
				t.Fatalf("upsert config: got %q want %q", got2, cfg2)
			}
			// created_at must be preserved across the upsert (ON CONFLICT does not
			// touch it); a regression here would silently reset channel age.
			if !ch2.CreatedAt.Equal(ch.CreatedAt) {
				t.Fatalf("upsert clobbered created_at: got %v want %v", ch2.CreatedAt, ch.CreatedAt)
			}
			// Still exactly one row for this (org, name).
			list2, err := s.ListChannels(ctx, org.ID)
			if err != nil {
				t.Fatalf("ListChannels after upsert: %v", err)
			}
			if len(list2) != 1 {
				t.Fatalf("ListChannels after upsert: got %d rows want 1", len(list2))
			}

			// Delete then Get -> ErrNotFound.
			if err := s.DeleteChannel(ctx, org.ID, id); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			if _, _, err := s.GetChannel(ctx, org.ID, id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetChannel after delete: got %v want ErrNotFound", err)
			}
			list3, err := s.ListChannels(ctx, org.ID)
			if err != nil {
				t.Fatalf("ListChannels after delete: %v", err)
			}
			if len(list3) != 0 {
				t.Fatalf("ListChannels after delete: got %d rows want 0", len(list3))
			}
		})
	}
}
