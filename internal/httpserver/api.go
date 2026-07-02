package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// This file is the read-only REST API and its DTOs. The DTOs are the wire
// contract: domain types are NEVER serialized directly. Each DTO is a
// purpose-built struct with explicit json tags that OMITS every secret field
// (heartbeat tokens, channel config ciphertext, secret ciphertext,
// password/token hashes). Adding a secret-bearing field to a DTO here is the one
// way to break the milestone's security guarantee, so the structs below are
// deliberately minimal and audited.
//
// All handlers read the Principal from the request context (set by requireAuth)
// and scope every store call to Principal.OrgID. Routes are registered on the
// requireAuth-gated mux in Handler().

const (
	// defaultAPILimit is the row cap applied to list endpoints when ?limit= is
	// missing, non-numeric, zero, or negative.
	defaultAPILimit = 50
	// maxAPILimit is the hard upper bound; larger ?limit= values are clamped.
	maxAPILimit = 200
)

// --- response helpers --------------------------------------------------------

// writeJSON sets the JSON content type, writes the status code, and encodes v.
// A late encoding error (after the header is committed) can only be logged.
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("api: encode response failed", "err", err)
	}
}

// apiError writes a minimal JSON error body: {"error":"…"}.
func (s *Server) apiError(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, map[string]string{"error": msg})
}

// --- request parsing ---------------------------------------------------------

// parseLimit reads ?limit= and returns a sane bound: missing/invalid/zero/
// negative -> defaultAPILimit; values above maxAPILimit -> maxAPILimit.
func parseLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultAPILimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultAPILimit
	}
	if n > maxAPILimit {
		return maxAPILimit
	}
	return n
}

// pathID parses the {id} path value as a positive int64. A non-numeric or
// non-positive id is a client error.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// --- DTOs --------------------------------------------------------------------

// jobDTO is the wire shape of a job. It carries no secret material (a job's Env
// may reference secrets by name but holds no secret values; we omit Env entirely
// to keep the read surface minimal and avoid leaking any embedded literal).
type jobDTO struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Command         string    `json:"command,omitempty"`
	HTTPURL         string    `json:"http_url,omitempty"`
	HTTPMethod      string    `json:"http_method,omitempty"`
	Cron            string    `json:"cron,omitempty"`
	IntervalSeconds int       `json:"interval_seconds,omitempty"`
	TimeoutSeconds  int       `json:"timeout_seconds,omitempty"`
	MaxRetries      int       `json:"max_retries,omitempty"`
	Enabled         bool      `json:"enabled"`
	NextRunAt       time.Time `json:"next_run_at,omitzero"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func fromJob(j jobs.Job) jobDTO {
	return jobDTO{
		ID:              j.ID,
		Name:            j.Name,
		Type:            string(j.Type),
		Command:         j.Command,
		HTTPURL:         j.HTTPURL,
		HTTPMethod:      j.HTTPMethod,
		Cron:            j.Cron,
		IntervalSeconds: j.IntervalSeconds,
		TimeoutSeconds:  j.TimeoutSeconds,
		MaxRetries:      j.MaxRetries,
		Enabled:         j.Enabled,
		NextRunAt:       j.NextRunAt,
		CreatedAt:       j.CreatedAt,
		UpdatedAt:       j.UpdatedAt,
	}
}

// runSummaryDTO is the LIST shape of a run. It deliberately OMITS Output to keep
// listings light; the full output is only served by GET /api/runs/{id}.
type runSummaryDTO struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Status    string    `json:"status"`
	Attempt   int       `json:"attempt"`
	ExitCode  int       `json:"exit_code"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
}

func fromRunSummary(r jobs.Run) runSummaryDTO {
	return runSummaryDTO{
		ID:        r.ID,
		JobID:     r.JobID,
		Status:    string(r.Status),
		Attempt:   r.Attempt,
		ExitCode:  r.ExitCode,
		StartedAt: r.StartedAt,
		EndedAt:   r.EndedAt,
		CreatedAt: r.CreatedAt,
	}
}

// runDetailDTO is the DETAIL shape of a run, including the full Output.
type runDetailDTO struct {
	runSummaryDTO
	Output string `json:"output"`
}

func fromRunDetail(r jobs.Run) runDetailDTO {
	return runDetailDTO{
		runSummaryDTO: fromRunSummary(r),
		Output:        r.Output,
	}
}

