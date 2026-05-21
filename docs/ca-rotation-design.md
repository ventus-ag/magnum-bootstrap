# CA Rotation Design (Dual-CA, Kubernetes-coordinated)

## Summary

This document describes the production CA rotation design for `magnum-bootstrap`.

The previous implementation performed a **hard certificate swap** on every node
(fetch new CA → generate new leaf certs → overwrite live cert dir → restart all
services). Because all nodes are updated in one Heat batch and every service
restarts at once, a node briefly trusts only the new CA while peers still
present old-CA certs, causing a short total outage and, in unlucky orderings,
quorum loss.

The new design replaces the hard swap with a **dual-CA, three-barrier rolling
protocol** coordinated through the **Kubernetes API** (etcd-backed), with a
cluster-wide restart **Lease** so the control plane never goes fully offline.
Heat is unchanged: it still fires a single all-at-once `ca_rotation_id` update.
All coordination and convergence logic lives in the reconciler.

### Compatibility (unchanged from before)

1. Live certificate **file names never change**.
2. Consumers keep reading the same canonical paths under
   `/etc/kubernetes/certs` and `/etc/etcd/certs`.
3. Dual trust is achieved by changing **file content** (`ca.crt`,
   `service_account.key`), never by adding new file names that services read.
4. Temporary material lives only under
   `/var/lib/magnum/ca-rotation/<rotation-id>/`.

## Background: what Magnum actually does on rotation

Tracing `magnum_victoria`:

1. `conductor/handlers/ca_conductor.rotate_ca_certificate`
   - keeps `old_ca_cert_ref` only in a local variable (synchronous rollback),
   - calls `generate_certificates_to_cluster()` which **mints a brand-new CA**
     and **overwrites `cluster.ca_cert_ref`**,
   - deletes client files, then calls the driver.
2. `drivers/heat/driver.rotate_ca_certificate`
   - mints one `ca_rotation_id`, new service-account keys, optional `ca_key`,
   - sets `update_max_batch_size = largest nodegroup` (**all-at-once**),
   - directly updates **every** child stack so masters and workers rotate
     simultaneously (bypassing the ResourceGroup rolling update).
3. `ca_rotation_id` flows `kubecluster → kubemaster/kubeminion →
   write-heat-params*.sh → /etc/sysconfig/heat-params`.

**Decisive constraint:** after step 1, Magnum serves **only the new CA**
(`GET /certificates/{uuid}` → `FetchCACert`) and signs every new CSR with the
new CA. **The old CA survives only on each node's disk**
(`/etc/kubernetes/certs/ca.crt`). Therefore the dual-trust bundle must be built
from:

- **new CA** — fetched from Magnum, and
- **old CA** — read from the node's existing live `ca.crt`.

Magnum can never give the old CA back. This is why the reconciler — the only
component that still holds the old CA — must own the rotation.

## Why coordinate through the Kubernetes API

The barriers below require a cluster-wide "may I advance?" signal that **both
masters and workers** can reach. Options considered:

| Substrate | Reachable by workers? | New surface | Notes |
|-----------|----------------------|-------------|-------|
| Node-to-node mesh + election + HMAC | yes | high (new port, auth, election) | the rejected heavyweight design |
| etcd directly | **no** (workers have no etcd access) | medium | masters only |
| **Kubernetes API (etcd-backed)** | **yes** (apiserver proxies etcd) | low (reuses auth + RBAC) | chosen |
| Heat / Magnum | n/a | — | explicitly kept out of node-local logic |

The Kubernetes API is the only consistent, HA store **all** nodes can already
reach with existing credentials. Masters use `admin.conf` (`system:masters`);
workers use the node-identity `admin.conf` (`system:node:<name>`). It is
etcd-backed, so all nodes effectively coordinate through etcd **via the
apiserver**.

### The reflexivity problem and why it is safe

The coordination store is secured by the very certs being rotated. The protocol
survives this because:

1. **Prepare widens trust before anything else changes.** After prepare every
   client (`kubectl`, `--client-ca-file`, etcd `--peer-trusted-ca-file`) trusts
   **both** CAs, so the API and etcd quorum keep working when leaves swap.
