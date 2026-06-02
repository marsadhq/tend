package config

import (
	"os"
	"strings"
)

type Config struct {
	Driver    string // "sqlite" | "postgres"
	DSN       string
	MasterKey string // base64, for secret encryption; required once secrets are used
}

func Load() (Config, error) {
	db := os.Getenv("TEND_DB")
	c := Config{Driver: "sqlite", DSN: "tend.db", MasterKey: os.Getenv("TEND_MASTER_KEY")}
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		c.Driver, c.DSN = "postgres", db
	} else if db != "" {
		c.DSN = db
	}
	return c, nil
}
