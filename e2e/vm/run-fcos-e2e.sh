#!/bin/bash
#
# run-fcos-e2e.sh — host orchestrator for the Fedora CoreOS reconciler e2e.
#
# Boots real Fedora CoreOS VM(s) under QEMU/KVM, drives the actual ventus-ag/magnum
# bootstrap pipeline (launcher + systemd units) against the freshly built
# reconciler, and walks the cluster through create -> ca-rotate -> upgrade,
# asserting health and idempotency. OpenStack-integrated addons (OCCM, Cinder/
# Manila CSI) are intentionally OFF here — see run-magnum-e2e.sh.
#
# Single node (default, WORKERS=0): one VM, user-mode networking, KUBE_NODE_IP
# 10.0.2.15, mock Magnum on 127.0.0.1 — the proven path.
#
# Multi node (WORKERS>=1): each VM gets a second NIC on a shared QEMU socket/
# mcast segment (no host bridge/root needed); MAC-matched static IPs form a
# cluster network (master 192.168.77.10, workers .20+). The mock Magnum runs on
# the master's cluster IP so workers can reach it; workers join the master's API.
# NOTE: the inter-VM path is a first cut — validate on the runner.
#
# Requires: qemu-system-x86_64 (KVM), qemu-img, jq, xz, curl, butane|podman|docker,
# ssh/scp, go.
#
# Env knobs (defaults):
#   KUBE_TAG v1.30.5   KUBE_TAG_UPGRADE v1.31.4   FCOS_STREAM stable
#   VICTORIA_DIR (required)   SCENARIOS (default: create ca-rotate upgrade)
#   WORKERS 0          MASTER_MEM_MB 6144   MASTER_CPUS 4
#                      WORKER_MEM_MB 2560   WORKER_CPUS 2
#   WORKDIR (mktemp)   KEEP_VM 0
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

KUBE_TAG="${KUBE_TAG:-v1.30.5}"
KUBE_TAG_UPGRADE="${KUBE_TAG_UPGRADE:-v1.31.4}"
FCOS_STREAM="${FCOS_STREAM:-stable}"
VICTORIA_DIR="${VICTORIA_DIR:-}"
SCENARIOS="${SCENARIOS:-create ca-rotate upgrade}"
WORKERS="${WORKERS:-0}"

# Per-role sizing (VM_MEM_MB/VM_CPUS kept as backward-compatible fallbacks).
MASTER_MEM_MB="${MASTER_MEM_MB:-${VM_MEM_MB:-6144}}"
MASTER_CPUS="${MASTER_CPUS:-${VM_CPUS:-4}}"
WORKER_MEM_MB="${WORKER_MEM_MB:-2560}"
WORKER_CPUS="${WORKER_CPUS:-2}"

# Cluster network (multi-node only).
CNET="${CNET:-192.168.77}"
MCAST="${MCAST:-230.0.0.77:34801}"
MASTER_CIP="${CNET}.10"
MASTER_CMAC="52:54:00:77:00:0a"

# QEMU CPU/accel. QEMU_CPU=auto (default) resolves at preflight. On a nested
# AMD-V host the FCoS guest soft-locks in early boot with >1 vCPU (TSC warps
# between vCPUs + a cross-vCPU TLB-flush IPI never completes), so auto uses a
# named model ($QEMU_CPU_AMD, default EPYC) AND caps to 1 vCPU/node; everywhere
# else auto = host with the configured vCPU count. Override the model with e.g.
# QEMU_CPU=EPYC-Milan / max / host, keep multi-vCPU with ALLOW_SMP_ON_NESTED_AMD=1,
# or rule KVM out entirely (slow) with QEMU_ACCEL=tcg QEMU_CPU=qemu64.
QEMU_ACCEL="${QEMU_ACCEL:-kvm}"
QEMU_CPU="${QEMU_CPU:-auto}"
QEMU_CPU_AMD="${QEMU_CPU_AMD:-EPYC}"

GUEST_E2E_DIR=/opt/e2e
KEEP_VM="${KEEP_VM:-0}"
WORKDIR="${WORKDIR:-$(mktemp -d /tmp/fcos-e2e.XXXXXX)}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/fcos-e2e}"
BUTANE=""
declare -A QEMU_PIDS=()

