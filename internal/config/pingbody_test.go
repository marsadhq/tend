package config

import (
	"strings"
	"testing"
)

// TestLoadPingBodyLimit: TEND_PING_BODY_KB is the most of a ping's body kept,
// in kB (1000 bytes): 10 by default, 0 keeps none, 100 at most, as
// Healthchecks keeps; anything else is an error naming the variable.
func TestLoadPingBodyLimit(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{{"", 10_000}, {"0", 0}, {" 64 ", 64_000}, {"100", 100_000}} {
		t.Setenv(EnvPingBodyKB, c.env)
		got, err := LoadPingBodyLimit()
		if err != nil || got != c.want {
			t.Errorf("%s=%q: %d, %v; want %d", EnvPingBodyKB, c.env, got, err, c.want)
		}
	}
	for _, bad := range []string{"-1", "101", "10kB", "1.5"} {
		t.Setenv(EnvPingBodyKB, bad)
		if _, err := LoadPingBodyLimit(); err == nil || !strings.Contains(err.Error(), EnvPingBodyKB) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvPingBodyKB, bad, err)
		}
	}
}
