package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/store"
)

// fillAndPrune writes n events with a 4 KiB payload each and prunes them all,
// leaving their pages on the free list.
func fillAndPrune(t *testing.T, ctx context.Context, s store.Store, n int) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	payload := strings.Repeat("x", 4096)
	for i := 0; i < n; i++ {
		if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.started", Source: "test", Payload: payload}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	if _, err := s.PruneEvents(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
}

// sqlitePragma reads an integer PRAGMA through the store's handle.
func sqlitePragma(t *testing.T, s store.Store, name string) int64 {
	t.Helper()
	var v int64
	if err := store.RawDB(s).QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// TestReclaimSpaceCompactsSQLite pins that space freed by a large prune is
// given back: the first time by one VACUUM that also switches the file to
// incremental auto-vacuum, afterwards by incremental vacuum steps. The prune
// alone used to leave the file at its high-water mark forever.
func TestReclaimSpaceCompactsSQLite(t *testing.T) {
	ctx := context.Background()
	defer store.SetReclaimThresholds(64<<10, 0.25, 64<<20)()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		if _, ok := s.(*store.SQLiteStore); !ok {
			fillAndPrune(t, ctx, s, 50)
			if n, err := s.ReclaimSpace(ctx); err != nil || n != 0 {
				t.Fatalf("Postgres ReclaimSpace = %d, %v; want 0, nil (autovacuum's job)", n, err)
			}
			return
		}
		if mode := sqlitePragma(t, s, "auto_vacuum"); mode != 0 {
			t.Fatalf("a fresh database has auto_vacuum=%d, want 0 (none) for this test to mean anything", mode)
		}

		fillAndPrune(t, ctx, s, 400)
		before := sqlitePragma(t, s, "page_count")
		n, err := s.ReclaimSpace(ctx)
		if err != nil {
			t.Fatalf("ReclaimSpace: %v", err)
		}
		if n <= 0 || sqlitePragma(t, s, "page_count") >= before {
			t.Fatalf("ReclaimSpace reclaimed %d bytes (pages %d -> %d), want the file to shrink", n, before, sqlitePragma(t, s, "page_count"))
		}
		if mode := sqlitePragma(t, s, "auto_vacuum"); mode != 2 {
			t.Fatalf("auto_vacuum after the first compaction = %d, want 2 (incremental)", mode)
		}

		fillAndPrune(t, ctx, s, 400)
		before = sqlitePragma(t, s, "page_count")
		if n, err := s.ReclaimSpace(ctx); err != nil || n <= 0 {
			t.Fatalf("incremental ReclaimSpace = %d, %v; want space back", n, err)
		}
		if free := sqlitePragma(t, s, "freelist_count"); free != 0 {
			t.Errorf("freelist_count after an incremental reclaim = %d, want 0", free)
		}
		if after := sqlitePragma(t, s, "page_count"); after >= before {
			t.Errorf("pages %d -> %d, want fewer", before, after)
		}
	})
}

// TestReclaimSpaceLeavesSmallGainsAlone: a little free space is reused by the
// next inserts, so it is not worth a rewrite.
func TestReclaimSpaceLeavesSmallGainsAlone(t *testing.T) {
	ctx := context.Background()
	defer store.SetReclaimThresholds(64<<20, 0.25, 512<<20)()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		fillAndPrune(t, ctx, s, 20)
		if n, err := s.ReclaimSpace(ctx); err != nil || n != 0 {
			t.Fatalf("ReclaimSpace = %d, %v; want 0, nil", n, err)
		}
		if _, ok := s.(*store.SQLiteStore); ok {
			if mode := sqlitePragma(t, s, "auto_vacuum"); mode != 0 {
				t.Errorf("auto_vacuum = %d, want it left at 0 when nothing was worth reclaiming", mode)
			}
		}
	})
}

// TestReclaimSpaceWillNotRewriteALargeFileWhileServing: a file over the
// rewrite limit is reported, not vacuumed.
func TestReclaimSpaceWillNotRewriteALargeFileWhileServing(t *testing.T) {
	ctx := context.Background()
	defer store.SetReclaimThresholds(64<<10, 0.25, 64<<10)()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		if _, ok := s.(*store.SQLiteStore); !ok {
			return // Postgres never compacts
		}
		fillAndPrune(t, ctx, s, 100)
		before := sqlitePragma(t, s, "page_count")
		_, err := s.ReclaimSpace(ctx)
		if !errors.Is(err, store.ErrCompactTooLarge) {
			t.Fatalf("ReclaimSpace = %v, want ErrCompactTooLarge", err)
		}
		if after := sqlitePragma(t, s, "page_count"); after != before {
			t.Errorf("pages %d -> %d, want the file untouched", before, after)
		}
	})
}
