package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IANABootstrapURL is IANA's RDAP bootstrap file for domain names (RFC 9224):
// which RDAP server answers for each TLD.
const IANABootstrapURL = "https://data.iana.org/rdap/dns.json"

// rdapBootstrapTTL is how long a fetched bootstrap file is used before it is
// fetched again. IANA updates it a few times a month at most.
const rdapBootstrapTTL = 24 * time.Hour

// Caps on what a domain probe reads: an RDAP answer for one domain is a few
// kB, the bootstrap file a few tens of kB.
const (
	maxRDAPAnswerBytes    = 1 << 20
	maxRDAPBootstrapBytes = 4 << 20
)

// rdapService is one entry of the bootstrap file: the TLDs (or longer
// suffixes) it covers and the base URLs of their RDAP server.
type rdapService struct {
	tlds []string
	urls []string
}

// rdapBootstrap is a parsed bootstrap file and when it was fetched.
type rdapBootstrap struct {
	fetched  time.Time
	services []rdapService
}

// service returns the bootstrap entry for domain: the one of the longest
// suffix of domain it lists.
func (b rdapBootstrap) service(domain string) (rdapService, bool) {
	labels := strings.Split(domain, ".")
	for i := 1; i < len(labels); i++ {
		suffix := strings.Join(labels[i:], ".")
		for _, svc := range b.services {
			for _, tld := range svc.tlds {
				if strings.EqualFold(tld, suffix) {
					return svc, true
				}
			}
		}
	}
	return rdapService{}, false
}

// lookup returns the base URL of the RDAP server for domain: the https:// URL
// of its entry. An entry with plain-http URLs only gives none: an answer that
// anyone on the path can change is not taken from a file, only from an
// rdap_url the user wrote down (see listsPlainHTTP).
func (b rdapBootstrap) lookup(domain string) (string, bool) {
	svc, ok := b.service(domain)
	if !ok {
		return "", false
	}
	for _, u := range svc.urls {
		if strings.HasPrefix(strings.ToLower(u), "https://") {
			return withSlash(u), true
		}
	}
	return "", false
}

// listsPlainHTTP reports whether domain's entry exists but lists plain-http
// servers only.
func (b rdapBootstrap) listsPlainHTTP(domain string) bool {
	svc, ok := b.service(domain)
	return ok && len(svc.urls) > 0
}

func withSlash(u string) string {
	if strings.HasSuffix(u, "/") {
		return u
	}
	return u + "/"
}

// rdapBootstraps caches the bootstrap file by the URL it came from, for every
// executor in the process (serve has one; tests point theirs at their own
// fake file).
var rdapBootstraps = struct {
	sync.Mutex
	byURL map[string]rdapBootstrap
}{byURL: map[string]rdapBootstrap{}}

// rdapBase returns the base URL of the RDAP server for domain, from the
// bootstrap file at e.RDAPBootstrapURL (IANA's when empty). The file is
// fetched once a day; when fetching it again fails, the copy held is used and
// a note about it is added to r.
func (e *Executor) rdapBase(ctx context.Context, domain string, r *probeReport) (string, error) {
	src := e.RDAPBootstrapURL
	if src == "" {
		src = IANABootstrapURL
	}
	now := e.now()
	rdapBootstraps.Lock()
	b, held := rdapBootstraps.byURL[src]
	rdapBootstraps.Unlock()

	if !held || now.Sub(b.fetched) >= rdapBootstrapTTL || now.Before(b.fetched) {
		fresh, err := fetchBootstrap(ctx, e.rdapClient(), src)
		switch {
		case err == nil:
			fresh.fetched = now
			rdapBootstraps.Lock()
			rdapBootstraps.byURL[src] = fresh
			rdapBootstraps.Unlock()
			b = fresh
		case held:
			r.add("bootstrap", "the copy fetched %s was used; fetching it again failed: %v", dateText(b.fetched), err)
		default:
			return "", fmt.Errorf("RDAP bootstrap file: %w", err)
		}
	}

	base, ok := b.lookup(domain)
	if !ok {
		tld := "." + domain[strings.LastIndex(domain, ".")+1:]
		if b.listsPlainHTTP(domain) {
			return "", fmt.Errorf("the IANA bootstrap file lists only plain http RDAP servers for %q, which are not used unless asked for: set rdap_url to one to use it anyway", tld)
		}
		return "", fmt.Errorf("no RDAP server for %q in the IANA bootstrap file; some TLDs have none: set rdap_url if the registry runs one, or watch the date with an expiry job", tld)
	}
	return base, nil
}

