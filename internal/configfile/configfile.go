// Package configfile provides config-as-code support for Tend: parse a YAML
// definition file (jobs, notification channels + rules, heartbeats) and
// reconcile its contents with the persistent store.
//
// v1 reconcile semantics are UPSERT-ONLY for every section: entries that were
// removed from the config are NOT deleted (channels/rules/heartbeats), except
// jobs absent from the config which are DISABLED (not deleted) to preserve M1
// behaviour. Deletion is deferred to a later milestone.
package configfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// -------------------------------------------------------------------
// Parsed config + spec structs
// -------------------------------------------------------------------

// Config is the fully-parsed config file: jobs plus notification channels,
// rules, and heartbeats. The *Spec slices carry the YAML shapes verbatim
// (including unresolved {{ secret.* }} placeholders) for Reconcile to apply.
type Config struct {
	Jobs       []jobs.Job
	Channels   []ChannelSpec
	Rules      []RuleSpec
	Heartbeats []HeartbeatSpec
}

// ChannelSpec is one notification channel from the config. Config string values
// may contain {{ secret.NAME }} placeholders resolved at Reconcile time.
type ChannelSpec struct {
	Name   string
	Type   string
	Config map[string]any
}

// RuleSpec is one notification rule from the config. It expands at Reconcile
// time to one notification_rules row per event in Events. Job, when set, scopes
// the rule to that named job; empty means all jobs (job_id 0).
type RuleSpec struct {
	Channel string
	Events  []string
	Job     string
}

// HeartbeatSpec is one heartbeat from the config.
type HeartbeatSpec struct {
	Name          string
	PeriodSeconds int
	GraceSeconds  int
}

// -------------------------------------------------------------------
// YAML schema structs (internal)
// -------------------------------------------------------------------

// fileRoot is the top-level YAML structure.
type fileRoot struct {
	Jobs          []jobSpec      `yaml:"jobs"`
	Notifications *notifications `yaml:"notifications"`
	Heartbeats    []hbSpec       `yaml:"heartbeats"`
}

type notifications struct {
	Channels []channelSpec `yaml:"channels"`
	Rules    []ruleSpec    `yaml:"rules"`
}

type channelSpec struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Config map[string]any `yaml:"config"`
}

type ruleSpec struct {
	Channel string   `yaml:"channel"`
	Events  []string `yaml:"events"`
	Job     string   `yaml:"job"`
}

type hbSpec struct {
	Name          string `yaml:"name"`
	PeriodSeconds int    `yaml:"period_seconds"`
	GraceSeconds  int    `yaml:"grace_seconds"`
}

// jobSpec mirrors the YAML fields for a single job entry.
type jobSpec struct {
	Name            string            `yaml:"name"`
	Type            string            `yaml:"type"`
	Command         string            `yaml:"command"`
	HTTPURL         string            `yaml:"http_url"`
	HTTPMethod      string            `yaml:"http_method"`
	HTTPBody        string            `yaml:"http_body"`
	Cron            string            `yaml:"cron"`
	IntervalSeconds int               `yaml:"interval_seconds"`
	RunAt           string            `yaml:"run_at"` // RFC3339 string; optional
	TimeoutSeconds  int               `yaml:"timeout_seconds"`
	MaxRetries      int               `yaml:"max_retries"`
	Enabled         *bool             `yaml:"enabled"` // nil when omitted → default true
	Env             map[string]string `yaml:"env"`
}

// validChannelTypes enumerates the channel kinds the config may declare.
var validChannelTypes = map[string]bool{
	string(notify.Webhook): true,
	string(notify.Slack):   true,
	string(notify.Discord): true,
	string(notify.SMTP):    true,
}

// -------------------------------------------------------------------
// Parse
// -------------------------------------------------------------------

