# EKS cluster

Provision an AWS EKS cluster with GPU node groups via Terraform, and point your
local kubeconfig at it.

## What it does

- Creates a VPC (public/private subnets, single NAT gateway) and an EKS cluster
  (default version `1.34`).
- Creates two kinds of managed node groups:
  - `cpu`: a CPU node group (`min = max = cpu_node_count`, default 1).
  - `gpu-<name>`: one GPU node group per key in `gpu_instance_types`, using
    the `AL2023_x86_64_NVIDIA` AMI, `desired = gpu_node_count` (default 1) and
    `min = 0`. Set `gpu_node_count=0` to park a group at no nodes and raise it
    again later without recreating the group.
- Installs common addons (`coredns`, `kube-proxy`, `vpc-cni`, `metrics-server`,
  `cert-manager`, `external-dns`, ...).
- Tags every node group's instances/volumes/ENIs `DO_NOT_DELETE=true` so the
  scheduled sweep does not terminate the nodes of a live cluster.
- After apply, runs `aws eks update-kubeconfig` to merge the cluster into
  `~/.kube/config` as a new context, which becomes the current one (unless
  `switch_kube_context=false`); on destroy it removes that
  context/cluster/user.

## Prerequisites

1. Install the [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
   and run `aws configure` to set your access key / secret key (the identity
   needs permission to create VPC/EKS/EC2/IAM resources).
2. Install `terraform` and `kubectl`.
3. An SSH public key on disk (default `~/.ssh/id_ed25519.pub`) — it is registered as
   the nodes' EC2 key pair for SSH access. Override the path with
   `-var='ssh_public_key=...'`.

## Usage

```bash
cd testing/infra/clusters/eks
terraform init

# Provision with the default GPU instances (g4dn)
terraform apply

# Or declare custom GPU node groups: each map key becomes a gpu-<key> group
terraform apply \
  -var='region=us-east-1' \
  -var='gpu_instance_types={ g4dn = ["g4dn.xlarge","g4dn.12xlarge"], g5 = ["g5.xlarge"] }'
```

GPU instance selection reference:
<https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html>.

EFA needs both variables set together, because the default CPU types do not support it:

```bash
terraform apply \
  -var='efa_enabled=true' \
  -var='cpu_instance_types=["c5n.9xlarge"]' \
  -var='cpu_node_count=2'
```

> **Cross-node EFA works only between nodes of the SAME node group here.** This module gives every
> node group its own cluster placement group, and EFA requires both ends of a transfer to sit in
> one. Nodes from the CPU group and a GPU group therefore cannot move bytes to each other, even
> though both are EFA-enabled and share an availability zone.
>
> The failure is silent, which is why it is called out here rather than left to be discovered: the
> libfabric handshake succeeds and the endpoint pair is reported established, then every work
> request hangs with the receiving adapter's byte counters at zero and no error on either side. It
> reads as a broken test, not as a placement fault. Measured, then confirmed by repeating the same
> transfer between two nodes of one group, where it completes.
>
> Put both ends of any cross-node fabric test in one node group. Making the groups share a
> placement group is a change to the upstream module's shape and is deliberately not made here.

Check a candidate type before applying; most are not EFA-capable, including every small size
of the general-purpose families:

```bash
aws ec2 describe-instance-types --instance-types c5n.9xlarge \
  --query 'InstanceTypes[].NetworkInfo.EfaSupported'
```

Once apply succeeds, the kubeconfig is already refreshed:

```bash
kubectl get nodes
# To re-fetch it manually
aws eks --region "$(terraform output -raw region)" update-kubeconfig --name "$(terraform output -raw cluster_name)"
```

Add `-var='switch_kube_context=false'` to keep the context you are already on:
the cluster is still merged in, and reached with
`kubectl --context "$(terraform output -raw cluster_name)" get nodes`.

Tear down:

```bash
terraform destroy
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `region` | AWS region | `us-east-1` |
| `ssh_public_key` | Path to the SSH public key registered as the node EC2 key pair | `~/.ssh/id_ed25519.pub` |
| `vpc_cidr` | VPC CIDR | `172.31.0.0/16` |
| `name_prefix` | Cluster name prefix (a random suffix is appended) | `gpustack-eks` |
| `release` | EKS version | `1.34` |
| `cpu_instance_types` | Instance types for the CPU node group | `["c6a.4xlarge","c7a.4xlarge"]` |
| `cpu_node_count` | Number of nodes in the CPU node group AT CREATE TIME. `0` drops the group entirely, the same as `clusters/nebius`. The same `desired_size` rule as `gpu_node_count` below applies to an edit against a live group | `1` |
| `efa_enabled` | Enable EFA on the CPU and GPU node groups: an EFA launch template, **one cluster placement group per node group**, one private subnet (single availability zone, reached through the NAT gateway and therefore not over SSH), and the label `gpustack.ai/efa=true`. Every group takes that one private subnet on purpose, because RDMA does not reach across availability zones -- necessary, and NOT sufficient for cross-node EFA; see the warning under Usage. REQUIRES every type in `cpu_instance_types` and `gpu_instance_types` to be EFA-capable, which neither default is | `false` |
| `gpu_instance_types` | GPU node groups as a `map(list(string))` keyed by group name | `{ g4dn = ["g4dn.xlarge","g4dn.12xlarge"] }` |
| `gpu_node_count` | Nodes in each GPU node group AT CREATE TIME. Drives `desired_size`; `max_size` follows it but never drops below 1, which EKS refuses. Terraform does NOT move this on a group that already exists, because the module ignores changes to `desired_size` — an edit moves the bounds and leaves the node count where it was. Resize or park a live group with `aws eks update-nodegroup-config --scaling-config desiredSize=N`, which is drift-free precisely because the attribute is ignored | `1` |
| `node_boot_disk_type` | Node root volume EBS type/performance (`volume_type`, optional `iops`/`throughput`) | `{ volume_type = "gp3", iops = 3000, throughput = 125 }` |
| `node_boot_disk_size_gb` | Node root (boot) volume size, in GiB | `100` |
| `node_instance_store_count` | Instance-store (ephemeral NVMe) devices mapped on every node group; must not exceed each instance type's disk count (e.g. `i7ie.xlarge` has 1). The devices surface as `/dev/nvme1n1` and onward | `0` |
| `enable_instance_store_csi_driver` | Install the `aws-ec2-local-instance-store-csi-driver` addon. ⚠️ The driver takes over every instance-store NVMe controller and **deletes unmanaged namespaces such as the raw `/dev/nvme1n1`** (observed 2026-09-08, i7ie.xlarge, driver v1.0.5), handing capacity out only via PVCs on its StorageClass. Keep it `false` when you need the raw device; if the driver already ran on a node, rebooting does NOT restore the namespace — recreate it with `nvme create-ns` + `nvme attach-ns` (or replace the node) | `false` |
| `switch_kube_context` | Let `update-kubeconfig` leave this cluster current; `false` restores the previous context | `true` |

## Outputs

| Output | Description |
|---|---|
| `region` | AWS region |
| `cluster_name` | EKS cluster name |
