# magnum-bootstrap end-to-end tests

These tests run the reconciler the way a real Magnum node does: a real
`/etc/sysconfig/heat-params`, the real launcher + systemd units from
`magnum_victoria`, and the real binary under test. The core idea that keeps it
cheap:

> **An operation is just a different `heat-params` file + a re-run.**
> Role (master/worker) and operation (create/upgrade/resize/ca-rotate) are
> detected entirely from `heat-params` + prior state (`internal/config`). So one
> fixture generator drives every scenario on every tier.

```
heat-params (create)   IS_UPGRADE=false IS_RESIZE=false  CA_ROTATION_ID=""
heat-params (upgrade)  IS_UPGRADE=true   (+ bumped KUBE_TAG)
heat-params (resize)   IS_RESIZE=true
heat-params (ca-rotate) CA_ROTATION_ID=<new value>
```

## Tiers

| Tier | Location | What it proves | Infra | Cadence |
|------|----------|----------------|-------|---------|
| Unit | `e2e/scenario`, `e2e/cmd/mock-magnum` | Fixtures trigger the intended op via the **real** parser; the mock is faithful to the **real** `magnum.Client` | none | every PR (`go test`) |
| FCoS VM | `e2e/vm/` + `.github/workflows/e2e-fcos.yaml` | A real single-node FCoS cluster comes up and converges through create → ca-rotate → upgrade, idempotently | `self-hosted` runner w/ KVM | nightly + manual |
| Real OpenStack | `e2e/openstack/` + `.github/workflows/e2e-openstack.yaml` | Full Magnum lifecycle **including OCCM LoadBalancer + Cinder CSI** | `self-hosted` runner + OpenStack creds | nightly + manual |

### Why two "real" tiers — the mock boundary

