package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/notify"
)

// TestRuleAddRefusesEventsThatNeverFire: `tend rule add` refuses an event type
// that is never delivered (it used to store the rule, which then did nothing)
// and lists the valid ones; alertable types are accepted.
func TestRuleAddRefusesEventsThatNeverFire(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", "ops", "-type", "webhook"},
		strings.NewReader(`{"url":"https://example.invalid/h"}`), &stdout, &stderr); err != nil {
		t.Fatalf("channel add: %v\nstderr: %s", err, stderr.String())
	}

	for ev, want := range map[string]string{
		"run.succeeded": "never triggers a notification",
		"run.started":   "never triggers a notification",
		"heartbeat.ok":  "unknown event",
	} {
		err := cli.Run(ctx, cfg, []string{"rule", "add", "-channel", "ops", "-event", ev}, nil, &stdout, &stderr)
		if err == nil {
			t.Fatalf("rule add -event %s: err = nil, want a refusal", ev)
		}
		for _, w := range []string{want, "run.failed", "run.recovered", "notification.failed"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("rule add -event %s: error %q does not mention %q", ev, err, w)
			}
		}
	}

	for _, ev := range notify.AlertableTypes() {
		if err := cli.Run(ctx, cfg, []string{"rule", "add", "-channel", "ops", "-event", ev}, nil, &stdout, &stderr); err != nil {
			t.Errorf("rule add -event %s: %v", ev, err)
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
	if len(rules) != len(notify.AlertableTypes()) {
		t.Fatalf("stored %d rules, want %d (one per alertable type, none for the refused ones)", len(rules), len(notify.AlertableTypes()))
	}
}

// TestRuleListMarksRulesThatNeverFire: a rule stored before event types were
// checked is flagged in `tend rule list`.
func TestRuleListMarksRulesThatNeverFire(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", "ops", "-type", "webhook"},
		strings.NewReader(`{"url":"https://example.invalid/h"}`), &stdout, &stderr); err != nil {
		t.Fatalf("channel add: %v\nstderr: %s", err, stderr.String())
	}
	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chans, err := st.ListChannels(ctx, org.ID)
	if err != nil || len(chans) != 1 {
		t.Fatalf("ListChannels = %v, %v", chans, err)
	}
	if _, err := st.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chans[0].ID, EventType: "run.succeeded", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := cli.Run(ctx, cfg, []string{"rule", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("rule list: %v", err)
	}
	if !strings.Contains(stdout.String(), "never fires") {
		t.Errorf("rule list does not flag the run.succeeded rule:\n%s", stdout.String())
	}
}
