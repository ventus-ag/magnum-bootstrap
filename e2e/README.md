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
| Real OpenStack | `e2e/cmd/magnum-e2e/` (ci.yaml `e2e-openstack` job) | Full Magnum lifecycle **including OCCM LoadBalancer + Cinder CSI** | `self-hosted` runner + OpenStack creds (`OS_*` env) | nightly + label |

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
| `e2e/cmd/magnum-e2e/` | Drives a real Magnum cluster through the full lifecycle via the gophercloud SDK + client-go (no `openstack`/`kubectl` CLIs; one static binary). |

## Running locally

Unit tier (no infra):

```bash
go test ./e2e/...
```

FCoS VM tier (needs KVM + qemu, jq, xz, podman, go, and a magnum_victoria checkout):

```bash
# single node + LB (default): 1 master + 1 lb VM (the two L4 LBs)
VICTORIA_DIR=/path/to/magnum ./e2e/vm/run-fcos-e2e.sh

# multi node: 1 master + 2 workers (exercises worker join + drain/uncordon)
VICTORIA_DIR=/path/to/magnum WORKERS=2 ./e2e/vm/run-fcos-e2e.sh

# multi master: 3 masters behind the api/etcd LBs (exercises etcd LB join +
# the dual-CA rotation barrier across masters)
VICTORIA_DIR=/path/to/magnum MASTERS=3 SCENARIOS="create ca-rotate" ./e2e/vm/run-fcos-e2e.sh

# horizontal scale-up: seed master-0 (1-member etcd) then grow the control plane
# 1 -> MASTERS one node at a time (each new master joins the running cluster),
# asserting the etcd member count climbs by one per added master
VICTORIA_DIR=/path/to/magnum MASTERS=3 SCENARIOS="scale-masters ca-rotate" ./e2e/vm/run-fcos-e2e.sh

# direct no-LB path (= Heat no_master_lb): single master, user-mode net, no lb VM
VICTORIA_DIR=/path/to/magnum MASTER_LB_ENABLED=false ./e2e/vm/run-fcos-e2e.sh
```

`SCENARIOS` is an ordered, repeatable list — `ca-rotate` may appear many times
(each gets a unique rotation id), so
`SCENARIOS="create ca-rotate ca-rotate ca-rotate upgrade ca-rotate"` reproduces
the repeated-operation sequence that exposed the wedge bug. `ca-rotate` asserts
the API server leaf cert content actually changed; `upgrade` asserts the kubelet
version changed; `create` asserts idempotency = **zero** host changes on re-run.
Multi-master adds: each extra master joins the existing etcd through the etcd VIP
and registers Ready; `ca-rotate` is applied to all masters **concurrently** so
the dual-CA barrier's per-master restart Lease is exercised; `assert-etcd-members`
checks the final member count. `scale-masters` is the explicit horizontal
scale-up (`MASTERS>=2`): master-0 first forms a **single**-member etcd and goes
Ready, then each remaining master joins the running cluster through the etcd LB
(op=create, mirroring Magnum, which CREATEs added master servers), with the member
count asserted to climb by exactly one per node (1 → 2 → … → MASTERS).
Every run ends with an **E2E SUMMARY** block — tier, trigger, per-scenario
verdicts, duration, and the live cluster state (`kubectl get nodes/pods`, helm
releases) — also written to `$GITHUB_STEP_SUMMARY` under Actions.

### Load balancers (mirrors Magnum's two Octavia LBs)

By default (`MASTER_LB_ENABLED=true`, like Heat `master_lb_enabled`), every run —
even a single master — stands up a dedicated **`lb` VM** running two L4 TCP load
balancers (`e2e/cmd/mock-lb`):

| LB | heat-params | VIP | port |
|----|-------------|-----|------|
| `api_lb`  | `KUBE_API_PRIVATE/PUBLIC_ADDRESS` | `192.168.<slot>.8` | 6443 |
| `etcd_lb` | `ETCD_LB_VIP`                     | `192.168.<slot>.9` | 2379 |

`mock-lb` is **pass-through** (it never terminates TLS), with TCP health checks —
exactly what Octavia provides for these pools. The VIPs live on the lb node, not
a master, because the apiserver binds `0.0.0.0:6443` (a co-located api VIP would
clash). etcd **peer** traffic (:2380) stays direct node↔node, matching Heat
(`etcd_lb` is :2379 only). `MASTER_LB_ENABLED=false` reverts to the direct no-LB
path (single master only; the simple user-mode network, no lb VM).