log()  { printf '\033[1;32m[host %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err()  { printf '\033[1;31m[host %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die()  { err "$*"; exit 1; }

multinode() { [ "${WORKERS:-0}" -ge 1 ]; }

# --- node identity helpers -------------------------------------------------
worker_ip()   { echo "${CNET}.$((20 + $1))"; }
worker_cmac() { printf '52:54:00:77:00:%02x' "$((20 + $1))"; }
master_ip()   { if multinode; then echo "$MASTER_CIP"; else echo "10.0.2.15"; fi; }
mock_host()   { if multinode; then echo "$MASTER_CIP"; else echo "127.0.0.1"; fi; }
ssh_port()    { case "$1" in master) echo 2222 ;; *) echo "$((2300 + ${1#worker}))" ;; esac; }

require() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }
resolve_butane() {
  if command -v butane >/dev/null 2>&1; then BUTANE="native"
  elif command -v podman >/dev/null 2>&1; then BUTANE="podman"
  elif command -v docker >/dev/null 2>&1; then BUTANE="docker"
  else die "need 'butane', or 'podman'/'docker' to render Ignition"; fi
}

# resolve_qemu_cpu — turn QEMU_CPU=auto into a concrete -cpu model. The fragile
# case is a nested AMD-V host: there, `-cpu host` passes through SVM features that
# don't virtualize at the L2 level and the guest kernel stalls in do_initcalls.
# A named EPYC model avoids that. Explicit QEMU_CPU values are left untouched.
resolve_qemu_cpu() {
  [ "$QEMU_CPU" = auto ] || { log "QEMU accel=$QEMU_ACCEL cpu=$QEMU_CPU (explicit)"; return; }
  if [ "$QEMU_ACCEL" != kvm ]; then
    QEMU_CPU=qemu64
    log "QEMU cpu=auto -> qemu64 (accel=$QEMU_ACCEL, no KVM)"
    return
  fi
  local virt vendor
  virt="$(systemd-detect-virt 2>/dev/null || echo none)"
  vendor="$(awk -F': ' '/^vendor_id/{print $2; exit}' /proc/cpuinfo 2>/dev/null)"
  if [ "$virt" != none ] && [ "$vendor" = AuthenticAMD ]; then
    QEMU_CPU="$QEMU_CPU_AMD"
    log "QEMU cpu=auto -> $QEMU_CPU (nested AMD-V '$virt')"
    if [ "${ALLOW_SMP_ON_NESTED_AMD:-0}" != 1 ]; then
      # Nested AMD-V (especially Zen1) livelocks with >1 vCPU during early boot:
      # the TSC warps between vCPUs (clock marked unstable) and a cross-vCPU
      # TLB-flush IPI in smp_call_function_many never completes -> all vCPUs spin
      # 100% -> soft lockup -> hang. A single vCPU removes both the inter-vCPU TSC
      # sync and the cross-CPU IPI. Override with ALLOW_SMP_ON_NESTED_AMD=1.
      MASTER_CPUS=1; WORKER_CPUS=1
      log "nested AMD-V: capping nodes to 1 vCPU (multi-vCPU soft-locks in early boot; set ALLOW_SMP_ON_NESTED_AMD=1 to keep the configured count)"
    fi
  else
    QEMU_CPU=host
    log "QEMU cpu=auto -> host (virt=$virt vendor=${vendor:-unknown})"
  fi
}

SSHBASE=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
         -o ConnectTimeout=10 -i "$WORKDIR/id_ed25519")
gssh() { local p="$1"; shift; ssh "${SSHBASE[@]}" -p "$p" root@127.0.0.1 "$@"; }
gscp() { local p="$1"; shift; scp "${SSHBASE[@]}" -P "$p" "$@"; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    err "failure (rc=$rc) — collecting master diagnostics"
    gssh "$(ssh_port master)" 'cat /var/lib/magnum/reconciler-last-run.json 2>/dev/null; echo; tail -120 /var/log/magnum-reconcile.log 2>/dev/null' 2>/dev/null || true
  fi
  if [ "$KEEP_VM" = "1" ]; then
    log "KEEP_VM=1 — leaving VMs up (master: ssh -p $(ssh_port master) -i $WORKDIR/id_ed25519 root@127.0.0.1)"
  else
    for name in "${!QEMU_PIDS[@]}"; do kill "${QEMU_PIDS[$name]}" 2>/dev/null || true; done
  fi
  exit "$rc"
}
trap cleanup EXIT

