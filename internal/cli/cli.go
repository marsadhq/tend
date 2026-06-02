// Package cli implements the tend command-line interface. Each subcommand is
// a function that accepts explicit dependencies and io.Writer sinks so that
// tests can drive commands without spawning a real process or using os.Exit.
package cli

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/config"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// Version is set by cmd/tend/main.go via the Version variable.
var Version = "dev"

// Run opens a store from cfg, bootstraps the default org, and dispatches
// to the subcommand indicated by args[0]. It returns an error that main
// should print and map to a non-zero exit code. args must have at least
// one element; if empty, usage is printed and nil is returned (caller
// prints nothing extra).
func Run(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return nil
	}

	cmd := args[0]

	// version is handled before opening the store.
	if cmd == "version" {
		fmt.Fprintf(stdout, "tend %s\n", Version)
		return nil
	}

	// Open store.
	st, err := store.Open(cfg.Driver, cfg.DSN)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap org: %w", err)
	}

	// Build secrets box (may be nil when no master key configured).
	var box *secrets.Box
	if cfg.MasterKey != "" {
		b, err := secrets.NewBox(cfg.MasterKey)
		if err != nil {
			return fmt.Errorf("init secrets box: %w", err)
		}
		box = b
	}

	switch cmd {
	case "serve":
		return cmdServe(ctx, st, box, cfg.MasterKey, stdout)

	case "sync":
		return cmdSync(ctx, st, box, org.ID, args[1:], stdout, stderr)

	case "job":
		return cmdJob(ctx, st, org.ID, args[1:], stdin, stdout, stderr)

	case "run":
		return cmdRun(ctx, st, box, org.ID, args[1:], stdout, stderr)

	case "logs":
		return cmdLogs(ctx, st, org.ID, args[1:], stdout, stderr)

	case "secret":
		return cmdSecret(ctx, st, box, org.ID, args[1:], stdin, stdout, stderr)

	case "channel":
		return cmdChannel(ctx, st, box, org.ID, args[1:], stdin, stdout, stderr)

	case "rule":
		return cmdRule(ctx, st, org.ID, args[1:], stdout, stderr)

	case "heartbeat":
		return cmdHeartbeat(ctx, st, org.ID, args[1:], stdout, stderr)

	case "user":
		return cmdUser(ctx, st, org.ID, args[1:], stdin, stdout, stderr)

	case "token":
		return cmdToken(ctx, st, org.ID, args[1:], stdout, stderr)

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// -------------------------------------------------------------------
// serve
// -------------------------------------------------------------------

// watcherInterval is how often the heartbeat watcher scans for missed pings.
const watcherInterval = 30 * time.Second

// serveShutdownTimeout bounds the HTTP server's graceful drain on ctx cancel.
const serveShutdownTimeout = 5 * time.Second

// sessionKeyInfo is the HKDF "info" label that domain-separates the session
// signing key from any other key derived from the same master key. Bumping the
// version suffix would rotate (and thus invalidate) all existing sessions.
const sessionKeyInfo = "tend-session-v1"

// sessionKeyLen is the length (bytes) of the derived HMAC-SHA256 session key.
const sessionKeyLen = 32

// deriveSessionKey derives the session-cookie signing key from the base64-encoded
// master key via HKDF-SHA256 with a fixed info label and NO salt. It is
// DETERMINISTIC for a given master key so a server restart does not invalidate
// outstanding sessions. The master key and the derived key are never logged.
func deriveSessionKey(masterKeyB64 string) ([]byte, error) {
	mk, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	// nil salt → deterministic derivation (no random salt) so restarts keep
	// existing session cookies valid.
	return hkdf.Key(sha256.New, mk, nil, sessionKeyInfo, sessionKeyLen)
}

// buildAuthConfig returns the httpserver.AuthConfig that mounts the authenticated
// dashboard/API surface, or nil when no master key is configured (public-only
// mode). It is the single shared seam that BOTH cmdServe and the test handler
// builder use, so the auth-mounting decision can never diverge between them.
//
// Cookie Secure flag: serve speaks plain HTTP by default (TLS is normally
// terminated at a reverse proxy), so Secure defaults to false - a true default
// would silently break the session cookie over plain HTTP. Deployments that
// terminate TLS directly at tend can opt in with TEND_COOKIE_SECURE=1/true.
func buildAuthConfig(masterKeyB64 string) (*httpserver.AuthConfig, error) {
	if masterKeyB64 == "" {
		return nil, nil
	}
	key, err := deriveSessionKey(masterKeyB64)
	if err != nil {
		return nil, err
	}
	return &httpserver.AuthConfig{
		Codec:  auth.NewSessionCodec(key),
		Secure: cookieSecure(),
	}, nil
}

// cookieSecure reports whether the session cookie should be flagged Secure
// (HTTPS-only). It defaults to false (plain HTTP behind a TLS-terminating
// proxy) and is opted into via TEND_COOKIE_SECURE.
func cookieSecure() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEND_COOKIE_SECURE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// buildServeHandler constructs the exact HTTP handler cmdServe serves, opening a
// store from the env-driven config and mounting the authenticated surface via the
// shared buildAuthConfig seam. It is used by tests to exercise serve's HTTP
// surface (both auth modes) without starting serve's long-lived goroutines; it
// does not change serve's runtime behavior.
func buildServeHandler(masterKeyB64 string) (http.Handler, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	authCfg, err := buildAuthConfig(masterKeyB64)
	if err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpserver.New(st, clock.RealClock{}, nil, logger, authCfg).Handler(), nil
}

// cmdServe runs the three long-lived components - the job runner, the HTTP
// server (heartbeat ping + healthz), and the heartbeat watcher - concurrently
// off ONE clock and ONE dispatcher. All three watch ctx and stop on cancel;
// cmdServe returns nil after every component has stopped (clean shutdown).
//
// A fatal HTTP error (e.g. EADDRINUSE on startup) tears down all components via
// a child context (serveCtx / cancelAll), so the process exits promptly rather
// than hanging until a SIGINT arrives. A clean parent-ctx cancel (SIGINT/SIGTERM)
// propagates identically; serveErr stays nil in that case.
func cmdServe(ctx context.Context, st store.Store, box *secrets.Box, masterKeyB64 string, stdout io.Writer) error {
	clk := clock.RealClock{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// authCfg mounts the authenticated dashboard/API surface when a master key is
	// present; nil leaves the server public-only (M2 behavior). The derived
	// session key is never logged.
	authCfg, err := buildAuthConfig(masterKeyB64)
	if err != nil {
		return fmt.Errorf("serve: build auth config: %w", err)
	}

	// One dispatcher shared by the runner, the HTTP server, and the watcher.
	// Only meaningful when a master key is configured - channel configs are
	// encrypted, so without the box the dispatcher cannot decrypt them. dispatch
	// stays nil otherwise; all three components are nil-safe on dispatch (they
	// still record/emit events).
	var dispatch func(context.Context, core.Event)
	if box != nil {
		disp := notify.NewDispatcher(st, box, notify.BuildProvider, logger)
		dispatch = disp.DispatchForEvent
	}

	addr := os.Getenv("TEND_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	alertsOn := "off (no master key)"
	if dispatch != nil {
		alertsOn = "on"
	}
	dashboardOn := "off (no master key)"
	if authCfg != nil {
		dashboardOn = "on"
	}
	fmt.Fprintf(stdout, "tend serve: runner + http(%s) + heartbeat watcher running; alerts %s; dashboard %s (Ctrl-C to stop)\n", addr, alertsOn, dashboardOn)

	// serveCtx is cancelled either by the parent (SIGINT/SIGTERM) or by the HTTP
	// goroutine on a fatal bind/listen error so that all components tear down
	// promptly instead of hanging until the parent context is cancelled.
	serveCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	var wg sync.WaitGroup
	var serveErr error // captured from a non-clean HTTP server exit; safe: written
	// before wg.Done() in the http goroutine, read after wg.Wait() (happens-before).

	// --- runner ---
	// v1 trade-off: runner.EventSink (DispatchForEvent) runs synchronously on the
	// worker after each terminal event. Its retry backoff is ctx-aware, so a
	// slow/failing channel never hangs shutdown.
	runner := jobs.NewRunner(st, jobs.NewExecutor(), box, clk)
	runner.EventSink = dispatch
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Start blocks until serveCtx is cancelled; its own error is logged (a
		// clean cancel returns nil).
		if err := runner.Start(serveCtx); err != nil {
			logger.Error("runner stopped with error", "err", err)
		}
	}()

	// --- HTTP server ---
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpserver.New(st, clk, dispatch, logger, authCfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped with error", "err", err)
			serveErr = err
			// Cancel the shared context so the runner, watcher, and shutdown
			// goroutine all stop immediately rather than waiting for a SIGINT.
			cancelAll()
		}
	}()
	// Shutdown goroutine: on serveCtx cancel, drain the server with a bounded timeout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-serveCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("http server shutdown", "err", err)
		}
	}()

	// --- heartbeat watcher ---
	watcher := heartbeat.NewWatcher(st, clk, dispatch)
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(watcherInterval)
		defer t.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-t.C:
				if err := watcher.Check(serveCtx); err != nil {
					logger.Error("heartbeat watcher check", "err", err)
				}
			}
		}
	}()

	wg.Wait()
	return serveErr
}

