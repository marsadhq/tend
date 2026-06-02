// Package core holds cross-cutting domain types shared across the suite:
// tenancy (Org) and the generic event record.
package core

import "time"

// Org is a tenant. Every tenant-scoped row carries its OrgID.
type Org struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}
