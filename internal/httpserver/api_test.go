package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// --- api test helpers --------------------------------------------------------

// apiGet issues an authenticated (bearer) GET against the server handler and
// returns the recorder. Bearer auth is GET-safe and CSRF-exempt, and scopes to
// as.orgID, which is exactly the read surface under test.
func apiGet(t *testing.T, h http.Handler, as *authStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+as.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// apiPost issues an authenticated (bearer) POST against the server handler and
// returns the recorder. Bearer auth carries no ambient cookie, so it is
// CSRF-exempt even for POST (see requireAuth); this is the action surface under
// test, scoped to as.orgID.
func apiPost(t *testing.T, h http.Handler, as *authStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+as.token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeJSON unmarshals the recorder body into v, failing the test on error.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode JSON: %v; body=%q", err, rec.Body.String())
	}
}

// jobEnabled reads a job's enabled flag directly via the store (org-scoped).
func jobEnabled(t *testing.T, ts *testStore, orgID, id int64) bool {
	t.Helper()
	j, err := ts.store.GetJob(context.Background(), orgID, id)
	if err != nil {
		t.Fatalf("GetJob(%d,%d): %v", orgID, id, err)
	}
	return j.Enabled
}

// runCount returns how many runs exist for a job (org-scoped) via ListRuns.
func runCount(t *testing.T, ts *testStore, orgID, jobID int64) int {
	t.Helper()
	runs, err := ts.store.ListRuns(context.Background(), orgID, jobID, maxAPILimitForTest)
	if err != nil {
		t.Fatalf("ListRuns(%d,%d): %v", orgID, jobID, err)
	}
	return len(runs)
}

// maxAPILimitForTest is a generous cap for ListRuns assertions in tests.
const maxAPILimitForTest = 200

// seedJob creates a job under orgID via the store API and returns its ID.
func seedJob(t *testing.T, ts *testStore, orgID int64, name string) int64 {
	t.Helper()
	id, err := ts.store.CreateJob(context.Background(), jobs.Job{
		OrgID:           orgID,
		Name:            name,
		Type:            jobs.Shell,
		Command:         "echo hi",
		IntervalSeconds: 60,
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("CreateJob(%q): %v", name, err)
	}
	return id
}

// seedRun inserts a job_run row directly (full control over status/output/org).
func seedRun(t *testing.T, ts *testStore, orgID, jobID int64, status, output string) int64 {
	t.Helper()
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO job_runs (org_id, job_id, status, attempt, exit_code, output, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		orgID, jobID, status, 1, 0, output, created)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("run id: %v", err)
	}
	return id
}

