# CA Rotation Design

## Summary

This document describes a production-grade CA rotation design for `magnum-bootstrap`.
The current implementation rotates certificates independently on each node, which can
leave the cluster in a mixed old/new trust state while services are restarting. The
new design introduces a durable multi-step rotation protocol, cluster coordination,
and dual-trust rollout before leaf certificate cutover.

The most important compatibility requirement is preserved:

1. Live certificate file names stay exactly the same as today.
2. Consumers continue reading the same canonical paths under `/etc/kubernetes/certs`
   and `/etc/etcd/certs`.
3. Temporary names exist only in the staging area under
   `/var/lib/magnum/ca-rotation/<rotation-id>/`.

## Current Problem

Today the `ca-rotation` module performs a local staged swap:

1. Fetch new CA material from Magnum.
2. Generate new leaf certificates.
3. Copy the staged files over the live certificate directory.
4. Restart local services.
5. Record the applied rotation ID.

This works for a single node, but it is not safe for a multi-node cluster.

Problems in the current design:

1. There is no cluster-wide barrier before nodes switch trust or leaf certificates.
2. Masters can restart while peers still trust only the old CA.
3. Workers can move to the new CA before all control-plane nodes are prepared.
4. `LastCARotationID` and the marker file model only a fully-complete rotation, not
   an in-progress distributed protocol.
5. Interrupted run recovery marks a run as interrupted, but it does not resume a
   CA rotation from an internal protocol step.

## Goals

1. Make CA rotation safe for multi-master and multi-worker clusters.
2. Preserve live certificate paths and file names exactly as they are today.
3. Roll out dual trust before switching live leaf certificates to the new CA.
4. Support interrupted and resumed runs without losing cluster progress.
5. Keep the design modular so the same coordination layer can be reused for future
   distributed workflows.
6. Prefer conservative safety over aggressive completion.

## Non-Goals

1. Do not redesign the entire PKI layout.
2. Do not rename existing live certificate files.
3. Do not introduce a LAN scan or gossip mesh in the first version.
4. Do not support mixed upgrade plus CA rotation in one protocol. The design still
   targets pure CA rotation.
5. Do not rely on Pulumi state alone as the source of truth for distributed
   protocol progress.

## Compatibility Requirement

After CA rotation, the live certificate names must remain exactly as they are now.

Canonical live paths remain:

1. `/etc/kubernetes/certs/ca.crt`
2. `/etc/kubernetes/certs/server.crt`
3. `/etc/kubernetes/certs/server.key`
4. `/etc/kubernetes/certs/kubelet.crt`
5. `/etc/kubernetes/certs/kubelet.key`
6. `/etc/kubernetes/certs/admin.crt`
7. `/etc/kubernetes/certs/admin.key`
8. `/etc/kubernetes/certs/proxy.crt`
9. `/etc/kubernetes/certs/proxy.key`
10. `/etc/kubernetes/certs/controller.crt`
11. `/etc/kubernetes/certs/controller.key`
12. `/etc/kubernetes/certs/scheduler.crt`
13. `/etc/kubernetes/certs/scheduler.key`
14. `/etc/kubernetes/certs/service_account.key`
15. `/etc/kubernetes/certs/service_account_private.key`
16. `/etc/etcd/certs/ca.crt`
17. `/etc/etcd/certs/server.crt`
18. `/etc/etcd/certs/server.key`

Temporary files may use additional names, but only inside the staging directory.
No extra certificate names should remain in the live certificate directories after
the rotation is finalized.

## Existing Single-CA Assumptions

The current codebase assumes a single live `ca.crt` in many places. This is good
for compatibility, but it means the dual-trust design must change file content,
not live file names.

Important consumers include:

1. API server flags in `internal/module/kube-master-config/kubeconfigs.go`
2. Controller manager flags in `internal/module/kube-master-config/kubeconfigs.go`
3. Worker and master kubeconfigs in `internal/module/kube-master-config` and
   `internal/module/kube-worker-config`
4. Kubelet config in `internal/module/kubecommon/kubelet.go`
5. Etcd TLS config in `internal/module/etcd-config/module.go`
6. Admin kubeconfig generation in `internal/module/admin-kubeconfig/module.go`

This means the transition strategy should be:

1. Keep `ca.crt` as the canonical live trust file.
2. Change its contents from `old` to `old + new` during prepare.
3. Change its contents from `old + new` to `new` during finalize.

## High-Level Architecture

