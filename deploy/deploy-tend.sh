#!/usr/bin/env bash
# Push-style Tend deploy: build here, ship to one host over ssh, swap the
# binary under a systemd --user unit, check /healthz, roll back on failure.
#
# Steps: preflight (clean tree, HEAD == origin/master) -> build -> upload the
# new binary next to the running one -> snapshot the database -> stop ->
# keep the old binary and swap -> start -> health check, retried for up to
# TEND_DEPLOY_HEALTH_WAIT seconds (on failure: roll back, start the old
# binary, check it again) -> append to an audit log on the host.
#
# Tend is only stopped once the new binary and a verified database snapshot
# are both on the host, and from the stop onwards every exit path starts it
# again. If this script ever exits non-zero, check at once that tend is
# running on the host:
#   ssh "$TEND_DEPLOY_HOST" 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user is-active tend'
#
# Configuration comes from the environment, or from deploy/deploy.env next to
# this script (untracked; copy deploy/deploy.env.example). The environment
# wins over the file. Nothing about any particular host lives in this file.
#
# Usage: deploy/deploy-tend.sh [--allow-dirty] [--allow-any-commit]
#   --allow-dirty       deploy although the working tree has uncommitted changes
#                       (they are NOT in the build's version stamp)
#   --allow-any-commit  deploy although HEAD is not origin/master

set -euo pipefail

usage() {
    sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 2
}

ALLOW_DIRTY=0
ALLOW_ANY_COMMIT=0
for arg in "$@"; do
    case "$arg" in
    --allow-dirty) ALLOW_DIRTY=1 ;;
    --allow-any-commit) ALLOW_ANY_COMMIT=1 ;;
    -h | --help) usage ;;
    *)
        echo "deploy-tend: unknown argument: $arg" >&2
        usage
        ;;
    esac
done

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$REPO_DIR/deploy/deploy.env"

# --- configuration -------------------------------------------------------------
# Values already in the environment take precedence over the file.
if [ -f "$ENV_FILE" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in '' | \#*) continue ;; esac
        key="${line%%=*}"
        val="${line#*=}"
        case "$key" in TEND_DEPLOY_* | GO) ;; *)
            echo "deploy-tend: $ENV_FILE: unknown setting ${key}" >&2
            exit 2
            ;;
        esac
        # Strip one level of surrounding quotes, as a shell assignment would.
        case "$val" in \'*\') val="${val:1:${#val}-2}" ;; \"*\") val="${val:1:${#val}-2}" ;; esac
        if [ -z "${!key+set}" ]; then
            export "$key=$val"
        fi
    done <"$ENV_FILE"
fi

HOST=${TEND_DEPLOY_HOST:-}
HEALTH_URL=${TEND_DEPLOY_HEALTH_URL:-}
DB=${TEND_DEPLOY_DB:-}
UNIT=${TEND_DEPLOY_UNIT:-tend}
# Paths on the host. They are expanded by the host's shell, so $HOME means the
# deploy user's home there.
BIN_DIR=${TEND_DEPLOY_BIN_DIR:-'$HOME/bin'}
BACKUP_DIR=${TEND_DEPLOY_BACKUP_DIR:-'$HOME/backups'}
AUDIT_LOG=${TEND_DEPLOY_AUDIT_LOG:-'$HOME/audit/deploys-tend.log'}
# How long to keep probing the health URL after a start, in seconds.
HEALTH_WAIT=${TEND_DEPLOY_HEALTH_WAIT:-30}
GO=${GO:-go}

missing=""
[ -n "$HOST" ] || missing="$missing TEND_DEPLOY_HOST"
[ -n "$HEALTH_URL" ] || missing="$missing TEND_DEPLOY_HEALTH_URL"
[ -n "$DB" ] || missing="$missing TEND_DEPLOY_DB"
if [ -n "$missing" ]; then
    echo "deploy-tend: not configured:$missing (set them in the environment or in $ENV_FILE; see deploy/deploy.env.example)" >&2
    exit 2
fi
# Everything below is interpolated into commands run by the host's shell:
# allow only characters that cannot change their meaning there.
for pair in "TEND_DEPLOY_HOST=$HOST" "TEND_DEPLOY_HEALTH_URL=$HEALTH_URL" "TEND_DEPLOY_DB=$DB" \
    "TEND_DEPLOY_UNIT=$UNIT" "TEND_DEPLOY_BIN_DIR=$BIN_DIR" "TEND_DEPLOY_BACKUP_DIR=$BACKUP_DIR" \
    "TEND_DEPLOY_AUDIT_LOG=$AUDIT_LOG"; do
    if ! [[ "${pair#*=}" =~ ^[A-Za-z0-9@._:/$~+-]+$ ]]; then
        echo "deploy-tend: ${pair%%=*} contains characters this script will not pass to a remote shell" >&2
        exit 2
    fi
done
if ! [[ "$HEALTH_WAIT" =~ ^[0-9]+$ ]]; then
    echo "deploy-tend: TEND_DEPLOY_HEALTH_WAIT must be a whole number of seconds" >&2
    exit 2
