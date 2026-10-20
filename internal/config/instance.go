package config

import (
	"os"
	"strings"
)

// EnvInstance names this Tend installation in the marsad envelope of the
// notifications it sends (source.instance), so a receiver such as Ward can
// tell two Tends apart.
const EnvInstance = "TEND_INSTANCE"

// LoadInstance reads TEND_INSTANCE, trimmed of surrounding space. Unset or
// blank, it is the host's name, or empty when that cannot be read (the
// envelope allows an empty instance).
func LoadInstance() string {
	if v := strings.TrimSpace(os.Getenv(EnvInstance)); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}
