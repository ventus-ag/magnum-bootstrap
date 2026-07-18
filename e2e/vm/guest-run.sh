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
#   assert-periodic-heal          inject drift, heal via run-periodic, assert zero-change steady state
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

helm_bin() {
  for h in /usr/local/bin/helm /srv/magnum/bin/helm helm; do
    if command -v "$h" >/dev/null 2>&1 || [ -x "$h" ]; then echo "$h"; return; fi
  done
  echo helm
}

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

# enable_host_provider — when the magnumhost Pulumi plugin was delivered
# (HOST_PROVIDER_PATH), point the reconcile systemd units at it via a drop-in so
# the launcher-spawned reconciler runs the real provider instead of the legacy
# hostresource bridge. The reconciler reads MAGNUM_HOST_PROVIDER_PATH, adds its
# dir to PATH, and pulumi loads the ambient pulumi-resource-magnumhost plugin.
enable_host_provider() {
  [ -n "${HOST_PROVIDER_PATH:-}" ] && [ -f "$HOST_PROVIDER_PATH" ] || return 0
  chmod +x "$HOST_PROVIDER_PATH"
  log "enabling magnumhost provider for reconcile: $HOST_PROVIDER_PATH"
  local u
  for u in magnum-reconcile magnum-reconcile-periodic; do
    mkdir -p "/etc/systemd/system/${u}.service.d"
    cat > "/etc/systemd/system/${u}.service.d/host-provider.conf" <<EOF
[Service]
Environment=MAGNUM_USE_HOST_PROVIDER=true
Environment=MAGNUM_HOST_PROVIDER_PATH=${HOST_PROVIDER_PATH}
EOF
  done
  systemctl daemon-reload 2>/dev/null || true
}

cmd_setup() {
  # Persist the wait multiplier so every later guest-run invocation (assert-*,
  # heat-deploy) scales its timeouts identically.
  echo "${WAIT_SCALE:-1}" > "$E2E_DIR/wait-scale"
  enable_host_provider
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
  # The reconciler writes pretty-printed JSON (`"status": "succeeded"`), so the
  # extraction must tolerate whitespace after the colon.
  grep -oE '"status"[[:space:]]*:[[:space:]]*"[^"]*"' "$RESULT_FILE" 2>/dev/null | head -1 | sed -E 's/.*"([^"]*)"$/\1/'
}

# run_periodic — like run_reconcile, but through the drift-correction unit
# (magnum-reconcile-periodic.service → `bootstrap run-periodic`): the code path
# with refresh enabled and etcd day-2 gating that no other tier exercises.
run_periodic() {
  rm -f "$RESULT_FILE"
  systemctl reset-failed magnum-reconcile-periodic.service 2>/dev/null || true
  log "periodic run — streaming /var/log/magnum-reconcile.log"
  touch /var/log/magnum-reconcile.log 2>/dev/null || true
  ( stdbuf -oL tail -n0 -F /var/log/magnum-reconcile.log 2>/dev/null | sed -u 's/^/    | /' ) &
  local tail_pid=$! rc=0
  systemctl start --wait magnum-reconcile-periodic.service || rc=$?
  sleep 1; pkill -P "$tail_pid" 2>/dev/null || true; kill "$tail_pid" 2>/dev/null || true
  if [ "$rc" != 0 ]; then
    err "periodic unit failed (rc=$rc)"
    journalctl -u magnum-reconcile-periodic.service -n 40 --no-pager 2>/dev/null | sed 's/^/    | /' >&2 || true
  fi
  return "$rc"
}

