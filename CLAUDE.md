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
3. Detect role (master/worker) and operation. Operation is state-driven and only
   distinguishes `ca-rotate` (active CA_ROTATION_ID token) from `create`; upgrade
   and resize are NOT distinct operations — they are ordinary convergence (version
   delta via KUBE_TAG, node add/remove via count). There are no IS_UPGRADE /
   IS_RESIZE heat-params; drain intent comes from the PreviousKubeTag delta.
4. Build ordered phase plan from unified catalog (same phases for all operations;
   each module decides internally whether to act)
5. Recover interrupted runs (PID-based detection)
6. Select or create Pulumi stack (select-first, upsert on 404):
   - `stack.Refresh()` syncs Pulumi state with Pulumi-managed resources
     (mainly Kubernetes/Helm resources). Host-level drift in files/services is
     still detected by module `Run()` logic on every execution. (run-once: off,
     run-periodic/up: on; 2 retries, non-fatal on failure)
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
- **Pulumi resource model**: node-level phases are custom component resources
  backed by imperative `Run()` logic; cluster-addon phases use native Pulumi
  Kubernetes/Helm resources
- **Host resource migration layer**: `internal/hostresource` centralizes common
  host state shapes (directory, file, copy, export, line, download, systemd
  service, module-load, sysctl, tar extract, mode, ownership). This is a transitional step toward a real
  Pulumi host provider; today these resources improve structure and Pulumi state
  visibility but do not yet provide independent `Read()`-based refresh of node
  state.
- **Provider-ready read/diff step**: core host resources (`directory`, `file`,
  `line`, `systemd`, `mode`, `ownership`) now expose observed state and drift
  reasons during `Register()`. This is still not a real Pulumi provider, but it
  establishes the `Read`/drift contract that a provider plugin would need.
- **DAG dependencies**: Each module declares `Dependencies()`. The program
  builder runs ready module `Run()` phases concurrently up to `--parallelism`
  and wires the same dependencies into Pulumi `DependsOn` edges. Failed modules
  still call `Register()` to keep the DAG intact for downstream phases.
- **Hierarchy vs ordering**: `Parent(...)` is used for resource hierarchy and
  grouping; it is not the primary ordering mechanism. Cross-phase ordering comes
  from `DependsOn(...)`, and provider-ready sibling ordering now uses
  `hostresource.ChildResourceOptionsWithDeps(...)` where intra-module sequence
  matters (for example, unit file before systemd unit state).
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
cmd/pulumi-resource-magnumhost/main.go
                          Real Pulumi host provider plugin entrypoint
internal/
  app/app.go                CLI commands, orchestration wrapper
  config/
    config.go               Config types, role/operation detection, ResolveNodeIP
    heatparams.go           Heat-params KEY=VALUE parser
  display/output.go         ANSI colored terminal output, Pulumi event streaming
  host/ops.go               Idempotent host file/command primitives
  hostresource/             Shared host resource specs used by Run()+Register()
  journal/run_state.go      Run lifecycle tracking (running/completed/failed/interrupted)
  logging/logger.go         Structured file + stderr logging
  magnum/client.go          Keystone auth, Magnum CA fetch, parallel CSR signing
  module/
    module.go               Type alias for moduleapi.Module
    registry.go             Module registry (32 modules)
    prereq-validation/      Input validation checks
    container-runtime/      Docker/containerd install, config, drift detection
    client-tools/           kubectl/kubelet binary download & install
    master-certs/           Master cert generation via Magnum API (6 certs, parallel signing)
    worker-certs/           Worker cert generation via Magnum API (2 certs, parallel signing)
    cert-api-manager/       CA key for controller-manager cert signing
    etcd-config/            Etcd: volume, service, etcdctl, cluster join/rejoin/scale-down
    kube-os-config/         OpenStack cloud-config rendering
    admin-kubeconfig/       Admin kubeconfig generation (root + core/ubuntu user)
    kubecommon/             Shared helpers: CNI, sysctl, kubeconfig, kubelet config
    kube-master-config/     Master: services, kubeconfigs, Keystone webhook
      module.go             Orchestrator, network, kubelet config, docker sysconfig
      kubeconfigs.go        API server/controller/scheduler args, kubeconfig writes
      services.go           Systemd unit templates (apiserver, controller, etc.)
    kube-worker-config/     Worker: kubelet/kube-proxy, TLS stripping
      module.go             Orchestrator, network, kubelet config, docker sysconfig
      kubeconfigs.go        Worker kubeconfig writes (remote master, TLS-disabled path)
      services.go           Systemd unit templates (kubelet, kube-proxy)
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
    cluster-gpu-operator/   NVIDIA GPU Operator via Helm (requires GPU nodes)
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
provider/
  hostplugin/              In-repo Pulumi host provider package
    provider.go            Provider builder and schema metadata
    file_resource.go       Real custom resource: host file
    directory_resource.go  Real custom resource: host directory
    line_resource.go       Real custom resource: required line in file
    export_resource.go     Real custom resource: shell export in file
    copy_resource.go       Real custom resource: copied file
    download_resource.go   Real custom resource: downloaded file
    systemd_resource.go    Real custom resource: systemd unit state
    mode_resource.go       Real custom resource: filesystem mode enforcement
    ownership_resource.go  Real custom resource: filesystem ownership enforcement
    sysctl_resource.go     Real custom resource: sysctl file + reload
    moduleload_resource.go Real custom resource: module-load file + modprobe
    extract_resource.go    Real custom resource: tar extraction workflow
    common.go              Provider constants and helpers
  hostsdk/                 Handwritten Go SDK wrapper for real host provider resources
