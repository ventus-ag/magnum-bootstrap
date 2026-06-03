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
# Load balancers (default ON, mirrors Magnum master_lb_enabled=true): every run —
# even a single master — stands up a dedicated `lb` VM running two L4 TCP load
# balancers that stand in for the cluster's Octavia api_lb + etcd_lb:
#     api_lb  (KUBE_API_PRIVATE/PUBLIC_ADDRESS) -> VIP 192.168.77.8 :6443
#     etcd_lb (ETCD_LB_VIP)                     -> VIP 192.168.77.9 :2379
# Both VIPs live on the lb node (NOT a master: apiserver binds 0.0.0.0:6443, so a
# co-located api VIP would clash). mock-lb is L4 pass-through, so the masters'
# real TLS flows end-to-end. Set MASTER_LB_ENABLED=false for the no-LB direct
# path (= Heat no_master_lb): single master only, user-mode net, no lb node.
#
# Because the LB is on by default, every run uses a shared cluster network: each
# VM gets a second NIC on a QEMU socket/mcast segment (no host bridge/root) with
# MAC-matched static IPs (lb .8/.9, masters .10/.11/..., workers .20+). The mock
# Magnum runs on master-0's cluster IP; workers/clients reach the API through the
# api VIP.
#
# Multi master (MASTERS>=2): masters join sequentially through the etcd VIP
# (member add) and serve behind the api VIP; CA rotation is applied to all
# masters concurrently so the dual-CA barrier's per-master restart serialization
# is exercised. TCG-slow, so >1 master is opt-in (MASTERS stays 1 by default).
# NOTE: the inter-VM + LB path is validated on the runner.
#
# Requires: qemu-system-x86_64 (KVM), qemu-img, jq, xz, curl, butane|podman|docker,
# ssh/scp, go.
#
# Env knobs (defaults):
#   KUBE_TAG v1.30.5   KUBE_TAG_UPGRADE v1.31.4   FCOS_STREAM stable
#   FCOS_VERSION (pin an older, pre-composefs build, e.g. 38.20231027.3.2 — old
#                 FCoS + v1 containerd layout, the production node layout)
#   VICTORIA_DIR (required)   SCENARIOS (default: create ca-rotate upgrade)
#   WORKERS 0          MASTERS 1            MASTER_LB_ENABLED true
#   MASTER_MEM_MB 2048 MASTER_CPUS 1        WORKER_MEM_MB 2048   WORKER_CPUS 1
#   LB_MEM_MB 768      LB_CPUS 1
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
MASTERS="${MASTERS:-1}"   # >=2 enables the multi-master + 2-LB path (opt-in; TCG-slow)

# Per-role sizing (VM_MEM_MB/VM_CPUS kept as backward-compatible fallbacks).
MASTER_MEM_MB="${MASTER_MEM_MB:-${VM_MEM_MB:-2048}}"
MASTER_CPUS="${MASTER_CPUS:-${VM_CPUS:-1}}"
WORKER_MEM_MB="${WORKER_MEM_MB:-2048}"
WORKER_CPUS="${WORKER_CPUS:-1}"

# Cluster network (multi-node only).
# Per-run isolation. Multiple pipelines can share one host without colliding on
# the cluster L2 (mcast group), subnet, or SSH host-ports by deriving everything
# from a slot. Provide E2E_SLOT for a deterministic slot; otherwise derive one
# from the CI run id; else 0. (A single self-hosted runner runs one job at a
# time, so concurrency only happens with multiple runner instances per host —
# the slot makes that safe.)
SLOT="${E2E_SLOT:-}"
if [ -z "$SLOT" ]; then
  if [ -n "${GITHUB_RUN_ID:-}" ]; then SLOT=$(( (GITHUB_RUN_ID + ${GITHUB_RUN_ATTEMPT:-0}) % 150 ))
  else SLOT=0; fi
fi
# Unique /24, unique mcast group+port, unique SSH port window per slot.
CNET="${CNET:-192.168.$((100 + SLOT % 150))}"
MCAST="${MCAST:-230.$((SLOT / 256)).$((SLOT % 256)).77:$((34801 + SLOT))}"
PORT_BASE="${PORT_BASE:-$((2200 + (SLOT % 200) * 256))}"
MASTER_CIP="${CNET}.10"
MASTER_CMAC="52:54:00:77:00:0a"
# Control-plane VIPs. These mirror Magnum's two Octavia LBs and live on the lb
# node (a master can't host the api VIP — apiserver binds 0.0.0.0:6443):
#   API_VIP  -> api_lb  (KUBE_API_PRIVATE/PUBLIC_ADDRESS), TCP :6443  (lb node primary IP)
#   ETCD_VIP -> etcd_lb (ETCD_LB_VIP),                     TCP :2379  (lb node alias IP)
API_VIP="${API_VIP:-${CNET}.8}"
ETCD_VIP="${ETCD_VIP:-${CNET}.9}"
LB_CIP="$API_VIP"               # the lb node's primary cluster IP
LB_CMAC="52:54:00:77:00:08"
LB_MEM_MB="${LB_MEM_MB:-768}"
LB_CPUS="${LB_CPUS:-1}"

# QEMU CPU/accel/machine. QEMU_CPU=auto (default) resolves at preflight. On a
# nested AMD-V host LAPIC interrupt delivery to the L2 guest is unreliable, which
# both soft-locks multi-vCPU boots and freezes single-vCPU boots; auto uses
# `-cpu host,-x2apic` (legacy xAPIC) + 1 vCPU to work around it. Override the
# model with QEMU_CPU=host/EPYC/max/..., the chipset with QEMU_MACHINE=q35, keep
# multi-vCPU with ALLOW_SMP_ON_NESTED_AMD=1, or rule KVM out entirely (slow) with
# QEMU_ACCEL=tcg QEMU_CPU=qemu64. SSH_WAIT_TRIES extends the per-boot wait (x5s).
QEMU_ACCEL="${QEMU_ACCEL:-kvm}"
QEMU_CPU="${QEMU_CPU:-auto}"
QEMU_MACHINE="${QEMU_MACHINE:-}"   # empty = qemu default (i440fx); try q35
QEMU_IRQCHIP="${QEMU_IRQCHIP:-}"   # empty = qemu default (full in-kernel); auto=split on nested AMD (KVM rejects 'off')
SSH_WAIT_TRIES="${SSH_WAIT_TRIES:-180}"   # per-attempt boot wait = tries x 5s (900s)

