#!/bin/bash
#
# runner-setup.sh — one-shot provisioning for a self-hosted runner so it can run
# the magnum-bootstrap e2e suite. Installs tools, enables nested KVM, pre-caches
# the Fedora CoreOS image + butane container, then runs the preflight check.
#
# Run as root (or with sudo) on the runner host:
#
#   sudo ./e2e/vm/runner-setup.sh                 # FCoS VM tier (installs Go too)
#   sudo ./e2e/vm/runner-setup.sh --openstack     # also install OpenStack tooling
#   sudo RUNNER_USER=github ./e2e/vm/runner-setup.sh --user github
#
# (Go is installed by default; --with-go is accepted but redundant.)
#
# Idempotent: safe to re-run. Supports apt / dnf-yum / pacman / zypper.
#
set -euo pipefail

WITH_OPENSTACK=0
NO_VM=0
RUNNER_USER="${RUNNER_USER:-${SUDO_USER:-$(id -un)}}"
FCOS_STREAM="${FCOS_STREAM:-stable}"
GO_VERSION="${GO_VERSION:-1.25.6}"
# CI knobs (env): SKIP_GO=1 when actions/setup-go provides Go; SKIP_PREFLIGHT=1
# when the workflow runs its own preflight step afterward.
SKIP_GO="${SKIP_GO:-0}"
SKIP_PREFLIGHT="${SKIP_PREFLIGHT:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --openstack) WITH_OPENSTACK=1 ;;
    --no-vm)     NO_VM=1 ;;          # skip qemu/KVM/image (OpenStack-only runner)
    --with-go)   : ;;                # accepted but redundant (Go installed by default)
    --user)      shift; RUNNER_USER="$1" ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
  shift
done

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNNER_HOME="$(getent passwd "$RUNNER_USER" | cut -d: -f6)"
[ -n "$RUNNER_HOME" ] || { echo "cannot resolve home for user '$RUNNER_USER'"; exit 1; }
CACHE_DIR="${CACHE_DIR:-$RUNNER_HOME/.cache/fcos-e2e}"

log()  { printf '\033[1;32m[setup %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die()  { printf '\033[1;31m[setup] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)"

as_runner() { sudo -u "$RUNNER_USER" -H bash -c "$1"; }

detect_pm() {
  for pm in apt-get dnf yum pacman zypper; do command -v "$pm" >/dev/null 2>&1 && { echo "$pm"; return; }; done
  die "no supported package manager found (apt/dnf/yum/pacman/zypper)"
}

install_pkgs() {
  local pm="$1"; shift
  log "installing via $pm: $*"
  case "$pm" in
    # DPkg::Lock::Timeout makes apt WAIT for the dpkg/lists lock instead of
    # failing immediately. The CI runs several e2e jobs concurrently on one
    # shared runner box, so two jobs can hit `apt-get` at the same instant
    # ("E: Could not get lock /var/lib/apt/lists/lock"); without the timeout
    # the loser aborts the whole provisioning step. 600s comfortably covers a
    # sibling job's install.
    apt-get) apt-get -o DPkg::Lock::Timeout=600 update -y && DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y "$@" ;;
    dnf|yum) "$pm" install -y "$@" ;;
    pacman)  pacman -Sy --needed --noconfirm "$@" ;;
    zypper)  zypper --non-interactive install -y "$@" ;;
  esac
}

# Per-distro package names for the VM tier. Verified by binary check afterwards.
vm_packages() {
  case "$1" in
    apt-get) echo "qemu-system-x86 qemu-utils jq xz-utils curl openssh-client podman ca-certificates" ;;
    dnf|yum) echo "qemu-system-x86 qemu-img jq xz curl openssh-clients podman butane ca-certificates" ;;
    pacman)  echo "qemu-base qemu-img jq xz curl openssh podman" ;;
    zypper)  echo "qemu-x86 qemu-tools jq xz curl openssh podman" ;;
  esac
}

openstack_packages() {
  case "$1" in
    apt-get) echo "python3-openstackclient python3-magnumclient" ;;
    dnf|yum) echo "python3-openstackclient python3-magnumclient" ;;
    pacman)  echo "python-openstackclient" ;;
    zypper)  echo "python3-openstackclient python3-magnumclient" ;;
  esac
}

# Tools needed even on an OpenStack-only (--no-vm) runner. `make` builds the
# reconciler + e2e helpers (run-fcos-e2e.sh calls `make build`).
base_packages() {
  case "$1" in
    apt-get) echo "jq curl xz-utils openssh-client ca-certificates make" ;;
    dnf|yum) echo "jq curl xz openssh-clients ca-certificates make" ;;
    pacman)  echo "jq curl xz openssh make" ;;
    zypper)  echo "jq curl xz openssh make" ;;
  esac
}

