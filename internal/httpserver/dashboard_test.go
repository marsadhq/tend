package httpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/httpserver"
)

// --- dashboard test helpers --------------------------------------------------

// dashGet issues a cookie-authenticated GET against the server handler and
// returns the recorder. The dashboard is the cookie-auth surface (cookie auth
// is what carries a CSRF token into the page), so unlike the API tests we
// authenticate with a session cookie, not a bearer token.
func dashGet(t *testing.T, h http.Handler, cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// csrfMetaRe extracts the CSRF token rendered into <meta name="csrf-token" ...>.
var csrfMetaRe = regexp.MustCompile(`<meta name="csrf-token" content="([^"]*)"`)

// csrfTokenFromPage pulls the rendered CSRF token out of a page body.
func csrfTokenFromPage(t *testing.T, body string) string {
	t.Helper()
	m := csrfMetaRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf-token meta tag in page; body=%q", body)
	}
	return m[1]
}

// --- GET / (job list) --------------------------------------------------------

func TestDashboardJobListRendersJobs(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "nightly-backup")
	jobID := seedJob(t, ts, ts.orgID, "report-roller")
	seedRun(t, ts, ts.orgID, jobID, "succeeded", "ok")
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q want text/html…", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"nightly-backup", "report-roller", "succeeded"} {
		if !strings.Contains(body, want) {
			t.Fatalf("job list page missing %q; body=%q", want, body)
		}
	}
	// htmx must be referenced from the local embedded asset, never a CDN.
	if !strings.Contains(body, "/static/htmx.min.js") {
		t.Fatalf("page does not reference local htmx asset; body=%q", body)
	}
	if strings.Contains(body, "cdn") || strings.Contains(body, "unpkg") {
		t.Fatalf("page references a CDN; htmx must be served locally; body=%q", body)
	}
}

func TestDashboardRootRendersCSRFToken(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()
	cookie, wantCSRF := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/")
	got := csrfTokenFromPage(t, rec.Body.String())
	if got == "" {
		t.Fatal("rendered CSRF token is empty")
	}
	if got != wantCSRF {
		t.Fatalf("rendered CSRF token mismatch: got %q want %q", got, wantCSRF)
	}
	// The logout control must be a real POST form carrying the same token.
	body := rec.Body.String()
	if !strings.Contains(body, `action="/logout"`) {
		t.Fatalf("page has no logout form; body=%q", body)
	}
	if !strings.Contains(body, `name="csrf_token" value="`+wantCSRF+`"`) {
		t.Fatalf("logout form missing hidden csrf_token field; body=%q", body)
	}
}

// Proves the CSRF plumbing end-to-end: the token rendered into the page is a
// valid token for the session, so POSTing /logout with it succeeds (302) and
// clears the cookie. (A logout WITHOUT the token is rejected by Task 3's test.)
func TestDashboardLogoutWithRenderedTokenWorks(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	srv := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec})
	h := srv.Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	page := dashGet(t, h, cookie, "/")
	token := csrfTokenFromPage(t, page.Body.String())

	form := url.Values{"csrf_token": {token}}
	req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("logout status: got %d want 302; body=%q", rec.Code, rec.Body.String())
	}
	c := findCookie(rec.Result().Cookies(), "tend_session")
	if c == nil {
		t.Fatal("logout set no tend_session cookie")
	}
	if c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("logout did not clear cookie: MaxAge=%d Value=%q", c.MaxAge, c.Value)
	}
}

// Bearer (API-token) requests are CSRF-exempt and must get an EMPTY CSRF token
// from the helper - the plumbing must not weaken auth or mint tokens for them.
func TestCSRFTokenFromEmptyForBearer(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	srv := newAuthServer(t, ts.store, authConfig(false))

	var got string
	var sawOK bool
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = httpserver.CSRFTokenFrom(r.Context())
		_, sawOK = httpserver.PrincipalFrom(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+as.token)
	srv.RequireAuthForTest(probe).ServeHTTP(httptest.NewRecorder(), req)

	if !sawOK {
		t.Fatal("bearer request was not authenticated")
	}
	if got != "" {
		t.Fatalf("bearer CSRF token: got %q want empty", got)
	}
}

func TestCSRFTokenFromEmptyWhenAbsent(t *testing.T) {
	if got := httpserver.CSRFTokenFrom(nil); got != "" {
		t.Fatalf("CSRFTokenFrom(nil): got %q want empty", got)
	}
}

// --- unauthenticated redirect ------------------------------------------------

