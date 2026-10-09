package urlredact

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestHost(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{"path token", "https://hooks.example.com/services/T00/B00/SECRET", "hooks.example.com"},
		{"query token", "http://example.com:8443/hook?token=SECRET", "example.com:8443"},
		{"userinfo", "https://user:SECRET@example.com/x", "example.com"},
		{"ipv6", "http://[::1]:9000/SECRET", "[::1]:9000"},
		{"unparseable", "http://example.com/SECRET\x7f", "<url>"},
		{"no host", "/just/a/path/SECRET", "<url>"},
		{"empty", "", "<url>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Host(tc.rawURL)
			if got != tc.want {
				t.Errorf("Host(%q) = %q, want %q", tc.rawURL, got, tc.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Errorf("Host(%q) leaked the secret: %q", tc.rawURL, got)
			}
		})
	}
}

func TestError_Nil(t *testing.T) {
	if err := Error("https://example.com/SECRET", nil); err != nil {
		t.Fatalf("Error(_, nil) = %v, want nil", err)
	}
}

// TestError_UnwrapsURLError covers the shape net/http returns from a failed
// request: a *url.Error quoting the full URL around the real cause.
func TestError_UnwrapsURLError(t *testing.T) {
	const rawURL = "https://hooks.example.com/services/T00/B00/SECRET?token=SECRET"
	root := errors.New("dial tcp 203.0.113.5:443: connect: connection refused")
	in := &url.Error{Op: "Post", URL: rawURL, Err: root}

	got := Error(rawURL, in)
	if got == nil {
		t.Fatal("Error returned nil for a non-nil error")
	}
	if strings.Contains(got.Error(), "SECRET") {
		t.Fatalf("redacted error leaked the secret: %v", got)
	}
	if want := "hooks.example.com: " + root.Error(); got.Error() != want {
		t.Errorf("Error() = %q, want %q", got.Error(), want)
	}
	if !errors.Is(got, root) {
		t.Errorf("errors.Is(redacted, root) = false, want the cause kept in the chain")
	}
}

