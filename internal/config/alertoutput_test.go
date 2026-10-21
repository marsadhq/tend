package config

import (
	"strings"
	"testing"
)

func TestLoadAlertOutputLines(t *testing.T) {
	for v, want := range map[string]int{"": 10, "0": 0, "3": 3, " 20 ": 20, "50": 50} {
		t.Setenv(EnvAlertOutputLines, v)
		got, err := LoadAlertOutputLines()
		if err != nil || got != want {
			t.Errorf("%s=%q: got %d, %v; want %d", EnvAlertOutputLines, v, got, err, want)
		}
	}
	for _, v := range []string{"51", "-1", "ten", "1.5"} {
		t.Setenv(EnvAlertOutputLines, v)
		if _, err := LoadAlertOutputLines(); err == nil || !strings.Contains(err.Error(), EnvAlertOutputLines) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvAlertOutputLines, v, err)
		}
	}
}