func TestDashboardUnauthenticatedRedirects(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("redirect Location: got %q want /login", loc)
	}
}

// --- GET /jobs/{id} (job detail + runs) --------------------------------------

func TestDashboardJobDetailRendersJobAndRuns(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "data-sync")
	seedRun(t, ts, ts.orgID, jobID, "succeeded", "first run output")
	seedRun(t, ts, ts.orgID, jobID, "failed", "second run output")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/jobs/"+itoa(jobID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"data-sync", "succeeded", "failed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("job detail page missing %q; body=%q", want, body)
		}
	}
}

func TestDashboardJobDetailUnknown404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/jobs/999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

// Cross-org: a caller in org A must get 404 for org B's job detail page.
func TestDashboardJobDetailCrossOrg404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A == ts.orgID
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "secret-b-job")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/jobs/"+itoa(bJob))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org job detail)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-b-job") {
		t.Fatalf("cross-org job name leaked into page; body=%q", rec.Body.String())
	}
}

// --- GET /runs/{id} (run detail + output) ------------------------------------

func TestDashboardRunDetailRendersOutput(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	runID := seedRun(t, ts, ts.orgID, jobID, "succeeded", "the-distinct-run-output-marker")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/runs/"+itoa(runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the-distinct-run-output-marker") {
		t.Fatalf("run detail page missing output; body=%q", rec.Body.String())
	}
}

// SECURITY: run output must be HTML-escaped, never injected as live markup.
func TestDashboardRunDetailEscapesOutput(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "alpha")
	const payload = `<script>alert('xss-pwn')</script>`
	runID := seedRun(t, ts, ts.orgID, jobID, "failed", payload)
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/runs/"+itoa(runID))
	body := rec.Body.String()
	// The raw, live <script> tag must NOT appear...
	if strings.Contains(body, payload) {
		t.Fatalf("run output rendered as live markup (XSS); body=%q", body)
	}
	// ...but its escaped form must (proving the value is shown, just inert).
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("run output not HTML-escaped; body=%q", body)
	}
}

func TestDashboardRunDetailCrossOrg404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job")
	bRun := seedRun(t, ts, orgB, bJob, "succeeded", "org-b-secret-output")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/runs/"+itoa(bRun))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org run)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "org-b-secret-output") {
		t.Fatalf("cross-org run output leaked; body=%q", rec.Body.String())
	}
}

// --- static assets (public, un-gated) ----------------------------------------

func TestStaticHtmxServed(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	// No auth: /static is public.
	req := httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js: got %d want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("htmx.min.js body is empty")
	}
}

func TestStaticCSSServed(t *testing.T) {
	ts := newStore(t)
	seedAuth(t, ts)
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/app.css: got %d want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("app.css body is empty")
	}
}

// Ensure no per-page leak of the session cookie value itself into HTML.
func TestDashboardDoesNotLeakSessionCookie(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "alpha")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/")
	if strings.Contains(rec.Body.String(), cookie) {
		t.Fatal("session cookie value leaked into the HTML page")
	}
}

// --- GET /partials/jobs (bare fragment, no full page) ------------------------

// The jobs polling fragment must be a BARE table fragment (no surrounding page
// chrome): it returns 200, contains a seeded job name, and must NOT contain the
// document preamble that only the full page carries. Task 6 left this uncovered.
func TestDashboardJobsPartialIsBareFragment(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "partial-job")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/partials/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial-job") {
		t.Fatalf("partial missing seeded job name; body=%q", body)
	}
	// A bare fragment must NOT carry the full-page doctype/layout.
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Fatalf("/partials/jobs returned a full page, not a bare fragment; body=%q", body)
	}
}

// --- GET /heartbeats ---------------------------------------------------------

func TestDashboardHeartbeatsRendersStatusNoToken(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	ts.seedHB(t, "db-up", "TOKEN-db-up-zzz", "up")
	ts.seedHB(t, "cache-down", "TOKEN-cache-down-yyy", "down")
	ts.seedHB(t, "fresh-one", "TOKEN-fresh-xxx", "new")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/heartbeats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q want text/html…", ct)
	}
	body := rec.Body.String()
	// Names and all three statuses must render.
	for _, want := range []string{"db-up", "cache-down", "fresh-one", "up", "down", "new"} {
		if !strings.Contains(body, want) {
			t.Fatalf("heartbeats page missing %q; body=%q", want, body)
		}
	}
	// The active nav link must be Heartbeats.
	if !strings.Contains(body, `href="/heartbeats"`) {
		t.Fatalf("heartbeats nav link missing; body=%q", body)
	}
}