fi
command -v "$GO" >/dev/null || {
    echo "deploy-tend: '$GO' not found; set GO to the go binary" >&2
    exit 2
}

BIN="$BIN_DIR/tend"
# systemctl --user over ssh needs the runtime dir of the lingering user manager.
SCTL='XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user'

audit() {
    local msg="$1"
    ssh "$HOST" "mkdir -p \"\$(dirname $AUDIT_LOG)\" && echo \"[\$(date -u +%Y-%m-%dT%H:%M:%SZ)] $msg\" >> $AUDIT_LOG" ||
        echo "deploy-tend: could not append to the audit log on the host" >&2
    echo "audit: $msg"
}

fail() {
    audit "FAILED: $1"
    echo "deploy-tend: FATAL: $1" >&2
    exit 1
}

# --- preflight -----------------------------------------------------------------
# Nothing is touched on the host until these pass.
cd "$REPO_DIR"
if [ -n "$(git status --porcelain)" ]; then
    if [ "$ALLOW_DIRTY" = 1 ]; then
        echo "deploy-tend: WARNING: working tree is dirty; deploying HEAD + local changes (--allow-dirty)" >&2
    else
        echo "deploy-tend: refusing to deploy a dirty working tree (commit or stash, or pass --allow-dirty)" >&2
        git status --short >&2
        exit 1
    fi
fi
git fetch -q origin master || {
    echo "deploy-tend: git fetch origin master failed; cannot tell whether HEAD is what master is" >&2
    exit 1
}
HEAD_SHA=$(git rev-parse HEAD)
MASTER_SHA=$(git rev-parse origin/master)
if [ "$HEAD_SHA" != "$MASTER_SHA" ]; then
    if [ "$ALLOW_ANY_COMMIT" = 1 ]; then
        echo "deploy-tend: WARNING: HEAD ${HEAD_SHA:0:7} is not origin/master ${MASTER_SHA:0:7} (--allow-any-commit)" >&2
    else
        echo "deploy-tend: refusing to deploy HEAD ${HEAD_SHA:0:7}: it is not origin/master ${MASTER_SHA:0:7} (push or check out master, or pass --allow-any-commit)" >&2
        exit 1
    fi
fi
NEW_SHA=$(git rev-parse --short HEAD)
OLD_SHA=$(ssh "$HOST" "cat $BIN.sha 2>/dev/null || echo unknown")
# OLD_SHA becomes part of file names on the host below: keep it to safe characters.
OLD_SHA=$(printf '%s' "$OLD_SHA" | tr -cd 'A-Za-z0-9._-')
[ -n "$OLD_SHA" ] || OLD_SHA=unknown
echo "deploy-tend: $OLD_SHA -> $NEW_SHA"

# --- build -----------------------------------------------------------------------
echo "deploy-tend: building..."
BUILD_OUT=$(mktemp "${TMPDIR:-/tmp}/tend-deploy-bin.XXXXXX")
trap 'rm -f "$BUILD_OUT"' EXIT
CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-X github.com/marsadhq/tend/internal/cli.Version=$NEW_SHA" -o "$BUILD_OUT" ./cmd/tend ||
    fail "build failed for $NEW_SHA"

# --- ship while the old binary is still serving ------------------------------------
# Upload first: a slow or failed transfer must cost no downtime.
echo "deploy-tend: uploading..."
# The binary goes over ssh's stdin rather than rsync: rsync quotes the remote
# path, so a '$HOME/...' in it would not be expanded on the host. The copy is
# checked against the local checksum and run once before it is trusted.
SUM=$(sha256sum "$BUILD_OUT" | cut -d' ' -f1)
ssh "$HOST" "mkdir -p $BIN_DIR && cat > $BIN.new && chmod +x $BIN.new" <"$BUILD_OUT" ||
    fail "upload failed (tend untouched, still running $OLD_SHA)"
REMOTE_SUM=$(ssh "$HOST" "sha256sum $BIN.new | cut -d' ' -f1") || REMOTE_SUM=""
[ "$REMOTE_SUM" = "$SUM" ] ||
    fail "the uploaded binary does not match the build (tend untouched, still running $OLD_SHA)"
ssh "$HOST" "$BIN.new version" >/dev/null ||
    fail "the uploaded binary does not run on the host (tend untouched, still running $OLD_SHA)"

# --- pre-deploy database snapshot ----------------------------------------------------
# Taken BEFORE the new binary can migrate the schema. tend.db on its own is
# NOT a backup: recent data lives in tend.db-wal, so the copy goes through
# the sqlite backup API and is checked before anything else happens. The
# source is opened read-only. Snapshots are kept until someone removes them.
if [ "$DB" = "skip" ]; then
    SNAP_NAME="none (TEND_DEPLOY_DB=skip)"
    echo "deploy-tend: WARNING: no database snapshot (TEND_DEPLOY_DB=skip); take one yourself (e.g. pg_dump) before a schema change" >&2
else
    SNAP_NAME="predeploy-$(date -u +%Y%m%dT%H%M%SZ)-$OLD_SHA-tend.db"
    echo "deploy-tend: snapshotting the database to $BACKUP_DIR/$SNAP_NAME ..."
    ssh "$HOST" "umask 077 && mkdir -p $BACKUP_DIR && python3 - $DB $BACKUP_DIR/$SNAP_NAME" <<'PY' ||