The mock (`e2e/cmd/mock-magnum`) implements exactly what the reconciler's cert
path needs: a Keystone token, CA fetch, and CSR signing. That is enough to bring
up the **control plane + flannel + CoreDNS + RBAC**. It is **not** the OpenStack
API: the Cloud Controller Manager and Cinder/Manila CSI need Nova/Neutron/Cinder/
Octavia, which the mock does not (and shouldn't) fake.

So the FCoS-VM tier runs with `CloudProvider=false` (one toggle in
`scenario.Config` that flips cloud-provider + OCCM + Cinder + Manila + Octavia
together). Critically, that also avoids `--cloud-provider=external`, whose
`node.cloudprovider.kubernetes.io/uninitialized` taint would otherwise never
clear without a real OCCM and would block scheduling. **OCCM and Cinder/Manila
CSI are validated only on the real-OpenStack tier.**

## Components

| Path | Role |
|------|------|
| `e2e/scenario/` | Renders production-shaped heat-params (mirrors `write-heat-params*.sh`). Tested against the real `config.Load`. |
| `e2e/cmd/mock-magnum/` | Keystone+Magnum stand-in. Serves one CA and signs CSRs with it; CA key goes into `heat-params` `CA_KEY`. |
| `e2e/cmd/scenario-gen/` | CLI wrapper around `e2e/scenario` so bash and Go share one source of truth; also generates the SA keypair. |
| `e2e/vm/butane.yaml` | Minimal Ignition for the FCoS test node (SSH only — the reconciler installs everything else). |
| `e2e/vm/guest-run.sh` | Runs inside the VM: self-ssh + mock + launcher/units install + apply + assertions. |
| `e2e/vm/run-fcos-e2e.sh` | Host orchestrator: build → fetch FCoS → boot QEMU → provision → walk scenarios. |
| `e2e/openstack/run-magnum-e2e.sh` | Drives a real Magnum cluster through the full lifecycle via the `openstack` CLI. |

## Running locally

Unit tier (no infra):

```bash
go test ./e2e/...
```

FCoS VM tier (needs KVM + qemu, jq, xz, podman, go, and a magnum_victoria checkout):

```bash
# single node (default)
VICTORIA_DIR=/path/to/magnum ./e2e/vm/run-fcos-e2e.sh

# multi node: 1 master + 2 workers (exercises worker join + drain/uncordon)
VICTORIA_DIR=/path/to/magnum WORKERS=2 ./e2e/vm/run-fcos-e2e.sh
```

Knobs: `KUBE_TAG`, `KUBE_TAG_UPGRADE`, `SCENARIOS="create ca-rotate upgrade"`,
`WORKERS` (0=single node), per-role sizing `MASTER_MEM_MB`/`MASTER_CPUS` and
`WORKER_MEM_MB`/`WORKER_CPUS` (`VM_MEM_MB`/`VM_CPUS` still work as fallbacks),
`KEEP_VM=1` (leave VMs up for debugging). Multi-node uses a QEMU socket/mcast
cluster network — see [IMPROVEMENTS.md](IMPROVEMENTS.md); it's a first cut to
validate on the runner.

### Nested-virt boot reliability

On a nested KVM host (the runner is itself a VM) — especially **nested AMD-V on
Zen1** — the L2 guest's LAPIC interrupt delivery and clock are unreliable. `auto`
already forces legacy xAPIC + 1 vCPU (`-cpu host,-x2apic`) and userspace irqchip
(`kernel-irqchip=off`), but a boot can still freeze *silently* (no panic) in the
**dracut initramfs**: the vCPU halts in an idle wait and the timer interrupt that
should wake it is lost. Three layers address this, all auto-enabled when nested
AMD-V is detected:

- `INJECT_KARGS=1` / `FIRSTBOOT_KARGS` — bakes `idle=poll nox2apic no-kvmclock
  tsc=reliable pci=nomsi` into the image's BLS boot entry (via `qemu-nbd`)
  **before** the first boot. `idle=poll` keeps the vCPU from ever halting (no lost
  timer-wake); `pci=nomsi` forces devices onto legacy INTx interrupts (delivered
  when MSI-X is lost — else the boot stalls in dracut waiting for the disk). This
  is needed because the freeze is *in the initramfs*, before Ignition — so butane
  `kernel_arguments` (which apply only after firstboot) are too late. Best-effort:
  needs passwordless sudo + the `nbd` module; on failure it warns and boots
  unmodified.
- `BOOT_RETRIES` — extra boot attempts (auto: **1** on nested AMD, 0 otherwise).
  Early freezes are non-deterministic, so a stalled VM is killed and retried once
  on a fresh overlay (kargs re-injected each time).
- `BOOT_STALL_SECS` — console-idle seconds that flag a silent hang (default 120).
  Steady output (even slow TCG) keeps resetting the timer, so it only fires on a
  true freeze, not a slow boot.

- `TCG_FALLBACK=1` (default) — if KVM still hangs in early boot after the retries
  (broken nested interrupt *delivery* that no karg/QEMU flag can fix), the harness
  automatically re-boots the node under **pure-emulation TCG** (`qemu64`, longer
  timeouts). TCG has no nested interrupt virtualization at all, so it boots where
  KVM's lost device/timer IRQs freeze it — much slower, but reliable. Good hosts
  never reach it (KVM wins on the first attempt). Set `TCG_FALLBACK=0` to fail
  instead. Failures that *reached multi-user* (an alive-but-unreachable guest —
  network/Ignition, which TCG can't fix) do **not** trigger the fallback.

If even the TCG fallback fails, the host is the limit: move the tier to a runner
with sound nested virt (Zen2+/Intel or bare-metal/`--device /dev/kvm`). Note the
full k8s e2e under TCG is slow — fine to prove the logic, but a real runner is the
proper home for this tier.

### Trigger mode: `direct` vs `agent`

`TRIGGER` controls *how* the reconciler is invoked on each node:

- **`direct`** (default) — the harness places `heat-params` and runs the
  `magnum-reconcile.service` systemd unit. Fast; exercises the runtime path.
- **`agent`** — runs the **real `heat-container-agent`** (the same
  `openstackmagnum/heat-container-agent` image Magnum uses) against a tiny
  **mock Heat** (`e2e/cmd/mock-heat`). The agent fetches a real
  `OS::Heat::SoftwareDeployment` via `os-collect-config`'s `request` collector,
  whose `config` is the **four real bootstrap scripts** from the cloned
  `ventus-ag/magnum` repo (`write-heat-params*.sh` → `install-reconciler-*.sh`
  → `run-reconciler-once.sh`) and whose `inputs` are the ~90 Heat params; it then
  POSTs the `HEAT_SIGNAL` back. This is the highest-fidelity path — it exercises
  `write-heat-params*.sh`, the install scripts, `run-reconciler-once.sh`, and the
  Heat success/`error_output` signal contract, end to end.

```bash
TRIGGER=agent VICTORIA_DIR=/path/to/magnum ./e2e/vm/run-fcos-e2e.sh
# knobs: AGENT_IMAGE (default openstackmagnum/heat-container-agent:victoria-stable-1),
#        HEAT_LISTEN (VM-local mock Heat, default 127.0.0.1:9512),
#        AGENT_DEPLOY_TIMEOUT (per-deployment wait, default 900s)
```

mock Heat contains **no bootstrap logic** — it only serves the deployment
metadata and records the signal (it's the Heat metadata/`HEAT_SIGNAL`
transport). The scripts, the agent, mock Magnum's CSR signing, and the
reconciler binary are all real. Note: the idempotency re-run is `direct`-only —
Heat re-runs only on a *new* deployment id, and `55-heat-config` skips an
already-deployed id, so a same-id replay isn't a meaningful agent test.

Real-OpenStack tier (needs OpenStack creds + a Magnum template running the forked driver):

```bash
export OS_CLOUD=e2e            # or OS_AUTH_URL + app credential
export CLUSTER_TEMPLATE=...    # existing Magnum template (forked driver)
export KEYPAIR=...
./e2e/openstack/run-magnum-e2e.sh
# knobs: KUBE_TAG, KUBE_TAG_UPGRADE, NODE_COUNT, NODE_COUNT_RESIZE,
#        KEEP_CLUSTER=1, SKIP_CA_ROTATE=1, RECONCILER_BINARY_URL=...
```

## Preparing the self-hosted runner

The FCoS-VM tier needs **nested KVM** plus qemu/jq/xz/podman; the OpenStack tier
needs the `openstack` CLI + kubectl.

**Curl install (one shot — clones both repos + provisions):** the repos are
private, so pass a PAT:

```bash
curl -fsSL -H "Authorization: Bearer $RW_PAT_TOKEN" \
  https://raw.githubusercontent.com/ventus-ag/magnum-bootstrap/main/e2e/vm/install.sh \
  | sudo RW_PAT_TOKEN="$RW_PAT_TOKEN" bash -s -- --openstack
```

**Or, from a clone**, provision once with:

```bash
sudo ./e2e/vm/runner-setup.sh                # FCoS VM tier (installs Go too)
sudo ./e2e/vm/runner-setup.sh --openstack    # also OpenStack tooling
```

`runner-setup.sh` is distro-aware (apt/dnf/yum/pacman/zypper), idempotent, and:
installs the tools, enables nested KVM + adds the runner user to the `kvm` group,
installs `kubectl`, pre-caches the Fedora CoreOS image and the butane container,
then runs the preflight. **Restart the runner service afterwards** so the new
`kvm` group membership takes effect.

Verify any runner (read-only, also the first CI step):

```bash
./e2e/vm/runner-preflight.sh              # VM tier
./e2e/vm/runner-preflight.sh --openstack  # OpenStack tier
```

Notes for the self-hosted runner:
- If the runner executes inside a container (as the gotham-infra deploy jobs do),
  the FCoS tier needs the container started with `--device /dev/kvm` and the host
  must have nested virt enabled — otherwise dedicate a non-containerized,
  VM-capable runner (e.g. label it and switch `runs-on`).
- The matrix runs two jobs (`pinned`, `latest`); a single runner agent executes
  them serially (one VM at a time). Size for two VMs only if you run multiple
  agents.

## CI

Both workflows target a `self-hosted` runner, run nightly + on demand,
and include a **`latest`** matrix entry that resolves the newest stable
Kubernetes (`dl.k8s.io/release/stable.txt`) to catch upstream/chart drift early.

**On pull requests** the heavy tiers are **opt-in via a label** (they boot real
VMs / provision real cloud, so they must not run on every push):

- Add the **`e2e`** label to a PR → `e2e-fcos` runs (re-runs on each push while
  the label is present; a `concurrency` group cancels superseded runs).
- Add the **`e2e-openstack`** label → `e2e-openstack` runs (rare — real, paid
  cloud resources).

The **fast** e2e checks need no label: the existing `ci.yaml` runs
`go test -short ./...` on every PR, which covers `e2e/scenario` (fixtures trigger
the right op) and `e2e/cmd/mock-magnum` (mock fidelity). Note the self-hosted
runner means fork PRs won't get secrets and shouldn't auto-run; same-repo
(trusted) PRs are the intended path.

**The workflows self-provision** — a `Provision runner` step runs
`runner-setup.sh` (idempotent: a no-op once the host is set up) so a fresh runner
configures itself with no manual step. Disable it by setting the repo/org
variable `E2E_SELF_PROVISION=false` (e.g. if you pre-provision). It needs
passwordless `sudo` for the runner user; the FCoS step also opens `/dev/kvm` for
the current job since a just-added `kvm` group won't apply mid-session.

Checkout of the two repos:
- `magnum-bootstrap` — the current repo, default `GITHUB_TOKEN`.
- the forked driver — `repository: ventus-ag/magnum` (override with
  `vars.MAGNUM_REPO`), `ref: victoria-ca-rotation` (override with the
  `magnum_ref` dispatch input or `vars.MAGNUM_REF`), checked out to
  `magnum_victoria/`, using `secrets.RW_PAT_TOKEN` for cross-repo private access.

- `e2e-fcos.yaml` — checks out both repos, self-provisions, runs `run-fcos-e2e.sh`.
- `e2e-openstack.yaml` — needs `secrets.OS_CLOUDS_YAML`,
  `vars.MAGNUM_CLUSTER_TEMPLATE`, `vars.MAGNUM_KEYPAIR`.

## How the shared CA fits together

`mock-magnum -gen-ca` writes `ca.crt`/`ca.key`. `scenario-gen` embeds `ca.key`
into `heat-params` as `CA_KEY` (→ `/etc/kubernetes/certs/ca.key`, used by the
controller-manager) and the mock serves `ca.crt` as the cluster CA and signs all
CSRs with it. Cert and key therefore share one CA, so signed leaf certs verify
against the CA the node trusts and the cluster actually comes up — and because
the mock preserves CSR SANs/Subject and grants server+client auth, re-runs
report **zero cert changes** (idempotency).
```
