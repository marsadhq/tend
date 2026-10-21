package heartbeat_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/tend/internal/heartbeat"
)

// TestFailedEventKeepsThePlainNamePayload: a failure is reported as the
// existing heartbeat.missed with the plain heartbeat name as its payload, and
// what happened (the exit status, the tail of the output) goes in Detail.
func TestFailedEventKeepsThePlainNamePayload(t *testing.T) {
	three := 3
	ev := heartbeat.FailedEvent(7, 12, "db-backup", &three, "line one\nline two\n")
	if ev.Type != heartbeat.EventMissed || ev.Payload != "db-backup" || ev.Source != "heartbeat" || ev.OrgID != 7 || ev.DedupKey != "" {
		t.Fatalf("event = %+v, want heartbeat.missed for db-backup with no dedup key", ev)
	}
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok {
		t.Fatalf("detail %q does not parse", ev.Detail)
	}
	if d.Reason != heartbeat.ReasonFailed || d.ExitStatus == nil || *d.ExitStatus != 3 || d.HeartbeatID != 12 {
		t.Fatalf("detail = %+v, want reason failed with exit status 3 for heartbeat 12", d)
	}
	if d.Output != "line one\nline two" {
		t.Fatalf("detail output = %q, want the trimmed tail", d.Output)
	}

	plain := heartbeat.FailedEvent(7, 12, "db-backup", nil, "")
	d, ok = heartbeat.ParseMissDetail(plain.Detail)
	if !ok || d.Reason != heartbeat.ReasonFailed || d.ExitStatus != nil || d.Output != "" {
		t.Fatalf("/fail without a body: detail = %+v (ok=%v), want reason failed and nothing else", d, ok)
	}
}

// TestOverranEvent: a started run that did not finish within its grace is a
// heartbeat.missed whose detail names the heartbeat, the start and the grace
// the run was allowed (its RunGrace).
func TestOverranEvent(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	ev := heartbeat.OverranEvent(heartbeat.Heartbeat{ID: 5, OrgID: 1, Name: "sync", Status: "up",
		PeriodSeconds: 3600, GraceSeconds: 300, StartedAt: at})
	if ev.Type != heartbeat.EventMissed || ev.Payload != "sync" || ev.OrgID != 1 {
		t.Fatalf("event = %+v, want heartbeat.missed for sync", ev)
	}
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok || d.Reason != heartbeat.ReasonOverran || !d.StartedAt.Equal(at) || d.GraceSeconds != 300 || d.HeartbeatID != 5 {
		t.Fatalf("detail = %+v (ok=%v), want heartbeat 5 overran from %v with grace 300", d, ok, at)
	}
	// With no grace a run is allowed the period.
	noGrace := heartbeat.OverranEvent(heartbeat.Heartbeat{ID: 5, Name: "sync", PeriodSeconds: 3600, StartedAt: at})
	if d, _ := heartbeat.ParseMissDetail(noGrace.Detail); d.GraceSeconds != 3600 {
		t.Fatalf("no grace: detail grace = %d, want the period, 3600", d.GraceSeconds)
	}
}

// TestOverdueEvent: the watcher's plain miss says when the heartbeat last
// pinged and what it expects (period and grace), so its alert can too; a
// heartbeat that never pinged has no last ping.
func TestOverdueEvent(t *testing.T) {
	last := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	ev := heartbeat.OverdueEvent(heartbeat.Heartbeat{ID: 9, OrgID: 2, Name: "db-backup", Status: "up",
		LastSeenAt: last, PeriodSeconds: 86400, GraceSeconds: 3600})
	if ev.Type != heartbeat.EventMissed || ev.Payload != "db-backup" || ev.OrgID != 2 || ev.Source != "heartbeat" {
		t.Fatalf("event = %+v, want heartbeat.missed for db-backup", ev)
	}
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok || d.Reason != heartbeat.ReasonMissed || d.HeartbeatID != 9 || !d.LastSeenAt.Equal(last) ||
		d.PeriodSeconds != 86400 || d.GraceSeconds != 3600 {
		t.Fatalf("detail = %+v (ok=%v)", d, ok)
	}
	never := heartbeat.OverdueEvent(heartbeat.Heartbeat{ID: 9, Name: "db-backup", Status: "new", PeriodSeconds: 60})
	if d, ok := heartbeat.ParseMissDetail(never.Detail); !ok || !d.LastSeenAt.IsZero() || d.PeriodSeconds != 60 {
		t.Fatalf("never pinged: detail = %+v (ok=%v), want no last ping", d, ok)
	}
	if strings.Contains(never.Detail, "last_seen_at") {
		t.Fatalf("never pinged: detail %s should leave last_seen_at out", never.Detail)
	}
}

// TestParseMissDetailOfAPlainMiss: a miss recorded before misses carried a
// detail has none, and junk is not mistaken for one.
func TestParseMissDetailOfAPlainMiss(t *testing.T) {
	for _, s := range []string{"", "not json", `{"reason":""}`, `[1]`} {
		if d, ok := heartbeat.ParseMissDetail(s); ok {
			t.Errorf("ParseMissDetail(%q) = %+v, true; want false", s, d)
		}
	}
}

// TestOutputTail: the alert carries a short tail of the logged output: the
// last lines, at most OutputTailBytes, cut on a character boundary.
func TestOutputTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	got := heartbeat.OutputTail(strings.Join(lines, "\n") + "\n\n")
	if n := strings.Count(got, "\n") + 1; n != heartbeat.OutputTailLines {
		t.Fatalf("tail has %d lines, want %d: %q", n, heartbeat.OutputTailLines, got)
	}
	if !strings.HasSuffix(got, lines[len(lines)-1]) {
		t.Fatalf("tail %q does not end with the last line", got)
	}

	long := strings.Repeat("é", heartbeat.OutputTailBytes) // two bytes each
	got = heartbeat.OutputTail(long)
	if len(got) > heartbeat.OutputTailBytes || !utf8.ValidString(got) || got == "" {
		t.Fatalf("tail of one long line: %d bytes, valid=%v; want at most %d valid bytes", len(got), utf8.ValidString(got), heartbeat.OutputTailBytes)
	}
	if heartbeat.OutputTail(" \n\t\n") != "" {
		t.Fatal("blank output should have an empty tail")
	}
}
