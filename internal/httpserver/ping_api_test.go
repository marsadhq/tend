package httpserver_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/store"
)

// pingAPI is a server over a store with one heartbeat, for driving the ping
// API in-process.
type pingAPI struct {
	s      store.Store
	srv    *httpserver.Server
	h      http.Handler
	orgID  int64
	id     int64
	token  string
	events []core.Event // dispatched
}

func newPingAPI(t *testing.T, s store.Store, token string) *pingAPI {
	t.Helper()
	ctx := context.Background()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	id, tok, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "nightly", Token: token, PeriodSeconds: 3600, GraceSeconds: 300})
	if err != nil {
		t.Fatalf("CreateHeartbeat: %v", err)
	}
	p := &pingAPI{s: s, orgID: org.ID, id: id, token: tok}
	fk := clock.NewFake(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	p.srv = httpserver.New(s, fk, func(_ context.Context, ev core.Event) { p.events = append(p.events, ev) },
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	p.h = p.srv.Handler()
	return p
}

// do sends one request for path (relative to /ping/) and returns the recorder.
func (p *pingAPI) do(method, path, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/ping/"+path, rd)
	req.RemoteAddr = "198.51.100.20:5555"
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	return rec
}

func (p *pingAPI) hb(t *testing.T) heartbeat.Heartbeat {
	t.Helper()
	hb, err := p.s.GetHeartbeat(context.Background(), p.orgID, p.id)
	if err != nil {
		t.Fatalf("GetHeartbeat: %v", err)
	}
	return hb
}

func (p *pingAPI) log(t *testing.T) []heartbeat.Ping {
	t.Helper()
	ps, err := p.s.ListHeartbeatPings(context.Background(), p.orgID, p.id, 200)
	if err != nil {
		t.Fatalf("ListHeartbeatPings: %v", err)
	}
	return ps
}

func wantOK(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("%s: %d %q, want 200 ok", what, rec.Code, rec.Body.String())
	}
}

// TestPingAPIVariants: every Healthchecks ping URL works with HEAD, GET and
// POST, answers 200 "ok" with Healthchecks' Ping-Body-Limit header, and is
// logged as the kind it names.
func TestPingAPIVariants(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-variants")
		cases := []struct {
			suffix string
			kind   string
			exit   int // -1: none
		}{
			{"", heartbeat.PingSuccess, -1},
			{"/", heartbeat.PingSuccess, -1},
			{"/0", heartbeat.PingSuccess, 0},
			{"/start", heartbeat.PingStart, -1},
			{"/log", heartbeat.PingLog, -1},
			{"/fail", heartbeat.PingFail, -1},
			{"/1", heartbeat.PingFail, 1},
			{"/255", heartbeat.PingFail, 255},
		}
		for _, method := range []string{http.MethodHead, http.MethodGet, http.MethodPost} {
			for _, c := range cases {
				rec := p.do(method, p.token+c.suffix, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s /ping/<token>%s: status %d, want 200", method, c.suffix, rec.Code)
				}
				if method != http.MethodHead && rec.Body.String() != "ok" {
					t.Fatalf("%s /ping/<token>%s: body %q, want ok", method, c.suffix, rec.Body.String())
				}
				if got := rec.Header().Get("Ping-Body-Limit"); got != "10000" {
					t.Errorf("%s %s: Ping-Body-Limit = %q, want 10000 (the default)", method, c.suffix, got)
				}
				got := p.log(t)[0]
				if got.Kind != c.kind {
					t.Fatalf("%s /ping/<token>%s logged kind %q, want %q", method, c.suffix, got.Kind, c.kind)
				}
				if (c.exit < 0) != (got.ExitStatus == nil) || (got.ExitStatus != nil && *got.ExitStatus != c.exit) {
					t.Fatalf("%s /ping/<token>%s logged exit status %v, want %d", method, c.suffix, got.ExitStatus, c.exit)
				}
				// Bring it back up for the next case without tripping the
				// per-heartbeat rate limit: the fake clock does not move,
				// so give the bucket room by using a fresh server.
				p = newPingAPIKeep(t, p)
			}
		}
	})
}