# --- setup -----------------------------------------------------------------
preflight() {
  for t in qemu-system-x86_64 qemu-img jq xz curl ssh scp go make; do require "$t"; done
  resolve_butane
  resolve_qemu_cpu
  log "butane renderer: $BUTANE; workers: $WORKERS"
  if [ "$QEMU_ACCEL" = kvm ]; then
    [ -e /dev/kvm ] || die "/dev/kvm not present — KVM acceleration is required (or set QEMU_ACCEL=tcg)"
  fi
  [ -n "$VICTORIA_DIR" ] || die "VICTORIA_DIR must point at a ventus-ag/magnum checkout (forked driver)"
  [ -d "$VICTORIA_DIR/magnum/drivers/common/templates/kubernetes/bootstrap" ] \
    || die "VICTORIA_DIR does not look like a ventus-ag/magnum checkout (missing magnum/drivers/.../bootstrap)"
  mkdir -p "$WORKDIR" "$CACHE_DIR"
  log "workdir: $WORKDIR"
}

build_binaries() {
  log "building reconciler + e2e helpers"
  ( cd "$REPO_ROOT" && make build >/dev/null )
  ( cd "$REPO_ROOT" && go build -o "$WORKDIR/mock-magnum"  ./e2e/cmd/mock-magnum )
  ( cd "$REPO_ROOT" && go build -o "$WORKDIR/scenario-gen" ./e2e/cmd/scenario-gen )
  cp "$REPO_ROOT/dist/bootstrap" "$WORKDIR/bootstrap"
}

generate_keys() {
  log "generating SSH key + shared mock CA"
  ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id_ed25519" -q
  "$WORKDIR/mock-magnum" -gen-ca -ca-cert "$WORKDIR/ca.crt" -ca-key "$WORKDIR/ca.key" >/dev/null
}

download_fcos() {
  local meta url xz img="$CACHE_DIR/fcos-${FCOS_STREAM}.qcow2"
  if [ -f "$img" ]; then echo "$img"; return; fi
  log "resolving Fedora CoreOS ${FCOS_STREAM} qcow2"
  meta="$(curl -fsSL "https://builds.coreos.fedoraproject.org/streams/${FCOS_STREAM}.json")"
  url="$(echo "$meta" | jq -r '.architectures.x86_64.artifacts.qemu.formats["qcow2.xz"].disk.location')"
  [ -n "$url" ] && [ "$url" != "null" ] || die "could not resolve FCoS qcow2 url"
  xz="$CACHE_DIR/$(basename "$url")"
  log "downloading $url"; curl -fsSL "$url" -o "$xz"
  log "decompressing image"; xz -dkc "$xz" > "$img.tmp" && mv "$img.tmp" "$img"
  echo "$img"
}

# render_ignition <name> <hostname> <cluster_mac> <cluster_ip>
render_ignition() {
  local name="$1" hostname="$2" cmac="$3" cip="$4" pubkey base
  pubkey="$(cat "$WORKDIR/id_ed25519.pub")"
  base="$(download_fcos)"
  sed -e "s|@@SSH_AUTHORIZED_KEY@@|${pubkey}|g" \
      -e "s|@@HOSTNAME@@|${hostname}|g" \
      -e "s|@@CLUSTER_MAC@@|${cmac}|g" \
      -e "s|@@CLUSTER_IP@@|${cip}|g" \
      "$REPO_ROOT/e2e/vm/butane.yaml" > "$WORKDIR/butane.${name}.yaml"
  case "$BUTANE" in
    native) butane --pretty --strict < "$WORKDIR/butane.${name}.yaml" > "$WORKDIR/ignition.${name}.json" ;;
    podman) podman run --rm -i quay.io/coreos/butane:release --pretty --strict < "$WORKDIR/butane.${name}.yaml" > "$WORKDIR/ignition.${name}.json" ;;
    docker) docker run --rm -i quay.io/coreos/butane:release --pretty --strict < "$WORKDIR/butane.${name}.yaml" > "$WORKDIR/ignition.${name}.json" ;;
  esac
  qemu-img create -f qcow2 -b "$base" -F qcow2 "$WORKDIR/${name}.qcow2" 25G >/dev/null
}

