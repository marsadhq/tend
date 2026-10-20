package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedNow is an executor whose probes judge dates as of now.
func fixedNow(now time.Time) *Executor {
	return &Executor{Now: func() time.Time { return now }}
}

func expiryJob(s Spec) Job {
	return Job{Name: "token", Type: Expiry, TimeoutSeconds: 5, Spec: &s}
}

func TestExpiryFixedDate(t *testing.T) {
	cases := []struct {
		expires  time.Time
		warnDays int
		status   RunStatus
		want     string
	}{
		{t0.Add(14 * day), 14, StatusFailed, "FAIL: expires in 14 days"},
		{t0.Add(14*day + time.Second), 14, StatusSucceeded, "ok: expires in 14 days"},
		{t0.Add(time.Hour), 0, StatusSucceeded, "ok: expires in 1 hour"},
		{t0.Add(-time.Second), 0, StatusFailed, "FAIL: expired"},
		{t0.Add(-40 * day), 14, StatusFailed, "FAIL: expired 40 days ago"},
		{t0.Add(-90 * time.Second), 0, StatusFailed, "FAIL: expired 1 minute ago"},
	}
	for _, c := range cases {
		res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiresAt: c.expires, WarnDays: c.warnDays}), nil)
		if res.Status != c.status || !strings.HasPrefix(res.Output, strings.SplitN(c.want, ":", 2)[0]) || !strings.Contains(res.Output, c.want) {
			t.Errorf("expires %v warn %d: status %s, output:\n%s\nwant %s and %q", c.expires, c.warnDays, res.Status, res.Output, c.status, c.want)
		}
		wantExit := 0
		if c.status == StatusFailed {
			wantExit = 1
		}
		if res.ExitCode != wantExit {
			t.Errorf("expires %v: exit %d, want %d", c.expires, res.ExitCode, wantExit)
		}
	}
	res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiresAt: t0.Add(100 * day), WarnDays: 14}), nil)
	for _, want := range []string{"expires_at:", dateText(t0.Add(100 * day)), "(as configured)", "days_left:", "100"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestExpiryCommand(t *testing.T) {
	res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiryCommand: "echo '  2027-01-31T00:00:00Z  '", WarnDays: 14}), nil)
	if res.Status != StatusSucceeded || res.ExitCode != 0 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	for _, want := range []string{"ok: expires in 101 days", "2027-01-31T00:00:00Z", "(printed by expiry_command)"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}

	// It runs with the job's env (where the runner put the resolved
	// secrets), and what it prints on stderr is shown but not parsed.
	env := map[string]string{"KEY_EXPIRY": "2026-10-25T00:00:00+02:00"}
	res = fixedNow(t0).Run(context.Background(),
		expiryJob(Spec{ExpiryCommand: `echo checking >&2; printf '%s\n' "$KEY_EXPIRY"`, WarnDays: 14}), env)
	if res.Status != StatusFailed || res.ExitCode != 1 || !strings.Contains(res.Output, "FAIL: expires in 3 days") {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "stderr:") || !strings.Contains(res.Output, "checking") {
		t.Fatalf("output does not show the command's stderr:\n%s", res.Output)
	}
}

func TestExpiryCommandBadOutput(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{`echo null`, `printed "null"`},
		{`echo`, "printed nothing"},
		{`printf '2027-01-31T00:00:00Z\n2028-01-31T00:00:00Z\n'`, "more than one line"},
		{`echo 31/01/2027`, `printed "31/01/2027"`},
		{`head -c 200000 /dev/zero | tr '\0' 7`, "printed"},
	}
	for _, c := range cases {
		res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiryCommand: c.command, WarnDays: 14}), nil)
		if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, c.want) {
			t.Errorf("%s: status %s exit %d, output:\n%.400s\nwant an error about %q", c.command, res.Status, res.ExitCode, res.Output, c.want)
		}
		if len(res.Output) > 4096 {
			t.Errorf("%s: output of %d bytes for one bad line", c.command, len(res.Output))
		}
	}
}