// newPingAPIKeep returns a fresh server (fresh rate limiters) over p's store
// and heartbeat.
func newPingAPIKeep(t *testing.T, p *pingAPI) *pingAPI {
	t.Helper()
	q := &pingAPI{s: p.s, orgID: p.orgID, id: p.id, token: p.token}
	fk := clock.NewFake(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	q.srv = httpserver.New(p.s, fk, func(_ context.Context, ev core.Event) { q.events = append(q.events, ev) },
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	q.h = q.srv.Handler()
	return q
}

// TestPingAPIFailureAndRecovery: a failure takes the heartbeat down at once
// and nudges delivery of the heartbeat.missed it recorded; a success brings
// it back with heartbeat.recovered.
func TestPingAPIFailureAndRecovery(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-fr")
		wantOK(t, p.do(http.MethodGet, p.token, ""), "success")
		wantOK(t, p.do(http.MethodPost, p.token+"/3", "pg_dump: connection refused\n"), "exit 3")
		if hb := p.hb(t); hb.Status != "down" {
			t.Fatalf("after /3: status %q, want down", hb.Status)
		}
		if len(p.events) != 1 || p.events[0].Type != heartbeat.EventMissed || p.events[0].Payload != "nightly" {
			t.Fatalf("dispatched %+v, want one heartbeat.missed for nightly", p.events)
		}
		d, ok := heartbeat.ParseMissDetail(p.events[0].Detail)
		if !ok || d.ExitStatus == nil || *d.ExitStatus != 3 || d.Output != "pg_dump: connection refused" || d.HeartbeatID != p.hb(t).ID {
			t.Fatalf("dispatched detail = %+v", d)
		}
		wantOK(t, p.do(http.MethodGet, p.token+"/fail", ""), "fail while down")
		if len(p.events) != 1 {
			t.Fatalf("a failure while down dispatched %d events, want still 1", len(p.events))
		}
		wantOK(t, p.do(http.MethodGet, p.token+"/0", ""), "exit 0")
		if hb := p.hb(t); hb.Status != "up" {
			t.Fatalf("after /0: status %q, want up", hb.Status)
		}
		if len(p.events) != 2 || p.events[1].Type != heartbeat.EventRecovered || heartbeat.IDOf(p.events[1].Detail) != p.hb(t).ID {
			t.Fatalf("dispatched %+v, want heartbeat.recovered second", p.events)
		}
	})
}

// TestPingAPIErrors: the answers Healthchecks gives to a URL it cannot use,
// with nothing recorded.
func TestPingAPIErrors(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-errors")
		for _, c := range []struct {
			method, path string
			code         int
			body         string
		}{
			{http.MethodGet, p.token + "/256", http.StatusBadRequest, "invalid url format"},
			{http.MethodGet, p.token + "/99999999999999999999", http.StatusBadRequest, "invalid url format"},
			{http.MethodGet, p.token + "/start?rid=not-a-uuid", http.StatusBadRequest, "invalid uuid format"},
			{http.MethodGet, p.token + "?rid=", http.StatusBadRequest, "invalid uuid format"},
			{http.MethodGet, p.token + "/start/extra", http.StatusNotFound, "404 page not found"},
			{http.MethodGet, p.token + "/a/b/c", http.StatusNotFound, "404 page not found"},
			{http.MethodGet, "", http.StatusNotFound, "404 page not found"},
			{http.MethodGet, "nope", http.StatusNotFound, "404 page not found"},
			{http.MethodGet, "nope/start", http.StatusNotFound, "404 page not found"},
			{http.MethodPost, "nope/log", http.StatusNotFound, "404 page not found"},
			{http.MethodGet, "nope/7", http.StatusNotFound, "404 page not found"},
		} {
			rec := p.do(c.method, c.path, "")
			if rec.Code != c.code || strings.TrimSpace(rec.Body.String()) != c.body {
				t.Errorf("%s /ping/%s: %d %q, want %d %q", c.method, c.path, rec.Code, rec.Body.String(), c.code, c.body)
			}
		}
		rec := p.do(http.MethodPut, p.token, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("PUT: status %d, want 405", rec.Code)
		}
		if n := len(p.log(t)); n != 0 {
			t.Fatalf("%d pings logged by refused requests, want 0", n)
		}
	})
}