# Nested-virt boots can hang at a RANDOM early-init point with no panic — the
# console just stops. That is non-deterministic, so a fresh reboot usually clears
# it. boot_node retries on a clean overlay: BOOT_RETRIES extra attempts, each
# abandoned early once the console is idle BOOT_STALL_SECS with SSH still down (so
# a silent freeze costs ~BOOT_STALL_SECS, not the full SSH_WAIT_TRIES window).
BOOT_RETRIES="${BOOT_RETRIES:-}"          # extra boot attempts (auto: 1 on nested AMD, 0 otherwise)
BOOT_STALL_SECS="${BOOT_STALL_SECS:-120}" # console-idle seconds that mark a silent boot hang

# When KVM keeps hanging in early boot (broken nested interrupt delivery that no
# karg/QEMU flag can fix), fall back to pure-emulation TCG — it has no nested
# interrupt virtualization, so it boots where KVM freezes. Slow; good hosts never
# reach it (KVM wins on attempt 1). Set 0 to fail instead of falling back.
TCG_FALLBACK="${TCG_FALLBACK:-1}"

# First-boot kernel args. The nested-virt freeze happens in the dracut INITRAMFS,
# before Ignition runs — so Ignition's kernel_arguments (butane) are too late for
# it. INJECT_KARGS bakes FIRSTBOOT_KARGS straight into the image's BLS boot entry
# via qemu-nbd before QEMU starts, so the very first boot already has idle=poll
# (vCPU never halts -> the lost timer-wake that freezes the L2 guest can't occur).
# Auto-on for nested AMD; needs passwordless sudo + nbd, best-effort (warns+boots
# unmodified on failure). Set INJECT_KARGS=0 to disable.
INJECT_KARGS="${INJECT_KARGS:-}"
# idle=poll: vCPU never halts (no lost timer-wake). nox2apic: legacy xAPIC.
# no-kvmclock+tsc=reliable: stable TSC with 1 vCPU. pci=nomsi: force all PCI
# devices onto legacy INTx (IOAPIC) instead of MSI-X — on broken nested-AMD the
# virtio block/net completion IRQ is often delivered via INTx when MSI-X is lost
# (the boot otherwise stalls in dracut "Expecting device …/by-label/boot").
FIRSTBOOT_KARGS="${FIRSTBOOT_KARGS:-idle=poll nox2apic no-kvmclock tsc=reliable pci=nomsi}"

GUEST_E2E_DIR=/opt/e2e
KEEP_VM="${KEEP_VM:-0}"
ROT_SEQ=0     # monotonic counter so repeated ca-rotate scenarios get unique ids
LBS_STARTED=0 # set once the control-plane LBs are up on master-0 (multi-master)

# Trigger mode — how the reconciler is invoked on each node:
#   direct (default) - place heat-params + run the systemd unit directly (fast).
#   agent            - run the REAL heat-container-agent against a local mock
#                      Heat that serves a SoftwareDeployment (the real bootstrap
#                      scripts + ~90 inputs) and receives the HEAT_SIGNAL,
#                      exactly like Heat. Exercises write-heat-params + the
#                      install scripts + run-reconciler-once + the signal path.
TRIGGER="${TRIGGER:-direct}"
HEAT_LISTEN="${HEAT_LISTEN:-127.0.0.1:9512}"   # mock Heat bind, VM-local per node
AGENT_IMAGE="${AGENT_IMAGE:-docker.io/openstackmagnum/heat-container-agent:victoria-stable-1}"
WORKDIR="${WORKDIR:-$(mktemp -d /tmp/fcos-e2e.XXXXXX)}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/fcos-e2e}"
BUTANE=""
declare -A QEMU_PIDS=()