// -------------------------------------------------------------------
// sync
// -------------------------------------------------------------------

func cmdSync(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prune := fs.Bool("prune", true, "disable jobs that are present in the store but absent from the config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("sync: file argument required (usage: tend sync <file>)")
	}

	filePath := fs.Arg(0)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("sync: read %q: %w", filePath, err)
	}

	cfg, err := configfile.Parse(data)
	if err != nil {
		return fmt.Errorf("sync: parse: %w", err)
	}

	result, err := configfile.Reconcile(ctx, st, orgID, clock.RealClock{}, box, cfg, *prune)
	if err != nil {
		return fmt.Errorf("sync: reconcile: %w", err)
	}

	fmt.Fprintf(stdout, "sync: jobs(created=%d updated=%d disabled=%d) channels=%d rules=%d heartbeats=%d\n",
		result.Created, result.Updated, result.Disabled, result.Channels, result.Rules, result.Heartbeats)
	if len(result.DisabledNames) > 0 {
		fmt.Fprintf(stdout, "disabled (absent from config): %s\n", strings.Join(result.DisabledNames, ", "))
	}
	return nil
}

// -------------------------------------------------------------------
// job
// -------------------------------------------------------------------

func cmdJob(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("job: subcommand required (usage: tend job <list|add|enable|disable|rm> [flags])")
	}
	switch args[0] {
	case "list":
		return cmdJobList(ctx, st, orgID, args[1:], stdout, stderr)
	case "add":
		return cmdJobAdd(ctx, st, orgID, args[1:], stdout, stderr)
	case "enable":
		return cmdJobEnable(ctx, st, orgID, args[1:], stdout, stderr)
	case "disable":
		return cmdJobDisable(ctx, st, orgID, args[1:], stdout, stderr)
	case "rm":
		return cmdJobRm(ctx, st, orgID, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("job: unknown subcommand %q", args[0])
	}
}