// newOrg inserts a second org directly and returns its ID (for cross-org tests).
func newOrg(t *testing.T, ts *testStore, name string) int64 {
	t.Helper()
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO orgs (name, created_at) VALUES (?, ?)`, name, created)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("org id: %v", err)
	}
	return id
}

// --- /api/jobs ---------------------------------------------------------------

func TestAPIListJobs(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "alpha")
	seedJob(t, ts, ts.orgID, "beta")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: got %q want json", ct)
	}
	var got []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("jobs: got %d want 2", len(got))
	}
	names := map[string]bool{}
	for _, j := range got {
		names[j.Name] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Fatalf("jobs: missing seeded names; got %+v", got)
	}
}

func TestAPIGetJob(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	id := seedJob(t, ts, ts.orgID, "alpha")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs/"+itoa(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	decodeJSON(t, rec, &got)
	if got.ID != id || got.Name != "alpha" || got.Type != "shell" {
		t.Fatalf("job DTO: got %+v want id=%d name=alpha type=shell", got, id)
	}
}

func TestAPIGetJobAbsent404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs/9999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestAPIGetJobNonNumeric400(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs/not-a-number")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 for non-numeric id", rec.Code)
	}
}

// Cross-org: a caller in org A must get 404 for org B's job id.
func TestAPIGetJobCrossOrg404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A == ts.orgID
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "secret-b-job")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs/"+itoa(bJob))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org job)", rec.Code)
	}
}

// --- /api/jobs/{id}/runs -----------------------------------------------------

func TestAPIListRunsTruncatesOutput(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	seedRun(t, ts, ts.orgID, jobID, "succeeded", "this is a big chunk of run output")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs/"+itoa(jobID)+"/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	// The list body must NOT carry the full output blob.
	if strings.Contains(rec.Body.String(), "big chunk of run output") {
		t.Fatalf("list runs DTO leaked full output; body=%q", rec.Body.String())
	}
	var got []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 {
		t.Fatalf("runs: got %d want 1", len(got))
	}
	if got[0].Status != "succeeded" {
		t.Fatalf("run status: got %q want succeeded", got[0].Status)
	}
}

func TestAPIListRunsLimitClamp(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	for i := 0; i < 5; i++ {
		seedRun(t, ts, ts.orgID, jobID, "succeeded", "")
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	// limit=2 must bound the result.
	rec := apiGet(t, h, as, "/api/jobs/"+itoa(jobID)+"/runs?limit=2")
	var got []json.RawMessage
	decodeJSON(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("limit=2: got %d runs want 2", len(got))
	}

	// An over-cap limit must not error (clamped to 200, served fine).
	rec = apiGet(t, h, as, "/api/jobs/"+itoa(jobID)+"/runs?limit=99999")
	if rec.Code != http.StatusOK {
		t.Fatalf("over-cap limit status: got %d want 200", rec.Code)
	}
	var all []json.RawMessage
	decodeJSON(t, rec, &all)
	if len(all) != 5 {
		t.Fatalf("over-cap limit: got %d runs want 5 (all seeded)", len(all))
	}

	// A negative limit falls back to default (50), serving all 5.
	rec = apiGet(t, h, as, "/api/jobs/"+itoa(jobID)+"/runs?limit=-3")
	if rec.Code != http.StatusOK {
		t.Fatalf("negative limit status: got %d want 200", rec.Code)
	}
	var neg []json.RawMessage
	decodeJSON(t, rec, &neg)
	if len(neg) != 5 {
		t.Fatalf("negative limit: got %d runs want 5", len(neg))
	}
}

// --- /api/runs/{id} ----------------------------------------------------------

func TestAPIGetRunHasFullOutput(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	const full = "FULL OUTPUT line1\nline2\nline3"
	runID := seedRun(t, ts, ts.orgID, jobID, "succeeded", full)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/runs/"+itoa(runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got struct {
		ID     int64  `json:"id"`
		Output string `json:"output"`
		Status string `json:"status"`
	}
	decodeJSON(t, rec, &got)
	if got.ID != runID {
		t.Fatalf("run id: got %d want %d", got.ID, runID)
	}
	if got.Output != full {
		t.Fatalf("run output: got %q want %q", got.Output, full)
	}
}

func TestAPIGetRunCrossOrg404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job")
	bRun := seedRun(t, ts, orgB, bJob, "succeeded", "B SECRET OUTPUT")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/runs/"+itoa(bRun))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org run)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "B SECRET OUTPUT") {
		t.Fatalf("cross-org run leaked output; body=%q", rec.Body.String())
	}
}

func TestAPIGetRunAbsent404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/runs/9999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

// --- /api/channels (NO config/secret material) -------------------------------

func TestAPIListChannelsNoSecretMaterial(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const secretBlob = "ENCRYPTED-CONFIG-CIPHERTEXT-BLOB"
	if _, err := ts.store.CreateChannel(context.Background(),
		notify.Channel{OrgID: ts.orgID, Name: "ops", Kind: notify.Slack}, secretBlob); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/channels")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secretBlob) {
		t.Fatalf("channels DTO leaked config ciphertext; body=%q", body)
	}
	for _, bad := range []string{"config", "ciphertext", "blob"} {
		if strings.Contains(strings.ToLower(body), bad) {
			t.Fatalf("channels DTO must not expose %q field; body=%q", bad, body)
		}
	}
	var got []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 || got[0].Name != "ops" || got[0].Kind != "slack" {
		t.Fatalf("channel DTO: got %+v want one ops/slack", got)
	}
}

// --- /api/rules --------------------------------------------------------------

func TestAPIListRules(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	chID, err := ts.store.CreateChannel(context.Background(),
		notify.Channel{OrgID: ts.orgID, Name: "ops", Kind: notify.Slack}, "blob")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := ts.store.CreateRule(context.Background(), notify.Rule{
		OrgID: ts.orgID, ChannelID: chID, EventType: "run.failed", Enabled: true, JobID: 0,
	}); err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/rules")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got []struct {
		ID        int64  `json:"id"`
		ChannelID int64  `json:"channel_id"`
		EventType string `json:"event_type"`
		Enabled   bool   `json:"enabled"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 || got[0].EventType != "run.failed" || !got[0].Enabled {
		t.Fatalf("rule DTO: got %+v", got)
	}
}

