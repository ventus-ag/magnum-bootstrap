# CLAUDE.md — magnum-bootstrap

## Project Overview

Kubernetes node reconciliation engine for OpenStack Magnum. Replaces legacy
bash-based Heat scripts with a Go binary using Pulumi Automation API for
declarative state tracking and idempotent host operations.

- **Language:** Go 1.22+
- **IaC Engine:** Pulumi SDK v3 (Automation API, inline source programs)
- **K8s Provider:** Pulumi Kubernetes SDK v4 (RBAC, Secrets, Helm releases)
- **Go Module:** `github.com/ventus-ag/magnum-bootstrap`
- **CLI:** Cobra (`github.com/spf13/cobra`)
- **Binary:** `bootstrap` (single static binary, ~105MB with K8s provider)

## Build & Run

```bash
make build    # Build binary → dist/bootstrap (stripped, trimpath)
make fmt      # Format code with gofmt
```

### CLI Commands

```bash
bootstrap preview      # Dry-run: show planned changes
bootstrap up           # Apply changes
bootstrap run-once     # Heat-triggered up invocation, refresh default false
bootstrap run-periodic # Timer-triggered up invocation, refresh default true
bootstrap destroy      # Destroy Pulumi-managed resources + module cleanup
bootstrap cancel       # Cancel the current Pulumi update for the local node stack
bootstrap validate-input   # Parse heat-params, print role/operation
bootstrap print-last-result # Print last result JSON
```

### Common Flags (preview/up/run-once/run-periodic)

```
--diff                 Show diff-oriented output
--allow-partial        Skip unimplemented modules and continue
--refresh              Pulumi refresh before run (default: true except run-once)
--target-phase STRING  Execute only specified phase
--parallelism INT      Maximum phase/resource operations to run in parallel (default: 10)
--debug                Enable Pulumi debug logging and verbose event output
--backend-url STRING   Override Pulumi backend URL
--heat-params-file     Override heat-params file path
```

## Architecture

### Reconciliation Flow

1. Parse `heat-params` → typed `config.Config` (100+ typed fields)
2. Load previous state → set `AppliedCARotationID` so completed rotations
   don't re-trigger on periodic runs
3. Detect role (master/worker) and operation (create/upgrade/resize/ca-rotate)
4. Build ordered phase plan from unified catalog (same phases for all operations;
   each module decides internally whether to act)
5. Recover interrupted runs (PID-based detection)
6. Select or create Pulumi stack (select-first, upsert on 404):
   - `stack.Refresh()` syncs Pulumi state with actual node state
     (run-once: off, run-periodic/up: on; 2 retries, non-fatal on failure)
   - `stack.Preview()` (preview mode) or `stack.Up()` (apply mode)
   - Stale lock detection via `runWithAutoCancel()`
7. Stream Pulumi engine events to terminal (colored, real-time)
8. Persist reconciler state + result JSON
9. Exit with code for Heat signal transport

### Desired-State Model

Every module follows the desired-state pattern:
- **Check current state** before acting (file content, service status, mount status)
- **Only change what differs** from desired state
- **Report drift** as changes (e.g. crashed service → restart → reported as update)
- **Second run with same config = zero changes**

### Inter-Module Communication

- **RestartTracker** — shared across modules in a run. Config-writing modules
  (kube-master-config, kube-worker-config, container-runtime) signal which
  services need restart. The `services` module reads the tracker and only
  restarts what actually changed. Periodic runs with no changes = zero restarts.

### Pulumi Integration

- **Automation API**: `auto.SelectStackInlineSource` (fast path) with fallback to
  `auto.UpsertStackInlineSource` (first run)