// Parse unmarshals a YAML config file into a Config: jobs, notification
// channels + rules, and heartbeats. Validation errors name the offending entry.
// Secret refs ({{ secret.* }} in channel config, {{ secret.* }} in job Env) are
// kept verbatim; Reconcile resolves them.
func Parse(data []byte) (Config, error) {
	var root fileRoot
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Config{}, fmt.Errorf("configfile: parse YAML: %w", err)
	}

	var cfg Config

	// --- jobs ---
	cfg.Jobs = make([]jobs.Job, 0, len(root.Jobs))
	for i, spec := range root.Jobs {
		j, err := mapSpec(i, spec)
		if err != nil {
			return Config{}, err
		}
		cfg.Jobs = append(cfg.Jobs, j)
	}

	// --- notification channels ---
	if root.Notifications != nil {
		for i, c := range root.Notifications.Channels {
			if c.Name == "" {
				return Config{}, fmt.Errorf("configfile: channel[%d]: name must not be empty", i)
			}
			if c.Type == "" {
				return Config{}, fmt.Errorf("configfile: channel %q: type is required", c.Name)
			}
			if !validChannelTypes[c.Type] {
				return Config{}, fmt.Errorf("configfile: channel %q: unknown type %q (must be webhook, slack, discord, or smtp)", c.Name, c.Type)
			}
			cfg.Channels = append(cfg.Channels, ChannelSpec{
				Name:   c.Name,
				Type:   c.Type,
				Config: c.Config,
			})
		}

		// --- notification rules ---
		// Build the set of channel names declared in this config so a rule
		// referencing an unknown channel is caught early. (Reconcile also
		// resolves against the store, which may carry channels from a prior sync.)
		declared := make(map[string]bool, len(cfg.Channels))
		for _, c := range cfg.Channels {
			declared[c.Name] = true
		}
		for i, r := range root.Notifications.Rules {
			if r.Channel == "" {
				return Config{}, fmt.Errorf("configfile: rule[%d]: channel must not be empty", i)
			}
			if !declared[r.Channel] {
				return Config{}, fmt.Errorf("configfile: rule[%d]: references unknown channel %q", i, r.Channel)
			}
			if len(r.Events) == 0 {
				return Config{}, fmt.Errorf("configfile: rule for channel %q: at least one event is required", r.Channel)
			}
			for _, ev := range r.Events {
				if strings.TrimSpace(ev) == "" {
					return Config{}, fmt.Errorf("configfile: rule for channel %q: event must not be empty", r.Channel)
				}
			}
			cfg.Rules = append(cfg.Rules, RuleSpec{
				Channel: r.Channel,
				Events:  append([]string(nil), r.Events...),
				Job:     r.Job,
			})
		}
	}

	// --- heartbeats ---
	for i, h := range root.Heartbeats {
		if h.Name == "" {
			return Config{}, fmt.Errorf("configfile: heartbeat[%d]: name must not be empty", i)
		}
		cfg.Heartbeats = append(cfg.Heartbeats, HeartbeatSpec{
			Name:          h.Name,
			PeriodSeconds: h.PeriodSeconds,
			GraceSeconds:  h.GraceSeconds,
		})
	}

	return cfg, nil
}

// mapSpec converts one jobSpec into a jobs.Job with full validation.
func mapSpec(idx int, spec jobSpec) (jobs.Job, error) {
	// --- name ---
	if spec.Name == "" {
		return jobs.Job{}, fmt.Errorf("configfile: job[%d]: name must not be empty", idx)
	}
	ref := fmt.Sprintf("job %q", spec.Name)

	// --- type ---
	var jt jobs.JobType
	switch spec.Type {
	case "shell":
		jt = jobs.Shell
		if spec.Command == "" {
			return jobs.Job{}, fmt.Errorf("configfile: %s: command is required for shell jobs", ref)
		}
	case "http":
		jt = jobs.HTTP
		if spec.HTTPURL == "" {
			return jobs.Job{}, fmt.Errorf("configfile: %s: http_url is required for http jobs", ref)
		}
	case "":
		return jobs.Job{}, fmt.Errorf("configfile: %s: type is required (shell or http)", ref)
	default:
		return jobs.Job{}, fmt.Errorf("configfile: %s: unknown type %q (must be shell or http)", ref, spec.Type)
	}

	// --- schedule: at most one (zero = a manual/no-schedule job) ---
	schedCount := 0
	if spec.Cron != "" {
		schedCount++
	}
	if spec.IntervalSeconds > 0 {
		schedCount++
	}
	if spec.RunAt != "" {
		schedCount++
	}
	if schedCount > 1 {
		return jobs.Job{}, fmt.Errorf("configfile: %s: only one schedule (cron, interval_seconds, or run_at) may be set, got %d", ref, schedCount)
	}

	// --- run_at ---
	var runAt time.Time
	if spec.RunAt != "" {
		var err error
		runAt, err = time.Parse(time.RFC3339, spec.RunAt)
		if err != nil {
			return jobs.Job{}, fmt.Errorf("configfile: %s: run_at %q is not RFC3339: %w", ref, spec.RunAt, err)
		}
	}

	// --- enabled default ---
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}

	j := jobs.Job{
		Name:            spec.Name,
		Type:            jt,
		Command:         spec.Command,
		HTTPURL:         spec.HTTPURL,
		HTTPMethod:      spec.HTTPMethod,
		HTTPBody:        spec.HTTPBody,
		Cron:            spec.Cron,
		IntervalSeconds: spec.IntervalSeconds,
		RunAt:           runAt,
		TimeoutSeconds:  spec.TimeoutSeconds,
		MaxRetries:      spec.MaxRetries,
		Enabled:         enabled,
		Env:             spec.Env,
	}
	return j, nil
}

