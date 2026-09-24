# EKS cluster

Provision an AWS EKS cluster with GPU node groups via Terraform, and point your
local kubeconfig at it.

## What it does

- Creates a VPC (public/private subnets, single NAT gateway) and an EKS cluster
  (default version `1.34`).
- Creates the managed node groups named by two maps:
  - `cpu_instance_types`: CPU node groups (`min = max = desired = node_count`), optionally pinned to one availability zone.
  - `gpu_instance_types`: accelerator node groups (`desired = node_count`,
    `max = node_count`, `min = 0`).
  In both, the map key IS the node group name, and a group that should not
  exist is an absent key rather than a zero count. Parking a live group at no
  nodes is a cloud-side change, not a config value -- see `gpu_instance_types`
  in the table below.
- Gives each non-EFA group's nodes a public IPv4 in the public subnets so they
  can be reached over SSH (`public_ip`, default `true`, the same per-group field
  `clusters/nebius` has); a group with `public_ip = false` sits in the private
  subnets instead and reaches out through the NAT gateway.
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

# Or declare custom node groups: the key IS the node group name, and the count
# lives inside the group it counts
terraform apply \
  -var='region=us-east-1' \
  -var='gpu_instance_types={ gpu-g4dn = { instance_types = ["g4dn.xlarge","g4dn.12xlarge"], node_count = 1 }, gpu-g5 = { instance_types = ["g5.xlarge"], node_count = 2 } }'
```

GPU instance selection reference:
<https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html>.

EFA needs both variables set together, because the default CPU types do not support it:

```bash
terraform apply \
  -var='efa_enabled=true' \
  -var='cpu_instance_types={ cpu = { instance_types = ["c5n.9xlarge"], node_count = 2 } }'
```

For the deterministic four-node, two-zone CPU topology fixture, keep EFA disabled and
pin each ordinary CPU node group to a distinct public subnet:

```bash
terraform plan \
  -var='region=us-east-1' \
  -var='gpu_instance_types={}' \
  -var='cpu_instance_types={topology-a={instance_types=["t3a.medium"],node_count=2,availability_zone_index=0},topology-b={instance_types=["t3a.medium"],node_count=2,availability_zone_index=1}}' \
  -var='efa_enabled=false' \
  -var='switch_kube_context=false'
```

When no test needs to reach a group's nodes from the internet, set `public_ip = false` in that
group's entry to keep them off public addresses, for example
`gpu-g4dn = { instance_types = ["g4dn.xlarge"], node_count = 1, public_ip = false }`. The group is
replaced on the next apply, so do it in a round where it is being rotated anyway; see the
`cpu_instance_types` row below.

Run `./test_plan.sh` only with AWS credentials and an initialized EKS Terraform state;
its legacy-plan replacement check needs existing resources to be meaningful.

Before any `terraform apply`, review the planned paid infrastructure and current AWS pricing,
quotas, and availability. Setting `topograph_aws_pod_identity_enabled=true` additionally gives the
node-data-broker Pods IMDSv2 access by setting the node metadata response hop limit to 2 and creates
one Pod Identity association for `gpustack-system/gpustack-operator-topograph`. Its separate IAM
role permits only `ec2:DescribeInstanceTopology`; it does not reuse a node role.

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

To remove one chosen node from a live group, suspend the group's `AZRebalance` process first.
Without it, terminating the last node of a zone makes the group launch a replacement in that
zone and then terminate a different node to get back to its desired count, so the node you
meant to keep is the one that goes. The suspension is deleted with the group on destroy.

```bash
asg="$(aws eks describe-nodegroup --cluster-name "$(terraform output -raw cluster_name)" \
  --nodegroup-name <group> --query 'nodegroup.resources.autoScalingGroups[0].name' --output text)"
aws autoscaling suspend-processes --auto-scaling-group-name "$asg" --scaling-processes AZRebalance
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
aws autoscaling terminate-instance-in-auto-scaling-group --instance-id <instance-id> \
  --should-decrement-desired-capacity
