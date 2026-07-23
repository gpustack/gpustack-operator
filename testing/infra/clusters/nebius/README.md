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
(a `h100` entry: `gpu-h100-sxm`/`1gpu-16vcpu-200gb`, CUDA 12.8) provision one
`cpu` node and one `gpu-h100` node. Override `-var='cpu_instance_types={...}'`
to reshape the CPU group, or `-var='gpu_instance_types={...}'` to change,
add, or remove GPU groups; each `gpu_instance_types` map key becomes that
group's `gpu-<key>` name.

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
| `gpu_instance_types` | GPU node groups as a `map(object)` keyed by group name (each becomes `gpu-<name>`): `{instance_type {platform, preset}, os, gpu {drivers_preset}}` | `{ h100 = { instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }, os = "ubuntu24.04", gpu = { drivers_preset = "cuda12.8" } } }` |

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
platforms, `gpu.drivers_preset`, per this platform/Kubernetes-version matrix:

| Platform(s) | `drivers_preset` | Kubernetes version | `os` |
|---|---|---|---|
| `cpu-e1`, `cpu-e2`, `cpu-d3`, `gpu-l40s-a`, `gpu-l40s-d`, `gpu-h100-sxm`, `gpu-h200-sxm` | `""` | 1.30 | `ubuntu22.04` |
| (same) | `""` | 1.31 | `ubuntu22.04` (default), `ubuntu24.04` |
| `gpu-l40s-a`, `gpu-l40s-d`, `gpu-h100-sxm`, `gpu-h200-sxm` | `cuda12` (CUDA 12.4) | 1.30, 1.31 | `ubuntu22.04` |
| (same) | `cuda12.4` | 1.31 | `ubuntu22.04` |
| (same) | `cuda12.8` | 1.31 | `ubuntu24.04` |
| `gpu-b200-sxm` | `""`, `cuda12` (CUDA 12.8), `cuda12.8` | 1.30/1.31 (see provider docs) | `ubuntu24.04` |
| `gpu-b200-sxm-a` | `""`, `cuda12.8` | 1.31 | `ubuntu24.04` |

See `variables.tf` for the platform -> region and platform -> preset catalogs
(shared with `computes/nebius`).
