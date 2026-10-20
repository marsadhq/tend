// Package cli implements the tend command-line interface. Each subcommand is
// a function that accepts explicit dependencies and io.Writer sinks so that
// tests can drive commands without spawning a real process or using os.Exit.
package cli

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kitauth "github.com/marsadhq/kit/auth"
	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/httpx"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/config"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
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

	// Help must be side-effect free and must not depend on configuration, so
	// the two requests below are answered BEFORE the TEND_DB check and before
	// opening the store. (Every other subcommand parses its own flags once the
	// store is open, so its -h still needs TEND_DB.)
	//
	// Top-level help: the command list is what a new user asks for first, with
	// nothing configured yet.
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		printUsage(stdout)
		return nil
	}
	// `tend exec` wraps a command on any host and talks to a ping URL only;
	// it never opens a database.
	if cmd == "exec" {
		return cmdExec(ctx, args[1:], stdin, stdout, stderr)
	}
	// `tend serve -h` works with no environment set and neither creates the
	// DSN file nor starts the daemon.
	if cmd == "serve" && wantsHelp(args[1:]) {
		printServeUsage(stdout)
		return nil
	}

	// Refuse to invent a database. The old "./tend.db" relative default
	// silently created or read a different DB per working directory, which
	// bit operators repeatedly; every DB-touching command now requires an
	// explicit TEND_DB.
	if cfg.DSN == "" {
		return errors.New("TEND_DB is not set; refusing to use default ./tend.db (set TEND_DB to an absolute SQLite path or a postgres:// DSN)")
	}

	// serve's flags are read, and its master-key requirement enforced, before
	// the store is opened: a server that refuses to start must not create or
	// migrate a database on its way out.
	var serveOpts serveOptions
	if cmd == "serve" {
		opts, err := parseServeFlags(args[1:], stderr)
		if err != nil {
			return err
		}
		if cfg.MasterKey == "" && !opts.noMasterKey {
			return errNoMasterKey
		}
		if opts.retention, err = config.LoadRetention(); err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		if opts.deliveryHold, err = config.LoadDeliveryHold(); err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		serveOpts = opts
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
			return fmt.Errorf("TEND_MASTER_KEY is not a valid key (it must be 32 random bytes, base64-encoded: head -c 32 /dev/urandom | base64): %w", err)
		}
		box = b
	}

	switch cmd {
	case "serve":
		return cmdServe(ctx, st, box, cfg, org, serveOpts, stdout)

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
		return cmdHeartbeat(ctx, st, org.ID, args[1:], stdin, stdout, stderr)

	case "doctor":
		return cmdDoctor(ctx, st, cfg, org, args[1:], stdout, stderr)

	case "user":
		return cmdUser(ctx, st, org.ID, args[1:], stdin, stdout, stderr)

	case "token":
		return cmdToken(ctx, st, org.ID, args[1:], stdout, stderr)

	case "import":
		return cmdImport(ctx, st, org.ID, args[1:], stdin, stdout, stderr)

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// -------------------------------------------------------------------
// serve
// -------------------------------------------------------------------

// errNoMasterKey is serve's refusal to start without TEND_MASTER_KEY. Without
// the key nothing can be alerted on (channel configs are encrypted with it) and
// there is no dashboard, and a typo in the variable name used to produce
// exactly that silently. --no-master-key is the explicit opt-out.
var errNoMasterKey = errors.New("serve: refusing to start without TEND_MASTER_KEY: without it no alert can be sent and the dashboard is off " +
	"(set it to a stable key from `head -c 32 /dev/urandom | base64`, or pass --no-master-key to run a local experiment without alerts or dashboard)")

// serveOptions are serve's command-line flags plus the settings Run reads
// for it from the environment before the store is opened.
type serveOptions struct {
	// noMasterKey lets serve start without TEND_MASTER_KEY, with alerts and
	// the dashboard off. A key that is set is used regardless.
	noMasterKey bool
	// retention is the history the daily sweep keeps (TEND_RETENTION_*).
	retention config.Retention
	// deliveryHold bounds how long a failing notification holds back its
	// channel (TEND_DELIVERY_HOLD_MINUTES).
	deliveryHold time.Duration
}

// parseServeFlags parses serve's flags.
func parseServeFlags(args []string, stderr io.Writer) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts serveOptions
	fs.BoolVar(&opts.noMasterKey, "no-master-key", false,
		"start without TEND_MASTER_KEY: no alerts and no dashboard (local experiments only)")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if fs.NArg() > 0 {
		return serveOptions{}, fmt.Errorf("serve: unexpected argument %q (serve is configured through the environment; see tend serve -h)", fs.Arg(0))
	}
	return opts, nil
}

