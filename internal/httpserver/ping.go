package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/httpx"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// The ping API follows Healthchecks' (https://healthchecks.io/docs/http_api/),
// so a script, borgmatic, runitor or anything else written for it switches to
// Tend by changing the base URL:
//
//	/ping/<token>                 success
//	/ping/<token>/0               success (exit status 0)
//	/ping/<token>/start           a run starts
//	/ping/<token>/fail            failure
//	/ping/<token>/<1..255>        failure with that exit status
//	/ping/<token>/log             a log entry; changes nothing
//
// HEAD, GET and POST all work. A POST body is logged with the ping, up to
// PingBodyLimit bytes, and ?rid=<uuid> ties a start to its completion.

// PingBodyLimit is how much of a ping's request body is kept, in bytes: the
// first 100 kB, as in Healthchecks, which announces it in the same
// Ping-Body-Limit response header.
const PingBodyLimit = 100_000

// PingBodyTruncated ends a logged body that was cut at PingBodyLimit.
const PingBodyTruncated = "\n... [ping body truncated at 100 kB]"

// Ping limits; see the limiters in New.
const (
	pingMissRate   = 1
	pingMissBurst  = 30
	pingTokenRate  = 1
	pingTokenBurst = 10
)

// pingWriteTimeout bounds recording one ping. The write runs detached from the
// request (see handlePing), so this is what stops it from running forever.
const pingWriteTimeout = 10 * time.Second

// pingRequest is one ping, parsed from its URL.
type pingRequest struct {
	token string // the heartbeat's ping token
	in    heartbeat.PingInput
}

// errPingURL and errPingRID are a ping URL Healthchecks answers 400 for.
var (
	errPingURL = errors.New("invalid url format")
	errPingRID = errors.New("invalid uuid format")
)

// errPingNotFound is a ping URL that names nothing: 404.
var errPingNotFound = errors.New("not found")

// parsePing reads the ping a request makes from its path below /ping/ and its
// query. The body is read separately (readPingBody), once the ping is known
// to be well formed.
func parsePing(path string, query map[string][]string) (pingRequest, error) {
	path = strings.TrimSuffix(path, "/")
	segs := strings.Split(path, "/")
	if path == "" || len(segs) > 2 {
		return pingRequest{}, errPingNotFound
	}
	req := pingRequest{token: normalizeToken(segs[0]), in: heartbeat.PingInput{Kind: heartbeat.PingSuccess}}
	if len(segs) == 2 {
		in, ok, err := pingAction(segs[1])
		if err != nil {
			return pingRequest{}, err
		}
		if !ok {
			return pingRequest{}, errPingNotFound
		}
		req.in = in
	}
	if rids, ok := query["rid"]; ok {
		rid, ok := canonicalUUID(rids[0])
		if !ok {
			return pingRequest{}, errPingRID
		}
		req.in.RID = rid
	}
	return req, nil
}

// pingAction maps the last segment of a ping URL to the ping it makes: start,
// fail, log, or an exit status (0 is a success, 1 to 255 a failure). ok is
// false for a segment that is none of these; an exit status out of range is
// errPingURL, as in Healthchecks.
func pingAction(seg string) (in heartbeat.PingInput, ok bool, err error) {
	switch seg {
	case "start":
		return heartbeat.PingInput{Kind: heartbeat.PingStart}, true, nil
	case "fail":
		return heartbeat.PingInput{Kind: heartbeat.PingFail}, true, nil
	case "log":
		return heartbeat.PingInput{Kind: heartbeat.PingLog}, true, nil
	}
	if seg == "" || strings.Trim(seg, "0123456789") != "" {
		return heartbeat.PingInput{}, false, nil
	}
	n, err := strconv.Atoi(seg)
	if err != nil || n > 255 {
		return heartbeat.PingInput{}, false, errPingURL
	}
	kind := heartbeat.PingFail
	if n == 0 {
		kind = heartbeat.PingSuccess
	}
	return heartbeat.PingInput{Kind: kind, ExitStatus: &n}, true, nil
}