func TestExpiryCommandFails(t *testing.T) {
	res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiryCommand: "echo no such node >&2; exit 3", WarnDays: 14}), nil)
	if res.Status != StatusFailed || res.ExitCode != 3 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "expiry_command exited with status 3") || !strings.Contains(res.Output, "no such node") {
		t.Fatalf("output:\n%s", res.Output)
	}
}

func TestExpiryCommandTimeout(t *testing.T) {
	j := expiryJob(Spec{ExpiryCommand: "sleep 10; echo 2027-01-31", WarnDays: 14})
	j.TimeoutSeconds = 1
	start := time.Now()
	res := fixedNow(t0).Run(context.Background(), j, nil)
	if res.Status != StatusTimedOut {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("took %v to time out after 1s", took)
	}
}

// TestExpiryCommandInjection checks that what the command prints is only ever
// parsed as a date and that Tend splices nothing into the command: output,
// env values and the job's own name that look like shell are never run.
func TestExpiryCommandInjection(t *testing.T) {
	dir := t.TempDir()
	marker := func(n string) string { return filepath.Join(dir, n) }

	printed := []string{
		`$(touch ` + marker("a") + `)`,
		"`touch " + marker("b") + "`",
		`2027-01-31T00:00:00Z; touch ` + marker("c"),
		`2027-01-31T00:00:00Z && touch ` + marker("d"),
	}
	for _, p := range printed {
		// printf %s prints p as it is; the command itself never runs it.
		cmd := `printf '%s\n' "$OUT"`
		res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiryCommand: cmd, WarnDays: 14}), map[string]string{"OUT": p})
		if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, "not an RFC 3339 date") {
			t.Errorf("printed %q: status %s exit %d, output:\n%s", p, res.Status, res.ExitCode, res.Output)
		}
	}

	// A name that would break out of quoting if it were spliced into the
	// command.
	j := expiryJob(Spec{ExpiryCommand: "echo 2027-01-31T00:00:00Z", WarnDays: 14})
	j.Name = `x"; touch ` + marker("e") + `; echo "`
	if res := fixedNow(t0).Run(context.Background(), j, nil); res.Status != StatusSucceeded {
		t.Errorf("status %s, output:\n%s", res.Status, res.Output)
	}

	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := os.Stat(marker(n)); err == nil {
			t.Errorf("marker %s was created: something was run as shell", n)
		}
	}
}

func TestParseExpiryDate(t *testing.T) {
	for s, want := range map[string]time.Time{
		"2027-01-31T00:00:00Z":           time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
		"2027-01-31T02:00:00+02:00":      time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
		"2027-01-31T00:00:00.123456789Z": time.Date(2027, 1, 31, 0, 0, 0, 123456789, time.UTC),
		"2027-01-31":                     time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
	} {
		got, err := ParseExpiryDate(s)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("ParseExpiryDate(%q) = %v, %v; want %v in UTC", s, got, err, want)
		}
	}
	for _, s := range []string{"", "tomorrow", "2027-01-31T00:00:00", "2027/01/31", "0001-01-01T00:00:00Z"} {
		if _, err := ParseExpiryDate(s); err == nil {
			t.Errorf("ParseExpiryDate(%q) = nil error", s)
		}
	}
}

func TestValidateSpecExpiry(t *testing.T) {
	for _, s := range []Spec{{ExpiresAt: t0}, {ExpiryCommand: "echo 2027-01-31", WarnDays: 3}} {
		if err := ValidateSpec(Expiry, &s); err != nil {
			t.Errorf("ValidateSpec(%+v) = %v", s, err)
		}
	}
	bad := []struct {
		spec Spec
		want string
	}{
		{Spec{}, "expires_at or expiry_command"},
		{Spec{ExpiryCommand: "   "}, "expires_at or expiry_command"},
		{Spec{ExpiresAt: t0, ExpiryCommand: "echo x"}, "not both"},
		{Spec{ExpiresAt: t0, WarnDays: -2}, "warn_days"},
	}
	for _, c := range bad {
		err := ValidateSpec(Expiry, &c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateSpec(%+v) = %v, want an error about %q", c.spec, err, c.want)
		}
	}
}
