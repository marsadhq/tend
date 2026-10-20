// Package pingexec is `tend exec`: it wraps a command the way runitor does,
// so an existing cron job is monitored without being rewritten. It sends a
// start ping, runs the command with its output passed through and its tail
// kept, then sends the command's exit status as the completion ping with that
// tail as the body, and exits with the command's status.
//
// It speaks the Healthchecks ping API, so it works against a Tend server and
// against healthchecks.io or anything compatible: the ping URL is the
// check's base URL (https://tend.example/ping/<token>, https://hc-ping.com/
// <uuid>, or a slug URL) and the actions are appended to its path.
//
// Monitoring never gets in the way of the job: a ping that fails is retried
// (network errors, 5xx and 429, honouring Retry-After) and then given up with
// a warning, and the command runs and exits as it would have without it.
package pingexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/urlredact"
)

// Defaults for the zero values of Config.
const (
	DefaultBodyLimit = 10_000
	DefaultTimeout   = 10 * time.Second
	DefaultRetries   = 5
)

// Backoff between attempts of one ping: maxBackoff caps the doubling, and
// maxRetryAfter caps a server's Retry-After.
const (
	firstBackoff  = time.Second
	maxBackoff    = 30 * time.Second
	maxRetryAfter = time.Minute
)

// Config is one run of a wrapped command.
type Config struct {
	// PingURL is the check's ping URL, with no action: /start and the exit
	// status are appended to its path, and ?rid= to its query.
	PingURL string
	// Start sends /start before running the command, so the server times
	// the run and notices one that hangs.
	Start bool
	// BodyLimit is the most bytes of output sent with the completion ping:
	// its tail. 0 means DefaultBodyLimit, a negative value sends none. A
	// smaller Ping-Body-Limit announced by the server wins.
	BodyLimit int
	// Timeout bounds each ping attempt; 0 means DefaultTimeout.
	Timeout time.Duration
	// Retries is how many times a failed ping is tried again; 0 means
	// DefaultRetries, a negative value none.
	Retries int
	// Client sends the pings; nil means a plain http.Client (Timeout bounds
	// each attempt either way).
	Client *http.Client
	// UserAgent is sent with every ping.
	UserAgent string

	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Log receives the warnings about pings that could not be sent.
	Log io.Writer

	// Sleep waits between attempts; nil waits on a timer (tests replace it).
	Sleep func(context.Context, time.Duration)
}

// ErrBadPingURL is returned by CheckPingURL.
var ErrBadPingURL = errors.New("ping URL must be an absolute http:// or https:// URL without a rid")

// CheckPingURL reports whether u can be a ping URL: absolute, http or https,
// with a host, and without a ?rid= of its own (each run picks one).
func CheckPingURL(u string) error {
	p, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadPingURL, urlredact.Error(u, err))
	}
	if (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.Query().Has("rid") {
		return ErrBadPingURL
	}
	return nil
}

// Run runs name with args under cfg and returns its exit status: the
// command's own, 128 + the signal number when a signal killed it, 127 when
// it could not be found and 126 when it could not be started otherwise.
// Signals tend exec receives (interrupt, terminate, hang-up, quit) are passed
// on to the command, and the completion ping is sent even after one.
func Run(ctx context.Context, cfg Config, name string, args ...string) int {
	p := newPinger(cfg)
	// The pings must go out even when ctx is cancelled by the signal that
	// is also stopping the command: its result is what they report.
	pctx := context.WithoutCancel(ctx)
	if cfg.Start {
		if limit, ok := p.send(pctx, "start", nil); ok && limit > 0 && limit < p.bodyLimit {
			p.bodyLimit = limit
		}
	}

	tail := newTailBuffer(max(p.bodyLimit, 0))
	code := runCommand(cfg, tail, name, args)
	var body []byte
	if p.bodyLimit > 0 {
		body = tail.Bytes()
	}
	suffix := strconv.Itoa(code)
	if code < 0 || code > 255 {
		suffix = "fail" // only 0 to 255 can be reported as a status
	}
	p.send(pctx, suffix, body)
	return code
}

