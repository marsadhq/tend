package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDeliveryHold(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 15 * time.Minute, "0": 0, "60": time.Hour, " 5 ": 5 * time.Minute, "1440": 24 * time.Hour} {
		t.Setenv(EnvDeliveryHoldMinutes, v)
		got, err := LoadDeliveryHold()
		if err != nil || got != want {
			t.Errorf("%s=%q: got %v, %v; want %v", EnvDeliveryHoldMinutes, v, got, err, want)
		}
	}
	for _, v := range []string{"-1", "15m", "1441", "x"} {
		t.Setenv(EnvDeliveryHoldMinutes, v)
		if _, err := LoadDeliveryHold(); err == nil || !strings.Contains(err.Error(), EnvDeliveryHoldMinutes) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvDeliveryHoldMinutes, v, err)
		}
	}
}