// TestPingAPIRunIDs: ?rid= pairs a start with its completion, also for
// concurrent runs, and is stored in canonical lower case.
func TestPingAPIRunIDs(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-rids")
		const ridA, ridB = "728B3763-EA80-4113-9FC0-F49B3ADF226A", "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55"
		wantOK(t, p.do(http.MethodGet, p.token+"/start?rid="+ridA, ""), "start A")
		wantOK(t, p.do(http.MethodGet, p.token+"/start?rid="+ridB, ""), "start B")
		if hb := p.hb(t); hb.StartedRID != ridB {
			t.Fatalf("running rid = %q, want %q", hb.StartedRID, ridB)
		}
		wantOK(t, p.do(http.MethodGet, p.token+"?rid="+ridA, ""), "A done")
		ps := p.log(t)
		if ps[0].RID != strings.ToLower(ridA) || ps[2].RID != strings.ToLower(ridA) {
			t.Fatalf("logged rids %q / %q, want %q in lower case", ps[0].RID, ps[2].RID, ridA)
		}
		if ps[0].Duration == nil {
			t.Fatal("run A's completion has no duration")
		}
		if hb := p.hb(t); hb.StartedRID != ridB {
			t.Fatalf("after A finished, running rid = %q, want B still", hb.StartedRID)
		}
		wantOK(t, p.do(http.MethodGet, p.token+"/0?rid="+ridB, ""), "B done")
		if hb := p.hb(t); !hb.StartedAt.IsZero() {
			t.Fatal("after B finished a run is still being timed")
		}
	})
}

// TestPingAPIBody: a POST body is logged with the ping, cleaned to valid
// UTF-8 without NULs, the ping's own credential redacted, and cut at 100 KB
// on a character boundary; a GET has none.
func TestPingAPIBody(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "0123456789abcdef0123456789abcdef")
		wantOK(t, p.do(http.MethodPost, p.token+"/log", "line 1\nline 2\n"), "log")
		if got := p.log(t)[0].Body; got != "line 1\nline 2\n" {
			t.Fatalf("body = %q", got)
		}

		wantOK(t, p.do(http.MethodPost, p.token, "a\x00b\xffc curl https://tend/ping/"+p.token), "dirty body")
		if got := p.log(t)[0].Body; got != "ab�c curl https://tend/ping/***" {
			t.Fatalf("cleaned body = %q", got)
		}

		// The default keeps 10 kB, cut on a character boundary.
		big := strings.Repeat("é", 60_000) // 120 000 bytes
		p = newPingAPIKeep(t, p)
		wantOK(t, p.do(http.MethodPost, p.token+"/fail", big), "big body")
		got := p.log(t)[0].Body
		body, marker := strings.CutSuffix(got, httpserver.PingBodyTruncated)
		if !utf8.ValidString(got) || !marker || len(body) > httpserver.DefaultPingBodyLimit || len(body) < httpserver.DefaultPingBodyLimit-1 {
			t.Fatalf("capped body: %d bytes, marker %v; want %d (or one less) and the marker", len(body), marker, httpserver.DefaultPingBodyLimit)
		}

		// TEND_PING_BODY_KB raises it up to Healthchecks' 100 kB.
		p = newPingAPIKeep(t, p)
		p.srv.SetPingBodyLimit(httpserver.MaxPingBodyLimit)
		rec := p.do(http.MethodPost, p.token+"/fail", big)
		if got := rec.Header().Get("Ping-Body-Limit"); got != "100000" {
			t.Fatalf("Ping-Body-Limit = %q, want 100000", got)
		}
		got = p.log(t)[0].Body
		body, marker = strings.CutSuffix(got, httpserver.PingBodyTruncated)
		if !marker || len(body) > httpserver.MaxPingBodyLimit || len(body) < httpserver.MaxPingBodyLimit-1 {
			t.Fatalf("body capped at 100 kB: %d bytes, marker %v", len(body), marker)
		}

		// A token cut in two by the cap is not left half in the log.
		p = newPingAPIKeep(t, p)
		straddle := strings.Repeat("x", httpserver.DefaultPingBodyLimit-10) + p.token + strings.Repeat("y", 50)
		wantOK(t, p.do(http.MethodPost, p.token+"/log", straddle), "token across the cap")
		if got := p.log(t)[0].Body; strings.Contains(got, p.token[:8]) || !strings.HasSuffix(got, httpserver.PingBodyTruncated) {
			t.Fatalf("a token straddling the cap left %q in the log", got[len(got)-60:])
		}

		// With a limit of 0 no body is kept.
		p = newPingAPIKeep(t, p)
		p.srv.SetPingBodyLimit(0)
		wantOK(t, p.do(http.MethodPost, p.token+"/log", "secret-ish"), "limit 0")
		if got := p.log(t)[0].Body; got != "" {
			t.Fatalf("limit 0 kept %q", got)
		}
		p.srv.SetPingBodyLimit(httpserver.DefaultPingBodyLimit)

		wantOK(t, p.do(http.MethodGet, p.token+"/log", ""), "no body")
		if got := p.log(t)[0].Body; got != "" {
			t.Fatalf("GET logged body %q, want none", got)
		}
	})
}

