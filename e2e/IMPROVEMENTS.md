# e2e suite — improvement plan

Prioritized by value-per-effort. P0 = do next (biggest coverage gap + cheap
high-signal assertions). Effort: S/M/L.

## P0 — biggest gap + cheap signal

| Item | Why | Effort | Status |
|------|-----|--------|--------|
| **Multi-node (1 master + N workers)** with per-role VM sizing (`MASTER_MEM_MB/CPUS`, `WORKER_MEM_MB/CPUS`) | Unlocks what single-node can't: **worker join, real resize, drain/uncordon during upgrade, etcd**. Fits an 8/32 host (master 4 GB + workers 2 GB). | M | in progress (first cut; needs on-runner validation) |
| **Assert outcomes, not just success** | After upgrade assert `node.status.kubeletVersion` actually changed; after ca-rotate assert the API cert **serial/notBefore changed**; idempotency = parse result JSON for **zero changes**, not just status. | S | todo |
| **CI lint gate** | `shellcheck` on `e2e/**/*.sh`, `gofmt -l`/`go vet` for e2e in `ci.yaml`; make the fast e2e unit tests a **required** PR check. | S | todo |

## P1 — fidelity + reliability

| Item | Why | Effort |
|------|-----|--------|
| **Drift / periodic self-heal** | Tamper a file / stop a service → `run-periodic` → assert convergence. Exercises the timer path + RestartTracker. | S |
| **heat-params contract guard** | Test that `scenario-gen` keys match the driver's `write-heat-params*.sh` keys so our renderer can't silently drift; optionally one full-fidelity run using the driver's own scripts. | S |
| **Real workload smoke** | nginx + ClusterIP Service + DNS lookup on the FCoS tier; `sonobuoy --mode quick` on the OpenStack tier. | M |
| **Boot from the real Magnum Ignition** (`fcct-config.yaml`) | Node base (SELinux, sysctls, flannel link) matches production. | M |
| **Local image/chart cache or pull-through mirror** | Cuts bring-up time, removes upstream (googleapis/ghcr/helm) flakiness. | M |
| **Snapshot post-`create` qcow2** | Branch upgrade/ca-rotate from the snapshot instead of re-bootstrapping. | M |
| **Richer failure diagnostics** | Bundle `journalctl` for kubelet/etcd/containerd + the pulumi log into the artifact. | S |

## P2 — breadth

| Item | Why | Effort |
|------|-----|--------|
| **HA 3-master tier** | Real etcd quorum, leader election, rolling control-plane upgrade. | L |
| **True CA-change rotation** | Mock serves a *new* CA + CA_KEY; validates real CA material change and the dual-CA trust model (see `caimprove.md`). | M |
| **Ubuntu node tier** | Once the Ubuntu templates are updated; needs cloud-init instead of butane. | M |
| **Negative tests** | Bad heat-params → clean failure with the right Heat error codes. | S |
| **Mock fault injection + metadata mock** | 500 on first CSR (retry); a 169.254.169.254 mock for `ResolveNodeIP` fallback. | S |
| **DRY workflows + PR summary** | Reusable workflow for resolve/provision; scenario/timing table to `$GITHUB_STEP_SUMMARY`. | S |

## Suggested order

P0 multi-node → P0 assertions → P0 lint gate → P1 drift + contract guard +
workload smoke. That roughly doubles real coverage (join/resize/drain + self-heal
+ meaningful asserts) before the heavier HA/Ubuntu tiers.

## Multi-node networking (first-cut design)

QEMU user-mode networking isolates each VM, so multi-node uses **two NICs per
node**:

1. **NAT NIC** (user-mode) — outbound internet + per-VM SSH host-forward.
2. **Cluster NIC** (`-netdev socket,mcast=...`) — a shared L2 segment across all
   VMs with no host bridge / root required. Static IPs are assigned by a
   MAC-matched NetworkManager keyfile baked into Ignition
   (`192.168.77.10` master, `.20+` workers).

`KUBE_NODE_IP` / API / etcd use the cluster IP; the mock Magnum runs on the
master's cluster IP so workers can reach it. Single-node (`WORKERS=0`) keeps the
original single-NIC user-mode path unchanged. **This inter-VM path is untested in
CI yet and must be validated on the runner.**
