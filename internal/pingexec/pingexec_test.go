package pingexec_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/pingexec"
)

// hit is one request the fake ping server received.
type hit struct {
	method, path, query, body string
}

// fakePings is a ping server: it records every request and answers each path
// with the next scripted status (200 once the script for it runs out).
type fakePings struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	hits   []hit
	script map[string][]int // path suffix -> statuses to answer in turn
	header http.Header      // extra headers on every 200
}

func newFakePings(t *testing.T) *fakePings {
	f := &fakePings{t: t, script: map[string][]int{}, header: http.Header{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, hit{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		code := http.StatusOK
		for suffix, codes := range f.script {
			if strings.HasSuffix(r.URL.Path, suffix) && len(codes) > 0 {
				code, f.script[suffix] = codes[0], codes[1:]
				break
			}
		}
		f.mu.Unlock()
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		if code == http.StatusOK {
			for k, v := range f.header {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePings) all() []hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hit(nil), f.hits...)
}

// run runs a shell command under pingexec against pingURL and returns its
// exit status, what it wrote, the warnings, and the waits the retries took.
func run(t *testing.T, cfg pingexec.Config, script string) (code int, stdout, stderr, warnings string, waits []time.Duration) {
	t.Helper()
	var out, errb, logb bytes.Buffer
	var mu sync.Mutex
	cfg.Stdout, cfg.Stderr, cfg.Log = &out, &errb, &logb
	if cfg.Stdin == nil {
		cfg.Stdin = strings.NewReader("")
	}
	cfg.Sleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
	}
	code = pingexec.Run(context.Background(), cfg, "sh", "-c", script)
	return code, out.String(), errb.String(), logb.String(), waits
}

var ridRe = regexp.MustCompile(`^rid=([0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$`)

// TestExecSuccess: /start, then the command with its output passed through,
// then /0 carrying the output, both pings with the same fresh rid.
func TestExecSuccess(t *testing.T) {
	f := newFakePings(t)
	code, stdout, stderr, warnings, _ := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true}, `echo out; echo err >&2`)
	if code != 0 || stdout != "out\n" || stderr != "err\n" || warnings != "" {
		t.Fatalf("code %d stdout %q stderr %q warnings %q", code, stdout, stderr, warnings)
	}
	hits := f.all()
	if len(hits) != 2 || hits[0].path != "/ping/tok/start" || hits[1].path != "/ping/tok/0" {
		t.Fatalf("pings = %+v, want /start then /0", hits)
	}
	m0, m1 := ridRe.FindStringSubmatch(hits[0].query), ridRe.FindStringSubmatch(hits[1].query)
	if m0 == nil || m1 == nil || m0[1] != m1[1] {
		t.Fatalf("rids %q / %q, want one fresh UUID on both", hits[0].query, hits[1].query)
	}
	if hits[0].body != "" || !strings.Contains(hits[1].body, "out\n") || !strings.Contains(hits[1].body, "err\n") {
		t.Fatalf("bodies %q / %q, want none and the output", hits[0].body, hits[1].body)
	}
	for _, h := range hits {
		if h.method != http.MethodPost {
			t.Errorf("ping %s used %s, want POST", h.path, h.method)
		}
	}
}

// TestExecExitStatus: the command's exit status is reported and returned.
func TestExecExitStatus(t *testing.T) {
	f := newFakePings(t)
	code, _, _, _, _ := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true}, `echo boom; exit 3`)
	if code != 3 {
		t.Fatalf("exit code %d, want 3", code)
	}
	hits := f.all()
	if got := hits[len(hits)-1]; got.path != "/ping/tok/3" || got.body != "boom\n" {
		t.Fatalf("completion = %+v, want /3 with the output", got)
	}
}

// TestExecKilledBySignal: a command killed by a signal is reported as 128 +
// the signal number, as a shell does.
func TestExecKilledBySignal(t *testing.T) {
	f := newFakePings(t)
	code, _, _, _, _ := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok"}, `kill -TERM $$`)
	if code != 143 {
		t.Fatalf("exit code %d, want 143", code)
	}
	if hits := f.all(); len(hits) != 1 || hits[0].path != "/ping/tok/143" {
		t.Fatalf("pings = %+v, want only /143 (no start)", hits)
	}
}

// TestExecCommandNotFound: a command that cannot be run is 127, and the
// failure ping says why.
func TestExecCommandNotFound(t *testing.T) {
	f := newFakePings(t)
	var logb bytes.Buffer
	code := pingexec.Run(context.Background(), pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true,
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &logb, Log: &logb,
		Sleep: func(context.Context, time.Duration) {}}, "no-such-command-tend-test")
	if code != 127 {
		t.Fatalf("exit code %d, want 127", code)
	}
	hits := f.all()
	if len(hits) != 2 || hits[1].path != "/ping/tok/127" || !strings.Contains(hits[1].body, "no-such-command-tend-test") {
		t.Fatalf("pings = %+v, want /start and /127 naming the command", hits)
	}
}