log()  { printf '\033[1;32m[host %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err()  { printf '\033[1;31m[host %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die()  { err "$*"; exit 1; }

multinode()   { [ "${WORKERS:-0}" -ge 1 ]; }
multimaster() { [ "${MASTERS:-1}" -ge 2 ]; }
lb_enabled()  { [ "${MASTER_LB_ENABLED:-true}" = true ]; }
# A shared cluster network is needed whenever there is more than one node — which,
# because the LB is on by default, is every run except an explicit
# MASTER_LB_ENABLED=false single master (the user-mode direct path).
clustered()   { lb_enabled || multimaster || multinode; }

# --- node identity helpers -------------------------------------------------
worker_ip()    { echo "${CNET}.$((20 + $1))"; }
worker_cmac()  { printf '52:54:00:77:00:%02x' "$((20 + $1))"; }
master_cip()   { echo "${CNET}.$((10 + $1))"; }                  # master i cluster IP (.10, .11, ...)
master_cmac()  { printf '52:54:00:77:00:%02x' "$((10 + $1))"; }  # master i cluster MAC
master_key()   { [ "$1" = 0 ] && echo master || echo "master$1"; }
master_nodeip(){ if clustered; then master_cip "$1"; else echo 10.0.2.15; fi; }
master_ip()    { master_nodeip 0; }                              # master-0 IP (workers/mock target)
mock_host()    { if clustered; then echo "$MASTER_CIP"; else echo "127.0.0.1"; fi; }
# The API endpoint workers/clients join: the api_lb VIP when the LB is enabled,
# else master-0 directly.
api_endpoint() { if lb_enabled; then echo "$API_VIP"; else master_ip; fi; }
# SSH host-ports, all within this run's PORT_BASE window (slot-isolated):
#   lb = +1, master-i = +10+i, worker-i = +50+i
ssh_port() {
  case "$1" in
    lb)       echo "$((PORT_BASE + 1))" ;;
    master)   echo "$((PORT_BASE + 10))" ;;
    master*)  echo "$((PORT_BASE + 10 + ${1#master}))" ;;
    *)        echo "$((PORT_BASE + 50 + ${1#worker}))" ;;
  esac
}

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
  QEMU_CPU=host
  if [ "$virt" != none ] && [ "$vendor" = AuthenticAMD ]; then
    # Nested AMD-V (esp. Zen1) breaks LAPIC interrupt delivery to the L2 guest.
    # In practice the full workaround set (legacy xAPIC + idle=poll + pci=nomsi +
    # kernel-irqchip=split + 1 vCPU) STILL freezes the boot right after
    # basic.target on these hosts: KVM is simply not viable here. So default
    # nested AMD-V straight to pure-emulation TCG — the path that actually boots —
    # instead of burning ~minutes per run on guaranteed-doomed KVM attempts before
    # the stall detector falls back anyway. Opt back into KVM (with the xAPIC
    # workarounds + TCG fallback) via ALLOW_KVM_ON_NESTED_AMD=1, or move the tier
    # to a sound-virt runner (Zen2+/Intel or bare-metal/--device /dev/kvm).
    if [ "${ALLOW_KVM_ON_NESTED_AMD:-0}" != 1 ]; then
      QEMU_ACCEL=tcg; QEMU_CPU=qemu64; QEMU_IRQCHIP=""
      SSH_WAIT_TRIES=$(( SSH_WAIT_TRIES > 360 ? SSH_WAIT_TRIES : 360 ))   # TCG boots slowly
      BOOT_STALL_SECS=$(( BOOT_STALL_SECS > 300 ? BOOT_STALL_SECS : 300 ))
      # The 1-vCPU cap below is a KVM nested-IPI workaround — irrelevant to TCG.
      # MTTCG runs each vCPU on its own host thread, so give the emulated guest
      # more cores (and the master more RAM) to cut the otherwise-painful k8s
      # bring-up time. Only bumps script-default sizing; explicit values win.
      local ncpu; ncpu="$(nproc 2>/dev/null || echo 4)"
      local tcg_cpus=$(( ncpu >= 6 ? 4 : (ncpu >= 4 ? 2 : 1) ))
      [ "${MASTER_CPUS:-1}" = 1 ] && MASTER_CPUS="$tcg_cpus"
      [ "${WORKER_CPUS:-1}" = 1 ] && WORKER_CPUS=$(( tcg_cpus >= 2 ? 2 : 1 ))
      [ "${MASTER_MEM_MB:-2048}" = 2048 ] && MASTER_MEM_MB=4096
      log "nested AMD-V '$virt': KVM interrupt delivery is unreliable here (boot freezes after basic.target even with xAPIC/idle=poll/irqchip=split) — using pure-emulation TCG (reliable but SLOW). Bumped to MTTCG ${MASTER_CPUS}c/${MASTER_MEM_MB}MB master, ${WORKER_CPUS}c workers (host ${ncpu} cores). Force KVM with ALLOW_KVM_ON_NESTED_AMD=1; better: use a sound-virt runner."
      return
    fi
    # ALLOW_KVM_ON_NESTED_AMD=1: attempt KVM with the legacy-xAPIC workarounds.
    # The TCG fallback in boot_node still catches a hang if KVM loses interrupts.
    # 1 vCPU also avoids the >1-vCPU cross-vCPU TLB-flush IPI soft-lockup (~28s).
    QEMU_CPU="host,-x2apic"
    log "QEMU cpu=auto -> host,-x2apic (nested AMD-V '$virt': ALLOW_KVM_ON_NESTED_AMD=1, legacy xAPIC; TCG fallback still applies)"
    if [ "${ALLOW_SMP_ON_NESTED_AMD:-0}" != 1 ]; then
      MASTER_CPUS=1; WORKER_CPUS=1
      log "nested AMD-V: capping nodes to 1 vCPU (set ALLOW_SMP_ON_NESTED_AMD=1 to keep the configured count)"
    fi
    # Early-boot freezes here are non-deterministic; retry once on a fresh overlay.
    [ -z "$BOOT_RETRIES" ] && { BOOT_RETRIES=1; log "nested AMD-V: enabling $BOOT_RETRIES boot retry for silent early-boot hangs (override with BOOT_RETRIES)"; }
    # The freeze is in the initramfs (pre-Ignition); bake idle=poll onto firstboot.
    [ -z "$INJECT_KARGS" ] && { INJECT_KARGS=1; log "nested AMD-V: will inject first-boot kargs '$FIRSTBOOT_KARGS' into the image (override with INJECT_KARGS=0)"; }
    # Move the IOAPIC/PIC to QEMU userspace. NOTE: kernel-irqchip=off (full
    # userspace APIC, incl. LAPIC) is REJECTED by KVM on x86 ("KVM does not
    # support userspace APIC") — KVM requires an in-kernel LAPIC — so it instantly
    # kills every KVM attempt and is never actually functional. Use `split`, which
    # IS KVM-valid (userspace IOAPIC, in-kernel LAPIC); combined with -x2apic +
    # idle=poll + 1 vCPU it's the workable nested-AMD config. If KVM interrupt
    # delivery is still broken on the host, the stall detector falls back to TCG.
    [ -z "$QEMU_IRQCHIP" ] && { QEMU_IRQCHIP=split; log "nested AMD-V: using kernel-irqchip=split (KVM rejects 'off'/userspace APIC; override with QEMU_IRQCHIP=on)"; }
  else
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
    # Also dump the cluster's nodes/pods so a failure shows what did/didn't come up.
    gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh dump-state" 2>/dev/null || true
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
  BOOT_RETRIES="${BOOT_RETRIES:-0}"   # default 0 (single attempt) unless nested AMD raised it
  INJECT_KARGS="${INJECT_KARGS:-0}"   # default off unless nested AMD turned it on
  # In-guest waits (node Ready, core pods, agent deployment) are sized for KVM.
  # Pure-emulation TCG is multiples slower, so scale the guest-side timeouts to
  # avoid false-timeouts mid-scenario. Override with WAIT_SCALE.
  WAIT_SCALE="${WAIT_SCALE:-$([ "$QEMU_ACCEL" = tcg ] && echo 3 || echo 1)}"
  log "butane renderer: $BUTANE; workers: $WORKERS; boot retries: $BOOT_RETRIES (stall=${BOOT_STALL_SECS}s); inject-kargs: $INJECT_KARGS; irqchip: ${QEMU_IRQCHIP:-default}; accel: ${QEMU_ACCEL}; wait-scale: ${WAIT_SCALE}x"
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
  ( cd "$REPO_ROOT" && go build -o "$WORKDIR/mock-lb" ./e2e/cmd/mock-lb )
  [ "$TRIGGER" = agent ] && ( cd "$REPO_ROOT" && go build -o "$WORKDIR/mock-heat" ./e2e/cmd/mock-heat )
  cp "$REPO_ROOT/dist/bootstrap" "$WORKDIR/bootstrap"
}

generate_keys() {
  log "generating SSH key + shared mock CA"
  ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id_ed25519" -q
  "$WORKDIR/mock-magnum" -gen-ca -ca-cert "$WORKDIR/ca.crt" -ca-key "$WORKDIR/ca.key" >/dev/null
}

