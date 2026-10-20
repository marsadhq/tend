package notify

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
)

// permanentError marks a send failure that no retry can fix: the destination
// understood the request and refused it for good (a 4xx answer to this
// notification, an SMTP 5xx reply). The delivery worker gives such a delivery
// up at once, recording notification.failed as it does after MaxAge, instead
// of retrying it for a day while it holds back the channel's later alerts.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so IsPermanent reports it. A nil err stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, was marked by
// Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// httpStatusError is the error for a non-2xx answer from an HTTP destination:
// permanent for a 4xx other than 408 (Request Timeout) and 429 (Too Many
// Requests), which ask for a retry, and transient otherwise (5xx, or anything
// odder). what names the destination without its URL, which may carry a
// token.
func httpStatusError(what string, code int) error {
	err := fmt.Errorf("%s: status %d", what, code)
	if code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests {
		return Permanent(err)
	}
	return err
}

// smtpReplyError marks err permanent when it carries an SMTP 5xx reply
// (permanent negative completion, RFC 5321 section 4.2.1); 4xx replies are
// transient and stay retryable.
func smtpReplyError(err error) error {
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 && tp.Code < 600 {
		return Permanent(err)
	}
	return err
}