# boot_node <name> <ssh_port> <mem_mb> <cpus> <cluster_mac|"">
boot_node() {
  local name="$1" port="$2" mem="$3" cpus="$4" cmac="$5"
  local net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${port}-:22" -device virtio-net-pci,netdev=n0)
  if [ -n "$cmac" ]; then
    # Second NIC on a shared QEMU mcast segment = the cluster network.
    net+=(-netdev "socket,id=n1,mcast=${MCAST}" -device "virtio-net-pci,netdev=n1,mac=${cmac}")
  fi
  log "booting $name (mem=${mem}MB cpus=${cpus} accel=${QEMU_ACCEL} cpu=${QEMU_CPU} ssh=:$port${cmac:+ cluster-mac=$cmac})"
  qemu-system-x86_64 \
    -machine "accel=${QEMU_ACCEL}" -cpu "$QEMU_CPU" -smp "$cpus" -m "$mem" \
    -nographic -serial file:"$WORKDIR/console.${name}.log" -monitor none \
    -drive if=virtio,file="$WORKDIR/${name}.qcow2",format=qcow2 \
    -fw_cfg name=opt/com.coreos/config,file="$WORKDIR/ignition.${name}.json" \
    "${net[@]}" &
  QEMU_PIDS[$name]=$!
  local clog="$WORKDIR/console.${name}.log" waited=0
  log "$name qemu pid ${QEMU_PIDS[$name]} — waiting for SSH on 127.0.0.1:$port (console: $clog)"
  for attempt in $(seq 1 120); do
    if gssh "$port" true 2>/dev/null; then log "$name SSH up after ${waited}s"; return 0; fi
    if ! kill -0 "${QEMU_PIDS[$name]}" 2>/dev/null; then
      err "$name qemu exited early after ${waited}s — last 40 console lines:"
      tail -n 40 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
      die "$name qemu exited early (see $clog)"
    fi
    # A fatal guest condition (panic, GPF, or — common under nested KVM — an RCU
    # stall / soft lockup / hung task during early init) never recovers. Surface
    # the reason and abort instead of polling for the full timeout.
    if grep -qiE 'Kernel panic|BUG:|Oops:|general protection fault|detected stall|soft lockup|hung task|Unable to mount root|not syncing' "$clog" 2>/dev/null; then
      err "$name fatal guest condition during boot after ${waited}s (SSH will never come up) — last 150 console lines:"
      tail -n 150 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
      die "$name guest boot crash — try QEMU_CPU=max (or QEMU_ACCEL=tcg QEMU_CPU=qemu64); full console: $clog"
    fi
    # Heartbeat every ~30s with a console tail so a stuck boot/Ignition is visible.
    if [ $((attempt % 6)) -eq 0 ]; then
      log "$name still waiting for SSH (${waited}s elapsed) — console tail:"
      tail -n 8 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || echo "    | (console log empty — qemu may not have started serial output yet)" >&2
    fi
    sleep 5; waited=$((waited + 5))
  done
  err "$name SSH did not come up after ${waited}s — last 60 console lines:"
  tail -n 60 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
  die "$name SSH did not come up (see $clog)"
}