# download_fcos — resolve + cache the FCoS qcow2. By default it pulls the latest
# build of FCOS_STREAM. Set FCOS_VERSION to pin a specific older build, e.g.
# FCOS_VERSION=38.20231027.3.2 — an old, PRE-composefs FCoS whose /usr is a
# normal writable tree (no read-only overlay), which is the layout production
# Magnum nodes run and which exercises the v1 cri-containerd bundle path (extract
# straight to /) instead of the composefs /var/usrlocal copy path. Pinning is how
# the tier covers "old FCoS + old containerd" alongside the default modern image.
download_fcos() {
  local meta url xz tag img
  tag="${FCOS_VERSION:-$FCOS_STREAM}"
  img="$CACHE_DIR/fcos-${tag}.qcow2"
  if [ -f "$img" ]; then echo "$img"; return; fi
  if [ -n "${FCOS_VERSION:-}" ]; then
    log "resolving pinned Fedora CoreOS ${FCOS_STREAM} build ${FCOS_VERSION} qcow2"
    url="https://builds.coreos.fedoraproject.org/prod/streams/${FCOS_STREAM}/builds/${FCOS_VERSION}/x86_64/fedora-coreos-${FCOS_VERSION}-qemu.x86_64.qcow2.xz"
  else
    log "resolving Fedora CoreOS ${FCOS_STREAM} qcow2 (latest)"
    meta="$(curl -fsSL "https://builds.coreos.fedoraproject.org/streams/${FCOS_STREAM}.json")"
    url="$(echo "$meta" | jq -r '.architectures.x86_64.artifacts.qemu.formats["qcow2.xz"].disk.location')"
  fi
  [ -n "$url" ] && [ "$url" != "null" ] || die "could not resolve FCoS qcow2 url"
  xz="$CACHE_DIR/$(basename "$url")"
  log "downloading $url"; curl -fsSL "$url" -o "$xz" || die "FCoS download failed (pinned FCOS_VERSION=${FCOS_VERSION:-<none>} may not exist for stream ${FCOS_STREAM})"
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
  create_overlay "$name"
}

# create_overlay <name> — fresh qcow2 overlay on the cached base, with first-boot
# kernel args injected when INJECT_KARGS=1 (nested-virt workaround).
create_overlay() {
  local name="$1" base; base="$(download_fcos)"
  rm -f "$WORKDIR/${name}.qcow2"
  qemu-img create -f qcow2 -b "$base" -F qcow2 "$WORKDIR/${name}.qcow2" 25G >/dev/null
  [ "$INJECT_KARGS" = 1 ] && inject_firstboot_kargs "$WORKDIR/${name}.qcow2"
  return 0
}

# recreate_overlay <name> — fresh overlay (kargs re-injected) + clean console.
recreate_overlay() {
  local name="$1"
  : > "$WORKDIR/console.${name}.log"
  create_overlay "$name"
}

# inject_firstboot_kargs <overlay.qcow2> — append FIRSTBOOT_KARGS to the image's
# BLS boot entry so the FIRST boot's initramfs already carries them (Ignition's
# kernel_arguments only apply *after* firstboot, but the nested-virt freeze is in
# the initramfs *before* Ignition). Writes land in the overlay (copy-on-write), so
# the cached base stays clean and every retry re-injects. Best-effort: needs
# passwordless sudo + nbd; on any failure it warns and the VM boots unmodified
# (the retry loop and butane kargs still apply).
inject_firstboot_kargs() {
  local img="$1" nbd="" mnt part done=0 n
  if ! sudo -n true 2>/dev/null; then
    log "inject-kargs: no passwordless sudo — booting $(basename "$img") unmodified"; return 0
  fi
  sudo modprobe nbd max_part=8 2>/dev/null || { log "inject-kargs: modprobe nbd failed — skipping"; return 0; }
  for n in 0 1 2 3 4 5 6 7; do [ -e "/sys/block/nbd$n/pid" ] || { nbd="/dev/nbd$n"; break; }; done
  [ -n "$nbd" ] || { log "inject-kargs: no free nbd device — skipping"; return 0; }
  sudo qemu-nbd --connect="$nbd" -f qcow2 "$img" 2>/dev/null || { log "inject-kargs: qemu-nbd connect failed — skipping"; return 0; }
  sudo partprobe "$nbd" 2>/dev/null || true; sleep 1
  mnt="$(mktemp -d)"
  # FCoS x86_64 layout: p3 = ext4 'boot' (BLS entries live here); try it first.
  for part in "${nbd}p3" "${nbd}p2" "${nbd}p4" "${nbd}p1"; do
    [ -e "$part" ] || continue
    sudo mount "$part" "$mnt" 2>/dev/null || continue
    if sudo test -d "$mnt/loader/entries"; then
      sudo bash -c "shopt -s nullglob; for f in '$mnt'/loader/entries/*.conf; do sed -i 's#^\\(options .*\\)#\\1 $FIRSTBOOT_KARGS#' \"\$f\"; done"
      log "inject-kargs: added '$FIRSTBOOT_KARGS' to BLS entries on $(basename "$part") of $(basename "$img")"
      done=1
    fi
    sudo umount "$mnt" 2>/dev/null || true
    [ "$done" = 1 ] && break
  done
  rmdir "$mnt" 2>/dev/null || true
  sudo qemu-nbd --disconnect "$nbd" >/dev/null 2>&1 || true
  [ "$done" = 1 ] || log "inject-kargs: could not locate the BLS boot entry — booting $(basename "$img") unmodified"
  return 0
}

# boot_progress <console-log> — how far the boot got, so a failed SSH wait can be
# classified as a real hang vs an alive-but-unreachable guest (network/Ignition).
boot_progress() {
  local clog="$1"
  if grep -qiE 'multi-user\.target|Startup finished|[a-z0-9-]+ login:|Server listening on .* port 22|sshd\[' "$clog" 2>/dev/null; then
    echo reached-multiuser
  elif grep -qiE 'Reached target .*[Bb]asic|switch.?root|Ignition|systemd\[1\]: Started' "$clog" 2>/dev/null; then
    echo in-initramfs
  else
    echo early-boot
  fi
}