Knobs: `KUBE_TAG`, `KUBE_TAG_UPGRADE`, `SCENARIOS="create ca-rotate upgrade"`,
`MASTERS` (1=single master), `WORKERS` (0=no workers), `MASTER_LB_ENABLED`
(true), per-role sizing `MASTER_MEM_MB`/`MASTER_CPUS`, `WORKER_MEM_MB`/`WORKER_CPUS`,
`LB_MEM_MB`/`LB_CPUS` (`VM_MEM_MB`/`VM_CPUS` still work as fallbacks),
`KEEP_VM=1` (leave VMs up for debugging). The cluster uses a QEMU socket/mcast
network; `E2E_SLOT` (or `GITHUB_RUN_ID`) isolates the subnet, mcast group, and
SSH host-ports so multiple runs can share one host — see
[IMPROVEMENTS.md](IMPROVEMENTS.md).

### Nested-virt boot reliability

On a nested KVM host (the runner is itself a VM) — especially **nested AMD-V on
Zen1** — the L2 guest's LAPIC interrupt delivery and clock are unreliable.

**Default: nested AMD-V uses pure-emulation TCG.** In practice KVM on these hosts
freezes the boot right after `basic.target` *even with the full workaround set*
(legacy xAPIC `-cpu host,-x2apic`, `idle=poll`, `pci=nomsi`,
`kernel-irqchip=split`, 1 vCPU). Rather than burn minutes per run on
guaranteed-doomed KVM attempts before falling back, `auto` now switches nested
AMD-V straight to TCG (reliable but slow). Opt back into KVM — with all the xAPIC
workarounds below plus the TCG fallback — via `ALLOW_KVM_ON_NESTED_AMD=1`. The
proper fix is a sound-virt runner (Zen2+/Intel or bare-metal/`--device
/dev/kvm`), where KVM is used normally.

> Note: `kernel-irqchip=off` (full userspace APIC) is **rejected by KVM** on x86
> ("KVM does not support userspace APIC") — it needs an in-kernel LAPIC — so the
> workaround uses `split` (userspace IOAPIC, in-kernel LAPIC); any `off`+KVM is
> auto-coerced to `split`.

When `ALLOW_KVM_ON_NESTED_AMD=1` forces KVM, a boot can still freeze *silently*
(no panic) in the **dracut initramfs**: the vCPU halts in an idle wait and the
timer interrupt that should wake it is lost. Three layers address this, all
auto-enabled when nested AMD-V is detected:

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

Real-OpenStack tier — a single static Go binary (`e2e/cmd/magnum-e2e`); needs
OpenStack creds + a Magnum template running the forked driver, but **no**
`openstack`/`kubectl` CLIs:

