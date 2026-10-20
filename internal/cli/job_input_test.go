package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
)

// TestJobAddRefusesANegativeTimeout: a negative -timeout is refused rather
// than stored.
func TestJobAddRefusesANegativeTimeout(t *testing.T) {
	cfg := tempConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"job", "add", "-name", "neg", "-command", "true", "-timeout", "-5"}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Fatalf("job add -timeout -5: err = %v, want a refusal naming -timeout", err)
	}
	stdout.Reset()
	if err := cli.Run(context.Background(), cfg, []string{"job", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "neg") {
		t.Errorf("the refused job was stored:\n%s", stdout.String())
	}
}