```

To free a node's disk by removing images, remove each image's CRI reference as well as its
repository reference. The CRI reference is named by the image's config digest (`sha256:...`,
labelled `io.cri-containerd.image=managed`) and pins the layers on its own, so deleting only the
`repo@digest` reference frees nothing. It does not carry the repository name, so a listing
filtered by that name does not show it. Check that no container still uses the image first.

```bash
ctr -n k8s.io images ls | grep -e '<repo>' -e '<config digest>'
ctr -n k8s.io images rm '<repo>@sha256:...' 'sha256:<config digest>'
```

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
| `cpu_instance_types` | CPU node groups as a `map(object)`, keyed by node group name (the key IS the name; no prefix is added): `instance_types`, `node_count` (`min = max = desired = node_count`, AT CREATE TIME), optional `availability_zone_index` (`0` to `2`) that pins an ordinary CPU group to one zone's subnet, and optional `public_ip`, the same per-group field `clusters/nebius` has. `public_ip = true` (the default) gives the group a public address in the public subnets. `false` places it in the private subnets without one: its nodes reach out through the NAT gateway and are NOT reachable over SSH. The subnet moves with the address because a node without a public address has no route out of a public subnet -- unlike `clusters/nebius`, where a default egress gateway lets the address go alone. Changing it and applying REPLACES the group, so switch it when the group is being rotated anyway. `public_ip` is ignored while `efa_enabled` is true, whose groups are already private. A group that should not exist is an absent key, not a zero count. Terraform does NOT move `desired_size` on a group that already exists (the module ignores it) -- an edit moves only the bounds; resize or park a live group with `aws eks update-nodegroup-config --scaling-config desiredSize=N`, which is drift-free precisely because the attribute is ignored | `{ cpu = { instance_types = ["c6a.4xlarge","c7a.4xlarge"], node_count = 1 } }` |
| `topograph_aws_pod_identity_enabled` | Set the node IMDSv2 response hop limit to 2 for the AWS node-data-broker and create the fixed Topograph API Pod Identity association with a separate role limited to `ec2:DescribeInstanceTopology` | `false` |
| `efa_enabled` | Enable EFA on the CPU and GPU node groups: an EFA launch template, **one cluster placement group per node group**, one private subnet (single availability zone, reached through the NAT gateway and therefore not over SSH), and the label `gpustack.ai/efa=true`. Every group takes that one private subnet on purpose, because RDMA does not reach across availability zones -- necessary, and NOT sufficient for cross-node EFA; see the warning under Usage. REQUIRES every type in `cpu_instance_types` and `gpu_instance_types` to be EFA-capable, which neither default is | `false` |
| `efa_availability_zone_index` | Index (`0`, `1` or `2`) into the module's three availability zones, deciding which one the EFA node groups and their placement groups land in. Only read when `efa_enabled` is true, and both groups follow this one index because a cluster placement group is scoped to a single zone and RDMA does not reach across zones. Move it when a zone refuses capacity -- an `InsufficientInstanceCapacity` refusal names the zones that do have the type -- and do NOT swap the instance types instead, because those are what is under test | `0` |
| `gpu_instance_types` | GPU node groups as a `map(object)`, keyed by node group name (the key IS the name; no prefix is added): `instance_types` (candidate types), `node_count`, and optional `public_ip` (same meaning and default `true` as in `cpu_instance_types`). `desired = node_count` and `max = node_count` AT CREATE TIME, `min = 0`; a count of zero is not valid -- an absent key is how a group is not created. Terraform does NOT move `desired_size` on a group that already exists (the module ignores it) -- an edit moves only `max_size` and can leave `max` under `desired`; resize or park a live group with `aws eks update-nodegroup-config --scaling-config desiredSize=N`, which is drift-free precisely because the attribute is ignored | `{ gpu-g4dn = { instance_types = ["g4dn.xlarge","g4dn.12xlarge"], node_count = 1 } }` |
| `node_boot_disk_type` | Node root volume EBS type/performance (`volume_type`, optional `iops`/`throughput`) | `{ volume_type = "gp3", iops = 3000, throughput = 125 }` |
| `node_boot_disk_size_gb` | Node root (boot) volume size, in GiB | `100` |
| `node_instance_store_count` | Instance-store (ephemeral NVMe) devices mapped on every node group; must not exceed each instance type's disk count (e.g. `i7ie.xlarge` has 1). The devices surface as `/dev/nvme1n1` and onward | `0` |
| `enable_instance_store_csi_driver` | Install the `aws-ec2-local-instance-store-csi-driver` addon. ⚠️ The driver takes over every instance-store NVMe controller and **deletes unmanaged namespaces such as the raw `/dev/nvme1n1`** (observed 2026-09-08, i7ie.xlarge, driver v1.0.5), handing capacity out only via PVCs on its StorageClass. Keep it `false` when you need the raw device; if the driver already ran on a node, rebooting does NOT restore the namespace — recreate it with `nvme create-ns` + `nvme attach-ns` (or replace the node) | `false` |
| `switch_kube_context` | Let `update-kubeconfig` leave this cluster current; `false` restores the previous context only when that context still exists | `true` |

## Outputs

| Output | Description |
|---|---|
| `region` | AWS region |
| `cluster_name` | EKS cluster name |
| `selected_zones` | Availability zone selected for each CPU node group, or `null` when unpinned |
| `node_group_names` | Configured managed node group names |
| `kubeconfig_command` | Command that writes a separate kubeconfig without switching the caller's current context |
