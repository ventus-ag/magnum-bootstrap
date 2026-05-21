#!/bin/bash
#
# run-magnum-e2e.sh — real-OpenStack end-to-end for the reconciler, driven
# through the Magnum (Container Infra) API with the `openstack` CLI.
#
# This is the only tier that exercises the OpenStack-integrated pieces the mock
# cannot fake: the cloud controller manager (LoadBalancer Services / Octavia)
# and Cinder CSI (dynamic PVCs). It walks a real cluster through:
#
#   create -> smoke (+OCCM LB +Cinder PVC) -> upgrade -> resize -> ca-rotate -> delete
#
# Auth: standard OpenStack env (OS_CLOUD or OS_AUTH_URL + app credential).
# Provide these as CI secrets. The Magnum service must already run the forked
# magnum_victoria driver; this test only drives its API.
#
# Required env:
#   CLUSTER_TEMPLATE      existing Magnum cluster template name or id
#   KEYPAIR               nova keypair name for the cluster
# Common env (defaults shown):
#   CLUSTER_NAME          recon-e2e-<run>          cluster name
#   KUBE_TAG              v1.30.5                  initial version (label kube_tag)
#   KUBE_TAG_UPGRADE      v1.31.4                  upgrade target
#   NODE_COUNT            1                        initial workers
#   NODE_COUNT_RESIZE     2                        resize target
#   MASTER_COUNT          1
#   RECONCILER_VERSION    (unset -> template default)   label override
#   RECONCILER_BINARY_URL (unset -> template default)   label override
#   EXTRA_LABELS          ""                       extra "k=v,k2=v2" cluster labels
#   TIMEOUT_MIN           60                       per-operation timeout
#   KEEP_CLUSTER          0                        "1" skips teardown
#   SKIP_CA_ROTATE        0                        "1" skips the ca-rotate step
#
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-recon-e2e-$(date +%Y%m%d-%H%M%S)}"
KUBE_TAG="${KUBE_TAG:-v1.30.5}"
KUBE_TAG_UPGRADE="${KUBE_TAG_UPGRADE:-v1.31.4}"
NODE_COUNT="${NODE_COUNT:-1}"
NODE_COUNT_RESIZE="${NODE_COUNT_RESIZE:-2}"
MASTER_COUNT="${MASTER_COUNT:-1}"
TIMEOUT_MIN="${TIMEOUT_MIN:-60}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
SKIP_CA_ROTATE="${SKIP_CA_ROTATE:-0}"
EXTRA_LABELS="${EXTRA_LABELS:-}"
WORKDIR="${WORKDIR:-$(mktemp -d /tmp/magnum-e2e.XXXXXX)}"
KUBECONFIG_FILE="$WORKDIR/kubeconfig"

log() { printf '\033[1;34m[magnum %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err() { printf '\033[1;31m[magnum %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { err "$*"; exit 1; }

require() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then err "failure (rc=$rc) — cluster state:"; openstack coe cluster show "$CLUSTER_NAME" 2>/dev/null || true; fi
  if [ "$KEEP_CLUSTER" = "1" ]; then
    log "KEEP_CLUSTER=1 — leaving $CLUSTER_NAME in place"
  else
    delete_cluster || true
  fi
  exit "$rc"
}
trap cleanup EXIT

preflight() {
  for t in openstack kubectl jq; do require "$t"; done
  [ -n "${CLUSTER_TEMPLATE:-}" ] || die "CLUSTER_TEMPLATE is required"
  [ -n "${KEYPAIR:-}" ] || die "KEYPAIR is required"
  [ -n "${OS_CLOUD:-}${OS_AUTH_URL:-}" ] || die "OpenStack auth env not set (OS_CLOUD or OS_AUTH_URL)"
  log "auth ok; verifying Magnum is reachable"
  openstack coe cluster template show "$CLUSTER_TEMPLATE" -f value -c name >/dev/null \
    || die "cluster template '$CLUSTER_TEMPLATE' not found"
}

# wait_status <expected-suffix e.g. CREATE_COMPLETE> — polls cluster status,
# failing fast on any *_FAILED.
wait_status() {
  local want="$1" deadline=$(( $(date +%s) + TIMEOUT_MIN*60 )) st
  log "waiting for status $want (timeout ${TIMEOUT_MIN}m)"
  while :; do
    st="$(openstack coe cluster show "$CLUSTER_NAME" -f value -c status 2>/dev/null || echo UNKNOWN)"
    case "$st" in
      "$want") log "reached $st"; return 0 ;;
      *_FAILED)
        err "cluster entered $st"
        openstack coe cluster show "$CLUSTER_NAME" -f value -c status_reason 2>/dev/null || true
        return 1 ;;
    esac
    [ "$(date +%s)" -lt "$deadline" ] || { err "timed out waiting for $want (last: $st)"; return 1; }
    sleep 20
  done
}

build_labels() {
  local labels="kube_tag=${KUBE_TAG}"
  [ -n "${RECONCILER_VERSION:-}" ]    && labels="${labels},reconciler_version=${RECONCILER_VERSION}"
  [ -n "${RECONCILER_BINARY_URL:-}" ] && labels="${labels},reconciler_binary_url=${RECONCILER_BINARY_URL}"
  [ -n "$EXTRA_LABELS" ]              && labels="${labels},${EXTRA_LABELS}"
  echo "$labels"
}

