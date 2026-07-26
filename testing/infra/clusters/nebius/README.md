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
  plus one `gpu-<name>` group per `gpu_instance_types` key (each node gets a
  public IP and cloud-init injecting an SSH user + key, same idiom as
  `computes/nebius`).
- On every **GPU** node, installs `gpustack-node-prep.service` — a boot-time
  oneshot that moves the image's vendor device-plugin **static Pod** manifest
  out of `/etc/kubernetes/manifests/` and disables the DCGM services. Both would
  otherwise fight GPUStack: the static Pod advertises the same accelerator
  resource the GPUStack device plugin does (and the kubelet owns it, so
  `kubectl delete` cannot remove it), and DCGM holds driver handles that make a
  MIG mode switch fail. It runs on **every** boot, not just the first, because
  the provider reboots a node whose GPU health check fails — and putting a card
  into MIG mode is by itself enough to fail that check.
- After apply, runs `nebius mk8s cluster get-credentials` to merge the cluster
  into `~/.kube/config` as a new context; on destroy it removes that
  context/cluster/user.

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
# ssh ubuntu@<ExternalIP> for any node

terraform destroy   # no -var needed -- reuses the last apply's variables
```

The default `cpu_instance_types` (`cpu-e2`/`4vcpu-16gb`) and `gpu_instance_types`
(a `h100` entry: `gpu-h100-sxm`/`1gpu-16vcpu-200gb`) provision one `cpu` node and
one `gpu-h100` node. A GPU group needs only `platform` + `preset`; its `os` and
`drivers_preset` are resolved automatically from the compatibility matrix for
`release` (see below). Override `-var='cpu_instance_types={...}'` to reshape the
CPU group, or `-var='gpu_instance_types={...}'` to change, add, or remove GPU
groups; each `gpu_instance_types` map key becomes that group's `gpu-<key>` name.

Node groups don't expose per-node IPs in Terraform state, so reach individual
nodes via `kubectl ... get nodes -o wide` -> `ssh ubuntu@<ExternalIP>`; the SSH
source CIDR (`0.0.0.0/0`) and SSH username (`ubuntu`) are fixed, matching
`computes/nebius`.

## Variables

| Variable | Description | Default |
|---|---|---|
| `project_id` | Nebius project ID (required); its region fixes node placement & platform availability | *(required)* |
| `name_prefix` | Prefix for the cluster and its network/subnet/security-group names (a random suffix is appended) | `gpustack-nebius` |
| `release` | Kubernetes version (`<major>.<minor>`) | `1.31` |
| `ssh_public_key` | Path to the SSH public key injected into every node via cloud-init | `~/.ssh/id_rsa.pub` |
| `node_boot_disk_size_gb` | Node boot disk size, in GiB, for every node group | `100` |
| `node_boot_disk_type` | Node boot disk type (`NETWORK_SSD`, `NETWORK_HDD`, `NETWORK_SSD_NON_REPLICATED`, `NETWORK_SSD_IO_M3`) | `NETWORK_SSD` |
| `cpu_instance_types` | Instance type for the CPU node group: `{platform, preset, os}` | `{ platform = "cpu-e2", preset = "4vcpu-16gb", os = "ubuntu24.04" }` |
| `gpu_instance_types` | GPU node groups keyed by group name (each becomes `gpu-<name>`): `{platform, preset, os (optional), drivers_preset (optional)}`. `os`/`drivers_preset` default to the newest match from the compatibility matrix for `release`; set them to override. | `{ h100 = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" } }` |

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