- **Self-bootstrapping CLI**: `auto.InstallPulumiCommand()` on first run
- **Local file backend**: `file:///var/lib/magnum/pulumi` (no cloud, no secrets)
- **Stack naming**: `node-{sanitized-instance-name}`
- **DAG dependencies**: Each module declares `Dependencies()`. The program
  builder runs ready module `Run()` phases concurrently up to `--parallelism`
  and wires the same dependencies into Pulumi `DependsOn` edges. Failed modules
  still call `Register()` to keep the DAG intact for downstream phases.
- **Stale lock recovery**: `runWithAutoCancel()` detects
  `IsConcurrentUpdateError`, extracts PID, verifies liveness, auto-cancels stale
  locks
- **Kubernetes provider**: Used by cluster-addon modules for RBAC, Secrets, Helm
- **patchForce**: Cluster-addon K8s resources use `pulumi.com/patchForce` annotation
  to adopt pre-existing resources created by legacy kubectl scripts
- **Event buffer**: 5000-capacity channel prevents Pulumi blocking on slow consumers
- **Heartbeat**: 30s ticker logs "still running..." for long operations

### Module System

Each module implements `moduleapi.Module`:

```go
type Module interface {
    PhaseID() string
    Dependencies() []string
    Run(ctx context.Context, cfg config.Config, req Request) (Result, error)
    Register(ctx *pulumi.Context, name string, heat *HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error)
}
```

- `Run()` — imperative host operations (file writes, downloads, commands)
- `Register()` — declarative Pulumi component for state tracking
- `req.Apply` is set by `!ctx.DryRun()`: preview=false, up=true
- `req.Restarts` — shared RestartTracker for service restart signaling

### Host Operations

`host.Executor` provides idempotent primitives that respect dry-run:

- `EnsureDir`, `EnsureFile`, `EnsureCopy` — file/directory management
- `DownloadFileWithRetry` — HTTP download with SHA256 verification
- `EnsureLine`, `UpsertExport` — idempotent line/export management
- `Run`, `RunCapture` — shell command execution
- `SystemctlIsActive` — service status check for drift detection
- All return `*host.Change` describing what happened

## File Structure

```
cmd/bootstrap/main.go       Entry point
internal/
  app/app.go                CLI commands, orchestration wrapper
  config/
    config.go               Config types, role/operation detection, ResolveNodeIP
    heatparams.go           Heat-params KEY=VALUE parser
  display/output.go         ANSI colored terminal output, Pulumi event streaming
  host/ops.go               Idempotent host file/command primitives
  journal/run_state.go      Run lifecycle tracking (running/completed/failed/interrupted)
  logging/logger.go         Structured file + stderr logging
  magnum/client.go          Keystone auth, Magnum CA fetch, parallel CSR signing
  module/
    module.go               Type alias for moduleapi.Module
    registry.go             Module registry (31 modules)
    prereq-validation/      Input validation checks
    container-runtime/      Docker/containerd install, config, drift detection
    client-tools/           kubectl/kubelet binary download & install
    master-certs/           Master cert generation via Magnum API (6 certs, parallel signing)
    worker-certs/           Worker cert generation via Magnum API (2 certs, parallel signing)
    cert-api-manager/       CA key for controller-manager cert signing
    etcd-config/            Etcd: volume, service, etcdctl, cluster join/rejoin/scale-down
    kube-os-config/         OpenStack cloud-config rendering
    admin-kubeconfig/       Admin kubeconfig generation
    kube-master-config/     Master: CNI, sysctl, services, kubeconfigs, Keystone webhook
    kube-worker-config/     Worker: CNI, sysctl, kubelet/kube-proxy, TLS stripping
    docker-registry/        Docker registry v2 with Swift backend (worker only)
    storage/                Cinder/ephemeral volume: wait loop, format, mount
    services/               Enable + conditional restart (RestartTracker-driven)
    stop-services/          Drain/cordon, stop services, podman cleanup (upgrade)
    start-services/         Start services, uncordon, reboot resilience (upgrade)
    ca-rotation/            Staged CA rotation: certs, verify, swap, health check
    proxy/                  HTTP proxy systemd drop-ins, bashrc exports
    health/                 Post-reconciliation health checks
    cluster-helm/           Shared Helm release helper for cluster addons
    cluster-rbac/           RBAC roles, admin SA, os-trustee Secret (K8s provider)
    cluster-flannel/        Flannel CNI via Helm
    cluster-coredns/        CoreDNS via Helm
    cluster-occm/           OpenStack Cloud Controller Manager via Helm
    cluster-cinder-csi/     Cinder CSI driver via Helm
    cluster-manila-csi/     Manila CSI + NFS driver via Helm
    cluster-metrics-server/ Metrics Server via Helm
    cluster-dashboard/      Kubernetes Dashboard via Helm
    cluster-auto-healer/    Node Problem Detector via Helm
    cluster-autoscaler/     Cluster Autoscaler via Helm (Magnum provider)
    cluster-health/         Cluster addon health checks
    zincati/                Fedora CoreOS auto-update settings
  moduleapi/moduleapi.go    Module interface, HeatParams, Request, RestartTracker
  paths/paths.go            Runtime paths from environment variables
  plan/
    plan.go                 Plan type, phase filtering
    catalog.go              Unified phase sequences for each role
  provider/heatparams/      Heat-params Pulumi provider (unused currently)
  pulumi/program.go         Pulumi program builder, RunAccumulator
  pulumi/phase_runner.go    Dependency-aware parallel phase scheduler
  reconcile/reconciler.go   Main reconcile orchestration, parallelism
  result/result.go          Result JSON, Heat signal text rendering
  state/state.go            Reconciler state persistence
```

