package cli_test

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
)

// TestHeartbeatPingKey: `heartbeat ping-key` shows the org's ping key (or how
// to make one), -new generates one, -set adopts one from stdin (a
// Healthchecks project's ping key), -clear removes it; `heartbeat show`
// prints the slug URL once there is a key.
func TestHeartbeatPingKey(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	t.Setenv("TEND_BASE_URL", "https://tend.example.com")
	run := func(stdin string, args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := cli.Run(ctx, cfg, args, strings.NewReader(stdin), &stdout, &stderr)
		return stdout.String(), err
	}

	out, err := run("", "heartbeat", "ping-key")
	if err != nil || !strings.Contains(out, "no ping key") || !strings.Contains(out, "-new") {
		t.Fatalf("ping-key with none: %q, %v; want a hint to create one", out, err)
	}

	out, err = run("", "heartbeat", "ping-key", "-new")
	if err != nil {
		t.Fatalf("ping-key -new: %v", err)
	}
	key := regexp.MustCompile(`ping key: ([A-Za-z0-9_-]{22})\n`).FindStringSubmatch(out)
	if key == nil {
		t.Fatalf("ping-key -new printed %q, want a 22-character key", out)
	}
	if !strings.Contains(out, "https://tend.example.com/ping/"+key[1]+"/<heartbeat-name>") {
		t.Errorf("ping-key -new does not show the slug URL form: %q", out)
	}
	if out, _ := run("", "heartbeat", "ping-key"); !strings.Contains(out, key[1]) {
		t.Errorf("ping-key does not show the key made by -new: %q", out)
	}

	if _, err := run("fqOOd6-F4MMNuCEnzTU01w\n", "heartbeat", "ping-key", "-set"); err != nil {
		t.Fatalf("ping-key -set: %v", err)
	}
	if _, err := run("", "heartbeat", "add", "-name", "db-backup", "-period", "60"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run("", "heartbeat", "show", "db-backup"); !strings.Contains(out, "slug url:  https://tend.example.com/ping/fqOOd6-F4MMNuCEnzTU01w/db-backup") {
		t.Errorf("show does not print the slug URL: %q", out)
	}
	for _, bad := range []string{"", "has space", "slash/inside", strings.Repeat("k", 129)} {
		if _, err := run(bad+"\n", "heartbeat", "ping-key", "-set"); err == nil {
			t.Errorf("ping-key -set accepted %q", bad)
		}
	}

	if _, err := run("", "heartbeat", "ping-key", "-clear"); err != nil {
		t.Fatalf("ping-key -clear: %v", err)
	}
	if out, _ := run("", "heartbeat", "ping-key"); !strings.Contains(out, "no ping key") {
		t.Errorf("after -clear: %q", out)
	}
	if out, _ := run("", "heartbeat", "show", "db-backup"); strings.Contains(out, "slug url") {
		t.Errorf("show prints a slug URL without a key: %q", out)
	}
}