# boot_node <name> <ssh_port> <mem_mb> <cpus> <cluster_mac|"">
# Boots with bounded retries. A nested-virt boot can freeze silently at a random
# early-init point; the next boot usually succeeds, so a stalled VM is killed and
# retried on a clean overlay rather than failing the whole run.
boot_node() {
  local name="$1" port="$2" mem="$3" cpus="$4" cmac="$5"
  local total=$((BOOT_RETRIES + 1)) n pid
  for n in $(seq 1 "$total"); do
    if [ "$n" -gt 1 ]; then
      log "$name boot attempt $n/$total — previous boot hung; recreating overlay"
      recreate_overlay "$name"
    fi
    if boot_node_once "$name" "$port" "$mem" "$cpus" "$cmac" "$n" "$total"; then
      return 0
    fi
    pid="${QEMU_PIDS[$name]:-}"
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true; sleep 2; kill -9 "$pid" 2>/dev/null || true
      unset 'QEMU_PIDS[$name]'
    fi
  done
  # KVM exhausted. If the failures were early-boot HANGS (not an alive-but-
  # unreachable guest, which TCG can't fix), fall back to pure-emulation TCG: it
  # has no nested interrupt virtualization, so it boots where KVM's lost device/
  # timer IRQs freeze the L2 guest. Much slower; good hosts never reach here (KVM
  # wins on attempt 1). Globals are updated so workers boot the same way.
  if [ "$QEMU_ACCEL" = kvm ] && [ "${TCG_FALLBACK:-1}" = 1 ] \
     && [ "$(boot_progress "$WORKDIR/console.${name}.log")" != reached-multiuser ]; then
    log "$name: KVM hung in early boot $total attempt(s) — falling back to TCG (software emulation; reliable but slow). Disable with TCG_FALLBACK=0."
    QEMU_ACCEL=tcg; QEMU_CPU=qemu64; QEMU_IRQCHIP=""
    SSH_WAIT_TRIES=$(( SSH_WAIT_TRIES > 360 ? SSH_WAIT_TRIES : 360 ))   # TCG boots slowly
    BOOT_STALL_SECS=$(( BOOT_STALL_SECS > 300 ? BOOT_STALL_SECS : 300 ))
    recreate_overlay "$name"
    if boot_node_once "$name" "$port" "$mem" "$cpus" "$cmac" 1 1; then return 0; fi
    pid="${QEMU_PIDS[$name]:-}"
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true; sleep 2; kill -9 "$pid" 2>/dev/null || true
      unset 'QEMU_PIDS[$name]'
    fi
  fi
  err "$name failed to boot after $total KVM attempt(s)$([ "$QEMU_ACCEL" = tcg ] && echo ' + TCG fallback') — last 80 console lines:"
  tail -n 80 "$WORKDIR/console.${name}.log" 2>/dev/null | sed 's/^/    | /' >&2 || true
  die "$name SSH never came up. If even the TCG fallback failed, use a runner with sound nested virt (Zen2+/Intel or bare-metal/--device /dev/kvm). console: $WORKDIR/console.${name}.log"
}

# boot_node_once <name> <port> <mem> <cpus> <cmac> <attempt> <total>
# Boots one VM and waits for SSH. Returns 0 when SSH is up; returns 1 if the boot
# crashes, exits, or stalls (console idle BOOT_STALL_SECS with no SSH) so the
# caller can retry on a fresh overlay.
boot_node_once() {
  local name="$1" port="$2" mem="$3" cpus="$4" cmac="$5" n="$6" total="$7"
  local net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${port}-:22" -device virtio-net-pci,netdev=n0)
  if [ -n "$cmac" ]; then
    # Second NIC on a shared QEMU mcast segment = the cluster network.
    net+=(-netdev "socket,id=n1,mcast=${MCAST}" -device "virtio-net-pci,netdev=n1,mac=${cmac}")
  fi
  # kernel-irqchip=off (userspace APIC) is invalid under KVM on x86 — QEMU exits
  # immediately with "KVM does not support userspace APIC". Coerce off->split for
  # KVM here so no code path (auto or explicit) can feed KVM an impossible config.
  local irq="$QEMU_IRQCHIP"
  if [ "$QEMU_ACCEL" = kvm ] && [ "$irq" = off ]; then
    irq=split
    log "$name: kernel-irqchip=off is rejected by KVM (userspace APIC) — using split"
  fi
  log "booting $name (attempt $n/$total mem=${mem}MB cpus=${cpus} machine=${QEMU_MACHINE:-default} irqchip=${irq:-default} accel=${QEMU_ACCEL} cpu=${QEMU_CPU} ssh=:$port${cmac:+ cluster-mac=$cmac})"
  qemu-system-x86_64 \
    -machine "${QEMU_MACHINE:+${QEMU_MACHINE},}accel=${QEMU_ACCEL}${irq:+,kernel-irqchip=${irq}}" -cpu "$QEMU_CPU" -smp "$cpus" -m "$mem" \
    -nographic -serial file:"$WORKDIR/console.${name}.log" -monitor none \
    -drive if=virtio,file="$WORKDIR/${name}.qcow2",format=qcow2 \
    -fw_cfg name=opt/com.coreos/config,file="$WORKDIR/ignition.${name}.json" \
    "${net[@]}" &
  QEMU_PIDS[$name]=$!
  local clog="$WORKDIR/console.${name}.log" waited=0 last_size=-1 last_change
  last_change="$(date +%s)"
  log "$name qemu pid ${QEMU_PIDS[$name]} — waiting for SSH on 127.0.0.1:$port (console: $clog)"
  for attempt in $(seq 1 "$SSH_WAIT_TRIES"); do
    if gssh "$port" true 2>/dev/null; then log "$name SSH up after ${waited}s (attempt $n/$total)"; return 0; fi
    if ! kill -0 "${QEMU_PIDS[$name]}" 2>/dev/null; then
      err "$name qemu exited early after ${waited}s (attempt $n/$total) — last 40 console lines:"
      tail -n 40 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
      return 1
    fi
    # A fatal guest condition (panic, GPF, or — common under nested KVM — an RCU
    # stall / soft lockup / hung task during early init) never recovers.
    if grep -qiE 'Kernel panic|BUG:|Oops:|general protection fault|detected stall|soft lockup|hung task|Unable to mount root|not syncing' "$clog" 2>/dev/null; then
      err "$name fatal guest condition during boot after ${waited}s (attempt $n/$total) — last 150 console lines:"
      tail -n 150 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
      return 1
    fi
    # Silent freeze: a nested-virt hang emits no panic — the console simply stops
    # growing. If it hasn't advanced in BOOT_STALL_SECS and SSH is still down,
    # treat the boot as hung so boot_node retries on a fresh overlay. Steady (even
    # slow, e.g. TCG) output keeps resetting last_change, so this never false-fires.
    local now sz; now="$(date +%s)"; sz="$(stat -c %s "$clog" 2>/dev/null || echo 0)"
    if [ "$sz" != "$last_size" ]; then last_size="$sz"; last_change="$now"; fi
    if [ $((now - last_change)) -ge "$BOOT_STALL_SECS" ]; then
      if [ "$(boot_progress "$clog")" = reached-multiuser ]; then
        err "$name console idle ${BOOT_STALL_SECS}s but the guest REACHED MULTI-USER (attempt $n/$total) — this is NOT a boot hang: SSH is unreachable. Check user-mode net/hostfwd (DHCP on :2222) or Ignition ssh-key/sshd config. Last 80 console lines:"
      else
        err "$name console idle ${BOOT_STALL_SECS}s, still in early boot (silent nested-virt boot hang) after ${waited}s (attempt $n/$total) — last 80 console lines:"
      fi
      tail -n 80 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
      return 1
    fi
    # Heartbeat every ~30s with a console tail so a stuck boot/Ignition is visible.
    if [ $((attempt % 6)) -eq 0 ]; then
      log "$name still waiting for SSH (${waited}s elapsed, console ${sz}B, idle $((now - last_change))s) — console tail:"
      tail -n 8 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || echo "    | (console log empty — qemu may not have started serial output yet)" >&2
    fi
    sleep 5; waited=$((waited + 5))
  done
  if [ "$(boot_progress "$clog")" = reached-multiuser ]; then
    err "$name SSH did not come up after ${waited}s but the guest REACHED MULTI-USER (attempt $n/$total) — NOT a boot hang: SSH unreachable. Check user-mode net/hostfwd (DHCP on :2222) or Ignition ssh-key/sshd config. Last 60 console lines:"
  else
    err "$name SSH did not come up after ${waited}s, still in early boot (likely a boot hang) (attempt $n/$total) — last 60 console lines:"
  fi
  tail -n 60 "$clog" 2>/dev/null | sed 's/^/    | /' >&2 || true
  return 1
}