The tool reads the **full standard OpenStack RC** (its own auth reader, not
gophercloud's narrow `AuthOptionsFromEnv` — so split-domain RC files work):

```bash
# Application credential (preferred): OS_AUTH_URL + OS_REGION_NAME +
#   OS_APPLICATION_CREDENTIAL_ID / OS_APPLICATION_CREDENTIAL_SECRET
# …or user/password: OS_USERNAME / OS_PASSWORD / OS_USER_DOMAIN_NAME +
#   OS_PROJECT_ID  (or OS_PROJECT_NAME + OS_PROJECT_DOMAIN_ID/NAME)
source ./your-openrc.sh        # a normal `openstack` RC file works as-is

# Deliver the CURRENT build to the nodes by staging it into the cloud's own
# Swift store (public-read, auto-set reconciler_version/url, deleted at the end):
make build                      # current tree -> dist/bootstrap
go run ./e2e/cmd/magnum-e2e \
  -template v1.30.10 -upgrade-template v1.31.6 \
  -bootstrap-binary dist/bootstrap          # <- nodes fetch THIS exact build
  # (KEYPAIR optional — an ephemeral keypair is auto-created/destroyed)

# Alternative delivery: skip -bootstrap-binary and point at a hosted binary
#   -reconciler-version <ver> -reconciler-binary-url https://.../bootstrap

# Modes:
#   -list           list cluster templates (+ reconciler labels) and keypairs
#   -preflight      auth + template check + keypair create→delete round-trip
#   -stage-selftest stage -bootstrap-binary, fetch it back anonymously, verify, unstage
#   -teardown       delete the named cluster (+ ephemeral keypair + staged binary)
# Flags default from env: CLUSTER_TEMPLATE, UPGRADE_TEMPLATE, KEYPAIR, KUBE_TAG,
#   NODE_COUNT, NODE_COUNT_RESIZE, MASTER_COUNT, EXTRA_LABELS, TIMEOUT_MIN,
#   KEEP_CLUSTER, SKIP_CA_ROTATE, RECONCILER_VERSION, RECONCILER_BINARY_URL,
#   BOOTSTRAP_BINARY.
```

It walks: create → smoke (nodes Ready) → cloud-integration (Cinder CSI PVC binds
+ OCCM LoadBalancer Service gets an Octavia external IP) → upgrade → resize →
ca-rotate → delete. The admin kubeconfig is built in-process from the Magnum cert
API (CSR signed against the cluster CA, `CN=admin O=system:masters`); the
cloud-integration checks are the payoff that the FCoS mock tier cannot fake.

> **Reconciler binary delivery.** The node launcher skips entirely unless *both*
> `reconciler_version` and `reconciler_binary_url` resolve. `-bootstrap-binary`
> is the easy path: it uploads the binary (+ `.sha256`) to a per-run public-read
> Swift container, sets both labels to the in-cloud URL, and deletes the
> container at teardown — so nodes always test the exact local build with no
> release or external host to maintain. (Version-pinned templates keep their own
> `kube_tag`, so leave `-kube-tag` unset; the create/upgrade versions come from
> `-template`/`-upgrade-template`.)

## Runner virtualization: KVM vs TCG

The FCoS-VM tier boots a real VM. **How fast (and whether at all) depends on the
runner's nested-virt support.** The harness auto-detects and picks an accelerator
— you usually set nothing — but here's what it does and how to configure it.

### Decision the harness makes (`resolve_qemu_cpu`)

| Runner | Accelerator | Notes |
|--------|-------------|-------|
| Bare metal / Intel / sound nested virt | **KVM** | fast; `-cpu host` |
| **Nested AMD-V without usable AVIC** | **TCG** (auto) | reliable but slow; see below |
| Anything with `QEMU_ACCEL=tcg` | TCG | forced emulation |

**Why nested AMD-V falls back to TCG.** On AMD, correct interrupt delivery to a
nested (L2) guest needs **AVIC** (hardware virtual APIC). AVIC is **mutually
exclusive with nested** on current kernels (and a cloud L1 usually isn't given
AVIC by its L0 host), so `kvm_amd` runs with `avic=N`. Without AVIC the L2's APIC
is software-emulated through the broken nested path and the boot **freezes right
after `basic.target`** — no `-cpu`/`-x2apic`/`idle=poll`/`pci=nomsi`/`irqchip`
combination fixes a missing virtual APIC. So the harness skips the doomed KVM
attempts and goes straight to pure-emulation **TCG**, which has no nested
interrupt virtualization and therefore boots reliably (just slowly).

Check a runner in one line:

```bash
cat /sys/module/kvm_amd/parameters/avic    # 'N' on AMD => nested KVM won't work => TCG
systemd-detect-virt                        # non-'none' => this host is itself a guest (nested)
```

### TCG runs are slow — and that's expected

Full Kubernetes bring-up under software emulation takes **multiples longer**
(tens of minutes per scenario). The harness compensates automatically:

- **MTTCG sizing**: TCG isn't capped to 1 vCPU (that cap is a KVM-only nested
  workaround). On nested AMD it bumps the master to **4 vCPU / 4 GB** and workers
  to 2 vCPU (scaled to host cores), since MTTCG runs each vCPU on its own host
  thread.
- **`WAIT_SCALE`**: in-guest waits (node Ready, core pods, agent deploy) are
  multiplied (×3 under TCG) so a slow run doesn't false-timeout mid-scenario.

### Configuration knobs

| Env | Default | Purpose |
|-----|---------|---------|
| `QEMU_ACCEL` | `kvm` (auto→tcg on nested AMD) | force `tcg` or `kvm` |
| `ALLOW_KVM_ON_NESTED_AMD` | `0` | try KVM on nested AMD anyway (xAPIC workarounds + TCG fallback) — only if your host has working AVIC+nested |
| `ALLOW_SMP_ON_NESTED_AMD` | `0` | keep configured vCPU count on a forced-KVM nested-AMD run |
| `WAIT_SCALE` | `3` on TCG, else `1` | multiply in-guest wait timeouts |
| `MASTER_CPUS`/`MASTER_MEM_MB`, `WORKER_CPUS`/`WORKER_MEM_MB` | TCG: 4/4096, 2/2048 | explicit per-role sizing (override the auto bump) |
| `TCG_FALLBACK` | `1` | when forcing KVM, fall back to TCG if it hangs |

### For fast e2e