// channelDTO is the wire shape of a notification channel. It carries ONLY
// non-secret metadata: there is structurally no config/ciphertext field, so a
// channel's encrypted provider configuration can never reach a client here.
type channelDTO struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

func fromChannel(c notify.Channel) channelDTO {
	return channelDTO{
		ID:        c.ID,
		Name:      c.Name,
		Kind:      string(c.Kind),
		CreatedAt: c.CreatedAt,
	}
}

// ruleDTO is the wire shape of a notification rule. It holds no secret material.
type ruleDTO struct {
	ID        int64     `json:"id"`
	ChannelID int64     `json:"channel_id"`
	EventType string    `json:"event_type"`
	Enabled   bool      `json:"enabled"`
	JobID     int64     `json:"job_id"`
	CreatedAt time.Time `json:"created_at"`
}

func fromRule(r notify.Rule) ruleDTO {
	return ruleDTO{
		ID:        r.ID,
		ChannelID: r.ChannelID,
		EventType: r.EventType,
		Enabled:   r.Enabled,
		JobID:     r.JobID,
		CreatedAt: r.CreatedAt,
	}
}

// heartbeatDTO is the wire shape of a heartbeat. It STRUCTURALLY OMITS the Token
// field (the ping credential): there is no Token field to serialize, so the
// dead-man's-switch token can never be exposed via the API.
type heartbeatDTO struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	LastSeenAt    time.Time `json:"last_seen_at,omitzero"`
	PeriodSeconds int       `json:"period_seconds"`
	GraceSeconds  int       `json:"grace_seconds"`
	CreatedAt     time.Time `json:"created_at"`
}

func fromHeartbeat(h heartbeat.Heartbeat) heartbeatDTO {
	return heartbeatDTO{
		ID:            h.ID,
		Name:          h.Name,
		Status:        h.Status,
		LastSeenAt:    h.LastSeenAt,
		PeriodSeconds: h.PeriodSeconds,
		GraceSeconds:  h.GraceSeconds,
		CreatedAt:     h.CreatedAt,
	}
}

// eventDTO is the wire shape of an event. The stored Payload is a string that is
// USUALLY JSON but may be a plain string (e.g. a heartbeat name); fromEvent
// guards this so the response is always valid JSON (see jsonPayload).
type eventDTO struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

func fromEvent(e core.Event) eventDTO {
	return eventDTO{
		ID:        e.ID,
		Type:      e.Type,
		Source:    e.Source,
		Payload:   jsonPayload(e.Payload),
		CreatedAt: e.CreatedAt,
	}
}

// jsonPayload renders a stored event payload as embeddable, always-valid JSON:
//   - empty payload  -> nil (omitted via omitempty)
//   - valid JSON     -> used verbatim as a raw message
//   - any other text -> marshaled as a JSON string value
//
// This prevents a non-JSON payload (e.g. "criticaljob") from corrupting the
// response into invalid JSON.
func jsonPayload(payload string) json.RawMessage {
	if payload == "" {
		return nil
	}
	if json.Valid([]byte(payload)) {
		return json.RawMessage(payload)
	}
	// Marshal as a JSON string. Marshaling a string never fails.
	b, _ := json.Marshal(payload)
	return json.RawMessage(b)
}

// secretDTO is the wire shape of a secret. It carries ONLY the name and
// created_at: there is structurally no value/ciphertext field, so secret
// material can never reach a client here.
type secretDTO struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func fromSecret(m store.SecretMeta) secretDTO {
	return secretDTO{Name: m.Name, CreatedAt: m.CreatedAt}
}

// --- handlers ----------------------------------------------------------------

// handleListJobs serves GET /api/jobs.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	js, err := s.store.ListJobs(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("api: list jobs failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]jobDTO, 0, len(js))
	for _, j := range js {
		out = append(out, fromJob(j))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGetJob serves GET /api/jobs/{id}.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	j, err := s.store.GetJob(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: get job failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, fromJob(j))
}

