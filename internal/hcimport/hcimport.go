// Package hcimport is `tend import healthchecks`: it reads a project's checks
// from the Healthchecks Management API (v3) and turns them into Tend
// heartbeats that keep each check's UUID as their ping token, so a client
// moves over by changing only the base of its ping URL
// (https://hc-ping.com/<uuid> becomes <TEND_BASE_URL>/ping/<uuid>).
//
// A simple check maps directly: its timeout is the heartbeat's period and its
// grace the heartbeat's grace. Tend heartbeats have no schedule, so a cron
// check gets a period that approximates its schedule (the longest gap
// between two of its runs) and a warning that says what that means.
package hcimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/marsadhq/kit/urlredact"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/robfig/cron/v3"
)

// Check is one check of the Management API's check list, the fields the
// import uses.
type Check struct {
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	UUID    string `json:"uuid"`     // read-write keys only
	PingURL string `json:"ping_url"` // read-write keys only
	// UniqueKey replaces uuid and ping_url for a read-only key.
	UniqueKey string `json:"unique_key"`
	// Timeout is a simple check's period, in seconds.
	Timeout int `json:"timeout"`
	// Grace is the grace time, in seconds.
	Grace int `json:"grace"`
	// Schedule and TZ are a cron check's schedule and its time zone.
	Schedule string `json:"schedule"`
	TZ       string `json:"tz"`
	// Status is "new", "up", "grace", "down" or "paused".
	Status string `json:"status"`
}

// ID returns the check's UUID in lower case, from its uuid field or else the
// last segment of its ping URL, or "" when it has neither.
func (c Check) ID() string {
	if u, ok := heartbeat.CanonicalUUID(c.UUID); ok {
		return u
	}
	if p, err := url.Parse(c.PingURL); err == nil {
		if u, ok := heartbeat.CanonicalUUID(path.Base(p.Path)); ok {
			return u
		}
	}
	return ""
}

// maxListBytes bounds the check list read from the API.
const maxListBytes = 32 << 20

// fetchTimeout bounds the API call.
const fetchTimeout = 30 * time.Second

// ErrReadOnlyKey is returned by Fetch for a read-only API key.
var ErrReadOnlyKey = errors.New("this is a read-only API key: Healthchecks does not give it the checks' UUIDs, which are what keeps their ping URLs working; use the project's read-write API key")

