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
	ev := heartbeat.FailedEvent(7, "db-backup", &three, "line one\nline two\n")
	if ev.Type != heartbeat.EventMissed || ev.Payload != "db-backup" || ev.Source != "heartbeat" || ev.OrgID != 7 || ev.DedupKey != "" {
		t.Fatalf("event = %+v, want heartbeat.missed for db-backup with no dedup key", ev)
	}
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok {
		t.Fatalf("detail %q does not parse", ev.Detail)
	}
	if d.Reason != heartbeat.ReasonFailed || d.ExitStatus == nil || *d.ExitStatus != 3 {
		t.Fatalf("detail = %+v, want reason failed with exit status 3", d)
	}
	if d.Output != "line one\nline two" {
		t.Fatalf("detail output = %q, want the trimmed tail", d.Output)
	}

	plain := heartbeat.FailedEvent(7, "db-backup", nil, "")
	d, ok = heartbeat.ParseMissDetail(plain.Detail)
	if !ok || d.Reason != heartbeat.ReasonFailed || d.ExitStatus != nil || d.Output != "" {
		t.Fatalf("/fail without a body: detail = %+v (ok=%v), want reason failed and nothing else", d, ok)
	}
}

// TestOverranEvent: a started run that did not finish within its grace is a
// heartbeat.missed whose detail names the start and the grace.
func TestOverranEvent(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	ev := heartbeat.OverranEvent(1, "sync", at, 300)
	if ev.Type != heartbeat.EventMissed || ev.Payload != "sync" {
		t.Fatalf("event = %+v, want heartbeat.missed for sync", ev)
	}
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok || d.Reason != heartbeat.ReasonOverran || !d.StartedAt.Equal(at) || d.GraceSeconds != 300 {
		t.Fatalf("detail = %+v (ok=%v), want overran from %v with grace 300", d, ok, at)
	}
}

// TestParseMissDetailOfAPlainMiss: the watcher's plain miss has no detail,
// and junk is not mistaken for one.
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
