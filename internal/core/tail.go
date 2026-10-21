package core

import (
	"strings"
	"unicode/utf8"
)

// The tail of some output an alert carries (a failure ping's logged body, a
// failed run's output): its last OutputTailLines lines, and at most
// OutputTailBytes of them. The whole output stays where it is kept.
const (
	OutputTailLines = 10
	OutputTailBytes = 1000
)

// OutputTail returns the end of s for an alert: its last OutputTailLines
// lines, trailing blank space dropped, at most OutputTailBytes long and cut
// on a character boundary. Blank output has an empty tail.
func OutputTail(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > OutputTailLines {
		lines = lines[len(lines)-OutputTailLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > OutputTailBytes {
		tail = tail[len(tail)-OutputTailBytes:]
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return tail
}
