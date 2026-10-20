# Probes: certificates, domains and dated secrets

Things with an expiry date are a common cause of outages that nobody saw
coming, and the warnings that used to arrive by themselves are going away:
Let's Encrypt stopped sending expiry emails on 4 June 2025, and CA/Browser
Forum ballot SC-081v3 cuts the longest a public TLS certificate may be valid
to 200 days from 15 March 2026, 100 days from 15 March 2027 and 47 days from
15 March 2029. Certificates will be renewed more often, by machines, and "the
renewal silently stopped" becomes the failure to watch for.

Tend has three job types for this. Each is a job like any other:

- it is scheduled with `cron`, `interval_seconds` or `run_at`, or run by hand
  with `tend run <name>`;
- it honours `timeout_seconds` (30 minutes when unset; a probe usually needs a
  few seconds, so set something like 30) and `max_retries`;
- every run is recorded with its output, and a run that finds a problem fails,
  which records `run.failed`; the next run that finds none records
  `run.recovered`. Route those to a channel or to Ward with a rule, as for any
  job. Nothing on the notification side is specific to probes.

Every failed run raises `run.failed`, so a probe that keeps finding the same
problem alerts on every run until it is fixed. Schedule probes every few hours
or once a day, not every minute.

The output starts with the verdict, one `FAIL:` line per problem or a single
`ok:` line, followed by what was checked:

```
FAIL: renewal missed: 77% of the 90-day lifetime has passed and the certificate was due for renewal on 2026-10-10T23:15:29Z; is the ACME client still renewing it?

target:      example.com:443
subject:     CN=example.com
issuer:      CN=R11,O=Let's Encrypt,C=US
not_before:  2026-08-11T23:15:29Z
not_after:   2026-11-09T23:15:29Z
days_left:   19
lifetime:    90 days, 77% used
renewal_due: 2026-10-10T23:15:29Z (after 67% of the lifetime; missed once 48h past that)
sans:        example.com, www.example.com
chain:       verified (2 sent)
```

The exit code says which kind of failure it was: `0` when everything checked
was fine, `1` when a check failed (or the answer lacked what is needed to
check, such as a registry that gives no expiration date), and `-1` when the
probe could not check at all (no connection, no answer, an error from the
server). A probe that runs into its timeout is `timed_out`, like any job.

`warn_days` is how close to the date a probe starts failing: the run fails
once the date is `warn_days` days away or less, and `0` fails only once the
date has passed. When it is left out it is 14 for `tls` and `expiry` jobs and
30 for `domain` jobs.

## `tls`: certificates

```yaml
jobs:
  - name: web-cert
    type: tls
    host: example.com
    interval_seconds: 21600       # every 6 hours
    timeout_seconds: 30
```

| Setting | Flag | Default | |
|---|---|---|---|
| `host` | `-host` | *(required)* | The server's host name or IP address, without a port. |
| `port` | `-port` | 443; 25 with `starttls: smtp`, 143 with `imap` | |
| `server_name` | `-server-name` | `host` | The name sent as SNI and checked against the certificate. Set it when `host` is an IP address or a name the certificate does not carry. |
| `starttls` | `-starttls` | *(none)* | `smtp` or `imap`: connect in plain text and upgrade with STARTTLS. |
| `warn_days` | `-warn-days` | 14 | |
| `renew_after_fraction` | `-renew-after-fraction` | 2/3 | The share of the lifetime after which the certificate should have been renewed. `0` turns the renewal check off. |
| `ca_file` | `-ca-file` | *(none)* | A PEM file of extra roots, for a private CA, used on top of the system's. Absolute path. |
| `insecure_skip_verify` | `-insecure-skip-verify` | `false` | Skip the chain check, for a self-signed certificate. |

The probe connects, completes a TLS handshake and reads the certificates the
server presents; nothing is sent over the encrypted connection. It fails when
the leaf certificate:

- **expires within `warn_days`**, or has expired, or is not valid yet;
- **has missed its renewal**: it is past `renew_after_fraction` of its
  lifetime by more than a grace (below);
- **is not valid for the name** (`server_name`, or `host`), including its
  wildcard and IP address SANs;
- **does not chain** to a root of the system (plus `ca_file`), counting the
  intermediates the server sent. A server that leaves out its intermediate
  fails here, as it does for many clients. `insecure_skip_verify` turns only
  this check off: the name, expiry and renewal checks still apply.

All the problems are listed, not only the first.

**The renewal check** catches a renewal that stopped long before the
certificate expires. ACME clients renew after about two thirds of the
lifetime (certbot 30 days before the end of a 90-day certificate, Caddy and
cert-manager at two thirds), so a 90-day certificate is due on day 60 and,
without this check, nobody hears of it until `warn_days` before day 90. Most
clients only look once or twice a day (certbot twice, Traefik once) and ACME
Renewal Info spreads renewals over a window, so a certificate is routinely a
few hours past its renewal point when it is renewed. The probe therefore
waits a grace past the renewal point before it reports the renewal as missed:
48 hours, or a tenth of the lifetime when that is shorter. For a 90-day
certificate the check fires on day 62, 28 days before it expires; for a
47-day certificate on day 33.