# provision_node <name> <ssh_port> <start_mock 0|1>
provision_node() {
  local name="$1" port="$2" start_mock="$3"
  log "provisioning $name"
  gssh "$port" "mkdir -p $GUEST_E2E_DIR /opt/victoria-bootstrap"
  gscp "$port" "$WORKDIR/bootstrap" "$WORKDIR/mock-magnum" "$WORKDIR/ca.crt" "$WORKDIR/ca.key" \
       "$REPO_ROOT/e2e/vm/guest-run.sh" root@127.0.0.1:"$GUEST_E2E_DIR/" >/dev/null
  gscp "$port" "$VICTORIA_DIR"/magnum/drivers/common/templates/kubernetes/bootstrap/*.sh \
       root@127.0.0.1:/opt/victoria-bootstrap/ >/dev/null
  gssh "$port" "chmod +x $GUEST_E2E_DIR/guest-run.sh"
  gssh "$port" "START_MOCK=${start_mock} MOCK_LISTEN=$(mock_host):9511 $GUEST_E2E_DIR/guest-run.sh setup"
  gssh "$port" "$GUEST_E2E_DIR/guest-run.sh install /opt/victoria-bootstrap"
}

# render_and_push <node-key> <role> <op> <tag> <rot> <node-ip> [master-ip]
render_and_push() {
  local key="$1" role="$2" op="$3" tag="$4" rot="$5" nip="$6" mip="${7:-}"
  local idx=0; [ "$key" != master ] && idx="${key#worker}"
  local host="$(mock_host)" port; port="$(ssh_port "$key")"
  local hp="$WORKDIR/heat-params.${key}.${op}"
  "$WORKDIR/scenario-gen" \
    -role "$role" -op "$op" -node-index "$idx" -node-ip "$nip" ${mip:+-master-ip "$mip"} \
    -kube-tag "$tag" -ca-rotation-id "$rot" \
    -ca-key-file "$WORKDIR/ca.key" -sa-key-file "$WORKDIR/sa.pub" -sa-priv-file "$WORKDIR/sa.key" \
    -auth-url "http://${host}:9511/v3" -magnum-url "http://${host}:9511/v1" \
    -reconciler-version e2e -reconciler-binary-url "file://$GUEST_E2E_DIR/bootstrap" \
    -o "$hp"
  gscp "$port" "$hp" root@127.0.0.1:"$GUEST_E2E_DIR/heat-params.${op}" >/dev/null
  echo "$GUEST_E2E_DIR/heat-params.${op}"
}

# --- scenarios -------------------------------------------------------------
for_each_worker() {  # for_each_worker <fn>; calls fn <i>
  local fn="$1" i=0
  while [ "$i" -lt "$WORKERS" ]; do "$fn" "$i"; i=$((i + 1)); done
}

apply_master() {  # apply_master <op> <tag> [rot]
  local op="$1" tag="$2" rot="${3:-}" mp; mp="$(ssh_port master)"
  local hp; hp="$(render_and_push master master "$op" "$tag" "$rot" "$(master_ip)")"
  gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh apply $hp $op"
}

apply_worker() {  # apply_worker <i> <op> <tag>
  local i="$1" op="$2" tag="$3" wp; wp="$(ssh_port "worker$i")"
  local hp; hp="$(render_and_push "worker$i" worker "$op" "$tag" "" "$(worker_ip "$i")" "$(master_ip)")"
  gssh "$wp" "$GUEST_E2E_DIR/guest-run.sh apply $hp $op"
}

assert_worker_joined() {  # <i>
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-node-ready e2e-minion-$1"
}

scenario_create() {
  log "=== SCENARIO: create ==="
  apply_master create "$KUBE_TAG"
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  local hp; hp="$GUEST_E2E_DIR/heat-params.create"
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-noop $hp create-idempotency"
  for_each_worker _create_worker
}
_create_worker() { apply_worker "$1" create "$KUBE_TAG"; assert_worker_joined "$1"; }

scenario_ca_rotate() {
  log "=== SCENARIO: ca-rotate ==="
  apply_master ca-rotate "$KUBE_TAG" "rot-$(date +%s)"
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
}

scenario_upgrade() {
  log "=== SCENARIO: upgrade -> $KUBE_TAG_UPGRADE ==="
  apply_master upgrade "$KUBE_TAG_UPGRADE"
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  for_each_worker _upgrade_worker
}
_upgrade_worker() { apply_worker "$1" upgrade "$KUBE_TAG_UPGRADE"; assert_worker_joined "$1"; }

# --- main ------------------------------------------------------------------
boot_all() {
  if multinode; then
    render_ignition master e2e-master-0 "$MASTER_CMAC" "$MASTER_CIP"
    boot_node master "$(ssh_port master)" "$MASTER_MEM_MB" "$MASTER_CPUS" "$MASTER_CMAC"
    local i=0
    while [ "$i" -lt "$WORKERS" ]; do
      render_ignition "worker$i" "e2e-minion-$i" "$(worker_cmac "$i")" "$(worker_ip "$i")"
      boot_node "worker$i" "$(ssh_port "worker$i")" "$WORKER_MEM_MB" "$WORKER_CPUS" "$(worker_cmac "$i")"
      i=$((i + 1))
    done
  else
    # Single-node: original user-mode path, no cluster NIC.
    render_ignition master e2e-master-0 "$MASTER_CMAC" "$MASTER_CIP"
    boot_node master "$(ssh_port master)" "$MASTER_MEM_MB" "$MASTER_CPUS" ""
  fi
}

provision_all() {
  provision_node master "$(ssh_port master)" 1
  if multinode; then
    local i=0
    while [ "$i" -lt "$WORKERS" ]; do provision_node "worker$i" "$(ssh_port "worker$i")" 0; i=$((i + 1)); done
  fi
}

main() {
  preflight
  build_binaries
  generate_keys
  boot_all
  provision_all
  for s in $SCENARIOS; do
    case "$s" in
      create)    scenario_create ;;
      ca-rotate) scenario_ca_rotate ;;
      upgrade)   scenario_upgrade ;;
      *) die "unknown scenario: $s" ;;
    esac
  done
  log "ALL SCENARIOS PASSED ✅"
}

main "$@"