// TestPingAPIUUIDToken: a heartbeat imported from Healthchecks keeps its UUID
// as its token, and is pinged in any letter case.
func TestPingAPIUUIDToken(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		const uuid = "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278"
		p := newPingAPI(t, s, uuid)
		wantOK(t, p.do(http.MethodGet, uuid, ""), "lower case")
		wantOK(t, p.do(http.MethodPost, strings.ToUpper(uuid)+"/start", "curl https://tend/ping/"+strings.ToUpper(uuid)), "upper case")
		if n := len(p.log(t)); n != 2 {
			t.Fatalf("logged %d pings, want 2", n)
		}
		if got := p.log(t)[0].Body; got != "curl https://tend/ping/***" {
			t.Fatalf("an upper-case UUID token was not redacted: %q", got)
		}
	})
}

// TestPingAPIRateLimitsUnchanged: every variant counts against the one
// per-heartbeat budget (a burst of 10), and unknown tokens on any variant
// count against the address's miss budget (a burst of 30).
func TestPingAPIRateLimitsUnchanged(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "busy", "tok-busy", "up")
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()
	send := func(path, addr string) int {
		req := httptest.NewRequest(http.MethodGet, "/ping/"+path, nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	suffixes := []string{"", "/start", "/0", "/log", "/fail"}
	for i := 0; i < 10; i++ {
		if code := send("tok-busy"+suffixes[i%len(suffixes)], "198.51.100.1:1"); code != http.StatusOK {
			t.Fatalf("ping %d: %d, want 200", i+1, code)
		}
	}
	if code := send("tok-busy/start", "198.51.100.2:1"); code != http.StatusTooManyRequests {
		t.Fatalf("11th ping for the heartbeat, from another address: %d, want 429", code)
	}

	for i := 0; i < 30; i++ {
		if code := send(fmt.Sprintf("guess-%d%s", i, suffixes[i%len(suffixes)]), "203.0.113.5:1"); code != http.StatusNotFound {
			t.Fatalf("guess %d: %d, want 404", i+1, code)
		}
	}
	if code := send("guess-next/fail", "203.0.113.5:1"); code != http.StatusTooManyRequests {
		t.Fatalf("31st miss from one address: %d, want 429", code)
	}
}