// -------------------------------------------------------------------
// Reconcile
// -------------------------------------------------------------------

// ReconcileStore is the persistence surface needed by Reconcile across all
// config sections (jobs, channels, rules, heartbeats, secret resolution). The
// concrete store backends satisfy it; it is kept here (rather than importing the
// full store.Store) so the dependency stays explicit and testable.
type ReconcileStore interface {
	// jobs
	ListJobs(ctx context.Context, orgID int64) ([]jobs.Job, error)
	GetJobByName(ctx context.Context, orgID int64, name string) (jobs.Job, error)
	CreateJob(ctx context.Context, j jobs.Job) (int64, error)
	UpdateJob(ctx context.Context, j jobs.Job) error

	// channels (CreateChannel + GetChannel + DeleteChannel satisfy
	// notify.ChannelStore, which notify.CreateChannel requires)
	CreateChannel(ctx context.Context, ch notify.Channel, configBlob string) (int64, error)
	GetChannel(ctx context.Context, orgID, id int64) (notify.Channel, string, error)
	ListChannels(ctx context.Context, orgID int64) ([]notify.Channel, error)
	DeleteChannel(ctx context.Context, orgID, id int64) error

	// rules
	CreateRule(ctx context.Context, r notify.Rule) (int64, error)

	// heartbeats
	CreateHeartbeat(ctx context.Context, hb heartbeat.Heartbeat) (int64, string, error)

	// secrets (ciphertext out; decrypted via the box)
	GetSecret(ctx context.Context, orgID int64, name string) (string, error)
}

// ReconcileResult holds counts from a reconcile pass.
type ReconcileResult struct {
	Created  int // jobs created
	Updated  int // jobs updated
	Disabled int // jobs disabled (absent from config)

	DisabledNames []string // names of jobs disabled during this pass (populated only when prune=true)

	Channels   int // channels upserted
	Rules      int // notification_rules rows upserted (one per event)
	Heartbeats int // heartbeats upserted
}

// Reconcile applies cfg to the store in dependency order: jobs (so rules can
// resolve job: names), then channels (so rules can resolve channel names), then
// rules, then heartbeats. All sections are idempotent UPSERTS. Jobs additionally
// disable entries absent from the config (M1 behaviour, preserved exactly);
// channels/rules/heartbeats removed from config are NOT deleted in v1.
//
// Reconcile is NOT atomic across sections: on a partial failure it leaves
// earlier sections committed. Because every operation is an idempotent UPSERT
// applied in dependency order (jobs → channels → rules → heartbeats), a fixed
// re-run converges to the desired state with no orphaned rows.
//
// box may be nil ONLY when cfg declares no channels (channel config requires
// encryption + secret resolution). The clock is used for job NextRunAt.
func Reconcile(ctx context.Context, s ReconcileStore, orgID int64, clk clock.Clock, box *secrets.Box, cfg Config, prune bool) (ReconcileResult, error) {
	var result ReconcileResult

	// --- jobs (M1 behaviour, unchanged) ---
	if err := reconcileJobs(ctx, s, orgID, clk, cfg.Jobs, &result, prune); err != nil {
		return ReconcileResult{}, err
	}

	// --- channels ---
	if len(cfg.Channels) > 0 && box == nil {
		return ReconcileResult{}, errors.New("reconcile: channels declared but no master key configured (TEND_MASTER_KEY); cannot encrypt channel config")
	}
	for _, c := range cfg.Channels {
		if err := reconcileChannel(ctx, s, orgID, box, c); err != nil {
			return ReconcileResult{}, err
		}
		result.Channels++
	}

	// --- rules (after channels so names resolve to ids) ---
	if len(cfg.Rules) > 0 {
		n, err := reconcileRules(ctx, s, orgID, cfg.Rules)
		if err != nil {
			return ReconcileResult{}, err
		}
		result.Rules += n
	}

	// --- heartbeats ---
	for _, h := range cfg.Heartbeats {
		token, err := heartbeat.NewToken()
		if err != nil {
			return ReconcileResult{}, fmt.Errorf("reconcile: heartbeat %q: generate token: %w", h.Name, err)
		}
		if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{
			OrgID:         orgID,
			Name:          h.Name,
			Token:         token,
			PeriodSeconds: h.PeriodSeconds,
			GraceSeconds:  h.GraceSeconds,
		}); err != nil {
			return ReconcileResult{}, fmt.Errorf("reconcile: heartbeat %q: %w", h.Name, err)
		}
		result.Heartbeats++
	}

	return result, nil
}

