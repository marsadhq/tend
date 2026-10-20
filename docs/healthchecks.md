# Coming from Healthchecks.io

Tend answers the [Healthchecks ping API](https://healthchecks.io/docs/http_api/).
Anything written for Healthchecks (a `curl` line at the end of a cron job,
borgmatic, runitor, a monitoring hook in a backup tool) moves to Tend by
changing the base of its ping URL. This page covers the URL mapping, what each
ping does in Tend, the importer that brings a project's checks over with their
UUIDs, `tend exec` for wrapping commands, and where Tend behaves differently.

## Ping URLs

| Healthchecks | Tend | What it does |
|---|---|---|
| `https://hc-ping.com/<uuid>` | `<TEND_BASE_URL>/ping/<token>` | Success: the heartbeat is up; one that was down recovers (`heartbeat.recovered`). |
| `…/<uuid>/start` | `…/ping/<token>/start` | A run starts. The heartbeat goes down if no success or failure follows within its grace (its period when the grace is 0). |
| `…/<uuid>/fail` | `…/ping/<token>/fail` | Failure: the heartbeat goes down at once. |
| `…/<uuid>/<exit-status>` | `…/ping/<token>/<exit-status>` | `0` is a success, `1` to `255` a failure with that status, anything larger is refused with `400 invalid url format`. |
| `…/<uuid>/log` | `…/ping/<token>/log` | A log entry: logged, changes nothing. |
| `?rid=<uuid>` on any of them | the same | Pairs a start with its completion, for durations and for concurrent runs. |
| `https://hc-ping.com/<ping-key>/<slug>[/start…]` | `<TEND_BASE_URL>/ping/<ping-key>/<name>[/start…]` | The same actions by heartbeat name, once the org has a ping key (below). `?create=1` creates a missing heartbeat. |

HEAD, GET and POST all work, and a trailing slash is accepted. A heartbeat
imported from Healthchecks keeps the check's UUID as its token, so
`https://hc-ping.com/5bf66975-…` becomes `<TEND_BASE_URL>/ping/5bf66975-…`
with nothing else changed; a heartbeat created in Tend has a 32-character hex
token instead (`tend heartbeat ping-url <name>` prints its URL). A UUID token
matches in either letter case.

Replies are short plain text. A ping that is recorded answers `200` with
`ok`, in lower case, exactly as Tend always has (Healthchecks answers `OK`;
clients normally check only the status code), or `201 Created` for a
heartbeat made by `?create=1`. A refused one answers `404 page not found`,
`400 invalid url format` or `400 invalid uuid format`, or `429 rate limit
exceeded` with `Retry-After`. A recorded ping's reply also carries
Healthchecks' `Ping-Body-Limit` header (the most bytes of body that are
kept); otherwise the headers are the ones Tend has always sent.

### The request body

