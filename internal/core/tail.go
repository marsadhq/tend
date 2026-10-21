package core

import (
	"strings"
	"unicode/utf8"
)

// The tail of some output an alert carries (a failure ping's logged body, a
// failed run's output). An event keeps the last MaxOutputTailLines lines of
// it, a chat alert shows the last OutputTailLines of those unless
// TEND_ALERT_OUTPUT_LINES says otherwise, and the webhook's text has always
// carried OutputTailLines; every tail is at most OutputTailBytes long. The
// whole output stays where it is kept.
const (
	OutputTailLines    = 10
	MaxOutputTailLines = 50
	OutputTailBytes    = 1000
)

// OutputTail is TailLines(s, OutputTailLines).
func OutputTail(s string) string { return TailLines(s, OutputTailLines) }

// TailLines returns the end of s for an alert: its last lines lines (none
// when lines is not positive), trailing blank space dropped, at most
// OutputTailBytes long and cut on a character boundary. Blank output has an
// empty tail. Cutting a tail again to fewer lines gives what cutting the
// whole output to that many would have.
func TailLines(s string, lines int) string {
	s = strings.TrimRight(s, " \t\r\n")
	if s == "" || lines <= 0 {
		return ""
	}
	parts := strings.Split(s, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	tail := strings.Join(parts, "\n")
	if len(tail) > OutputTailBytes {
		tail = tail[len(tail)-OutputTailBytes:]
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return tail
}

// HeadLines is the start of s for an alert: its first lines lines (none when
// lines is not positive), surrounding blank space dropped, at most
// OutputTailBytes long and cut on a character boundary.
func HeadLines(s string, lines int) string {
	s = strings.Trim(s, " \t\r\n")
	if s == "" || lines <= 0 {
		return ""
	}
	parts := strings.SplitN(s, "\n", lines+1)
	if len(parts) > lines {
		parts = parts[:lines]
	}
	head := strings.Join(parts, "\n")
	if len(head) > OutputTailBytes {
		cut := OutputTailBytes
		for cut > 0 && !utf8.RuneStart(head[cut]) {
			cut--
		}
		head = head[:cut]
	}
	return head
}
