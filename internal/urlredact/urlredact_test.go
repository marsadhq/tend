package urlredact

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