// TestExecOutputTail: only the tail of the output is sent, at most BodyLimit
// bytes, and at most what the server's Ping-Body-Limit says it keeps.
func TestExecOutputTail(t *testing.T) {
	f := newFakePings(t)
	script := `i=0; while [ $i -lt 2000 ]; do echo "line $i"; i=$((i+1)); done`
	_, stdout, _, _, _ := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true, BodyLimit: 1000}, script)
	if !strings.HasPrefix(stdout, "line 0\n") || !strings.HasSuffix(stdout, "line 1999\n") {
		t.Fatal("the output was not passed through in full")
	}
	body := f.all()[1].body
	if len(body) != 1000 || !strings.HasSuffix(body, "line 1999\n") {
		t.Fatalf("body is %d bytes ending %q, want the last 1000", len(body), body[max(0, len(body)-20):])
	}

	f = newFakePings(t)
	f.header.Set("Ping-Body-Limit", "100")
	run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true, BodyLimit: 1000}, script)
	if body := f.all()[1].body; len(body) != 100 || !strings.HasSuffix(body, "line 1999\n") {
		t.Fatalf("with Ping-Body-Limit 100: body is %d bytes, want the last 100", len(body))
	}

	f = newFakePings(t)
	run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", BodyLimit: -1}, `echo secret-ish`)
	if body := f.all()[0].body; body != "" {
		t.Fatalf("BodyLimit -1 sent %q, want no output", body)
	}
}

// TestExecRetries: a ping is retried on a server error and on 429 (waiting
// its Retry-After), but not on another 4xx; the command runs regardless and
// its status is returned.
func TestExecRetries(t *testing.T) {
	f := newFakePings(t)
	f.script["/start"] = []int{503, 502}
	f.script["/0"] = []int{429}
	code, _, _, warnings, waits := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true, Retries: 3}, `true`)
	if code != 0 || warnings != "" {
		t.Fatalf("code %d warnings %q", code, warnings)
	}
	if n := len(f.all()); n != 5 {
		t.Fatalf("%d requests, want 5 (start x3, completion x2)", n)
	}
	if len(waits) != 3 || waits[0] != time.Second || waits[1] != 2*time.Second || waits[2] != 7*time.Second {
		t.Fatalf("waits = %v, want [1s 2s 7s] (backoff, then Retry-After)", waits)
	}

	f = newFakePings(t)
	f.script["/start"] = []int{404}
	code, _, _, warnings, waits = run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true, Retries: 3}, `exit 4`)
	if code != 4 || len(waits) != 0 || !strings.Contains(warnings, "404") {
		t.Fatalf("404: code %d waits %v warnings %q, want 4, no retry and a warning", code, waits, warnings)
	}

	f = newFakePings(t)
	f.script["/start"] = []int{500, 500, 500, 500}
	_, _, _, warnings, waits = run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Start: true, Retries: 3}, `true`)
	if len(waits) != 3 || !strings.Contains(warnings, "giving up") {
		t.Fatalf("persistent 500: waits %v warnings %q, want 3 retries then a warning", waits, warnings)
	}
}

// TestExecNetworkErrorsDoNotLeakTheURL: a server that cannot be reached is
// retried, the command still runs, and the warning names the host only, not
// the token in the path.
func TestExecNetworkErrorsDoNotLeakTheURL(t *testing.T) {
	f := newFakePings(t)
	dead := f.srv.URL
	f.srv.Close()
	code, stdout, _, warnings, waits := run(t, pingexec.Config{PingURL: dead + "/ping/secret-token-value", Start: true, Retries: 2}, `echo ran; exit 5`)
	if code != 5 || stdout != "ran\n" {
		t.Fatalf("code %d stdout %q, want the command run anyway", code, stdout)
	}
	if len(waits) != 4 {
		t.Fatalf("waits = %v, want 2 retries for each of the 2 pings", waits)
	}
	if !strings.Contains(warnings, strings.TrimPrefix(dead, "http://")) || strings.Contains(warnings, "secret-token-value") {
		t.Fatalf("warnings %q: want the host, never the token", warnings)
	}
}

// TestExecHealthchecksURLs: a hc-ping.com style URL (no /ping/ prefix) and a
// slug URL with ?create=1 get their suffixes in the path and keep the query.
func TestExecHealthchecksURLs(t *testing.T) {
	f := newFakePings(t)
	run(t, pingexec.Config{PingURL: f.srv.URL + "/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", Start: true}, `true`)
	run(t, pingexec.Config{PingURL: f.srv.URL + "/fqOOd6-F4MMNuCEnzTU01w/db-backup/?create=1", Start: true}, `exit 2`)
	hits := f.all()
	want := []string{
		"/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278/start",
		"/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278/0",
		"/fqOOd6-F4MMNuCEnzTU01w/db-backup/start",
		"/fqOOd6-F4MMNuCEnzTU01w/db-backup/2",
	}
	for i, w := range want {
		if hits[i].path != w {
			t.Errorf("ping %d path %q, want %q", i, hits[i].path, w)
		}
	}
	for _, h := range hits[2:] {
		if !strings.HasPrefix(h.query, "create=1&rid=") {
			t.Errorf("slug ping query %q, want create=1 kept and the rid added", h.query)
		}
	}
}

// TestExecStdin: the command reads tend exec's stdin.
func TestExecStdin(t *testing.T) {
	f := newFakePings(t)
	_, stdout, _, _, _ := run(t, pingexec.Config{PingURL: f.srv.URL + "/ping/tok", Stdin: strings.NewReader("hello\n")}, `cat`)
	if stdout != "hello\n" {
		t.Fatalf("stdout %q, want stdin passed through", stdout)
	}
}

// TestPingURLValidation: a ping URL must be absolute http(s).
func TestPingURLValidation(t *testing.T) {
	for _, u := range []string{"", "tend.example/ping/x", "ftp://tend.example/ping/x", "https:///ping/x", "https://tend.example/ping/x?rid=728b3763-ea80-4113-9fc0-f49b3adf226a"} {
		if err := pingexec.CheckPingURL(u); err == nil {
			t.Errorf("CheckPingURL(%q) = nil, want an error", u)
		}
	}
	if err := pingexec.CheckPingURL("https://hc-ping.com/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278"); err != nil {
		t.Errorf("CheckPingURL of a good URL: %v", err)
	}
}