# cmd_assert_periodic_heal — inject real host drift (crashed control-plane
# service + content-mangled managed file), run the periodic unit, assert both
# healed, then assert a second periodic run reports zero host changes.
cmd_assert_periodic_heal() {
  local f=/etc/kubernetes/kubelet-config.yaml
  [ -f "$f" ] || { err "periodic-heal: $f missing (create scenario must run first)"; return 1; }
  local before; before="$(sha256sum "$f" | awk '{print $1}')"
  log "periodic-heal: injecting drift (stop kube-scheduler, mangle $f)"
  systemctl stop kube-scheduler || { err "periodic-heal: could not stop kube-scheduler"; return 1; }
  echo "# drift injected by e2e" >> "$f"

  run_periodic || { err "periodic heal run failed"; return 1; }
  [ "$(result_status)" = "succeeded" ] || { err "periodic heal run status=$(result_status)"; return 1; }
  systemctl is-active kube-scheduler >/dev/null 2>&1 || { err "periodic-heal: kube-scheduler not restarted"; return 1; }
  local after; after="$(sha256sum "$f" | awk '{print $1}')"
  [ "$before" = "$after" ] || { err "periodic-heal: $f content drift not healed"; return 1; }
  log "periodic-heal: drift healed ✅"

  # Steady state: a second periodic run on a converged node must be zero-change
  # (same strictness as assert-noop).
  run_periodic || { err "periodic steady-state run failed"; return 1; }
  if grep -qE '"changed"[[:space:]]*:[[:space:]]*\[' "$RESULT_FILE" 2>/dev/null; then
    err "periodic steady-state run reported host changes (expected none):"
    grep -oE '"changed"[[:space:]]*:[[:space:]]*\[[^]]*\]' "$RESULT_FILE" >&2 || true
    return 1
  fi
  log "periodic-heal: zero-change steady state ✅"
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
  if [ "$status" != "succeeded" ]; then
    err "scenario '${name}': expected succeeded, got '${status}'"
    [ -s "$RESULT_FILE" ] && { echo '--- reconciler-last-run.json ---' >&2; cat "$RESULT_FILE" >&2; }
    echo '--- last 80 log lines ---' >&2
    tail -80 /var/log/magnum-reconcile.log 2>/dev/null >&2 || true
    return 1
  fi
}

cmd_assert_ready() {
  log "asserting single-node cluster readiness"
  # The kubelet registers the node a few seconds after the reconcile returns
  # (longer under TCG), so do NOT bail early when no node is registered yet — the
  # wait loop below tolerates an empty node list and keeps polling for Ready.
  for _ in $(seq 1 "$(scaled 60)"); do
    if kc get nodes --no-headers 2>/dev/null | grep -qw Ready; then
      log "node Ready: $(kc get nodes --no-headers | tr -s ' ')"
      break
    fi
    sleep 5
  done
  kc get nodes --no-headers 2>/dev/null | grep -qw Ready || { err "node never registered/became Ready"; kc get nodes >&2 || true; kc describe nodes | tail -40 >&2 || true; return 1; }

  log "waiting for core system pods (coredns / flannel)"
  for _ in $(seq 1 "$(scaled 60)"); do
    local cd fl
    # coredns + flannel are deployed by Helm charts: coredns lands in kube-system
    # but without the legacy k8s-app=kube-dns label, and flannel lands in the
    # kube-flannel namespace. Match by pod name across all namespaces rather than
    # by a namespace/label that the charts do not set.
    cd="$(kc get pods -A --no-headers 2>/dev/null | grep -i coredns | grep -c Running || true)"
    fl="$(kc get pods -A --no-headers 2>/dev/null | grep -i flannel | grep -c Running || true)"
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
  local summary; summary="$(grep -oE '"summary"[[:space:]]*:[[:space:]]*"[^"]*"' "$RESULT_FILE" | head -1 | sed -E 's/.*"([^"]*)"$/\1/' || true)"
  log "scenario '${name}': re-run summary = ${summary:-<none>}"
  [ "$(result_status)" = "succeeded" ] || { err "idempotency re-run not succeeded"; return 1; }
  # Strict idempotency: a converged node must report ZERO host changes. The
  # result's "changed" array is omitempty, so its presence means real changes
  # happened on a re-apply of identical input — a drift/idempotency bug. The JSON
  # is pretty-printed (`"changed": [`), so match whitespace after the colon.
  if grep -qE '"changed"[[:space:]]*:[[:space:]]*\[' "$RESULT_FILE" 2>/dev/null; then
    err "scenario '${name}': idempotency re-run reported host changes (expected none):"
    grep -oE '"changed"[[:space:]]*:[[:space:]]*\[[^]]*\]' "$RESULT_FILE" >&2 || true
    return 1
  fi
  log "scenario '${name}': zero host changes ✅"
}