// SECURITY: a heartbeat's ping token is secret credential material and must
// NEVER appear in any HTML response.
func TestDashboardHeartbeatsNoTokenLeak(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const secretToken = "SUPER-SECRET-PING-TOKEN-do-not-render-abcdef0123456789"
	ts.seedHB(t, "watched", secretToken, "up")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/heartbeats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretToken) {
		t.Fatalf("heartbeat token leaked into HTML page; body=%q", rec.Body.String())
	}
}

// Cross-org: heartbeats are org-scoped; org A must not see org B's heartbeat.
func TestDashboardHeartbeatsOrgScoped(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A == ts.orgID
	ts.seedHB(t, "mine-hb", "tok-mine", "up")
	orgB := newOrg(t, ts, "org-b")
	createdB := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := ts.raw.ExecContext(context.Background(),
		`INSERT INTO heartbeats (org_id, name, token, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		orgB, "theirs-hb", "tok-theirs", "up", createdB); err != nil {
		t.Fatalf("seed org-b heartbeat: %v", err)
	}
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/heartbeats")
	body := rec.Body.String()
	if !strings.Contains(body, "mine-hb") {
		t.Fatalf("heartbeats page missing own heartbeat; body=%q", body)
	}
	if strings.Contains(body, "theirs-hb") {
		t.Fatalf("heartbeats page leaked another org's heartbeat; body=%q", body)
	}
}

// --- GET /events -------------------------------------------------------------

func TestDashboardEventsRendersRecentEvents(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "run.failed", Source: "jobs.runner",
		Payload: `{"job_id":7,"exit":1}`,
	}); err != nil {
		t.Fatalf("EmitEvent run.failed: %v", err)
	}
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "heartbeat.missed", Source: "heartbeat",
		Payload: "criticaljob",
	}); err != nil {
		t.Fatalf("EmitEvent heartbeat.missed: %v", err)
	}
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"run.failed", "heartbeat.missed", "criticaljob"} {
		if !strings.Contains(body, want) {
			t.Fatalf("events page missing %q; body=%q", want, body)
		}
	}
}

// SECURITY: an event payload is attacker-influenced text (e.g. a heartbeat name)
// and must be HTML-escaped, never rendered as live markup.
func TestDashboardEventsEscapesPayload(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	const payload = `<img src=x onerror=alert('xss')>`
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "heartbeat.missed", Source: "heartbeat", Payload: payload,
	}); err != nil {
		t.Fatalf("EmitEvent: %v", err)
	}
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/events")
	body := rec.Body.String()
	if strings.Contains(body, payload) {
		t.Fatalf("event payload rendered as live markup (XSS); body=%q", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Fatalf("event payload not HTML-escaped; body=%q", body)
	}
}

// Cross-org: events are org-scoped; org A must not see org B's events.
func TestDashboardEventsOrgScoped(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: ts.orgID, Type: "run.failed", Source: "jobs.runner", Payload: "mine-event-marker",
	}); err != nil {
		t.Fatalf("EmitEvent A: %v", err)
	}
	orgB := newOrg(t, ts, "org-b")
	if _, err := ts.store.EmitEvent(context.Background(), core.Event{
		OrgID: orgB, Type: "run.failed", Source: "jobs.runner", Payload: "theirs-event-marker",
	}); err != nil {
		t.Fatalf("EmitEvent B: %v", err)
	}
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/events")
	body := rec.Body.String()
	if !strings.Contains(body, "mine-event-marker") {
		t.Fatalf("events page missing own event; body=%q", body)
	}
	if strings.Contains(body, "theirs-event-marker") {
		t.Fatalf("events page leaked another org's event; body=%q", body)
	}
}

// --- dashboard action endpoints (htmx run-now / enable / disable) ------------

// dashPostCSRF issues a cookie-authenticated POST carrying the rendered CSRF
// token in the X-CSRF-Token header (the header validCSRF reads), matching how
// the htmx buttons post via hx-headers.
func dashPostCSRF(t *testing.T, h http.Handler, cookie, csrf, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The run-now button posts to /jobs/{id}/run with the CSRF header, enqueues a
// run, and the response is the updated job ROW fragment (HTML <tr>), NOT a full
// page and NOT JSON.
func TestDashboardActionRunNowReturnsRowFragment(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "run-me")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	if got := runCount(t, ts, ts.orgID, jobID); got != 0 {
		t.Fatalf("precondition: got %d runs want 0", got)
	}

	rec := dashPostCSRF(t, h, cookie, csrf, "/jobs/"+itoa(jobID)+"/run")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q want text/html (row fragment)", ct)
	}
	body := rec.Body.String()
	// A row fragment: a <tr> for this job, NOT a whole page.
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Fatalf("run action returned a full page, not a row fragment; body=%q", body)
	}
	if !strings.Contains(body, "<tr") {
		t.Fatalf("run action did not return a <tr> row fragment; body=%q", body)
	}
	if !strings.Contains(body, "run-me") {
		t.Fatalf("row fragment missing job name; body=%q", body)
	}
	// A run must have been enqueued.
	if got := runCount(t, ts, ts.orgID, jobID); got != 1 {
		t.Fatalf("run-now did not enqueue: got %d runs want 1", got)
	}
}

// The disable/enable buttons toggle the job and return the updated row fragment
// reflecting the new state.
func TestDashboardActionDisableEnableReturnsRowFragment(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "toggle-me") // seeded Enabled=true
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	// Disable -> row fragment, persisted enabled=false.
	rec := dashPostCSRF(t, h, cookie, csrf, "/jobs/"+itoa(jobID)+"/disable")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<tr") || strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Fatalf("disable did not return a bare row fragment; body=%q", body)
	}
	if !strings.Contains(body, "paused") {
		t.Fatalf("disabled row fragment should show paused state; body=%q", body)
	}
	if jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("disable not persisted: GetJob still reports enabled")
	}

	// Enable -> row fragment, persisted enabled=true.
	rec = dashPostCSRF(t, h, cookie, csrf, "/jobs/"+itoa(jobID)+"/enable")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "enabled") {
		t.Fatalf("enabled row fragment should show enabled state; body=%q", body)
	}
	if !jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("enable not persisted: GetJob reports disabled")
	}
}

// SECURITY: the action POSTs are cookie-auth mutations; a POST WITHOUT a valid
// CSRF token must be rejected with 403 and have no side effect.
func TestDashboardActionRunNowNoCSRFRejected(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "guarded")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	// No X-CSRF-Token header and no csrf_token form field.
	req := httptest.NewRequest(http.MethodPost, "/jobs/"+itoa(jobID)+"/run", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (cookie action POST without CSRF)", rec.Code)
	}
	if got := runCount(t, ts, ts.orgID, jobID); got != 0 {
		t.Fatalf("rejected run-now had a side effect: got %d runs want 0", got)
	}
}

func TestDashboardActionDisableNoCSRFRejected(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "guarded") // Enabled=true
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	req := httptest.NewRequest(http.MethodPost, "/jobs/"+itoa(jobID)+"/disable", nil)
	req.AddCookie(&http.Cookie{Name: "tend_session", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (cookie disable without CSRF)", rec.Code)
	}
	if !jobEnabled(t, ts, ts.orgID, jobID) {
		t.Fatalf("rejected disable had a side effect: job was disabled")
	}
}

// Cross-org: org A POSTing an action to org B's job must 404 with NO side
// effect, even with a valid CSRF token for org A's session.
func TestDashboardActionRunNowCrossOrg404NoSideEffect(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	if got := runCount(t, ts, orgB, bJob); got != 0 {
		t.Fatalf("precondition: org B job has %d runs want 0", got)
	}

	rec := dashPostCSRF(t, h, cookie, csrf, "/jobs/"+itoa(bJob)+"/run")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org dashboard run)", rec.Code)
	}
	if got := runCount(t, ts, orgB, bJob); got != 0 {
		t.Fatalf("cross-org dashboard /run side effect: org B job now has %d runs want 0", got)
	}
}

func TestDashboardActionDisableCrossOrg404NoSideEffect(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts) // org A
	orgB := newOrg(t, ts, "org-b")
	bJob := seedJob(t, ts, orgB, "b-job") // Enabled=true
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	rec := dashPostCSRF(t, h, cookie, csrf, "/jobs/"+itoa(bJob)+"/disable")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (cross-org dashboard disable)", rec.Code)
	}
	if !jobEnabled(t, ts, orgB, bJob) {
		t.Fatalf("cross-org dashboard /disable side effect: org B job was disabled")
	}
}

func TestDashboardActionAbsentJob404(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	for _, action := range []string{"run", "enable", "disable"} {
		rec := dashPostCSRF(t, h, cookie, csrf, "/jobs/999999/"+action)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s absent job: got %d want 404", action, rec.Code)
		}
	}
}

// compile-time anchor that auth.Session is the type CSRF is bound to (keeps the
// import in use if the test list above changes).
var _ = auth.Session{}
