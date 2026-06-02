package httpserver

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// This file is the server-rendered htmx dashboard: a base layout plus the jobs
// list, job detail (with recent runs), and run detail (with full output) pages.
//
// SECURITY: every dynamic value reaches the page through html/template, whose
// contextual auto-escaping is the XSS defense. No view field below is ever
// wrapped in template.HTML/JS/URL, so attacker-influenced data (notably run
// Output and job names) can never become live markup. The view structs are also
// deliberately narrow: they carry NO secret material (no heartbeat tokens, no
// channel/secret config, no Job.Env, no password/token hashes, no session
// cookie). Every page is org-scoped via the request Principal.

// templatesFS embeds the page templates; staticFS embeds the browser assets
// (the pre-vendored htmx.min.js and app.css). Both are compiled into the binary
// so the server needs no on-disk assets at runtime (CGO-free, single binary).
//
//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// dashboardPages holds one parsed template SET per page, built ONCE at package
// init. Each page file defines its own {{define "content"}} block; because all
// three would collide in a single set, each page is parsed together with the
// shared base layout into its own set, keyed by page filename. Rendering a page
// executes the "base" template of that set. The standalone "jobsTable" fragment
// lives in jobs.html and is rendered from that page's set.
//
// template.Must panics at startup on a malformed template - a deploy-time
// failure, never a per-request one.
var dashboardPages = parseDashboardPages()

// pageFiles enumerates the content templates that each compose with base.html.
var pageFiles = []string{"jobs.html", "job_detail.html", "run_detail.html",
	"heartbeats.html", "events.html"}

// parseDashboardPages parses base.html together with each page file into its
// own isolated template set so the per-page {{define "content"}} blocks do not
// collide.
func parseDashboardPages() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageFiles))
	for _, page := range pageFiles {
		out[page] = template.Must(template.ParseFS(templatesFS,
			"templates/base.html", "templates/"+page))
	}
	return out
}

// staticHandler serves the embedded static/ directory under /static/. fs.Sub
// strips the "static" path element so the URL /static/app.css maps to the
// embedded static/app.css. It stays on the PUBLIC mux (un-gated by auth).
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The embed path is a compile-time constant, so this cannot fail in a
		// built binary; panic loudly if it somehow does at startup.
		panic("httpserver: embedded static FS: " + err.Error())
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// --- view models -------------------------------------------------------------
//
// View structs are separate from the API DTOs: they pre-format values for
// display (schedules, timestamps, status CSS classes) and carry only
// non-secret, page-relevant fields.

// pageData is the data passed to the base layout for every page.
type pageData struct {
	Title     string
	Active    string // nav highlight key: "jobs" | "heartbeats" | "events"
	CSRFToken string // session-bound CSRF token (empty for non-cookie auth)
}

// jobsPage backs the job list page and its polling fragment.
type jobsPage struct {
	pageData
	Jobs []jobRow
}

// jobRow is one row in the jobs table. It is rendered by the shared "jobRow"
// template block both inside the table range and standalone (the htmx action
// fragment), so it carries its own CSRFToken: the row's action buttons embed it
// via hx-headers, and referencing it as {{.CSRFToken}} works in BOTH contexts
// (where {{$}} would differ). The token is the session-bound CSRF token, NOT
// secret credential material; it is meant to be rendered into the page.
type jobRow struct {
	ID          int64
	Name        string
	Type        string
	Schedule    string
	Enabled     bool
	LastStatus  string // "" when the job has no runs
	StatusClass string
	CSRFToken   string
}

// jobDetailPage backs the job detail page.
type jobDetailPage struct {
	pageData
	Job  jobView
	Runs []runRow
}

// jobView is the formatted job metadata shown on the detail page. It omits
// Job.Env entirely (values may reference secrets) and every other secret.
type jobView struct {
	ID         int64
	Name       string
	Type       string
	Schedule   string
	Target     string // http URL/method or empty; never secret
	Timeout    string
	MaxRetries int
	Enabled    bool
}

// runRow is one row in a job's recent-runs table.
type runRow struct {
	ID          int64
	Status      string
	StatusClass string
	Attempt     int
	ExitCode    int
	Started     string
}

// runDetailPage backs the run detail page.
type runDetailPage struct {
	pageData
	Run runView
}

