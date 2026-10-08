// Package urlredact keeps secret-bearing URLs out of error text.
//
// Webhook, Slack and Discord channel URLs, and the URLs of HTTP jobs, often
// carry a credential in the path or query string. net/http reports a failed
// request as a *url.Error whose message quotes the full URL, so logging such
// an error, or storing it as job output, leaks the credential. The helpers
// here reduce a URL to its host and an error to its underlying cause.
//
// The package depends only on the standard library, so every package that
// makes outbound HTTP requests can share this one implementation.
package urlredact

import (
	"errors"
	"fmt"
	"net/url"
)

// placeholder stands in for a URL whose host cannot be determined.
const placeholder = "<url>"

// errNoCause replaces a *url.Error that carries no underlying error, so the
// URL-bearing value itself is never returned.
var errNoCause = errors.New("request failed")

// Host returns the host (with port, if any) of rawURL for use in error
// messages and logs. Path, query, fragment and userinfo are dropped. When
// rawURL cannot be parsed or has no host it returns "<url>", never the raw
// string itself.
func Host(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return placeholder
}

// Error returns err rewritten as "<host>: <cause>", where host is Host(rawURL)
// and cause is err with every *url.Error unwrapped to the error it wraps. The
// diagnostic survives ("connection refused", "no such host", "context deadline
// exceeded") while the URL that url.Error prints does not. The cause stays in
// the chain, so errors.Is and errors.As still work on the result.
//
// An error that neither is nor wraps a *url.Error is kept as the cause
// unchanged, so callers must not pass in text they formatted the URL into
// themselves. Error returns nil when err is nil.
func Error(rawURL string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", Host(rawURL), cause(err))
}

// cause strips every *url.Error from err's chain and returns what is left.
func cause(err error) error {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			return err
		}
		if ue.Err == nil {
			return errNoCause
		}
		err = ue.Err
	}
}