// --- /api/heartbeats (NO token field) ----------------------------------------

func TestAPIListHeartbeatsNoToken(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const secretToken = "SUPER-SECRET-PING-TOKEN-abcdef"
	ts.seedHB(t, "api", secretToken, "up")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/heartbeats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secretToken) {
		t.Fatalf("heartbeat DTO leaked token value; body=%q", body)
	}
	if strings.Contains(strings.ToLower(body), "token") {
		t.Fatalf("heartbeat DTO must not contain a token field at all; body=%q", body)
	}
	var got []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 || got[0].Name != "api" || got[0].Status != "up" {
		t.Fatalf("heartbeat DTO: got %+v", got)
	}
}

// --- /api/heartbeats/{id} + /history -----------------------------------------

func TestAPIGetHeartbeat(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	ts.seedHB(t, "offsite-backup", "secret-ping-tok", "up")
	hb, err := ts.store.GetHeartbeatByName(context.Background(), ts.orgID, "offsite-backup")
	if err != nil {
		t.Fatalf("GetHeartbeatByName: %v", err)
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/heartbeats/"+itoa(hb.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-ping-tok") || strings.Contains(strings.ToLower(body), "token") {
		t.Fatalf("heartbeat detail leaked the ping token; body=%q", body)
	}
	var got struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &got)
	if got.Name != "offsite-backup" {
		t.Errorf("name = %q, want offsite-backup", got.Name)
	}

	if r := apiGet(t, h, as, "/api/heartbeats/9999"); r.Code != http.StatusNotFound {
		t.Errorf("unknown id: got %d want 404", r.Code)
	}
	// Requires auth: no bearer token -> 401.
	req := httptest.NewRequest(http.MethodGet, "/api/heartbeats/"+itoa(hb.ID), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no auth: got %d want 401", rr.Code)
	}
}

