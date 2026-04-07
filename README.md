# Magnum Bootstrap

Kubernetes node reconciliation engine for OpenStack Magnum. Replaces legacy bash-based Heat scripts with a single Go binary that converges nodes to desired state using Pulumi for declarative tracking.

## Features

- **28 native modules** covering full node lifecycle: create, upgrade, resize, CA rotation
- **Unified phase plan** — same phases for all operations; each module decides internally whether to act
- **Cluster-level addons** via Pulumi Kubernetes/Helm providers (Flannel, CoreDNS, OCCM, Cinder CSI, Manila CSI, metrics-server, autoscaler, auto-healer)
- **Desired-state reconciliation** — idempotent, drift-detecting, change-driven restarts
- **Crash recovery** — stale Pulumi lock detection, PID verification, auto-cancel
- **Resilient refresh** — 2 retries, non-fatal on failure (K8s API may be down during rotation)
- **Real-time output** — colored Pulumi event streaming with 30s heartbeat
- **Heat-compatible** — result JSON with `deploy_status_code`/`deploy_stdout`/`deploy_stderr`
- **Self-contained** — auto-installs Pulumi CLI, no external dependencies
- **Periodic drift correction** — daily at midnight via systemd timer
- **Log rotation** — auto-trims log file at 100MB

## Quick Start

```bash
# Build
make build

# Preview changes (dry-run)
./dist/bootstrap preview --allow-partial --diff

# Apply changes
./dist/bootstrap up --allow-partial --diff

# Validate heat-params input
./dist/bootstrap validate-input

# Cancel a stuck local update/lock
./dist/bootstrap cancel

# Print last result
./dist/bootstrap print-last-result
```

## Commands

| Command | Description |
|---------|-------------|
| `preview` | Dry-run: show planned changes without applying |
| `up` | Apply changes to reconcile node state |
| `run-once` | Alias for `up` (Heat-triggered invocations) |
| `run-periodic` | Alias for `up` (timer-triggered drift correction) |
| `cancel` | Cancel the current Pulumi update for the local node stack |
| `validate-input` | Parse heat-params and print role/operation |
| `print-last-result` | Print last reconcile result JSON |

## Flags

```
--diff                 Show diff-oriented output
--allow-partial        Skip unimplemented modules, run only implemented ones
--refresh              Pulumi refresh to detect drift (default: true)
--target-phase STRING  Execute only the specified phase
--parallelism INT      Pulumi resource operation parallelism (default: 50)
--debug                Enable Pulumi debug logging and verbose event output
--backend-url STRING   Override Pulumi backend URL
--heat-params-file     Override heat-params file path
```

`cancel` also supports:

```
--stack-name STRING    Override the Pulumi stack name to cancel
--backend-url STRING   Override Pulumi backend URL
--heat-params-file     Override heat-params file path
```

## Phase Catalog

### Master Create

prereq-validation → container-runtime → client-tools → master-certificates →
cert-api-manager → etcd → kube-os-config → admin-kubeconfig → kube-master-config →
storage → services → proxy-env → health → cluster-rbac → cluster-flannel →
cluster-coredns → cluster-occm → cluster-cinder-csi → cluster-manila-csi →
cluster-metrics-server → cluster-auto-healer → cluster-autoscaler

### Worker Create

prereq-validation → container-runtime → client-tools → kube-os-config →
worker-certificates → registry → admin-kubeconfig → kube-worker-config →
proxy-env → storage → services → health

All operations (create, upgrade, resize, CA rotation, periodic) use the same
unified phase list per role.  Each module internally decides whether to act.

## Runtime Contract

The launcher exports these environment variables (all have defaults):

| Variable | Default |
|----------|---------|
| `MAGNUM_RECONCILE_HEAT_PARAMS_FILE` | `/etc/sysconfig/heat-params` |
| `MAGNUM_RECONCILE_RESULT_FILE` | `/var/lib/magnum/reconciler-last-run.json` |
| `MAGNUM_RECONCILE_LOG_FILE` | `/var/log/magnum-reconcile.log` |
| `MAGNUM_RECONCILE_STATE_FILE` | `/var/lib/magnum/reconciler-state.json` |
| `MAGNUM_RECONCILE_RUN_STATE_FILE` | `/var/lib/magnum/reconciler-run.json` |
| `MAGNUM_PULUMI_BACKEND_DIR` | `/var/lib/magnum/pulumi` |
| `MAGNUM_PULUMI_BACKEND_URL` | `file:///var/lib/magnum/pulumi` |

## Architecture

```
heat-params (desired state)
    ↓
config.Load() → typed Config (~80 fields)
    ↓
plan.Build() → ordered phase list
    ↓
reconcile.Run() → Pulumi Automation API
    ↓
┌─ For each phase:
│   mod.Run()      → imperative host ops (files, downloads, systemctl)
│   mod.Register() → Pulumi component (state tracking, Helm, K8s resources)
└─
    ↓
result.Write() → Heat-compatible JSON
```

Key design:
- **RestartTracker** — config modules signal which services need restart; services module only restarts what changed
- **Drift detection** — `--refresh` syncs Pulumi state; services module starts crashed services
- **Migration** — `patchForce` annotation adopts existing K8s resources; Helm releases auto-adopted from bash
- **Idempotent** — second run with same config = zero changes
- **Error handling** — critical ops (cert copy, config write) fail hard; expected failures (useradd, etcd quorum) log and continue
- **CA rotation** — staged certs, verified before swap, rotation ID tracking prevents re-runs on periodic timer

## Layout

```
cmd/bootstrap/          CLI entrypoint
internal/
  app/                  Cobra commands, run orchestration
  config/               heat-params parser, Config types, ResolveNodeIP
  display/              Colored terminal output, Pulumi event streaming
  host/                 Idempotent file/command primitives (EnsureFile, Run, etc.)
  journal/              Run state tracking, crash recovery
  logging/              Structured logger with auto-trim at 100MB
  magnum/               Keystone auth, Magnum CA fetch, CSR signing (Go crypto)
  module/               28 reconcile modules (see CLAUDE.md for details)
  moduleapi/            Module interface, RestartTracker, HeatParamsComponent
  paths/                Runtime path resolution from environment
  plan/                 Phase catalog for master/worker × create/reconcile
  pulumi/               Pulumi program builder, RunAccumulator, dependency chain
  reconcile/            Main orchestration, parallelism, error handling
  result/               Result JSON, Heat signal text
  state/                Reconciler state persistence
```

## License

Proprietary — Ventus AG
