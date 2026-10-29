package configfile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/notify"
)

const repeatChannels = `
notifications:
  channels:
    - name: ops
      type: webhook
      config: {url: "https://example.invalid/h"}
  rules:
`

// TestParseRuleRepeat: a rule's repeat is "every", 0 or a duration, and
// paces run.failed, so a rule that does not route run.failed cannot set it.
func TestParseRuleRepeat(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "30m": 30 * time.Minute, "every": 0, "0": 0, `"2h"`: 2 * time.Hour} {
		cfg, err := configfile.Parse([]byte(repeatChannels + `    - channel: ops
      events: [run.failed, run.recovered]
      repeat: ` + in + "\n"))
		if err != nil {
			t.Fatalf("repeat %s: %v", in, err)
		}
		if r := cfg.Rules[0].Repeat; r == nil || *r != want {
			t.Errorf("repeat %s parsed as %v, want %v", in, r, want)
		}
	}
	cfg, err := configfile.Parse([]byte(repeatChannels + "    - channel: ops\n      events: [run.failed]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].Repeat != nil {
		t.Errorf("a rule without repeat parsed as %v, want the default", *cfg.Rules[0].Repeat)
	}
	for in, wants := range map[string][]string{
		"10s":   {`"ops"`, "10s", "every"},
		"never": {`"ops"`, "never", "every"},
		"2d":    {`"ops"`, "2d"},
	} {
		_, err := configfile.Parse([]byte(repeatChannels + "    - channel: ops\n      events: [run.failed]\n      repeat: " + in + "\n"))
		if err == nil {
			t.Fatalf("repeat %s was accepted", in)
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("repeat %s: error %q does not mention %s", in, err, w)
			}
		}
	}
	_, err = configfile.Parse([]byte(repeatChannels + "    - channel: ops\n      events: [heartbeat.missed]\n      repeat: 1h\n"))
	if err == nil || !strings.Contains(err.Error(), "run.failed") || !strings.Contains(err.Error(), `"ops"`) {
		t.Fatalf("repeat on a rule without run.failed: %v, want a refusal naming run.failed", err)
	}
}

// TestReconcileRuleRepeat: sync stores the repeat on the rule's run.failed
// row only, and a re-sync without it puts the default back.
func TestReconcileRuleRepeat(t *testing.T) {
	st, orgID := reconcileTestStore(t)
	box := newBox(t)
	ctx := context.Background()
	sync := func(repeat *time.Duration) {
		t.Helper()
		cfg := configfile.Config{
			Channels: []configfile.ChannelSpec{{Name: "ops", Type: "webhook", Config: map[string]any{"url": "https://example.invalid/h"}}},
			Rules:    []configfile.RuleSpec{{Channel: "ops", Events: []string{"run.failed", "run.recovered"}, Repeat: repeat}},
		}
		if _, err := configfile.Reconcile(ctx, st, orgID, clock.NewFake(time.Now()), box, cfg, true); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	read := func() map[string]*time.Duration {
		t.Helper()
		rules, err := st.ListRules(ctx, orgID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*time.Duration{}
		for _, r := range rules {
			out[r.EventType] = r.Repeat
		}
		return out
	}
	half := 30 * time.Minute
	sync(&half)
	got := read()
	if r := got["run.failed"]; r == nil || *r != half {
		t.Errorf("run.failed repeat = %v, want 30m", r)
	}
	if r := got["run.recovered"]; r != nil {
		t.Errorf("run.recovered repeat = %v, want none", *r)
	}
	sync(nil)
	if r := read()["run.failed"]; r != nil {
		t.Errorf("after a sync without repeat, run.failed repeat = %v, want the default", *r)
	}
	_ = notify.DefaultRepeat
}