// runView is the formatted run detail, including the full captured Output. The
// Output is rendered as plain text inside a <pre> and escaped by the template.
type runView struct {
	ID          int64
	JobID       int64
	Status      string
	StatusClass string
	Attempt     int
	ExitCode    int
	Started     string
	Ended       string
	Output      string
}

// heartbeatsPage backs the heartbeats list page.
type heartbeatsPage struct {
	pageData
	Heartbeats []heartbeatRow
}

// heartbeatRow is one row in the heartbeats table. It STRUCTURALLY OMITS the
// heartbeat Token (the ping credential): there is no Token field, so the secret
// can never reach the page even via a template typo. It carries only non-secret
// display fields (name, status, period/grace, last-seen).
type heartbeatRow struct {
	Name        string
	Status      string // "new" | "up" | "down"
	StatusClass string
	Period      string
	Grace       string
	LastSeen    string
}

// eventsPage backs the events/alerts list page.
type eventsPage struct {
	pageData
	Events []eventRow
}

// eventRow is one row in the events table. Type/Source/Payload are all rendered
// through html/template auto-escaping; Payload is attacker-influenced text (e.g.
// a heartbeat name) and is NEVER wrapped in template.HTML.
type eventRow struct {
	Type    string
	Source  string
	Payload string
	Created string
	IsAlert bool   // run.failed or heartbeat.* - surfaced prominently
	Class   string // pill CSS class for the event type
}

// --- formatting helpers ------------------------------------------------------

// statusClass maps a run status to its pill CSS class.
func statusClass(status jobs.RunStatus) string {
	switch status {
	case jobs.StatusSucceeded:
		return "st-ok"
	case jobs.StatusFailed:
		return "st-fail"
	case jobs.StatusRunning:
		return "st-run"
	case jobs.StatusPending:
		return "st-pending"
	case jobs.StatusTimedOut:
		return "st-timeout"
	default:
		return "pill-none"
	}
}

// scheduleOf renders a human schedule description from the job's schedule fields.
func scheduleOf(j jobs.Job) string {
	switch {
	case j.Cron != "":
		return j.Cron
	case j.IntervalSeconds > 0:
		return "every " + (time.Duration(j.IntervalSeconds) * time.Second).String()
	case !j.RunAt.IsZero():
		return "once @ " + j.RunAt.UTC().Format("2006-01-02 15:04 MST")
	default:
		return "manual"
	}
}

// targetOf returns a non-secret target hint for the job (http jobs only).
func targetOf(j jobs.Job) string {
	if j.Type != jobs.HTTP {
		return ""
	}
	method := j.HTTPMethod
	if method == "" {
		method = "GET"
	}
	return method + " " + j.HTTPURL
}

// timeoutOf renders the job timeout (or the executor default sentinel).
func timeoutOf(j jobs.Job) string {
	if j.TimeoutSeconds <= 0 {
		return "default"
	}
	return (time.Duration(j.TimeoutSeconds) * time.Second).String()
}

// fmtTime renders a timestamp for display, or a dash when zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

func toJobRow(j jobs.Job, last *jobs.Run) jobRow {
	row := jobRow{
		ID:       j.ID,
		Name:     j.Name,
		Type:     string(j.Type),
		Schedule: scheduleOf(j),
		Enabled:  j.Enabled,
	}
	if last != nil {
		row.LastStatus = string(last.Status)
		row.StatusClass = statusClass(last.Status)
	}
	return row
}

func toJobView(j jobs.Job) jobView {
	return jobView{
		ID:         j.ID,
		Name:       j.Name,
		Type:       string(j.Type),
		Schedule:   scheduleOf(j),
		Target:     targetOf(j),
		Timeout:    timeoutOf(j),
		MaxRetries: j.MaxRetries,
		Enabled:    j.Enabled,
	}
}

func toRunRow(r jobs.Run) runRow {
	return runRow{
		ID:          r.ID,
		Status:      string(r.Status),
		StatusClass: statusClass(r.Status),
		Attempt:     r.Attempt,
		ExitCode:    r.ExitCode,
		Started:     fmtTime(r.StartedAt),
	}
}

