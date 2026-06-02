package httpserver_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the raw seed/read handle

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/store"
)

// capture is a mutex-guarded recorder for dispatched events.
type capture struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *capture) dispatch(_ context.Context, ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *capture) all() []core.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.Event, len(c.events))
	copy(out, c.events)
	return out
}

// testStore bundles a migrated store with a raw handle to the same underlying
// SQLite file, used to seed and inspect heartbeat rows directly (the server's
// public API does not create heartbeats - that is Task 7).
type testStore struct {
	store store.Store
	raw   *sql.DB
	orgID int64
}

// newStore opens a fresh, migrated SQLite store with the default org
// bootstrapped, plus a raw *sql.DB on the same file for seeding/inspection.
func newStore(t *testing.T) *testStore {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return &testStore{store: s, raw: raw, orgID: org.ID}
}

// seedHB inserts a heartbeat row directly so the server tests do not depend on a
// heartbeat package (Task 7).
func (ts *testStore) seedHB(t *testing.T, name, token, status string) {
	t.Helper()
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO heartbeats (org_id, name, token, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		ts.orgID, name, token, status, created,
	); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
}

func (ts *testStore) rawHB(t *testing.T, token string) (status, lastSeen string) {
	t.Helper()
	var ls *string
	if err := ts.raw.QueryRowContext(context.Background(),
		`SELECT status, last_seen_at FROM heartbeats WHERE token = ?`, token,
	).Scan(&status, &ls); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if ls != nil {
		lastSeen = *ls
	}
	return status, lastSeen
}

func startServer(t *testing.T, s store.Store, dispatch func(context.Context, core.Event)) *httptest.Server {
	t.Helper()
	srv := httpserver.New(s, clock.RealClock{}, dispatch, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthz(t *testing.T) {
	ts := newStore(t)
	srv := startServer(t, ts.store, nil)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
}

func TestPingKnownNoRecovery(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "api", "tok-known", "up")
	cap := &capture{}
	srv := startServer(t, ts.store, cap.dispatch)

	resp, err := http.Post(srv.URL+"/ping/tok-known", "", nil)
	if err != nil {
		t.Fatalf("POST /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	status, lastSeen := ts.rawHB(t, "tok-known")
	if status != "up" {
		t.Fatalf("status: got %q want up", status)
	}
	if lastSeen == "" {
		t.Fatal("last_seen_at not updated")
	}
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("dispatch: got %d events want 0 (no recovery)", len(got))
	}
	ts.assertNoRecoveredEvent(t)
}

func TestPingNewHeartbeatNoRecovery(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "fresh", "tok-fresh", "new")
	cap := &capture{}
	srv := startServer(t, ts.store, cap.dispatch)

	resp, err := http.Post(srv.URL+"/ping/tok-fresh", "", nil)
	if err != nil {
		t.Fatalf("POST /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	status, _ := ts.rawHB(t, "tok-fresh")
	if status != "up" {
		t.Fatalf("status: got %q want up", status)
	}
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("dispatch: got %d events want 0 (new is not a recovery)", len(got))
	}
}

func TestPingUnknownToken(t *testing.T) {
	ts := newStore(t)
	srv := startServer(t, ts.store, nil)

	resp, err := http.Post(srv.URL+"/ping/nope", "", nil)
	if err != nil {
		t.Fatalf("POST /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
}

func TestPingGetConvenience(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "api", "tok-get", "up")
	srv := startServer(t, ts.store, nil)

	resp, err := http.Get(srv.URL + "/ping/tok-get")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
}

func TestPingRecovery(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "criticaljob", "tok-rec", "down")
	cap := &capture{}
	srv := startServer(t, ts.store, cap.dispatch)

	resp, err := http.Post(srv.URL+"/ping/tok-rec", "", nil)
	if err != nil {
		t.Fatalf("POST /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	status, _ := ts.rawHB(t, "tok-rec")
	if status != "up" {
		t.Fatalf("status: got %q want up", status)
	}

	// Event recorded in the store.
	events, err := ts.store.ListEvents(context.Background(), ts.orgID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found *core.Event
	for i := range events {
		if events[i].Type == "heartbeat.recovered" {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no heartbeat.recovered event in store")
	}
	if found.Payload != "criticaljob" {
		t.Fatalf("event payload: got %q want %q", found.Payload, "criticaljob")
	}
	if found.OrgID != ts.orgID {
		t.Fatalf("event org: got %d want %d", found.OrgID, ts.orgID)
	}

	// Dispatched to the sink.
	disp := cap.all()
	if len(disp) != 1 {
		t.Fatalf("dispatch: got %d events want 1", len(disp))
	}
	if disp[0].Type != "heartbeat.recovered" || disp[0].Payload != "criticaljob" {
		t.Fatalf("dispatched event: got %+v", disp[0])
	}
}

func TestPingRecoveryNilDispatch(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "nodispatch", "tok-nil", "down")
	srv := startServer(t, ts.store, nil) // nil dispatch must not panic

	resp, err := http.Post(srv.URL+"/ping/tok-nil", "", nil)
	if err != nil {
		t.Fatalf("POST /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	events, err := ts.store.ListEvents(context.Background(), ts.orgID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Type == "heartbeat.recovered" {
			found = true
		}
	}
	if !found {
		t.Fatal("recovered event not stored when dispatch is nil")
	}
}

func (ts *testStore) assertNoRecoveredEvent(t *testing.T) {
	t.Helper()
	events, err := ts.store.ListEvents(context.Background(), ts.orgID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range events {
		if e.Type == "heartbeat.recovered" {
			t.Fatal("unexpected heartbeat.recovered event")
		}
	}
}