// canonicalUUID returns s, a UUID in its canonical text form (8-4-4-4-12 hex
// digits, either letter case, no braces), in lower case; ok is false for
// anything else.
func canonicalUUID(s string) (string, bool) {
	if len(s) != 36 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// normalizeToken returns a ping token as it is stored. A token in the shape
// of a UUID (a check imported from Healthchecks keeps its UUID as its token)
// matches in either letter case, as a UUID does; any other token is exact.
func normalizeToken(token string) string {
	if u, ok := canonicalUUID(token); ok {
		return u
	}
	return token
}

// readPingBody returns the body of a ping as it is logged: at most
// PingBodyLimit bytes of it, made plain text the way captured job output is
// (NUL bytes dropped, invalid UTF-8 replaced with U+FFFD), with every
// credential in creds (the ping's own token) replaced by "***" so a script
// that echoes its ping URL does not put it on the dashboard, and cut on a
// character boundary with PingBodyTruncated when it was longer. The order is
// that of a job's output: clean, then redact, then cap.
func readPingBody(r *http.Request, creds ...string) (string, error) {
	if r.Body == nil || r.Method != http.MethodPost {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, PingBodyLimit+1))
	if err != nil {
		return "", err
	}
	truncated := len(raw) > PingBodyLimit
	if truncated {
		raw = trimPartialRune(raw[:PingBodyLimit])
	}
	s := string(raw)
	if !utf8.ValidString(s) || strings.Contains(s, "\x00") {
		s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
	}
	for _, c := range creds {
		if c != "" {
			s = strings.ReplaceAll(s, c, "***")
		}
	}
	if len(s) > PingBodyLimit {
		truncated = true
		cut := PingBodyLimit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	if truncated {
		s += PingBodyTruncated
	}
	return s, nil
}

// trimPartialRune drops a trailing, unfinished UTF-8 sequence from b, which a
// cut at a byte offset can leave; bytes that were never valid UTF-8 are kept
// (cleaning replaces them).
func trimPartialRune(b []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if start := len(b) - i; utf8.RuneStart(b[start]) {
			if !utf8.FullRune(b[start:]) {
				return b[:start]
			}
			return b
		}
	}
	return b
}

// pingReply writes a ping response: Healthchecks' short plain-text bodies,
// its Ping-Body-Limit header, and CORS open to any origin (a ping is
// authorized by its URL alone, never by a cookie).
func pingReply(w http.ResponseWriter, code int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Ping-Body-Limit", strconv.Itoa(PingBodyLimit))
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// handlePing records one ping of the ping API. A failure takes the heartbeat
// down at once and appends heartbeat.missed, a success on a down heartbeat
// appends heartbeat.recovered; either is written by the store, with the
// deliveries its rules match, in the same transaction as the state change and
// the ping log entry, and the dispatch func only nudges the delivery worker.
// Nothing here blocks on a slow destination, so the ping response is never
// held hostage by a down webhook receiver.
//
// The write is detached from the request's context: once a ping has arrived it
// is recorded in full even if the client times out or disconnects meanwhile,
// so a transition can no longer be committed without its event, or skipped
// because the caller went away. pingWriteTimeout bounds it instead.
//
// Rate limiting happens here rather than in a RateLimiter.Limit wrapper,
// because it depends on whether the token is real. Before the database is
// touched, a request is refused (429 with Retry-After) when its client address
// has used up its budget of unknown tokens, or its token its own ping budget.
// A miss then costs the address one unit; a real ping, of any kind, costs its
// token one, and nothing per address. Pings from one host for many heartbeats,
// all on the same minute, therefore never crowd each other out, while an
// address that keeps guessing tokens is cut off whatever it sends next.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodHead, http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", "HEAD, GET, POST")
		pingReply(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req, err := parsePing(strings.TrimPrefix(r.URL.Path, "/ping/"), r.URL.Query())
	switch {
	case errors.Is(err, errPingURL), errors.Is(err, errPingRID):
		pingReply(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		pingReply(w, http.StatusNotFound, "not found")
		return
	}

	now := s.clk.Now()
	missKey, tokenKey := clientIP(r)+"|ping-miss", "ping-token|"+req.token
	if !s.pingMissLimiter.Has(missKey, now) {
		httpx.TooManyRequests(w, s.pingMissLimiter)
		return
	}
	if !s.pingTokenLimiter.Has(tokenKey, now) {
		httpx.TooManyRequests(w, s.pingTokenLimiter)
		return
	}
	if req.in.Body, err = readPingBody(r, req.token); err != nil {
		pingReply(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pingWriteTimeout)
	defer cancel()
	res, err := s.store.RecordHeartbeatPing(ctx, req.token, req.in, now)
	if errors.Is(err, store.ErrNotFound) {
		s.pingMissLimiter.Allow(missKey, now)
		pingReply(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		// A real DB error must not masquerade as a 404. The token is a
		// credential, so log only a truncated hint, never the full value.
		s.log.Error("record ping failed", "token_hint", tokenHint(req.token), "kind", req.in.Kind, "err", err)
		pingReply(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.pingTokenLimiter.Allow(tokenKey, now)
	if res.Event != nil && s.dispatch != nil {
		s.dispatch(ctx, *res.Event)
	}
	pingReply(w, http.StatusOK, "OK")
}

// tokenHint returns a short, non-sensitive prefix of a ping token for logs, so
// the full token (a credential) is never written to a log aggregator.
func tokenHint(token string) string {
	if len(token) <= 8 {
		return "********"
	}
	return token[:8] + "..."
}
