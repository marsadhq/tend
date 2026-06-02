package notify_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
)

// fakeChannelStore is a minimal in-memory ChannelStore that captures the
// encrypted blob passed to CreateChannel so the test can assert it is not
// plaintext and decrypts back.
type fakeChannelStore struct {
	ch   notify.Channel
	blob string
}

func (f *fakeChannelStore) CreateChannel(_ context.Context, ch notify.Channel, blob string) (int64, error) {
	f.ch = ch
	f.ch.ID = 1
	f.blob = blob
	return 1, nil
}

func (f *fakeChannelStore) GetChannel(_ context.Context, _, _ int64) (notify.Channel, string, error) {
	return f.ch, f.blob, nil
}

func (f *fakeChannelStore) ListChannels(_ context.Context, _ int64) ([]notify.Channel, error) {
	return []notify.Channel{f.ch}, nil
}

func (f *fakeChannelStore) DeleteChannel(_ context.Context, _, _ int64) error { return nil }

func newBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

func TestCreateChannelEncryptsConfig(t *testing.T) {
	ctx := context.Background()
	box := newBox(t)
	fake := &fakeChannelStore{}

	cfg := []byte(`{"webhook_url":"https://hooks.example/abc"}`)
	id, err := notify.CreateChannel(ctx, fake, box,
		notify.Channel{OrgID: 7, Kind: notify.Slack, Name: "ops"}, cfg)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != 1 {
		t.Fatalf("id: got %d want 1", id)
	}
	if strings.Contains(fake.blob, "hooks.example") {
		t.Fatalf("captured blob is plaintext: %q", fake.blob)
	}
	if fake.blob == string(cfg) {
		t.Fatal("captured blob is unencrypted")
	}

	dec, err := box.Decrypt(fake.blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(dec) != string(cfg) {
		t.Fatalf("decrypt: got %q want %q", dec, cfg)
	}
}

func TestGetChannelDecrypted(t *testing.T) {
	ctx := context.Background()
	box := newBox(t)
	fake := &fakeChannelStore{}

	cfg := []byte(`{"k":"v"}`)
	if _, err := notify.CreateChannel(ctx, fake, box,
		notify.Channel{OrgID: 7, Kind: notify.Discord, Name: "ch"}, cfg); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	ch, got, err := notify.GetChannelDecrypted(ctx, fake, box, 7, 1)
	if err != nil {
		t.Fatalf("GetChannelDecrypted: %v", err)
	}
	if ch.Name != "ch" || ch.Kind != notify.Discord {
		t.Fatalf("channel: %+v", ch)
	}
	if string(got) != string(cfg) {
		t.Fatalf("config: got %q want %q", got, cfg)
	}
}
