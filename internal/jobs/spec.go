package jobs

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// The job types that watch something rather than do something, and the
// restore drill. Like shell and http jobs they are scheduled, run, retried and
// timed out by the runner, record a run with its output, and raise run.failed
// and run.recovered; what they need beyond a command or a URL is in Job.Spec.
const (
	// TLS connects to a TLS server and checks its certificate: the days left,
	// whether it is past the point an ACME client renews it, the chain and the
	// host name.
	TLS JobType = "tls"
	// Domain looks a domain's registration expiry up over RDAP.
	Domain JobType = "domain"
	// Expiry watches a date: a token, a key or a licence with no API to ask,
	// given in the config or printed by a command.
	Expiry JobType = "expiry"
	// Drill restores a backup into a scratch location, runs checks on the
	// result, times the restore and always cleans up.
	Drill JobType = "drill"
)

// Spec holds the settings of the job types that need more than a command or a
// URL: the tls, domain and expiry probes and the restore drill. Shell and http
// jobs have none (Job.Spec is nil). It is stored as JSON in the jobs table's
// spec column. Each field applies to the types named in its comment and is
// inert on the others.
//
// Defaults are applied where a job is defined (tend job add, tend sync), so a
// stored Spec means what it says: a WarnDays of 0 fails only once the date has
// passed, and a RenewAfterFraction of 0 turns the renewal check off.
type Spec struct {
	// WarnDays (tls, domain, expiry): the run fails once the expiry is this
	// many days away or less.
	WarnDays int `json:"warn_days"`

	// Host (tls) is the server's host name or IP address, without a port.
	Host string `json:"host,omitempty"`
	// Port (tls) is the server's port; 0 means 443, or 25 for STARTTLS over
	// SMTP and 143 for IMAP.
	Port int `json:"port,omitempty"`
	// ServerName (tls) is the name sent as SNI and checked against the
	// certificate; empty means Host.
	ServerName string `json:"server_name,omitempty"`
	// StartTLS (tls) is "smtp" or "imap" to upgrade a plain connection with
	// STARTTLS before the handshake; empty for TLS from the first byte.
	StartTLS string `json:"starttls,omitempty"`
	// InsecureSkipVerify (tls) skips the chain check, for a self-signed
	// certificate. The host name, expiry and renewal checks still apply.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
	// CAFile (tls) is a PEM file of extra roots to verify the chain against
	// (a private CA), on top of the system's.
	CAFile string `json:"ca_file,omitempty"`
	// RenewAfterFraction (tls) is the share of a certificate's lifetime after
	// which it should have been renewed (ACME clients renew at about 2/3); the
	// run fails once that point is passed by more than a grace. 0 turns the
	// check off, for certificates renewed by hand.
	RenewAfterFraction float64 `json:"renew_after_fraction,omitempty"`

	// Domain (domain) is the registered domain name, in ASCII (an
	// internationalized name in its xn-- form).
	Domain string `json:"domain,omitempty"`
	// RDAPURL (domain) is the base URL of the RDAP server to ask, for a TLD
	// the IANA bootstrap file does not list; empty means look it up there.
	RDAPURL string `json:"rdap_url,omitempty"`

	// ExpiresAt (expiry) is the date being watched.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// ExpiryCommand (expiry) is run like a shell job's command at each run;
	// what it prints on stdout must be one RFC 3339 date, which is then
	// watched instead of ExpiresAt.
	ExpiryCommand string `json:"expiry_command,omitempty"`

	// Restore (drill) is the shell command that restores the backup into the
	// scratch location ($TEND_SCRATCH).
	Restore string `json:"restore,omitempty"`
	// Checks (drill) are shell commands run on the restored data, in order,
	// once the restore succeeded; each must exit 0.
	Checks []string `json:"checks,omitempty"`
	// Cleanup (drill) is a shell command run after the restore and the checks
	// however they ended, for what the scratch directory does not hold (a
	// temporary database).
	Cleanup string `json:"cleanup,omitempty"`
	// ScratchDir (drill) is the directory the scratch directory is made in;
	// empty means the system's temporary directory.
	ScratchDir string `json:"scratch_dir,omitempty"`
	// MaxDurationSeconds (drill) fails the run when the restore took longer;
	// 0 means no limit beyond the job's timeout.
	MaxDurationSeconds int `json:"max_duration_seconds,omitempty"`
}

