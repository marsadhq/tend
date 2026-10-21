package jobs

import (
	"bytes"
	"unicode/utf8"
)

// A capture that keeps only part of a step's output (the first 1 MiB of a
// shell job's, the first and last 32 KiB of a drill step's, the first 16 KiB
// of an expiry command's stderr) keeps the longest secret's length more than
// it shows, so that a secret across the cut is captured whole. The cut is
// then found in the captured bytes, before redaction, by headOf and tailOf;
// what lies outside it is dropped, and only then are the secrets redacted
// and the text held to its size. Cutting after redaction instead lets text
// slide: redacting a secret shortens the text, and the piece of a secret the
// capture itself cut, which lies past the cut, moves back inside it.

// headOf returns the part of b that a capture keeping its first n bytes
// shows, and whether anything was cut: all of b when it is no longer than n,
// otherwise its first n bytes, extended to the end of any secret that starts
// before that point and ends after it. lost says the capture dropped bytes
// after b; b then ends where it was cut, maybe inside a character.
func headOf(b []byte, n int, secrets []string, lost bool) ([]byte, bool) {
	if len(b) <= n {
		if lost {
			return trimPartialRune(b), true
		}
		return b, false
	}
	end := n
	for _, v := range secrets {
		if len(v) < 2 {
			continue // one byte cannot straddle a point
		}
		lo, hi := max(0, n-len(v)+1), min(len(b), n+len(v)-1)
		for i := lo; i < n; {
			at := bytes.Index(b[i:hi], []byte(v))
			if at < 0 || i+at >= n {
				break
			}
			end = max(end, i+at+len(v))
			i += at + 1
		}
	}
	if end == n {
		// The cut may fall inside a character; a secret's end never needs
		// trimming, so only a plain cut is.
		return trimPartialRune(b[:end]), true
	}
	return b[:end], true
}

// tailOf is headOf for a capture keeping the last n bytes of b: when b is
// longer, it starts n bytes before b's end, moved back to the start of any
// secret that starts before that point and ends after it. lost says the
// capture dropped bytes before b.
func tailOf(b []byte, n int, secrets []string, lost bool) ([]byte, bool) {
	if len(b) <= n {
		if lost {
			for len(b) > 0 && !utf8.RuneStart(b[0]) {
				b = b[1:]
			}
			return b, true
		}
		return b, false
	}
	at := len(b) - n
	start := at
	for _, v := range secrets {
		if len(v) < 2 {
			continue
		}
		lo, hi := max(0, at-len(v)+1), min(len(b), at+len(v)-1)
		for i := lo; i < at; {
			j := bytes.Index(b[i:hi], []byte(v))
			if j < 0 || i+j >= at {
				break
			}
			start = min(start, i+j)
			i += j + 1
		}
	}
	tail := b[start:]
	if start == at {
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return tail, true
}

// firstBytes holds a redacted text to its first n bytes, cut on a character
// boundary.
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return string(trimPartialRune([]byte(s[:n])))
}

// lastBytes holds a redacted text to its last n bytes, cut on a character
// boundary.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return t
}
