# Restore drills

A backup that has never been restored is a hope, not a backup. A `drill` job
restores one on a schedule into a scratch location, checks what came back,
times the restore, and cleans up after itself, so that the first restore you
do is not the one you need.

```yaml
jobs:
  - name: app-db-restore-drill
    type: drill
    cron: "30 4 * * 0"            # Sundays, after the night's backup
    restore: 'cp "$(ls -1t /backups/app-*.db | head -n 1)" "$TEND_SCRATCH/app.db"'
    checks:
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check')" = ok
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 100
    max_duration_seconds: 300     # the restore must take at most 5 minutes
    timeout_seconds: 1800         # and is killed after 30
```

A drill is a job like any other: scheduled, retried (`max_retries`), timed out
(`timeout_seconds`), recorded with its output, and a failed drill records
`run.failed` (and the next good one `run.recovered`), which a rule routes to a
channel or to Ward.

## How a drill runs

1. Tend makes a fresh, empty scratch directory (mode 0700) inside
   `scratch_dir`, or the system's temporary directory.
2. It runs `restore`. Every step runs as a shell job's command does (`sh -c`,
   the job's `env` with its `{{ secret.NAME }}` references resolved, in its
   own process group) with the scratch directory as its working directory and
   two more variables:
   - `TEND_SCRATCH`: the scratch directory's path;
   - `TEND_DRILL_ID`: its name, `tend_drill_<digits>`, unique to the run and
     safe to use as the name of a temporary database.
   Tend never puts either into a command; quote them in your commands
   (`"$TEND_SCRATCH"`).
3. When the restore succeeded, it runs each of `checks` in order, all of them
   even when one fails, so the output shows everything that is wrong.
4. However that went, a timeout included, it runs `cleanup` (if set) and then
   removes the scratch directory with everything in it, read-only directories
   included.

The run **fails** when the restore fails, when a check fails, when the
restore took longer than `max_duration_seconds`, when the cleanup fails, or
when the scratch directory could not be removed. It **times out** when the
restore or a check runs into `timeout_seconds`; the cleanup and the removal
still happen. The exit code is that of the first step that failed (`1` for a
restore over `max_duration_seconds`, `-1` for a scratch directory that could
not be made or removed).

`max_duration_seconds` is your restore-time objective: the restore is allowed
to finish and its time is measured, and the run fails when it was too slow.
`timeout_seconds` is the hard limit at which it is killed; set it well above
the objective. Only the restore is timed, not the checks.

The cleanup runs after the job's timeout has passed when the restore timed
out, so it has a bound of its own: one minute. (It also runs when `tend serve`
is shutting down, within that minute.)

The output starts with the verdict and the measured restore time, then one
line per step, then each step's command and output (the first and the last
32 KiB of each):

```
FAIL: check 2 failed (exit 1): test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 100

restore:     ok, took 1.204s
check 1:     ok
check 2:     failed (exit 1)
scratch:     /var/tmp/tend_drill_2104613735 (removed)

--- restore (exit 0, 1.204s)
$ cp "$(ls -1t /backups/app-*.db | head -n 1)" "$TEND_SCRATCH/app.db"

--- check 1 (exit 0, 31ms)
$ test "$(sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check')" = ok

--- check 2 (exit 1, 4ms)
$ test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 100
```

| Setting | Flag | Default | |
|---|---|---|---|
| `restore` | `-restore` | *(required)* | Restores the backup into `$TEND_SCRATCH`. |
| `checks` | `-check` (repeatable) | *(none)* | Commands that must exit 0 on the restored data. |
| `cleanup` | `-cleanup` | *(none)* | Undoes what the scratch directory does not hold, e.g. drops a temporary database. |
| `scratch_dir` | `-scratch-dir` | the temp dir (`$TMPDIR`, else `/tmp`) | Absolute path of the directory the scratch directory is made in. |
| `max_duration_seconds` | `-max-duration` | 0 (no limit) | Fail when the restore took longer. |

Things to get right:

- **Room.** The scratch directory has to hold a whole restore, and `/tmp` is
  often a small `tmpfs`. Point `scratch_dir` at a disk with room, e.g.
  `/var/tmp` or a directory of its own.
- **Access.** The steps run as the user `tend serve` runs as, which must be
  able to read the backups and needs the credentials (in `env`, from secrets)
  to reach the repository. `tend doctor` warns when the scratch directory
  cannot be made, as the user running it.
- **The shipped systemd unit** (`deploy/tend.service`) sandboxes the steps
  with Tend: `ProtectSystem=strict` leaves only `/var/lib/tend` and the
  service's private `/tmp` and `/var/tmp` writable, so another `scratch_dir`
  needs a `ReadWritePaths=` line; `ProtectHome=true` hides `/home`; and
  `DynamicUser=yes` runs it as a user of its own, so backups must be readable
  to it (a group added with `SupplementaryGroups=`, say).
- **Check the data, not the exit status.** A restore that exits 0 can still
  bring back an empty database. Count rows, look for a recent record, compare
  against a threshold you would notice.
