package jobs

import "slices"

// InFlightRuns returns the IDs of the runs r has registered as executing in
// this process, in ascending order. Test-only (export_test.go is not part of
// the normal build).
func (r *Runner) InFlightRuns() []int64 {
	r.inFlight.mu.Lock()
	defer r.inFlight.mu.Unlock()
	ids := make([]int64, 0, len(r.inFlight.limits))
	for id := range r.inFlight.limits {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
