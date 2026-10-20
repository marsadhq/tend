package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rdapAnswer is a registry's answer for a domain, as a fake RDAP server
// serves it.
func rdapAnswer(domain string, events map[string]string, status ...string) map[string]any {
	var evs []map[string]string
	for action, date := range events {
		evs = append(evs, map[string]string{"eventAction": action, "eventDate": date})
	}
	return map[string]any{
		"objectClassName": "domain",
		"ldhName":         strings.ToUpper(domain),
		"status":          status,
		"events":          evs,
	}
}

// fakeRDAP serves answers by domain under /domain/<name> and counts requests.
type fakeRDAP struct {
	srv     *httptest.Server
	answers map[string]any // domain -> JSON body; a missing domain is a 404
	code    int            // when non-zero, every answer is this status
	hits    atomic.Int64
	delay   time.Duration
}

func newFakeRDAP(t *testing.T) *fakeRDAP {
	t.Helper()
	f := &fakeRDAP{answers: map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/rdap+json") {
			http.Error(w, "want rdap+json", http.StatusNotAcceptable)
			return
		}
		if f.code != 0 {
			http.Error(w, "nope", f.code)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/rdap/domain/")
		body, known := f.answers[name]
		if !ok || !known {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/rdap+json")
		if s, isString := body.(string); isString {
			fmt.Fprint(w, s)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRDAP) base() string { return f.srv.URL + "/rdap/" }

// fakeBootstrap serves an IANA-style dns.json naming base for the given TLDs,
// and counts how often it is fetched.
type fakeBootstrap struct {
	srv  *httptest.Server
	hits atomic.Int64
	down atomic.Bool
}

func newFakeBootstrap(t *testing.T, base string, tlds ...string) *fakeBootstrap {
	t.Helper()
	f := &fakeBootstrap{}
	doc := map[string]any{
		"version":     "1.0",
		"publication": "2026-10-01T00:00:00Z",
		"services": [][][]string{
			{tlds, {base}},
			{{"other"}, {"https://rdap.other.invalid/"}},
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// domainSetup is a fake registry for .test and .example and an executor that
// finds it through a fake bootstrap file. (The fake serves plain http, which
// the probe uses when a TLD's entry lists no https:// URL.)
func domainSetup(t *testing.T) (*fakeRDAP, *fakeBootstrap, *Executor) {
	t.Helper()
	rdap := newFakeRDAP(t)
	boot := newFakeBootstrap(t, rdap.base(), "test", "example")
	ex := &Executor{RDAPBootstrapURL: boot.srv.URL + "/dns.json"}
	return rdap, boot, ex
}

func domainJob(domain string, warnDays int) Job {
	return Job{Name: "domain", Type: Domain, TimeoutSeconds: 5, Spec: &Spec{Domain: domain, WarnDays: warnDays}}
}

func TestRDAPBootstrapLookup(t *testing.T) {
	b := rdapBootstrap{services: []rdapService{
		{tlds: []string{"uk"}, urls: []string{"http://rdap.uk.invalid/", "https://rdap.uk.invalid/"}},
		{tlds: []string{"co.uk"}, urls: []string{"https://rdap.co-uk.invalid"}},
		{tlds: []string{"COM", "net"}, urls: []string{"https://rdap.verisign.invalid/com/v1/"}},
	}}
	cases := map[string]string{
		"example.com":        "https://rdap.verisign.invalid/com/v1/",
		"www.example.net":    "https://rdap.verisign.invalid/com/v1/",
		"example.uk":         "https://rdap.uk.invalid/",
		"example.co.uk":      "https://rdap.co-uk.invalid/",
		"deep.example.co.uk": "https://rdap.co-uk.invalid/",
	}
	for domain, want := range cases {
		got, ok := b.lookup(domain)
		if !ok || got != want {
			t.Errorf("lookup(%q) = %q, %v; want %q", domain, got, ok, want)
		}
	}
	if got, ok := b.lookup("example.de"); ok {
		t.Errorf("lookup(example.de) = %q, want no server", got)
	}
}

func TestParseRDAPDate(t *testing.T) {
	want := time.Date(2027, 8, 13, 4, 0, 0, 0, time.UTC)
	for _, s := range []string{"2027-08-13T04:00:00Z", "2027-08-13T04:00:00.000Z", "2027-08-13T06:00:00+02:00", "2027-08-13T04:00:00"} {
		got, err := parseRDAPDate(s)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseRDAPDate(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	if got, err := parseRDAPDate("2027-08-13"); err != nil || !got.Equal(time.Date(2027, 8, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parseRDAPDate(date only) = %v, %v", got, err)
	}
	for _, s := range []string{"", "soon", "13/08/2027", "2027-13-01T00:00:00Z"} {
		if _, err := parseRDAPDate(s); err == nil {
			t.Errorf("parseRDAPDate(%q) = nil error", s)
		}
	}
}

func TestDomainProbeOK(t *testing.T) {
	rdap, _, ex := domainSetup(t)
	exp := time.Now().Add(200 * day).UTC().Truncate(time.Second)
	rdap.answers["example.test"] = rdapAnswer("example.test", map[string]string{
		"registration": "2001-01-01T00:00:00Z",
		"expiration":   exp.Format(time.RFC3339),
		"last changed": "2026-01-01T00:00:00Z",
	}, "client transfer prohibited", "active")

	res := ex.Run(context.Background(), domainJob("Example.Test.", 30), nil)
	if res.Status != StatusSucceeded || res.ExitCode != 0 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	for _, want := range []string{
		"ok: example.test expires in 199 days",
		"domain:", "example.test",
		"rdap:", rdap.base() + "domain/example.test",
		"expiration:", exp.Format(time.RFC3339),
		"days_left:", "199",
		"status:", "client transfer prohibited, active",
		"registered:", "2001-01-01T00:00:00Z",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestDomainProbeFailures(t *testing.T) {
	future := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	cases := []struct {
		name   string
		answer any // nil: no record (404)
		exit   int
		want   string
	}{
		{"expiring", rdapAnswer("example.test", map[string]string{"expiration": future(20 * day)}), 1, "expires in 19 days"},
		{"expired", rdapAnswer("example.test", map[string]string{"expiration": future(-3 * day)}), 1, "expired 3 days ago"},
		{"no expiration", rdapAnswer("example.test", map[string]string{"registration": "2001-01-01T00:00:00Z"}), 1, "no expiration date"},
		{"bad date", rdapAnswer("example.test", map[string]string{"expiration": "next year"}), 1, "not a date"},
		{"lapsed", rdapAnswer("example.test", map[string]string{"expiration": future(100 * day)}, "redemption period"), 1, `"redemption period"`},
		{"not registered", nil, 1, "no record of example.test"},
		{"not json", "<html>hello</html>", -1, "not an RDAP answer"},
		{"not a domain", map[string]any{"objectClassName": "entity"}, -1, "not an RDAP answer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rdap, _, ex := domainSetup(t)
			if c.answer != nil {
				rdap.answers["example.test"] = c.answer
			}
			res := ex.Run(context.Background(), domainJob("example.test", 30), nil)
			if res.Status != StatusFailed || res.ExitCode != c.exit {
				t.Fatalf("status %s exit %d (want failed %d), output:\n%s", res.Status, res.ExitCode, c.exit, res.Output)
			}
			if !strings.Contains(res.Output, c.want) {
				t.Fatalf("output lacks %q:\n%s", c.want, res.Output)
			}
		})
	}
}

func TestDomainProbeRegistryErrors(t *testing.T) {
	rdap, _, ex := domainSetup(t)
	rdap.code = http.StatusTooManyRequests
	res := ex.Run(context.Background(), domainJob("example.test", 30), nil)
	if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, "429") {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
}

func TestDomainProbeTLDWithoutRDAP(t *testing.T) {
	_, _, ex := domainSetup(t)
	res := ex.Run(context.Background(), domainJob("example.de", 30), nil)
	if res.Status != StatusFailed || res.ExitCode != -1 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	for _, want := range []string{`".de"`, "rdap_url", "expiry job"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestDomainProbeRDAPURLOverride(t *testing.T) {
	rdap, boot, ex := domainSetup(t)
	rdap.answers["example.de"] = rdapAnswer("example.de", map[string]string{"expiration": time.Now().Add(90 * day).UTC().Format(time.RFC3339)})
	j := domainJob("example.de", 30)
	j.Spec.RDAPURL = strings.TrimSuffix(rdap.base(), "/") // the missing slash is added
	res := ex.Run(context.Background(), j, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if n := boot.hits.Load(); n != 0 {
		t.Fatalf("the bootstrap file was fetched %d times with rdap_url set", n)
	}
}

func TestRDAPBootstrapCached(t *testing.T) {
	rdap, boot, ex := domainSetup(t)
	rdap.answers["example.test"] = rdapAnswer("example.test", map[string]string{"expiration": time.Now().Add(90 * day).UTC().Format(time.RFC3339)})
	now := time.Now()
	ex.Now = func() time.Time { return now }

	for range 3 {
		if res := ex.Run(context.Background(), domainJob("example.test", 30), nil); res.Status != StatusSucceeded {
			t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
		}
	}
	if n := boot.hits.Load(); n != 1 {
		t.Fatalf("bootstrap fetched %d times for three probes, want 1", n)
	}

	// A day later it is fetched again.
	now = now.Add(25 * time.Hour)
	if res := ex.Run(context.Background(), domainJob("example.test", 30), nil); res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if n := boot.hits.Load(); n != 2 {
		t.Fatalf("bootstrap fetched %d times after a day, want 2", n)
	}

	// When IANA cannot be reached the copy held is used, and the output says so.
	boot.down.Store(true)
	now = now.Add(25 * time.Hour)
	res := ex.Run(context.Background(), domainJob("example.test", 30), nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "bootstrap:") || !strings.Contains(res.Output, "503") {
		t.Fatalf("output does not say the cached bootstrap file was used:\n%s", res.Output)
	}
}

func TestRDAPBootstrapUnreachable(t *testing.T) {
	_, boot, ex := domainSetup(t)
	boot.down.Store(true)
	res := ex.Run(context.Background(), domainJob("example.test", 30), nil)
	if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, "bootstrap") {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
}

func TestDomainProbeTimeout(t *testing.T) {
	rdap, _, ex := domainSetup(t)
	rdap.delay = 10 * time.Second
	j := domainJob("example.test", 30)
	j.TimeoutSeconds = 1
	start := time.Now()
	res := ex.Run(context.Background(), j, nil)
	if res.Status != StatusTimedOut {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("took %v to time out after 1s", took)
	}
}

func TestDomainProbeAnswerTooLarge(t *testing.T) {
	rdap, _, ex := domainSetup(t)
	rdap.answers["example.test"] = `{"objectClassName":"domain","x":"` + strings.Repeat("a", maxRDAPAnswerBytes) + `"}`
	res := ex.Run(context.Background(), domainJob("example.test", 30), nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "too large") {
		t.Fatalf("status %s, output:\n%.300s", res.Status, res.Output)
	}
}

func TestValidateSpecDomain(t *testing.T) {
	for _, d := range []string{"example.com", "xn--bcher-kva.example", "a-b.co.uk", "Example.COM."} {
		if err := ValidateSpec(Domain, &Spec{Domain: d, WarnDays: 30}); err != nil {
			t.Errorf("ValidateSpec(domain %q) = %v", d, err)
		}
	}
	bad := []struct {
		spec Spec
		want string
	}{
		{Spec{}, "domain is required"},
		{Spec{Domain: "localhost"}, "not a registered domain"},
		{Spec{Domain: "bücher.example"}, "xn--"},
		{Spec{Domain: "-bad.example"}, "not a domain name"},
		{Spec{Domain: "exa_mple.com"}, "not a domain name"},
		{Spec{Domain: "https://example.com"}, "not a domain name"},
		{Spec{Domain: "example.com", RDAPURL: "rdap.example"}, "rdap_url"},
		{Spec{Domain: "example.com", RDAPURL: "ftp://rdap.example/"}, "rdap_url"},
		{Spec{Domain: "example.com", WarnDays: -1}, "warn_days"},
	}
	for _, c := range bad {
		err := ValidateSpec(Domain, &c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateSpec(%+v) = %v, want an error about %q", c.spec, err, c.want)
		}
	}
	if got, _ := NormalizeDomain("Example.COM."); got != "example.com" {
		t.Errorf("NormalizeDomain = %q", got)
	}
}
