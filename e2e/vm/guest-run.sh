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

# "agent" trigger: run the real heat-container-agent against the local mock Heat.
HEAT_LISTEN="${HEAT_LISTEN:-127.0.0.1:9512}"
AGENT_IMAGE="${AGENT_IMAGE:-docker.io/openstackmagnum/heat-container-agent:victoria-stable-1}"
AGENT_STATE_DIR="${AGENT_STATE_DIR:-$E2E_DIR/heat-state}"
AGENT_NODE="${AGENT_NODE:-self}"
AGENT_DEPLOY_TIMEOUT="${AGENT_DEPLOY_TIMEOUT:-}"   # empty = auto (900s * wait-scale)

log() { printf '\033[1;36m[guest %s]\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*"; }
err() { printf '\033[1;31m[guest %s] ERROR:\033[0m %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# wscale — wait multiplier persisted by cmd_setup (3x under pure-emulation TCG,
# 1x under KVM). In-guest wait loops multiply their iteration count by it so a
# slow TCG run does not false-timeout. scaled <n> returns n*scale (min n).
wscale() { local s; s="$(cat "$E2E_DIR/wait-scale" 2>/dev/null || echo 1)"; case "$s" in ''|*[!0-9]*) echo 1 ;; *) echo "$s" ;; esac; }
scaled() { echo $(( $1 * $(wscale) )); }

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
  # Persist the wait multiplier so every later guest-run invocation (assert-*,
  # heat-deploy) scales its timeouts identically.
  echo "${WAIT_SCALE:-1}" > "$E2E_DIR/wait-scale"
  setup_self_ssh
  # Workers set START_MOCK=0 — only the master runs the mock Magnum, which
  # workers reach over the cluster network at MOCK_LISTEN.
  if [ "${START_MOCK:-1}" = "1" ]; then start_mock_magnum; fi
  cache_binary
  log "setup complete (wait-scale=$(wscale)x)"
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
    log "reconcile attempt ${attempt}/${attempts} — streaming /var/log/magnum-reconcile.log"
    # Stream the reconciler log live to our stdout (-> host harness -> CI) so the
    # whole run is visible in CI, not just a tail on failure. -F survives the
    # launcher creating/trimming the file; -n0 emits only new lines. Wrapped in a
    # subshell so we can kill the tail+sed children after the unit finishes.
    touch /var/log/magnum-reconcile.log 2>/dev/null || true
    ( stdbuf -oL tail -n0 -F /var/log/magnum-reconcile.log 2>/dev/null | sed -u 's/^/    | /' ) &
    local tail_pid=$!
    if systemctl start --wait magnum-reconcile.service; then rc=0; else rc=$?; fi
    sleep 1; pkill -P "$tail_pid" 2>/dev/null || true; kill "$tail_pid" 2>/dev/null || true
    [ "$rc" = 0 ] && break
    err "attempt ${attempt} failed (rc=$rc)"
    # journald captures what the log file may not (panics, stderr, the unit exit
    # code), so dump it too — this is the real cause when the unit fails to start.
    echo "    --- journalctl -u magnum-reconcile.service (last 40) ---" >&2
    journalctl -u magnum-reconcile.service -n 40 --no-pager 2>/dev/null | sed 's/^/    | /' >&2 || true
    [ "$attempt" -lt "$attempts" ] && { err "retrying in 15s"; sleep 15; }
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
  for _ in $(seq 1 "$(scaled 60)"); do
    if kc get nodes --no-headers 2>/dev/null | grep -qw Ready; then
      log "node Ready: $(kc get nodes --no-headers | tr -s ' ')"
      break
    fi
    sleep 5
  done
  kc get nodes --no-headers 2>/dev/null | grep -qw Ready || { err "node never became Ready"; kc describe nodes | tail -40 >&2 || true; return 1; }

  log "waiting for core system pods (coredns / flannel)"
  for _ in $(seq 1 "$(scaled 60)"); do
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
  for _ in $(seq 1 "$(scaled 72)"); do  # up to 6 min (x wait-scale)
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
  local summary; summary="$(grep -o '"summary":"[^"]*"' "$RESULT_FILE" | head -1 | cut -d'"' -f4 || true)"
  log "scenario '${name}': re-run summary = ${summary:-<none>}"
  [ "$(result_status)" = "success" ] || { err "idempotency re-run not success"; return 1; }
  # Strict idempotency: a converged node must report ZERO host changes. The
  # result's "changed" array is omitempty, so its presence means real changes
  # happened on a re-apply of identical input — a drift/idempotency bug.
  if grep -q '"changed":\[' "$RESULT_FILE" 2>/dev/null; then
    err "scenario '${name}': idempotency re-run reported host changes (expected none):"
    grep -o '"changed":\[[^]]*\]' "$RESULT_FILE" >&2 || true
    return 1
  fi
  log "scenario '${name}': zero host changes ✅"
}

