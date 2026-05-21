#!/bin/bash
#
# run-fcos-e2e.sh — host orchestrator for the Fedora CoreOS reconciler e2e.
#
# Boots a real Fedora CoreOS VM under QEMU/KVM, drives the actual magnum_victoria
# bootstrap pipeline (launcher + systemd units) against the freshly built
# reconciler, and walks the node through create -> ca-rotate -> upgrade,
# asserting cluster health and idempotency at each step. OpenStack-integrated
# addons (OCCM, Cinder/Manila CSI) are intentionally OFF here — they require a
# real cloud and are covered by run-magnum-e2e.sh.
#
# Requires on the runner: qemu-system-x86_64 (KVM), qemu-img, jq, xz, curl,
# podman (for butane), ssh/scp, go.
#
# Key env knobs (all have defaults):
#   KUBE_TAG            initial Kubernetes version           (default v1.30.5)
#   KUBE_TAG_UPGRADE    version for the upgrade scenario     (default v1.31.4)
#   FCOS_STREAM         Fedora CoreOS stream                 (default stable)
#   VICTORIA_DIR        path to magnum_victoria checkout     (required)
#   SCENARIOS           space list: create ca-rotate upgrade (default all three)
#   VM_MEM_MB / VM_CPUS VM sizing                            (default 6144 / 4)
#   WORKDIR             scratch dir                          (default mktemp)
#   KEEP_VM            "1" leaves the VM running for debugging
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

KUBE_TAG="${KUBE_TAG:-v1.30.5}"
KUBE_TAG_UPGRADE="${KUBE_TAG_UPGRADE:-v1.31.4}"
FCOS_STREAM="${FCOS_STREAM:-stable}"
VICTORIA_DIR="${VICTORIA_DIR:-}"
SCENARIOS="${SCENARIOS:-create ca-rotate upgrade}"
VM_MEM_MB="${VM_MEM_MB:-6144}"
VM_CPUS="${VM_CPUS:-4}"
SSH_PORT="${SSH_PORT:-2222}"
NODE_IP="10.0.2.15"                # fixed QEMU user-mode guest IP
GUEST_E2E_DIR=/opt/e2e
KEEP_VM="${KEEP_VM:-0}"

WORKDIR="${WORKDIR:-$(mktemp -d /tmp/fcos-e2e.XXXXXX)}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/fcos-e2e}"
QEMU_PID=""

log()  { printf '\033[1;32m[host %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err()  { printf '\033[1;31m[host %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die()  { err "$*"; exit 1; }

SSHOPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
         -o ConnectTimeout=10 -i "$WORKDIR/id_ed25519" -p "$SSH_PORT")
gssh() { ssh "${SSHOPTS[@]}" root@127.0.0.1 "$@"; }
gscp() { scp "${SSHOPTS[@]}" "$@"; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ] && [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
    err "failure (rc=$rc) — collecting diagnostics"
    gssh 'cat /var/lib/magnum/reconciler-last-run.json 2>/dev/null; echo; tail -120 /var/log/magnum-reconcile.log 2>/dev/null' 2>/dev/null || true
  fi
  if [ "$KEEP_VM" = "1" ]; then
    log "KEEP_VM=1 — leaving VM up (ssh -p $SSH_PORT -i $WORKDIR/id_ed25519 root@127.0.0.1)"
  else
    [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  fi
  exit "$rc"
}
trap cleanup EXIT

require() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

BUTANE=""   # resolved by preflight: native butane, or podman/docker container

resolve_butane() {
  if command -v butane >/dev/null 2>&1; then BUTANE="native"; return; fi
  if command -v podman >/dev/null 2>&1; then BUTANE="podman"; return; fi
  if command -v docker >/dev/null 2>&1; then BUTANE="docker"; return; fi
  die "need 'butane', or 'podman'/'docker' to render Ignition"
}

preflight() {
  for t in qemu-system-x86_64 qemu-img jq xz curl ssh scp go; do require "$t"; done
  resolve_butane
  log "butane renderer: $BUTANE"
  [ -e /dev/kvm ] || die "/dev/kvm not present — KVM acceleration is required"
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
  log "generating SSH key + shared mock CA + SA keypair"
  ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id_ed25519" -q
  "$WORKDIR/mock-magnum" -gen-ca -ca-cert "$WORKDIR/ca.crt" -ca-key "$WORKDIR/ca.key" >/dev/null
}

download_fcos() {
  local meta url xz img="$CACHE_DIR/fcos-${FCOS_STREAM}.qcow2"
  if [ -f "$img" ]; then log "using cached FCoS image $img"; echo "$img"; return; fi
  log "resolving Fedora CoreOS ${FCOS_STREAM} qcow2"
  meta="$(curl -fsSL "https://builds.coreos.fedoraproject.org/streams/${FCOS_STREAM}.json")"
  url="$(echo "$meta" | jq -r '.architectures.x86_64.artifacts.qemu.formats["qcow2.xz"].disk.location')"
  [ -n "$url" ] && [ "$url" != "null" ] || die "could not resolve FCoS qcow2 url"
  xz="$CACHE_DIR/$(basename "$url")"
  log "downloading $url"
  curl -fsSL "$url" -o "$xz"
  log "decompressing image"
  xz -dkc "$xz" > "$img.tmp" && mv "$img.tmp" "$img"
  echo "$img"
}

render_ignition() {
  local base="$1" pubkey
  pubkey="$(cat "$WORKDIR/id_ed25519.pub")"
  log "rendering Ignition from butane"
  sed "s|@@SSH_AUTHORIZED_KEY@@|${pubkey}|g" "$REPO_ROOT/e2e/vm/butane.yaml" > "$WORKDIR/butane.yaml"
  case "$BUTANE" in
    native) butane --pretty --strict < "$WORKDIR/butane.yaml" > "$WORKDIR/ignition.json" ;;
    podman) podman run --rm -i quay.io/coreos/butane:release --pretty --strict < "$WORKDIR/butane.yaml" > "$WORKDIR/ignition.json" ;;
    docker) docker run --rm -i quay.io/coreos/butane:release --pretty --strict < "$WORKDIR/butane.yaml" > "$WORKDIR/ignition.json" ;;
    *) die "no butane renderer resolved" ;;
  esac
  # Backed overlay so the cached base image stays pristine across runs.
  qemu-img create -f qcow2 -b "$base" -F qcow2 "$WORKDIR/node.qcow2" 25G >/dev/null
}

