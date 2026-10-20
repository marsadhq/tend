package cli_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/config"
)

// pingRecorder is a ping server recording the paths it was sent.
func pingRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// TestExecCommand: `tend exec` needs no database, wraps the command between
// /start and /<exit-status>, and exits with the command's status.
func TestExecCommand(t *testing.T) {
	srv, paths := pingRecorder(t)
	noDB := config.Config{Driver: "sqlite"}
	run := func(args ...string) (string, string, error) {
		var stdout, stderr bytes.Buffer
		err := cli.Run(context.Background(), noDB, args, strings.NewReader(""), &stdout, &stderr)
		return stdout.String(), stderr.String(), err
	}

	out, _, err := run("exec", "--ping-url", srv.URL+"/ping/tok", "--", "sh", "-c", "echo hi; exit 3")
	var exit *cli.ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("exec of a command exiting 3: err = %v, want an ExitError with code 3", err)
	}
	if out != "hi\n" {
		t.Fatalf("stdout = %q, want the command's", out)
	}
	if got := strings.Join(paths(), " "); got != "/ping/tok/start /ping/tok/3" {
		t.Fatalf("pings = %s", got)
	}

	t.Setenv("TEND_BASE_URL", srv.URL+"/")
	if _, _, err := run("exec", "--token", "tok2", "--no-start", "true"); err != nil {
		t.Fatalf("exec --token: %v", err)
	}
	if got := paths(); got[len(got)-1] != "/ping/tok2/0" || len(got) != 3 {
		t.Fatalf("pings = %v, want /ping/tok2/0 only after the first run", got)
	}

	t.Setenv("TEND_PING_URL", srv.URL+"/ping/from-env")
	if _, _, err := run("exec", "true"); err != nil {
		t.Fatalf("exec with TEND_PING_URL: %v", err)
	}
	if got := paths(); got[len(got)-1] != "/ping/from-env/0" {
		t.Fatalf("pings = %v, want /ping/from-env/0 last", got)
	}
}

// TestExecUsageErrors: the ways a `tend exec` line can be wrong.
func TestExecUsageErrors(t *testing.T) {
	t.Setenv("TEND_PING_URL", "")
	t.Setenv("TEND_BASE_URL", "")
	noDB := config.Config{Driver: "sqlite"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"exec", "true"}, "--ping-url"},
		{[]string{"exec", "--ping-url", "https://hc-ping.com/x"}, "command"},
		{[]string{"exec", "--token", "tok", "true"}, "TEND_BASE_URL"},
		{[]string{"exec", "--token", "tok", "--ping-url", "https://hc-ping.com/x", "true"}, "not both"},
		{[]string{"exec", "--ping-url", "hc-ping.com/x", "true"}, "absolute"},
	} {
		var stdout, stderr bytes.Buffer
		err := cli.Run(context.Background(), noDB, c.args, nil, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want one mentioning %q", c.args, err, c.want)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), noDB, []string{"exec", "-h"}, nil, &stdout, &stderr); err != nil {
		t.Errorf("exec -h: %v, want the usage and no error", err)
	}
	if !strings.Contains(stdout.String()+stderr.String(), "tend exec") {
		t.Errorf("exec -h printed no usage: %q", stdout.String()+stderr.String())
	}
}

// TestExecUsageHidesThePingURL: the ping URL holds the token, so the one in
// TEND_PING_URL is never echoed by the usage text a bad flag prints.
func TestExecUsageHidesThePingURL(t *testing.T) {
	t.Setenv("TEND_PING_URL", "https://tend.example/ping/secret-token-in-env")
	noDB := config.Config{Driver: "sqlite"}
	for _, args := range [][]string{{"exec", "-h"}, {"exec", "--no-such-flag", "true"}} {
		var stdout, stderr bytes.Buffer
		_ = cli.Run(context.Background(), noDB, args, nil, &stdout, &stderr)
		if out := stdout.String() + stderr.String(); strings.Contains(out, "secret-token-in-env") {
			t.Errorf("%v printed the ping URL from the environment:\n%s", args, out)
		}
	}
}
