#!/bin/bash
#
# runner-preflight.sh — verify a self-hosted runner can host the e2e tests.
# Read-only: it checks, it never installs. Run it on the runner, or as the first
# CI step so jobs fail fast with an actionable reason.
#
#   ./runner-preflight.sh            # FCoS VM tier checks (default)
#   ./runner-preflight.sh --openstack # real-OpenStack tier checks
#
set -uo pipefail

MODE="vm"
[ "${1:-}" = "--openstack" ] && MODE="openstack"

VM_MEM_MB="${VM_MEM_MB:-6144}"
MIN_DISK_GB="${MIN_DISK_GB:-40}"
fails=0
warns=0

pass() { printf '  \033[1;32mPASS\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33mWARN\033[0m %s\n' "$*"; warns=$((warns+1)); }
fail() { printf '  \033[1;31mFAIL\033[0m %s\n' "$*"; fails=$((fails+1)); }

have() { command -v "$1" >/dev/null 2>&1; }
check_tool() { if have "$1"; then pass "$1 ($(command -v "$1"))"; else fail "$1 not found"; fi; }

echo "== runner preflight (${MODE} tier) =="

echo "- core tools"
check_tool go
check_tool make
check_tool jq
check_tool curl
check_tool ssh
check_tool scp

if [ "$MODE" = "vm" ]; then
  echo "- virtualization"
  if [ -e /dev/kvm ]; then
    pass "/dev/kvm present"
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
      pass "/dev/kvm is read/write for $(id -un)"
    else
      fail "/dev/kvm not accessible — add the runner user to the 'kvm' group"
    fi
  else
    fail "/dev/kvm missing — host needs KVM (and nested virt if the runner is itself a VM)"
  fi
  for p in /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested; do
    if [ -r "$p" ]; then
      v="$(cat "$p")"
      case "$v" in Y|1) pass "nested virt enabled ($p=$v)";; *) warn "nested virt off ($p=$v) — only matters if this runner is a VM";; esac
    fi
  done

  echo "- VM tools"
  check_tool qemu-system-x86_64
  check_tool qemu-img
  check_tool xz
  if have podman; then pass "podman (for butane rendering)"
  elif have docker; then warn "podman missing but docker present — adjust run-fcos-e2e.sh to use docker for butane"
  else fail "podman not found (needed to render Ignition via quay.io/coreos/butane)"; fi

  echo "- capacity"
  cpus="$(nproc 2>/dev/null || echo 0)"
  [ "$cpus" -ge 4 ] && pass "cpus=$cpus" || warn "cpus=$cpus (<4); VM uses 4 vCPUs"
  if have free; then
    mem_mb="$(free -m | awk '/^Mem:/{print $2}')"
    [ "${mem_mb:-0}" -ge $((VM_MEM_MB + 2048)) ] && pass "RAM ${mem_mb}MB" \
      || warn "RAM ${mem_mb}MB may be tight (VM wants ${VM_MEM_MB}MB + host overhead; matrix may run 2 jobs)"
  fi
  disk_gb="$(df -BG --output=avail "${HOME}" 2>/dev/null | tail -1 | tr -dc '0-9')"
  [ "${disk_gb:-0}" -ge "$MIN_DISK_GB" ] && pass "free disk ${disk_gb}G in \$HOME" \
    || warn "free disk ${disk_gb}G (<${MIN_DISK_GB}G) for FCoS image + overlay + pulled images"

  echo "- egress (image + binaries + charts)"
  for url in https://builds.coreos.fedoraproject.org https://dl.k8s.io https://quay.io; do
    if curl -fsS --max-time 10 -o /dev/null "$url" 2>/dev/null; then pass "reachable: $url"; else warn "unreachable: $url (guest needs this too)"; fi
  done
else
  echo "- OpenStack tools"
  check_tool openstack
  check_tool kubectl
  echo "- auth"
  if [ -n "${OS_CLOUD:-}${OS_AUTH_URL:-}" ]; then
    if openstack coe cluster template list >/dev/null 2>&1; then pass "Magnum API reachable + authenticated"
    else fail "cannot reach/authenticate Magnum API (check clouds.yaml / OS_* and Octavia/Cinder access)"; fi
  else
    warn "no OS_CLOUD/OS_AUTH_URL in env — set before running the OpenStack tier"
  fi
fi

echo
if [ "$fails" -gt 0 ]; then
  printf '\033[1;31mPREFLIGHT FAILED: %d hard issue(s), %d warning(s)\033[0m\n' "$fails" "$warns"
  exit 1
fi
printf '\033[1;32mPREFLIGHT OK\033[0m (%d warning(s))\n' "$warns"
