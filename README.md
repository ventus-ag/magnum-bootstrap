# Magnum Bootstrap

Kubernetes node reconciliation engine for OpenStack Magnum. Replaces legacy bash-based Heat scripts with a single Go binary that converges nodes to desired state using Pulumi for declarative tracking.

## Features

- **32 reconcile modules** covering full node lifecycle: create, upgrade, resize, CA rotation
- **Unified phase plan** — same phases for all operations; each module decides internally whether to act
- **Dependency-aware phase parallelism** — independent module phases run concurrently up to `--parallelism`
- **Parallel certificate signing** — cert modules generate keys/CSRs and call Magnum signing concurrently, then write files deterministically
- **Cluster-level addons** via Pulumi Kubernetes/Helm providers (Flannel, CoreDNS, OCCM, Cinder CSI, Manila CSI, metrics-server, autoscaler, auto-healer)
- **Mixed Pulumi model** — host phases use imperative reconciliation plus Pulumi component tracking; cluster addons are native Pulumi Kubernetes/Helm resources
- **Shared hostresource layer** — common host state shapes now live in reusable resource specs (`file`, `copy`, `line`, `download`, `systemd`, `sysctl`, `module-load`, `extract`, `mode`, `ownership`)
- **Provider-ready hostresource core** — core resources now expose observed state and drift reasons during registration, which is the first step toward a real host provider with native `Read`/diff behavior
- **In-repo real provider started** — `cmd/pulumi-resource-magnumhost` and `provider/hostplugin` now provide a real Pulumi provider binary for `File`, `Directory`, `Line`, `Export`, `Copy`, `Download`, `SystemdService`, `Mode`, `Ownership`, `Sysctl`, `ModuleLoad`, and `ExtractTar`
- **Integration path started** — `provider/hostsdk` is a handwritten Go SDK wrapper plus `Register...Spec(...)` bridge layer for the real provider, and the reconciler can source the provider binary either from `MAGNUM_HOST_PROVIDER_PATH` or by downloading `MAGNUM_HOST_PROVIDER_URL` into local Pulumi state
- **Release default** — tagged release builds of `bootstrap` now default to downloading `pulumi-resource-magnumhost` from the same GitHub release, so no provider env vars are required in the normal release path; `MAGNUM_USE_HOST_PROVIDER=false` disables that behavior if needed
- **Dependency model** — use `Parent(...)` for hierarchy, `DependsOn(...)` for real ordering, and explicit sibling dependencies where provider-backed resources must not race under Pulumi parallelism
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
| `run-once` | Heat-triggered `up` invocation; refresh defaults to false |
| `run-periodic` | Timer-triggered `up` invocation; refresh defaults to true |
| `destroy` | Destroy Pulumi-managed resources and run module cleanup |
| `cancel` | Cancel the current Pulumi update for the local node stack |
| `validate-input` | Parse heat-params and print role/operation |
| `print-last-result` | Print last reconcile result JSON |

## Flags

```
--diff                 Show diff-oriented output
--allow-partial        Skip unimplemented modules, run only implemented ones
--refresh              Pulumi refresh provider-managed resources before preview/up (default: true except run-once)
--target-phase STRING  Execute only the specified phase
--parallelism INT      Maximum phase/resource operations to run in parallel (default: 10)
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

`destroy` also supports:

```
--backend-url STRING   Override Pulumi backend URL
--heat-params-file     Override heat-params file path
```

## Phase Catalog

### Master Phases

This is the catalog order. Actual execution follows module dependencies, so
independent phases can overlap.

prereq-validation → ca-rotation → container-runtime → client-tools →
master-certificates → cert-api-manager → etcd → kube-os-config →
admin-kubeconfig → stop-services → kube-master-config → storage → proxy-env →
services → start-services → health → cluster-rbac → cluster-flannel →
cluster-coredns → cluster-occm → cluster-cinder-csi → cluster-manila-csi →
cluster-metrics-server → cluster-dashboard → cluster-auto-healer →
cluster-autoscaler → cluster-gpu-operator → cluster-health → zincati

### Worker Phases

This is the catalog order. Actual execution follows module dependencies, so
independent phases can overlap.

prereq-validation → ca-rotation → container-runtime → client-tools →
kube-os-config → worker-certificates → registry → admin-kubeconfig →
stop-services → kube-worker-config → storage → proxy-env → services →
start-services → health → zincati

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
config.Load() → typed Config (100+ fields)
    ↓
plan.Build() → ordered phase list
    ↓
reconcile.Run() → Pulumi Automation API
    ↓
┌─ Dependency DAG:
│   ready mod.Run() phases execute in parallel up to --parallelism
│   completed phases call mod.Register() on the main goroutine
│   dependencies release downstream phases
└─
    ↓
result.Write() → Heat-compatible JSON
```

Key design:
- **RestartTracker** — config modules signal which services need restart; services module only restarts what changed
- **Drift detection** — `--refresh` syncs Pulumi state for provider-managed resources; host-level drift is detected by each module's own checks and by the services module
- **Pulumi model** — host phases register custom component resources and do imperative convergence in `Run()`; cluster addons are native Pulumi Kubernetes/Helm resources
- **Migration status** — `proxy-env`, `zincati`, `kube-os-config`, `container-runtime`, `client-tools`, `admin-kubeconfig`, `storage`, `docker-registry`, and `cert-api-manager` are partially migrated to the shared `hostresource` layer; master/worker modules now also register kubelet files, kubeconfigs, unit files, Calico/Docker config tails, and kubecommon sysctl/flannel resources through shared host resources
- **Provider bridge status** — all current hostresource-backed `Register()` callsites now go through the provider bridge, so enabling the host provider moves those modules onto real provider-backed resources without more module rewrites
- **Hybrid boundary** — `Register()` is now provider-ready across the hostresource-backed path, but `Run()` and operational workflow phases still remain intentionally imperative where they model runtime orchestration rather than durable resource state
- **Etcd/cert migration status** — `etcd-config` now routes low-risk file state through shared resources (dir/line/file/download) while join/rejoin/mount orchestration remains imperative; `cert-api-manager`, `master-certificates`, and `worker-certificates` now use shared resources for cert file state, and `master-certificates` also routes etcd cert copy plus ownership/permission state through shared resources while signing and user/group flows remain imperative
- **Service migration status** — `services`, `start-services`, and parts of `stop-services` now use shared systemd/file resources for reusable service state, while tiered startup, readiness checks, drain/uncordon, and podman cleanup remain imperative
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
  hostresource/         Shared host resource specs bridging imperative ops to Pulumi state
  journal/              Run state tracking, crash recovery
  logging/              Structured logger with auto-trim at 100MB
  magnum/               Keystone auth, Magnum CA fetch, parallel CSR signing
  module/               32 reconcile modules (see CLAUDE.md for details)
    kubecommon/          Shared: CNI plugins, sysctl, kubeconfig builders, kubelet config
  moduleapi/            Module interface, RestartTracker, HeatParamsComponent
  paths/                Runtime path resolution from environment
  plan/                 Unified phase catalog for master/worker plans
  pulumi/               Pulumi program builder, RunAccumulator, parallel phase scheduler
  reconcile/            Main orchestration, parallelism, error handling
  result/               Result JSON, Heat signal text
  state/                Reconciler state persistence
```

## License

Proprietary — Ventus AG
