package config

import "testing"

func TestLoadDefaultsToSQLite(t *testing.T) {
	t.Setenv("TEND_DB", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Driver != "sqlite" || c.DSN != "tend.db" {
		t.Fatalf("want sqlite/tend.db, got %s/%s", c.Driver, c.DSN)
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