2. **Every phase boundary is a complete, working state.** The ConfigMap answers
   only "may I advance?"; "where am I?" comes from local `state.json`. An API
   blip therefore **pauses** the rotation at a safe phase — it never tears state
   or loses progress. The node retries; the next periodic run resumes.
3. **A cluster-wide Lease prevents a full API outage.** Masters acquire a
   `coordination.k8s.io/v1` Lease before restarting control-plane services, so
   only one master restarts at a time and ≥1 apiserver always serves.
4. **Workers never touch etcd.** etcd rotation is entirely a master-stage
   concern; workers only rotate kubelet/kube-proxy leaves and read desired
   phase.
5. **Escape hatch.** If the API never recovers, the node times out, signals
   Heat, and stays in the last safe phase. A manual `--ca-rotation-phase`
   override lets an operator step a wedged cluster by hand.

## Trust model: three barriers (and why two is not enough)

A naive "bundle + new leaf in one wave" is **not** rolling-safe: during the
window, an updated node presents a new-CA leaf to a not-yet-updated node that
still trusts only the old CA → TLS rejection. Safe rolling needs three barriers,
each separating a trust change from a leaf change:

| Stage | `ca.crt` content | live leaf certs | SA verify keys | SA signing key |
|-------|------------------|-----------------|----------------|----------------|
| (start) | old | old | old | old |
| **prepare** | **new + old** (bundle) | old | **old + new** | old |
| **cutover** | new + old (bundle) | **new** | old + new | **new** |
| **finalize** | **new** | new | **new** | new |

Why each transition is rolling-safe:

- **prepare**: presenter still shows old leaf; every verifier always had old. ✔
- **cutover**: presenter shows new (or old) leaf; every verifier trusts both
  (requires **all** nodes finished prepare). ✔
- **finalize**: all leaves are already new; dropping old trust is safe
  (requires **all** nodes finished cutover). ✔

Bundle order is `new` **then** `old`, so single-cert readers
(`certutil.loadCertificate`, controller-manager `--cluster-signing-cert-file`)
pick the new CA.

### Barrier strictness

Both barriers are **strict for every node** (masters and workers): you cannot
cut over until all nodes are prepared, and cannot finalize until all nodes have
cut over. A dead node halts progress at a safe phase (bundle trust + correct
leaf = fully functional). v1 does not batch or partial-finalize; finalizing
while any node still presents an old leaf would break every peer.

## Service-Account key rotation (rides the same barriers)

Magnum rotates SA keys on every CA rotation, so they must phase identically or
live SA tokens get rejected mid-rotation. SA keys are content-swapped on the
canonical files (flags in `kube-master-config` already point at them):

- `service_account.key` (verify, public): `old+new` through cutover → `new` at
  finalize. kube-apiserver loads **all** keys from `--service-account-key-file`,
  so a concatenated file means tokens signed by either key validate.
- `service_account_private.key` (sign, private): `old` until cutover → `new` at
  cutover. Switched together with the leaf cutover so newly minted tokens use
  the new key only once everyone can verify it.

`old` SA material is the current on-disk content; `new` comes from heat-params
(`KUBE_SERVICE_ACCOUNT_KEY` / `KUBE_SERVICE_ACCOUNT_PRIVATE_KEY`).

## etcd (master-only)

etcd uses the same cert dir, so it rotates with the masters:

- `/etc/etcd/certs/ca.crt` carries the same bundle/new content as the kube CA.
- etcd `trusted-ca-file` and `peer-trusted-ca-file` already point at
  `…/ca.crt`, so they pick up the bundle automatically.
- etcd server/peer **leaf** certs swap at cutover, serialized under the Lease so
  quorum is preserved while masters restart one at a time.

### Quorum safety across cluster sizes

A master is not considered healthy after restart — and therefore does not
release the restart Lease — until its etcd has rejoined a working quorum. This
is verified directly with `etcdctl endpoint health` against etcd's always-present
local plaintext listener (`http://127.0.0.1:2379`), which performs a
linearizable read and so only succeeds when quorum is present. The check needs
no certs, so it is reliable even while TLS material is changing.