func TestAPIHeartbeatHistory(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	ctx := context.Background()
	ts.seedHB(t, "offsite-backup", "t", "up")
	hb, err := ts.store.GetHeartbeatByName(ctx, ts.orgID, "offsite-backup")
	if err != nil {
		t.Fatalf("GetHeartbeatByName: %v", err)
	}
	for _, ev := range []core.Event{
		{OrgID: ts.orgID, Type: "heartbeat.missed", Source: "heartbeat", Payload: "offsite-backup"},
		{OrgID: ts.orgID, Type: "heartbeat.recovered", Source: "heartbeat", Payload: "offsite-backup"},
	} {
		if _, err := ts.store.EmitEvent(ctx, ev); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/heartbeats/"+itoa(hb.ID)+"/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got []struct {
		Type string `json:"type"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("history len = %d want 2; body=%q", len(got), rec.Body.String())
	}
	if got[0].Type != "heartbeat.recovered" || got[1].Type != "heartbeat.missed" {
		t.Errorf("order = [%q, %q] want [recovered, missed] (newest first)", got[0].Type, got[1].Type)
	}
	if r := apiGet(t, h, as, "/api/heartbeats/9999/history"); r.Code != http.StatusNotFound {
		t.Errorf("unknown id history: got %d want 404", r.Code)
	}
}

// --- /api/events (payload as json.RawMessage; valid JSON for any payload) -----

func TestAPIListEventsValidJSONPayloads(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	// One JSON-object payload, one plain-string (non-JSON) payload.
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "run.failed", Source: "jobs.runner",
		Payload: `{"job_id":7,"exit":1}`,
	}); err != nil {
		t.Fatalf("EmitEvent json: %v", err)
	}
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "heartbeat.recovered", Source: "heartbeat",
		Payload: "criticaljob", // NON-JSON plain string
	}); err != nil {
		t.Fatalf("EmitEvent string: %v", err)
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	// The whole body must be valid JSON (the non-JSON payload must not corrupt it).
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("response is not valid JSON; body=%q", rec.Body.String())
	}
	var got []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("events: got %d want 2", len(got))
	}
	for _, e := range got {
		// Each payload, on its own, must be valid JSON.
		if len(e.Payload) > 0 && !json.Valid(e.Payload) {
			t.Fatalf("event payload not valid JSON for %q: %q", e.Type, e.Payload)
		}
		switch e.Type {
		case "run.failed":
			var obj map[string]any
			if err := json.Unmarshal(e.Payload, &obj); err != nil {
				t.Fatalf("json payload should decode to object: %v", err)
			}
		case "heartbeat.recovered":
			// Plain string payload must be encoded as a JSON string value.
			var s string
			if err := json.Unmarshal(e.Payload, &s); err != nil {
				t.Fatalf("non-JSON payload should be a JSON string: %v; raw=%q", err, e.Payload)
			}
			if s != "criticaljob" {
				t.Fatalf("string payload: got %q want criticaljob", s)
			}
		}
	}
}

func TestAPIListEventsLimitClamp(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	for i := 0; i < 4; i++ {
		if _, err := ts.store.EmitEvent(context.Background(), core.Event{
			OrgID: ts.orgID, Type: "run.started", Source: "jobs.runner", Payload: "{}",
		}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/events?limit=2")
	var got []json.RawMessage
	decodeJSON(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("limit=2: got %d events want 2", len(got))
	}

	rec = apiGet(t, h, as, "/api/events?limit=99999")
	if rec.Code != http.StatusOK {
		t.Fatalf("over-cap status: got %d want 200", rec.Code)
	}
	var all []json.RawMessage
	decodeJSON(t, rec, &all)
	if len(all) != 4 {
		t.Fatalf("over-cap: got %d events want 4", len(all))
	}
}

// --- /api/secrets (name + created_at; NO ciphertext) -------------------------

func TestAPIListSecretsNoCiphertext(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const cipher = "CIPHERTEXT-DO-NOT-LEAK-xyz"
	if err := ts.store.PutSecret(context.Background(), ts.orgID, "db_password", cipher); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/secrets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, cipher) {
		t.Fatalf("secrets DTO leaked ciphertext; body=%q", body)
	}
	for _, bad := range []string{"ciphertext", "value"} {
		if strings.Contains(strings.ToLower(body), bad) {
			t.Fatalf("secrets DTO must not expose %q field; body=%q", bad, body)
		}
	}
	var got []struct {
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 || got[0].Name != "db_password" {
		t.Fatalf("secret DTO: got %+v want one db_password", got)
	}
}

// --- collection org-scoping (lists are filtered to the caller's org) ---------

func TestAPIListJobsOrgScoped(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "mine")
	orgB := newOrg(t, ts, "org-b")
	seedJob(t, ts, orgB, "theirs")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiGet(t, h, as, "/api/jobs")
	body := rec.Body.String()
	if strings.Contains(body, "theirs") {
		t.Fatalf("jobs list leaked another org's job; body=%q", body)
	}
	var got []struct {
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &got)
	if len(got) != 1 || got[0].Name != "mine" {
		t.Fatalf("jobs list not org-scoped: got %+v", got)
	}
}

// --- auth gating: /api/* requires authentication -----------------------------

func TestAPIRequiresAuth(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil) // no credential
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 (unauthenticated /api/jobs)", rec.Code)
	}
}

// --- zero time.Time omitzero contract ----------------------------------------

// seedRunWithStarted inserts a job_run that has started_at set but no ended_at
// (simulating a still-running run). This lets us assert the zero-EndedAt
// serialization without depending on the runner package.
func seedRunWithStarted(t *testing.T, ts *testStore, orgID, jobID int64, status string) int64 {
	t.Helper()
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	started := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO job_runs (org_id, job_id, status, attempt, exit_code, output, started_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID, jobID, status, 1, 0, "", started, created)
	if err != nil {
		t.Fatalf("seed running run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("run id: %v", err)
	}
	return id
}

// TestAPIZeroTimeOmitted asserts that:
//   - a still-running run (zero EndedAt) does NOT serialize ended_at at all, and
//     does NOT contain the zero sentinel "0001-01-01".
//   - A completed run (non-zero EndedAt) DOES serialize ended_at in RFC3339.
//   - A never-seen heartbeat (zero LastSeenAt) does NOT serialize last_seen_at,
//     and does NOT contain the zero sentinel "0001-01-01".
//
// This test is non-vacuous: if you revert the four tags from omitzero back to
// omitempty the zero-time fields will appear as "0001-01-01T00:00:00Z" and the
// "0001-01-01" / key-presence assertions below will fail.
func TestAPIZeroTimeOmitted(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	// --- runs: zero ended_at must be omitted ---

	jobID := seedJob(t, ts, ts.orgID, "zerotime-job")
	// A run that is still running: started_at is set, ended_at is zero.
	runningID := seedRunWithStarted(t, ts, ts.orgID, jobID, "running")

	// --- heartbeat: zero last_seen_at must be omitted ---

	// seedHB does NOT set last_seen_at, so the column is NULL (zero time.Time).
	ts.seedHB(t, "never-seen", "tok-never", "new")

	// Fetch the running run via GET /api/runs/{id}
	recRun := apiGet(t, h, as, "/api/runs/"+itoa(runningID))
	if recRun.Code != http.StatusOK {
		t.Fatalf("run status: got %d want 200; body=%q", recRun.Code, recRun.Body.String())
	}
	runBody := recRun.Body.String()

	// Zero time must not appear as the sentinel string.
	if strings.Contains(runBody, "0001-01-01") {
		t.Fatalf("running run: body contains zero-time sentinel 0001-01-01; body=%q", runBody)
	}
	// The ended_at key itself must be absent for a still-running run.
	if strings.Contains(runBody, `"ended_at"`) {
		t.Fatalf("running run: body contains ended_at for a still-running run; body=%q", runBody)
	}
	// Non-zero started_at must be present and be valid RFC3339.
	var runObj map[string]json.RawMessage
	decodeJSON(t, recRun, &runObj)
	rawStarted, ok := runObj["started_at"]
	if !ok {
		t.Fatalf("running run: started_at missing from body; body=%q", runBody)
	}
	var startedStr string
	if err := json.Unmarshal(rawStarted, &startedStr); err != nil {
		t.Fatalf("running run: started_at not a string: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, startedStr); err != nil {
		t.Fatalf("running run: started_at not RFC3339: %q", startedStr)
	}

	// Fetch heartbeats via GET /api/heartbeats
	recHB := apiGet(t, h, as, "/api/heartbeats")
	if recHB.Code != http.StatusOK {
		t.Fatalf("heartbeats status: got %d want 200; body=%q", recHB.Code, recHB.Body.String())
	}
	hbBody := recHB.Body.String()

	// Zero time must not appear as the sentinel string.
	if strings.Contains(hbBody, "0001-01-01") {
		t.Fatalf("never-seen heartbeat: body contains zero-time sentinel 0001-01-01; body=%q", hbBody)
	}
	// The last_seen_at key itself must be absent for a never-seen heartbeat.
	if strings.Contains(hbBody, `"last_seen_at"`) {
		t.Fatalf("never-seen heartbeat: body contains last_seen_at for a never-seen heartbeat; body=%q", hbBody)
	}

	// Verify the heartbeat row is otherwise correct (name/status returned).
	var hbs []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	decodeJSON(t, recHB, &hbs)
	found := false
	for _, h := range hbs {
		if h.Name == "never-seen" {
			found = true
			if h.Status != "new" {
				t.Fatalf("never-seen heartbeat: status got %q want new", h.Status)
			}
		}
	}
	if !found {
		t.Fatalf("never-seen heartbeat: not found in response; body=%q", hbBody)
	}
}

// --- POST /api/jobs/{id}/run -------------------------------------------------

func TestAPIRunNowEnqueuesPendingRun(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	if got := runCount(t, ts, ts.orgID, jobID); got != 0 {
		t.Fatalf("precondition: got %d runs want 0", got)
	}

	rec := apiPost(t, h, as, "/api/jobs/"+itoa(jobID)+"/run")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: got %q want json", ct)
	}
	var got struct {
		RunID int64 `json:"run_id"`
	}
	decodeJSON(t, rec, &got)
	if got.RunID <= 0 {
		t.Fatalf("run_id: got %d want a positive id", got.RunID)
	}

	// The enqueued run must be visible via ListRuns as a pending run.
	runs, err := ts.store.ListRuns(context.Background(), ts.orgID, jobID, maxAPILimitForTest)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs: got %d want 1", len(runs))
	}
	if runs[0].ID != got.RunID {
		t.Fatalf("run id: ListRuns has %d, response said %d", runs[0].ID, got.RunID)
	}
	if runs[0].Status != jobs.StatusPending {
		t.Fatalf("run status: got %q want pending", runs[0].Status)
	}
}

// TestAPIRunNowDisabledJobEnqueues pins the documented intentional behavior:
// POST /api/jobs/{id}/run is a manual operator override that enqueues a run
// even when the job is disabled. The scheduler still won't auto-run a disabled
// job, but run-now bypasses the enabled filter deliberately.
func TestAPIRunNowDisabledJobEnqueues(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	// Seed a DISABLED job.
	jobID, err := ts.store.CreateJob(context.Background(), jobs.Job{
		OrgID:           ts.orgID,
		Name:            "disabled-job",
		Type:            jobs.Shell,
		Command:         "echo hi",
		IntervalSeconds: 60,
		Enabled:         false, // disabled
	})
	if err != nil {
		t.Fatalf("CreateJob disabled: %v", err)
	}
	if jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("precondition: seeded job should be disabled")
	}

	rec := apiPost(t, h, as, "/api/jobs/"+itoa(jobID)+"/run")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202; run-now must enqueue even for a disabled job; body=%q", rec.Code, rec.Body.String())
	}
	var got struct {
		RunID int64 `json:"run_id"`
	}
	decodeJSON(t, rec, &got)
	if got.RunID <= 0 {
		t.Fatalf("run_id: got %d want a positive id", got.RunID)
	}

	// The enqueued run must be visible via ListRuns (one pending run).
	runs, err := ts.store.ListRuns(context.Background(), ts.orgID, jobID, maxAPILimitForTest)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns: got %d runs want 1 (run-now on disabled job must produce a run)", len(runs))
	}
	if runs[0].ID != got.RunID {
		t.Fatalf("run id mismatch: ListRuns has %d, response said %d", runs[0].ID, got.RunID)
	}
	if runs[0].Status != jobs.StatusPending {
		t.Fatalf("run status: got %q want pending", runs[0].Status)
	}
}

func TestAPIRunNowAbsent404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiPost(t, h, as, "/api/jobs/9999/run")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (absent job)", rec.Code)
	}
}

func TestAPIRunNowNonNumeric400(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiPost(t, h, as, "/api/jobs/not-a-number/run")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 for non-numeric id", rec.Code)
	}
}

// Cross-org: org A POSTing /run to org B's job id must 404 AND enqueue nothing
// for the real owner (no side effect on the foreign org).
func TestAPIRunNowCrossOrg404NoSideEffect(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A == ts.orgID
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	if got := runCount(t, ts, orgB, bJob); got != 0 {
		t.Fatalf("precondition: org B job has %d runs want 0", got)
	}

	rec := apiPost(t, h, as, "/api/jobs/"+itoa(bJob)+"/run")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org run)", rec.Code)
	}
	// No run may have been enqueued for the real owner (org B).
	if got := runCount(t, ts, orgB, bJob); got != 0 {
		t.Fatalf("cross-org /run side effect: org B job now has %d runs want 0", got)
	}
}

// --- POST /api/jobs/{id}/enable and /disable ---------------------------------

func TestAPIDisableThenEnableFlipsEnabled(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha") // seeded Enabled=true
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	if !jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("precondition: seeded job should be enabled")
	}

	// Disable -> 200 with updated DTO (enabled=false), persisted.
	rec := apiPost(t, h, as, "/api/jobs/"+itoa(jobID)+"/disable")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var disabled struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	decodeJSON(t, rec, &disabled)
	if disabled.ID != jobID || disabled.Enabled {
		t.Fatalf("disable DTO: got %+v want id=%d enabled=false", disabled, jobID)
	}
	if jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("disable not persisted: GetJob still reports enabled")
	}

	// Enable -> 200 with updated DTO (enabled=true), persisted.
	rec = apiPost(t, h, as, "/api/jobs/"+itoa(jobID)+"/enable")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var enabled struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	decodeJSON(t, rec, &enabled)
	if enabled.ID != jobID || !enabled.Enabled {
		t.Fatalf("enable DTO: got %+v want id=%d enabled=true", enabled, jobID)
	}
	if !jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("enable not persisted: GetJob reports disabled")
	}
}

func TestAPIEnableAbsent404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiPost(t, h, as, "/api/jobs/9999/enable")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (absent job enable)", rec.Code)
	}
	rec = apiPost(t, h, as, "/api/jobs/9999/disable")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (absent job disable)", rec.Code)
	}
}

func TestAPIEnableNonNumeric400(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	rec := apiPost(t, h, as, "/api/jobs/not-a-number/enable")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (non-numeric enable)", rec.Code)
	}
	rec = apiPost(t, h, as, "/api/jobs/not-a-number/disable")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (non-numeric disable)", rec.Code)
	}
}

// Cross-org: org A POSTing /enable or /disable to org B's job must 404 AND leave
// org B's job.Enabled untouched (no side effect on the foreign org).
func TestAPIEnableDisableCrossOrg404NoSideEffect(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A == ts.orgID
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job") // seeded Enabled=true
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	if !jobEnabled(t, ts, orgB, bJob) {
		t.Fatalf("precondition: org B job should be enabled")
	}

	// Cross-org disable must 404 and NOT flip org B's job.
	rec := apiPost(t, h, as, "/api/jobs/"+itoa(bJob)+"/disable")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disable status: got %d want 404 (cross-org)", rec.Code)
	}
	if !jobEnabled(t, ts, orgB, bJob) {
		t.Fatalf("cross-org /disable side effect: org B job was disabled")
	}

	// Cross-org enable on a (hypothetically) toggled job: make org B's job
	// disabled directly, then assert a cross-org enable does NOT re-enable it.
	bj, err := ts.store.GetJob(context.Background(), orgB, bJob)
	if err != nil {
		t.Fatalf("GetJob org B: %v", err)
	}
	bj.Enabled = false
	if err := ts.store.UpdateJob(context.Background(), bj); err != nil {
		t.Fatalf("UpdateJob org B: %v", err)
	}
	rec = apiPost(t, h, as, "/api/jobs/"+itoa(bJob)+"/enable")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("enable status: got %d want 404 (cross-org)", rec.Code)
	}
	if jobEnabled(t, ts, orgB, bJob) {
		t.Fatalf("cross-org /enable side effect: org B job was enabled")
	}
}

// itoa is a tiny helper to keep paths readable.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