```

## Phase Catalog

All operations (create, upgrade, resize, ca-rotate, periodic) use the **same
unified phase list** per role.  Each module internally decides whether to act
based on current vs desired state.

### Master (29 phases)

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
| 27 | cluster-gpu-operator | no | Master-0 only, requires GPU nodes |
| 28 | cluster-health | no | Master-0 only, skip on other masters |
| 29 | zincati | no | Fedora CoreOS OS auto-upgrade settings |

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
- Pulumi refresh reconciles Pulumi state for provider-managed resources; it does
  not inspect host files or systemd units by itself
- Services module detects crashed services via `SystemctlIsActive`
- File modules detect content drift via content comparison or SHA256 checks

### Hostresource Migration Status
- Migrated modules: `proxy-env`, `zincati`, `kube-os-config`,
  `container-runtime`, `client-tools`, `admin-kubeconfig`, `storage`,
  `docker-registry`, `cert-api-manager`
- Master/worker config modules now register shared host resources for kubelet
  files, kubeconfigs, systemd unit files, Calico/Docker config tails, and
  common network/sysctl helpers
- Shared helpers migrated and registered from parent modules: `kubecommon`
  sysctl and flannel CNI resources now appear in `kube-master-config` and
  `kube-worker-config`
- Service orchestration modules now use shared systemd/file resources for the
  reusable state transitions (enable/start/restart/stop and `uncordon.service`),
  while tiered ordering, readiness waits, drain/uncordon, and cleanup remain
  imperative
- `etcd-config` now routes low-risk file state through shared resources
  (data dir, fstab line, service file, config dir/file, etcdctl download),
  while membership, discovery, mount, and health orchestration remain imperative
- `master-certificates` and `worker-certificates` now use shared resources for
  low-risk certificate file and directory writes; `master-certificates` also
  uses shared ownership and file-copy resources for the etcd cert handoff, while
  user/group creation and signing workflows remain imperative
- Remaining work: move more master/worker file+service operations into
  `hostresource`, then replace the current transition layer with a real Pulumi
  host provider if full native refresh/diff is required
- Provider extraction next step: move the new observed-state/drift logic behind
  provider-style CRUD/Read interfaces and package it as a real Pulumi plugin so
  refresh can query host state directly instead of only via component outputs
- Initial provider status: the in-repo `magnumhost` provider now exists as a
  real plugin binary with custom resources for `File`, `Directory`, `Line`,
  `Export`, `Copy`, `Download`, `SystemdService`, `Mode`, `Ownership`,
  `Sysctl`, `ModuleLoad`, and `ExtractTar`.
- Integration bridge status: `provider/hostsdk` now exposes typed constructors
  plus `Register...Spec(...)` bridge helpers that choose the real provider when
  enabled and fall back to legacy `hostresource.Register(...)` otherwise.
- Provider distribution path: the provider can be supplied either via
  `MAGNUM_HOST_PROVIDER_PATH` (ambient plugin binary) or
  `MAGNUM_HOST_PROVIDER_URL` (downloaded by the reconciler into local Pulumi
  state before preview/up/destroy). This fits publishing the provider binary in
  the same GitHub release flow as the bootstrap binary.
- Release-default behavior: tagged release builds now derive the provider URL
  from the same GitHub release tag as `bootstrap`, so no provider env vars are
  needed in the normal release path. `MAGNUM_USE_HOST_PROVIDER=false` disables
  provider usage even when a default release URL is available.
- Real-provider bridge usage: `cert-api-manager`, `zincati`, `proxy-env`,
  `docker-registry`, `storage`, `services`, `start-services`,
  `admin-kubeconfig`, `kube-os-config`, `client-tools`, `container-runtime`,
  the low-risk registration path of `etcd-config`, plus the cert registration
  paths in `master-certificates` and `worker-certificates`, now register
  through the bridge layer, so enabling the provider flips those modules onto
  real provider resources without additional module-specific wiring.
- Bridge completion status: there are no direct `hostresource.Register(...)`
  callsites left under `internal/`; all current hostresource-backed register
  paths now flow through `provider/hostsdk` and can switch to real provider
  resources via env-based enablement.
- Remaining hybrid surface: `Run()` apply paths are still mostly imperative,
  and operational workflow modules (health, cluster-health, CA rotation,
  membership/join logic, drain/uncordon, storage mount/format, cert signing)
  remain intentionally non-provider-managed.

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

## E2E Test Runner

The FCoS-VM e2e tier (`e2e/vm/`) runs on a **self-hosted GitHub Actions runner**.
Read this before testing so you don't rediscover its quirks.

- **Host:** `ssh ubuntu@185.226.43.12` (runner `magnum-actions01`, 8 vCPU / 31 GB).
  Key-based SSH; passwordless sudo. No `gh` CLI; no libguestfs; no `butane`/`docker`
  (only **`podman`** — the harness uses it as the butane renderer).
- **Virtualization — TCG only, SLOW.** The runner is itself a nested AMD-V (Zen1)
  L1 guest; KVM is unusable (AVIC is mutually exclusive with nesting → L2 boots
  freeze after `basic.target`). `run-fcos-e2e.sh` auto-selects **TCG** (pure
  emulation, ~4.5 min FCoS boot; a full `create` reaches the containerd phase
  ~12 min in). Force KVM with `ALLOW_KVM_ON_NESTED_AMD=1` (it will hang). For fast
  e2e use an Intel-nested / AMD-with-AVIC / bare-metal runner instead.
- **Go:** not in the default PATH — at
  `/home/ubuntu/actions-runner/_work/_tool/go/<ver>/x64/bin` (currently 1.25.6).
  Add it to PATH for manual runs.
- **Cached FCoS image:** `~/.cache/fcos-e2e/fcos-stable.qcow2` (currently **FCoS 44**,
  composefs/read-only `/usr` — see the `--keep-directory-symlink` containerd note).
  Prod runs an older pre-composefs FCoS; the reconciler must work on both.

### Run a scenario manually (when the GHA agent is idle)

The harness builds the binary from `$REPO_ROOT` (the script's repo), so sync your
working tree to the runner and run it there — no commit/push required:

```bash
# from a local checkout of both repos:
rsync -az --mkpath -e ssh --delete --exclude=.git --exclude=dist --exclude='*.qcow2' \
  magnum-bootstrap/  ubuntu@185.226.43.12:/home/ubuntu/e2e-validate/magnum-bootstrap/
