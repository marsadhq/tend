package store

import "database/sql"

// RawDB exposes a store's underlying *sql.DB to external tests for raw schema
// assertions. Test-only (export_test.go is not part of the normal build).
func RawDB(s Store) *sql.DB {
	switch v := s.(type) {
	case *SQLiteStore:
		return v.db
	case *PostgresStore:
		return v.db
	}
	return nil
}

// FormatTime exposes the storage timestamp layout to external tests (e.g. for
// backdating rows to exercise retention pruning).
var FormatTime = formatTime

// SetReclaimThresholds lowers ReclaimSpace's thresholds for a test and returns
// a func that restores them.
func SetReclaimThresholds(minFreeBytes int64, minFreeRatio float64, maxVacuumBytes int64) (restore func()) {
	oldMin, oldRatio, oldMax := reclaimMinFreeBytes, reclaimMinFreeRatio, reclaimMaxVacuumBytes
	reclaimMinFreeBytes, reclaimMinFreeRatio, reclaimMaxVacuumBytes = minFreeBytes, minFreeRatio, maxVacuumBytes
	return func() {
		reclaimMinFreeBytes, reclaimMinFreeRatio, reclaimMaxVacuumBytes = oldMin, oldRatio, oldMax
	}
}
