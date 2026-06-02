package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestClaimIsExclusiveUnderConcurrency proves that when a single pending run is
// available, exactly one of many concurrent ClaimRun callers wins it and the
// rest get ok=false with no error.
func TestClaimIsExclusiveUnderConcurrency(t *testing.T) {
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			orgID, jobID := seedJob(t, ctx, b.store)

			runID, err := b.store.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}

			const workers = 20
			var (
				start    sync.WaitGroup
				done     sync.WaitGroup
				claimed  atomic.Int64 // count of ok=true
				notFound atomic.Int64 // count of ok=false
				errCount atomic.Int64
				wrongID  atomic.Int64
			)
			start.Add(1)
			done.Add(workers)

			for i := 0; i < workers; i++ {
				go func() {
					defer done.Done()
					start.Wait()
					run, ok, err := b.store.ClaimRun(ctx, "worker")
					if err != nil {
						errCount.Add(1)
						return
					}
					if !ok {
						notFound.Add(1)
						return
					}
					claimed.Add(1)
					if run.ID != runID {
						wrongID.Add(1)
					}
				}()
			}

			start.Done()
			done.Wait()

			if got := errCount.Load(); got != 0 {
				t.Fatalf("ClaimRun returned %d error(s), want 0", got)
			}
			if got := claimed.Load(); got != 1 {
				t.Fatalf("exactly one claim expected, got %d", got)
			}
			if got := wrongID.Load(); got != 0 {
				t.Fatalf("%d goroutine(s) claimed a run with the wrong ID", got)
			}
			if got := notFound.Load(); got != workers-1 {
				t.Fatalf("expected %d ok=false results, got %d", workers-1, got)
			}
		})
	}
}

// TestClaimNoDoubleClaimManyRuns is the stronger property: with M pending runs
// and N workers each looping until the queue drains, every run is claimed
// exactly once. No run may be handed to two workers.
func TestClaimNoDoubleClaimManyRuns(t *testing.T) {
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			orgID, jobID := seedJob(t, ctx, b.store)

			const (
				runs    = 50
				workers = 16
			)
			for i := 0; i < runs; i++ {
				if _, err := b.store.EnqueueRun(ctx, orgID, jobID); err != nil {
					t.Fatalf("EnqueueRun %d: %v", i, err)
				}
			}

			var (
				start    sync.WaitGroup
				done     sync.WaitGroup
				mu       sync.Mutex
				claimed  []int64
				errCount atomic.Int64
			)
			start.Add(1)
			done.Add(workers)

			for w := 0; w < workers; w++ {
				go func() {
					defer done.Done()
					start.Wait()
					for {
						run, ok, err := b.store.ClaimRun(ctx, "worker")
						if err != nil {
							errCount.Add(1)
							return
						}
						if !ok {
							return
						}
						mu.Lock()
						claimed = append(claimed, run.ID)
						mu.Unlock()
					}
				}()
			}

			start.Done()
			done.Wait()

			if got := errCount.Load(); got != 0 {
				t.Fatalf("ClaimRun returned %d error(s), want 0", got)
			}
			if len(claimed) != runs {
				t.Fatalf("total claims = %d, want %d", len(claimed), runs)
			}
			seen := make(map[int64]bool, len(claimed))
			for _, id := range claimed {
				if seen[id] {
					t.Fatalf("run %d was claimed more than once", id)
				}
				seen[id] = true
			}
		})
	}
}