// reconcileJobs performs the M1 upsert-by-name pass:
//   - Jobs in cfg but not in the store → created (with initial NextRunAt).
//   - Jobs in both → updated (ID preserved; NextRunAt recomputed only when the
//     schedule changes or NextRunAt is zero).
//   - Jobs in the store but not in cfg → disabled (Enabled=false).
//
// Secret refs in Env are kept verbatim.
func reconcileJobs(ctx context.Context, s ReconcileStore, orgID int64, clk clock.Clock, parsed []jobs.Job, result *ReconcileResult, prune bool) error {
	now := clk.Now()

	parsedByName := make(map[string]jobs.Job, len(parsed))
	for _, j := range parsed {
		parsedByName[j.Name] = j
	}

	existingJobs, err := s.ListJobs(ctx, orgID)
	if err != nil {
		return fmt.Errorf("reconcile: list jobs: %w", err)
	}

	for _, pj := range parsed {
		pj.OrgID = orgID

		existing, lookupErr := s.GetJobByName(ctx, orgID, pj.Name)
		if lookupErr != nil {
			if !errors.Is(lookupErr, store.ErrNotFound) {
				return fmt.Errorf("reconcile: get job %q: %w", pj.Name, lookupErr)
			}
			// Not found → create.
			// SCHEDULING CONTRACT: compute initial NextRunAt before Create so the
			// runner's DueJobs query can pick up this job immediately.
			pj.NextRunAt = mustNextRun(pj, now)
			if _, err := s.CreateJob(ctx, pj); err != nil {
				return fmt.Errorf("reconcile: create job %q: %w", pj.Name, err)
			}
			result.Created++
			continue
		}

		// Found → update.
		scheduleChanged := existing.Cron != pj.Cron ||
			existing.IntervalSeconds != pj.IntervalSeconds ||
			!existing.RunAt.Equal(pj.RunAt)

		pj.ID = existing.ID
		pj.OrgID = existing.OrgID
		pj.CreatedAt = existing.CreatedAt

		// NOTE: existing.NextRunAt.IsZero() can mean "never initialized", a
		// legitimately terminal state for an elapsed one-off job (whose
		// NextRunAt is zeroed after firing), OR a manual/no-schedule job (always
		// zero). For the first two we want to recompute so a re-synced one-off
		// gets rescheduled rather than silently dropped; for a manual job
		// mustNextRun returns zero again, so the recompute is a harmless no-op.
		// Do NOT remove this condition assuming it's dead code.
		if scheduleChanged || existing.NextRunAt.IsZero() {
			pj.NextRunAt = mustNextRun(pj, now)
		} else {
			pj.NextRunAt = existing.NextRunAt
		}

		if err := s.UpdateJob(ctx, pj); err != nil {
			return fmt.Errorf("reconcile: update job %q: %w", pj.Name, err)
		}
		result.Updated++
	}

	// --- Disable existing jobs not present in the config ---
	// Skipped entirely when prune=false (jobs absent from config are left untouched).
	if prune {
		for _, ej := range existingJobs {
			if _, present := parsedByName[ej.Name]; present {
				continue
			}
			if !ej.Enabled {
				continue // already disabled - no-op
			}
			ej.Enabled = false
			if err := s.UpdateJob(ctx, ej); err != nil {
				return fmt.Errorf("reconcile: disable job %q: %w", ej.Name, err)
			}
			result.Disabled++
			result.DisabledNames = append(result.DisabledNames, ej.Name)
		}
	}

	return nil
}