// cmdJobRm implements `job rm <name>`: resolve the job by name then DeleteJob it,
// which transactionally removes the job, its runs, and its job-scoped rules.
func cmdJobRm(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("job rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("job rm: job name required (usage: tend job rm <job-name>)")
	}

	name := fs.Arg(0)
	j, err := st.GetJobByName(ctx, orgID, name)
	if err != nil {
		return fmt.Errorf("job rm: get job %q: %w", name, err)
	}
	if err := st.DeleteJob(ctx, orgID, j.ID); err != nil {
		return fmt.Errorf("job rm: delete job %q: %w", name, err)
	}

	fmt.Fprintf(stdout, "job %q removed\n", name)
	return nil
}

func cmdJobEnable(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	return cmdJobToggle(ctx, st, orgID, "enable", true, args, stdout, stderr)
}

func cmdJobDisable(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	return cmdJobToggle(ctx, st, orgID, "disable", false, args, stdout, stderr)
}

// cmdJobToggle is the shared implementation for `job enable` and `job disable`.
// It flips j.Enabled and calls UpdateJob, leaving NextRunAt untouched (matching
// reconcileJobs, which only recomputes NextRunAt when the schedule changes or
// NextRunAt is zero).
func cmdJobToggle(ctx context.Context, st store.Store, orgID int64, sub string, enabled bool, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("job "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("job %s: job name required (usage: tend job %s <job-name>)", sub, sub)
	}

	name := fs.Arg(0)
	j, err := st.GetJobByName(ctx, orgID, name)
	if err != nil {
		return fmt.Errorf("job %s: get job %q: %w", sub, name, err)
	}

	j.Enabled = enabled
	if err := st.UpdateJob(ctx, j); err != nil {
		return fmt.Errorf("job %s: update job %q: %w", sub, name, err)
	}

	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	fmt.Fprintf(stdout, "job %q %s\n", name, state)
	return nil
}