// maxRDAPRedirects bounds how many redirects one RDAP request follows.
const maxRDAPRedirects = 5

// rdapClient is the HTTP client of the domain probes: e.RDAPTransport (or
// Go's default transport) with a redirect policy that keeps a request on the
// host it was made to and on https once it started there. A registry's
// server redirecting elsewhere, or to plain http, would let another party
// answer for the registry; such a redirect fails the request.
func (e *Executor) rdapClient() *http.Client {
	return &http.Client{
		Transport: e.RDAPTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			first := via[0].URL
			switch {
			case len(via) >= maxRDAPRedirects:
				return fmt.Errorf("stopped after %d redirects", len(via))
			case first.Scheme == "https" && req.URL.Scheme != "https":
				return fmt.Errorf("refused a redirect from https to http (%s)", req.URL.Redacted())
			case !strings.EqualFold(req.URL.Host, first.Host):
				return fmt.Errorf("refused a redirect to another host (%s)", req.URL.Host)
			}
			return nil
		},
	}
}

// fetchBootstrap fetches and parses a bootstrap file.
func fetchBootstrap(ctx context.Context, client *http.Client, src string) (rdapBootstrap, error) {
	_, body, err := getRDAP(ctx, client, src, maxRDAPBootstrapBytes)
	if err != nil {
		return rdapBootstrap{}, err
	}
	var doc struct {
		Services [][][]string `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return rdapBootstrap{}, fmt.Errorf("%s is not a bootstrap file: %v", src, err)
	}
	var b rdapBootstrap
	for _, svc := range doc.Services {
		if len(svc) >= 2 {
			b.services = append(b.services, rdapService{tlds: svc[0], urls: svc[1]})
		}
	}
	if len(b.services) == 0 {
		return rdapBootstrap{}, fmt.Errorf("%s lists no RDAP services", src)
	}
	return b, nil
}

// getRDAP fetches url with client and RDAP's media type and returns the
// status and, for a 200, the body (at most limit bytes; a longer one is an
// error).
func getRDAP(ctx context.Context, client *http.Client, url string, limit int) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read the answer from %s: %w", url, err)
	}
	if len(body) > limit {
		return resp.StatusCode, nil, fmt.Errorf("the answer from %s is too large (over %d bytes)", url, limit)
	}
	return resp.StatusCode, body, nil
}

// rdapDomainAnswer is the part of an RDAP domain answer (RFC 9083) a domain
// probe reads.
type rdapDomainAnswer struct {
	ObjectClassName string   `json:"objectClassName"`
	Status          []string `json:"status"`
	Events          []struct {
		Action string `json:"eventAction"`
		Date   string `json:"eventDate"`
	} `json:"events"`
}

// lapsedStatuses are the RDAP statuses (RFC 8056) of a registration that has
// already expired: the domain is on its way to deletion.
var lapsedStatuses = map[string]bool{
	"redemption period": true,
	"pending delete":    true,
	"pending restore":   true,
}

// event returns the date of the first event with action, or "".
func (a rdapDomainAnswer) event(action string) string {
	for _, ev := range a.Events {
		if strings.EqualFold(ev.Action, action) {
			return ev.Date
		}
	}
	return ""
}

// checkDomain judges a registry's answer: whether it gives an expiration date,
// whether that date is within warnDays, and whether the domain's status says
// it has lapsed. It returns the expiration date, zero when there is none.
func checkDomain(a rdapDomainAnswer, now time.Time, warnDays int) (probeReport, time.Time) {
	var r probeReport
	raw, which := a.event("expiration"), ""
	if raw == "" {
		if raw = a.event("registrar expiration"); raw != "" {
			which = " (the registrar's expiration; the registry gives none)"
		}
	}
	var exp time.Time
	switch t, err := parseRDAPDate(raw); {
	case raw == "":
		r.fail("the registry's RDAP answer has no expiration date; some registries leave it out (watch the date with an expiry job instead)")
	case err != nil:
		r.fail("the expiration date %q is not a date", raw)
	default:
		exp = t
		left := exp.Sub(now)
		r.add("expiration", "%s%s", dateText(exp), which)
		r.add("days_left", "%d", floorDays(left))
		switch {
		case left <= 0:
			r.fail("registration expired %s ago (%s)", humanDays(left), dateText(exp))
		case left <= time.Duration(warnDays)*day:
			r.fail("registration expires in %s (%s), within warn_days %d", humanDays(left), dateText(exp), warnDays)
		}
	}
	for _, st := range a.Status {
		if lapsedStatuses[strings.ToLower(st)] {
			r.fail("the registry lists the domain as %q: its registration has lapsed", st)
		}
	}
	if len(a.Status) > 0 {
		r.add("status", "%s", strings.Join(a.Status, ", "))
	}
	if reg, err := parseRDAPDate(a.event("registration")); err == nil {
		r.add("registered", "%s", dateText(reg))
	}
	if changed, err := parseRDAPDate(a.event("last changed")); err == nil {
		r.add("changed", "%s", dateText(changed))
	}
	return r, exp
}

// parseRDAPDate parses an RDAP event date. RFC 9083 asks for RFC 3339; some
// servers leave the zone out (read as UTC) or give a bare date.
func parseRDAPDate(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC 3339 date", s)
}

// runOnceDomain runs the domain branch of runOnce: find the registry's RDAP
// server, ask it about the domain and judge the answer.
func (e *Executor) runOnceDomain(ctx context.Context, j Job, attempt int, started time.Time) RunResult {
	s := specOf(j)
	if err := ValidateSpec(Domain, &s); err != nil {
		return probeErrorResult(ctx, attempt, started, err)
	}
	domain, _ := NormalizeDomain(s.Domain) // valid: checked just above
	var r probeReport
	r.add("domain", "%s", domain)

	base := s.RDAPURL
	if base == "" {
		var err error
		if base, err = e.rdapBase(ctx, domain, &r); err != nil {
			return probeErrorResult(ctx, attempt, started, err)
		}
	}
	url := withSlash(base) + "domain/" + domain
	r.add("rdap", "%s", url)

	status, body, err := getRDAP(ctx, e.rdapClient(), url, maxRDAPAnswerBytes)
	if status == http.StatusNotFound {
		r.fail("the registry has no record of %s (HTTP 404): it is not registered, or was deleted after it lapsed", domain)
		return r.result(attempt, started, "")
	}
	if err != nil {
		return probeErrorResult(ctx, attempt, started, err)
	}
	var answer rdapDomainAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return probeErrorResult(ctx, attempt, started, fmt.Errorf("not an RDAP answer from %s: %v", url, err))
	}
	if answer.ObjectClassName != "domain" {
		return probeErrorResult(ctx, attempt, started,
			fmt.Errorf("not an RDAP answer for a domain from %s (objectClassName %q)", url, answer.ObjectClassName))
	}

	now := e.now()
	checked, exp := checkDomain(answer, now, s.WarnDays)
	r.problems = checked.problems
	// The bootstrap note, when there is one, goes last.
	head, note := r.info[:2], r.info[2:]
	r.info = append(append(append([]string(nil), head...), checked.info...), note...)
	return r.result(attempt, started, fmt.Sprintf("%s expires in %s (%s; %s)",
		domain, humanDays(exp.Sub(now)), dateText(exp), warnText(s.WarnDays)))
}

// errDomainRequired is ValidateSpec's complaint about a domain job without one.
var errDomainRequired = errors.New("domain is required for domain jobs")
