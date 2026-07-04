#!/usr/bin/env bash
# Canonical Tend deploy — THE only prod path.
#
# Runs FROM the dev box (the machine holding this repo), PUSHES to prod1.
# Same shape as ward's deploy/deploy-ward.sh: pushing from the dev box needs
# only the already-authorized devbox->prod1 SSH key, so it runs headlessly
# (prod1->devbox Tailscale SSH would require interactive check-mode approval).
#
# Steps: capture pre-deploy SHA -> build -> stop (systemd) -> back up old
# binary -> rsync new binary -> start -> health check (auto-rollback on
# failure) -> append audit log on prod1.
#
# Usage: deploy/deploy-tend.sh   (from any directory; deploys HEAD of this repo)

set -euo pipefail

PROD=prod1
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
GO=${GO:-go}
command -v "$GO" >/dev/null || GO=/home/hermes/go/bin/go

HEALTH_URL="http://100.73.180.5:8080/healthz"
BIN='$HOME/bin/tend'
AUDIT_LOG='$HOME/audit/deploys-tend.log'
# systemctl --user over ssh needs the runtime dir of the lingering user manager.
SCTL='XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user'

audit() {
    local msg="$1"
    ssh "$PROD" "mkdir -p \$HOME/audit && echo \"[\$(date -u +%Y-%m-%dT%H:%M:%SZ)] $msg\" >> $AUDIT_LOG"
    echo "audit: $msg"
}

fail() {
    audit "FAILED: $1"
    echo "deploy-tend: FATAL: $1" >&2
    exit 1
}

# --- preflight ---------------------------------------------------------------
cd "$REPO_DIR"
if [ -n "$(git status --porcelain)" ]; then
    echo "deploy-tend: WARNING: working tree is dirty; deploying HEAD + local changes" >&2
fi
NEW_SHA=$(git rev-parse --short HEAD)
OLD_SHA=$(ssh "$PROD" "cat \$HOME/bin/tend.sha 2>/dev/null || echo unknown")
echo "deploy-tend: $OLD_SHA -> $NEW_SHA"

# --- build -------------------------------------------------------------------
echo "deploy-tend: building..."
"$GO" build -ldflags "-X github.com/marsadhq/tend/internal/cli.Version=$NEW_SHA" -o /tmp/tend-deploy-bin ./cmd/tend \
    || fail "build failed for $NEW_SHA"

# --- stop, back up, ship -----------------------------------------------------
echo "deploy-tend: stopping tend..."
ssh "$PROD" "$SCTL stop tend" || fail "systemctl stop tend failed"
ssh "$PROD" "cp $BIN $BIN.prev 2>/dev/null || true"
rsync -q /tmp/tend-deploy-bin "$PROD:bin/tend.new" || fail "rsync failed"
ssh "$PROD" "mv $BIN.new $BIN && chmod +x $BIN && echo $NEW_SHA > $BIN.sha"

# --- start + health check ----------------------------------------------------
echo "deploy-tend: starting tend..."
ssh "$PROD" "$SCTL start tend" || fail "systemctl start tend failed"
sleep 3
HEALTH=$(ssh "$PROD" "curl -sf --max-time 5 $HEALTH_URL || echo FAIL")
if [ "$HEALTH" != "ok" ]; then
    echo "deploy-tend: health check failed ($HEALTH); rolling back to $OLD_SHA" >&2
    ssh "$PROD" "$SCTL stop tend; cp $BIN.prev $BIN 2>/dev/null && echo $OLD_SHA > $BIN.sha; $SCTL start tend" || true
    fail "health check failed after deploying $NEW_SHA; rolled back to $OLD_SHA"
fi

audit "deploy OK: $OLD_SHA -> $NEW_SHA (healthz ok, via deploy-tend.sh/systemd)"
echo "deploy-tend: SUCCESS ($NEW_SHA)"