// Fetch lists the checks of the project apiKey belongs to, from the
// Healthchecks instance at baseURL (https://healthchecks.io, or a
// self-hosted one; a URL that already ends in /api/v3 is used as it is).
// client nil means a default client. Errors never quote the key.
func Fetch(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]Check, error) {
	if apiKey == "" {
		return nil, errors.New("no API key")
	}
	endpoint, err := checksURL(baseURL)
	if err != nil {
		return nil, err
	}
	// Go copies a custom header like X-Api-Key onto every redirect, so one
	// to another host would hand that host the key: refuse it. (A redirect
	// on the same host, http to https say, is followed.)
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing to follow a redirect to another host (%s) with the API key", req.URL.Host)
		}
		return nil
	}
	client = &c
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, urlredact.Error(endpoint, err)
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list checks at %s: %w", urlredact.Host(endpoint), urlredact.Error(endpoint, err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("list checks at %s: the API key was refused (HTTP 401)", urlredact.Host(endpoint))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("list checks at %s: HTTP %d", urlredact.Host(endpoint), resp.StatusCode)
	}
	var body struct {
		Checks []Check `json:"checks"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("list checks at %s: not a check list: %w", urlredact.Host(endpoint), err)
	}
	for _, c := range body.Checks {
		if c.ID() == "" && c.UniqueKey != "" {
			return nil, ErrReadOnlyKey
		}
	}
	return body.Checks, nil
}

// checksURL is the check list endpoint under baseURL.
func checksURL(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the Healthchecks URL must be an absolute http:// or https:// URL, like https://healthchecks.io")
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/api/v3") {
		p += "/api/v3"
	}
	u.Path, u.RawQuery, u.Fragment = p+"/checks/", "", ""
	return u.String(), nil
}

// What the import does with one check.
const (
	OpCreate    = "create"    // a new heartbeat
	OpUpdate    = "update"    // the heartbeat imported before, its period or grace changed
	OpUnchanged = "unchanged" // the heartbeat imported before, nothing to change
	OpSkip      = "skip"      // nothing; Reason says why
)

// Action is the plan for one check.
type Action struct {
	Check Check
	Op    string
	// Name, Token, PeriodSeconds and GraceSeconds are the heartbeat the
	// check becomes (for an update, its existing name).
	Name          string
	Token         string
	PeriodSeconds int
	GraceSeconds  int
	// Was is the heartbeat as it is before an update or unchanged.
	Was *heartbeat.Heartbeat
	// Reason says why a check is skipped.
	Reason string
	// Warnings say where the heartbeat differs from the check.
	Warnings []string
}

// Options tune Plan.
type Options struct {
	// Now anchors the sampling of cron schedules.
	Now time.Time
	// IncludePaused imports paused checks too.
	IncludePaused bool
}

// defaultPeriod is the period of a check whose schedule cannot be read.
const defaultPeriod = 24 * time.Hour

// Plan decides what each check becomes, given the org's existing
// heartbeats. A check whose UUID is already a heartbeat's token is that
// heartbeat (updated, or unchanged); otherwise a heartbeat is created, named
// after the check's slug (which keeps slug ping URLs working), else its name,
// else its UUID. A name already taken by a heartbeat with another token is
// skipped rather than taken over, and a name two checks share gets the
// second one's UUID prefix.
func Plan(checks []Check, existing []heartbeat.Heartbeat, opts Options) []Action {
	byToken := map[string]heartbeat.Heartbeat{}
	byName := map[string]bool{}
	for _, hb := range existing {
		byToken[hb.Token] = hb
		byName[hb.Name] = true
	}
	planned := map[string]bool{}
	out := make([]Action, 0, len(checks))
	for _, c := range checks {
		a := Action{Check: c, Token: c.ID()}
		// name is the heartbeat a new check becomes; label names the check
		// in warnings the way Healthchecks shows it.
		name := firstOf(c.Slug, c.Name, a.Token)
		label := firstOf(c.Name, c.Slug, a.Token)
		if a.Token == "" {
			a.Op, a.Name, a.Reason = OpSkip, name, "no UUID in the API's answer"
			out = append(out, a)
			continue
		}
		a.PeriodSeconds, a.GraceSeconds, a.Warnings = mapTiming(c, label, opts.Now)
		if c.Status == "paused" {
			if !opts.IncludePaused {
				a.Op, a.Name, a.Reason = OpSkip, name, "paused in Healthchecks (--include-paused imports it)"
				out = append(out, a)
				continue
			}
			a.Warnings = append(a.Warnings, fmt.Sprintf("%q is paused in Healthchecks; Tend has no pause, so it alerts once its period passes without a ping", label))
		}

		if hb, ok := byToken[a.Token]; ok {
			was := hb
			a.Was, a.Name = &was, hb.Name
			a.Op = OpUnchanged
			if hb.PeriodSeconds != a.PeriodSeconds || hb.GraceSeconds != a.GraceSeconds {
				a.Op = OpUpdate
			}
			planned[a.Name] = true
			out = append(out, a)
			continue
		}

		if planned[name] && !byName[name] {
			renamed := name + "-" + a.Token[:8]
			a.Warnings = append(a.Warnings, fmt.Sprintf("another check is also called %q; this one is imported as %q", name, renamed))
			name = renamed
		}
		a.Name = name
		if byName[name] {
			a.Op = OpSkip
			a.Reason = fmt.Sprintf("a heartbeat named %q already exists with another ping URL; rename or remove it (tend heartbeat rm %s) and import again", name, name)
			out = append(out, a)
			continue
		}
		a.Op = OpCreate
		planned[name] = true
		out = append(out, a)
	}
	return out
}

// firstOf returns the first of ss that is not empty.
func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// mapTiming is the period and grace a check gets, with warnings where they
// only approximate it.
func mapTiming(c Check, label string, now time.Time) (period, grace int, warnings []string) {
	grace = c.Grace
	if c.Schedule == "" {
		return c.Timeout, grace, nil
	}
	tzName := c.TZ
	if tzName == "" {
		tzName = "UTC"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%q: unknown time zone %q, its schedule is read in UTC", label, tzName))
		loc = time.UTC
	}
	longest, shortest, ok := scheduleGaps(c.Schedule, now.In(loc))
	if !ok {
		warnings = append(warnings, fmt.Sprintf(
			"%q: could not read its schedule %q (Tend reads standard cron expressions), so it is imported with a period of %s; set the right one with: tend heartbeat add -name %s -period <seconds> -grace %d",
			label, c.Schedule, defaultPeriod, label, grace))
		return int(defaultPeriod / time.Second), grace, warnings
	}
	what := "the time between its runs"
	consequence := ""
	if shortest != longest {
		what = fmt.Sprintf("the longest gap between two of its runs (the shortest is %s)", shortest)
		consequence = "; a run missed after a shorter gap is noticed later than Healthchecks would notice it"
	}
	warnings = append(warnings, fmt.Sprintf(
		"%q is a cron check (%q, %s): Tend heartbeats have no schedule, so it is imported with a period of %s, %s, counted from its last ping%s",
		label, c.Schedule, tzName, longest, what, consequence))
	return int(longest / time.Second), grace, warnings
}

// The sample of a schedule's runs scheduleGaps looks at: up to sampleRuns
// runs within sampleSpan.
const (
	sampleRuns = 2000
	sampleSpan = 5 * 366 * 24 * time.Hour
)

// scheduleGaps returns the longest and shortest time between two
// consecutive runs of the cron schedule expr after from, over a sample of
// its runs; ok is false when expr does not parse or runs fewer than twice.
func scheduleGaps(expr string, from time.Time) (longest, shortest time.Duration, ok bool) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return 0, 0, false
	}
	end := from.Add(sampleSpan)
	prev := sched.Next(from)
	if prev.IsZero() {
		return 0, 0, false
	}
	for i := 0; i < sampleRuns; i++ {
		next := sched.Next(prev)
		if next.IsZero() || next.After(end) {
			break
		}
		gap := next.Sub(prev)
		if gap > longest {
			longest = gap
		}
		if shortest == 0 || gap < shortest {
			shortest = gap
		}
		prev = next
	}
	return longest, shortest, longest > 0
}