## Phase Catalog

All operations (create, upgrade, resize, ca-rotate, periodic) use the **same
unified phase list** per role.  Each module internally decides whether to act
based on current vs desired state.

### Master (28 phases)

| # | Phase | Disruptive | Notes |
|---|-------|------------|-------|
| 1 | prereq-validation | no | |
| 2 | ca-rotation | yes | No-op unless rotation ID changed |
| 3 | container-runtime | yes | |
| 4 | client-tools | no | |
| 5 | master-certificates | yes | Errors from useradd non-fatal (may exist) |
| 6 | cert-api-manager | yes | |
| 7 | etcd | yes | LB always checked for join detection (scaling) |
| 8 | kube-os-config | no | |
| 9 | admin-kubeconfig | no | |
| 10 | stop-services | yes | Only drain/uncordon during upgrade/resize |
| 11 | kube-master-config | yes | Signals restart for kube services |
| 12 | storage | yes | Skips if already mounted; mounts specific device |
| 13 | proxy-env | no | Runtime proxy drop-ins before service convergence |
| 14 | services | yes | RestartTracker-driven |
| 15 | start-services | yes | Only drain/uncordon during upgrade/resize |
| 16 | health | no | |
| 17 | cluster-rbac | no | Master-0 only, skip on other masters |
| 18 | cluster-flannel | no | Master-0 only, skip on other masters |
| 19 | cluster-coredns | no | Master-0 only, skip on other masters |
| 20 | cluster-occm | no | Master-0 only, skip on other masters |
| 21 | cluster-cinder-csi | no | Master-0 only, skip on other masters |
| 22 | cluster-manila-csi | no | Master-0 only, skip on other masters |
| 23 | cluster-metrics-server | no | Master-0 only, skip on other masters |
| 24 | cluster-dashboard | no | Master-0 only, skip on other masters |
| 25 | cluster-auto-healer | no | Master-0 only, skip on other masters |
| 26 | cluster-autoscaler | no | Master-0 only, skip on other masters |
| 27 | cluster-health | no | Master-0 only, skip on other masters |
| 28 | zincati | no | Fedora CoreOS OS auto-upgrade settings |

### Worker (16 phases)