// handleListJobRuns serves GET /api/jobs/{id}/runs (output omitted in the list).
func (s *Server) handleListJobRuns(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	runs, err := s.store.ListRuns(r.Context(), p.OrgID, id, parseLimit(r))
	if err != nil {
		s.log.Error("api: list runs failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]runSummaryDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, fromRunSummary(run))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGetRun serves GET /api/runs/{id} (full output). ListRuns is org-scoped
// on (org_id, job_id); GetRun is org-scoped on (org_id, run_id), so a cross-org
// run id resolves to ErrNotFound -> 404.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	run, err := s.store.GetRun(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: get run failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, fromRunDetail(run))
}

// handleListChannels serves GET /api/channels (no config/secret material).
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	chs, err := s.store.ListChannels(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("api: list channels failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]channelDTO, 0, len(chs))
	for _, c := range chs {
		out = append(out, fromChannel(c))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleListRules serves GET /api/rules.
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rules, err := s.store.ListRules(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("api: list rules failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]ruleDTO, 0, len(rules))
	for _, rule := range rules {
		out = append(out, fromRule(rule))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleListHeartbeats serves GET /api/heartbeats (DTO has no token field).
func (s *Server) handleListHeartbeats(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	hbs, err := s.store.ListHeartbeats(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("api: list heartbeats failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]heartbeatDTO, 0, len(hbs))
	for _, h := range hbs {
		out = append(out, fromHeartbeat(h))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGetHeartbeat serves GET /api/heartbeats/{id}. The DTO is token-free (the
// ping token is a credential surfaced only via the local CLI).
func (s *Server) handleGetHeartbeat(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	hb, err := s.store.GetHeartbeat(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: get heartbeat failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, fromHeartbeat(hb))
}

// handleHeartbeatHistory serves GET /api/heartbeats/{id}/history: the heartbeat's
// missed/recovered transition events, newest first. The id is resolved to the
// heartbeat name (org-scoped) because heartbeat events are keyed by name in the
// events payload.
func (s *Server) handleHeartbeatHistory(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	hb, err := s.store.GetHeartbeat(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: heartbeat history: get heartbeat failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	evs, err := s.store.ListHeartbeatEvents(r.Context(), p.OrgID, hb.Name, parseLimit(r))
	if err != nil {
		s.log.Error("api: list heartbeat events failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]eventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, fromEvent(e))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleHeartbeatsNotCreatable answers POST /api/heartbeats with an explicit 405
// and a JSON hint. Heartbeats are config-as-code (CLI or YAML sync), so there is
// no create API; without this handler the mux returns a bodyless 405 that reads
// like a wrong URL.
func (s *Server) handleHeartbeatsNotCreatable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET")
	s.apiError(w, http.StatusMethodNotAllowed, "heartbeats are managed via the CLI or YAML sync (tend heartbeat add / tend sync); the API is read-only")
}

// handleListEvents serves GET /api/events (payload embedded as raw JSON).
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	evs, err := s.store.ListEvents(r.Context(), p.OrgID, parseLimit(r))
	if err != nil {
		s.log.Error("api: list events failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]eventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, fromEvent(e))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleListSecrets serves GET /api/secrets (names + created_at; no ciphertext).
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	metas, err := s.store.ListSecrets(r.Context(), p.OrgID)
	if err != nil {
		s.log.Error("api: list secrets failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]secretDTO, 0, len(metas))
	for _, m := range metas {
		out = append(out, fromSecret(m))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// --- action handlers (the ONLY API mutations) --------------------------------
//
// These are the sole mutating endpoints the API exposes. Every other definition
// (jobs/channels/rules/heartbeats) stays config-as-code in the CLI; there is
// deliberately no create/edit/delete here. Each action proves the job belongs to
// the caller's org BEFORE any mutation: a foreign-org id resolves to 404 with no
// side effect.

// runEnqueuedDTO is the 202 response body for POST /api/jobs/{id}/run.
// The JSON field name run_id is the documented wire contract.
type runEnqueuedDTO struct {
	RunID int64 `json:"run_id"`
}

// --- shared store-mutation cores (reused by api.go AND dashboard.go) ----------
//
// These two helpers hold the ONE org-guarded implementation of each mutation.
// Both the JSON API handlers (this file) and the htmx dashboard action handlers
// (dashboard.go) call them, so the load-bearing GetJob org-guard is never
// duplicated. They return domain values + an error; HTTP status/representation
// (JSON vs row fragment) is the caller's concern. A foreign/absent id resolves
// to store.ErrNotFound, which every caller maps to 404 with no side effect.

// enqueueJobRun proves the job belongs to orgID (404 on a foreign/absent id,
// BEFORE any run is created - store.EnqueueRun does NOT validate ownership), then
// enqueues one pending run and returns its id.
//
// Disabled-job behavior: this deliberately enqueues a run even when Job.Enabled
// is false. It is an intentional manual operator override: an operator can force
// a one-off run of a disabled job. The scheduler still respects Enabled and will
// NOT auto-schedule a disabled job; only this explicit action bypasses that
// filter.
func (s *Server) enqueueJobRun(ctx context.Context, orgID, id int64) (int64, error) {
	// Prove org ownership before enqueuing (EnqueueRun does not check it).
	if _, err := s.store.GetJob(ctx, orgID, id); err != nil {
		return 0, err
	}
	return s.store.EnqueueRun(ctx, orgID, id)
}

// setJobEnabledCore GetJob's the job org-scoped (ErrNotFound on a foreign/absent
// id, BEFORE any mutation), flips Enabled, persists it via UpdateJob, and returns
// the updated job.
//
// This runtime toggle is intentionally TRANSIENT: a later `sync` reconciles
// Enabled back to the config value, because config-as-code remains the source of
// truth for job definitions. Toggling here is an operational override, not an
// edit to the definition.
//
// TOCTOU window: the job existed at GetJob above but may have been deleted before
// UpdateJob runs. UpdateJob is org-scoped (WHERE org_id=? AND id=?) and returns
// ErrNotFound when 0 rows match, so a concurrent delete maps to 404 rather than
// leaking as a 500. For the run-now path the FK/insert handles the equivalent
// race at the DB layer.
func (s *Server) setJobEnabledCore(ctx context.Context, orgID, id int64, enabled bool) (jobs.Job, error) {
	j, err := s.store.GetJob(ctx, orgID, id)
	if err != nil {
		return jobs.Job{}, err
	}
	j.Enabled = enabled
	if err := s.store.UpdateJob(ctx, j); err != nil {
		return jobs.Job{}, err
	}
	return j, nil
}

// handleRunJobNow serves POST /api/jobs/{id}/run: it enqueues one pending run for
// the job and returns 202 with {"run_id": <id>}. The org-guard + enqueue live in
// the shared enqueueJobRun core (also used by the dashboard's htmx action).
func (s *Server) handleRunJobNow(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	runID, err := s.enqueueJobRun(r.Context(), p.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: run-now failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusAccepted, runEnqueuedDTO{RunID: runID})
}

// handleEnableJob serves POST /api/jobs/{id}/enable.
func (s *Server) handleEnableJob(w http.ResponseWriter, r *http.Request) {
	s.apiSetJobEnabled(w, r, true)
}

// handleDisableJob serves POST /api/jobs/{id}/disable.
func (s *Server) handleDisableJob(w http.ResponseWriter, r *http.Request) {
	s.apiSetJobEnabled(w, r, false)
}

// apiSetJobEnabled is the JSON-API wrapper around the shared setJobEnabledCore:
// it parses the request, calls the core, and returns the updated jobDTO (200).
func (s *Server) apiSetJobEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	j, err := s.setJobEnabledCore(r.Context(), p.OrgID, id, enabled)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.log.Error("api: enable/disable failed", "err", err)
		s.apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, fromJob(j))
}

// registerAPIRoutes registers the REST API on the given (already
// requireAuth-gated) mux. Called from Handler() at the Task 4/5 marker.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	// Read-only surface (Task 4).
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("GET /api/jobs/{id}/runs", s.handleListJobRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/channels", s.handleListChannels)
	mux.HandleFunc("GET /api/rules", s.handleListRules)
	mux.HandleFunc("GET /api/heartbeats", s.handleListHeartbeats)
	mux.HandleFunc("GET /api/heartbeats/{id}", s.handleGetHeartbeat)
	mux.HandleFunc("GET /api/heartbeats/{id}/history", s.handleHeartbeatHistory)
	mux.HandleFunc("POST /api/heartbeats", s.handleHeartbeatsNotCreatable)
	mux.HandleFunc("GET /api/events", s.handleListEvents)
	mux.HandleFunc("GET /api/secrets", s.handleListSecrets)

	// Action endpoints (Task 5) - the ONLY API mutations, all org-scoped.
	mux.HandleFunc("POST /api/jobs/{id}/run", s.handleRunJobNow)
	mux.HandleFunc("POST /api/jobs/{id}/enable", s.handleEnableJob)
	mux.HandleFunc("POST /api/jobs/{id}/disable", s.handleDisableJob)
}