rsync -az --mkpath -e ssh --delete --exclude=.git \
  magnum_victoria/   ubuntu@185.226.43.12:/home/ubuntu/e2e-validate/magnum_victoria/

ssh ubuntu@185.226.43.12 '
  cd /home/ubuntu/e2e-validate/magnum-bootstrap
  export PATH=/home/ubuntu/actions-runner/_work/_tool/go/1.25.6/x64/bin:$PATH
  setsid env SCENARIOS=create MASTERS=1 KEEP_VM=0 \
    VICTORIA_DIR=/home/ubuntu/e2e-validate/magnum_victoria FCOS_STREAM=stable \
    bash ./e2e/vm/run-fcos-e2e.sh > /home/ubuntu/e2e-validate/run-create.log 2>&1 </dev/null &
'
# then poll /home/ubuntu/e2e-validate/run-create.log
```

Key env knobs: `SCENARIOS` (`create ca-rotate upgrade`), `MASTERS` (>=2 → multi-master
+ 2 LBs, TCG-slow), `WORKERS`, `KEEP_VM=1` (leave VMs up — SSH the master via the
per-workdir key `<workdir>/id_ed25519` on the hostfwd port printed in the log),
`VICTORIA_DIR` (required). Reconcile output is streamed into the harness log.

### CI path

The `e2e-fcos` job (in `.github/workflows/ci.yaml`) runs on **every PR**
(`opened`/`synchronize`/`reopened`) on this runner; pushing to a PR branch
re-triggers it. Requires the runner's GHA agent to be up
(`Runner.Listener`/`Runner.Worker` processes). The `e2e-fcos-multimaster` and
`e2e-openstack` jobs are label-gated. `e2e-openstack` drives real OpenStack via
the prebuilt `e2e/cmd/magnum-e2e` Go binary (gophercloud + client-go, standard
`OS_*` env auth) — no `openstack`/`kubectl` CLIs or `clouds.yaml` on the runner.

#### e2e-openstack: reusable workflow + scenario matrix

`.github/workflows/e2e-openstack.yaml` is a **reusable workflow** with two
entrypoints: `workflow_call` (invoked by `ci.yaml` after `build`, reusing the
`bootstrap-dist` artifact via `use_prebuilt: true`) and `workflow_dispatch`
(standalone — builds its own `bootstrap` + `magnum-e2e`, decoupled from CI).
Concurrency group `e2e-openstack` (`cancel-in-progress: false`) so no two billed
clusters ever run at once.

The driver is an **op-engine**, not a fixed pipeline. `e2e/cmd/magnum-e2e`
creates a cluster of a configured shape, then runs an ordered op chain
(`internal` to the driver, file `e2e/cmd/magnum-e2e/ops.go`): `upgrade`,
`ca-rotate`, `resize-workers=N`, `resize-masters=N`, `add-nodepool=N`,
`resize-nodepool=N`, `del-nodepool`, `post-rotate`, `cloud-smoke`, `verify-sa`.
Selection precedence: `OPS` > `SCENARIO` > legacy `SKIP_*` flags. Presets
(`SCENARIO`): `smoke` (1m/1w, back-compat), `multinode` (3m/2w + extra worker
nodepool; worker+nodepool resize up/down → upgrade → ca-rotate → post-rotate),
`chained-single` and `chained-multinode` (the wedge sequence
`upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate`).

**`version-ladder` (1m/1w, dispatch-only).** A long multi-version upgrade walk:
create at the first version, then for every rung `upgrade → cloud-smoke`. Its op
chain is **generated** from the upgrade ladder (not a static preset), so it lives
outside the `scenarios` map / `allScenarios` (not in `SCENARIO=all`). Default
ladder (built-in, zero-config on the ventus cloud, all templates version-pinned):
`v1.20.12 → v1.23.17 → v1.28.4 → v1.30.10 → v1.32.2 → v1.33.10 → v1.34.6 →
v1.35.3` (7 upgrades). Override with `UPGRADE_LADDER` (ordered comma list of
template names) + `CLUSTER_TEMPLATE` (the create version). When `UPGRADE_LADDER`
is empty the built-in ladder OWNS the create version too (`v1.20.12`), overriding
a generic `CLUSTER_TEMPLATE`, so the first hop is never a downgrade. The ladder
cursor advances once per `upgrade` op (in `execOp`, not per `runMutation` retry,
so a retried trigger re-fires the same rung). The jumps are deliberately
multi-minor (e.g. 1.23→1.28) — non-standard for kubeadm, a stress test of the
reconciler's binary-swap upgrade. `triggerUpgrade(ctx, target)` and the per-op
`upgradeTarget()` selector live in `cluster.go`; `defaultVersionLadder`,
`nextLadderTarget`, `ladderOps` in `ops.go`.

**cloud-smoke depth (`smokeCloudIntegration`, kube.go).** The `cloud-smoke` op
(also run after every create) proves the OCCM/Cinder datapath, not just
provisioning: an **nginx** Deployment mounts the PVC (`/data`) behind a
LoadBalancer Service; it asserts the PVC **binds**, the LB **serves HTTP 200**
(`waitLBServes`, real GET to the Octavia VIP), then **resizes** the PVC 1Gi→2Gi
and waits for `status.capacity` to converge (`resizePVC`, online expansion —
needs the mount; `ensureExpandableDefaultSC` fails fast if the default
StorageClass forbids expansion). Runs in every scenario's create + ladder rung.

**Robustness (`runMutation` in cluster.go):** every mutating op goes
`ensureSettled` → snapshot → trigger (retry on busy/transient) → `waitTransition`
→ `waitStatus(UPDATE_COMPLETE)` → `verifyBundle`. This fixes the chained-op race
where `waitStatus` matched a *stale* `*_COMPLETE` and the next op hit a busy
cluster (`400 "...status is UPDATE_IN_PROGRESS is not supported"`).
`verifyBundle` = `smokeCore` + `verifyNodeCount` (k8s nodes == sum of nodegroup
counts, control-plane == master ng) + `verifySAConsistency` (disruptive ops) +
`verifyNodepoolSchedulable` (when a nodepool exists). Idempotency re-run stays
FCoS-only (this tier can't re-trigger a node without a Heat op).

`SCENARIO=all` is a meta-scenario: a **single `magnum-e2e` invocation** runs
every scenario in `allScenarios` order (smoke → multinode → chained-single →
chained-multinode) one-by-one, each its own cluster created + torn down before
the next (one run, one log, `runAllScenarios` in main.go). It does NOT stop on
first failure — all run, a per-scenario PASS/FAIL summary prints, exit is
non-zero if any failed. `-teardown` + `SCENARIO=all` deletes every scenario's
cluster (the always() safety net). `ci.yaml` `e2e-openstack` is now a **single
job** calling the reusable with `scenario: all` (schedule/label) or a single
scenario via the `os_scenario` dispatch choice.

Nodepool nodes are ordinary workers to the reconciler, labeled
`magnum.openstack.org/nodegroup=<name>`; master nodegroups are API-blocked so
multi-master is `master_count` at create (gophercloud
`nodegroups.Create/Resize/Delete`, v2.12.0).

## Manual live-cluster test (real OpenStack, curl + SSH)

Direct probe of a **pre-existing** Magnum cluster on the ventus cloud — no e2e
driver, no teardown. Use when the user provisions a cluster and asks to exercise
an op (ca-rotate, master/worker resize, upgrade) and root-cause it from node
logs. Auth + SSH key come from `.env` and `~/.ssh/id_ed25519` (the `ed` keypair).
The user provides the cluster UUID; they own create/recreate, you only mutate +
diagnose. Never delete or "fix" the live cluster without asking.

### Setup (token + endpoints)

```bash
cd magnum-bootstrap; set -a; . ./.env; set +a
# scoped token → /tmp/os_token
printf '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"%s","domain":{"name":"%s"},"password":"%s"}}},"scope":{"project":{"id":"%s"}}}}' \
  "$OS_USERNAME" "$OS_USER_DOMAIN_NAME" "$OS_PASSWORD" "$OS_PROJECT_ID" > /tmp/auth.json
