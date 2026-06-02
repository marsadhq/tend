package notify

import (
	"context"
	"time"

	"github.com/marsadhq/tend/internal/secrets"
)

// Channel is a configured notification destination. Tenant-scoped via OrgID and
// uniquely named within an org. The (encrypted) provider configuration is NOT a
// field here: plaintext config moves through the helpers below as []byte, and
// the ciphertext travels through the store API as a separate string blob.
type Channel struct {
	ID        int64
	OrgID     int64
	Name      string
	Kind      ChannelType
	CreatedAt time.Time
}

// ChannelStore is the consumer-defined persistence contract the helpers below
// depend on. The concrete store backends satisfy it structurally, so notify
// never imports store (no import cycle). The config blob is opaque ciphertext.
type ChannelStore interface {
	// CreateChannel upserts a channel by (org, name) and returns its row id.
	CreateChannel(ctx context.Context, ch Channel, configBlob string) (int64, error)
	// GetChannel returns the channel plus its encrypted config blob, or
	// ErrNotFound (with an empty blob) when absent.
	GetChannel(ctx context.Context, orgID, id int64) (Channel, string, error)
	// ListChannels returns channel metadata for an org (no config blob).
	ListChannels(ctx context.Context, orgID int64) ([]Channel, error)
	// DeleteChannel removes a channel scoped to its org.
	DeleteChannel(ctx context.Context, orgID, id int64) error
}

// CreateChannel encrypts cfg with box and upserts the channel via s, returning
// the row id. Plaintext config never reaches the store. box must be non-nil
// (a channel always carries credentials that require encryption).
func CreateChannel(ctx context.Context, s ChannelStore, box *secrets.Box, ch Channel, cfg []byte) (int64, error) {
	blob, err := box.Encrypt(cfg)
	if err != nil {
		return 0, err
	}
	return s.CreateChannel(ctx, ch, blob)
}

// GetChannelDecrypted loads a channel and decrypts its config blob with box,
// returning the channel and its plaintext config. box must be non-nil.
func GetChannelDecrypted(ctx context.Context, s ChannelStore, box *secrets.Box, orgID, id int64) (Channel, []byte, error) {
	ch, blob, err := s.GetChannel(ctx, orgID, id)
	if err != nil {
		return Channel{}, nil, err
	}
	cfg, err := box.Decrypt(blob)
	if err != nil {
		return Channel{}, nil, err
	}
	return ch, cfg, nil
}