func cmdJobList(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	jbs, err := st.ListJobs(ctx, orgID)
	if err != nil {
		return fmt.Errorf("job list: %w", err)
	}

	if len(jbs) == 0 {
		fmt.Fprintln(stdout, "(no jobs)")
		return nil
	}

	// Table header.
	fmt.Fprintf(stdout, "%-20s  %-6s  %-22s  %-7s  %s\n",
		"NAME", "TYPE", "SCHEDULE", "ENABLED", "NEXT_RUN")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 80))

	for _, j := range jbs {
		sched := scheduleStr(j)
		enabled := "yes"
		if !j.Enabled {
			enabled = "no"
		}
		nextRun := "-"
		if !j.NextRunAt.IsZero() {
			nextRun = j.NextRunAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "%-20s  %-6s  %-22s  %-7s  %s\n",
			j.Name, string(j.Type), sched, enabled, nextRun)
	}
	return nil
}

func scheduleStr(j jobs.Job) string {
	switch {
	case j.Cron != "":
		return "cron:" + j.Cron
	case j.IntervalSeconds > 0:
		return fmt.Sprintf("interval:%ds", j.IntervalSeconds)
	case !j.RunAt.IsZero():
		return "run_at:" + j.RunAt.UTC().Format(time.RFC3339)
	default:
		return "(none)"
	}
}

// envFlag is a repeatable flag.Value that accumulates KEY=VALUE pairs into a
// map[string]string. The value is split on the FIRST '=' so that values may
// themselves contain '='. An entry without '=' causes Set to return an error.
type envFlag struct {
	m map[string]string
}

func (e *envFlag) String() string {
	if e.m == nil {
		return ""
	}
	parts := make([]string, 0, len(e.m))
	for k, v := range e.m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

func (e *envFlag) Set(s string) error {
	// Cut on the first '=' so values may themselves contain '='. (strings.Cut
	// is the idiom already used elsewhere in this codebase.)
	key, val, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("env entry %q must have the form KEY=VALUE", s)
	}
	if key == "" {
		return fmt.Errorf("env entry %q has an empty key", s)
	}
	if e.m == nil {
		e.m = make(map[string]string)
	}
	// Repeated -env with the same KEY is last-write-wins (standard env semantics).
	e.m[key] = val
	return nil
}

func cmdJobAdd(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("job add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	name := fs.String("name", "", "job name (required)")
	typ := fs.String("type", "shell", "job type: shell or http")
	command := fs.String("command", "", "shell command")
	cronExpr := fs.String("cron", "", "cron expression")
	interval := fs.Int("interval", 0, "interval in seconds")
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	maxRetries := fs.Int("max-retries", 0, "max retries")
	httpURL := fs.String("url", "", "HTTP URL (http jobs)")
	httpMethod := fs.String("method", "GET", "HTTP method (http jobs)")
	httpBody := fs.String("body", "", "HTTP request body (http jobs)")
	var envF envFlag
	fs.Var(&envF, "env", "set an env var KEY=VALUE (repeatable)")
	runAtStr := fs.String("run-at", "", "one-off run time (RFC3339, e.g. 2026-06-01T02:00:00Z)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return fmt.Errorf("job add: -name is required")
	}
	// At most one schedule; zero = a manual/no-schedule job.
	schedCount := 0
	if *cronExpr != "" {
		schedCount++
	}
	if *interval != 0 {
		schedCount++
	}
	if *runAtStr != "" {
		schedCount++
	}
	if schedCount > 1 {
		return fmt.Errorf("job add: only one of -cron, -interval, or -run-at may be set")
	}

	var runAt time.Time
	if *runAtStr != "" {
		t, err := time.Parse(time.RFC3339, *runAtStr)
		if err != nil {
			return fmt.Errorf("job add: run_at %q is not RFC3339: %w", *runAtStr, err)
		}
		runAt = t
	}

	var jt jobs.JobType
	switch *typ {
	case "shell":
		jt = jobs.Shell
		if *command == "" {
			return fmt.Errorf("job add: -command is required for shell jobs")
		}
	case "http":
		jt = jobs.HTTP
		if *httpURL == "" {
			return fmt.Errorf("job add: -url is required for http jobs")
		}
	default:
		return fmt.Errorf("job add: unknown type %q", *typ)
	}

	now := time.Now()
	j := jobs.Job{
		OrgID:           orgID,
		Name:            *name,
		Type:            jt,
		Command:         *command,
		HTTPURL:         *httpURL,
		HTTPMethod:      *httpMethod,
		Cron:            *cronExpr,
		IntervalSeconds: *interval,
		RunAt:           runAt,
		TimeoutSeconds:  *timeout,
		MaxRetries:      *maxRetries,
		Enabled:         true,
		// Env and HTTPBody are set unconditionally (no type gating), mirroring
		// configfile which also sets both fields on any job type. The fields are
		// type-specific in effect only: Env is consumed by shell jobs; HTTPBody is
		// consumed by http jobs. Setting either on the "wrong" type is a no-op.
		Env:      envF.m,
		HTTPBody: *httpBody,
	}

	// Compute initial NextRunAt (scheduling contract).
	if next, err := j.NextRun(now); err == nil {
		j.NextRunAt = next
	}

	id, err := st.CreateJob(ctx, j)
	if err != nil {
		return fmt.Errorf("job add: create: %w", err)
	}
	fmt.Fprintf(stdout, "job created id=%d\n", id)
	return nil
}

// -------------------------------------------------------------------
// run
// -------------------------------------------------------------------

func cmdRun(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("run: job name required (usage: tend run <job-name>)")
	}

	name := fs.Arg(0)
	j, err := st.GetJobByName(ctx, orgID, name)
	if err != nil {
		return fmt.Errorf("run: get job %q: %w", name, err)
	}

	if _, err := st.EnqueueRun(ctx, orgID, j.ID); err != nil {
		return fmt.Errorf("run: enqueue: %w", err)
	}

	// Execute all pending runs inline (no-overlap: ClaimRun is exclusive).
	runner := jobs.NewRunner(st, jobs.NewExecutor(), box, clock.RealClock{})
	if err := runner.DrainOnce(ctx); err != nil {
		return fmt.Errorf("run: drain: %w", err)
	}

	// Show the latest run's result.
	runs, err := st.ListRuns(ctx, orgID, j.ID, 1)
	if err != nil {
		return fmt.Errorf("run: list runs: %w", err)
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "run: no runs found")
		return nil
	}
	r := runs[0]
	fmt.Fprintf(stdout, "run: status=%s exit_code=%d\n", r.Status, r.ExitCode)
	if r.Output != "" {
		fmt.Fprintf(stdout, "output:\n%s\n", r.Output)
	}
	return nil
}