- **Freshness is a different question.** A drill proves the latest backup can
  be restored; whether a backup was made last night is better watched by a
  [heartbeat](../README.md#alert-on-a-missed-heartbeat) the backup job pings.

## Recipes

These assume a POSIX `sh` and the usual tools on the Tend host. Adjust paths,
names and thresholds.

### SQLite

A backup taken with SQLite's online backup API (`sqlite3 app.db ".backup
/backups/app-$(date +%F).db"`, or `VACUUM INTO`) is a self-contained database
file, so restoring it is a copy:

```yaml
  - name: app-db-restore-drill
    type: drill
    cron: "30 4 * * *"
    scratch_dir: /var/tmp
    restore: 'cp "$(ls -1t /backups/app-*.db | head -n 1)" "$TEND_SCRATCH/app.db"'
    checks:
      # PRAGMA quick_check is faster on a large database.
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check')" = ok
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 100
      # Something from the last two days made it into the backup.
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" "SELECT max(created_at) > datetime('now', '-2 days') FROM orders")" = 1
    max_duration_seconds: 300
```

A compressed backup is restored the same way: `zstd -dc "$(ls -1t
/backups/app-*.db.zst | head -n 1)" > "$TEND_SCRATCH/app.db"`. A file-level
copy of a database in WAL mode needs its `-wal` file copied with it; check it
with `PRAGMA integrity_check` as above, which is exactly what goes wrong when
it is missing.

Tend's own database can be drilled the same way, from the backups described
in the [README](../README.md#backups), with `SELECT count(*) FROM jobs` as a
check.

### Postgres: `pg_restore` into a temporary database

The drill creates a database named after `$TEND_DRILL_ID`, restores the
latest custom-format dump (`pg_dump -Fc`) into it, checks it and drops it.
The role needs `CREATEDB`; give the drill a role of its own rather than a
superuser.

```yaml
  - name: pg-restore-drill
    type: drill
    cron: "0 5 * * 0"
    env:
      PGHOST: db.internal
      PGUSER: tend_drill            # CREATE ROLE tend_drill LOGIN CREATEDB PASSWORD '...'
      PGPASSWORD: "{{ secret.pg_drill_password }}"
    restore: >-
      createdb "$TEND_DRILL_ID" &&
      pg_restore --no-owner --no-acl --exit-on-error --jobs 4
      --dbname "$TEND_DRILL_ID" "$(ls -1t /backups/app-*.pgdump | head -n 1)"
    checks:
      - test "$(psql -d "$TEND_DRILL_ID" -tAc 'SELECT count(*) FROM users')" -ge 100
      - test "$(psql -d "$TEND_DRILL_ID" -tAc "SELECT max(created_at) > now() - interval '2 days' FROM orders")" = t
    cleanup: dropdb --if-exists "$TEND_DRILL_ID"
    max_duration_seconds: 1800
    timeout_seconds: 3600
```

`--exit-on-error` makes `pg_restore` stop at the first error instead of
restoring what it can and reporting the errors at the end. Restore with the
`pg_restore` of the server's major version, as you would in an emergency: a
newer one sets parameters an older server rejects (from version 17 on it sends
`SET transaction_timeout`, which a 16 server refuses, and the drill fails at
once). That is the kind of surprise a drill is for. A drill database is left
behind only if the Tend process itself dies mid-drill; find any with `psql -l
| grep tend_drill_`.

### restic

`restic restore` puts the files under the target with their full original
paths:

```yaml
  - name: restic-restore-drill
    type: drill
    cron: "0 6 * * 0"
    scratch_dir: /srv/drills
    env:
      RESTIC_REPOSITORY: "s3:s3.amazonaws.com/my-bucket/web1"
      RESTIC_PASSWORD: "{{ secret.restic_password }}"
      AWS_ACCESS_KEY_ID: "{{ secret.restic_s3_key_id }}"
      AWS_SECRET_ACCESS_KEY: "{{ secret.restic_s3_secret }}"
    restore: restic restore latest --host web1 --path /srv/app --target "$TEND_SCRATCH" --verify
    checks:
      - test -s "$TEND_SCRATCH/srv/app/data/app.db"
      - test "$(sqlite3 "$TEND_SCRATCH/srv/app/data/app.db" 'PRAGMA integrity_check')" = ok
      - test "$(find "$TEND_SCRATCH/srv/app/uploads" -type f | wc -l)" -ge 1000
    max_duration_seconds: 900
```

`--verify` has restic read the restored files back and compare them with the
repository. To restore part of a large snapshot, add `--include
/srv/app/data`. A periodic `restic check --read-data-subset=5%` can go in the
checks too; it checks the repository rather than the restore.

### Borg

`borg extract` restores into the current directory, which for a drill step
is the scratch directory; paths in a Borg archive have no leading slash.
Borg 1.x syntax:

```yaml
  - name: borg-restore-drill
    type: drill
    cron: "0 6 * * 0"
    scratch_dir: /srv/drills
    env:
      BORG_REPO: "ssh://borg@backup.internal/./web1"
      BORG_PASSPHRASE: "{{ secret.borg_passphrase }}"
      BORG_RSH: "ssh -i /etc/tend/borg_ed25519 -o BatchMode=yes"
    restore: borg extract "::$(borg list --last 1 --short)" srv/app/data
    checks:
      - test -s srv/app/data/app.db
      - test "$(sqlite3 srv/app/data/app.db 'PRAGMA integrity_check')" = ok
    max_duration_seconds: 900
```

`borg list --last 1 --short` names the newest archive; with several hosts or
prefixes in one repository, add `--glob-archives 'web1-*'`. A `borg check
--verify-data --last 1` check reads the archive's data back from the
repository.
