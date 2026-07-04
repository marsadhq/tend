package config

import "testing"

// An unset TEND_DB yields an EMPTY DSN (no ./tend.db fallback): cli.Run
// refuses to open a database in that case.
func TestLoadWithoutTendDBLeavesDSNEmpty(t *testing.T) {
	t.Setenv("TEND_DB", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Driver != "sqlite" || c.DSN != "" {
		t.Fatalf("want sqlite/<empty>, got %s/%q", c.Driver, c.DSN)
	}
}

func TestLoadParsesPostgresURL(t *testing.T) {
	t.Setenv("TEND_DB", "postgres://u:p@localhost/tend")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Driver != "postgres" || c.DSN != "postgres://u:p@localhost/tend" {
		t.Fatalf("want postgres/<url>, got %s/%s", c.Driver, c.DSN)
	}
}