The body of a POST is logged with the ping: the first 10 kB of it by default
(`TEND_PING_BODY_KB`, from 0 to Healthchecks' 100; the limit is announced in
the `Ping-Body-Limit` reply header, which Healthchecks clients size what they
send by), made into
plain text the way captured job output is (NUL bytes dropped, bytes that are
not UTF-8 replaced with U+FFFD), with the ping's own token or ping key
replaced by `***` in case the script echoes its URL, and cut on a character
boundary with `... [ping body truncated]` when it was longer. It is
shown by `tend heartbeat history <name> -body`, by `tend heartbeat show
<name>` (the tail of the last result's body), on the heartbeat's dashboard
page, and by `GET /api/heartbeats/{id}/pings`.

### Starts, durations and run ids

After `/start` the heartbeat goes down when its grace has passed without a
success or failure, as in Healthchecks: the deadline is the earlier of the
regular one (period plus grace after the last ping) and the start plus the
grace, so a start never buys more time. Healthchecks' grace is at least a
minute, while Tend's defaults to 0; with a grace of 0 a started run may last
up to one period instead, so that `tend exec`, which sends `/start` by
default, does not report every run that outlasts a watcher tick. Give a
heartbeat a grace to bound its runs more tightly (and to tolerate a late
ping, which a grace of 0 does not), or use `tend exec --no-start` for a job
that should only report its completion. A run that did not finish reports
`heartbeat.missed` with the subject `Heartbeat missed: <name> (run did not
finish)`.

A completion is paired with the latest start that carries the same `rid`
(none pairs with none) within 72 hours, unless a success or failure with that
`rid` came in between; the pair's duration is logged with the completion.
With concurrent runs only the most recently started one is timed: a success
that carries another run's `rid` leaves it running, while a success without a
`rid`, and any failure, end it.

### Alerts

No new event types: a heartbeat that goes down on `/fail` or a non-zero exit
status records the existing `heartbeat.missed` (payload: the plain heartbeat
name, as always) in the same transaction as the status change, with the
subject `Heartbeat failed: <name> (exit 3)` (or `Heartbeat failed: <name>` for
`/fail`) and a body that ends with the last lines (at most 1000 bytes) of the
output the ping logged. The next success records `heartbeat.recovered`. A
failure while the heartbeat is already down records nothing more. The
`marsad` envelope keeps its `kind`, `subject`, `dedup_key` and `data`; its
`title` and `body` carry the new text. Event DTOs in the API carry the reason
under `detail` (`{"reason":"failed","exit_status":3,"output":"…"}` or
`{"reason":"overran","started_at":"…","grace_seconds":300}`).

## Slug URLs and the ping key

Healthchecks also addresses a check by its project's ping key and the check's
slug. In Tend the slug is the heartbeat's name, and the key belongs to the
org:

```sh
tend heartbeat ping-key -new           # make one (prints it and the URL form)
tend heartbeat ping-key -set < key.txt # or adopt a Healthchecks project's ping key
tend heartbeat ping-key                # show it
tend heartbeat ping-key -clear         # turn slug URLs off
```

With the project's key adopted and the checks imported (their slugs become
their names), `https://hc-ping.com/<ping-key>/<slug>` keeps working as
`<TEND_BASE_URL>/ping/<ping-key>/<slug>`. `?create=1` creates a missing
heartbeat named after the slug, with Healthchecks' defaults (a period of one
day, a grace of one hour), and answers `201 Created`; the slug must be lower
case letters, digits, `-` and `_`. An org can create at most 20 heartbeats this
way at once, then one a minute. A heartbeat named `start`, `fail`, `log` or a
number reads as an action in the two-part form; ping it with an explicit
action (`/ping/<key>/<name>/0`).

## Importing a project

```sh
tend import healthchecks --api-key-file ~/hc-api-key --dry-run   # print the plan
tend import healthchecks --api-key-file ~/hc-api-key             # import
```

