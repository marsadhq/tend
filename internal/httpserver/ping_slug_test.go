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

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/store"
)

const testPingKey = "fqOOd6-F4MMNuCEnzTU01w"

// TestSlugPings: with an org ping key, /ping/<key>/<slug> reaches the
// heartbeat named <slug>, with every action of the token form.
func TestSlugPings(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-slug")
		if err := s.SetOrgPingKey(context.Background(), p.orgID, testPingKey); err != nil {
			t.Fatalf("SetOrgPingKey: %v", err)
		}
		for _, c := range []struct {
			suffix, kind string
		}{
			{"", heartbeat.PingSuccess},
			{"/", heartbeat.PingSuccess},
			{"/start", heartbeat.PingStart},
			{"/0", heartbeat.PingSuccess},
			{"/log", heartbeat.PingLog},
			{"/fail", heartbeat.PingFail},
			{"/7", heartbeat.PingFail},
		} {
			wantOK(t, p.do(http.MethodPost, testPingKey+"/nightly"+c.suffix, "body with "+testPingKey), "slug"+c.suffix)
			got := p.log(t)[0]
			if got.Kind != c.kind {
				t.Fatalf("slug%s logged %q, want %q", c.suffix, got.Kind, c.kind)
			}
			if strings.Contains(got.Body, testPingKey) {
				t.Fatalf("slug%s logged the ping key: %q", c.suffix, got.Body)
			}
		}
		if hb := p.hb(t); hb.Status != "down" {
			t.Fatalf("after /7 by slug: status %q, want down", hb.Status)
		}

		// A name with characters that need escaping is one segment.
		ctx := context.Background()
		if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: p.orgID, Name: "db/backup nightly", Token: "tok-escaped", PeriodSeconds: 60}); err != nil {
			t.Fatal(err)
		}
		p = newPingAPIKeep(t, p)
		wantOK(t, p.do(http.MethodGet, testPingKey+"/db%2Fbackup%20nightly/start", ""), "escaped slug")
		hb, err := s.GetHeartbeatByName(ctx, p.orgID, "db/backup nightly")
		if err != nil || hb.StartedAt.IsZero() {
			t.Fatalf("escaped slug ping did not reach its heartbeat: %+v, %v", hb, err)
		}
	})
}

// TestSlugPingErrors: an unknown key or slug is a 404 that counts against the
// address's miss budget; a slug that would be created must look like one.
func TestSlugPingErrors(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-slug-errors")
		if err := s.SetOrgPingKey(context.Background(), p.orgID, testPingKey); err != nil {
			t.Fatalf("SetOrgPingKey: %v", err)
		}
		for _, c := range []struct {
			path string
			code int
			body string
		}{
			{"wrong-key/nightly", http.StatusNotFound, "not found"},
			{"wrong-key/nightly/start", http.StatusNotFound, "not found"},
			{testPingKey + "/no-such-check", http.StatusNotFound, "not found"},
			{testPingKey + "/no-such-check/fail", http.StatusNotFound, "not found"},
			{testPingKey + "/nightly/256", http.StatusBadRequest, "invalid url format"},
			{testPingKey + "/nightly/bogus", http.StatusNotFound, "not found"},
			{testPingKey + "/Not-A-Slug?create=1", http.StatusBadRequest, "invalid url format"},
		} {
			rec := p.do(http.MethodGet, c.path, "")
			if rec.Code != c.code || rec.Body.String() != c.body {
				t.Errorf("/ping/%s: %d %q, want %d %q", c.path, rec.Code, rec.Body.String(), c.code, c.body)
			}
		}
	})

	// Misses on the slug forms count per address like unknown tokens.
	ts := newStore(t)
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()
	for i := 0; i < 31; i++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ping/key-%d/slug", i), nil)
		req.RemoteAddr = "203.0.113.9:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := http.StatusNotFound
		if i == 30 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("slug guess %d: %d, want %d", i+1, rec.Code, want)
		}
	}
}

// TestSlugPingCreate: ?create=1 provisions a missing heartbeat named after the
// slug with Healthchecks' defaults (a day's period, an hour's grace), records
// the ping and answers 201 Created; once it exists, the same ping is a 200.
func TestSlugPingCreate(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		p := newPingAPI(t, s, "tok-create")
		ctx := context.Background()
		if err := s.SetOrgPingKey(ctx, p.orgID, testPingKey); err != nil {
			t.Fatalf("SetOrgPingKey: %v", err)
		}
		rec := p.do(http.MethodGet, testPingKey+"/db-backup/start?create=1", "")
		if rec.Code != http.StatusCreated || rec.Body.String() != "Created" {
			t.Fatalf("create: %d %q, want 201 Created", rec.Code, rec.Body.String())
		}
		hb, err := s.GetHeartbeatByName(ctx, p.orgID, "db-backup")
		if err != nil {
			t.Fatalf("auto-created heartbeat: %v", err)
		}
		if hb.PeriodSeconds != 86400 || hb.GraceSeconds != 3600 || hb.StartedAt.IsZero() || len(hb.Token) != 32 {
			t.Fatalf("auto-created heartbeat = %+v, want period 1d, grace 1h, the start recorded and a fresh token", hb)
		}
		wantOK(t, p.do(http.MethodGet, testPingKey+"/db-backup?create=1", ""), "existing with create=1")
		ps, err := s.ListHeartbeatPings(ctx, p.orgID, hb.ID, 10)
		if err != nil || len(ps) != 2 {
			t.Fatalf("pings of the created heartbeat = %v, %v; want 2", ps, err)
		}
		// Without a key, nothing can be created.
		if rec := p.do(http.MethodGet, "no-key-here/other?create=1", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("create under an unknown key: %d, want 404", rec.Code)
		}
	})
}

// TestSlugAndTokenShareTheRateLimit: the slug form counts against the same
// per-heartbeat budget as the token form.
func TestSlugAndTokenShareTheRateLimit(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "shared", "tok-shared", "up")
	if err := ts.store.SetOrgPingKey(context.Background(), ts.orgID, testPingKey); err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := httpserver.New(ts.store, fk, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler()
	send := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, "/ping/"+path, nil)
		req.RemoteAddr = "198.51.100.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 10; i++ {
		path := "tok-shared"
		if i%2 == 0 {
			path = testPingKey + "/shared"
		}
		if code := send(path); code != http.StatusOK {
			t.Fatalf("ping %d: %d", i+1, code)
		}
	}
	if code := send(testPingKey + "/shared/start"); code != http.StatusTooManyRequests {
		t.Fatalf("11th ping by slug: %d, want 429", code)
	}
}