create_cluster() {
  local labels; labels="$(build_labels)"
  log "=== create cluster $CLUSTER_NAME (kube_tag=$KUBE_TAG, workers=$NODE_COUNT) ==="
  log "labels: $labels"
  openstack coe cluster create "$CLUSTER_NAME" \
    --cluster-template "$CLUSTER_TEMPLATE" \
    --keypair "$KEYPAIR" \
    --master-count "$MASTER_COUNT" \
    --node-count "$NODE_COUNT" \
    --labels "$labels" >/dev/null
  wait_status CREATE_COMPLETE
}

fetch_kubeconfig() {
  log "fetching kubeconfig"
  rm -f "$KUBECONFIG_FILE"
  openstack coe cluster config "$CLUSTER_NAME" --dir "$WORKDIR" --output-certs >/dev/null
  [ -f "$WORKDIR/config" ] && mv "$WORKDIR/config" "$KUBECONFIG_FILE"
  [ -f "$KUBECONFIG_FILE" ] || die "could not obtain kubeconfig"
}

kc() { KUBECONFIG="$KUBECONFIG_FILE" kubectl "$@"; }

smoke_core() {
  log "smoke: nodes Ready + system pods"
  local deadline=$(( $(date +%s) + 600 ))
  while :; do
    if ! kc get nodes >/dev/null 2>&1; then sleep 10
    elif [ "$(kc get nodes --no-headers | grep -cw Ready)" -ge 1 ] \
         && ! kc get nodes --no-headers | grep -qvw Ready; then
      log "all nodes Ready:"; kc get nodes; break
    fi
    [ "$(date +%s)" -lt "$deadline" ] || { err "nodes not all Ready"; kc get nodes || true; return 1; }
    sleep 10
  done
  kc -n kube-system get pods
}

smoke_cloud_integration() {
  # The payoff of the real-OpenStack tier: prove OCCM + Cinder CSI actually work.
  log "smoke: Cinder CSI dynamic PVC"
  kc apply -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: e2e-pvc, namespace: default }
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 1Gi } }
YAML
  if kc wait --for=jsonpath='{.status.phase}'=Bound pvc/e2e-pvc --timeout=300s; then
    log "Cinder CSI PVC bound ✅"
  else
    err "PVC did not bind — Cinder CSI/OCCM issue"; kc describe pvc e2e-pvc | tail -30 >&2; return 1
  fi

  log "smoke: OCCM LoadBalancer Service (Octavia)"
  kc create deployment e2e-web --image=registry.k8s.io/e2e-test-images/agnhost:2.47 -- /agnhost netexec --http-port=8080 >/dev/null 2>&1 || true
  kc expose deployment e2e-web --type=LoadBalancer --port=80 --target-port=8080 --name=e2e-lb >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + 600 )) ip=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ip="$(kc get svc e2e-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    [ -n "$ip" ] && break
    sleep 15
  done
  if [ -n "$ip" ]; then log "OCCM provisioned LoadBalancer IP $ip ✅"; else err "LoadBalancer never got an external IP"; kc describe svc e2e-lb | tail -30 >&2; return 1; fi

  log "cleaning up smoke workloads"
  kc delete svc e2e-lb deployment e2e-web pvc e2e-pvc --ignore-not-found >/dev/null 2>&1 || true
}

upgrade_cluster() {
  log "=== upgrade cluster -> $KUBE_TAG_UPGRADE ==="
  # Magnum upgrade requires a target template (or version). We assume the same
  # template is parameterised by the kube_tag label; adjust UPGRADE_TEMPLATE if
  # your deployment uses a distinct template per version.
  local target="${UPGRADE_TEMPLATE:-$CLUSTER_TEMPLATE}"
  openstack coe cluster upgrade "$CLUSTER_NAME" "$target" >/dev/null
  wait_status UPDATE_COMPLETE
  fetch_kubeconfig
  smoke_core
}

resize_cluster() {
  log "=== resize cluster workers -> $NODE_COUNT_RESIZE ==="
  openstack coe cluster resize "$CLUSTER_NAME" "$NODE_COUNT_RESIZE" >/dev/null
  wait_status UPDATE_COMPLETE
  fetch_kubeconfig
  local n; n="$(kc get nodes --no-headers | grep -c -v master || true)"
  log "worker nodes after resize: $n (target $NODE_COUNT_RESIZE)"
  smoke_core
}

ca_rotate_cluster() {
  log "=== ca-rotate cluster ==="
  if openstack coe ca rotate --cluster "$CLUSTER_NAME" 2>/dev/null; then
    wait_status UPDATE_COMPLETE
  else
    err "could not trigger 'openstack coe ca rotate' (check magnumclient version / API support)"
    return 1
  fi
  fetch_kubeconfig
  smoke_core
}

delete_cluster() {
  openstack coe cluster show "$CLUSTER_NAME" >/dev/null 2>&1 || return 0
  log "=== delete cluster $CLUSTER_NAME ==="
  openstack coe cluster delete "$CLUSTER_NAME" >/dev/null || true
  wait_status DELETE_COMPLETE || true
}

main() {
  preflight
  create_cluster
  fetch_kubeconfig
  smoke_core
  smoke_cloud_integration
  upgrade_cluster
  resize_cluster
  [ "$SKIP_CA_ROTATE" = "1" ] || ca_rotate_cluster
  log "ALL OPENSTACK SCENARIOS PASSED ✅"
}

main "$@"