boot_vm() {
  log "booting FCoS VM (mem=${VM_MEM_MB}MB cpus=${VM_CPUS}, ssh on :$SSH_PORT)"
  qemu-system-x86_64 \
    -machine accel=kvm -cpu host -smp "$VM_CPUS" -m "$VM_MEM_MB" \
    -nographic -serial file:"$WORKDIR/console.log" -monitor none \
    -drive if=virtio,file="$WORKDIR/node.qcow2",format=qcow2 \
    -fw_cfg name=opt/com.coreos/config,file="$WORKDIR/ignition.json" \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:"$SSH_PORT"-:22 \
    -device virtio-net-pci,netdev=n0 &
  QEMU_PID=$!
  log "qemu pid $QEMU_PID — waiting for SSH"
  for _ in $(seq 1 120); do
    if gssh true 2>/dev/null; then log "SSH up"; return 0; fi
    kill -0 "$QEMU_PID" 2>/dev/null || die "qemu exited early (see $WORKDIR/console.log)"
    sleep 5
  done
  die "VM SSH did not come up (see $WORKDIR/console.log)"
}

provision_guest() {
  log "copying artifacts into the VM"
  gssh "mkdir -p $GUEST_E2E_DIR /opt/victoria-bootstrap"
  gscp "$WORKDIR/bootstrap" "$WORKDIR/mock-magnum" "$WORKDIR/ca.crt" "$WORKDIR/ca.key" \
       "$REPO_ROOT/e2e/vm/guest-run.sh" root@127.0.0.1:"$GUEST_E2E_DIR/" >/dev/null
  gscp "$VICTORIA_DIR"/magnum/drivers/common/templates/kubernetes/bootstrap/*.sh \
       root@127.0.0.1:/opt/victoria-bootstrap/ >/dev/null
  gssh "chmod +x $GUEST_E2E_DIR/guest-run.sh"
  log "running guest setup + bootstrap install"
  gssh "$GUEST_E2E_DIR/guest-run.sh setup"
  gssh "$GUEST_E2E_DIR/guest-run.sh install /opt/victoria-bootstrap"
}

# render_and_push <op> <kube-tag> <ca-rotation-id> -> remote heat-params path
render_and_push() {
  local op="$1" tag="$2" rot="${3:-}" name="${op}"
  local hp="$WORKDIR/heat-params.${name}"
  "$WORKDIR/scenario-gen" \
    -role master -op "$op" -node-ip "$NODE_IP" -kube-tag "$tag" \
    -ca-key-file "$WORKDIR/ca.key" \
    -sa-key-file "$WORKDIR/sa.pub" -sa-priv-file "$WORKDIR/sa.key" \
    -ca-rotation-id "$rot" \
    -reconciler-version e2e \
    -reconciler-binary-url "file://$GUEST_E2E_DIR/bootstrap" \
    -o "$hp"
  gscp "$hp" root@127.0.0.1:"$GUEST_E2E_DIR/heat-params.${name}" >/dev/null
  echo "$GUEST_E2E_DIR/heat-params.${name}"
}

scenario_create() {
  log "=== SCENARIO: create ==="
  local hp; hp="$(render_and_push create "$KUBE_TAG")"
  gssh "$GUEST_E2E_DIR/guest-run.sh apply $hp create"
  gssh "$GUEST_E2E_DIR/guest-run.sh assert-ready"
  gssh "$GUEST_E2E_DIR/guest-run.sh assert-noop $hp create-idempotency"
}

scenario_ca_rotate() {
  log "=== SCENARIO: ca-rotate ==="
  local hp; hp="$(render_and_push ca-rotate "$KUBE_TAG" "rot-$(date +%s)")"
  gssh "$GUEST_E2E_DIR/guest-run.sh apply $hp ca-rotate"
  gssh "$GUEST_E2E_DIR/guest-run.sh assert-ready"
}

scenario_upgrade() {
  log "=== SCENARIO: upgrade -> $KUBE_TAG_UPGRADE ==="
  local hp; hp="$(render_and_push upgrade "$KUBE_TAG_UPGRADE")"
  gssh "$GUEST_E2E_DIR/guest-run.sh apply $hp upgrade"
  gssh "$GUEST_E2E_DIR/guest-run.sh assert-ready"
}

main() {
  preflight
  build_binaries
  generate_keys
  local base; base="$(download_fcos)"
  render_ignition "$base"
  boot_vm
  provision_guest
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