// Defaults applied where a probe is defined when it does not say otherwise.
const (
	// DefaultTLSWarnDays is a tls probe's warn_days: two weeks, the most
	// Let's Encrypt's retired expiry emails ever gave.
	DefaultTLSWarnDays = 14
	// DefaultDomainWarnDays is a domain probe's warn_days: a month, time to
	// sort out a renewal that needs a person (a card that expired, a
	// registrar account nobody can log in to).
	DefaultDomainWarnDays = 30
	// DefaultRenewAfterFraction is a tls probe's renew_after_fraction: ACME
	// clients (certbot, Caddy, cert-manager) renew after two thirds of a
	// certificate's lifetime.
	DefaultRenewAfterFraction = 2.0 / 3
)

// DefaultWarnDays is the warn_days of a probe of type t that sets none.
func DefaultWarnDays(t JobType) int {
	switch t {
	case TLS:
		return DefaultTLSWarnDays
	case Domain:
		return DefaultDomainWarnDays
	}
	return 0
}

// HasSpec reports whether jobs of type t carry settings in Job.Spec.
func HasSpec(t JobType) bool {
	switch t {
	case TLS, Domain:
		return true
	}
	return false
}

// ValidateSpec reports what is wrong with the settings s of a job of type t,
// or nil when they are complete and make sense. Shell and http jobs have no
// settings, so s is not looked at for them. tend job add and tend sync call it
// before anything is written, and the executor again before a run, so a job
// stored by other means fails its run with the same message.
func ValidateSpec(t JobType, s *Spec) error {
	if !HasSpec(t) {
		return nil
	}
	if s == nil {
		s = &Spec{}
	}
	if s.WarnDays < 0 {
		return fmt.Errorf("warn_days must be 0 or more, got %d", s.WarnDays)
	}
	switch t {
	case TLS:
		return validateTLS(s)
	case Domain:
		return validateDomain(s)
	}
	return nil
}

func validateDomain(s *Spec) error {
	if _, err := NormalizeDomain(s.Domain); err != nil {
		return err
	}
	if s.RDAPURL != "" {
		u, err := url.Parse(s.RDAPURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("rdap_url must be an http:// or https:// URL, got %q", s.RDAPURL)
		}
	}
	return nil
}

// NormalizeDomain returns domain as a domain probe asks for it (lower case,
// no trailing dot), or an error when it is not a registered domain name in
// ASCII.
func NormalizeDomain(domain string) (string, error) {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if d == "" {
		return "", errDomainRequired
	}
	for _, r := range d {
		if r > unicode.MaxASCII {
			return "", fmt.Errorf("domain %q: give an internationalized name in its ASCII form (xn--...)", domain)
		}
	}
	labels := strings.Split(d, ".")
	if len(d) > 253 {
		return "", fmt.Errorf("domain %q is not a domain name", domain)
	}
	for _, l := range labels {
		if !validLabel(l) {
			return "", fmt.Errorf("domain %q is not a domain name", domain)
		}
	}
	if len(labels) < 2 {
		return "", fmt.Errorf("domain %q is not a registered domain (it has no TLD)", domain)
	}
	return d, nil
}

// validLabel accepts a lower-case LDH label: 1 to 63 letters, digits and
// hyphens, not starting or ending with a hyphen.
func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validateTLS(s *Spec) error {
	if s.Host == "" {
		return errors.New("host is required for tls jobs")
	}
	if err := validHost(s.Host); err != nil {
		return fmt.Errorf("host %q: %w", s.Host, err)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (0 for the default), got %d", s.Port)
	}
	if s.ServerName != "" {
		if err := validHost(s.ServerName); err != nil {
			return fmt.Errorf("server_name %q: %w", s.ServerName, err)
		}
	}
	switch s.StartTLS {
	case "", "smtp", "imap":
	default:
		return fmt.Errorf("starttls must be smtp or imap (or unset for TLS from the start), got %q", s.StartTLS)
	}
	if s.RenewAfterFraction < 0 || s.RenewAfterFraction >= 1 {
		return fmt.Errorf("renew_after_fraction must be at least 0 (off) and below 1, got %g", s.RenewAfterFraction)
	}
	if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
		return fmt.Errorf("ca_file must be an absolute path, got %q", s.CAFile)
	}
	return nil
}

// validHost accepts a host name or an IP address, without a port or a scheme.
func validHost(h string) error {
	if net.ParseIP(h) != nil {
		return nil
	}
	switch {
	case strings.Contains(h, "://"):
		return errors.New("give the host name alone, without a scheme")
	case strings.Contains(h, ":"):
		return errors.New("give the host name alone; the port has its own setting")
	case strings.ContainsAny(h, "/ \t\r\n"), len(h) > 253:
		return errors.New("not a host name")
	}
	return nil
}