Use a runner with real nested virt: **Intel VT-x** nested, **AMD with
AVIC+nested**, or **bare metal** (`--device /dev/kvm` if the runner is a
container). There the harness uses KVM automatically and a full run is minutes,
not tens of minutes.

> The current `ventus`/`magnum-actions01` runner (185.x) is a nested **AMD EPYC**
> L1 guest with `avic=N` — confirmed KVM-incapable for L2, so it runs **TCG**
> (slow but green). It's fine for correctness/PR gating; move to a sound-virt
> runner if turnaround time matters.

## Preparing the self-hosted runner

The FCoS-VM tier needs **nested KVM** (fast) **or** falls back to TCG (slow but
works anywhere) plus qemu/jq/xz/podman. The OpenStack tier needs **nothing** on
the runner beyond the prebuilt `magnum-e2e` static binary (downloaded from the
build artifact) and `OS_*` auth env — no `openstack`/`kubectl` CLIs. (The legacy
`runner-setup.sh --openstack` / `runner-preflight.sh --openstack` helpers remain
for anyone who still wants the CLIs locally, but CI no longer uses them.)

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

**On pull requests** (to `main`/`master`):

- **`ci.yaml` (fast tier) — every PR, required.** Build, `go vet`, `gofmt`,
  `go test -short ./...` (covers `e2e/scenario` incl. the **heat-params contract
  guard**, and `e2e/cmd/mock-magnum`/`mock-heat`), plus `shellcheck` on the e2e
  scripts. The contract guard checks out the forked driver and asserts the
  renderer emits every `write-heat-params*.sh` key; on fork PRs (no PAT) it
  self-skips.
- **`e2e-fcos` — every PR** (open + each push). Boots a real FCoS VM on the
  self-hosted KVM runner and walks create → ca-rotate → upgrade with outcome
  assertions. A `concurrency` group cancels superseded runs; the matrix runs
  serially on one agent. No label needed.
- **`e2e-openstack` — opt-in via the `e2e-openstack` label.** Provisions real,
  **billed** cloud resources (clusters, LBs, volumes), so it stays label-gated.

The self-hosted runner means fork PRs won't get secrets and shouldn't auto-run;
same-repo (trusted) PRs are the intended path.

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

- `e2e-fcos` job — checks out both repos, self-provisions, runs `run-fcos-e2e.sh`.
- `e2e-openstack` job — downloads the prebuilt `magnum-e2e` binary and runs it
  against real OpenStack. Configure via repo **secrets** and **vars**:

  | Setting | Kind | Purpose |
  |---------|------|---------|
  | `OS_AUTH_URL` | secret | Keystone v3 endpoint |
  | `OS_APPLICATION_CREDENTIAL_ID` / `OS_APPLICATION_CREDENTIAL_SECRET` | secret | app-credential auth (preferred) |
  | `OS_USERNAME` / `OS_PASSWORD` | secret | user/password auth (fallback) |
  | `OS_REGION_NAME`, `OS_INTERFACE`, `OS_PROJECT_ID` (or `OS_PROJECT_NAME` + `OS_PROJECT_DOMAIN_ID/NAME`), `OS_USER_DOMAIN_NAME/ID` | var | scope/endpoint selection (full split-domain RC honoured; `OS_PROJECT_ID` is used alone) |
  | `MAGNUM_CLUSTER_TEMPLATE` | var | create-time template name/UUID (forked driver); version-pinned templates carry their own `kube_tag` |
  | `MAGNUM_UPGRADE_TEMPLATE` | var | upgrade-target template (a distinct version-pinned template) |
  | `MAGNUM_KEYPAIR` | var | nova keypair name — **optional**; an ephemeral keypair is auto-created/destroyed if unset |
  | `RECONCILER_VERSION` + `RECONCILER_BINARY_URL` | var | reconciler binary delivery (both required unless baked into the template) |

  No `clouds.yaml`, no OpenStack/kubectl CLIs on the runner.

## How the shared CA fits together

`mock-magnum -gen-ca` writes `ca.crt`/`ca.key`. `scenario-gen` embeds `ca.key`
into `heat-params` as `CA_KEY` (→ `/etc/kubernetes/certs/ca.key`, used by the
controller-manager) and the mock serves `ca.crt` as the cluster CA and signs all
CSRs with it. Cert and key therefore share one CA, so signed leaf certs verify
against the CA the node trusts and the cluster actually comes up — and because
the mock preserves CSR SANs/Subject and grants server+client auth, re-runs
report **zero cert changes** (idempotency).
```
