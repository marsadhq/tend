package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvPingBodyKB sets how much of a ping's request body is kept in the ping
// log, in whole kB (1000 bytes): 0 keeps none, at most 100 (what Healthchecks
// keeps). The server announces it in the Ping-Body-Limit reply header.
const EnvPingBodyKB = "TEND_PING_BODY_KB"

// DefaultPingBodyKB is the limit when the variable is unset: 10 kB, enough
// for the tail of a job's output (tend exec and runitor send 10 kB) while a
// heartbeat's 1000 kept pings stay within its 10 MB log budget.
const DefaultPingBodyKB = 10

// MaxPingBodyKB is the largest limit, Healthchecks' 100 kB.
const MaxPingBodyKB = 100

// LoadPingBodyLimit reads TEND_PING_BODY_KB and returns the limit in bytes.
// A value that is not a whole number from 0 to 100 is an error naming the
// variable.
func LoadPingBodyLimit() (int, error) {
	kb := DefaultPingBodyKB
	if v := strings.TrimSpace(os.Getenv(EnvPingBodyKB)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > MaxPingBodyKB {
			return 0, fmt.Errorf("%s=%q: want a whole number of kB from 0 (keep no ping bodies) to %d", EnvPingBodyKB, v, MaxPingBodyKB)
		}
		kb = n
	}
	return kb * 1000, nil
}