# cmd_lb_start <api_vip> <etcd_vip> <cnet_prefix> <master_cips_csv> — stand up the
# two control-plane load balancers a real Magnum cluster gets from Octavia:
#   api_lb  : <api_vip>:6443  -> each master :6443  (KUBE_API_*_ADDRESS)
#   etcd_lb : <etcd_vip>:2379 -> each master :2379  (ETCD_LB_VIP)
# Both VIPs are added as IP aliases on this node's cluster interface (master-0
# hosts the LBs; the VIPs are reachable by every master/worker over the shared
# cluster network). mock-lb is L4 only, so the masters' real TLS passes through.
cmd_lb_start() {
  local api_vip="$1" etcd_vip="$2" prefix="$3" cips="$4"
  chmod +x "$E2E_DIR/mock-lb"
  local dev
  dev="$(ip -o -4 addr show 2>/dev/null | awk -v p="$prefix" '$4 ~ ("^" p "\\.") {print $2; exit}')"
  [ -n "$dev" ] || { err "lb-start: no interface on ${prefix}.0/24"; ip -o -4 addr show >&2; return 1; }
  log "lb-start: VIPs api=${api_vip} etcd=${etcd_vip} on ${dev}; backends ${cips}"
  ip addr add "${api_vip}/24"  dev "$dev" 2>/dev/null || true   # idempotent: EEXIST is fine
  ip addr add "${etcd_vip}/24" dev "$dev" 2>/dev/null || true
  local api_be="" etcd_be="" ip
  for ip in ${cips//,/ }; do
    api_be="${api_be:+$api_be,}${ip}:6443"
    etcd_be="${etcd_be:+$etcd_be,}${ip}:2379"
  done
  local unit
  for unit in mock-lb-api mock-lb-etcd; do systemctl reset-failed "$unit" 2>/dev/null || true; done
  systemd-run --unit=mock-lb-api  --collect "$E2E_DIR/mock-lb" -name api  -listen "${api_vip}:6443"  -backends "$api_be"
  systemd-run --unit=mock-lb-etcd --collect "$E2E_DIR/mock-lb" -name etcd -listen "${etcd_vip}:2379" -backends "$etcd_be"
  # Best-effort confirmation that the LB processes are listening. Backends are
  # expected to be DOWN here: the LBs come up before any master reconcile, and
  # mock-lb's health loop picks each apiserver/etcd up within ~2s of it binding.
  # So this never gates the run — it only surfaces a totally dead LB process.
  local _
  for _ in $(seq 1 6); do
    if ss -ltn 2>/dev/null | grep -q ":6443"; then
      log "lb-start: mock-lb listening (api ${api_vip}:6443, etcd ${etcd_vip}:2379) ✅"; return 0
    fi
    sleep 2
  done
  log "lb-start: mock-lb started (api ${api_vip}:6443, etcd ${etcd_vip}:2379); backends register as masters come up"
  systemctl is-active mock-lb-api mock-lb-etcd >/dev/null 2>&1 \
    || { err "lb-start: a mock-lb unit failed to start"; journalctl -u mock-lb-api -u mock-lb-etcd --no-pager 2>/dev/null | tail -20 >&2 || true; return 1; }
  return 0
}

# cmd_assert_etcd_members <expected> <endpoint> — assert the etcd cluster has
# <expected> members (run on master-0 after additional masters join). <endpoint>
# is a reachable etcd client URL, e.g. https://<master-0-cluster-ip>:2379.
cmd_assert_etcd_members() {
  local want="$1" ep="$2" cdir=/etc/etcd/certs
  local etcdctl=/usr/local/bin/etcdctl
  [ -x "$etcdctl" ] || { err "assert-etcd-members: $etcdctl missing"; return 1; }
  log "asserting etcd has ${want} member(s) via ${ep}"
  local _ got
  for _ in $(seq 1 "$(scaled 36)"); do  # up to 3 min (x wait-scale) for joins to settle
    got="$("$etcdctl" --endpoints="$ep" --command-timeout=5s \
            --cacert="$cdir/ca.crt" --cert="$cdir/server.crt" --key="$cdir/server.key" \
            member list 2>/dev/null | grep -c started || true)"
    if [ "${got:-0}" = "$want" ]; then log "etcd member count = ${got} ✅"; return 0; fi
    sleep 5
  done
  err "etcd member count = ${got:-?}, want ${want}"
  "$etcdctl" --endpoints="$ep" --cacert="$cdir/ca.crt" --cert="$cdir/server.crt" --key="$cdir/server.key" \
    member list 2>&1 | sed 's/^/    | /' >&2 || true
  return 1
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

# cmd_dump_state — print the cluster's nodes, pods (all namespaces) and Helm
# releases so a finished run shows what actually came up, and a failed run shows
# what didn't. Best-effort: never fails the run (callers ignore the exit code).
cmd_dump_state() {
  log "cluster state: nodes / pods / helm releases"
  echo "    --- nodes ---"
  kc get nodes -o wide 2>&1 | sed 's/^/    | /' || true
  echo "    --- pods (all namespaces) ---"
  kc get pods -A -o wide 2>&1 | sed 's/^/    | /' || true
  echo "    --- helm releases ---"
  KUBECONFIG="$ADMIN_KUBECONFIG" "$(helm_bin)" list -A 2>&1 | sed 's/^/    | /' || true
  # Problem pods: anything not Running/Completed, or with restarts. A crash-
  # looping addon (e.g. flannel on one node) is otherwise a black box in CI —
  # the pod listing shows "Error" but never WHY. Dump describe-events plus
  # current AND previous container logs, bounded so a wide outage can't flood
  # the harness log.
  echo "    --- problem pods: events + logs ---"
  kc get pods -A --no-headers 2>/dev/null \
    | awk '$4 != "Running" && $4 != "Completed" && $4 != "Succeeded" || $5+0 > 0 {print $1" "$2}' \
    | head -10 | while read -r ns pod; do
      [ -n "$pod" ] || continue
      echo "    | ===== $ns/$pod ====="
      kc describe pod "$pod" -n "$ns" 2>&1 | sed -n '/^Events:/,$p' | tail -15 | sed 's/^/    | /' || true
      echo "    | ----- logs (current) -----"
      kc logs "$pod" -n "$ns" --all-containers --tail=60 2>&1 | sed 's/^/    | /' || true
      echo "    | ----- logs (previous attempt) -----"
      kc logs "$pod" -n "$ns" --all-containers --previous --tail=60 2>&1 | sed 's/^/    | /' || true
    done
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
    assert-periodic-heal) cmd_assert_periodic_heal ;;
    cert-hashes)      cmd_cert_hashes ;;
    dump-state)       cmd_dump_state ;;
    kubelet-version)  cmd_kubelet_version "$@" ;;
    lb-start)            cmd_lb_start "$@" ;;
    assert-etcd-members) cmd_assert_etcd_members "$@" ;;
    agent-setup)      cmd_agent_setup "$@" ;;
    heat-deploy)      cmd_heat_deploy "$@" ;;
    *) err "unknown subcommand: ${sub}"; exit 2 ;;
  esac
}

main "$@"
