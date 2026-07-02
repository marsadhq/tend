package cli

import (
	"strings"
	"testing"
)

// TestRedactDSN verifies the diagnostic DSN redaction: Postgres userinfo (incl.
// password) is stripped while host/db remain, and a SQLite path resolves to an
// absolute path so the exact file is unambiguous.
func TestRedactDSN(t *testing.T) {
	pg := redactDSN("postgres", "postgres://tend:s3cr3t@db.internal:5432/tend?sslmode=disable")
	if strings.Contains(pg, "s3cr3t") {
		t.Errorf("redactDSN leaked the password: %q", pg)
	}
	if !strings.Contains(pg, "db.internal:5432") {
		t.Errorf("redactDSN dropped the host: %q", pg)
	}
	if !strings.Contains(pg, "/tend") {
		t.Errorf("redactDSN dropped the database name: %q", pg)
	}

	sq := redactDSN("sqlite", "tend.db")
	if !strings.HasPrefix(sq, "/") {
		t.Errorf("redactDSN(sqlite) should be an absolute path, got %q", sq)
	}
	if !strings.HasSuffix(sq, "tend.db") {
		t.Errorf("redactDSN(sqlite) should end in tend.db, got %q", sq)
	}
}
