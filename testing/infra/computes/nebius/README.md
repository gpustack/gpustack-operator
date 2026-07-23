# Nebius standalone compute VM

Provision a single Nebius AI Cloud VM with a public IP reachable over SSH.

## What it does

- Creates a virtual network (`nebius_vpc_v1_network`) and a subnet
  (`nebius_vpc_v1_subnet`) inheriting the network's default private/public
  address pools.
- Creates a security group (`nebius_vpc_v1_security_group`) with an SSH
  ingress rule (`TCP/22` from `0.0.0.0/0`) and an egress rule (allow all).
- Creates a single VM (`nebius_compute_v1_instance`) with a managed boot disk,
  an auto-allocated public IP, and cloud-init injecting an SSH user + key.

## Prerequisites

1. Install the [Nebius CLI](https://docs.nebius.com/cli) and `terraform`.
2. A Nebius Service Account with an authorized key. Export:
   ```bash
   export NEBIUS_SA_ID="serviceaccount-..."
   export NEBIUS_AUTHKEY_PUBLIC_ID="publickey-..."
   export NEBIUS_AUTHKEY_PRIVATE_PATH="$HOME/.nebius/authkey/private.pem"
   ```
3. An SSH public key on disk (default `~/.ssh/id_ed25519.pub`) — injected into
   the VM via cloud-init. Override the path with `-var='ssh_public_key=...'`.
4. A Nebius project ID (`project_id`, e.g. `project-...`) — its region fixes
   VM placement and platform availability (see the reference table below).

## Usage

```bash
cd testing/infra/computes/nebius
terraform init

terraform plan  -var='project_id=project-...'
terraform apply -var='project_id=project-...'

terraform output -raw ssh_command
# ssh -i <ssh_private_key> ubuntu@<public_ip>

terraform destroy   # no -var needed -- reuses the last apply's variables
```

The SSH source CIDR (`0.0.0.0/0`) and SSH username (`ubuntu`) are fixed; the
VM only accepts key-based auth via `ssh_public_key`.

## Variables

| Variable | Description | Default |
|---|---|---|
| `project_id` | Nebius project ID (required); its region fixes VM placement & platform availability | *(required)* |
| `name_prefix` | Prefix for the VM and its network/subnet/security-group names (a random suffix is appended) | `gpustack-nebius` |
| `ssh_public_key` | Path to the SSH public key injected into the VM via cloud-init | `~/.ssh/id_ed25519.pub` |
| `instance_type` | The platform/preset/image_family combination (see the reference table below) | `{ platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "ubuntu24.04-cuda13.0" }` |
| `boot_disk_type` | Boot disk type (`NETWORK_SSD`, `NETWORK_HDD`, `NETWORK_SSD_NON_REPLICATED`, `NETWORK_SSD_IO_M3`) | `NETWORK_SSD` |
| `boot_disk_size_gb` | Boot disk size, in GiB | `100` |

## Outputs

| Output | Description |
|---|---|
| `vm_name` | Name of the VM instance |
| `public_ip` | Public IPv4 address of the VM |
| `private_ip` | Private IPv4 address of the VM |
| `ssh_command` | Ready-to-run SSH command to reach the VM |

## Platform / preset / image_family / region reference

Region is implied by `project_id`'s project — Nebius resources take no
`region` field. Platform availability varies by region:

| Region | Available platforms |
|---|---|
| `eu-north1` | `cpu-d3`, `cpu-e2`, `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-l40s-a`, `gpu-l40s-d` |
| `eu-west1` | `cpu-d3`, `gpu-h200-sxm` |
| `me-west1` | `cpu-d3`, `gpu-b200-sxm-a` |
| `uk-south1` | `cpu-d3`, `gpu-b300-sxm` |
| `us-central1` | `cpu-d3`, `gpu-b200-sxm`, `gpu-h200-sxm`, `gpu-rtx6000` |

The default `instance_type` is `gpu-h100-sxm` / `1gpu-16vcpu-200gb` /
`ubuntu24.04-cuda13.0` (available in `eu-north1`); for a CPU box in any
region use `cpu-d3` (the sole CPU platform available in all regions, minimum
`4vcpu-16gb`).

Example — `eu-north1` (richest availability), platform -> preset -> image_family:

| Platform | Presets | Image family |
|---|---|---|
| `cpu-e2` | `2vcpu-8gb`, `4vcpu-16gb`, ... `80vcpu-320gb` | `ubuntu24.04-driverless` |
| `cpu-d3` | `4vcpu-16gb`, ... `128vcpu-512gb` | `ubuntu24.04-driverless` |
| `gpu-h100-sxm` | `1gpu-16vcpu-200gb` (default), `8gpu-128vcpu-1600gb` | `ubuntu24.04-cuda13.0` (default) |
| `gpu-h200-sxm` | `1gpu-16vcpu-200gb`, `8gpu-128vcpu-1600gb` | `ubuntu24.04-cuda13.0` |
| `gpu-l40s-a` | `1gpu-8vcpu-32gb`, ... `1gpu-40vcpu-160gb` | `ubuntu24.04-cuda13.0` |
| `gpu-l40s-d` | `1gpu-16vcpu-96gb`, ... `4gpu-192vcpu-1152gb` | `ubuntu24.04-cuda13.0` |

See `variables.tf` for the full platform / preset / image_family catalog
(all CPU and GPU shapes across all regions) and the object syntax to
override a combo, e.g.:

```bash
terraform apply -var='project_id=project-...' \
  -var='instance_type={platform="cpu-d3",preset="4vcpu-16gb",image_family="ubuntu24.04-driverless"}'
```