| # | Phase | Disruptive | Notes |
|---|-------|------------|-------|
| 1 | prereq-validation | no | |
| 2 | ca-rotation | yes | No-op unless rotation ID changed |
| 3 | container-runtime | yes | |
| 4 | client-tools | no | |
| 5 | kube-os-config | no | |
| 6 | worker-certificates | yes | chmod errors are fatal |
| 7 | registry | no | |
| 8 | admin-kubeconfig | no | |
| 9 | stop-services | yes | Only drain/uncordon during upgrade/resize |
| 10 | kube-worker-config | yes | |
| 11 | storage | yes | |
| 12 | proxy-env | no | Runtime proxy drop-ins before service convergence |
| 13 | services | yes | |
| 14 | start-services | yes | |
| 15 | health | no | |
| 16 | zincati | no | Fedora CoreOS OS auto-upgrade settings |

## Key Design Decisions

### Idempotency
- All file writes use `EnsureFile` (content-compare, atomic write)
- Services only restart when config actually changed (RestartTracker)
- Certs skipped if existing material matches desired certificate spec; changed
  certs are signed in parallel and written in deterministic order
- Storage skipped if already mounted (`mountpoint -q`)
- Device wait loops (60 attempts, 30s) for Cinder volume attachment

### Drift Detection
- `--refresh` defaults to true for preview/up/run-periodic and false for run-once
- Services module detects crashed services via `SystemctlIsActive`
- File modules detect content drift via SHA256 comparison

### Migration from Bash
- Cluster-addon K8s resources use `patchForce` annotation to adopt existing resources
- Helm releases use `ForceUpdate` + `Replace` to adopt existing releases
- Config parser handles both `parseBool` (default false) and `!parseFalse` (default true)
  matching bash semantics for each field

### Node IP Resolution
- `Config.ResolveNodeIP()` falls back to OpenStack metadata service
  (`http://169.254.169.254/latest/meta-data/local-ipv4`) when `KUBE_NODE_IP`
  is empty — used by cert modules, kubelet config, etcd config

### CA Rotation Safety
- Staged cert generation in `/var/lib/magnum/ca-rotation/{id}/`
- All certs verified (exist + non-empty) before replacing
- Cert swap errors (cp, chown) are fatal — prevents silent broken state
- Admin kubeconfig updated with new certs; read errors are fatal
- Service health check after restart (API /healthz, 7.5 min timeout)
- Workload patching (Deployments/DaemonSets) with failure counting in warnings
- Rotation ID tracking prevents duplicate rotation on periodic runs
- Completed rotation detected at operation-detection time → falls back to
  normal create/reconcile plan instead of re-running full ca-rotate phases

### Error Handling Philosophy
- **Must succeed** (cert copy, config write, chmod): return error, halt phase
- **Expected failure** (useradd existing user, etcd during quorum formation,
  kubectl during API restart): log warning, continue
- **Informational** (daemon-reload, docker already stopped): log, don't block

## Systemd Timer

The periodic reconciler runs via `magnum-reconcile.timer`:
- **First tick**: 5 min after timer activation (`OnActiveSec=5min`)
- **Subsequent**: daily at midnight (`OnCalendar=*-*-* 00:00:00`)
- **Persistent**: fires on next boot if node was off at midnight

The timer is started AFTER the synchronous Heat-triggered run to avoid racing.

## Remaining Work

- [ ] Add `make test` and `make lint` targets to Makefile
- [ ] Add unit tests for config parsing edge cases
- [x] Add unit tests for certificate generation/signing concurrency (magnum client)
- [ ] Add golden tests for rendered files (cloud-config, kubeconfig, kubelet-config, etcd.conf)
- [ ] Add state backup before disruptive apply steps
- [ ] Add Pulumi state backup before apply steps
- [ ] Reduce binary size (K8s provider adds ~80MB)
- [ ] Implement dual-CA trust model for zero-downtime CA rotation (see caimprove.md)
- [ ] Make cluster-addon chart versions configurable (currently hardcoded in some modules)
