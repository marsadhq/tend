package config

import (
	"os"
	"testing"
)

func TestLoadInstance(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	for v, want := range map[string]string{"": host, "  ": host, "primary": "primary", " eu-1 ": "eu-1"} {
		t.Setenv(EnvInstance, v)
		if got := LoadInstance(); got != want {
			t.Errorf("%s=%q: LoadInstance() = %q, want %q", EnvInstance, v, got, want)
		}
	}
}
