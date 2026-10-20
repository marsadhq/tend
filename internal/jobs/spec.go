package jobs

import "time"

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
