# Nebius mk8s cluster

Provision a Nebius AI Cloud Managed Kubernetes (mk8s) cluster with CPU and GPU
node groups, and point your local kubeconfig at it.

## What it does

- Creates a virtual network (`nebius_vpc_v1_network`) and a subnet
  (`nebius_vpc_v1_subnet`) inheriting the network's default private/public
  address pools.
- Creates a security group (`nebius_vpc_v1_security_group`) with an SSH
  ingress rule (`TCP/22` from `0.0.0.0/0`) and an egress rule (allow all).
- Creates a `nebius_mk8s_v1_cluster` with a public control-plane endpoint.
- Creates a `cpu` `nebius_mk8s_v1_node_group` (shaped by `cpu_instance_types`),
  plus one `gpu-<name>` group per `gpu_instance_types` key (every node gets
  cloud-init injecting an SSH user + key, same idiom as `computes/nebius`).
- Gives each **GPU** group's nodes a public IPv4 so they can be reached over SSH
  (`public_ip`, default `true`); the CPU group takes none unless
  `cpu_instance_types.public_ip` asks for one. See
  [Public addresses](#public-addresses).
- On every **GPU** node, installs `gpustack-node-prep.service` — a boot-time
  oneshot that moves the image's vendor device-plugin **static Pod** manifest
  out of `/etc/kubernetes/manifests/` and, on MIG-capable groups only, disables
  the DCGM services. Both would otherwise fight GPUStack: the static Pod
  advertises the same accelerator resource the GPUStack device plugin does (and
  the kubelet owns it, so `kubectl delete` cannot remove it), and DCGM holds
  driver handles that make a MIG mode switch fail. It runs on **every** boot,
  not just the first, because the provider reboots a node whose GPU health check
  fails — and putting a card into MIG mode is by itself enough to fail that
  check. DCGM is **masked**, not merely disabled: it was observed running again
  hours after a clean disable, pulled back in by the vendor's own units.
- Turns off auto-repair for the `NebiusGPUError` node condition on **MIG-capable
  GPU** node groups. See [MIG readiness](#mig-readiness) — without this, a
  partitioning test shuts down the node it runs on.
- Buys a GPU node group from **preemptible** capacity when its
  `gpu_instance_types` entry sets `preemptible = true`. See
  [Preemptible nodes](#preemptible-nodes).
- After apply, runs `nebius mk8s cluster get-credentials` to merge the cluster
  into `~/.kube/config` as a new context, which becomes the current one (unless
  `switch_kube_context=false`); on destroy it removes that
  context/cluster/user.

## MIG readiness

Putting a card into a hardware partitioning mode is enough, on its own, to fail
the provider's GPU health check: its NVLink topology probe cannot read a
partitioned card. The failure raises the `NebiusGPUError` node condition, and
the **default auto-repair rule for that condition cordons the node and then
shuts it down** — so a MIG test destroys the node it is running on. The first
symptom is rarely the GPU: unrelated Pods start failing to create, because the
node hosting a webhook went away.

Two things make a node MIG-ready, and both are in the Terraform:

- `auto_repair.conditions` names `NebiusGPUError` with `disabled = true`. The
  condition is still reported and still visible in `kubectl describe node`; it is
  simply no longer node-fatal. Every other auto-repair rule keeps its default.
- `gpustack-node-prep.service` masks `nvidia-dcgm` / `nvidia-dcgm-exporter`,
  which otherwise hold driver handles that make `nvidia-smi -mig 1` fail.

Neither is reversed automatically. If you want the platform's GPU auto-repair
back on a cluster you are done partitioning, drop the `auto_repair` block and
re-apply.

`nvidia-fabricmanager` is deliberately left running — MIG does not require it to
be stopped, and stopping it breaks NVLink for whole-card workloads on the same
node.

### Groups that cannot be partitioned

MIG is a datacenter-SXM feature. An `gpu-l40s-*` or `gpu-rtx6000` card has no
MIG at all, and applying the two preparations above to such a group is pure
damage: it would lose its GPU telemetry for a capability it will never have, and
give up platform auto-repair for a `NebiusGPUError` that on those cards can only
mean the GPU is genuinely broken.

So both are gated on a per-group `mig` flag. It defaults to whether the group's
platform appears in `main.tf`'s `mig_platforms` list (`gpu-h100-sxm`,
`gpu-h200-sxm`, `gpu-b200-sxm`, `gpu-b200-sxm-a`, `gpu-b300-sxm`); anything else
is treated as non-partitionable and keeps DCGM and its platform auto-repair. A
non-MIG group still gets the vendor device-plugin static Pod moved aside —
GPUStack has to own the card either way.

Set `mig` explicitly on a group to override the derivation. Do that as soon as
Nebius adds a partitionable platform this list does not know about: the default
would be `false`, auto-repair would stay on, and the first MIG switch would
**shut the node down**.

## Preemptible nodes

A `gpu_instance_types` entry with `preemptible = true` is provisioned from
preemptible capacity: cheaper, and reclaimable by the platform at any time — the
node can disappear mid-test and its Pods go with it. Use it for a group whose
loss the test can absorb (a second accelerator flavour, a scale-out node), not
for the one hosting the thing under test. The CPU group is always on-demand.

`preemptible` is per group, so a cluster can mix both: below, an on-demand H100
group and a preemptible L40S one.

## Public addresses

A public IPv4 is a **quota'd** resource: it is charged against the project's
`vpc.ipv4-address.public.count` quota, the allowance is small (single digits by
default), and it counts **allocations, not running instances** — a *stopped* VM
still holds its address and still consumes the quota. Exhaust it and node-group
creation fails at apply time with

```
code = ResourceExhausted desc = Quota limit exceeded ... quota vpc.ipv4-address.public.count
```

leaving the group `tainted` in state. Raise the quota (Administration → Limits →
Quotas), release an address, or ask for fewer.

The **flag** is per group; the **address, and so the quota unit, is per node**. A group of N nodes with
`public_ip = true` takes N addresses. Every group this module creates carries `fixed_node_count = 1`, so
today the two readings give the same number — but plan the quota per node, not per group, because that
is what the provider charges. Only the groups that need an address take one:

- **GPU groups: `public_ip = true` (default).** A GPU node is driven from outside
  the cluster over SSH — the hardware-partition tests toggle MIG on the card that
  way, and they take the node's address as a run-time input rather than guess it,
  so a GPU node that cannot be reached cannot be partition-tested.
- **The CPU group: none by default.** The accelerator tests drive GPU nodes, not
  this one. Set `public_ip = true` in `cpu_instance_types` for the one workflow
  that does need inbound reach — building images on the node itself, natively,
  instead of under emulation on a workstation of another architecture.

Set `public_ip = false` on a GPU group nobody has to log in to (a scale-out node,
a second flavour that only has to schedule) to bring the requirement down
further. Do it **before** the group exists: the provider's `public_ip_address` is
optional-and-computed, so flipping the flag on an already-created group plans no
change at all and the node keeps its address — the group has to be replaced
(`terraform taint`, or remove and re-add it) for the release to happen.

**Dropping the address does not cost the node its internet.** The network's
default route table carries a `0.0.0.0/0` route whose next hop is Nebius' default
egress gateway; for a resource with no public address that gateway NATs the
traffic behind a dynamic address from a pool shared across the region. A
private-only node still joins the cluster, pulls images and installs packages.
What it loses is *inbound* reachability — SSH — which is why the flag tracks who
needs to log in and nothing else.

## Prerequisites

1. Install the [Nebius CLI](https://docs.nebius.com/cli), `terraform`, and
   `kubectl`.
2. A Nebius Service Account with an authorized key. Export:
   ```bash
   export NEBIUS_SA_ID="serviceaccount-..."
   export NEBIUS_AUTHKEY_PUBLIC_ID="publickey-..."
   export NEBIUS_AUTHKEY_PRIVATE_PATH="$HOME/.nebius/authkey/private.pem"
   ```
   The same variables authenticate the `nebius` CLI itself (`nebius profile
   create` or equivalent), since the kubeconfig it writes uses an exec
   credential plugin that shells out to `nebius` at `kubectl` time.
3. An SSH public key on disk (default `~/.ssh/id_rsa.pub`) — injected into
   every node via cloud-init. Override the path with
   `-var='ssh_public_key=...'`.
4. A Nebius project ID (`project_id`, e.g. `project-...`) — its region fixes
   node placement and platform availability (see `variables.tf` for the
   region -> platform table).

## Usage

```bash
cd testing/infra/clusters/nebius
terraform init

terraform plan  -var='project_id=project-...'
terraform apply -var='project_id=project-...'

kubectl --context "$(terraform output -raw context_name)" get nodes
kubectl --context "$(terraform output -raw context_name)" get nodes -o wide
# ssh ubuntu@<ExternalIP> for any node in a group with public_ip = true

terraform destroy   # no -var needed -- project_id is carried across from the last apply
```

A successful apply writes `.last-apply.auto.tfvars.json` holding **only**
`project_id`, the one variable with no default, so a destroy (including a retry
after an interrupted one) does not need it on the command line. Terraform
auto-loads any `*.auto.tfvars.json`, on `apply` as readily as on `destroy`, so
nothing that has a default is recorded there: a variable in that file silently
overrides its own default on every later command in this directory, with nothing
on the command line to hint at it. If you ever add to that snapshot, expect a
plain `terraform apply` to keep rebuilding whatever shape it captured.

The default `cpu_instance_types` (`cpu-e2`/`4vcpu-16gb`) and `gpu_instance_types`
(a `h100` entry: `gpu-h100-sxm`/`1gpu-16vcpu-200gb`) provision one `cpu` node and
one `gpu-h100` node. A GPU group needs only `platform` + `preset`; its `os` and
`drivers_preset` are resolved automatically from the compatibility matrix for
`release` (see below). Override `-var='cpu_instance_types={...}'` to reshape the
CPU group, or `-var='gpu_instance_types={...}'` to change, add, or remove GPU
groups; each `gpu_instance_types` map key becomes that group's `gpu-<key>` name.

A three-node cluster with one on-demand, MIG-capable H100 node, one preemptible
L40S node (Intel host CPU, no MIG) and one CPU node:

```bash
terraform apply \
  -var="project_id=$NEBIUS_PROJECT_ID" \
  -var='gpu_instance_types={
          h100 = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" },
          l40s = { platform = "gpu-l40s-a",   preset = "1gpu-8vcpu-32gb", preemptible = true }
        }'
```

Only `project_id` survives in `.last-apply.auto.tfvars.json`, so **re-apply with
the same `-var='gpu_instance_types=...'`** — a bare `terraform apply` falls back
to the single-`h100` default and destroys the `gpu-l40s` group. `terraform
destroy` needs no vars.

Node groups don't expose per-node IPs in Terraform state, so reach individual
nodes via `kubectl ... get nodes -o wide` -> `ssh ubuntu@<ExternalIP>`; the SSH
source CIDR (`0.0.0.0/0`) and SSH username (`ubuntu`) are fixed, matching
`computes/nebius`. Only a group with `public_ip = true` has an `ExternalIP` at all
— the CPU node has none unless `cpu_instance_types.public_ip` asks for one
([public addresses](#public-addresses)).

## Variables

| Variable | Description | Default |
|---|---|---|
| `project_id` | Nebius project ID (required); its region fixes node placement & platform availability | *(required)* |
| `name_prefix` | Prefix for the cluster and its network/subnet/security-group names (a random suffix is appended) | `gpustack-nebius` |
| `release` | Kubernetes version (`<major>.<minor>`) | `1.35` |
| `ssh_public_key` | Path to the SSH public key injected into every node via cloud-init | `~/.ssh/id_rsa.pub` |
| `node_boot_disk_size_gb` | Node boot disk size, in GiB, for every node group | `100` |
| `node_boot_disk_type` | Node boot disk type (`NETWORK_SSD`, `NETWORK_HDD`, `NETWORK_SSD_NON_REPLICATED`, `NETWORK_SSD_IO_M3`) | `NETWORK_SSD` |
| `cpu_instance_types` | Instance type for the CPU node group: `{platform, preset, os, public_ip (optional)}`. `public_ip` defaults to `false`; `true` gives the node an SSH-reachable public IPv4 at one public-address quota unit ([public addresses](#public-addresses)). | `{ platform = "cpu-e2", preset = "4vcpu-16gb", os = "ubuntu24.04" }` |
| `gpu_instance_types` | GPU node groups keyed by group name (each becomes `gpu-<name>`): `{platform, preset, os (optional), drivers_preset (optional), preemptible (optional), mig (optional), public_ip (optional)}`. `os`/`drivers_preset` default to the newest match from the compatibility matrix for `release`; `preemptible` defaults to `false` ([preemptible nodes](#preemptible-nodes)); `mig` defaults to whether the platform supports MIG ([groups that cannot be partitioned](#groups-that-cannot-be-partitioned)); `public_ip` defaults to `true`, so the node is SSH-reachable, at one public-address quota unit per node ([public addresses](#public-addresses)). | `{ h100 = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" } }` |
| `switch_kube_context` | Let `get-credentials` leave this cluster current; `false` restores the previous context | `true` |

## Outputs

| Output | Description |
|---|---|
| `context_name` | The kubectl context merged into `~/.kube/config` |
| `cluster_id` | Nebius mk8s cluster ID |
| `public_endpoint` | Public Kubernetes API endpoint |
| `node_group_names` | Names of the provisioned node groups |
| `ssh_note` | Reminder of how to find and SSH into individual nodes |

## Node-group OS / driver reference

`nebius_mk8s_v1_node_group` has no `image_family` field (unlike a standalone
`computes/nebius` VM) -- the node image is picked from `os` and, for GPU
platforms, `gpu.drivers_preset`. **The supported `drivers_preset` values are
tied to the Kubernetes version, and older CUDA presets are dropped as new
versions land**, so this module resolves both automatically instead of asking
you to track a static table.

### Automatic resolution (`get-compatibility-matrix`)

For each GPU group, a `data.external` runs

```bash
nebius mk8s node-group get-compatibility-matrix \
  --cluster-kubernetes-version <release> --platform <platform>
```

and selects the entry with the **newest** `drivers_preset` (its `os` comes with
it), so you set only `platform` + `preset` per group and `release` on the
cluster. Each `items[]` entry lists an `os`, an optional `drivers_preset`, and
the `compatible_platforms` it applies to. To pin a specific (e.g. older) preset,
set `os` and/or `drivers_preset` on the group to override the auto-pick; run the
command yourself to see the valid combinations.

> **CLI dependency:** because this is a `data.external`, Terraform runs it on
> every `plan`, `apply` **and `destroy`** — the `nebius` CLI (authenticated) and
> `jq` must be on `PATH` whenever you run Terraform against this module, or the
> run (including a teardown) fails.

### Example combinations

| Kubernetes version | `drivers_preset` | `os` | NVIDIA driver |
|---|---|---|---|
| 1.30 | `cuda12` | `ubuntu22.04` | CUDA 12.4 |
| 1.31 | `cuda12` / `cuda12.4` | `ubuntu22.04` | CUDA 12.4 |
| 1.31 | `cuda12.8` | `ubuntu24.04` | 570.x |
| 1.33, 1.34 | `cuda13.0` | `ubuntu24.04` | 580.x |

Applies to `gpu-l40s-a`, `gpu-l40s-d`, `gpu-h100-sxm`, `gpu-h200-sxm`. Note the
break at 1.33: the `cuda12*` presets are **not** implemented for 1.33+ -- only
`cuda13.0` -- so `release = "1.34"` with `cuda12.8` is rejected. (`gpu-b200-sxm*`
use `cuda12.8`/`cuda13.0` on `ubuntu24.04`; check the matrix.)

The older rows are kept for the driver pairing only: **a version near its end of
life is refused outright**, with `spec.control_plane.version - Forbidden: k8s
version <x> is deprecated and cannot be used (end of life: <date>)`, and the
window is generous -- 1.31 was already rejected a month before its 2026-09-01
EOL. The compatibility matrix does **not** catch this (it answers for a
deprecated version just as happily), so the rejection only ever surfaces at
apply time, after the network and security group are created. Raise `release`
and re-apply; the created resources are reused.

### Empty preset / installing drivers manually

Every matrix also exposes an entry with **no** `drivers_preset` (e.g.
`ubuntu24.04` alone): the node boots without GPU drivers and you install the
[NVIDIA GPU Operator](https://docs.nebius.com/kubernetes/gpu/set-up#gpu-drivers-and-other-components)
yourself (this is the path Nebius recommends when you need to customize the
device plug-in, e.g. to enable MIG). **This module does not support that path**:
it always emits `gpu_settings = { drivers_preset = <value> }` for a GPU group,
and the API rejects an empty string with `value is required`. To use the
manual path, extend `main.tf` to omit `gpu_settings` and install the operator
out-of-band; otherwise pass a real preset from the matrix above.

See `variables.tf` for the platform -> region and platform -> preset catalogs
(shared with `computes/nebius`).