// reconcileChannel resolves {{ secret.* }} placeholders in the channel config's
// top-level string values, marshals the resolved config to JSON, and upserts the
// channel (encrypting the config via box). A missing referenced secret yields a
// clear error naming the secret.
func reconcileChannel(ctx context.Context, s ReconcileStore, orgID int64, box *secrets.Box, c ChannelSpec) error {
	resolved := make(map[string]any, len(c.Config))
	for k, v := range c.Config {
		// Only top-level string values may carry {{ secret.* }} refs; non-string
		// values pass through unchanged.
		if str, ok := v.(string); ok {
			out, err := resolveSecretRefs(ctx, s, orgID, box, str)
			if err != nil {
				return fmt.Errorf("reconcile: channel %q: %w", c.Name, err)
			}
			resolved[k] = out
			continue
		}
		resolved[k] = v
	}

	configJSON, err := json.Marshal(resolved)
	if err != nil {
		return fmt.Errorf("reconcile: channel %q: marshal config: %w", c.Name, err)
	}

	if _, err := notify.CreateChannel(ctx, s, box, notify.Channel{
		OrgID: orgID,
		Name:  c.Name,
		Kind:  notify.ChannelType(c.Type),
	}, configJSON); err != nil {
		return fmt.Errorf("reconcile: channel %q: %w", c.Name, err)
	}
	return nil
}

// reconcileRules expands each RuleSpec to one notification_rules row per event,
// resolving the channel name to an id (from the store, populated after channel
// reconcile) and the optional job: name to a job id (0 = all jobs). Returns the
// number of rule rows upserted.
func reconcileRules(ctx context.Context, s ReconcileStore, orgID int64, rules []RuleSpec) (int, error) {
	channels, err := s.ListChannels(ctx, orgID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list channels: %w", err)
	}
	byName := make(map[string]int64, len(channels))
	for _, ch := range channels {
		byName[ch.Name] = ch.ID
	}

	count := 0
	for _, r := range rules {
		channelID, ok := byName[r.Channel]
		if !ok {
			return 0, fmt.Errorf("reconcile: rule references unknown channel %q", r.Channel)
		}

		var jobID int64
		if r.Job != "" {
			j, err := s.GetJobByName(ctx, orgID, r.Job)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return 0, fmt.Errorf("reconcile: rule for channel %q references unknown job %q", r.Channel, r.Job)
				}
				return 0, fmt.Errorf("reconcile: rule for channel %q: get job %q: %w", r.Channel, r.Job, err)
			}
			jobID = j.ID
		}

		for _, ev := range r.Events {
			if _, err := s.CreateRule(ctx, notify.Rule{
				OrgID:     orgID,
				ChannelID: channelID,
				EventType: ev,
				Enabled:   true,
				JobID:     jobID,
			}); err != nil {
				return 0, fmt.Errorf("reconcile: rule channel %q event %q: %w", r.Channel, ev, err)
			}
			count++
		}
	}
	return count, nil
}

// resolveSecretRefs replaces every {{ secret.NAME }} placeholder in s with the
// decrypted secret value. Whitespace inside the braces is tolerated. A missing
// secret yields a clear error naming it. Strings without placeholders pass
// through unchanged.
func resolveSecretRefs(ctx context.Context, st ReconcileStore, orgID int64, box *secrets.Box, s string) (string, error) {
	const openTok, closeTok = "{{", "}}"
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, openTok)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(openTok):]
		j := strings.Index(rest, closeTok)
		if j < 0 {
			// Unterminated placeholder - emit literally and stop.
			b.WriteString(openTok)
			b.WriteString(rest)
			break
		}
		inner := strings.TrimSpace(rest[:j])
		rest = rest[j+len(closeTok):]

		name, ok := strings.CutPrefix(inner, "secret.")
		if !ok {
			// Not a secret ref - leave the original token intact.
			b.WriteString(openTok)
			b.WriteString(inner)
			b.WriteString(closeTok)
			continue
		}
		name = strings.TrimSpace(name)
		ct, err := st.GetSecret(ctx, orgID, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", fmt.Errorf("secret %q not found", name)
			}
			return "", fmt.Errorf("get secret %q: %w", name, err)
		}
		plain, err := box.Decrypt(ct)
		if err != nil {
			return "", fmt.Errorf("decrypt secret %q: %w", name, err)
		}
		b.WriteString(string(plain))
	}
	return b.String(), nil
}

// mustNextRun returns j.NextRun(now), or the zero time on error.
func mustNextRun(j jobs.Job, now time.Time) time.Time {
	next, err := j.NextRun(now)
	if err != nil {
		return time.Time{}
	}
	return next
}