The new design has three main parts:

1. A local persistent rotation state on every node.
2. A simple coordination service reachable on a private node port.
3. A multi-phase CA rotation protocol.

### Coordination Model

The first version should use a coordinator pattern, not a mesh.

1. One master acts as coordinator.
2. The coordinator exposes cluster desired state for the current rotation.
3. Every node exposes local read-only rotation status.
4. The coordinator advances the cluster phase only after required participants
   report readiness.

Coordinator selection:

1. Prefer the lowest available master index.
2. In normal cases this is `master-0`.
3. If `master-0` is unavailable before rotation starts, the next eligible master
   may take over.

This design is simpler than a gossip system and easier to reason about when the
control plane is unstable.

### Node Inventory

The design should avoid blind LAN discovery.

Preferred inventory source:

1. Add an explicit heat-param carrying the node inventory.
2. Include node name, role, private IP, master index, and optional labels.

Fallback inventory source:

1. The coordinator snapshots the Kubernetes node set while the cluster is healthy.
2. The frozen participant set becomes the expected rotation scope.

Workers do not need cluster-wide discovery logic. The coordinator is responsible
for building and freezing the participant set.

## Live and Staging File Layout

Live paths stay unchanged. All temporary material lives under the rotation staging
directory.

Per-node staging layout:

```text
/var/lib/magnum/ca-rotation/<rotation-id>/
  state.json
  participants.json
  old/
    ca.crt
    server.crt
    server.key
    kubelet.crt
    kubelet.key
    ...
  new/
    ca.crt
    server.crt
    server.key
    kubelet.crt
    kubelet.key
    ...
  bundle/
    ca.crt
```

Rules:

1. `old/` is a local snapshot of the currently active material.
2. `new/` contains newly fetched or generated material.
3. `bundle/ca.crt` contains `old CA + new CA` in PEM order.
4. Only the live directories under `/etc/kubernetes/certs` and `/etc/etcd/certs`
   are used by running services.

## Rotation Protocol

The protocol is split into four logical stages.

### Stage 1: Snapshot

The coordinator freezes the participant set before any trust changes happen.

Actions:

1. Confirm that a pure CA rotation is requested.
2. Elect or confirm the coordinator.
3. Build the participant inventory.
4. Snapshot the expected master and worker sets.
5. Persist the rotation policy and participants.

Rules:

1. Master participation is strict.
2. If a required master is missing before rotation starts, the protocol does not
   advance past the snapshot stage.
3. Worker participation may be policy-driven, but the frozen set must still be
   recorded explicitly.

### Stage 2: Prepare

Each node stages new material and prepares dual trust.

Actions on each node:

1. Snapshot current live certs into `old/`.
2. Fetch the new CA from Magnum.
3. Generate new leaf certs into `new/`.
4. Write live `ca.crt` as `old + new`.
5. Keep live leaf certs on the old active leafs.
6. Restart only the services that must reload trust.
7. Verify local health and report `prepared`.

Important property:

1. During prepare, nodes still present the old live leaf certificates.
2. During prepare, peers trust both old and new CA.

Master rollout during prepare:

1. Roll masters one by one.
2. After each master, verify etcd health and API readiness before continuing.

Worker rollout during prepare:

1. Workers may be rolled in batches.
2. Batch size should be configurable.

### Stage 3: Cutover

After required participants are prepared, nodes switch live leaf certificates to
the new certs.

Actions on each node:

1. Atomically replace live leaf cert files with the staged `new/` material.
2. Keep live `ca.crt` as `old + new` during this stage.
3. Restart local services that consume the leaf certificates.
4. Verify health and report `cutover-complete`.

Master rollout during cutover:

1. Roll masters one by one.
2. Check etcd quorum after each master.
3. Check API health after each master.

Worker rollout during cutover:

1. Roll workers in batches.
2. Require the coordinator policy to allow the next batch.

### Stage 4: Finalize

After required participants have cut over to the new leaf certs, old trust is
removed.

Actions on each node:

1. Rewrite live `ca.crt` from bundle form to `new` only.
2. Restart services that must reload trust.
3. Verify health.
4. Clean up staging data.
5. Report `finalized`.

Safety rule:

1. `LastCARotationID` must be updated only after finalize succeeds.
2. The legacy marker file must also be written only after finalize succeeds.

## Readiness and Progress Policy

The protocol should distinguish between Kubernetes readiness and rotation
readiness.

Kubernetes readiness is used for:

