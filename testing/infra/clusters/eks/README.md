# EKS cluster

Provision an AWS EKS cluster with GPU node groups via Terraform, and point your
local kubeconfig at it.

## What it does

- Creates a VPC (public/private subnets, single NAT gateway) and an EKS cluster
  (default version `1.34`).
- Creates two kinds of managed node groups:
  - `cpu`: a CPU node group (`min = max = 1`).
  - `gpu-<name>`: one GPU node group per key in `eks_gpu_instance_types`, using
    the `AL2023_x86_64_NVIDIA` AMI and `min = 0` (scaled to zero by default,
    brought up on demand).
- Installs common addons (`coredns`, `kube-proxy`, `vpc-cni`, `metrics-server`,
  `cert-manager`, `external-dns`, ...).
- After apply, runs `aws eks update-kubeconfig` to merge the cluster into
  `~/.kube/config` as a new context; on destroy it removes that
  context/cluster/user.

## Prerequisites

1. Install the [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
   and run `aws configure` to set your access key / secret key (the identity
   needs permission to create VPC/EKS/EC2/IAM resources).
2. Install `terraform` and `kubectl`.
3. An SSH public key on disk (default `~/.ssh/id_rsa.pub`) — it is registered as
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
  -var='eks_gpu_instance_types={ g4dn = ["g4dn.xlarge","g4dn.12xlarge"], g5 = ["g5.xlarge"] }'
```

GPU instance selection reference:
<https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html>.

Once apply succeeds, the kubeconfig is already refreshed:

```bash
kubectl get nodes
# To re-fetch it manually
aws eks --region "$(terraform output -raw region)" update-kubeconfig --name "$(terraform output -raw cluster_name)"
```

Tear down:

```bash
terraform destroy
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `region` | AWS region | `us-east-1` |
| `ssh_public_key` | Path to the SSH public key registered as the node EC2 key pair | `~/.ssh/id_rsa.pub` |
| `vpc_cidr` | VPC CIDR | `172.31.0.0/16` |
| `eks_name_prefix` | Cluster name prefix (a random suffix is appended) | `gpustack-eks` |
| `eks_version` | EKS version | `1.34` |
| `eks_cpu_instance_types` | Instance types for the CPU node group | `["c6a.4xlarge","c7a.4xlarge"]` |
| `eks_gpu_instance_types` | GPU node groups as a `map(list(string))` keyed by group name | `{ g4dn = ["g4dn.xlarge","g4dn.12xlarge"] }` |

## Outputs

| Output | Description |
|---|---|
| `region` | AWS region |
| `cluster_name` | EKS cluster name |
