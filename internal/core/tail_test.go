package core

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 80; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	all := strings.Join(lines, "\n") + "\n\n"
	if got := TailLines(all, 0); got != "" {
		t.Errorf("TailLines(_, 0) = %q, want nothing", got)
	}
	if got := TailLines(all, 3); got != "line 78\nline 79\nline 80" {
		t.Errorf("TailLines(_, 3) = %q", got)
	}
	if got := TailLines(all, MaxOutputTailLines); strings.Count(got, "\n")+1 != MaxOutputTailLines || !strings.HasPrefix(got, "line 31\n") {
		t.Errorf("TailLines(_, %d) = %q", MaxOutputTailLines, got)
	}
	long := strings.Repeat("é", OutputTailBytes)
	if got := TailLines(long, 50); len(got) > OutputTailBytes || !utf8.ValidString(got) {
		t.Errorf("one long line: %d bytes, valid=%v; want at most %d valid bytes", len(got), utf8.ValidString(got), OutputTailBytes)
	}
}

// What an event keeps (the longest tail an alert can show) cut again to the
// default tail is exactly the default tail of the whole output, so the
// webhook's text, which has always carried the default tail, does not change.
func TestDefaultTailOfTheKeptTail(t *testing.T) {
	var many []string
	for i := 0; i < 60; i++ {
		many = append(many, strings.Repeat("x", i*7))
	}
	for name, s := range map[string]string{
		"short":                 "a\nb\n",
		"many short lines":      strings.Repeat("ok\n", 70),
		"lines of every length": strings.Join(many, "\n"),
		"one huge line":         strings.Repeat("é", 2*OutputTailBytes),
		"ten long lines":        strings.Repeat(strings.Repeat("ü", 60)+"\n", 12),
		"blank":                 " \n\t\n",
		"exactly the bytes":     strings.Repeat("y", OutputTailBytes-10) + "\n" + strings.Repeat("z", 9),
	} {
		if got, want := OutputTail(TailLines(s, MaxOutputTailLines)), OutputTail(s); got != want {
			t.Errorf("%s: default tail of the kept tail = %q, want %q", name, got, want)
		}
	}
}

func TestHeadLines(t *testing.T) {
	if got := HeadLines("FAIL: a\nFAIL: b\nFAIL: c\n", 2); got != "FAIL: a\nFAIL: b" {
		t.Errorf("HeadLines(_, 2) = %q", got)
	}
	if got := HeadLines("x", 0); got != "" {
		t.Errorf("HeadLines(_, 0) = %q", got)
	}
	long := strings.Repeat("é", OutputTailBytes)
	if got := HeadLines(long, 5); len(got) > OutputTailBytes || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Errorf("one long line: %d bytes, valid=%v; want at most %d valid bytes from the start", len(got), utf8.ValidString(got), OutputTailBytes)
	}
}
