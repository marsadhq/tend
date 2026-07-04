package config

import (
	"os"
	"strings"
)

type Config struct {
	Driver    string // "sqlite" | "postgres"
	DSN       string // empty when TEND_DB is unset (commands that touch the DB must refuse; see cli.Run)
	MasterKey string // base64, for secret encryption; required once secrets are used
}

// Load reads configuration from the environment.
//
// TEND_DB has NO default: an unset TEND_DB leaves DSN empty and commands that
// open the database refuse to run. The old default ("./tend.db", relative to
// the current working directory) silently created or read a different
// database per directory, which repeatedly produced wrong-DB incidents in
// production; a hard refusal is the fix that does not depend on operator
// vigilance.
func Load() (Config, error) {
	db := os.Getenv("TEND_DB")
	c := Config{Driver: "sqlite", DSN: "", MasterKey: os.Getenv("TEND_MASTER_KEY")}
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		c.Driver, c.DSN = "postgres", db
	} else if db != "" {
		c.DSN = db
	}
	return c, nil
}