// watcherInterval is how often the heartbeat watcher scans for missed pings.
const watcherInterval = 30 * time.Second

// retentionSweepInterval is how often serve's retention sweep prunes old rows
// from the audit tables (events, job_runs, finalized deliveries, heartbeat
// pings), which otherwise grow without bound. The sweep also runs once at startup.
const retentionSweepInterval = 24 * time.Hour

// sweeper runs serve's retention sweep. It remembers whether it has already
// said that the database is too large to compact while serving, so that is
// logged once per process, not on every daily sweep.
type sweeper struct {
	st             store.Store
	ret            config.Retention
	logger         *slog.Logger
	warnedTooLarge bool
}

// run prunes the history older than the retention windows (a zero window
// keeps that history forever) and then, when the prune deleted anything, gives
// the freed space back to the file system if there is enough of it (see
// store.ReclaimSpace). Errors are logged, never fatal: a failed sweep just
// retries on the next tick.
func (sw *sweeper) run(ctx context.Context, now time.Time) {
	st, ret, logger := sw.st, sw.ret, sw.logger
	var deleted int64
	prune := func(what string, fn func(context.Context, time.Time) (int64, error), window time.Duration) {
		if window <= 0 {
			return // kept forever
		}
		n, err := fn(ctx, now.Add(-window))
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("retention sweep: prune "+what, "err", err)
		case n > 0:
			logger.Info("retention sweep: pruned "+what, "rows", n)
			deleted += n
		}
	}
	prune("events", st.PruneEvents, ret.Events)
	prune("job runs", st.PruneJobRuns, ret.JobRuns)
	prune("deliveries", st.PruneDeliveries, ret.Deliveries)
	prune("heartbeat pings", st.PruneHeartbeatPings, ret.Pings)
	if deleted == 0 || ctx.Err() != nil {
		return
	}
	reclaimed, err := st.ReclaimSpace(ctx)
	switch {
	case errors.Is(err, store.ErrCompactTooLarge):
		if !sw.warnedTooLarge {
			sw.warnedTooLarge = true
			logger.Warn("retention sweep: space not reclaimed (said once per process)", "err", err)
		}
	case err != nil && ctx.Err() == nil:
		logger.Error("retention sweep: reclaim space", "err", err)
	case reclaimed > 0:
		logger.Info("retention sweep: reclaimed disk space", "bytes", reclaimed)
	}
}

// serveShutdownTimeout bounds the HTTP server's graceful drain on ctx cancel.
const serveShutdownTimeout = 5 * time.Second

// httpTimeout bounds each phase of an HTTP connection (reading the headers,
// reading the whole request including its body, writing the response, and
// idling between requests); see httpx.NewServer. All handlers are quick and
// the largest request body is a ping's (up to 100 kB is read), so one short
// limit fits them all.
const httpTimeout = 10 * time.Second

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
		Codec:  kitauth.NewSessionCodec(key),
		Secure: cookieSecure(),
	}, nil
}

