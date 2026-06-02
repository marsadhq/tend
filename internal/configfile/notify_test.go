package configfile_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// newBox returns a Box keyed with 32 zero bytes (test-only).
func newBox(t *testing.T) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return b
}

// TestParseNotificationsAndHeartbeats parses the extended testdata and asserts
// channels/rules/heartbeats are mapped, with {{ secret.* }} kept verbatim.
func TestParseNotificationsAndHeartbeats(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "jobs.yaml"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	cfg, err := configfile.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// --- channels ---
	if len(cfg.Channels) != 2 {
		t.Fatalf("want 2 channels, got %d", len(cfg.Channels))
	}
	c0 := cfg.Channels[0]
	if c0.Name != "ops-slack" || c0.Type != "slack" {
		t.Errorf("channel[0] = %q/%q; want ops-slack/slack", c0.Name, c0.Type)
	}
	if got, _ := c0.Config["webhook_url"].(string); got != "{{ secret.slack_webhook }}" {
		t.Errorf("channel[0] webhook_url = %q; want verbatim placeholder", got)
	}
	c1 := cfg.Channels[1]
	if c1.Name != "db-oncall" || c1.Type != "webhook" {
		t.Errorf("channel[1] = %q/%q; want db-oncall/webhook", c1.Name, c1.Type)
	}
	if got, _ := c1.Config["url"].(string); got != "{{ secret.pager_webhook }}" {
		t.Errorf("channel[1] url = %q; want verbatim placeholder", got)
	}

	// --- rules ---
	if len(cfg.Rules) != 2 {
		t.Fatalf("want 2 rule specs, got %d", len(cfg.Rules))
	}
	r0 := cfg.Rules[0]
	if r0.Channel != "ops-slack" {
		t.Errorf("rule[0].Channel = %q; want ops-slack", r0.Channel)
	}
	if len(r0.Events) != 2 || r0.Events[0] != "run.failed" || r0.Events[1] != "heartbeat.missed" {
		t.Errorf("rule[0].Events = %v; want [run.failed heartbeat.missed]", r0.Events)
	}
	if r0.Job != "" {
		t.Errorf("rule[0].Job = %q; want empty (all jobs)", r0.Job)
	}
	r1 := cfg.Rules[1]
	if r1.Channel != "db-oncall" || r1.Job != "nightly-backup" {
		t.Errorf("rule[1] = channel %q job %q; want db-oncall/nightly-backup", r1.Channel, r1.Job)
	}
	if len(r1.Events) != 1 || r1.Events[0] != "run.failed" {
		t.Errorf("rule[1].Events = %v; want [run.failed]", r1.Events)
	}

	// --- heartbeats ---
	if len(cfg.Heartbeats) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(cfg.Heartbeats))
	}
	h := cfg.Heartbeats[0]
	if h.Name != "external-backup" || h.PeriodSeconds != 86400 || h.GraceSeconds != 3600 {
		t.Errorf("heartbeat = %q period=%d grace=%d; want external-backup/86400/3600", h.Name, h.PeriodSeconds, h.GraceSeconds)
	}
}

// reconcileTestStore opens a fresh migrated SQLite store + default org.
func reconcileTestStore(t *testing.T) (store.Store, int64) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "tend_test.db")
	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	return st, org.ID
}