curl -s -D - -o /dev/null -X POST "$OS_AUTH_URL/auth/tokens" -H 'Content-Type: application/json' -d @/tmp/auth.json \
  | awk 'BEGIN{IGNORECASE=1}/^x-subject-token:/{print $2}' | tr -d '\r' > /tmp/os_token
TOKEN=$(cat /tmp/os_token)
curl -s "$OS_AUTH_URL/auth/catalog" -H "X-Auth-Token: $TOKEN" -o /tmp/catalog.json   # find container-infra / orchestration / compute public URLs
```

Endpoints (ventus upper-austria-m1): Magnum `:9511/v1`, Heat `:8004/v1/<proj>`,
Nova `:8774/v2.1`. Heat `GET /stacks/{id}` needs `-L` (redirects to
`/stacks/{name}/{id}`). Nodegroup calls need header
`OpenStack-API-Version: container-infra latest`. Token TTL ~1h — re-mint if calls
start returning empty.

### Triggers (mirror the gophercloud ops in `e2e/cmd/magnum-e2e`)

```bash
MAGNUM=https://upper-austria-m1.ventuscloud.eu:9511; CID=<cluster-uuid>
# ca-rotate  (PATCH /certificates/{uuid}, empty body → 202)
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH "$MAGNUM/v1/certificates/$CID" -H "X-Auth-Token: $TOKEN"
# master/worker resize  (POST actions/resize; nodegroup = master/worker ng UUID from /nodegroups)
curl -s -X POST "$MAGNUM/v1/clusters/$CID/actions/resize" -H "X-Auth-Token: $TOKEN" \
  -H 'OpenStack-API-Version: container-infra latest' -H 'Content-Type: application/json' \
  -d '{"node_count":3,"nodegroup":"<master-ng-uuid>"}'