enable_kvm() {
  log "enabling nested KVM + group access for '$RUNNER_USER'"
  cat > /etc/modprobe.d/99-kvm-nested.conf <<'EOF'
options kvm_intel nested=1
options kvm_amd nested=1
EOF
  modprobe kvm 2>/dev/null || true
  modprobe kvm_intel nested=1 2>/dev/null || modprobe kvm_amd nested=1 2>/dev/null || true
  getent group kvm >/dev/null || groupadd -r kvm || true
  usermod -aG kvm "$RUNNER_USER" || true
  if [ -e /dev/kvm ]; then
    chgrp kvm /dev/kvm 2>/dev/null || true
    chmod 660 /dev/kvm 2>/dev/null || true
    log "/dev/kvm present"
  else
    log "WARNING: /dev/kvm not present — host may lack virtualization or nested virt"
  fi
}

install_kubectl() {
  command -v kubectl >/dev/null 2>&1 && { log "kubectl present"; return; }
  local v; v="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
  log "installing kubectl $v"
  curl -fsSL "https://dl.k8s.io/release/${v}/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl
  chmod 755 /usr/local/bin/kubectl
}

install_go() {
  command -v go >/dev/null 2>&1 && { log "go present ($(go version))"; return; }
  log "installing Go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
}

precache_fcos() {
  log "pre-caching Fedora CoreOS '${FCOS_STREAM}' into $CACHE_DIR (as $RUNNER_USER)"
  as_runner "mkdir -p '$CACHE_DIR'"
  local img="$CACHE_DIR/fcos-${FCOS_STREAM}.qcow2"
  if as_runner "[ -f '$img' ]"; then log "image already cached"; return; fi
  local meta url xz
  meta="$(curl -fsSL "https://builds.coreos.fedoraproject.org/streams/${FCOS_STREAM}.json")"
  url="$(echo "$meta" | jq -r '.architectures.x86_64.artifacts.qemu.formats["qcow2.xz"].disk.location')"
  [ -n "$url" ] && [ "$url" != "null" ] || die "could not resolve FCoS qcow2 url"
  xz="$CACHE_DIR/$(basename "$url")"
  log "downloading $url"
  as_runner "curl -fsSL '$url' -o '$xz'"
  log "decompressing"
  as_runner "xz -dkc '$xz' > '$img.tmp' && mv '$img.tmp' '$img'"
  log "cached $img"
}

precache_butane() {
  command -v butane >/dev/null 2>&1 && { log "native butane present — skipping container pull"; return; }
  if as_runner "command -v podman >/dev/null 2>&1"; then
    log "pre-pulling quay.io/coreos/butane:release"
    as_runner "podman pull quay.io/coreos/butane:release" || log "WARNING: butane image pull failed (will retry at test time)"
  fi
}

main() {
  local pm; pm="$(detect_pm)"
  log "package manager: $pm; runner user: $RUNNER_USER; home: $RUNNER_HOME; no_vm=$NO_VM"

  # shellcheck disable=SC2046
  install_pkgs "$pm" $(base_packages "$pm") || die "base package install failed"
  if [ "$NO_VM" != "1" ]; then
    # shellcheck disable=SC2046
    install_pkgs "$pm" $(vm_packages "$pm") || die "VM package install failed"
    enable_kvm
  fi
  install_kubectl
  # Go builds the reconciler + e2e helpers. Skipped when CI's setup-go provides
  # it (SKIP_GO=1); otherwise installed to /usr/local for self-sufficiency.
  [ "$SKIP_GO" = "1" ] || install_go
  if [ "$WITH_OPENSTACK" = "1" ]; then
    # shellcheck disable=SC2046
    install_pkgs "$pm" $(openstack_packages "$pm") || log "WARNING: OpenStack client install failed — install python-magnumclient manually"
  fi

  if [ "$NO_VM" != "1" ]; then
    precache_fcos
    precache_butane
  fi

  if [ "$SKIP_PREFLIGHT" = "1" ]; then
    log "SETUP COMPLETE ✅ (preflight skipped; the workflow runs its own)"
    return 0
  fi
  log "verifying with preflight"
  local pf_args=""; [ "$WITH_OPENSTACK" = "1" ] && pf_args="--openstack"
  if as_runner "CACHE_DIR='$CACHE_DIR' bash '$SELF_DIR/runner-preflight.sh' $pf_args"; then
    log "SETUP COMPLETE ✅  (if the runner user was just added to 'kvm', restart the runner service so the group takes effect)"
  else
    die "preflight reported issues — see output above"
  fi
}

main "$@"
