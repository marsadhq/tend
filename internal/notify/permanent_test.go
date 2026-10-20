package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPStatusErrorClassification pins which HTTP answers end a delivery at
// once: every 4xx except 408 and 429; never a 5xx.
func TestHTTPStatusErrorClassification(t *testing.T) {
	for code, permanent := range map[int]bool{
		400: true, 401: true, 403: true, 404: true, 410: true, 413: true, 422: true,
		408: false, 429: false, 500: false, 502: false, 503: false, 504: false, 302: false,
	} {
		if got := IsPermanent(httpStatusError("x", code)); got != permanent {
			t.Errorf("status %d: IsPermanent = %v, want %v", code, got, permanent)
		}
	}
	if IsPermanent(errors.New("plain")) || IsPermanent(nil) || Permanent(nil) != nil {
		t.Error("only errors marked by Permanent are permanent")
	}
}

// TestProvidersReportRejectionsAsPermanent drives every HTTP provider against
// a receiver that answers 400 (Ward's answer to a body it cannot read) and one
// that answers 503: the first is permanent, the second retryable.
func TestProvidersReportRejectionsAsPermanent(t *testing.T) {
	providers := func(url string) map[string]Provider {
		return map[string]Provider{
			"webhook":  NewWebhook(url),
			"slack":    NewSlack(url),
			"discord":  NewDiscord(url),
			"telegram": NewTelegram("tok", "1"),
		}
	}
	old := telegramAPIBase
	defer func() { telegramAPIBase = old }()
	for code, permanent := range map[int]bool{http.StatusBadRequest: true, http.StatusServiceUnavailable: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		telegramAPIBase = srv.URL
		for name, p := range providers(srv.URL) {
			err := p.Send(context.Background(), Message{Subject: "s", Body: "b"})
			if err == nil {
				t.Errorf("%s on %d: err = nil", name, code)
				continue
			}
			if got := IsPermanent(err); got != permanent {
				t.Errorf("%s on %d: IsPermanent = %v, want %v (%v)", name, code, got, permanent, err)
			}
		}
		srv.Close()
	}
}

// rejectingProvider refuses every send for good, the way a receiver that
// answers 400 to this notification does.
type rejectingProvider struct{ calls int }

func (p *rejectingProvider) Send(context.Context, Message) error {
	p.calls++
	return Permanent(errors.New("webhook POST ward.example: status 400"))
}

// TestWorkerGivesUpARejectedDeliveryAtOnce: a delivery refused for good is
// finalized on its first attempt, long before MaxAge, with notification.failed
// recorded; it is not rescheduled.
func TestWorkerGivesUpARejectedDeliveryAtOnce(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "run.failed", `{"job_id":1}`, now)}}
	p := &rejectingProvider{}
	w := newTestWorker(t, fs, p)
	w.now = func() time.Time { return now }

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("sends = %d, want 1", p.calls)
	}
	if len(fs.failed) != 1 || fs.failed[0] != 1 {
		t.Fatalf("failed deliveries = %v, want [1]", fs.failed)
	}
	if len(fs.emitted) != 1 || fs.emitted[0].Type != "notification.failed" || fs.emitted[0].Payload != "run.failed" {
		t.Fatalf("emitted = %+v, want one notification.failed for run.failed", fs.emitted)
	}
	if len(fs.rescheduled) != 0 {
		t.Errorf("a rejected delivery was rescheduled: %v", fs.rescheduled)
	}
}

// TestWorkerRejectedFailureReportIsNotReported keeps the loop guard for the
// immediate give-up too.
func TestWorkerRejectedFailureReportIsNotReported(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fs := &fakeWorkerStore{due: []Delivery{due(1, "notification.failed", "run.failed", now)}}
	w := newTestWorker(t, fs, &rejectingProvider{})
	w.now = func() time.Time { return now }

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(fs.failed) != 1 || len(fs.emitted) != 0 {
		t.Fatalf("failed=%v emitted=%+v, want the delivery failed and nothing recorded", fs.failed, fs.emitted)
	}
}