# cmd_cert_hashes — print content fingerprints of the live CA + API server leaf
# so the host harness can assert a CA rotation actually replaced them. Uses
# sha256sum (always present) rather than openssl so it has no extra dependency.
cmd_cert_hashes() {
  local cdir=/etc/kubernetes/certs ca server
  ca="$(sha256sum "$cdir/ca.crt" 2>/dev/null | awk '{print $1}')"
  server="$(sha256sum "$cdir/server.crt" 2>/dev/null | awk '{print $1}')"
  echo "ca=${ca:-none} server=${server:-none}"
}

# cmd_kubelet_version [node] — print a node's reported kubelet version (defaults
# to the first node) so the host can assert an upgrade actually took effect.
cmd_kubelet_version() {
  local node="${1:-}"
  [ -n "$node" ] || node="$(kc get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "$node" ] || { echo none; return; }
  kc get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion}' 2>/dev/null || echo none
}

# --- "agent" trigger: real heat-container-agent + local mock Heat -----------

write_oscc_conf() {
  # configure_container_agent.sh only writes this if absent, and the agent mounts
  # the host /etc, so pre-writing it points os-collect-config at our mock Heat.
  log "writing /etc/os-collect-config.conf (request collector -> mock Heat)"
  cat > /etc/os-collect-config.conf <<EOF
[DEFAULT]
command = os-refresh-config
polling_interval = 5
collectors = request

[request]
metadata_url = http://${HEAT_LISTEN}/md/${AGENT_NODE}
EOF
  chmod 600 /etc/os-collect-config.conf
}

start_mock_heat() {
  log "starting mock Heat on ${HEAT_LISTEN} (state ${AGENT_STATE_DIR})"
  mkdir -p "$AGENT_STATE_DIR"
  printf '{"deployments":[]}' > "$AGENT_STATE_DIR/${AGENT_NODE}.md.json"
  chmod +x "$E2E_DIR/mock-heat"
  systemctl reset-failed mock-heat 2>/dev/null || true
  systemd-run --unit=mock-heat --collect "$E2E_DIR/mock-heat" -listen "$HEAT_LISTEN" -dir "$AGENT_STATE_DIR" -v
  for _ in $(seq 1 30); do
    if curl -fsS "http://${HEAT_LISTEN}/healthz" >/dev/null 2>&1; then log "mock Heat healthy"; return 0; fi
    sleep 1
  done
  err "mock Heat did not become healthy"; journalctl -u mock-heat --no-pager | tail -20 >&2; return 1
}