// TestError_NestedAndWrappedURLErrors covers a *url.Error wrapped by another
// error (whose own text repeats the URL) and one url.Error inside another.
func TestError_NestedAndWrappedURLErrors(t *testing.T) {
	const rawURL = "http://example.com/hook/SECRET"
	inner := &url.Error{Op: "parse", URL: rawURL, Err: context.DeadlineExceeded}
	outer := &url.Error{Op: "Get", URL: rawURL, Err: inner}
	wrapped := fmt.Errorf("calling %s: %w", rawURL, outer)

	got := Error(rawURL, wrapped)
	if strings.Contains(got.Error(), "SECRET") {
		t.Fatalf("redacted error leaked the secret: %v", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("errors.Is(redacted, context.DeadlineExceeded) = false; got %v", got)
	}
}

// TestError_URLErrorWithoutCause guards the degenerate *url.Error with a nil
// Err: its own text is all there is, and that text contains the URL.
func TestError_URLErrorWithoutCause(t *testing.T) {
	const rawURL = "http://example.com/hook/SECRET"
	got := Error(rawURL, &url.Error{Op: "Get", URL: rawURL})
	if strings.Contains(got.Error(), "SECRET") {
		t.Fatalf("redacted error leaked the secret: %v", got)
	}
	if !strings.HasPrefix(got.Error(), "example.com: ") {
		t.Errorf("Error() = %q, want it to start with the host", got.Error())
	}
}

// TestError_PlainError checks that an error that is not a *url.Error is kept
// as the cause and only gains the host prefix.
func TestError_PlainError(t *testing.T) {
	root := errors.New(`net/http: invalid method "BAD METHOD"`)
	got := Error("http://example.com/hook/SECRET", root)
	if want := "example.com: " + root.Error(); got.Error() != want {
		t.Errorf("Error() = %q, want %q", got.Error(), want)
	}
	if !errors.Is(got, root) {
		t.Errorf("errors.Is(redacted, root) = false")
	}
}

// TestError_UnparseableURL checks that a URL Host cannot parse is replaced by
// the placeholder, and that the parse failure itself (a *url.Error quoting the
// raw URL) is reduced to its cause.
func TestError_UnparseableURL(t *testing.T) {
	const rawURL = "http://example.com/hook/SECRET\x7f"
	_, parseErr := url.Parse(rawURL)
	if parseErr == nil {
		t.Fatal("test setup: expected url.Parse to reject a control character")
	}
	got := Error(rawURL, parseErr)
	if strings.Contains(got.Error(), "SECRET") {
		t.Fatalf("redacted error leaked the secret: %v", got)
	}
	if !strings.HasPrefix(got.Error(), "<url>: ") {
		t.Errorf("Error() = %q, want the <url> placeholder as prefix", got.Error())
	}
}

// TestError_EscapeAndHostErrorsAreNotQuoted covers the two net/url errors
// whose own text quotes characters of the URL: EscapeError (up to three
// characters, starting at a bad '%') and InvalidHostError (the character that
// may not appear in a host). In a URL with a token in it those characters can
// belong to the token, so the redacted error must say what was wrong without
// repeating them.
func TestError_EscapeAndHostErrorsAreNotQuoted(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		quoted string // what net/url's message quotes
		want   string
	}{
		{"bad escape in the path", "https://hooks.example.com/services/T00/tok%S3CRET", "%S3", "<url>: invalid URL escape"},
		{"escape cut short at the end", "https://hooks.example.com/services/T00/tok%S", "%S", "<url>: invalid URL escape"},
		{"lone percent at the end", "https://hooks.example.com/hook/S3CRET%", "%", "<url>: invalid URL escape"},
		{"bad escape in the host", "https://hooks%S3.example.com/hook", "%S3", "<url>: invalid URL escape"},
		{"character not allowed in a host", "https://tok{S3CRET.example.com/hook", "{", "<url>: invalid character in host name"},
		{"space in the host", "https://tok S3CRET.example.com/hook", " ", "<url>: invalid character in host name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := url.Parse(tc.rawURL)
			if parseErr == nil {
				t.Fatalf("test setup: url.Parse accepted %q", tc.rawURL)
			}
			// What a caller would have logged or stored without the replacement.
			if !strings.Contains(parseErr.Error(), strconv.Quote(tc.quoted)) {
				t.Fatalf("test setup: parse error %q does not quote %q", parseErr, tc.quoted)
			}

			got := Error(tc.rawURL, parseErr)
			if got.Error() != tc.want {
				t.Errorf("Error() = %q, want %q", got.Error(), tc.want)
			}
			if strings.Contains(got.Error(), `"`) {
				t.Errorf("redacted error still quotes part of the URL: %q", got.Error())
			}
			var escape url.EscapeError
			var host url.InvalidHostError
			if errors.As(got, &escape) || errors.As(got, &host) {
				t.Errorf("the URL-bearing error is still in the chain of %q", got.Error())
			}
		})
	}
}

// TestError_EscapeAndHostErrorsOutsideAURLError covers the same two errors
// reaching Error some other way: bare (url.PathUnescape returns them like that)
// or wrapped by a caller. The wrapper goes too, since its text repeats the
// quotation.
func TestError_EscapeAndHostErrorsOutsideAURLError(t *testing.T) {
	const rawURL = "https://hooks.example.com/services/T00/S3CRET"
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"bare escape error", url.EscapeError("%S3"), errBadEscape},
		{"wrapped escape error", fmt.Errorf("decoding path: %w", url.EscapeError("%S3")), errBadEscape},
		{"escape error inside two url.Errors", &url.Error{Op: "Post", URL: rawURL,
			Err: &url.Error{Op: "parse", URL: rawURL, Err: url.EscapeError("%S3")}}, errBadEscape},
		{"bare host error", url.InvalidHostError("S"), errBadHost},
		{"wrapped host error", fmt.Errorf("checking host: %w", url.InvalidHostError("S")), errBadHost},
		{"joined with another error", errors.Join(errors.New("first"), url.EscapeError("%S3")), errBadEscape},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Error(rawURL, tc.err)
			if want := "hooks.example.com: " + tc.want.Error(); got.Error() != want {
				t.Errorf("Error() = %q, want %q", got.Error(), want)
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("errors.Is(redacted, %q) = false", tc.want)
			}
			if strings.Contains(got.Error(), "S3") || strings.Contains(got.Error(), `"`) {
				t.Errorf("redacted error still quotes part of the URL: %q", got.Error())
			}
		})
	}
}

