package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// TestHeartbeatShowAndHistoryShowThePingLog: `heartbeat show` names the last
// ping, the run being timed and the last logged output; `heartbeat history`
// interleaves the pings (kind, exit status, duration, rid) with the
// transitions, newest first, and prints the logged bodies with -body.
func TestHeartbeatShowAndHistoryShowThePingLog(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := cli.Run(ctx, cfg, args, nil, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v\nstderr: %s", args, err, stderr.String())
		}
		return stdout.String()
	}
	run("heartbeat", "add", "-name", "db-backup", "-period", "3600", "-grace", "300")

	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := st.GetHeartbeatByName(ctx, org.ID, "db-backup")
	if err != nil {
		t.Fatal(err)
	}
	const rid = "728b3763-ea80-4113-9fc0-f49b3adf226a"
	t0 := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	three := 3
	for _, p := range []struct {
		in heartbeat.PingInput
		at time.Time
	}{
		{heartbeat.PingInput{Kind: heartbeat.PingStart, RID: rid}, t0},
		{heartbeat.PingInput{Kind: heartbeat.PingFail, ExitStatus: &three, RID: rid, Body: "dumping\npg_dump: connection refused\n"}, t0.Add(90 * time.Second)},
		{heartbeat.PingInput{Kind: heartbeat.PingStart}, t0.Add(time.Hour)},
	} {
		if _, err := st.RecordHeartbeatPing(ctx, hb.Token, p.in, p.at); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	show := run("heartbeat", "show", "db-backup")
	for _, want := range []string{
		"status:    down",
		"running:   started 2026-10-20T04:00:00Z",
		"last ping: start at 2026-10-20T04:00:00Z",
		"result:    fail (exit 3) at 2026-10-20T03:01:30Z, took 1m30s",
		"  pg_dump: connection refused",
	} {
		if !strings.Contains(show, want) {
			t.Errorf("show output missing %q; got:\n%s", want, show)
		}
	}

	hist := run("heartbeat", "history", "db-backup")
	lines := strings.Split(strings.TrimSpace(hist), "\n")
	if len(lines) != 4 {
		t.Fatalf("history has %d lines, want 4 (3 pings, 1 transition):\n%s", len(lines), hist)
	}
	for i, want := range []string{"start", "heartbeat.missed", "fail", "start"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("history line %d = %q, want it to show %q", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[1], "Heartbeat failed: db-backup (exit 3)") {
		t.Errorf("transition line = %q, want its subject", lines[1])
	}
	for _, want := range []string{"exit 3", "took 1m30s", "rid 728b3763", "36 B"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("failure line = %q, want %q", lines[2], want)
		}
	}
	if strings.Contains(hist, "pg_dump") {
		t.Errorf("history printed a body without -body:\n%s", hist)
	}

	withBody := run("heartbeat", "history", "db-backup", "-body")
	if !strings.Contains(withBody, "    dumping\n    pg_dump: connection refused\n") {
		t.Errorf("history -body does not print the logged body indented:\n%s", withBody)
	}
	if got := run("heartbeat", "history", "db-backup", "-limit", "1"); strings.Count(strings.TrimSpace(got), "\n") != 0 {
		t.Errorf("history -limit 1 printed more than one line:\n%s", got)
	}
}

// TestHeartbeatBodiesAreSafeOnATerminal: a logged body is what a token holder
// sent, so the CLI prints it without its control characters (an escape
// sequence could rewrite the operator's terminal); newlines and tabs stay.
func TestHeartbeatBodiesAreSafeOnATerminal(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "esc", "-period", "60"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, cfg.DSN)
	org, _ := st.BootstrapDefaultOrg(ctx)
	hb, err := st.GetHeartbeatByName(ctx, org.ID, "esc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordHeartbeatPing(ctx, hb.Token, heartbeat.PingInput{Kind: heartbeat.PingSuccess, Body: "ok\x1b[2J\x1b]0;owned\x07\tcol\r\nnext\x9b31m"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	for _, args := range [][]string{{"heartbeat", "show", "esc"}, {"heartbeat", "history", "esc", "-body"}} {
		stdout.Reset()
		if err := cli.Run(ctx, cfg, args, nil, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		out := stdout.String()
		for _, bad := range []string{"\x1b", "\x07", "\r", "\u009b"} {
			if strings.Contains(out, bad) {
				t.Errorf("%v printed a control character %q:\n%q", args, bad, out)
			}
		}
		if !strings.Contains(out, "\tcol") || !strings.Contains(out, "next") {
			t.Errorf("%v lost the body's text:\n%q", args, out)
		}
	}
}
