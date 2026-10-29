package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
)

// TestRuleAddRepeat: `tend rule add -repeat` sets how often a run.failed
// rule reminds; without it the rule has its channel type's default, and the
// command says which applies. A repeat on another event, or one that does
// not parse, is refused. `tend rule list` shows the interval of each
// run.failed rule.
func TestRuleAddRepeat(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	for name, typ := range map[string]string{"ops": "slack", "ward": "webhook"} {
		if err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", name, "-type", typ},
			strings.NewReader(`{"url":"https://example.invalid/h","webhook_url":"https://example.invalid/h"}`), &stdout, &stderr); err != nil {
			t.Fatalf("channel add %s: %v\nstderr: %s", name, err, stderr.String())
		}
	}
	add := func(args ...string) (string, error) {
		t.Helper()
		stdout.Reset()
		err := cli.Run(ctx, cfg, append([]string{"rule", "add"}, args...), nil, &stdout, &stderr)
		return stdout.String(), err
	}
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"-channel", "ops", "-event", "run.failed"}, "reminders every 1h, the default for slack"},
		{[]string{"-channel", "ward", "-event", "run.failed"}, "every failure, the default for webhook"},
		{[]string{"-channel", "ops", "-event", "run.failed", "-job", "", "-repeat", "30m"}, "reminders every 30m"},
		{[]string{"-channel", "ward", "-event", "run.failed", "-repeat", "1h"}, "reminders every 1h"},
	} {
		out, err := add(tc.args...)
		if err != nil {
			t.Fatalf("rule add %v: %v", tc.args, err)
		}
		if !strings.Contains(out, tc.says) {
			t.Errorf("rule add %v said %q, want it to say %q", tc.args, out, tc.says)
		}
	}
	if out, err := add("-channel", "ops", "-event", "run.recovered"); err != nil || strings.Contains(out, "every") {
		t.Errorf("rule add run.recovered = %q, %v; want no word on reminders", out, err)
	}
	for args, want := range map[string]string{
		"-channel ops -event heartbeat.missed -repeat 1h": "run.failed",
		"-channel ops -event run.failed -repeat 10s":      "every",
		"-channel ops -event run.failed -repeat sometime": "every",
	} {
		if _, err := add(strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rule add %s = %v, want a refusal mentioning %s", args, err, want)
		}
	}

	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := st.ListRules(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	chans, err := st.ListChannels(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]string{}
	for _, c := range chans {
		byID[c.ID] = c.Name
	}
	var failed int
	for _, r := range rules {
		if r.EventType != "run.failed" {
			continue
		}
		failed++
		want := map[string]time.Duration{"ops": 30 * time.Minute, "ward": time.Hour}[byID[r.ChannelID]]
		if r.Repeat == nil || *r.Repeat != want {
			t.Errorf("%s run.failed repeat = %v, want %v", byID[r.ChannelID], r.Repeat, want)
		}
	}
	if failed != 2 {
		t.Errorf("%d run.failed rules, want 2 (the second add of each replaced the first)", failed)
	}

	stdout.Reset()
	if err := cli.Run(ctx, cfg, []string{"rule", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	list := stdout.String()
	if !strings.Contains(list, "REPEAT") {
		t.Fatalf("rule list has no REPEAT column:\n%s", list)
	}
	for _, line := range strings.Split(list, "\n") {
		switch {
		case strings.HasPrefix(line, "ops ") && strings.Contains(line, "run.failed"):
			if !strings.Contains(line, " 30m ") {
				t.Errorf("ops run.failed line %q does not show 30m", line)
			}
		case strings.HasPrefix(line, "ward ") && strings.Contains(line, "run.failed"):
			if !strings.Contains(line, " 1h ") {
				t.Errorf("ward run.failed line %q does not show 1h", line)
			}
		case strings.HasPrefix(line, "ops ") && strings.Contains(line, "run.recovered"):
			if !strings.Contains(line, " - ") {
				t.Errorf("run.recovered line %q shows a repeat", line)
			}
		}
	}

	// Back to the default: listed as such.
	if _, err := add("-channel", "ops", "-event", "run.failed"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := cli.Run(ctx, cfg, []string{"rule", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "1h (default)") {
		t.Errorf("rule list does not show the default:\n%s", stdout.String())
	}
}