// -------------------------------------------------------------------
// logs
// -------------------------------------------------------------------

func cmdLogs(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", false, "poll for new runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("logs: job name required (usage: tend logs <job-name> [--follow])")
	}

	name := fs.Arg(0)
	j, err := st.GetJobByName(ctx, orgID, name)
	if err != nil {
		return fmt.Errorf("logs: get job %q: %w", name, err)
	}

	printRuns := func(limit int) error {
		runs, err := st.ListRuns(ctx, orgID, j.ID, limit)
		if err != nil {
			return fmt.Errorf("logs: list runs: %w", err)
		}
		for _, r := range runs {
			fmt.Fprintf(stdout, "[%s] status=%-9s exit_code=%-4d %s\n",
				r.StartedAt.UTC().Format(time.RFC3339),
				r.Status, r.ExitCode, r.Output)
		}
		return nil
	}

	if !*follow {
		return printRuns(20)
	}

	// --follow: poll every 2s, printing new runs (by ID) until ctx cancelled.
	var lastID int64
	for {
		runs, err := st.ListRuns(ctx, orgID, j.ID, 50)
		if err != nil {
			return fmt.Errorf("logs: list runs: %w", err)
		}
		// Runs come back newest-first; iterate in reverse to print oldest-first.
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			if r.ID > lastID {
				fmt.Fprintf(stdout, "[%s] status=%-9s exit_code=%-4d %s\n",
					r.StartedAt.UTC().Format(time.RFC3339),
					r.Status, r.ExitCode, r.Output)
				lastID = r.ID
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// -------------------------------------------------------------------
// secret
// -------------------------------------------------------------------

func cmdSecret(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("secret: subcommand required (usage: tend secret set <key>)")
	}
	switch args[0] {
	case "set":
		return cmdSecretSet(ctx, st, box, orgID, args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("secret: unknown subcommand %q", args[0])
	}
}

func cmdSecretSet(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("secret set: key required (usage: tend secret set <key>)")
	}
	if box == nil {
		return fmt.Errorf("secret set: TEND_MASTER_KEY is not set; cannot encrypt secrets")
	}

	key := fs.Arg(0)

	// Read secret value from stdin (never from argv to avoid process-list leakage).
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("secret set: read stdin: %w", err)
	}
	// Trim a single trailing newline (common when piping via `echo`).
	value := strings.TrimRight(string(raw), "\n")

	ciphertext, err := box.Encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("secret set: encrypt: %w", err)
	}

	if err := st.PutSecret(ctx, orgID, key, ciphertext); err != nil {
		return fmt.Errorf("secret set: store: %w", err)
	}

	fmt.Fprintf(stdout, "secret %q stored (never echoed)\n", key)
	return nil
}