// cookieSecure reports whether the session cookie should be flagged Secure
// (HTTPS-only). It defaults to false (plain HTTP behind a TLS-terminating
// proxy) and is opted into via TEND_COOKIE_SECURE.
func cookieSecure() bool {
	return httpx.EnvFlag("TEND_COOKIE_SECURE")
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

// cmdServe runs serve's long-lived components concurrently off ONE clock: the
// job runner (scheduler, workers and stale-run reaper), the HTTP server, the
// heartbeat watcher, the retention sweep and, when a master key is configured,
// the notification delivery worker. All of them watch ctx and stop on cancel;
// cmdServe returns nil after every component has stopped (clean shutdown).
//
// A fatal HTTP error (e.g. EADDRINUSE on startup) tears down all components via
// a child context (serveCtx / cancelAll), so the process exits promptly rather
// than hanging until a SIGINT arrives. A clean parent-ctx cancel (SIGINT/SIGTERM)
// propagates identically; serveErr stays nil in that case.
func cmdServe(ctx context.Context, st store.Store, box *secrets.Box, cfg config.Config, org core.Org, opts serveOptions, stdout io.Writer) error {
	clk := clock.RealClock{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// authCfg mounts the authenticated dashboard/API surface when a master key is
	// present; nil leaves the server public-only, which Run allows only under
	// --no-master-key. The derived session key is never logged.
	authCfg, err := buildAuthConfig(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("serve: build auth config: %w", err)
	}

	// One durable delivery worker shared by the runner, the HTTP server, and
	// the watcher. Deliveries are enqueued by the STORE, transactionally with
	// each event insert, so the dispatch seam handed to the components below
	// only nudges the worker for a prompt drain - it never sends inline and can
	// never block a runner worker, a ping response, or the watcher (F3).
	// Only meaningful when a master key is configured - channel configs are
	// encrypted, so without the box the worker cannot decrypt them. dispatch
	// stays nil otherwise; all three components are nil-safe on dispatch (they
	// still record/emit events, and the enqueued deliveries are drained once
	// serve runs with a key).
	var dispatch func(context.Context, core.Event)
	var worker *notify.Worker
	if box != nil {
		worker = notify.NewWorker(st, box, kitnotify.BuildProvider, config.LoadInstance(), logger)
		worker.Hold = opts.deliveryHold
		dispatch = func(context.Context, core.Event) { worker.Nudge() }
	}

	addr := os.Getenv("TEND_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Reaching here without a box means --no-master-key was passed (Run
	// refuses otherwise); say so loudly, since nothing will be alerted on.
	alertsOn := "OFF (--no-master-key: nothing will be alerted on)"
	if dispatch != nil {
		alertsOn = "on"
	}
	dashboardOn := "OFF (--no-master-key)"
	if authCfg != nil {
		dashboardOn = "on"
	}
	fmt.Fprintf(stdout, "tend serve: runner + http(%s) + heartbeat watcher running; alerts %s; dashboard %s; db %s(%s) org %q; retention %s (Ctrl-C to stop)\n",
		addr, alertsOn, dashboardOn, redactDSN(cfg.Driver, cfg.DSN), cfg.Driver, org.Name, opts.retention)

	// serveCtx is cancelled either by the parent (SIGINT/SIGTERM) or by the HTTP
	// goroutine on a fatal bind/listen error so that all components tear down
	// promptly instead of hanging until the parent context is cancelled.
	serveCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	var wg sync.WaitGroup
	var serveErr error // captured from a non-clean HTTP server exit; safe: written
	// before wg.Done() in the http goroutine, read after wg.Wait() (happens-before).

	// --- delivery worker ---
	// Drains the durable deliveries queue with capped exponential backoff. All
	// senders (runner, ping handler, watcher) only enqueue + nudge.
	if worker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker.Run(serveCtx)
		}()
	}

	// --- runner ---
	// runner.EventSink only nudges the delivery worker (the delivery itself was
	// already enqueued atomically by FinishRunAndEmit), so a slow/failing
	// channel never blocks a runner worker or shutdown.
	runner := jobs.NewRunner(st, jobs.NewExecutor(), box, clk)
	runner.EventSink = dispatch
	runner.Logger = logger
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
	srv := httpx.NewServer(addr, httpserver.New(st, clk, dispatch, logger, authCfg).Handler(), httpTimeout)
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
	watcher := heartbeat.NewWatcher(st, clk, dispatch, logger)
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

	// --- retention sweep ---
	// Prunes old events/job_runs/deliveries once at startup and then daily so
	// the audit tables stay bounded, then reclaims the space (sweeper.run).
	wg.Add(1)
	go func() {
		defer wg.Done()
		sw := &sweeper{st: st, ret: opts.retention, logger: logger}
		sweep := func() { sw.run(serveCtx, clk.Now()) }
		sweep()
		t := time.NewTicker(retentionSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-t.C:
				sweep()
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
	timeout := fs.Int("timeout", 0, "timeout per attempt in seconds (0 = the default, 30 minutes)")
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
	if *timeout < 0 {
		return fmt.Errorf("job add: -timeout must be 0 (the default, 30 minutes) or a positive number of seconds, got %d", *timeout)
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
	if *cronExpr != "" {
		if err := jobs.ValidateCron(*cronExpr); err != nil {
			return fmt.Errorf("job add: %w", err)
		}
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

	// Execute all pending runs inline (no-overlap: ClaimRun is exclusive). The
	// runner logs to stderr, like the delivery worker below: a result that could
	// not be recorded, or was dropped because the run had already been failed,
	// would otherwise show up only as a run status that contradicts the job.
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	runner := jobs.NewRunner(st, jobs.NewExecutor(), box, clock.RealClock{})
	runner.Logger = logger
	// Claimed under this process's own name, so a serve that starts while the
	// run executes does not requeue it and run the job a second time.
	runner.Claimant = jobs.ManualClaimant()
	if err := runner.DrainOnce(ctx); err != nil {
		return fmt.Errorf("run: drain: %w", err)
	}

	// F4: a manual run must notify like a scheduled one. The terminal event
	// enqueued its deliveries transactionally (FinishRunAndEmit); drain them
	// inline so the notification goes out even when no serve process is
	// running. A destination that fails here stays queued (pending with
	// backoff) and is picked up by a running serve worker - claim atomicity
	// makes the overlap safe. Without a master key the channel configs cannot
	// be decrypted, so warn rather than silently dropping the alert.
	if box != nil {
		w := notify.NewWorker(st, box, kitnotify.BuildProvider, config.LoadInstance(), logger)
		if hold, err := config.LoadDeliveryHold(); err != nil {
			fmt.Fprintf(stderr, "run: warning: %v; using the default\n", err)
		} else {
			w.Hold = hold
		}
		if _, err := w.DrainOnce(ctx); err != nil {
			fmt.Fprintf(stderr, "run: warning: notification delivery: %v\n", err)
		}
	} else {
		fmt.Fprintln(stderr, "run: warning: no master key configured; notifications for this run cannot be delivered now (they stay queued for a serve process with a key)")
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
	typ := fs.String("type", "", "channel type: "+notify.ChannelTypeList()+" (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("channel add: -name is required")
	}
	if *typ == "" {
		return fmt.Errorf("channel add: -type is required (%s)", notify.ChannelTypeList())
	}
	if !notify.ValidChannelType(*typ) {
		return fmt.Errorf("channel add: unknown type %q (must be %s)", *typ, notify.ChannelTypeList())
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
		Kind:  kitnotify.ChannelType(*typ),
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
	event := fs.String("event", "", "event type (required): "+strings.Join(notify.AlertableTypes(), ", "))
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
	if err := notify.ValidateRuleEvent(*event); err != nil {
		return fmt.Errorf("rule add: %w", err)
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
		if !notify.Alertable(r.EventType) {
			// A rule stored before event types were checked; it never fires.
			enabled += " (never fires: not an alertable event)"
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

// redactDSN produces a safe-to-print DSN for diagnostics: the absolute file path
// for SQLite (so the exact database file is unambiguous) and a userinfo-stripped
// URL for Postgres (so credentials never reach stdout or logs).
func redactDSN(driver, dsn string) string {
	if driver == "postgres" {
		if u, err := url.Parse(dsn); err == nil && u.Host != "" {
			u.User = nil
			return u.String()
		}
		return "postgres://<unparseable>"
	}
	if abs, err := filepath.Abs(dsn); err == nil {
		return abs
	}
	return dsn
}

// cmdDoctor prints the resolved configuration and resource counts. It is the
// diagnostic for the most common operational confusion: the CLI and the running
// server reading different databases (TEND_DB defaults to a RELATIVE tend.db,
// while the Docker image runs with /data/tend.db). It reuses the store + org the
// caller already resolved via the standard config.Load + store.Open path.
func cmdDoctor(ctx context.Context, st store.Store, cfg config.Config, org core.Org, args []string, stdout, stderr io.Writer) error {
	jl, err := st.ListJobs(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("doctor: list jobs: %w", err)
	}
	cl, err := st.ListChannels(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("doctor: list channels: %w", err)
	}
	rl, err := st.ListRules(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("doctor: list rules: %w", err)
	}
	hl, err := st.ListHeartbeats(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("doctor: list heartbeats: %w", err)
	}
	masterKey := "unset (serve refuses to start without it unless --no-master-key; no alerts, dashboard or secrets)"
	if cfg.MasterKey != "" {
		masterKey = "set"
	}
	fmt.Fprintf(stdout, "driver:     %s\n", cfg.Driver)
	fmt.Fprintf(stdout, "db:         %s\n", redactDSN(cfg.Driver, cfg.DSN))
	fmt.Fprintf(stdout, "org:        %d %q\n", org.ID, org.Name)
	fmt.Fprintf(stdout, "base url:   %s\n", baseURL())
	fmt.Fprintf(stdout, "master key: %s\n", masterKey)
	if ret, err := config.LoadRetention(); err != nil {
		fmt.Fprintf(stdout, "retention:  invalid, serve will refuse to start: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "retention:  %s\n", ret)
	}
	if hold, err := config.LoadDeliveryHold(); err != nil {
		fmt.Fprintf(stdout, "delivery hold: invalid, serve will refuse to start: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "delivery hold: %s per channel\n", hold)
	}
	fmt.Fprintf(stdout, "counts:     jobs=%d channels=%d rules=%d heartbeats=%d\n", len(jl), len(cl), len(rl), len(hl))
	return nil
}

func cmdHeartbeat(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("heartbeat: subcommand required (usage: tend heartbeat <add|list|show|ping-url|ping-key|history|rm> [flags])")
	}
	switch args[0] {
	case "add":
		return cmdHeartbeatAdd(ctx, st, orgID, args[1:], stdout, stderr)
	case "list":
		return cmdHeartbeatList(ctx, st, orgID, args[1:], stdout, stderr)
	case "show":
		return cmdHeartbeatShow(ctx, st, orgID, args[1:], stdout, stderr)
	case "ping-url":
		return cmdHeartbeatPingURL(ctx, st, orgID, args[1:], stdout, stderr)
	case "history":
		return cmdHeartbeatHistory(ctx, st, orgID, args[1:], stdout, stderr)
	case "rm":
		return cmdHeartbeatRemove(ctx, st, orgID, args[1:], stdout, stderr)
	case "ping-key":
		return cmdHeartbeatPingKey(ctx, st, orgID, args[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("heartbeat: unknown subcommand %q", args[0])
	}
}

// cmdHeartbeatPingURL prints the recoverable ping URL for a heartbeat by name.
// The token is a credential, but the CLI is the trusted local surface (the same
// operator can already read the DB), so surfacing it here is by design.
func cmdHeartbeatPingURL(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("heartbeat ping-url: usage: tend heartbeat ping-url <name>")
	}
	hb, err := st.GetHeartbeatByName(ctx, orgID, args[0])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("heartbeat ping-url: no heartbeat named %q", args[0])
		}
		return fmt.Errorf("heartbeat ping-url: %w", err)
	}
	fmt.Fprintf(stdout, "%s/ping/%s\n", baseURL(), hb.Token)
	return nil
}

// cmdHeartbeatPingKey shows or changes the org's ping key, which turns on the
// slug form of the ping API, /ping/<ping-key>/<heartbeat-name> (as in
// Healthchecks). -new generates a key, -set reads one from stdin (to keep a
// Healthchecks project's slug URLs working), -clear turns the slug form off.
// Like a ping token the key is a credential, printed here on the trusted
// local surface only.
func cmdHeartbeatPingKey(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("heartbeat ping-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gen := fs.Bool("new", false, "generate a new ping key (the old one, if any, stops working)")
	set := fs.Bool("set", false, "read the ping key from stdin, e.g. a Healthchecks project's")
	clearKey := fs.Bool("clear", false, "remove the ping key: slug ping URLs stop working")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("heartbeat ping-key: unexpected argument %q (usage: tend heartbeat ping-key [-new | -set | -clear])", fs.Arg(0))
	}
	n := 0
	for _, b := range []bool{*gen, *set, *clearKey} {
		if b {
			n++
		}
	}
	if n > 1 {
		return fmt.Errorf("heartbeat ping-key: pass at most one of -new, -set and -clear")
	}
	var key string
	switch {
	case *gen:
		var err error
		if key, err = newPingKey(); err != nil {
			return fmt.Errorf("heartbeat ping-key: %w", err)
		}
	case *set:
		raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return fmt.Errorf("heartbeat ping-key: read stdin: %w", err)
		}
		key = strings.TrimSpace(string(raw))
		if !validPingKey(key) {
			return fmt.Errorf("heartbeat ping-key: a ping key is 1 to 128 letters, digits, '-' or '_' (read from stdin)")
		}
	}
	if *gen || *set || *clearKey {
		if err := st.SetOrgPingKey(ctx, orgID, key); err != nil {
			return fmt.Errorf("heartbeat ping-key: %w", err)
		}
		if *clearKey {
			fmt.Fprintln(stdout, "ping key removed; slug ping URLs no longer work")
			return nil
		}
	} else {
		var err error
		if key, err = st.GetOrgPingKey(ctx, orgID); err != nil {
			return fmt.Errorf("heartbeat ping-key: %w", err)
		}
		if key == "" {
			fmt.Fprintln(stdout, "no ping key: slug ping URLs are off (create one with tend heartbeat ping-key -new, or adopt a Healthchecks project's with -set)")
			return nil
		}
	}
	fmt.Fprintf(stdout, "ping key: %s\n", key)
	fmt.Fprintf(stdout, "slug URLs: %s/ping/%s/<heartbeat-name>[/start|/fail|/log|/<exit-status>] (?create=1 creates a missing one)\n", baseURL(), key)
	return nil
}

// newPingKey returns a random ping key: 16 bytes, base64url without
// padding, 22 characters like a Healthchecks ping key.
func newPingKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate ping key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validPingKey reports whether key can be a ping key: one path segment of 1
// to 128 letters, digits, '-' and '_' (what Healthchecks' keys are made of).
func validPingKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// cmdHeartbeatShow prints a heartbeat's details, including the recoverable
// ping URL, the run being timed, the last ping and the last result (success or
// failure) with the tail of what it logged.
func cmdHeartbeatShow(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("heartbeat show: usage: tend heartbeat show <name>")
	}
	hb, err := st.GetHeartbeatByName(ctx, orgID, args[0])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("heartbeat show: no heartbeat named %q", args[0])
		}
		return fmt.Errorf("heartbeat show: %w", err)
	}
	pings, err := st.ListHeartbeatPings(ctx, orgID, hb.ID, showPingScan)
	if err != nil {
		return fmt.Errorf("heartbeat show: %w", err)
	}
	lastSeen := "never"
	if !hb.LastSeenAt.IsZero() {
		lastSeen = hb.LastSeenAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(stdout, "name:      %s\n", hb.Name)
	fmt.Fprintf(stdout, "status:    %s\n", hb.Status)
	fmt.Fprintf(stdout, "period:    %ds\n", hb.PeriodSeconds)
	fmt.Fprintf(stdout, "grace:     %ds\n", hb.GraceSeconds)
	fmt.Fprintf(stdout, "last seen: %s\n", lastSeen)
	if !hb.StartedAt.IsZero() {
		running := "started " + hb.StartedAt.UTC().Format(time.RFC3339)
		if hb.StartedRID != "" {
			running += " (rid " + hb.StartedRID + ")"
		}
		fmt.Fprintf(stdout, "running:   %s\n", running)
	}
	var result *heartbeat.Ping
	for i, p := range pings {
		if p.Kind == heartbeat.PingSuccess || p.Kind == heartbeat.PingFail {
			result = &pings[i]
			break
		}
	}
	if len(pings) > 0 {
		fmt.Fprintf(stdout, "last ping: %s at %s\n", pingSummary(pings[0]), pings[0].CreatedAt.UTC().Format(time.RFC3339))
	}
	if result != nil {
		line := pingSummary(*result) + " at " + result.CreatedAt.UTC().Format(time.RFC3339)
		if result.Duration != nil {
			line += ", took " + result.Duration.String()
		}
		fmt.Fprintf(stdout, "result:    %s\n", line)
	}
	fmt.Fprintf(stdout, "ping url:  %s/ping/%s\n", baseURL(), hb.Token)
	if key, err := st.GetOrgPingKey(ctx, orgID); err == nil && key != "" {
		fmt.Fprintf(stdout, "slug url:  %s/ping/%s/%s\n", baseURL(), key, url.PathEscape(hb.Name))
	}
	if result != nil {
		if tail := heartbeat.OutputTail(result.Body); tail != "" {
			fmt.Fprintln(stdout, "output:")
			for _, l := range strings.Split(tail, "\n") {
				fmt.Fprintf(stdout, "  %s\n", l)
			}
		}
	}
	return nil
}

// showPingScan is how many of the newest pings `heartbeat show` looks through
// for the last result.
const showPingScan = 50

// pingSummary is a ping's kind with its exit status, e.g. "fail (exit 3)".
func pingSummary(p heartbeat.Ping) string {
	if p.ExitStatus != nil {
		return fmt.Sprintf("%s (exit %d)", p.Kind, *p.ExitStatus)
	}
	return p.Kind
}

// cmdHeartbeatHistory prints a heartbeat's history, newest first: its pings
// (kind, exit status, run duration, rid, body size) interleaved with its
// transitions (missed, recovered, removed, with their alert subject). -body
// prints each ping's logged body under it. Usage: tend heartbeat history
// <name> [-limit N] [-body].
func cmdHeartbeatHistory(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("heartbeat history: usage: tend heartbeat history <name> [-limit N] [-body]")
	}
	name := args[0]
	fs := flag.NewFlagSet("heartbeat history", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 20, "maximum number of entries (pings and transitions) to show")
	withBody := fs.Bool("body", false, "print each ping's logged request body")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Resolve the name first so an unknown heartbeat is a clear error.
	hb, err := st.GetHeartbeatByName(ctx, orgID, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("heartbeat history: no heartbeat named %q", name)
		}
		return fmt.Errorf("heartbeat history: %w", err)
	}
	evs, err := st.ListHeartbeatEvents(ctx, orgID, name, *limit)
	if err != nil {
		return fmt.Errorf("heartbeat history: %w", err)
	}
	pings, err := st.ListHeartbeatPings(ctx, orgID, hb.ID, *limit)
	if err != nil {
		return fmt.Errorf("heartbeat history: %w", err)
	}
	if len(evs) == 0 && len(pings) == 0 {
		fmt.Fprintf(stdout, "(no pings or transitions recorded for %q)\n", name)
		return nil
	}
	// Merge newest first. A transition is listed above the ping that made
	// it, which carries the same time.
	for n := 0; n < *limit && (len(evs) > 0 || len(pings) > 0); n++ {
		if len(evs) > 0 && (len(pings) == 0 || !evs[0].CreatedAt.Before(pings[0].CreatedAt)) {
			e := evs[0]
			evs = evs[1:]
			subject, _ := notify.Render(e)
			fmt.Fprintf(stdout, "%s  %s  %s\n", e.CreatedAt.UTC().Format(time.RFC3339), e.Type, subject)
			continue
		}
		p := pings[0]
		pings = pings[1:]
		parts := []string{fmt.Sprintf("%-7s", p.Kind)}
		if p.ExitStatus != nil {
			parts = append(parts, fmt.Sprintf("exit %d", *p.ExitStatus))
		}
		if p.Duration != nil {
			parts = append(parts, "took "+p.Duration.String())
		}
		if rid := p.RID; rid != "" {
			if len(rid) > 8 {
				rid = rid[:8] // a UUID's first group tells runs apart at a glance
			}
			parts = append(parts, "rid "+rid)
		}
		if p.Body != "" {
			parts = append(parts, fmt.Sprintf("%d B", len(p.Body)))
		}
		fmt.Fprintf(stdout, "%s  %s\n", p.CreatedAt.UTC().Format(time.RFC3339), strings.TrimRight(strings.Join(parts, "  "), " "))
		if *withBody && p.Body != "" {
			for _, l := range strings.Split(strings.TrimRight(p.Body, "\n"), "\n") {
				fmt.Fprintf(stdout, "    %s\n", l)
			}
		}
	}
	return nil
}

// cmdHeartbeatRemove hard-deletes a heartbeat by name. Its past transition
// events are preserved as an audit trail.
func cmdHeartbeatRemove(ctx context.Context, st store.Store, orgID int64, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("heartbeat rm: usage: tend heartbeat rm <name>")
	}
	hb, err := st.GetHeartbeatByName(ctx, orgID, args[0])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("heartbeat rm: no heartbeat named %q", args[0])
		}
		return fmt.Errorf("heartbeat rm: %w", err)
	}
	if err := st.DeleteHeartbeat(ctx, orgID, hb.ID); err != nil {
		return fmt.Errorf("heartbeat rm: %w", err)
	}
	fmt.Fprintf(stdout, "heartbeat %q removed (past events are kept)\n", args[0])
	return nil
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

	hash, err := kitauth.HashPassword(pw)
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
		TokenHash: kitauth.HashToken(plaintext),
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
                      (needs TEND_MASTER_KEY unless --no-master-key)
  sync <file>         reconcile jobs/channels/rules/heartbeats from a YAML config
  job list            list jobs
  job add [flags]     create a new job
  job enable <name>   enable a job
  job disable <name>  disable a job
  job rm <name>       delete a job (and its runs + job-scoped rules)
  run <name>          run a job immediately
  exec [flags] -- <cmd>  run a command and report it to a ping URL (start,
                      exit status, output tail); no database needed
  logs <name>         show run logs
  secret set <key>    store a secret (value read from stdin)
  channel add [flags] create/update a notification channel (config from stdin)
  channel list        list notification channels
  rule add [flags]    create/update a notification rule
  rule list           list notification rules
  heartbeat add ...   create/update a heartbeat (prints the ping URL)
  heartbeat list      list heartbeats
  heartbeat show <n>  show a heartbeat: status, run, last result and output
  heartbeat ping-key [-new|-set|-clear]  the org's ping key for slug URLs
  heartbeat history <n> [-body]  its pings and transitions, newest first
  import healthchecks --api-key-file <f>  import a Healthchecks project's checks
                      as heartbeats, keeping their UUIDs as tokens (--dry-run)
  doctor              diagnose config/DB/connectivity
  user add [flags]    create a user (admin) - password read from stdin
  token create [flags]  create an API token (printed once)
  token list          list API tokens (never shows the hash)
  token revoke -id N  revoke an API token by id
  version             print version
  help                print this list (also -h, --help)`)
}

// wantsHelp reports whether args contains a help flag.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

// printServeUsage prints `tend serve`'s help text. It must stay accurate to
// the components cmdServe starts and to serve's actual knobs (its flags in
// parseServeFlags; TEND_DB and TEND_MASTER_KEY at config.Load, TEND_ADDR at
// cmdServe's addr resolution, TEND_COOKIE_SECURE at cookieSecure(),
// TEND_TRUST_PROXY in httpserver).
func printServeUsage(w io.Writer) {
	fmt.Fprintln(w, `tend serve - start the runner + HTTP server + heartbeat watcher

Usage:
  tend serve [--no-master-key]

Runs these components until interrupted (Ctrl-C):
  - the job runner (executes due jobs; fails runs left running past their timeout)
  - the HTTP server: the heartbeat ping API at /ping/<token>, compatible
    with Healthchecks' (/start, /fail, /log, /<exit-status>, ?rid=, a POST
    body of up to 100 kB is logged; /ping/<ping-key>/<name> once the org
    has a ping key), /healthz, and the dashboard/API
  - the heartbeat watcher (flags missed dead-man's-switch pings, and started
    runs that did not finish within their grace)
  - the notification delivery worker
  - a daily retention sweep (prunes old events, job runs, deliveries and
    heartbeat pings, then gives the space back on SQLite)

serve refuses to start without TEND_MASTER_KEY: channel configs are encrypted
with it, so without it nothing can be alerted on, and the dashboard needs it
to sign sessions.

Flags:
  --no-master-key      start without TEND_MASTER_KEY anyway, with alerts and the
                       dashboard off (local experiments only)

Environment:
  TEND_DB              sqlite file path, or a postgres://... DSN (required)
  TEND_MASTER_KEY      base64 32-byte master key (required; see above)
  TEND_ADDR            listen address (default ":8080")
  TEND_COOKIE_SECURE   mark session cookie Secure (default off; set behind TLS)
  TEND_TRUST_PROXY     honor X-Forwarded-For for rate-limit keying (default off)
  TEND_RETENTION_EVENTS_DAYS      days of events to keep (default 30; 0 = forever)
  TEND_RETENTION_RUNS_DAYS        days of finished job runs to keep (default 90; 0 = forever)
  TEND_RETENTION_DELIVERIES_DAYS  days of finished deliveries to keep (default 7; 0 = forever)
  TEND_RETENTION_PINGS_DAYS       days of heartbeat ping logs to keep (default 30; 0 = forever;
                                  each heartbeat also keeps only its newest 1000 pings)
  TEND_DELIVERY_HOLD_MINUTES      how long a failing notification may hold back the
                                  later ones on its channel (default 15; 0 = no ordering)
  TEND_INSTANCE        this installation's name in webhook notifications
                       (default: the host name)

Related commands:
  tend doctor          diagnose config/DB/connectivity
  tend heartbeat add   create a heartbeat (prints the ping URL)
  tend heartbeat list  list heartbeats
  tend heartbeat ping-key  turn on slug ping URLs (/ping/<ping-key>/<name>)
  tend exec            wrap a command with start and exit-status pings
  tend import healthchecks  import a Healthchecks project's checks`)
}