// TestError_UnparseableURLIsNotQuoted covers the reasons net/url gives for
// rejecting a URL that are neither of the two typed errors above and still
// quote the text that would not parse, with no limit on its length. The usual
// one is `invalid port ":..." after host`: everything from a colon to the end
// of the authority, which in a URL that has lost its host or its '@' is the
// token. Whatever the reason, a URL that does not parse is reported as
// "invalid URL" unless the reason is a known fixed phrase.
func TestError_UnparseableURLIsNotQuoted(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		// accepted: releases of Go differ on whether this is a valid URL at
		// all. Where it is, there is no parse error to test.
		accepted bool
	}{
		{name: "token where the port belongs", rawURL: "https://hooks.example.com:S3CRETTOKEN/services/x"},
		{name: "host missing in front of id:token", rawURL: "https://bot123456:AAF-S3CRETTOKEN/sendMessage"},
		{name: "a second colon", rawURL: "https://bot:123456:AAF-S3CRETTOKEN/sendMessage"},
		{name: "userinfo without its @", rawURL: "https://user:S3CRETTOKEN.example.com/hook"},
		{name: "token after an IP literal", rawURL: "https://[::1]:S3CRETTOKEN/hook"},
		{name: "token as an IP literal", rawURL: "https://[S3CRETTOKEN]/hook", accepted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := url.Parse(tc.rawURL)
			if parseErr == nil {
				if tc.accepted {
					t.Skipf("this release of Go accepts %q", tc.rawURL)
				}
				t.Fatalf("test setup: url.Parse accepted %q", tc.rawURL)
			}
			// What a caller would have logged or stored without the replacement.
			if !strings.Contains(parseErr.Error(), "S3CRETTOKEN") {
				t.Fatalf("test setup: parse error %q does not quote the token", parseErr)
			}

			for name, err := range map[string]error{
				"as url.Parse returns it":    parseErr,
				"wrapped by the caller":      fmt.Errorf("building request: %w", parseErr),
				"an error of another origin": errors.New(`dial tcp: lookup "S3CRETTOKEN": no such host`),
			} {
				got := Error(tc.rawURL, err)
				if want := "<url>: invalid URL"; got.Error() != want {
					t.Errorf("%s: Error() = %q, want %q", name, got.Error(), want)
				}
				if !errors.Is(got, errBadURL) {
					t.Errorf("%s: errors.Is(redacted, errBadURL) = false", name)
				}
				if errors.Is(got, err) {
					t.Errorf("%s: the quoting error is still in the chain of %q", name, got.Error())
				}
			}
		})
	}
}

// TestError_FixedParseErrorsKeepTheirText checks the replacement leaves alone
// the reasons net/url gives as a fixed phrase: those say nothing about the
// content of the URL, and they tell an operator what to look for.
func TestError_FixedParseErrorsKeepTheirText(t *testing.T) {
	for rawURL, want := range map[string]string{
		"://hooks.example.com/S3CRET":      "<url>: missing protocol scheme",
		"https://hooks.example.com/S3\x7f": "<url>: net/url: invalid control character in URL",
		"https://[::1/S3CRET":              "<url>: missing ']' in host",
		"https://us er:S3CRET@example.com": "<url>: net/url: invalid userinfo",
		"3S:CRET/hook":                     "<url>: first path segment in URL cannot contain colon",
	} {
		_, parseErr := url.Parse(rawURL)
		if parseErr == nil {
			t.Fatalf("test setup: url.Parse accepted %q", rawURL)
		}
		if got := Error(rawURL, parseErr); got.Error() != want {
			t.Errorf("Error(%q) = %q, want %q", rawURL, got.Error(), want)
		}
	}
}

// TestError_FixedParseReasonsAreFixed guards the list itself: a phrase with a
// format verb or a quotation in it has no place there.
func TestError_FixedParseReasonsAreFixed(t *testing.T) {
	for reason := range fixedParseReasons {
		if strings.ContainsAny(reason, `%"`) {
			t.Errorf("fixedParseReasons holds %q, which is not a fixed phrase", reason)
		}
	}
	for _, own := range []error{errNoCause, errBadEscape, errBadHost} {
		if !fixedReason(own) {
			t.Errorf("fixedReason(%q) = false, want this package's own phrases kept", own)
		}
	}
	if fixedReason(errors.New(`invalid port ":S3CRETTOKEN" after host`)) {
		t.Error("fixedReason accepts an error that quotes the URL")
	}
}