// TestReconcileNotificationsAndHeartbeats exercises the full notifications +
// heartbeats reconcile: channels resolve {{ secret.* }} to real values, rules
// expand one-per-event with correct job scope, heartbeats get a token, and a
// re-Reconcile is idempotent (same channel config decrypts, token unchanged).
func TestReconcileNotificationsAndHeartbeats(t *testing.T) {
	st, orgID := reconcileTestStore(t)
	box := newBox(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC))

	// Seed referenced secrets (encrypted) and one job referenced by a rule.
	for name, val := range map[string]string{
		"slack_webhook": "https://hooks.slack.com/services/REAL",
		"pager_webhook": "https://pager.example.com/hook/REAL",
	} {
		ct, err := box.Encrypt([]byte(val))
		if err != nil {
			t.Fatalf("encrypt %q: %v", name, err)
		}
		if err := st.PutSecret(ctx, orgID, name, ct); err != nil {
			t.Fatalf("put secret %q: %v", name, err)
		}
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "jobs.yaml"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	cfg, err := configfile.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	res, err := configfile.Reconcile(ctx, st, orgID, clk, box, cfg, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Channels != 2 {
		t.Errorf("Channels = %d; want 2", res.Channels)
	}
	// 2 events on ops-slack + 1 event on db-oncall = 3 rule rows.
	if res.Rules != 3 {
		t.Errorf("Rules = %d; want 3", res.Rules)
	}
	if res.Heartbeats != 1 {
		t.Errorf("Heartbeats = %d; want 1", res.Heartbeats)
	}
	if res.Created != 2 {
		t.Errorf("jobs Created = %d; want 2", res.Created)
	}

	// --- channels: config decrypts to the REAL url, not the placeholder ---
	channels, err := st.ListChannels(ctx, orgID)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("ListChannels = %d; want 2", len(channels))
	}
	chByName := map[string]notify.Channel{}
	for _, c := range channels {
		chByName[c.Name] = c
	}
	slack := chByName["ops-slack"]
	_, plain, err := notify.GetChannelDecrypted(ctx, st, box, orgID, slack.ID)
	if err != nil {
		t.Fatalf("GetChannelDecrypted ops-slack: %v", err)
	}
	var slackCfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal(plain, &slackCfg); err != nil {
		t.Fatalf("unmarshal slack config: %v", err)
	}
	if slackCfg.WebhookURL != "https://hooks.slack.com/services/REAL" {
		t.Errorf("slack webhook_url = %q; want resolved real URL", slackCfg.WebhookURL)
	}
	if strings.Contains(slackCfg.WebhookURL, "secret.") {
		t.Error("slack config still contains an unresolved {{ secret.* }} placeholder")
	}

	// --- rules: one row per event with correct job scope ---
	rules, err := st.ListRules(ctx, orgID)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("ListRules = %d; want 3", len(rules))
	}
	nb, err := st.GetJobByName(ctx, orgID, "nightly-backup")
	if err != nil {
		t.Fatalf("GetJobByName nightly-backup: %v", err)
	}
	// Count: ops-slack rules (job_id 0) and the db-oncall job-scoped rule.
	var unscoped, jobScoped int
	for _, r := range rules {
		if r.JobID == 0 {
			unscoped++
		}
		if r.ChannelID == chByName["db-oncall"].ID {
			if r.JobID != nb.ID {
				t.Errorf("db-oncall rule JobID = %d; want %d (nightly-backup)", r.JobID, nb.ID)
			}
			jobScoped++
		}
		if !r.Enabled {
			t.Errorf("rule %d not enabled; want enabled", r.ID)
		}
	}
	if unscoped != 2 {
		t.Errorf("unscoped (job_id 0) rules = %d; want 2", unscoped)
	}
	if jobScoped != 1 {
		t.Errorf("job-scoped db-oncall rules = %d; want 1", jobScoped)
	}

	// --- heartbeats: a token was generated ---
	hbs, err := st.ListHeartbeats(ctx, orgID)
	if err != nil {
		t.Fatalf("ListHeartbeats: %v", err)
	}
	if len(hbs) != 1 {
		t.Fatalf("ListHeartbeats = %d; want 1", len(hbs))
	}
	firstToken := hbs[0].Token
	if firstToken == "" {
		t.Fatal("heartbeat token is empty after reconcile")
	}

	// --- idempotent re-Reconcile: same counts, heartbeat token unchanged ---
	res2, err := configfile.Reconcile(ctx, st, orgID, clk, box, cfg, true)
	if err != nil {
		t.Fatalf("Reconcile (2nd): %v", err)
	}
	if res2.Channels != 2 || res2.Rules != 3 || res2.Heartbeats != 1 {
		t.Errorf("2nd Reconcile counts = ch %d rules %d hb %d; want 2/3/1", res2.Channels, res2.Rules, res2.Heartbeats)
	}
	if res2.Updated != 2 {
		t.Errorf("2nd Reconcile jobs Updated = %d; want 2", res2.Updated)
	}
	channels2, _ := st.ListChannels(ctx, orgID)
	if len(channels2) != 2 {
		t.Errorf("channels after 2nd reconcile = %d; want 2 (no dupes)", len(channels2))
	}
	rules2, _ := st.ListRules(ctx, orgID)
	if len(rules2) != 3 {
		t.Errorf("rules after 2nd reconcile = %d; want 3 (no dupes)", len(rules2))
	}
	hbs2, _ := st.ListHeartbeats(ctx, orgID)
	if len(hbs2) != 1 {
		t.Fatalf("heartbeats after 2nd reconcile = %d; want 1", len(hbs2))
	}
	if hbs2[0].Token != firstToken {
		t.Errorf("heartbeat token changed on re-reconcile: was %q now %q", firstToken, hbs2[0].Token)
	}
}

// TestReconcileMissingSecret asserts a referenced-but-absent secret yields a
// clear error naming the secret.
func TestReconcileMissingSecret(t *testing.T) {
	st, orgID := reconcileTestStore(t)
	box := newBox(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now())

	cfg := configfile.Config{
		Channels: []configfile.ChannelSpec{{
			Name: "ops", Type: "slack",
			Config: map[string]any{"webhook_url": "{{ secret.missing_one }}"},
		}},
	}
	_, err := configfile.Reconcile(ctx, st, orgID, clk, box, cfg, true)
	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "missing_one") {
		t.Errorf("error %q does not name the missing secret", err.Error())
	}
}

// TestReconcileUnknownJob asserts a rule referencing an unknown job: name errors.
func TestReconcileUnknownJob(t *testing.T) {
	st, orgID := reconcileTestStore(t)
	box := newBox(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now())

	ct, _ := box.Encrypt([]byte("https://hooks.slack.com/x"))
	if err := st.PutSecret(ctx, orgID, "wh", ct); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	cfg := configfile.Config{
		Channels: []configfile.ChannelSpec{{
			Name: "ops", Type: "slack",
			Config: map[string]any{"webhook_url": "{{ secret.wh }}"},
		}},
		Rules: []configfile.RuleSpec{{
			Channel: "ops", Events: []string{"run.failed"}, Job: "no-such-job",
		}},
	}
	_, err := configfile.Reconcile(ctx, st, orgID, clk, box, cfg, true)
	if err == nil {
		t.Fatal("expected error for unknown job, got nil")
	}
	if !strings.Contains(err.Error(), "no-such-job") {
		t.Errorf("error %q does not name the unknown job", err.Error())
	}
}

// TestReconcileUnknownChannelRule asserts a rule referencing an unknown channel
// errors at reconcile time (when the spec is built directly, bypassing Parse).
func TestReconcileUnknownChannelRule(t *testing.T) {
	st, orgID := reconcileTestStore(t)
	box := newBox(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now())

	cfg := configfile.Config{
		Rules: []configfile.RuleSpec{{
			Channel: "ghost", Events: []string{"run.failed"},
		}},
	}
	_, err := configfile.Reconcile(ctx, st, orgID, clk, box, cfg, true)
	if err == nil {
		t.Fatal("expected error for unknown channel, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q does not name the unknown channel", err.Error())
	}
}