start_heat_agent() {
  log "starting real heat-container-agent (${AGENT_IMAGE})"
  mkdir -p /opt/stack/os-config-refresh /var/run/heat-config /var/lib/heat-container-agent \
           /var/run/os-collect-config /srv/magnum
  podman rm -f heat-container-agent >/dev/null 2>&1 || true
  podman pull "$AGENT_IMAGE"
  # Same mounts/flags as the Magnum user_data unit: privileged + net=host so the
  # agent's scripts reach the host via ssh root@localhost and bind-mounted paths.
  podman run -d --name heat-container-agent --privileged --net=host \
    --volume /srv/magnum:/srv/magnum \
    --volume /opt/stack/os-config-refresh:/opt/stack/os-config-refresh \
    --volume /run/systemd:/run/systemd \
    --volume /etc/:/etc/ \
    --volume /var/lib:/var/lib \
    --volume /var/run:/var/run \
    --volume /var/log:/var/log \
    --volume /tmp:/tmp \
    --volume /dev:/dev \
    "$AGENT_IMAGE" /usr/bin/start-heat-container-agent >/dev/null
  for _ in $(seq 1 30); do
    if podman ps --format '{{.Names}}' 2>/dev/null | grep -qx heat-container-agent; then
      log "agent container running"; return 0
    fi
    sleep 1
  done
  err "heat-container-agent did not start"; podman logs heat-container-agent 2>&1 | tail -30 >&2; return 1
}

cmd_agent_setup() {
  AGENT_NODE="${1:-$AGENT_NODE}"
  write_oscc_conf
  start_mock_heat
  start_heat_agent
  log "agent-setup complete (node=${AGENT_NODE}, mock Heat ${HEAT_LISTEN})"
}

# cmd_heat_deploy <metadata.json> <deploy-id> <name> [node] — publish a new
# SoftwareDeployment for the agent to pick up, then wait for its HEAT_SIGNAL and
# assert success exactly as Heat would (reconcile_status=success, no error_output).
cmd_heat_deploy() {
  local md="$1" id="$2" name="$3" node="${4:-$AGENT_NODE}"
  local sig="$AGENT_STATE_DIR/${id}.signal.json"
  local deploy_timeout="${AGENT_DEPLOY_TIMEOUT:-}"; [ -n "$deploy_timeout" ] || deploy_timeout="$(scaled 900)"
  log "scenario '${name}': publishing deployment id=${id} (node=${node})"
  rm -f "$sig"
  install -m 600 "$md" "$AGENT_STATE_DIR/${node}.md.json.tmp"
  mv "$AGENT_STATE_DIR/${node}.md.json.tmp" "$AGENT_STATE_DIR/${node}.md.json"
  log "scenario '${name}': waiting up to ${deploy_timeout}s for the agent to run + signal"
  local waited=0
  while [ "$waited" -lt "$deploy_timeout" ]; do
    [ -s "$sig" ] && break
    sleep 5; waited=$((waited + 5))
    [ $((waited % 60)) -eq 0 ] && log "  ...still waiting (${waited}s)"
  done
  if [ ! -s "$sig" ]; then
    err "scenario '${name}': no Heat signal after ${deploy_timeout}s"
    podman logs heat-container-agent 2>&1 | tail -50 >&2 || true
    return 1
  fi
  local status code failure
  status="$(sed -n 's/.*"reconcile_status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$sig" | head -1)"
  code="$(sed -n 's/.*"deploy_status_code"[[:space:]]*:[[:space:]]*\([0-9-]*\).*/\1/p' "$sig" | head -1)"
  failure="$(sed -n 's/.*"reconcile_failure"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$sig" | head -1)"
  log "scenario '${name}': signal status='${status}' deploy_status_code='${code:-?}' failure='${failure}'"
  if [ "$status" = "success" ] && [ -z "$failure" ]; then
    log "scenario '${name}': Heat deployment succeeded ✅"
    return 0
  fi
  err "scenario '${name}': Heat deployment did not succeed"
  echo '--- signal ---' >&2; cat "$sig" >&2; echo >&2
  echo '--- last 80 reconcile log lines ---' >&2; tail -80 /var/log/magnum-reconcile.log 2>/dev/null >&2 || true
  return 1
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
    cert-hashes)      cmd_cert_hashes ;;
    kubelet-version)  cmd_kubelet_version "$@" ;;
    agent-setup)      cmd_agent_setup "$@" ;;
    heat-deploy)      cmd_heat_deploy "$@" ;;
    *) err "unknown subcommand: ${sub}"; exit 2 ;;
  esac
}

main "$@"