The importer lists the project's checks through the
[Management API](https://healthchecks.io/docs/api/) (v3, `X-Api-Key`) and,
for each check, creates a heartbeat whose token is the check's UUID, or
updates the one an earlier import created. Use the project's read-write API
key: a read-only key is not given the UUIDs, and the import refuses it. The
key is read from the file (or from stdin with `--api-key-file -`) and never
printed. `--url` points at a self-hosted Healthchecks (default
`https://healthchecks.io`).

| Check | Heartbeat |
|---|---|
| UUID | token, so the ping URL only changes its base |
| slug (else name, else UUID) | name; a name two checks share gets the second one's UUID prefix |
| `timeout` | period |
| `grace` | grace |
| cron `schedule` and `tz` | a period: the longest gap between two runs of the schedule, with a warning (below) |
| `paused` | skipped, unless `--include-paused` |
| tags, description, channels, filters, `manual_resume` | not imported |

Tend heartbeats have no schedule, only a period, so a cron check gets one
that approximates it, and the import prints a warning per check saying which.
For a schedule that runs at even intervals (`15 5 * * *`) the period is that
interval, counted from the last ping instead of from the scheduled time. For
an uneven one (`0 9 * * 1-5`) it is the longest gap (72 hours here), so a run
missed after a shorter gap is noticed later than Healthchecks would notice
it. A schedule that does not parse as a standard cron expression (an
OnCalendar one, say) gets a period of one day and a warning that says how to
set the right one (`tend heartbeat add -name <name> -period <seconds>`, which
keeps the token).

A heartbeat whose name is already taken by another heartbeat with a
different token is skipped with a message rather than taken over. Imported
heartbeats start as `new` and are armed from the moment of the import, like
any new heartbeat. Alert routing does not come over: add Tend channels and
rules (`tend channel add`, `tend rule add`, or YAML) for `heartbeat.missed`
and `heartbeat.recovered`.

## tend exec

`tend exec` wraps a command the way runitor does, so an existing cron job is
monitored without being rewritten:

```cron
15 3 * * *  tend exec --ping-url https://tend.example/ping/<token> -- /usr/local/bin/backup.sh
```

It sends `/start`, runs the command with stdin, stdout and stderr passed
through, keeps the last 10 kB of its output (`--body-limit`, or less if the
server's `Ping-Body-Limit` says so), then sends `/<exit-status>` with that
tail as the body, and exits with the command's status: 128 plus the signal
number when a signal killed it, 127 when the command was not found, 126 when
it could not be started. Both pings carry the same random `?rid=`. Signals
`tend exec` receives (interrupt, terminate, hang-up, quit) are passed to the
command; one that arrives while the start ping is still going out (a slow
server) abandons that ping and the command is not started at all. The
completion ping goes out either way.

A ping that fails is retried on network errors, 5xx and 429 (`--retries`,
default 5; backoff from 1 second, doubling to 30, or the server's
`Retry-After` up to a minute; `--timeout` per attempt, default 10 seconds),
then given up with a warning that names only the server's host. The command
runs and exits the same way whether or not its pings got through.

| Flag | Meaning |
|---|---|
| `--ping-url <url>` | The check's ping URL without an action: `https://tend.example/ping/<token>`, `https://hc-ping.com/<uuid>`, or a slug URL (`…/<ping-key>/<slug>?create=1` keeps its query). Default `$TEND_PING_URL`. |
| `--token <token>` | Instead of `--ping-url`: `<base-url>/ping/<token>`, with `--base-url` or `$TEND_BASE_URL`. |
| `--no-start` | Skip `/start`. |
| `--body-limit <n>` | Bytes of output tail to send (default 10000; 0 sends none). |
| `--retries <n>`, `--timeout <d>` | See above. |

`tend exec` needs no database and no `TEND_DB`; any host that can reach the
ping URL can run it, against Tend or against a Healthchecks-compatible
server.

## Other clients

- **curl**: `curl -fsS -m 10 --retry 5 -X POST --data-binary @log.txt <TEND_BASE_URL>/ping/<token>/$?`
- **borgmatic**: set `healthchecks: ping_url:` to `<TEND_BASE_URL>/ping/<token>`; borgmatic appends `/start`, `/fail` and `/log` and sends its logs as the body.
- **runitor**: `runitor -api-url <TEND_BASE_URL>/ping -uuid <token> -- <command>` (runitor appends the token, and its own run ids, to that base), or with a ping key `-ping-key <key> -slug <name>`.

## What differs from Healthchecks

- **`ok`, not `OK`.** A recorded ping answers `ok` in lower case, as Tend
  always has, so scripts that match Tend's reply keep working.
- **Unknown token: 404.** The hosted Healthchecks answers `200 OK (not
  found)` for an unknown UUID on everything but `/log`; Tend answers `404 page
  not found` on every endpoint, as it always has, so a mistyped URL shows up in
  `curl -f` and similar. Unknown tokens count against the client address's
  budget (a burst of 30, then one a second), which is what stops token
  guessing.
- **Rate limited: 429.** Healthchecks answers `200 OK (rate limited)` on UUID
  endpoints; Tend answers `429 rate limit exceeded` with `Retry-After`, so a
  retrying client tries again instead of losing the ping. The limit is per
  heartbeat (a burst of 10, then one a second), far above Healthchecks'
  recommended five a minute.
- **Never-pinged heartbeats are watched.** A Healthchecks check stays `new`
  until its first ping; a Tend heartbeat is armed from its creation, so a job
  whose first ping never comes is reported too.
- **No schedules.** Cron checks are approximated by a period (see the import).
- **No pause, no `manual_resume`, no keyword or method filters, no `ign` pings,
  no e-mail pinging.**
- **Retention.** Healthchecks keeps the last pings per check by count; Tend
  keeps each heartbeat's newest 1000 pings, and of those only as many as fit
  in 10 MB of bodies, and drops older ones after `TEND_RETENTION_PINGS_DAYS`
  (30 by default, `0` keeps them).
- **Bodies are text.** A body that is not UTF-8 is stored with its invalid
  bytes replaced; binary bodies are not kept as files.
- **Management API.** Tend does not serve Healthchecks' Management API; its own
  read-mostly API is under `/api`, and heartbeats are managed with the CLI or
  YAML.