1. Discovering the initial healthy participant set.
2. Verifying post-restart health.

Rotation readiness is used for:

1. Determining whether a node has prepared dual trust.
2. Determining whether a node has switched to new leaf certs.
3. Determining whether old trust can be removed.

This distinction is important because a node may be Kubernetes `Ready` while still
using only the old CA or while it has not staged the new material yet.

Recommended default policy:

1. Require all frozen masters to reach `prepared` before any master leaf cutover.
2. Require all frozen masters to reach `cutover-complete` before finalizing trust.
3. Allow worker cutover in batches.
4. Require all in-scope workers to reach `cutover-complete` before removing old
   trust, unless an explicit override policy is configured.

Possible future policy knobs:

1. `workerBatchSize`
2. `minPreparedWorkersPercent`
3. `minCutoverWorkersPercent`
4. `finalizeTimeout`
5. `allowFinalizeWithMissingWorkers`

The first version may keep finalization strict even if worker batching is allowed.

## Persistent State

Pulumi helps with idempotent resource application, but it is not enough to model
the internal state of a distributed protocol. CA rotation needs its own durable
state.

### Local State

Each node persists local state in:

```text
/var/lib/magnum/ca-rotation/<rotation-id>/state.json
```

Suggested schema:

```json
{
  "rotationId": "rotate-123",
  "role": "master",
  "instance": "cluster-master-0",
  "coordinator": true,
  "coordinatorAddress": "10.0.0.10:10443",
  "phase": "prepare",
  "caMode": "bundle",
  "leafMode": "old",
  "health": "ok",
  "participantsHash": "...",
  "updatedAt": "2026-04-17T10:00:00Z"
}
```

Important fields:

1. `phase` is the local protocol step.
2. `caMode` is one of `old`, `bundle`, `new`.
3. `leafMode` is one of `old`, `new`.
4. `participantsHash` protects against accidental reuse of stale participant data.

### Cluster State

The coordinator persists a cluster view in its own staging directory.

Suggested files:

1. `participants.json`
2. `desired-state.json`
3. `observed-status.json`

The coordinator is the source of truth for desired cluster phase during a single
rotation ID.

### Existing Reconciler State

The existing reconciler state should remain, but its meaning must change slightly.

Rules:

1. `LastCARotationID` means the rotation has been fully finalized.
2. It must not be written at prepare or cutover.
3. The existing marker file should follow the same rule.

## Coordination API

The node-to-node API should stay minimal in the first version.

### Node API

Each node exposes local read-only status.

Possible endpoint:

1. `GET /v1/ca-rotation/status`

Example response:

```json
{
  "rotationId": "rotate-123",
  "instance": "cluster-worker-2",
  "role": "worker",
  "phase": "prepare",
  "caMode": "bundle",
  "leafMode": "old",
  "health": "ok",
  "updatedAt": "2026-04-17T10:00:00Z"
}
```

### Coordinator API

The coordinator exposes desired cluster phase.

Possible endpoint:

1. `GET /v1/ca-rotation/plan`

Example response:

```json
{
  "rotationId": "rotate-123",
  "desiredPhase": "prepare",
  "participantsHash": "...",
  "requiredMasters": ["cluster-master-0", "cluster-master-1", "cluster-master-2"],
  "workerBatchSize": 5
}
```

### Authentication

The first version should use a simple authenticated private API.

Recommended approach:

1. Bind only to node private addresses.
2. Require a shared token or HMAC-based header derived from existing cluster
   secret material.
3. Reject unauthenticated requests.

Mutual TLS can be added later, but it should not block the first implementation.

## Module and Phase Changes

The current single `ca-rotation` phase should be split into explicit protocol
steps.

Recommended new phases:

1. `coordination-agent`
2. `ca-rotation-prepare`
3. `ca-rotation-cutover`
4. `ca-rotation-finalize`

Why split the phase:

1. It makes protocol progress explicit.
2. It simplifies interrupted-run resume behavior.
3. It prevents later phases from accidentally overwriting intermediate rotation
   state.
4. It gives clearer operator output and easier test coverage.

Dependency updates:

1. Modules that currently depend on `ca-rotation` should depend on
   `ca-rotation-finalize` if they assume a single stable CA.
2. Certificate reconciliation modules must not overwrite `ca.crt` or leaf certs
   during prepare or cutover.
3. The existing health checks should be reused by the new phases where possible.

## Service Restart Strategy