Combined with the Lease (only one master restarts at a time), this gives:

| Masters | One etcd restarting | Result |
|---------|---------------------|--------|
| 1 | quorum is just itself | brief API outage during its own restart (inherent to single-master) |
| **2** | 1 of 2 up → no quorum | **brief** unavailability per restart; the quorum gate waits for rejoin before the next master, so the two restarts never overlap and no data is lost |
| 3 / 5 | majority still up | quorum maintained throughout |

The 2-master window is inherent to a 2-node etcd (no fault tolerance) and lasts
only as long as one etcd process restart; running pods are unaffected. The
protocol handles it as safely as physically possible: serialized restarts plus a
quorum-rejoin gate.

A **pre-flight quorum check** also runs before a *fresh* rotation begins on a
master: if etcd is not already in a healthy quorum, the rotation refuses to
start (before mutating anything) rather than risk a control plane that cannot
converge. The check is skipped on resume so an in-progress rotation whose etcd
is momentarily re-forming is not aborted.

### Restart Lease lifetime

The Lease deliberately has a long duration and no renewal loop. A master
restarts the very apiserver its admin.conf points at (`127.0.0.1`), so it cannot
renew the Lease through the API during its own restart; a renewal loop could let
the Lease lapse mid-restart and allow a second master to restart too — breaking
quorum. Erring long instead guarantees mutual exclusion can never be violated;
the only cost is that a crashed holder is recovered after the duration elapses
(the rotation simply pauses in a safe phase until then).

## Per-node staging and state

```text
/var/lib/magnum/ca-rotation/<rotation-id>/
  state.json            # local protocol state (source of truth for "where am I")
  old/   ca.crt service_account.key service_account_private.key <leaf certs…>
  new/   ca.crt service_account.key service_account_private.key <leaf certs…>
  bundle/ ca.crt        # new + old
```

`state.json` schema:

```json
{
  "rotationId": "rotate-123",
  "role": "master",
  "instance": "cluster-master-0",
  "phase": "prepare|cutover|finalize|done",
  "caMode": "old|bundle|new",
  "leafMode": "old|new",
  "saVerifyMode": "old|bundle|new",
  "saSignMode": "old|new",
  "updatedAt": "2026-05-21T10:00:00Z"
}
```

Rules:

- `old/` is snapshotted from live certs **once**, at the start of prepare, and
  reused by later phases (live `ca.crt` is already a bundle by cutover, so the
  pure old/new copies must be preserved).
- `LastCARotationID` (reconciler state) and the legacy
  `/var/lib/magnum/last_ca_rotation_id` marker are written **only after
  finalize** succeeds. Until then a re-run resumes from `state.json`.

## Coordination objects (Kubernetes API)

| Object | Holds | Writers | Readers |
|--------|-------|---------|---------|
| ConfigMap `kube-system/magnum-ca-rotation` | `{rotationId, desiredPhase}` | any master (forward-only, `RetryOnConflict`) | all nodes |
| Node annotation `magnum.openstack.org/ca-rotation` | `<phase>@<rotationId>` per node | each node on **its own** Node | masters (list) |
| Lease `kube-system/magnum-ca-rotation-restart` | control-plane restart mutex | masters | masters |

- **Desired phase** advances `prepare → cutover → finalize` (monotonic).
  Advancement is idempotent — concurrent identical patches by multiple masters
  are harmless; `RetryOnConflict` handles `resourceVersion` races.
- **Status reporting** is distributed: each node patches an annotation on its
  own Node object (allowed by the Node authorizer / NodeRestriction), avoiding
  contention on a single ConfigMap.
- **RBAC:** the `cluster-rbac` module (master-0) grants `system:nodes` `get`/
  `list`/`watch` on the single coordination ConfigMap. Self-Node annotation and
  master Lease access need no extra rules.

## Execution model

Heat fires one all-at-once `ca_rotation_id` update; every node's `run-once`
executes the protocol concurrently and **blocks at each barrier**:

```
prepare:
  snapshot live → old/;  fetch new CA → new/;  write bundle/ca.crt
  write live ca.crt = bundle (kube + etcd);  SA verify = old+new
  (Lease) restart trust-reloading services in dependency order;  health
  annotate Node "prepare@id"
  [master] if all nodes report prepare@id → advance desiredPhase=cutover
  wait until desiredPhase == cutover   (retry across transient API loss)

cutover:
  write live leaf certs = new/;  SA signing key = new
  (Lease) restart leaf-consuming services in dependency order;  health
  annotate Node "cutover@id"
  [master] if all nodes report cutover@id → advance desiredPhase=finalize
  wait until desiredPhase == finalize

finalize:
  write live ca.crt = new;  SA verify = new (kube + etcd)
  (Lease) restart trust-reloading services;  health
  annotate Node "finalize@id"
  patch workloads (best-effort);  write LastCARotationID + legacy marker
  clean staging
```

Barrier waits are bounded (`finalizeTimeout`, default generous). On timeout the
node leaves the cluster in its current safe phase and reports failure to Heat.

Heat's `update_timeout` (driver `_get_update_timeout`) must comfortably exceed a
full three-barrier rotation across the slowest node; this is the only
magnum_victoria-side consideration.

## Module and phase changes

The single `ca-rotation` phase keeps its catalog slot (#2), but its `Run()` is
rewritten to drive the staged protocol via a new coordination package:

```
internal/carotation/
  phase.go    # Phase/CAMode/LeafMode constants
  state.go    # per-node state.json load/save, staging layout helpers
  coord.go    # client-go coordinator: desiredPhase CAS, Node status, Lease,
              # barrier wait, "all nodes reported" check
```

`--ca-rotation-phase <prepare|cutover|finalize>` (app flag) forces a single
stage without coordination, for manual recovery.

Dependency note: modules that assume a single stable CA must not rewrite
`ca.crt` or leaf certs while a rotation is in progress. `master-certificates`
already skips a healthy `ca.crt` (`CertFileNeedsRefresh` only refetches when
missing/expired), so a valid bundle is preserved across normal reconciles; the
ca-rotation module owns all cert writes during an active rotation.

## Failure handling (fail-safe states)

Safe states: `old/old`, `bundle/old`, `bundle/new`, `new/new`.
Unsafe state (never entered): new leaf while any peer trusts only old CA.

- prepare fails on any node → stop; cluster is `old/old` or `bundle/old`.
- cutover fails on a master → stop, do not finalize; cluster is `bundle/*`.
- a node is unavailable → barrier blocks; cluster stays in dual-trust until it
  returns or an operator intervenes.
- coordinator/master churn → any master can advance via the idempotent,
  forward-only ConfigMap CAS; no election to recover.
- API unreachable → retry; resume from `state.json` on recovery / next run.

## Implementation plan

1. `internal/carotation` package: phases, state.json, client-go coordinator
   (in-cluster config from `admin.conf`, ConfigMap CAS, Node annotations,
   Lease, bounded barrier wait).
2. Rewrite `ca-rotation/module.go`: prepare/cutover/finalize, staging snapshot,
   bundle build, SA dual-key, etcd bundle, Lease-serialized restarts, per-stage
   health.
3. `cluster-rbac`: Role/RoleBinding for `system:nodes` on the coordination
   ConfigMap.
4. Reconciler/state: write `LastCARotationID` + legacy marker only after
   finalize.
5. `--ca-rotation-phase` manual override flag.
6. go.mod: add `k8s.io/client-go` (already linked via pulumi-kubernetes).
7. magnum_victoria: ensure `update_timeout` covers a full rotation.
8. Tests: phase transitions, bundle ordering, resume-from-state, missing-node
   barrier, "live file names unchanged".

## Invariants

1. Live certificate file names never change.
2. New + old CA trust overlaps before any leaf cutover, and persists until every
   node has cut over.
3. Masters restart serially under a cluster-wide Lease; quorum is preserved.
4. Coordination is through the Kubernetes API; an outage pauses (never
   corrupts) the rotation.
5. Final completion is recorded only after old trust is safely removed.
