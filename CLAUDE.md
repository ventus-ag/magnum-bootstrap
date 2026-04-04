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
bootstrap run-once     # Alias for up (Heat-triggered invocations)
bootstrap run-periodic # Alias for up (timer-triggered)
bootstrap cancel       # Cancel the current Pulumi update for the local node stack
bootstrap validate-input   # Parse heat-params, print role/operation
bootstrap print-last-result # Print last result JSON
```

### Common Flags (preview/up/run-once/run-periodic)

```
--diff                 Show diff-oriented output
--allow-partial        Skip unimplemented modules and continue
--refresh              Pulumi refresh before run (default: true, detects drift)
--target-phase STRING  Execute only specified phase
--parallelism INT      Pulumi resource operation parallelism (default: 10)
--debug                Enable Pulumi debug logging and verbose event output
--backend-url STRING   Override Pulumi backend URL
--heat-params-file     Override heat-params file path
```

## Architecture

### Reconciliation Flow

1. Parse `heat-params` → typed `config.Config` (~80 typed fields)
2. Detect role (master/worker) and operation (create/upgrade/resize/ca-rotate)
3. Build ordered phase plan from catalog
4. Recover interrupted runs (PID-based detection)
5. Create Pulumi stack with inline program:
   - `stack.Refresh()` syncs Pulumi state with actual node state (default: on)
   - `stack.Preview()` (preview mode) or `stack.Up()` (apply mode)
   - Parallelism controls concurrent Pulumi resource operations
6. Stream Pulumi engine events to terminal (colored, real-time)
7. Persist reconciler state + result JSON
8. Exit with code for Heat signal transport

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

- **Automation API**: `auto.UpsertStackInlineSource` with inline `RunFunc`
- **Self-bootstrapping CLI**: `auto.InstallPulumiCommand()` on first run
- **Local file backend**: `file:///var/lib/magnum/pulumi` (no cloud, no secrets)
- **Stack naming**: `node-{sanitized-instance-name}`
- **Dependency chain**: Each phase `DependsOn` the previous phase's Pulumi component
- **Kubernetes provider**: Used by cluster-addon modules for RBAC, Secrets, Helm
- **patchForce**: Cluster-addon K8s resources use `pulumi.com/patchForce` annotation
  to adopt pre-existing resources created by legacy kubectl scripts

### Module System

Each module implements `moduleapi.Module`:

```go
type Module interface {
    PhaseID() string
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
  magnum/client.go          Keystone auth, Magnum CA fetch, CSR signing (Go crypto)
  module/
    module.go               Type alias for moduleapi.Module
    registry.go             Module registry (28 modules)
    prereq-validation/      Input validation checks
    container-runtime/      Docker/containerd install, config, drift detection
    client-tools/           kubectl/kubelet binary download & install
    master-certs/           Master cert generation via Magnum API (6 certs)
    worker-certs/           Worker cert generation via Magnum API (2 certs)
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
    cluster-auto-healer/    Node Problem Detector via Helm
    cluster-autoscaler/     Cluster Autoscaler via Helm (Magnum provider)
  moduleapi/moduleapi.go    Module interface, HeatParams, Request, RestartTracker
  paths/paths.go            Runtime paths from environment variables
  plan/
    plan.go                 Plan type, phase filtering
    catalog.go              Phase sequences for each role + operation
  provider/heatparams/      Heat-params Pulumi provider (unused currently)
  pulumi/program.go         Pulumi program builder, RunAccumulator, dependency chain
  reconcile/reconciler.go   Main reconcile orchestration, parallelism
  result/result.go          Result JSON, Heat signal text rendering
  state/state.go            Reconciler state persistence
```

## Phase Catalog

### Master Create (22 phases)

| # | Phase | Disruptive | Notes |
|---|-------|------------|-------|
| 1 | prereq-validation | no | |
| 2 | container-runtime | yes | |
| 3 | client-tools | no | |
| 4 | master-certificates | yes | |
| 5 | cert-api-manager | yes | |
| 6 | etcd | yes | |
| 7 | kube-os-config | no | |
| 8 | admin-kubeconfig | no | |
| 9 | kube-master-config | yes | Signals restart for kube services |
| 10 | storage | yes | |
| 11 | services | yes | RestartTracker-driven |
| 12 | proxy-env | no | |
| 13 | health | no | |
| 14-22 | cluster-* addons | no | Master-0 only, skip on other masters |

### Worker Create (12 phases)

| # | Phase | Disruptive |
|---|-------|------------|
| 1 | prereq-validation | no |
| 2 | container-runtime | yes |
| 3 | client-tools | no |
| 4 | kube-os-config | no |
| 5 | worker-certificates | yes |
| 6 | registry | no |
| 7 | admin-kubeconfig | no |
| 8 | kube-worker-config | yes |
| 9 | proxy-env | no |
| 10 | storage | yes |
| 11 | services | yes |
| 12 | health | no |

### Master Reconcile (9-10 + cluster addons)

prereq-validation → [ca-rotation] → etcd → admin-kubeconfig → stop-services →
client-tools → container-runtime → kube-master-config → start-services →
health → cluster-* addons

### Worker Reconcile (8-9 phases)

prereq-validation → [ca-rotation] → admin-kubeconfig → stop-services →
client-tools → container-runtime → kube-worker-config → start-services → health

## Key Design Decisions

### Idempotency
- All file writes use `EnsureFile` (content-compare, atomic write)
- Services only restart when config actually changed (RestartTracker)
- Certs skipped if already exist (`certExists` check)
- Storage skipped if already mounted (`mountpoint -q`)
- Device wait loops (60 attempts, 30s) for Cinder volume attachment

### Drift Detection
- `--refresh` defaults to true — Pulumi syncs state each run
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
- Admin kubeconfig updated with new certs (base64 inline)
- Service health check after restart (API /healthz)
- Workload patching (Deployments/DaemonSets) for pod rollout
- Rotation ID tracking prevents duplicate rotation

## Remaining Work

- [ ] Add `make test` and `make lint` targets to Makefile
- [ ] Add unit tests for config parsing edge cases
- [ ] Add unit tests for certificate generation (magnum client)
- [ ] Add golden tests for rendered files (cloud-config, kubeconfig, kubelet-config, etcd.conf)
- [ ] Add state backup before disruptive apply steps
- [ ] Add Pulumi state backup before apply steps
- [ ] Integration test on real master/worker nodes (create, upgrade, resize, CA rotation)
- [ ] Verify Heat signal transport end-to-end
- [ ] Test etcd cluster join/rejoin/scale-down logic with multi-master setups
- [ ] Test cluster-addon Helm chart deployment and adoption of existing releases
- [ ] Reduce binary size (K8s provider adds ~80MB)
