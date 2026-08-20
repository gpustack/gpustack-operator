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

terraform destroy   # no -var needed -- project_id is carried across from the last apply
```

The SSH source CIDR (`0.0.0.0/0`) and SSH username (`ubuntu`) are fixed; the
VM only accepts key-based auth via `ssh_public_key`.

## Variables

| Variable | Description | Default |
|---|---|---|
| `project_id` | Nebius project ID (required); its region fixes VM placement & platform availability | *(required)* |
| `name_prefix` | Prefix for the VM and its network/subnet/security-group names (a random suffix is appended) | `gpustack-nebius` |
| `ssh_public_key` | Path to the SSH public key injected into the VM via cloud-init | `~/.ssh/id_ed25519.pub` |
| `instance_type` | `platform` and `preset` are required; `image_family` is optional and resolved from the live catalogue when omitted | `{ platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }` |
| `boot_disk_type` | Boot disk type (`NETWORK_SSD`, `NETWORK_HDD`, `NETWORK_SSD_NON_REPLICATED`, `NETWORK_SSD_IO_M3`) | `NETWORK_SSD` |
| `boot_disk_size_gb` | Boot disk size, in GiB | `100` |

## Outputs

| Output | Description |
|---|---|
| `vm_name` | Name of the VM instance |
| `public_ip` | Public IPv4 address of the VM |
| `image_family` | The family the VM was built from: pinned, or resolved from the platform |
| `image_architecture` | The resolved image's CPU architecture (null when the family was pinned) |
| `private_ip` | Private IPv4 address of the VM |
| `ssh_command` | Ready-to-run SSH command to reach the VM |

## Platform, preset and image family

Region is implied by `project_id`'s project -- Nebius resources take no `region`
field, and which platforms exist varies by region.

**You give the platform and the preset; the image family is resolved for you.**
Leave `instance_type.image_family` out and the module asks Nebius which family
that platform recommends, takes the newest, and validates the platform and the
preset in the same call. So a platform Nebius adds tomorrow works with no change
here -- and an ARM64 platform gets its ARM64 family instead of the AMD64 one a
hand-written table would have handed it.

```bash
# the default: gpu-h100-sxm / 1gpu-16vcpu-200gb, family resolved (ubuntu24.04-cuda13.0)
terraform apply -var='project_id=project-...'

# a CPU box; the family resolves to ubuntu24.04-driverless
terraform apply -var='project_id=project-...' \
  -var='instance_type={platform="cpu-d3",preset="4vcpu-16gb"}'
```

The resolved family and its CPU architecture are module outputs (`image_family`,
`image_architecture`).

To see what the API currently offers:

```bash
project=project-...
region=$(nebius iam project get --id "$project" --format json | jq -r .spec.region)

nebius compute platform list --parent-id "$project" --format json \
  | jq -r '.items[] | "\(.metadata.name): \([.spec.presets[].name] | join(", "))"'

nebius compute image list-public --region "$region" --format json \
  | jq -r '.items[] | select((.spec.recommended_platforms // []) | length > 0)
          | "\(.spec.image_family) <- \(.spec.recommended_platforms | join(", "))"'
```

### Pinning the family, and what it opts out of

```bash
terraform apply -var='project_id=project-...' \
  -var='instance_type={platform="cpu-d3",preset="4vcpu-16gb",image_family="ubuntu24.04-driverless"}'
```

Pinning skips the lookup **entirely** -- no `nebius` CLI and no `jq` are needed
on plan, apply or destroy. It also skips the platform/preset check and the
boot-disk minimum check, since both come from the same call.

Two consequences worth knowing before you pin:

- **Pass it on every operation, not just apply.** A `-var` given only on apply is
  not remembered at destroy: `.last-apply.auto.tfvars.json` deliberately
  snapshots only variables that have no default, and `instance_type` has one. A
  destroy without the pin runs the lookup again, and so needs the CLI.
- **An unpinned plan re-reads the live catalogue.** If Nebius publishes a newer
  family for your platform, the next plan proposes **replacing** the VM. Pin the
  family for anything you intend to keep.

### Boot disk

`boot_disk_size_gb` is checked twice at plan time: against the disk type's own
granularity (`NETWORK_SSD_NON_REPLICATED` and `NETWORK_SSD_IO_M3` are allocated
in whole 93 GiB units; `NETWORK_SSD` is 1-8192 GiB), and -- when the family is
resolved rather than pinned -- against the minimum the image itself publishes
(10 GiB driverless, 40 GiB CUDA), rounded up from the bytes the API reports.
