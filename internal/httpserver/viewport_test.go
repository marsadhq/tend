package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/httpserver"
)

// viewportMetaRe finds the viewport declaration of a page.
var viewportMetaRe = regexp.MustCompile(`<meta name="viewport" content="([^"]*)">`)

// checkViewport fails unless body declares the mobile viewport exactly once,
// in its head, without turning off pinch zoom.
func checkViewport(t *testing.T, page, body string) {
	t.Helper()
	all := viewportMetaRe.FindAllStringSubmatchIndex(body, -1)
	if len(all) != 1 {
		t.Errorf("%s: want exactly one viewport meta, found %d", page, len(all))
		return
	}
	if head := strings.Index(body, "</head>"); head < 0 || all[0][0] > head {
		t.Errorf("%s: the viewport meta is not in the head", page)
	}
	content := body[all[0][2]:all[0][3]]
	for _, want := range []string{"width=device-width", "initial-scale=1"} {
		if !strings.Contains(content, want) {
			t.Errorf("%s: viewport %q lacks %q", page, content, want)
		}
	}
	// Zoom stays allowed: a phone user must be able to enlarge the output.
	for _, banned := range []string{"user-scalable", "maximum-scale"} {
		if strings.Contains(content, banned) {
			t.Errorf("%s: viewport %q must not set %s", page, content, banned)
		}
	}
}

// TestEveryPageSetsTheViewport: every page template, and the login form, tells
// a phone to lay the page out at the phone's own width. Without it a phone
// renders a zoomed-out desktop page. The test fails for a page template it
// does not render, so a new page cannot skip the check.
func TestEveryPageSetsTheViewport(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "viewport-job")
	runID := seedRun(t, ts, ts.orgID, jobID, "failed", "boom")
	hb := seedPings(t, ts, "viewport-hb", "tok-viewport")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	pages := map[string]string{ // page template -> a path that renders it
		"jobs.html":             "/",
		"job_detail.html":       "/jobs/" + id(jobID),
		"run_detail.html":       "/runs/" + id(runID),
		"heartbeats.html":       "/heartbeats",
		"heartbeat_detail.html": "/heartbeats/" + id(hb.ID),
		"events.html":           "/events",
	}

	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); name != "base.html" && pages[name] == "" {
			t.Errorf("templates/%s: no path in this test renders it; add one", name)
		}
	}

	for tmpl, path := range pages {
		rec := dashGet(t, h, cookie, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s (%s): status %d, want 200", tmpl, path, rec.Code)
			continue
		}
		checkViewport(t, tmpl+" ("+path+")", rec.Body.String())
	}

	// The login form has a template of its own.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, want 200", rec.Code)
	}
	checkViewport(t, "login", rec.Body.String())
}

// TestDashboardTablesKeepTheirRoles: on a phone the tables are shown as cards
// (display: block and flex), which makes some browsers drop their table
// semantics, so every table, row group, row, header and cell carries its
// role explicitly and screen readers keep the structure. The row fragment the
// htmx actions return is checked too.
func TestDashboardTablesKeepTheirRoles(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	jobID := seedJob(t, ts, ts.orgID, "roles-job")
	seedRun(t, ts, ts.orgID, jobID, "failed", "boom")
	hb := seedPings(t, ts, "roles-hb", "tok-roles")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, csrf := validSessionCookie(t, as, codec)

	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	bodies := map[string]string{}
	for _, path := range []string{"/", "/partials/jobs", "/jobs/" + id(jobID), "/heartbeats", "/heartbeats/" + id(hb.ID), "/events"} {
		bodies[path] = dashGet(t, h, cookie, path).Body.String()
	}
	bodies["run-now fragment"] = dashPostCSRF(t, h, cookie, csrf, "/jobs/"+id(jobID)+"/run").Body.String()

	for _, tc := range []struct {
		tag  *regexp.Regexp
		role string
	}{
		{regexp.MustCompile(`<table[ >]`), `<table role="table"`},
		{regexp.MustCompile(`<thead[ >]`), `<thead role="rowgroup"`},
		{regexp.MustCompile(`<tbody[ >]`), `<tbody role="rowgroup"`},
		{regexp.MustCompile(`<tr[ >]`), `<tr role="row"`},
		{regexp.MustCompile(`<th[ >]`), `<th role="columnheader"`},
		{regexp.MustCompile(`<td[ >]`), `<td role="cell"`},
	} {
		for page, body := range bodies {
			if all, with := len(tc.tag.FindAllString(body, -1)), strings.Count(body, tc.role); all != with {
				t.Errorf("%s: %d of %d %s elements lack %s", page, all-with, all, tc.tag, tc.role)
			}
		}
	}
	if strings.Count(bodies["/"], `<td role="cell"`) == 0 || strings.Count(bodies["/heartbeats/"+id(hb.ID)], `<th role="columnheader"`) == 0 {
		t.Fatal("the pages rendered no tables; the check above proved nothing")
	}
}
