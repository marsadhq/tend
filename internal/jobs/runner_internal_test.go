package jobs

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestFinishRetryPauses pins the shape of the default retry schedule for a
// failed finish: a few attempts, each pause longer than the one before, and the
// whole series short enough that a run being retried is still far from the
// reaper's slack.
func TestFinishRetryPauses(t *testing.T) {
	if n := len(finishRetryPauses); n < 2 || n > 5 {
		t.Fatalf("%d retries, want a few (2 to 5)", n)
	}
	r := &Runner{}
	var total, prev time.Duration
	for i, want := range finishRetryPauses {
		got := r.finishBackoff(i + 1)
		if got != want {
			t.Errorf("finishBackoff(%d) = %s, want %s", i+1, got, want)
		}
		if got <= prev {
			t.Errorf("pause %d is %s, not longer than the %s before it", i+1, got, prev)
		}
		prev = got
		total += got
	}
	if total >= reapSlack/2 {
		t.Errorf("pauses add up to %s, want well under reapSlack (%s)", total, reapSlack)
	}

	r.FinishBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Hour }
	if got := r.finishBackoff(2); got != 2*time.Hour {
		t.Errorf("finishBackoff(2) with FinishBackoff set = %s, want 2h", got)
	}
}

// TestCleanOutput pins the clean-up applied to captured output: NUL bytes go
// first, then every run of invalid UTF-8 becomes one U+FFFD, and the result is
// always valid and NUL-free. Clean text passes through untouched.
func TestCleanOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"ascii", "plain text\n", "plain text\n"},
		{"valid multi-byte", "héllo € \U0001F600", "héllo € \U0001F600"},
		{"a literal U+FFFD is kept", "a�b", "a�b"},
		{"NUL dropped", "a\x00b", "ab"},
		{"only NULs", "\x00\x00\x00", ""},
		{"invalid byte", "a\xffb", "a�b"},
		{"run of invalid bytes is one replacement", "a\xff\xfe\xfdb", "a�b"},
		{"separate invalid bytes are separate replacements", "a\xffb\xfec", "a�b�c"},
		{"truncated sequence at the end", "caf\xc3", "caf�"},
		{"latin-1 text", "na\xefve", "na�ve"},
		{"overlong encoding", "a\xc0\xafb", "a�b"},
		{"surrogate half", "a\xed\xa0\x80b", "a�b"},
		{"NULs splitting a character put it back together", "\xe2\x00\x82\x00\xac", "€"},
		{"NULs between invalid bytes merge them into one run", "a\xff\x00\xfeb", "a�b"},
		{"utf-16 text", "\xff\xfeh\x00i\x00", "�hi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cleanOutput(c.in)
			if got != c.want {
				t.Errorf("cleanOutput(%q) = %q, want %q", c.in, got, c.want)
			}
			if !utf8.ValidString(got) || strings.Contains(got, "\x00") {
				t.Errorf("cleanOutput(%q) = %q is not clean text", c.in, got)
			}
			if again := cleanOutput(got); again != got {
				t.Errorf("cleaning twice changed the text: %q -> %q", got, again)
			}
		})
	}
}