```

Poll `GET /v1/clusters/$CID` `.status` until terminal. **Guard the stale-status
race** (Magnum reports the prior `*_COMPLETE` for seconds after a trigger): only
accept `UPDATE_COMPLETE` *after* first seeing `UPDATE_IN_PROGRESS` (same logic as
`runMutation` in the driver). On `*_FAILED`, read the real reason from Heat:
`GET /stacks/{id}` `.stack_status_reason`, then walk failed nested resources
(`/resources?nested_depth=2`, filter `*FAILED*`). `deploy_status_code: ... non-zero`
on a `*_config_deployment` = the node's reconciler `run-once` exited non-zero.

### SSH into nodes (find IPs via Nova, not just `master_addresses`)

`master_addresses`/`node_addresses` only list *reported* nodes; on a failed
resize the new VM exists but isn't listed. Get every VM + floating IP from Nova:

```bash
NOVA=https://upper-austria-m1.ventuscloud.eu:8774/v2.1
curl -s "$NOVA/servers/detail?name=$STACKNAME-master" -H "X-Auth-Token: $TOKEN"   # parse addresses[].addr where type=floating
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519 core@<floating-ip>
```

On-node diagnostics (run via `sudo`): `kubectl --kubeconfig=/etc/kubernetes/admin.conf
get nodes -o wide`; reconciler log `/var/log/magnum-reconcile.log` (grep
`ERROR|fail|not match`); last result `/var/lib/magnum/reconciler-last-run.json`;
`crictl pods`; `systemctl is-active containerd kubelet etcd kube-apiserver
kube-controller-manager`; `journalctl -u etcd|kube-controller-manager`. CA-pair
sanity: `openssl x509 -in /etc/kubernetes/certs/ca.crt -noout -modulus | openssl md5`
vs `openssl rsa -in .../ca.key -noout -modulus | openssl md5` (must match);
fingerprint vs Barbican `GET /v1/certificates/$CID .pem`. etcd membership:
`etcdctl --endpoints=https://<ip>:2379 --cacert/--cert/--key=/etc/etcd/certs/...
member list -w table` + `endpoint health`. Compare `NUMBER_OF_MASTERS` in
`/etc/sysconfig/heat-params` across masters (existing master often carries a
**stale** count after resize — see below).

### Known-good vs known-bad outcomes

- **ca-rotate** (validated June 2026): Barbican CA fingerprint changes; every node
  converges to the new `ca.crt` with a matching `ca.key`; workloads not recreated
  (zero-downtime dual-CA). A new master added *after* a rotation must get the
  refreshed `ca_key` (driver `_resize_stack` → `_fetch_ca_key`) or
  kube-controller-manager crashes on `tls: private key does not match public key`.
- **master resize 1→N**: existing master-0's `/etc/sysconfig/heat-params` stays at
  the OLD `NUMBER_OF_MASTERS` (the params-only `existing:True` resize doesn't
  re-fire its `CREATE,UPDATE` deployment when the update aborts on a failed new
  member). The reconciler must therefore never trust that count destructively —
  `etcd-config`'s `cleanupExcessMembers` only evicts **unreachable** members, never
  a healthy one (else it nukes a freshly-joined master → "rejected stream … because
  it was removed" wedge). See `[[project_etcd_bootstrap_stateful]]`.

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