func toRunView(r jobs.Run) runView {
	return runView{
		ID:          r.ID,
		JobID:       r.JobID,
		Status:      string(r.Status),
		StatusClass: statusClass(r.Status),
		Attempt:     r.Attempt,
		ExitCode:    r.ExitCode,
		Started:     fmtTime(r.StartedAt),
		Ended:       fmtTime(r.EndedAt),
		Output:      r.Output,
	}
}

// secondsLabel renders a seconds count as a human duration, or a dash when zero.
func secondsLabel(seconds int) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).String()
}

// heartbeatStatusClass maps a heartbeat status to its pill CSS class.
func heartbeatStatusClass(status string) string {
	switch status {
	case "up":
		return "st-ok"
	case "down":
		return "st-fail"
	case "new":
		return "st-pending"
	default:
		return "pill-none"
	}
}

func toHeartbeatRow(h heartbeat.Heartbeat) heartbeatRow {
	// NOTE: h.Token is deliberately NOT copied into the row - it must never reach
	// the page.
	return heartbeatRow{
		Name:        h.Name,
		Status:      h.Status,
		StatusClass: heartbeatStatusClass(h.Status),
		Period:      secondsLabel(h.PeriodSeconds),
		Grace:       secondsLabel(h.GraceSeconds),
		LastSeen:    fmtTime(h.LastSeenAt),
	}
}

// eventStatusClass maps an event type to a pill CSS class, mirroring the run
// status palette where it overlaps.
func eventStatusClass(eventType string) string {
	switch {
	case eventType == "run.failed", strings.HasPrefix(eventType, "heartbeat.missed"):
		return "st-fail"
	case eventType == "run.succeeded", strings.HasPrefix(eventType, "heartbeat.recovered"):
		return "st-ok"
	case eventType == "run.started", eventType == "run.running":
		return "st-run"
	case strings.HasPrefix(eventType, "heartbeat."):
		return "st-pending"
	default:
		return "pill-none"
	}
}

// isAlertEvent reports whether an event type is an operational alert worth
// surfacing prominently: a failed run or any heartbeat lifecycle event.
func isAlertEvent(eventType string) bool {
	return eventType == "run.failed" || strings.HasPrefix(eventType, "heartbeat.")
}

func toEventRow(e core.Event) eventRow {
	return eventRow{
		Type:    e.Type,
		Source:  e.Source,
		Payload: e.Payload,
		Created: fmtTime(e.CreatedAt),
		IsAlert: isAlertEvent(e.Type),
		Class:   eventStatusClass(e.Type),
	}
}

// dashboardRunLimit caps the recent-runs list on a job detail page.
const dashboardRunLimit = 50

// dashboardEventLimit caps the events list on the events page (matching the
// API's default page size; the store query itself is newest-first).
const dashboardEventLimit = 100

// --- rendering ---------------------------------------------------------------

// render executes the full page (base layout + the page's content block) for
// the named page file. A render error after the header is committed can only be
// logged. The templates are parsed at startup, so a render error here is a
// programmer bug, not attacker-reachable.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	set, ok := dashboardPages[page]
	if !ok {
		s.log.Error("dashboard: unknown page template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := set.ExecuteTemplate(w, "base", data); err != nil {
		s.log.Error("dashboard: render failed", "page", page, "err", err)
	}
}

// renderFragment executes a named block (no surrounding layout) from a page's
// template set, for htmx swaps.
func (s *Server) renderFragment(w http.ResponseWriter, page, block string, data any) {
	set, ok := dashboardPages[page]
	if !ok {
		s.log.Error("dashboard: unknown page template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := set.ExecuteTemplate(w, block, data); err != nil {
		s.log.Error("dashboard: render fragment failed", "block", block, "err", err)
	}
}

// --- handlers ----------------------------------------------------------------

// handleJobsPage serves GET /{$}: the job list. It loads the org's jobs and, for
// each, the most recent run's status for the live-status column.
func (s *Server) handleJobsPage(w http.ResponseWriter, r *http.Request) {
	page, ok := s.buildJobsPage(w, r)
	if !ok {
		return
	}
	s.render(w, "jobs.html", page)
}

// handleJobsPartial serves GET /partials/jobs: just the jobs table block, for
// the htmx 5s poll (hx-get + hx-swap="outerHTML").
func (s *Server) handleJobsPartial(w http.ResponseWriter, r *http.Request) {
	page, ok := s.buildJobsPage(w, r)
	if !ok {
		return
	}
	s.renderFragment(w, "jobs.html", "jobsTable", page)
}

// buildJobsPage assembles the shared jobs view used by both the full page and
// the polling fragment. It writes an error response and returns ok=false on
// failure so callers can simply return.
func (s *Server) buildJobsPage(w http.ResponseWriter, r *http.Request) (jobsPage, bool) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return jobsPage{}, false
	}
	js, err := s.store.ListJobs(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("dashboard: list jobs failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return jobsPage{}, false
	}
	rows := make([]jobRow, 0, len(js))
	for _, j := range js {
		// Fetch only the single most recent run for the status column.
		rows = append(rows, s.jobRowFor(r, p.OrgID, j))
	}
	return jobsPage{
		pageData: s.pageData(r, "Jobs", "jobs"),
		Jobs:     rows,
	}, true
}

