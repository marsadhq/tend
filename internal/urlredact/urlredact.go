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

// errBadEscape and errBadHost replace net/url's EscapeError and
// InvalidHostError. Both of those are strings holding the part of the URL that
// could not be parsed, up to three characters of it, and quote it in their
// message; in a URL that carries a token those characters can be part of the
// token. The fixed phrases keep the diagnosis and drop the quotation.
var (
	errBadEscape = errors.New("invalid URL escape")
	errBadHost   = errors.New("invalid character in host name")
)

// errBadURL replaces any other reason for which net/url rejects a URL, unless
// it is one of fixedParseReasons. Some of those reasons quote the part that
// would not parse, and with no limit on its length: `invalid port ":..." after
// host` holds everything from a colon to the end of the authority, which in a
// URL that has lost its host or its '@' is the token.
var errBadURL = errors.New("invalid URL")

// fixedParseReasons are the reasons net/url gives for rejecting a URL that are
// a fixed phrase, and so say nothing about what is in the URL. They are worth
// keeping: they tell an operator what to look for. A reason that is not listed
// here, one a later Go release adds included, is reported as errBadURL.
var fixedParseReasons = map[string]bool{
	"empty url":               true,
	"missing protocol scheme": true,
	"invalid URI for request": true,
	"first path segment in URL cannot contain colon": true,
	"net/url: invalid control character in URL":      true,
	"net/url: invalid userinfo":                      true,
	"missing ']' in host":                            true,
	"invalid IP-literal":                             true,
}

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
// Two causes are not kept: a url.EscapeError or url.InvalidHostError is itself
// a piece of the URL, so it is replaced by a fixed phrase ("invalid URL
// escape", "invalid character in host name") and is no longer in the chain.
//
// A rawURL that net/url cannot parse is a case of its own. No request was made
// with it, so err is that failure, and some of the reasons net/url gives
// for one quote the text that would not parse. The cause is then kept only if
// it is one of the fixed phrases above or in fixedParseReasons, and is
// "invalid URL" otherwise.
//
// For a rawURL that does parse, an error that neither is nor wraps a
// *url.Error is kept as the cause unchanged, so callers must not pass in text
// they formatted the URL into themselves. Error returns nil when err is nil.
func Error(rawURL string, err error) error {
	if err == nil {
		return nil
	}
	c := cause(err)
	if _, parseErr := url.Parse(rawURL); parseErr != nil && !fixedReason(c) {
		c = errBadURL
	}
	return fmt.Errorf("%s: %w", Host(rawURL), c)
}

// fixedReason reports whether c is a phrase known to say nothing about the
// URL: one of this package's own, or one of net/url's fixed ones.
func fixedReason(c error) bool {
	return c == errNoCause || c == errBadEscape || c == errBadHost || fixedParseReasons[c.Error()]
}

// cause strips every *url.Error from err's chain and returns what is left,
// unless what is left is, or wraps, one of the two net/url errors that quote
// characters of the URL: those are swapped for their fixed phrase, wrappers
// and all, since a wrapper's text would repeat the quotation.
func cause(err error) error {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			break
		}
		if ue.Err == nil {
			return errNoCause
		}
		err = ue.Err
	}
	var escape url.EscapeError
	if errors.As(err, &escape) {
		return errBadEscape
	}
	var host url.InvalidHostError
	if errors.As(err, &host) {
		return errBadHost
	}
	return err
}