The restart strategy should be different for prepare, cutover, and finalize.

Prepare restarts:

1. Reload dual trust.
2. Keep old leaf certs active.
3. Restart in dependency order.

Cutover restarts:

1. Switch to new leaf certs.
2. Keep dual trust in place.
3. Restart in dependency order.

Finalize restarts:

1. Remove old trust.
2. Restart only components that require trust reload.

Masters must continue to use serialized restarts because etcd and the API server
have strict dependency ordering.

## Health Checks

Local health checks should be performed after every disruptive step.

Master checks:

1. `etcd` is active.
2. `kube-apiserver` is active.
3. `kube-controller-manager` is active.
4. `kube-scheduler` is active.
5. `kubelet` is active.
6. `/readyz` succeeds via admin kubeconfig.
7. etcd endpoint health succeeds.

Worker checks:

1. `kubelet` is active.
2. `kube-proxy` is active.
3. Node registration remains visible from the coordinator.

The coordinator should not advance the cluster phase based only on local service
status. It must also require the correct reported rotation state.

## Workload Rollout

Workloads may also need trust updates.

Current behavior patches Deployments and DaemonSets after CA rotation. The new
design should keep that behavior but make the timing explicit.

Recommended workload rollout points:

1. After dual trust is prepared on the control plane, patch workloads so they can
   consume the bundle trust if needed.
2. After finalization, optionally patch workloads again if they need to drop old
   trust.

This should remain best-effort with warnings, not a hard blocker for core control
plane safety.

## Failure Handling

The design should fail safe.

Safe states:

1. Old trust plus old leafs.
2. Bundle trust plus old leafs.
3. Bundle trust plus new leafs.

Unsafe state:

1. New leafs while required peers still trust only old CA.

Failure rules:

1. If prepare fails on a required master, stop the protocol.
2. If cutover fails on a master, stop the protocol and do not finalize trust.
3. If some workers are unavailable, the cluster may remain in dual-trust mode
   until they rejoin or an explicit policy override is used.
4. If the coordinator changes unexpectedly, the new coordinator must rebuild state
   from persisted cluster and node status before advancing the phase.

## Implementation Plan

### Phase 1: State and Inventory

1. Add a design-specific config model for rotation policy and node inventory.
2. Add persistent local rotation state files.
3. Change `LastCARotationID` semantics so it is written only after finalize.

### Phase 2: Coordination Service

1. Add a `bootstrap` subcommand for the node coordination agent.
2. Add a systemd unit for the agent.
3. Implement status and plan endpoints.
4. Add request authentication.

### Phase 3: Prepare Stage

1. Refactor current `ca-rotation` logic into a reusable staging layer.
2. Snapshot old material.
3. Generate new material into staging.
4. Write bundle trust to live `ca.crt`.
5. Add serialized master prepare rollout and batched worker prepare rollout.

### Phase 4: Cutover Stage

1. Atomically replace live leaf certificates from staged `new/` material.
2. Keep bundle trust active.
3. Add coordinator gating before each batch.

### Phase 5: Finalize Stage

1. Rewrite live `ca.crt` to new-only trust.
2. Clean staging directories.
3. Write the final marker and reconciler state.

### Phase 6: Hardening

1. Add unit tests for state transitions.
2. Add tests for interrupted runs.
3. Add tests for missing worker and missing master scenarios.
4. Add tests that verify live certificate paths remain unchanged.
5. Add integration tests for rolling master cutover order.

## Open Questions

1. Can Magnum provide a full node inventory directly in heat-params, or must the
   coordinator always snapshot nodes from Kubernetes?
2. Which secret should be used to derive the node API authentication token?
3. Should worker finalization allow a configurable threshold, or should the first
   version stay strict?
4. Should the coordinator run as a long-lived agent only, or can the `bootstrap`
   process temporarily assume coordinator responsibilities while the agent exposes
   status?
5. Which services truly require restart at prepare versus finalize, and which can
   tolerate a simple reload?

## Final Recommendation

Implement CA rotation as a resumable coordinator-driven protocol with explicit
prepare, cutover, and finalize stages.

The key invariants are:

1. Live certificate file names never change.
2. Old and new CA trust overlap before any live leaf cutover.
3. Masters rotate serially.
4. Workers rotate in controlled batches.
5. Final rotation completion is recorded only after old trust is removed safely.

This keeps the deployment compatible with the existing file layout while making
the rotation path safer, more observable, and suitable for production clusters.