import os, sqlite3, sys
src_path, dst_path = sys.argv[1], sys.argv[2]
try:
    if not os.path.isfile(src_path):
        raise RuntimeError("no database file at %s" % src_path)
    src = sqlite3.connect("file:" + src_path + "?mode=ro", uri=True)
    dst = sqlite3.connect(dst_path)
    src.backup(dst)
    src.close()
    check = dst.execute("PRAGMA integrity_check").fetchone()[0]
    versions = [r[0] for r in dst.execute("SELECT version FROM schema_migrations ORDER BY version")]
    dst.close()
    if check != "ok" or not versions:
        raise RuntimeError("integrity_check=%r schema_migrations=%r" % (check, versions))
except Exception as e:
    # Never leave a file behind that looks like a good snapshot but is not.
    if os.path.exists(dst_path):
        os.remove(dst_path)
    sys.exit("deploy-tend: snapshot of %s failed: %s" % (src_path, e))
print("deploy-tend: snapshot ok (schema_migrations %s)" % ",".join(str(v) for v in versions))
PY
        fail "pre-deploy snapshot failed (tend untouched, still running $OLD_SHA)"
fi

# --- stop, swap, start ---------------------------------------------------------------
# From the stop until a start has succeeded, ANY exit (a failed step, a lost
# connection, Ctrl-C) starts tend again with whatever binary is in place:
# systemd does not restart a unit that was stopped on purpose.
STOPPED=0
on_exit() {
    rm -f "$BUILD_OUT"
    if [ "$STOPPED" = 1 ]; then
        echo "deploy-tend: exiting while tend may be stopped; starting it" >&2
        ssh "$HOST" "$SCTL start $UNIT" ||
            echo "deploy-tend: COULD NOT START TEND - start it by hand NOW: ssh $HOST '$SCTL start $UNIT'" >&2
    fi
}
trap on_exit EXIT
trap 'exit 130' INT TERM

echo "deploy-tend: stopping tend..."
STOPPED=1
ssh "$HOST" "$SCTL stop $UNIT" || fail "systemctl stop $UNIT failed"
# tend.prev is what the rollback below restores; tend.<old sha> survives later
# deploys, so the binary that matches a pre-deploy snapshot stays available.
ssh "$HOST" "if [ -f $BIN ]; then cp $BIN $BIN.prev && cp $BIN $BIN.$OLD_SHA; fi && mv $BIN.new $BIN && echo $NEW_SHA > $BIN.sha" ||
    fail "binary swap failed"

# --- start + health check ------------------------------------------------------------

# healthy probes the health URL from the host every 2 seconds until it answers
# "ok", for up to HEALTH_WAIT seconds. Anything else counts as not healthy
# (yet), the ssh call itself failing included.
healthy() {
    local deadline=$((SECONDS + HEALTH_WAIT)) out
    while :; do
        out=$(ssh "$HOST" "curl -sf --max-time 5 $HEALTH_URL" 2>/dev/null) || out=""
        [ "$out" = ok ] && return 0
        [ "$SECONDS" -lt "$deadline" ] || return 1
        sleep 2
    done
}

# rollback puts the previous binary back, starts it, and checks it, saying
# plainly how far it got. Every way out of it is a failure of the deploy, and
# the exit trap keeps starting the service whatever happens.
rollback() {
    local why="$1"
    echo "deploy-tend: $why; rolling back to $OLD_SHA" >&2
    STOPPED=1
    ssh "$HOST" "$SCTL stop $UNIT" ||
        echo "deploy-tend: WARNING: systemctl stop $UNIT failed during the rollback" >&2
    ssh "$HOST" "cp $BIN.prev $BIN && echo $OLD_SHA > $BIN.sha" ||
        fail "ROLLBACK FAILED: could not put $BIN.prev back, so $NEW_SHA ($why) is still installed and is being started again - fix the host by hand NOW"
    ssh "$HOST" "$SCTL start $UNIT" ||
        fail "ROLLBACK INCOMPLETE: $OLD_SHA is back in place but systemctl start $UNIT failed ($why on $NEW_SHA) - start it by hand NOW"
    STOPPED=0
    echo "deploy-tend: checking $OLD_SHA after the rollback..." >&2
    if healthy; then
        fail "$why after deploying $NEW_SHA; rolled back to $OLD_SHA, which is healthy"
    fi
    fail "$why after deploying $NEW_SHA; rolled back to $OLD_SHA, but it is NOT healthy either - check the host NOW"
}

echo "deploy-tend: starting tend..."
ssh "$HOST" "$SCTL start $UNIT" || rollback "systemctl start $UNIT failed"
STOPPED=0
echo "deploy-tend: waiting up to ${HEALTH_WAIT}s for the health check..."
healthy || rollback "health check failed"

audit "deploy OK: $OLD_SHA -> $NEW_SHA (healthz ok, snapshot $SNAP_NAME, via deploy-tend.sh/systemd)"
echo "deploy-tend: SUCCESS ($NEW_SHA)"
