# Nebius standalone compute VM

Provision a single Nebius AI Cloud VM with a public IP reachable over SSH.

## What it does

- Creates a virtual network (`nebius_vpc_v1_network`) and a subnet
  (`nebius_vpc_v1_subnet`) inheriting the network's default private/public
  address pools.
- Creates a security group (`nebius_vpc_v1_security_group`) with an SSH
  ingress rule (`TCP/22` from `ssh_source_cidrs`) and an egress rule (allow
  all).
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
3. An SSH public key on disk (default `~/.ssh/id_rsa.pub`) — injected into the
   VM via cloud-init. Override the path with `-var='ssh_public_key=...'`.
4. A Nebius project ID (`parent_id`, e.g. `project-...`) — its region fixes
   VM placement and platform availability (see the reference table below).

## Usage

```bash
cd testing/infra/computes/nebius
terraform init

terraform plan  -var='parent_id=project-...'
terraform apply -var='parent_id=project-...'

terraform output -raw ssh_command
# ssh <ssh_username>@<public_ip>

terraform destroy -var='parent_id=project-...'
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `parent_id` | Nebius project ID (required); its region fixes VM placement & platform availability | *(required)* |
| `vm_name_prefix` | Prefix for the VM and its network/subnet/security-group names (a random suffix is appended) | `gpustack-nebius` |
| `ssh_public_key` | Path to the SSH public key injected into the VM via cloud-init | `~/.ssh/id_rsa.pub` |
| `ssh_username` | Username created on the VM with the SSH public key as an authorized key | `ubuntu` |
| `ssh_source_cidrs` | CIDR blocks allowed to SSH (TCP/22) into the VM | `["0.0.0.0/0"]` |
| `platform` | Nebius compute platform (see the platform/preset table below) | `cpu-e2` |
| `preset` | Nebius compute preset (vCPU/RAM shape) matching `platform` | `2vcpu-8gb` |
| `image_family` | Boot disk source image family | `ubuntu24.04` |
| `boot_disk_type` | Boot disk type (`NETWORK_SSD`, `NETWORK_HDD`, `NETWORK_SSD_NON_REPLICATED`, `NETWORK_SSD_IO_M3`) | `NETWORK_SSD` |
| `boot_disk_size_gibibytes` | Boot disk size, in GiB | `64` |

## Outputs

| Output | Description |
|---|---|
| `vm_name` | Name of the VM instance |
| `public_ip` | Public IPv4 address of the VM |
| `private_ip` | Private IPv4 address of the VM |
| `ssh_command` | Ready-to-run SSH command to reach the VM |

## Platform / preset / region reference

Region is implied by `parent_id`'s project — Nebius resources take no
`region` field. Platform availability varies by region:

| Region | Available platforms |
|---|---|
| `eu-north1` | `cpu-d3`, `cpu-e2`, `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-l40s-a`, `gpu-l40s-d` |
| `eu-west1` | `cpu-d3`, `gpu-h200-sxm` |
| `me-west1` | `cpu-d3`, `gpu-b200-sxm-a` |
| `uk-south1` | `cpu-d3`, `gpu-b300-sxm` |
| `us-central1` | `cpu-d3`, `gpu-b200-sxm`, `gpu-h200-sxm`, `gpu-rtx6000` |

The default `platform = cpu-e2` / `preset = 2vcpu-8gb` is the smallest CPU box
but exists only in `eu-north1`; for a project in another region use `cpu-d3`
(the sole CPU platform available in all regions, minimum `4vcpu-16gb`).

Example — `eu-north1` (richest availability), platform -> presets:

| Platform | Presets |
|---|---|
| `cpu-e2` | `2vcpu-8gb` (default), `4vcpu-16gb`, ... `80vcpu-320gb` |
| `cpu-d3` | `4vcpu-16gb`, ... `128vcpu-512gb` |
| `gpu-h100-sxm` | `1gpu-16vcpu-200gb`, `8gpu-128vcpu-1600gb` |
| `gpu-h200-sxm` | `1gpu-16vcpu-200gb`, `8gpu-128vcpu-1600gb` |
| `gpu-l40s-a` | `1gpu-8vcpu-32gb`, ... `1gpu-40vcpu-160gb` |
| `gpu-l40s-d` | `1gpu-16vcpu-96gb`, ... `4gpu-192vcpu-1152gb` |

See `variables.tf` for the full platform -> preset catalog (all CPU and GPU
shapes across all regions).