// TestStoredOutputRedactsWhatWillBeStored covers the order of cleaning and
// redaction: no spelling of a secret that cleaning turns back into the secret
// may survive, a secret that is not valid UTF-8 is still redacted, whatever is
// printed next to it, and the result is always clean text.
func TestStoredOutputRedactsWhatWillBeStored(t *testing.T) {
	const secret = "s3cr3t-value"
	spaced := func(s, sep string) string { return strings.Join(strings.Split(s, ""), sep) }
	bytewise := func(s, sep string) string {
		parts := make([]string, len(s))
		for i := 0; i < len(s); i++ {
			parts[i] = s[i : i+1]
		}
		return strings.Join(parts, sep)
	}

	cases := []struct {
		name    string
		raw     string
		secrets []string
		want    string
	}{
		{"plain", "key=" + secret + "\n", []string{secret}, "key=***\n"},
		{"NUL between every character", "key=" + spaced(secret, "\x00") + "\n", []string{secret}, "key=***\n"},
		{"one NUL in the middle", "key=s3cr3t\x00-value\n", []string{secret}, "key=***\n"},
		{"NULs around and inside", "\x00s3\x00\x00cr3t-value\x00", []string{secret}, "***"},
		{"plain and spaced in one output", secret + " " + spaced(secret, "\x00"), []string{secret}, "*** ***"},
		{"multi-byte secret split between its bytes", "k=" + bytewise("päss-€", "\x00"), []string{"päss-€"}, "k=***"},
		{"two secrets, one spaced", "a=alpha-1 b=" + spaced("bravo-2", "\x00"), []string{"alpha-1", "bravo-2"}, "a=*** b=***"},
		{"invalid byte inside is not the secret and is kept", "s3cr3t\xff-value", []string{secret}, "s3cr3t�-value"},
		{"no secrets", "a\x00b\xff", nil, "ab�"},
		{"empty secret is ignored", "a\x00b", []string{""}, "ab"},

		// Secrets that are not valid UTF-8 (a password in Latin-1, a binary key).
		{"latin-1 secret printed as it is", "pw=p\xe4ssw\xf6rd\n", []string{"p\xe4ssw\xf6rd"}, "pw=***\n"},
		{"latin-1 secret with NULs between its bytes", "pw=" + bytewise("p\xe4ssw\xf6rd", "\x00"), []string{"p\xe4ssw\xf6rd"}, "pw=***"},
		{"secret completed into a character by its neighbour", "\xc2\xa9abc", []string{"\xa9abc"}, "�***"},
		{"secret of invalid bytes only", "k=\xff\xfe\xfd other \xfa", []string{"\xff\xfe\xfd"}, "k=*** other �"},
		{"invalid bytes the job had replaced itself", "pw=p�ssw�rd\n", []string{"p\xe4ssw\xf6rd"}, "pw=***\n"},
		{"secret holding a NUL, with and without it", "k=ab\x00cd and abcd\n", []string{"ab\x00cd"}, "k=*** and ***\n"},

		// The same with a NUL inside the secret as well. The NUL keeps the raw
		// bytes from matching, and the neighbouring byte turns the end of the
		// secret into a character, so its cleaned form does not match either.
		{"half a character at the end, a NUL inside, completed by its neighbour", "k=a\x00bc\xc3\xa9\n", []string{"abc\xc3"}, "k=***�\n"},
		{"half a character at the start, a NUL inside, completed by its neighbour", "k=\xc3\xa9a\x00bc\n", []string{"\xa9abc"}, "k=�***\n"},
		{"completed at both ends", "\xe2\x82\x00\xacab\xc3\x00\xa9", []string{"\xacab\xc3"}, "�***�"},
		{"completed at one end, an invalid byte in the middle", "k=\xc3\xa9a\x00b\xffcd\n", []string{"\xa9ab\xffcd"}, "k=�***\n"},
		{"holding a NUL itself, and completed by its neighbour", "k=a\x00b\xc3\xa9\n", []string{"a\x00b\xc3"}, "k=***�\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := storedOutput(c.raw, c.secrets)
			if got != c.want {
				t.Errorf("storedOutput(%q) = %q, want %q", c.raw, got, c.want)
			}
			if !utf8.ValidString(got) || strings.Contains(got, "\x00") {
				t.Errorf("storedOutput(%q) = %q is not clean text", c.raw, got)
			}
			for _, s := range c.secrets {
				if s != "" && strings.Contains(got, s) {
					t.Errorf("storedOutput(%q) = %q still contains the secret %q", c.raw, got, s)
				}
				// Nor may it contain what is left of a secret without its bad bytes.
				for _, part := range strings.FieldsFunc(cleanOutput(s), func(r rune) bool { return r == utf8.RuneError }) {
					if len(part) > 1 && strings.Contains(got, part) {
						t.Errorf("storedOutput(%q) = %q still contains %q, part of the secret %q", c.raw, got, part, s)
					}
				}
			}
		})
	}
}

// TestCapOutput pins the cap on the stored text: at most maxOutputBytes plus
// one marker, cut on a character boundary at every alignment, and untouched
// when it already fits.
func TestCapOutput(t *testing.T) {
	fill := func(n int) string { return strings.Repeat("x", n) }

	t.Run("within the cap is unchanged", func(t *testing.T) {
		for _, s := range []string{
			"", "short", fill(maxOutputBytes), fill(maxOutputBytes) + truncationMarker, "short" + truncationMarker,
			// A capture the executor cut a few bytes short of the cap, to keep a
			// character whole: with its marker it is longer than the cap.
			fill(maxOutputBytes-1) + truncationMarker, fill(maxOutputBytes-3) + truncationMarker,
		} {
			if got := capOutput(s); got != s {
				t.Errorf("capOutput changed %d bytes of fitting text into %d bytes", len(s), len(got))
			}
		}
	})

	t.Run("over the cap is cut and marked once", func(t *testing.T) {
		for name, s := range map[string]string{
			"unmarked":       fill(maxOutputBytes + 1),
			"already marked": fill(2*maxOutputBytes) + truncationMarker,
		} {
			got := capOutput(s)
			if want := fill(maxOutputBytes) + truncationMarker; got != want {
				t.Errorf("%s: capOutput gave %d bytes ending %q, want %d bytes ending with one marker",
					name, len(got), got[max(0, len(got)-40):], len(want))
			}
		}
	})

	t.Run("the cut never splits a character", func(t *testing.T) {
		for _, ch := range []string{"é", "€", "\U0001F600", "�"} {
			for pad := 0; pad < len(ch); pad++ {
				s := fill(pad) + strings.Repeat(ch, maxOutputBytes/len(ch)+8)
				got := capOutput(s)
				kept := strings.TrimSuffix(got, truncationMarker)
				if kept == got {
					t.Fatalf("%q pad=%d: output was not marked as truncated", ch, pad)
				}
				if !utf8.ValidString(got) {
					t.Errorf("%q pad=%d: the cut split a character", ch, pad)
				}
				if len(kept) > maxOutputBytes || len(kept) <= maxOutputBytes-len(ch) {
					t.Errorf("%q pad=%d: kept %d bytes, want within one character of the %d byte cap", ch, pad, len(kept), maxOutputBytes)
				}
			}
		}
	})
}
