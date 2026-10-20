package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/marsadhq/tend/internal/pingexec"
)

// ExitError asks main to exit with Code and print nothing: what failed (a
// command `tend exec` wrapped) has already said so itself.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// envPingURL is the ping URL `tend exec` uses when --ping-url is not given.
const envPingURL = "TEND_PING_URL"

// cmdExec is `tend exec`: run a command between a start ping and a
// completion ping carrying its exit status and the tail of its output (see
// package pingexec). It needs no database, so it runs on any host that can
// reach the ping URL, Tend's or a Healthchecks-compatible one.
func cmdExec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The default comes from the environment after parsing, not as the
	// flag's default: the usage text prints defaults, and this one holds a
	// token.
	pingURL := fs.String("ping-url", "", "the check's ping URL, e.g. https://tend.example/ping/<token> or https://hc-ping.com/<uuid> (default $"+envPingURL+")")
	token := fs.String("token", "", "a Tend ping token; the URL is then <base-url>/ping/<token>")
	base := fs.String("base-url", "", "Tend's base URL for --token (default $TEND_BASE_URL)")
	noStart := fs.Bool("no-start", false, "do not send /start before running the command")
	bodyLimit := fs.Int("body-limit", pingexec.DefaultBodyLimit, "bytes of output tail to send with the completion ping (0: none)")
	timeout := fs.Duration("timeout", pingexec.DefaultTimeout, "timeout of each ping attempt")
	retries := fs.Int("retries", pingexec.DefaultRetries, "times a failed ping is retried (network errors, 5xx, 429)")
	fs.Usage = func() { printExecUsage(fs.Output(), fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("exec: a command to run is required (usage: tend exec --ping-url <url> [--] <command> [args...])")
	}

	target := *pingURL
	if target == "" && *token == "" {
		target = os.Getenv(envPingURL)
	}
	if *token != "" {
		if isFlagSet(fs, "ping-url") {
			return errors.New("exec: pass --ping-url or --token, not both")
		}
		b := *base
		if b == "" {
			b = os.Getenv("TEND_BASE_URL")
		}
		if b == "" {
			return errors.New("exec: --token needs Tend's base URL: set TEND_BASE_URL or pass --base-url")
		}
		target = strings.TrimRight(b, "/") + "/ping/" + url.PathEscape(*token)
	}
	if target == "" {
		return errors.New("exec: no ping URL: pass --ping-url <url> (or set " + envPingURL + "), or --token with TEND_BASE_URL")
	}
	if err := pingexec.CheckPingURL(target); err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	cfg := pingexec.Config{
		PingURL:   target,
		Start:     !*noStart,
		BodyLimit: *bodyLimit,
		Timeout:   *timeout,
		Retries:   *retries,
		UserAgent: "tend/" + Version,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		Log:       stderr,
	}
	if *bodyLimit == 0 {
		cfg.BodyLimit = -1
	}
	if *retries == 0 {
		cfg.Retries = -1
	}
	if *timeout <= 0 {
		cfg.Timeout = pingexec.DefaultTimeout
	}
	if code := pingexec.Run(ctx, cfg, fs.Arg(0), fs.Args()[1:]...); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// isFlagSet reports whether the flag called name was given on the command
// line (rather than taking its default, which may come from the environment).
func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func printExecUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, `tend exec - run a command and report it to a ping URL (like runitor)

Usage:
  tend exec --ping-url <url> [flags] [--] <command> [args...]
  tend exec --token <token> [flags] [--] <command> [args...]   (with TEND_BASE_URL)

Sends <url>/start, runs the command with its output passed through, then
sends <url>/<exit-status> with the tail of the output as the body, and exits
with the command's status. Every ping carries the same ?rid=, so concurrent
runs are told apart. A ping that fails is retried (network errors, 5xx, 429)
and then given up with a warning; the command runs regardless. Works with a
Tend server and with any Healthchecks-compatible one (https://hc-ping.com/<uuid>,
slug URLs too). Needs no database.

Flags:`)
	fs.PrintDefaults()
	fmt.Fprintln(w, `
Example (crontab):
  15 3 * * *  tend exec --ping-url https://tend.example/ping/<token> -- /usr/local/bin/backup.sh`)
}