// runCommand runs the command with stdin, stdout and stderr passed through,
// its output also written to tail, and returns its exit status.
func runCommand(cfg Config, tail *tailBuffer, name string, args []string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdin = cfg.Stdin
	cmd.Stdout = io.MultiWriter(orDiscard(cfg.Stdout), tail)
	cmd.Stderr = io.MultiWriter(orDiscard(cfg.Stderr), tail)
	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			code = 127
		}
		msg := fmt.Sprintf("tend exec: cannot run %s: %v\n", name, err)
		_, _ = io.WriteString(orDiscard(cfg.Stderr), msg)
		_, _ = io.WriteString(tail, msg)
		return code
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, forwardedSignals...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	signal.Stop(sigs)
	close(done)

	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		msg := fmt.Sprintf("tend exec: %s: %v\n", name, err)
		_, _ = io.WriteString(orDiscard(cfg.Stderr), msg)
		_, _ = io.WriteString(tail, msg)
		return 126
	}
	if sig, ok := killedBy(exitErr.ProcessState); ok {
		return 128 + sig
	}
	return exitErr.ExitCode()
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// pinger sends the pings of one run.
type pinger struct {
	cfg       Config
	base      *url.URL
	rid       string
	bodyLimit int
	client    *http.Client
	log       io.Writer
}

func newPinger(cfg Config) *pinger {
	p := &pinger{cfg: cfg, rid: newRID(), log: orDiscard(cfg.Log), client: cfg.Client}
	if p.client == nil {
		p.client = &http.Client{}
	}
	p.base, _ = url.Parse(cfg.PingURL) // checked by CheckPingURL; a bad one fails each send
	switch {
	case cfg.BodyLimit == 0:
		p.bodyLimit = DefaultBodyLimit
	case cfg.BodyLimit < 0:
		p.bodyLimit = 0
	default:
		p.bodyLimit = cfg.BodyLimit
	}
	if p.cfg.Timeout <= 0 {
		p.cfg.Timeout = DefaultTimeout
	}
	switch {
	case p.cfg.Retries == 0:
		p.cfg.Retries = DefaultRetries
	case p.cfg.Retries < 0:
		p.cfg.Retries = 0
	}
	if p.cfg.Sleep == nil {
		p.cfg.Sleep = sleep
	}
	return p
}

// url is the ping URL for action: the base with action appended to its path
// and the run's rid added to its query.
func (p *pinger) url(action string) string {
	u := p.base.JoinPath(action)
	q := u.Query()
	q.Set("rid", p.rid)
	u.RawQuery = q.Encode()
	return u.String()
}

// send POSTs the ping for action with body, retrying network errors, 5xx and
// 429. It reports whether the ping got through and the Ping-Body-Limit the
// server announced (0 when none). A ping that does not get through is a
// warning on Log naming only the server's host: the URL holds the token.
func (p *pinger) send(ctx context.Context, action string, body []byte) (limit int, ok bool) {
	if p.base == nil || CheckPingURL(p.cfg.PingURL) != nil {
		fmt.Fprintf(p.log, "tend exec: not pinging /%s: %v\n", action, ErrBadPingURL)
		return 0, false
	}
	target := p.url(action)
	host := urlredact.Host(target)
	backoff := firstBackoff
	for attempt := 0; ; attempt++ {
		status, retryAfter, limit, err := p.attempt(ctx, target, body)
		if err == nil && status >= 200 && status < 300 {
			return limit, true
		}
		retryable := err != nil || status >= 500 || status == http.StatusTooManyRequests
		reason := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			reason = urlredact.Error(target, err).Error()
		}
		if !retryable {
			fmt.Fprintf(p.log, "tend exec: /%s ping to %s refused: %s\n", action, host, reason)
			return 0, false
		}
		if attempt >= p.cfg.Retries {
			fmt.Fprintf(p.log, "tend exec: /%s ping to %s failed (%s); giving up after %d attempts\n", action, host, reason, attempt+1)
			return 0, false
		}
		wait := backoff
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		}
		p.cfg.Sleep(ctx, wait)
		backoff = min(2*backoff, maxBackoff)
	}
}

// attempt sends one ping request and returns its status, Retry-After and
// Ping-Body-Limit.
func (p *pinger) attempt(ctx context.Context, target string, body []byte) (status int, retryAfter time.Duration, limit int, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if p.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", p.cfg.UserAgent)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	if n, err := strconv.Atoi(resp.Header.Get("Ping-Body-Limit")); err == nil && n > 0 {
		limit = n
	}
	return resp.StatusCode, retryAfter, limit, nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// newRID returns a random (version 4) UUID, the run id that pairs this run's
// start with its completion.
func newRID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := fmt.Sprintf("%x", b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// tailBuffer keeps the last max bytes written to it, from stdout and stderr
// alike, in the order they arrive.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(n int) *tailBuffer { return &tailBuffer{max: n} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.max <= 0 {
		return len(p), nil
	}
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// Bytes returns the kept tail, starting on a character boundary.
func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
		b = b[1:]
	}
	return append([]byte(nil), b...)
}
