package httpserver_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/httpserver"
)

// TestJobsPage_HasAriaLiveRegion verifies the jobs table's auto-polling
// container is wrapped in a labelled aria-live region, so screen-reader users
// are notified of the 5s htmx swap instead of getting silent updates.
func TestJobsPage_HasAriaLiveRegion(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "aria-job")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	rec := dashGet(t, h, cookie, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `aria-live="polite"`) {
		t.Fatalf("jobs page missing aria-live=\"polite\"; body=%q", body)
	}
	if !strings.Contains(body, `role="region"`) {
		t.Fatalf("jobs page missing a labelled aria-live region (role=\"region\"); body=%q", body)
	}
}

// TestJobsPage_PauseTogglesPolling verifies GET / carries the htmx polling
// trigger by default, and GET /?static=1 omits it (WCAG 2.2.2 pause control)
// while offering a resume affordance back to live mode.
func TestJobsPage_PauseTogglesPolling(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	seedJob(t, ts, ts.orgID, "pause-job")
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)

	live := dashGet(t, h, cookie, "/")
	if live.Code != http.StatusOK {
		t.Fatalf("live status: got %d want 200; body=%q", live.Code, live.Body.String())
	}
	if !strings.Contains(live.Body.String(), `hx-trigger="every 5s"`) {
		t.Fatalf("live page missing hx-trigger=\"every 5s\"; body=%q", live.Body.String())
	}
	if !strings.Contains(live.Body.String(), `aria-label="Jobs (auto-updating every 5 seconds)"`) {
		t.Errorf("live page's region label should announce the auto-update; body=%q", live.Body.String())
	}

	paused := dashGet(t, h, cookie, "/?static=1")
	if paused.Code != http.StatusOK {
		t.Fatalf("paused status: got %d want 200; body=%q", paused.Code, paused.Body.String())
	}
	pausedBody := paused.Body.String()
	if strings.Contains(pausedBody, `hx-trigger="every 5s"`) {
		t.Fatalf("paused page still carries hx-trigger=\"every 5s\"; body=%q", pausedBody)
	}
	if !strings.Contains(strings.ToLower(pausedBody), "resume") {
		t.Fatalf("paused page missing a resume affordance; body=%q", pausedBody)
	}
	// The region label must not keep announcing an auto-update that is off.
	if strings.Contains(pausedBody, "auto-updating") || !strings.Contains(pausedBody, `aria-label="Jobs (updates paused)"`) {
		t.Errorf("paused page's region label should say updates are paused; body=%q", pausedBody)
	}
}