SCRIPTS_DIR() { echo "$VICTORIA_DIR/magnum/drivers/common/templates/kubernetes/bootstrap"; }

# provision_node <name> <ssh_port> <start_mock 0|1>
provision_node() {
  local name="$1" port="$2" start_mock="$3"
  log "provisioning $name (trigger=$TRIGGER)"
  gssh "$port" "mkdir -p $GUEST_E2E_DIR /opt/victoria-bootstrap"
  local files=("$WORKDIR/bootstrap" "$WORKDIR/mock-magnum" "$WORKDIR/ca.crt" "$WORKDIR/ca.key" "$REPO_ROOT/e2e/vm/guest-run.sh")
  [ "$TRIGGER" = agent ] && files+=("$WORKDIR/mock-heat")
  gscp "$port" "${files[@]}" root@127.0.0.1:"$GUEST_E2E_DIR/" >/dev/null
  gssh "$port" "chmod +x $GUEST_E2E_DIR/guest-run.sh"
  gssh "$port" "START_MOCK=${start_mock} MOCK_LISTEN=$(mock_host):9511 WAIT_SCALE=${WAIT_SCALE:-1} $GUEST_E2E_DIR/guest-run.sh setup"
  if [ "$TRIGGER" = agent ]; then
    # The real bootstrap scripts are delivered inside the deployment metadata,
    # not scp'd; the agent installs the launcher/units itself when it runs them.
    gssh "$port" "HEAT_LISTEN='$HEAT_LISTEN' AGENT_IMAGE='$AGENT_IMAGE' AGENT_STATE_DIR='$GUEST_E2E_DIR/heat-state' $GUEST_E2E_DIR/guest-run.sh agent-setup self"
  else
    gscp "$port" "$(SCRIPTS_DIR)"/*.sh root@127.0.0.1:/opt/victoria-bootstrap/ >/dev/null
    gssh "$port" "$GUEST_E2E_DIR/guest-run.sh install /opt/victoria-bootstrap"
  fi
}

# node_index <key> -> numeric index for scenario-gen (-node-index).
node_index() {
  case "$1" in
    master)  echo 0 ;;
    master*) echo "${1#master}" ;;
    *)       echo "${1#worker}" ;;
  esac
}

# scenario_extra_flags <role> <array-name> — fill the array with the master-only
# scenario-gen flags carrying the cluster's master count and (when the LB is on)
# the two LB VIPs. Empty for workers.
scenario_extra_flags() {
  local role="$1"; local -n _out="$2"; _out=()
  if [ "$role" = master ]; then
    _out+=(-number-of-masters "${MASTERS:-1}")
    if lb_enabled; then _out+=(-api-ip "$API_VIP" -etcd-lb-vip "$ETCD_VIP"); fi
  fi
}

# render_and_push <node-key> <role> <op> <tag> <rot> <node-ip> [master-ip]
render_and_push() {
  local key="$1" role="$2" op="$3" tag="$4" rot="$5" nip="$6" mip="${7:-}"
  local idx; idx="$(node_index "$key")"
  local host="$(mock_host)" port; port="$(ssh_port "$key")"
  local hp="$WORKDIR/heat-params.${key}.${op}"
  local extra; scenario_extra_flags "$role" extra
  "$WORKDIR/scenario-gen" \
    -role "$role" -op "$op" -node-index "$idx" -node-ip "$nip" ${mip:+-master-ip "$mip"} \
    ${extra[@]+"${extra[@]}"} \
    -kube-tag "$tag" -ca-rotation-id "$rot" \
    -ca-key-file "$WORKDIR/ca.key" -sa-key-file "$WORKDIR/sa.pub" -sa-priv-file "$WORKDIR/sa.key" \
    -auth-url "http://${host}:9511/v3" -magnum-url "http://${host}:9511/v1" \
    -reconciler-version e2e -reconciler-binary-url "file://$GUEST_E2E_DIR/bootstrap" \
    -o "$hp"
  gscp "$port" "$hp" root@127.0.0.1:"$GUEST_E2E_DIR/heat-params.${op}" >/dev/null
  echo "$GUEST_E2E_DIR/heat-params.${op}"
}

# render_and_push_deploy <node-key> <role> <op> <tag> <rot> <node-ip> [master-ip]
# Generates the Heat SoftwareDeployment metadata (real bootstrap scripts as the
# config + inputs) and scp's it in. Echoes "<remote-metadata-path> <deploy-id>".
# A fresh id per call is required: 55-heat-config skips an already-deployed id.
render_and_push_deploy() {
  local key="$1" role="$2" op="$3" tag="$4" rot="$5" nip="$6" mip="${7:-}"
  local idx; idx="$(node_index "$key")"
  local host; host="$(mock_host)"; local port; port="$(ssh_port "$key")"
  local id="${key}-${op}-$(date +%s)" action=UPDATE
  [ "$op" = create ] && action=CREATE
  local dep="$WORKDIR/deploy.${key}.${op}.json"
  local extra; scenario_extra_flags "$role" extra
  "$WORKDIR/scenario-gen" -emit deployment \
    -role "$role" -op "$op" -node-index "$idx" -node-ip "$nip" ${mip:+-master-ip "$mip"} \
    ${extra[@]+"${extra[@]}"} \
    -kube-tag "$tag" -ca-rotation-id "$rot" \
    -ca-key-file "$WORKDIR/ca.key" -sa-key-file "$WORKDIR/sa.pub" -sa-priv-file "$WORKDIR/sa.key" \
    -auth-url "http://${host}:9511/v3" -magnum-url "http://${host}:9511/v1" \
    -reconciler-version e2e -reconciler-binary-url "file://$GUEST_E2E_DIR/bootstrap" \
    -scripts-dir "$(SCRIPTS_DIR)" -signal-id "http://${HEAT_LISTEN}/signal/${id}" \
    -deploy-id "$id" -deploy-action "$action" \
    -o "$dep"
  gscp "$port" "$dep" root@127.0.0.1:"$GUEST_E2E_DIR/deploy.${op}.json" >/dev/null
  echo "$GUEST_E2E_DIR/deploy.${op}.json $id"
}

# --- scenarios -------------------------------------------------------------
for_each_worker() {  # for_each_worker <fn>; calls fn <i>
  local fn="$1" i=0
  while [ "$i" -lt "$WORKERS" ]; do "$fn" "$i"; i=$((i + 1)); done
}

# apply_master_idx <i> <op> <tag> [rot] — render + apply heat-params on master i.
apply_master_idx() {
  local i="$1" op="$2" tag="$3" rot="${4:-}"
  local key; key="$(master_key "$i")"; local mp; mp="$(ssh_port "$key")"
  local nip; nip="$(master_nodeip "$i")"
  if [ "$TRIGGER" = agent ]; then
    local res; res="$(render_and_push_deploy "$key" master "$op" "$tag" "$rot" "$nip")"
    gssh "$mp" "AGENT_STATE_DIR='$GUEST_E2E_DIR/heat-state' $GUEST_E2E_DIR/guest-run.sh heat-deploy ${res% *} ${res##* } $op"
  else
    local hp; hp="$(render_and_push "$key" master "$op" "$tag" "$rot" "$nip")"
    gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh apply $hp $op"
  fi
}
# apply_master <op> <tag> [rot] — master-0 (back-compat for single-master scenarios).
apply_master() { apply_master_idx 0 "$@"; }

apply_worker() {  # apply_worker <i> <op> <tag>
  local i="$1" op="$2" tag="$3" wp; wp="$(ssh_port "worker$i")"
  if [ "$TRIGGER" = agent ]; then
    local res; res="$(render_and_push_deploy "worker$i" worker "$op" "$tag" "" "$(worker_ip "$i")" "$(api_endpoint)")"
    gssh "$wp" "AGENT_STATE_DIR='$GUEST_E2E_DIR/heat-state' $GUEST_E2E_DIR/guest-run.sh heat-deploy ${res% *} ${res##* } $op"
  else
    local hp; hp="$(render_and_push "worker$i" worker "$op" "$tag" "" "$(worker_ip "$i")" "$(api_endpoint)")"
    gssh "$wp" "$GUEST_E2E_DIR/guest-run.sh apply $hp $op"
  fi
}

# start_lbs — bring up the two control-plane LBs (api_lb + etcd_lb) on the lb node,
# each forwarding to every master. Started before any master reconcile; mock-lb's
# health loop registers each backend as its apiserver/etcd binds. Idempotent.
start_lbs() {
  [ "$LBS_STARTED" = 1 ] && return 0
  lb_enabled || return 0
  local cips="" i=0
  while [ "$i" -lt "${MASTERS:-1}" ]; do cips="${cips:+$cips,}$(master_cip "$i")"; i=$((i + 1)); done
  log "starting control-plane LBs on lb node (api ${API_VIP}:6443, etcd ${ETCD_VIP}:2379 -> ${cips})"
  gssh "$(ssh_port lb)" "$GUEST_E2E_DIR/guest-run.sh lb-start $API_VIP $ETCD_VIP $CNET $cips"
  LBS_STARTED=1
}

# provision_lb — minimal provisioning for the lb node: just mock-lb + guest-run.sh
# (no mock Magnum, no reconciler).
provision_lb() {
  local port; port="$(ssh_port lb)"
  log "provisioning lb node"
  gssh "$port" "mkdir -p $GUEST_E2E_DIR"
  gscp "$port" "$WORKDIR/mock-lb" "$REPO_ROOT/e2e/vm/guest-run.sh" root@127.0.0.1:"$GUEST_E2E_DIR/" >/dev/null
  gssh "$port" "chmod +x $GUEST_E2E_DIR/guest-run.sh $GUEST_E2E_DIR/mock-lb; echo ${WAIT_SCALE:-1} > $GUEST_E2E_DIR/wait-scale"
}

assert_worker_joined() {  # <i>
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-node-ready e2e-minion-$1"
}

scenario_create() {
  log "=== SCENARIO: create (trigger=$TRIGGER, masters=${MASTERS:-1}) ==="
  apply_master create "$KUBE_TAG"
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  if multimaster; then
    # master-0 is up and its etcd is now reachable through the etcd VIP; bring up
    # the remaining masters one at a time (each joins the existing etcd via the LB,
    # exactly as Heat rolls out the master batch).
    local i=1
    while [ "$i" -lt "${MASTERS:-1}" ]; do
      log "--- joining master-$i ---"
      apply_master_idx "$i" create "$KUBE_TAG"
      gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-node-ready e2e-master-$i"
      i=$((i + 1))
    done
    gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-etcd-members ${MASTERS} https://$(master_cip 0):2379"
  fi
  # Idempotency re-run only in direct mode: re-applying the same heat-params must
  # be a no-op. Under the agent, Heat re-runs only on a NEW deployment id, so a
  # same-id replay is skipped by 55-heat-config by design — not a meaningful test.
  if [ "$TRIGGER" != agent ]; then
    gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh assert-noop $GUEST_E2E_DIR/heat-params.create create-idempotency"
  fi
  for_each_worker _create_worker
}
_create_worker() { apply_worker "$1" create "$KUBE_TAG"; assert_worker_joined "$1"; }

# scenario_ca_rotate — drive one CA rotation and assert it did real work, not
# just exit 0: the API server leaf cert content must change (the reconciler
# re-keys + re-signs every leaf). Repeatable: each call gets a unique rotation
# id, so SCENARIOS="create ca-rotate ca-rotate ca-rotate upgrade ca-rotate"
# reproduces the repeated-operation sequence that exposed the wedge bug.
# NOTE: the mock serves a single CA, so ca.crt content may be unchanged across a
# rotation (the new CA == old CA); the leaf re-key is the definitive signal. A
# true CA-material change is the real-OpenStack tier's job (IMPROVEMENTS.md P2).
scenario_ca_rotate() {
  ROT_SEQ=$((ROT_SEQ + 1))
  local rot="rot-$(date +%s)-${ROT_SEQ}" mp before after sb sa
  mp="$(ssh_port master)"
  log "=== SCENARIO: ca-rotate (id=$rot) ==="
  before="$(gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh cert-hashes")"
  if multimaster; then
    # Apply the rotation to every master ~concurrently — Heat rotates the whole
    # master batch at once, and the dual-CA barrier's per-master restart Lease is
    # only exercised under concurrent applies (sequential would block at barrier 1).
    local pids=() i=0 rc=0 p
    while [ "$i" -lt "${MASTERS:-1}" ]; do
      apply_master_idx "$i" ca-rotate "$KUBE_TAG" "$rot" & pids+=($!)
      i=$((i + 1))
    done
    for p in "${pids[@]}"; do wait "$p" || rc=1; done
    [ "$rc" = 0 ] || die "ca-rotate: a master reconcile failed (see per-master output above)"
  else
    apply_master ca-rotate "$KUBE_TAG" "$rot"
  fi
  gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  after="$(gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh cert-hashes")"
  sb="${before##*server=}"; sa="${after##*server=}"
  log "ca-rotate cert fingerprints: before[$before] after[$after]"
  { [ "$sb" != "$sa" ] && [ "$sa" != none ]; } \
    || die "ca-rotate did not replace the API server leaf cert (server hash unchanged: ${sa:-none})"
  log "ca-rotate replaced the API server leaf cert ✅"
}

scenario_upgrade() {
  log "=== SCENARIO: upgrade -> $KUBE_TAG_UPGRADE ==="
  local mp before after; mp="$(ssh_port master)"
  before="$(gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh kubelet-version")"
  if multimaster; then
    # Rolling upgrade: one master at a time (each cordons/drains itself), like
    # Heat's per-batch master rollout.
    local i=0
    while [ "$i" -lt "${MASTERS:-1}" ]; do
      log "--- upgrading master-$i ---"
      apply_master_idx "$i" upgrade "$KUBE_TAG_UPGRADE"
      gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh assert-node-ready e2e-master-$i"
      i=$((i + 1))
    done
  else
    apply_master upgrade "$KUBE_TAG_UPGRADE"
    gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  fi
  after="$(gssh "$mp" "$GUEST_E2E_DIR/guest-run.sh kubelet-version e2e-master-0")"
  log "upgrade kubelet version: before[$before] after[$after] (want ~$KUBE_TAG_UPGRADE)"
  case "$after" in
    *"${KUBE_TAG_UPGRADE#v}"*) log "kubelet upgraded to $after ✅" ;;
    *) die "upgrade did not take effect (kubelet before=$before after=$after, want $KUBE_TAG_UPGRADE)" ;;
  esac
  for_each_worker _upgrade_worker
}
_upgrade_worker() { apply_worker "$1" upgrade "$KUBE_TAG_UPGRADE"; assert_worker_joined "$1"; }

# --- main ------------------------------------------------------------------
boot_all() {
  if clustered; then
    # lb node first (small; mock-lb tolerates down backends and registers each
    # master as its apiserver/etcd comes up).
    if lb_enabled; then
      render_ignition lb e2e-lb "$LB_CMAC" "$LB_CIP"
      boot_node lb "$(ssh_port lb)" "$LB_MEM_MB" "$LB_CPUS" "$LB_CMAC"
    fi
    local i=0 key
    while [ "$i" -lt "${MASTERS:-1}" ]; do
      key="$(master_key "$i")"
      render_ignition "$key" "e2e-master-$i" "$(master_cmac "$i")" "$(master_cip "$i")"
      boot_node "$key" "$(ssh_port "$key")" "$MASTER_MEM_MB" "$MASTER_CPUS" "$(master_cmac "$i")"
      i=$((i + 1))
    done
    i=0
    while [ "$i" -lt "$WORKERS" ]; do
      render_ignition "worker$i" "e2e-minion-$i" "$(worker_cmac "$i")" "$(worker_ip "$i")"
      boot_node "worker$i" "$(ssh_port "worker$i")" "$WORKER_MEM_MB" "$WORKER_CPUS" "$(worker_cmac "$i")"
      i=$((i + 1))
    done
  else
    # MASTER_LB_ENABLED=false single master: user-mode path, no cluster NIC.
    render_ignition master e2e-master-0 "$MASTER_CMAC" "$MASTER_CIP"
    boot_node master "$(ssh_port master)" "$MASTER_MEM_MB" "$MASTER_CPUS" ""
  fi
}

provision_all() {
  provision_node master "$(ssh_port master)" 1
  if clustered; then
    if lb_enabled; then provision_lb; fi
    local i=1 key
    while [ "$i" -lt "${MASTERS:-1}" ]; do
      key="$(master_key "$i")"; provision_node "$key" "$(ssh_port "$key")" 0; i=$((i + 1))
    done
    i=0
    while [ "$i" -lt "$WORKERS" ]; do provision_node "worker$i" "$(ssh_port "worker$i")" 0; i=$((i + 1)); done
  fi
  # Start the LBs once the nodes are provisioned; backends register as each master
  # reconciles. (master-0 bootstraps its etcd before the LB has a healthy backend.)
  start_lbs
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
  # Show the final cluster state (nodes + pods + helm releases) so a passing run
  # makes visible what came up, not just that the asserts passed.
  log "=== final cluster state ==="
  gssh "$(ssh_port master)" "$GUEST_E2E_DIR/guest-run.sh dump-state" || true
  log "ALL SCENARIOS PASSED ✅"
}

main "$@"
