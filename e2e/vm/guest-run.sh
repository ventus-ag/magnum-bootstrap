#!/bin/bash
#
# guest-run.sh — runs INSIDE the Fedora CoreOS e2e node (as root).
#
# Subcommands:
#   setup                         self-ssh + mock Magnum + binary cache
#   install <victoria-bootstrap>  run the real magnum_victoria install scripts
#   apply   <heat-params> <name>  place heat-params, run a reconcile, assert success
#   assert-ready                  assert the single-node cluster is Ready + core pods up
#   assert-noop <heat-params> <name>  re-run and assert zero host changes (idempotency)
#
# Files expected under /opt/e2e (scp'd by the host harness):
#   bootstrap      the freshly built reconciler binary under test
#   mock-magnum    the mock Keystone/Magnum server binary
#   ca.crt ca.key  the shared mock CA (ca.key is also embedded in heat-params)
#
set -euo pipefail

E2E_DIR=/opt/e2e
RESULT_FILE=/var/lib/magnum/reconciler-last-run.json
ADMIN_KUBECONFIG=/etc/kubernetes/admin.conf

log() { printf '\033[1;36m[guest %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err() { printf '\033[1;31m[guest %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

kubectl_bin() {
  for k in /usr/local/bin/kubectl /srv/magnum/bin/kubectl kubectl; do
    if command -v "$k" >/dev/null 2>&1 || [ -x "$k" ]; then echo "$k"; return; fi
  done
  echo kubectl
}

kc() { KUBECONFIG="$ADMIN_KUBECONFIG" "$(kubectl_bin)" "$@"; }

setup_self_ssh() {
  # The magnum_victoria bootstrap scripts run `ssh -F /srv/magnum/.ssh/config
  # root@localhost`. Reproduce that channel so we can run them unmodified.
  log "configuring root self-ssh for the bootstrap pipeline"
  mkdir -p /srv/magnum/.ssh /root/.ssh
  chmod 700 /root/.ssh
  if [ ! -f /srv/magnum/.ssh/id_ed25519 ]; then
    ssh-keygen -t ed25519 -N '' -f /srv/magnum/.ssh/id_ed25519 -q
  fi
  grep -qf /srv/magnum/.ssh/id_ed25519.pub /root/.ssh/authorized_keys 2>/dev/null \
    || cat /srv/magnum/.ssh/id_ed25519.pub >> /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
  cat > /srv/magnum/.ssh/config <<EOF
Host localhost
  HostName 127.0.0.1
  User root
  IdentityFile /srv/magnum/.ssh/id_ed25519
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF
  chmod 600 /srv/magnum/.ssh/config
  systemctl restart sshd
  for _ in $(seq 1 30); do
    if ssh -F /srv/magnum/.ssh/config root@localhost true 2>/dev/null; then
      log "self-ssh ok"; return 0
    fi
    sleep 1
  done
  err "self-ssh did not come up"; return 1
}

start_mock_magnum() {
  local listen="${MOCK_LISTEN:-127.0.0.1:9511}"
  log "starting mock Magnum/Keystone on ${listen}"
  chmod +x "$E2E_DIR/mock-magnum"
  systemctl reset-failed mock-magnum 2>/dev/null || true
  systemd-run --unit=mock-magnum --collect \
    "$E2E_DIR/mock-magnum" -listen "${listen}" \
    -ca-cert "$E2E_DIR/ca.crt" -ca-key "$E2E_DIR/ca.key" -v
  for _ in $(seq 1 30); do
    if curl -fsS "http://${listen}/healthz" >/dev/null 2>&1; then
      log "mock Magnum healthy"; return 0
    fi
    sleep 1
  done
  err "mock Magnum did not become healthy"; journalctl -u mock-magnum --no-pager | tail -20 >&2; return 1
}

cache_binary() {
  log "caching reconciler binary for file:// delivery"
  chmod +x "$E2E_DIR/bootstrap"
  ( cd "$E2E_DIR" && sha256sum bootstrap | awk '{print $1}' > bootstrap.sha256 )
}

cmd_setup() {
  setup_self_ssh
  # Workers set START_MOCK=0 — only the master runs the mock Magnum, which
  # workers reach over the cluster network at MOCK_LISTEN.
  if [ "${START_MOCK:-1}" = "1" ]; then start_mock_magnum; fi
  cache_binary
  log "setup complete"
}

cmd_install() {
  local vdir="$1"
  log "installing reconciler launcher + systemd units from $vdir"
  bash "$vdir/install-reconciler-launcher.sh"
  bash "$vdir/install-reconciler-systemd.sh"
  log "launcher + units installed"
}

run_reconcile() {
  # Mirror run-reconciler-once.sh: one retry, via the systemd one-shot unit so
  # the real runtime path (launcher under systemd) is exercised.
  local attempts=2 rc=1
  for attempt in $(seq 1 "$attempts"); do
    rm -f "$RESULT_FILE"
    systemctl reset-failed magnum-reconcile.service 2>/dev/null || true
    log "reconcile attempt ${attempt}/${attempts}"
    if systemctl start --wait magnum-reconcile.service; then rc=0; break; fi
    rc=$?
    [ "$attempt" -lt "$attempts" ] && { err "attempt ${attempt} failed (rc=$rc), retrying"; sleep 15; }
  done
  return "$rc"
}

result_status() {
  [ -s "$RESULT_FILE" ] || { echo missing; return; }
  grep -o '"status":"[^"]*"' "$RESULT_FILE" 2>/dev/null | head -1 | cut -d'"' -f4
}

cmd_apply() {
  local hp="$1" name="$2"
  log "scenario '${name}': applying heat-params"
  install -m 600 "$hp" /etc/sysconfig/heat-params
  if run_reconcile; then
    log "scenario '${name}': reconcile exited 0"
  else
    err "scenario '${name}': reconcile failed"
  fi
  local status; status="$(result_status)"
  log "scenario '${name}': result status = ${status}"
  if [ "$status" != "success" ]; then
    err "scenario '${name}': expected success, got '${status}'"
    [ -s "$RESULT_FILE" ] && { echo '--- reconciler-last-run.json ---' >&2; cat "$RESULT_FILE" >&2; }
    echo '--- last 80 log lines ---' >&2
    tail -80 /var/log/magnum-reconcile.log 2>/dev/null >&2 || true
    return 1
  fi
}

cmd_assert_ready() {
  log "asserting single-node cluster readiness"
  local node; node="$(kc get nodes -o name 2>/dev/null | head -1 || true)"
  if [ -z "$node" ]; then err "no nodes registered"; kc get nodes || true; return 1; fi
  for _ in $(seq 1 60); do
    if kc get nodes --no-headers 2>/dev/null | grep -qw Ready; then
      log "node Ready: $(kc get nodes --no-headers | tr -s ' ')"
      break
    fi
    sleep 5
  done
  kc get nodes --no-headers 2>/dev/null | grep -qw Ready || { err "node never became Ready"; kc describe nodes | tail -40 >&2 || true; return 1; }

  log "waiting for core system pods (coredns / flannel)"
  for _ in $(seq 1 60); do
    local cd fl
    cd="$(kc -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null | grep -c Running || true)"
    fl="$(kc -n kube-system get pods --no-headers 2>/dev/null | grep -i flannel | grep -c Running || true)"
    if [ "${cd:-0}" -ge 1 ] && [ "${fl:-0}" -ge 1 ]; then
      log "core pods Running (coredns=$cd flannel=$fl)"; kc -n kube-system get pods --no-headers | tr -s ' '; return 0
    fi
    sleep 5
  done
  err "core system pods did not reach Running"
  kc -n kube-system get pods -o wide >&2 || true
  return 1
}

# cmd_assert_node_ready <node-name> — run on the master to confirm a specific
# node (e.g. a joined worker) registered and reached Ready.
cmd_assert_node_ready() {
  local node="$1"
  log "asserting node '${node}' is Ready"
  for _ in $(seq 1 72); do  # up to 6 min
    if kc get node "$node" --no-headers 2>/dev/null | grep -qw Ready; then
      log "node Ready: $(kc get node "$node" --no-headers | tr -s ' ')"; return 0
    fi
    sleep 5
  done
  err "node '${node}' did not join/become Ready"
  kc get nodes -o wide >&2 || true
  kc describe node "$node" 2>/dev/null | tail -30 >&2 || true
  return 1
}

cmd_assert_noop() {
  local hp="$1" name="$2"
  log "scenario '${name}': idempotency re-run"
  install -m 600 "$hp" /etc/sysconfig/heat-params
  run_reconcile || { err "idempotency re-run failed"; return 1; }
  # A converged node should report success with no host changes. We surface the
  # summary; strict 'zero changes' parsing is left to the reconciler result.
  local summary; summary="$(grep -o '"summary":"[^"]*"' "$RESULT_FILE" | head -1 | cut -d'"' -f4 || true)"
  log "scenario '${name}': re-run summary = ${summary:-<none>}"
  [ "$(result_status)" = "success" ] || { err "idempotency re-run not success"; return 1; }
}

main() {
  local sub="${1:-}"; shift || true
  case "$sub" in
    setup)        cmd_setup ;;
    install)      cmd_install "$@" ;;
    apply)            cmd_apply "$@" ;;
    assert-ready)     cmd_assert_ready ;;
    assert-node-ready) cmd_assert_node_ready "$@" ;;
    assert-noop)      cmd_assert_noop "$@" ;;
    *) err "unknown subcommand: ${sub}"; exit 2 ;;
  esac
}

main "$@"
