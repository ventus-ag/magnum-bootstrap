#!/bin/bash
#
# install.sh — one-shot bootstrap for a self-hosted runner: clone the repos and
# run runner-setup.sh. Because the e2e harness needs the full magnum-bootstrap
# tree plus magnum_victoria at run time (and both are private), this clones them
# rather than piping a single file.
#
# Curl install (token needed for the private repos):
#
#   curl -fsSL -H "Authorization: Bearer $RW_PAT_TOKEN" \
#     https://raw.githubusercontent.com/ventus-ag/magnum-bootstrap/main/e2e/vm/install.sh \
#     | sudo RW_PAT_TOKEN="$RW_PAT_TOKEN" bash -s -- [--openstack]
#
# Env:
#   RW_PAT_TOKEN / GITHUB_TOKEN  PAT with read access to the private repos (required)
#   BOOTSTRAP_REF                magnum-bootstrap branch/tag           (default main)
#   MAGNUM_REPO                  forked driver repo name under ORG     (default magnum)
#   MAGNUM_REF                   forked driver branch/tag              (default stable/victoria)
#   MAGNUM_DIR                   local dir for the driver checkout     (default magnum_victoria)
#   ORG                          GitHub org                            (default ventus-ag)
#   RUNNER_USER                  runner user                           (default SUDO_USER/ubuntu)
#   INSTALL_DIR                  clone parent dir                      (default runner home)
#   CLONE_VICTORIA               also clone the forked driver          (default 1)
#
# Extra args (e.g. --openstack) are forwarded to runner-setup.sh.
#
set -euo pipefail

ORG="${ORG:-ventus-ag}"
BOOTSTRAP_REF="${BOOTSTRAP_REF:-${REF:-main}}"
MAGNUM_REPO="${MAGNUM_REPO:-magnum}"
MAGNUM_REF="${MAGNUM_REF:-stable/victoria}"
MAGNUM_DIR="${MAGNUM_DIR:-magnum_victoria}"
TOKEN="${RW_PAT_TOKEN:-${GITHUB_TOKEN:-}}"
RUNNER_USER="${RUNNER_USER:-${SUDO_USER:-ubuntu}}"
CLONE_VICTORIA="${CLONE_VICTORIA:-1}"

[ "$(id -u)" -eq 0 ] || { echo "install.sh: run as root (sudo)"; exit 1; }
[ -n "$TOKEN" ] || { echo "install.sh: RW_PAT_TOKEN (or GITHUB_TOKEN) is required for the private repos"; exit 1; }

RUNNER_HOME="$(getent passwd "$RUNNER_USER" | cut -d: -f6)"
[ -n "$RUNNER_HOME" ] || { echo "install.sh: cannot resolve home for '$RUNNER_USER'"; exit 1; }
INSTALL_DIR="${INSTALL_DIR:-$RUNNER_HOME}"

log() { printf '\033[1;36m[install %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }

command -v git >/dev/null 2>&1 || { log "installing git"; apt-get update -y && apt-get install -y git; }

clone_or_update() {
  local repo="$1" ref="$2" dir="$INSTALL_DIR/$3"
  if [ -d "$dir/.git" ]; then
    log "updating $dir ($repo@$ref)"
    git -C "$dir" -c http.extraheader="AUTHORIZATION: bearer $TOKEN" fetch --depth 1 origin "$ref"
    git -C "$dir" checkout -f FETCH_HEAD
  else
    log "cloning $ORG/$repo@$ref -> $dir"
    git -c http.extraheader="AUTHORIZATION: bearer $TOKEN" clone --depth 1 --branch "$ref" \
      "https://github.com/$ORG/$repo.git" "$dir"
  fi
  chown -R "$RUNNER_USER":"$RUNNER_USER" "$dir"
}

clone_or_update magnum-bootstrap "$BOOTSTRAP_REF" magnum-bootstrap
[ "$CLONE_VICTORIA" = "1" ] && clone_or_update "$MAGNUM_REPO" "$MAGNUM_REF" "$MAGNUM_DIR"

log "running runner-setup.sh $*"
RUNNER_USER="$RUNNER_USER" bash "$INSTALL_DIR/magnum-bootstrap/e2e/vm/runner-setup.sh" "$@"

cat <<EOF

Done. To run a manual smoke on this host (as $RUNNER_USER):

  cd $INSTALL_DIR/magnum-bootstrap
  VICTORIA_DIR=$INSTALL_DIR/$MAGNUM_DIR SCENARIOS=create ./e2e/vm/run-fcos-e2e.sh

Or just dispatch the 'e2e-fcos' GitHub workflow (it checks out the repos itself).
EOF