// latestRunOf returns the single most recent run for a job (org-scoped), or nil
// when the job has no runs. A query error is logged and treated as "no runs" so
// the status column degrades gracefully rather than failing the whole row.
func (s *Server) latestRunOf(r *http.Request, orgID, jobID int64) *jobs.Run {
	runs, err := s.store.ListRuns(r.Context(), orgID, jobID, 1)
	if err != nil {
		s.log.Error("dashboard: list runs for status failed", "job_id", jobID, "err", err)
		return nil
	}
	if len(runs) > 0 {
		return &runs[0]
	}
	return nil
}

// jobRowFor builds a fully-populated jobRow for j: it fetches the latest run for
// the status column and stamps the request's CSRF token so the row's action
// buttons can post. Used by both the jobs table and the standalone action
// fragment, so a swapped-in row is byte-identical in shape to a table row.
func (s *Server) jobRowFor(r *http.Request, orgID int64, j jobs.Job) jobRow {
	row := toJobRow(j, s.latestRunOf(r, orgID, j.ID))
	row.CSRFToken = CSRFTokenFrom(r.Context())
	return row
}

// handleHeartbeatsPage serves GET /heartbeats: the org's heartbeats with their
// status and period/grace. The heartbeatRow carries NO token (structural).
func (s *Server) handleHeartbeatsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	hbs, err := s.store.ListHeartbeats(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("dashboard: list heartbeats failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]heartbeatRow, 0, len(hbs))
	for _, h := range hbs {
		rows = append(rows, toHeartbeatRow(h))
	}
	s.render(w, "heartbeats.html", heartbeatsPage{
		pageData:   s.pageData(r, "Heartbeats", "heartbeats"),
		Heartbeats: rows,
	})
}

// handleEventsPage serves GET /events: recent org events (newest first), with
// run.failed and heartbeat.* surfaced as alerts. Payloads are auto-escaped.
func (s *Server) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	evs, err := s.store.ListEvents(r.Context(), p.OrgID, dashboardEventLimit)
	if err != nil {
		s.log.Error("dashboard: list events failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]eventRow, 0, len(evs))
	for _, e := range evs {
		rows = append(rows, toEventRow(e))
	}
	s.render(w, "events.html", eventsPage{
		pageData: s.pageData(r, "Events", "events"),
		Events:   rows,
	})
}

// --- htmx action handlers (HTML row fragment, NOT JSON) ----------------------
//
// These are the dashboard-owned cookie-auth action endpoints. They are DISTINCT
// from the JSON API's POST /api/jobs/{id}/{run,enable,disable}: the public JSON
// API must keep returning JSON for machine clients, so the htmx surface gets its
// own endpoints that return the updated job ROW fragment for hx-swap="outerHTML".
// Both surfaces call the SAME org-guarded store-mutation cores (enqueueJobRun /
// setJobEnabledCore in api.go), so the load-bearing org guard is never
// duplicated and a cross-org id 404s with no side effect on either surface.
//
// requireAuth already enforces CSRF on these cookie-auth POSTs before they run,
// so a missing/invalid token never reaches here (it is rejected with 403).

// handleActionRunNow serves POST /jobs/{id}/run: enqueue a run, then return the
// updated job row fragment.
func (s *Server) handleActionRunNow(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	id, ok := dashID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.enqueueJobRun(r.Context(), p.OrgID, id); err != nil {
		s.actionError(w, r, "run-now", err)
		return
	}
	s.renderJobRow(w, r, p.OrgID, id)
}

// handleActionEnable serves POST /jobs/{id}/enable.
func (s *Server) handleActionEnable(w http.ResponseWriter, r *http.Request) {
	s.actionSetEnabled(w, r, true)
}

// handleActionDisable serves POST /jobs/{id}/disable.
func (s *Server) handleActionDisable(w http.ResponseWriter, r *http.Request) {
	s.actionSetEnabled(w, r, false)
}

// actionSetEnabled toggles the job via the shared core and returns the updated
// row fragment built from the returned job.
func (s *Server) actionSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	id, ok := dashID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	j, err := s.setJobEnabledCore(r.Context(), p.OrgID, id, enabled)
	if err != nil {
		s.actionError(w, r, "enable/disable", err)
		return
	}
	s.renderFragment(w, "jobs.html", "jobRow", s.jobRowFor(r, p.OrgID, j))
}

// renderJobRow re-reads the job (org-scoped) and renders its single row fragment.
// Used after run-now, where the mutation returns only a run id.
func (s *Server) renderJobRow(w http.ResponseWriter, r *http.Request, orgID, id int64) {
	j, err := s.store.GetJob(r.Context(), orgID, id)
	if err != nil {
		s.actionError(w, r, "render row", err)
		return
	}
	s.renderFragment(w, "jobs.html", "jobRow", s.jobRowFor(r, orgID, j))
}

// actionError maps a store error from an action handler to an HTTP response:
// ErrNotFound (foreign/absent id, after the org guard) -> 404; anything else ->
// 500. There is no JSON body here - the htmx surface is HTML.
func (s *Server) actionError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.log.Error("dashboard: action failed", "op", op, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// handleJobDetailPage serves GET /jobs/{id}: the job plus its recent runs.
func (s *Server) handleJobDetailPage(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	id, ok := dashID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	j, err := s.store.GetJob(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("dashboard: get job failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), p.OrgID, id, dashboardRunLimit)
	if err != nil {
		s.log.Error("dashboard: list runs failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, toRunRow(run))
	}
	s.render(w, "job_detail.html", jobDetailPage{
		pageData: s.pageData(r, j.Name, "jobs"),
		Job:      toJobView(j),
		Runs:     rows,
	})
}

// handleRunDetailPage serves GET /runs/{id}: the run plus its full output.
// GetRun is org-scoped, so a foreign/absent id resolves to ErrNotFound -> 404.
func (s *Server) handleRunDetailPage(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	id, ok := dashID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetRun(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("dashboard: get run failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, "run_detail.html", runDetailPage{
		pageData: s.pageData(r, "Run #"+strconv.FormatInt(run.ID, 10), "jobs"),
		Run:      toRunView(run),
	})
}

// pageData builds the layout-shared data: title, nav highlight, and the
// session-bound CSRF token (empty for non-cookie auth) for the logout form and
// the Task 7 action buttons.
func (s *Server) pageData(r *http.Request, title, active string) pageData {
	return pageData{
		Title:     title,
		Active:    active,
		CSRFToken: CSRFTokenFrom(r.Context()),
	}
}

// dashID parses the {id} path value as a positive int64 (404 otherwise).
func dashID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// registerDashboardRoutes registers the dashboard pages on the given (already
// requireAuth-gated) mux. Called from Handler() at the Task 6 marker.
func (s *Server) registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleJobsPage)
	mux.HandleFunc("GET /partials/jobs", s.handleJobsPartial)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobDetailPage)
	mux.HandleFunc("GET /runs/{id}", s.handleRunDetailPage)
	mux.HandleFunc("GET /heartbeats", s.handleHeartbeatsPage)
	mux.HandleFunc("GET /events", s.handleEventsPage)

	// Dashboard-owned htmx action endpoints (HTML row fragment, NOT JSON). These
	// are method+path distinct from "GET /jobs/{id}", so no routing collision.
	// requireAuth enforces cookie-auth CSRF on these POSTs before they run.
	mux.HandleFunc("POST /jobs/{id}/run", s.handleActionRunNow)
	mux.HandleFunc("POST /jobs/{id}/enable", s.handleActionEnable)
	mux.HandleFunc("POST /jobs/{id}/disable", s.handleActionDisable)
}