// -------------------------------------------------------------------
// channel
// -------------------------------------------------------------------

func cmdChannel(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("channel: subcommand required (usage: tend channel <add|list> [flags])")
	}
	switch args[0] {
	case "add":
		return cmdChannelAdd(ctx, st, box, orgID, args[1:], stdin, stdout, stderr)
	case "list":
		return cmdChannelList(ctx, st, orgID, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("channel: unknown subcommand %q", args[0])
	}
}

func cmdChannelAdd(ctx context.Context, st store.Store, box *secrets.Box, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("channel add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "channel name (required)")
	typ := fs.String("type", "", "channel type: webhook, slack, discord, or smtp (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("channel add: -name is required")
	}
	if *typ == "" {
		return fmt.Errorf("channel add: -type is required (webhook, slack, discord, or smtp)")
	}
	switch notify.ChannelType(*typ) {
	case notify.Webhook, notify.Slack, notify.Discord, notify.SMTP:
	default:
		return fmt.Errorf("channel add: unknown type %q (must be webhook, slack, discord, or smtp)", *typ)
	}
	if box == nil {
		return fmt.Errorf("channel add: TEND_MASTER_KEY is not set; cannot encrypt channel config")
	}

	// Config JSON is read from STDIN (never argv) so credentials never appear in
	// the process list.
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("channel add: read stdin: %w", err)
	}
	cfg := []byte(strings.TrimSpace(string(raw)))
	if len(cfg) == 0 {
		return fmt.Errorf("channel add: config JSON required on stdin")
	}

	id, err := notify.CreateChannel(ctx, st, box, notify.Channel{
		OrgID: orgID,
		Name:  *name,
		Kind:  notify.ChannelType(*typ),
	}, cfg)
	if err != nil {
		return fmt.Errorf("channel add: %w", err)
	}
	fmt.Fprintf(stdout, "channel %q (%s) saved id=%d\n", *name, *typ, id)
	return nil
}

func cmdChannelList(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("channel list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	channels, err := st.ListChannels(ctx, orgID)
	if err != nil {
		return fmt.Errorf("channel list: %w", err)
	}
	if len(channels) == 0 {
		fmt.Fprintln(stdout, "(no channels)")
		return nil
	}
	fmt.Fprintf(stdout, "%-24s  %-8s  %s\n", "NAME", "TYPE", "ID")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 44))
	for _, c := range channels {
		fmt.Fprintf(stdout, "%-24s  %-8s  %d\n", c.Name, string(c.Kind), c.ID)
	}
	return nil
}

// -------------------------------------------------------------------
// rule
// -------------------------------------------------------------------

func cmdRule(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("rule: subcommand required (usage: tend rule <add|list> [flags])")
	}
	switch args[0] {
	case "add":
		return cmdRuleAdd(ctx, st, orgID, args[1:], stdout, stderr)
	case "list":
		return cmdRuleList(ctx, st, orgID, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("rule: unknown subcommand %q", args[0])
	}
}

func cmdRuleAdd(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rule add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	channelName := fs.String("channel", "", "channel name to route to (required)")
	event := fs.String("event", "", "event type, e.g. run.failed (required)")
	jobName := fs.String("job", "", "job name to scope to (optional; omit for all jobs)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channelName == "" {
		return fmt.Errorf("rule add: -channel is required")
	}
	if *event == "" {
		return fmt.Errorf("rule add: -event is required")
	}

	// Resolve channel name -> id.
	channels, err := st.ListChannels(ctx, orgID)
	if err != nil {
		return fmt.Errorf("rule add: list channels: %w", err)
	}
	var channelID int64
	for _, c := range channels {
		if c.Name == *channelName {
			channelID = c.ID
			break
		}
	}
	if channelID == 0 {
		return fmt.Errorf("rule add: unknown channel %q", *channelName)
	}

	// Resolve optional job name -> id (0 = all jobs).
	var jobID int64
	if *jobName != "" {
		j, err := st.GetJobByName(ctx, orgID, *jobName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("rule add: unknown job %q", *jobName)
			}
			return fmt.Errorf("rule add: get job %q: %w", *jobName, err)
		}
		jobID = j.ID
	}

	id, err := st.CreateRule(ctx, notify.Rule{
		OrgID:     orgID,
		ChannelID: channelID,
		EventType: *event,
		Enabled:   true,
		JobID:     jobID,
	})
	if err != nil {
		return fmt.Errorf("rule add: %w", err)
	}
	scope := "all jobs"
	if jobID != 0 {
		scope = fmt.Sprintf("job %q", *jobName)
	}
	fmt.Fprintf(stdout, "rule saved id=%d: %s -> %q (%s)\n", id, *event, *channelName, scope)
	return nil
}