Set `renew_after_fraction: 0` for a certificate renewed by hand (one bought
from a CA would otherwise be "due" two thirds of the way through its term,
long before anyone means to renew it), and match it to your client when it
renews elsewhere, e.g. `0.5` for short-lived
6-day certificates renewed at half their life. For those also lower
`warn_days`, to 1 or 2: the default of 14 is longer than the whole lifetime.

**STARTTLS** is supported for SMTP (RFC 3207: the greeting, `EHLO`, then
`STARTTLS` if the server offers it) and IMAP (RFC 3501: `STARTTLS` after the
greeting). A server that sends anything after agreeing to STARTTLS and before
the handshake is refused. Other STARTTLS protocols (POP3, LDAP, XMPP,
PostgreSQL) are not supported; probe the service's implicit-TLS port instead
where it has one (465 for SMTP submission, 993 for IMAP, 995 for POP3, 636 for
LDAP).

## `domain`: registrations, over RDAP

```yaml
jobs:
  - name: example-com-registration
    type: domain
    domain: example.com
    cron: "0 6 * * *"             # once a day
    timeout_seconds: 30
```

| Setting | Flag | Default | |
|---|---|---|---|
| `domain` | `-domain` | *(required)* | The registered domain, in ASCII: an internationalized name in its `xn--` form. |
| `warn_days` | `-warn-days` | 30 | |
| `rdap_url` | `-rdap-url` | *(from IANA)* | The base URL of the registry's RDAP server, for a TLD the IANA bootstrap file does not list. |

RDAP is WHOIS's successor, with answers in JSON. The probe finds the
registry's RDAP server in IANA's bootstrap file
(<https://data.iana.org/rdap/dns.json>, RFC 9224), asks it about the domain
and reads its `expiration` event. The bootstrap file is fetched once a day
and held in memory by `tend serve`; when it cannot be fetched again, the copy
held is used and the output says so. (`tend run` fetches it every time.)

The run fails when the registration expires within `warn_days` or has
expired, when the registry lists the domain as `redemption period`, `pending
delete` or `pending restore` (it has lapsed), and when the registry has no
record of the domain at all (HTTP 404: never registered, or deleted after it
lapsed). The output gives the expiration date, the days left, the domain's
status, and when it was registered and last changed.

Not every TLD can be watched this way:

- **Some TLDs have no RDAP service** in the bootstrap file, several
  country-code TLDs among them. The probe then fails with `no RDAP server for
  ".xx"` and exit code `-1`. If the registry runs an RDAP server anyway, give
  its base URL as `rdap_url`; otherwise watch the date your registrar shows
  with an [`expiry`](#expiry-dated-secrets-with-no-api) job.
- **Some registries leave the expiration date out** of their answers. The
  probe fails with `no expiration date`; use an `expiry` job instead.
- Registries rate-limit RDAP. Once a day is plenty.

## `expiry`: dated secrets with no API

API tokens, Tailscale node keys, software licences, a support contract: things
that expire and that no server can be asked about. An `expiry` job watches a
date you give it, or one a command of yours prints.

```yaml
jobs:
  - name: vendor-licence
    type: expiry
    expires_at: 2027-01-31        # or 2027-01-31T00:00:00Z
    cron: "0 9 * * *"

  - name: tailscale-node-key
    type: expiry
    expiry_command: "tailscale status --json | jq -er .Self.KeyExpiry"
    warn_days: 7
    cron: "0 9 * * *"
    timeout_seconds: 30
```

| Setting | Flag | Default | |
|---|---|---|---|
| `expires_at` | `-expires-at` | | The date: RFC 3339 with its zone, or a bare date (midnight UTC). |
| `expiry_command` | `-expiry-command` | | A shell command whose stdout is the date, in the same formats. |
| `warn_days` | `-warn-days` | 14 | |

Give exactly one of `expires_at` and `expiry_command`.

`expiry_command` runs exactly as a shell job's `command` does: through `sh
-c`, with the job's `env` (`{{ secret.NAME }}` references resolved at run
time and redacted from the output), in its own process group, killed at the
job's timeout. Its stdout must be **one date** and nothing else, white space
around it aside; anything else (nothing, two lines, `null`, more than 4 KiB)
fails the run with exit code `-1` and says what was printed. What it prints is
only ever parsed as a date: Tend never runs it and never puts it, or anything
else, into a command. Its stderr is kept in the run's output. A command that
exits non-zero fails the run with its exit status.

Some commands to start from:

```sh
# A Tailscale node key (jq -e fails on null, when key expiry is off).
tailscale status --json | jq -er .Self.KeyExpiry

# A certificate on disk that is not served over TLS (a client certificate,
# a CA, a signing key's certificate). GNU date.
date -u -d "$(openssl x509 -enddate -noout -in /etc/ssl/client.pem | cut -d= -f2)" +%Y-%m-%dT%H:%M:%SZ

# A GitHub fine-grained token, from the header GitHub sends back.
curl -fsSI -H "Authorization: Bearer $GH_TOKEN" https://api.github.com/ | tr -d '\r' |
  awk -F': ' 'tolower($1) == "github-authentication-token-expiration" { print $2 }' |
  xargs -I{} date -u -d {} +%Y-%m-%dT%H:%M:%SZ
```

with the token in the job's env as `GH_TOKEN: "{{ secret.github_token }}"`.