func cmdRuleList(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rule list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rules, err := st.ListRules(ctx, orgID)
	if err != nil {
		return fmt.Errorf("rule list: %w", err)
	}
	if len(rules) == 0 {
		fmt.Fprintln(stdout, "(no rules)")
		return nil
	}

	// Build a channel id -> name map for readable output.
	channels, err := st.ListChannels(ctx, orgID)
	if err != nil {
		return fmt.Errorf("rule list: list channels: %w", err)
	}
	nameByID := make(map[int64]string, len(channels))
	for _, c := range channels {
		nameByID[c.ID] = c.Name
	}

	fmt.Fprintf(stdout, "%-24s  %-20s  %-10s  %s\n", "CHANNEL", "EVENT", "JOB_ID", "ENABLED")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 70))
	for _, r := range rules {
		chName := nameByID[r.ChannelID]
		if chName == "" {
			chName = fmt.Sprintf("#%d", r.ChannelID)
		}
		jobCol := "all"
		if r.JobID != 0 {
			jobCol = fmt.Sprintf("%d", r.JobID)
		}
		enabled := "yes"
		if !r.Enabled {
			enabled = "no"
		}
		fmt.Fprintf(stdout, "%-24s  %-20s  %-10s  %s\n", chName, r.EventType, jobCol, enabled)
	}
	return nil
}

// -------------------------------------------------------------------
// heartbeat
// -------------------------------------------------------------------

// baseURL returns the externally-reachable base for ping URLs, from
// TEND_BASE_URL or the localhost default.
func baseURL() string {
	if b := os.Getenv("TEND_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "http://localhost:8080"
}

func cmdHeartbeat(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("heartbeat: subcommand required (usage: tend heartbeat <add|list> [flags])")
	}
	switch args[0] {
	case "add":
		return cmdHeartbeatAdd(ctx, st, orgID, args[1:], stdout, stderr)
	case "list":
		return cmdHeartbeatList(ctx, st, orgID, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("heartbeat: unknown subcommand %q", args[0])
	}
}

func cmdHeartbeatAdd(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("heartbeat add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "heartbeat name (required)")
	period := fs.Int("period", 0, "expected period between pings, in seconds (required)")
	grace := fs.Int("grace", 0, "grace period before a missed ping alerts, in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("heartbeat add: -name is required")
	}
	if *period <= 0 {
		return fmt.Errorf("heartbeat add: -period must be > 0 seconds")
	}

	token, err := heartbeat.NewToken()
	if err != nil {
		return fmt.Errorf("heartbeat add: generate token: %w", err)
	}
	_, effToken, err := st.CreateHeartbeat(ctx, heartbeat.Heartbeat{
		OrgID:         orgID,
		Name:          *name,
		Token:         token,
		PeriodSeconds: *period,
		GraceSeconds:  *grace,
	})
	if err != nil {
		return fmt.Errorf("heartbeat add: %w", err)
	}
	// The effective token (existing on a re-add, new on first add) is the user's
	// to paste into their external job - printing it here is expected.
	fmt.Fprintf(stdout, "heartbeat %q saved; ping URL: %s/ping/%s\n", *name, baseURL(), effToken)
	return nil
}

func cmdHeartbeatList(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("heartbeat list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	hbs, err := st.ListHeartbeats(ctx, orgID)
	if err != nil {
		return fmt.Errorf("heartbeat list: %w", err)
	}
	if len(hbs) == 0 {
		fmt.Fprintln(stdout, "(no heartbeats)")
		return nil
	}
	fmt.Fprintf(stdout, "%-24s  %-6s  %-8s  %-7s  %s\n", "NAME", "STATUS", "PERIOD", "GRACE", "PING_PATH")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 72))
	for _, h := range hbs {
		fmt.Fprintf(stdout, "%-24s  %-6s  %-8d  %-7d  /ping/%s\n",
			h.Name, h.Status, h.PeriodSeconds, h.GraceSeconds, h.Token)
	}
	return nil
}

// -------------------------------------------------------------------
// user
// -------------------------------------------------------------------

func cmdUser(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("user: subcommand required (usage: tend user add [flags])")
	}
	switch args[0] {
	case "add":
		return cmdUserAdd(ctx, st, orgID, args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("user: unknown subcommand %q", args[0])
	}
}

func cmdUserAdd(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "user email (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("user add: -email is required")
	}

	// The password is read from STDIN (never a flag/argv) so it never appears in
	// the process list or shell history - same as `secret set`.
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("user add: read stdin: %w", err)
	}
	// Trim a single trailing newline (common when piping via `echo`).
	pw := strings.TrimRight(string(raw), "\n")
	if pw == "" {
		return fmt.Errorf("user add: password required on stdin")
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		return fmt.Errorf("user add: hash password: %w", err)
	}

	userID, err := st.CreateUser(ctx, auth.User{
		OrgID:        orgID,
		Email:        *email,
		PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("user add: create user: %w", err)
	}

	if _, err := st.CreateMembership(ctx, auth.Membership{
		OrgID:  orgID,
		UserID: userID,
		Role:   "admin",
	}); err != nil {
		return fmt.Errorf("user add: create membership: %w", err)
	}

	// Success line carries neither the password nor its hash.
	fmt.Fprintf(stdout, "user %q created id=%d (role: admin)\n", *email, userID)
	return nil
}

// -------------------------------------------------------------------
// token
// -------------------------------------------------------------------

func cmdToken(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("token: subcommand required (usage: tend token <create|list|revoke> [flags])")
	}
	switch args[0] {
	case "create":
		return cmdTokenCreate(ctx, st, orgID, args[1:], stdout, stderr)
	case "list":
		return cmdTokenList(ctx, st, orgID, args[1:], stdout, stderr)
	case "revoke":
		return cmdTokenRevoke(ctx, st, orgID, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("token: unknown subcommand %q", args[0])
	}
}

func cmdTokenCreate(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "token name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("token create: -name is required")
	}

	plaintext, err := auth.GenerateToken()
	if err != nil {
		return fmt.Errorf("token create: generate: %w", err)
	}

	// Persist ONLY the hash; the plaintext is never stored.
	id, err := st.CreateToken(ctx, auth.APIToken{
		OrgID:     orgID,
		Name:      *name,
		TokenHash: auth.HashToken(plaintext),
	})
	if err != nil {
		return fmt.Errorf("token create: %w", err)
	}

	// Print the plaintext token EXACTLY ONCE. It cannot be recovered later.
	fmt.Fprintf(stdout, "token %q created id=%d\n", *name, id)
	fmt.Fprintf(stdout, "  %s\n", plaintext)
	fmt.Fprintln(stdout, "store this now - it won't be shown again")
	return nil
}

func cmdTokenList(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	tokens, err := st.ListTokens(ctx, orgID)
	if err != nil {
		return fmt.Errorf("token list: %w", err)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(stdout, "(no tokens)")
		return nil
	}
	// The hash is never selected by ListTokens and is never printed here.
	fmt.Fprintf(stdout, "%-8s  %-24s  %s\n", "ID", "NAME", "CREATED")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 56))
	for _, tk := range tokens {
		created := "-"
		if !tk.CreatedAt.IsZero() {
			created = tk.CreatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "%-8d  %-24s  %s\n", tk.ID, tk.Name, created)
	}
	return nil
}

func cmdTokenRevoke(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Int64("id", 0, "token id to revoke (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 {
		return fmt.Errorf("token revoke: -id is required")
	}
	if err := st.DeleteToken(ctx, orgID, *id); err != nil {
		return fmt.Errorf("token revoke: %w", err)
	}
	fmt.Fprintf(stdout, "token id=%d revoked\n", *id)
	return nil
}

// -------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `tend - job runner

Usage:
  tend <command> [flags]

Commands:
  serve               start the runner + HTTP server + heartbeat watcher
  sync <file>         reconcile jobs/channels/rules/heartbeats from a YAML config
  job list            list jobs
  job add [flags]     create a new job
  job enable <name>   enable a job
  job disable <name>  disable a job
  job rm <name>       delete a job (and its runs + job-scoped rules)
  run <name>          run a job immediately
  logs <name>         show run logs
  secret set <key>    store a secret (value read from stdin)
  channel add [flags] create/update a notification channel (config from stdin)
  channel list        list notification channels
  rule add [flags]    create/update a notification rule
  rule list           list notification rules
  heartbeat add ...   create/update a heartbeat (prints the ping URL)
  heartbeat list      list heartbeats
  user add [flags]    create a user (admin) - password read from stdin
  token create [flags]  create an API token (printed once)
  token list          list API tokens (never shows the hash)
  token revoke -id N  revoke an API token by id
  version             print version`)
}
