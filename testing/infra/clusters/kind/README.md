# Kind cluster (remote over SSH)

Provision a multi-node [Kind](https://kind.sigs.k8s.io/) cluster over SSH on a
remote Docker host via Terraform, supporting any Kubernetes version, and merge
the cluster's kubeconfig into your local `~/.kube/config`.

Aligned with [testing/infra/clusters/k3s](../k3s/README.md) so you can run
high-node-count Kind clusters (2 to 32+ workers) on a remote machine with larger
CPU and memory capacity while driving `kubectl` and `helm` directly from your
workstation.

## What it does

- Connects over SSH (directly or through `ssh_jumper`) to a remote Docker host
  (`host`).
- Raises `fs.inotify.max_user_instances` and `fs.inotify.max_user_watches` on
  the remote host when below the configured minimums (never lowers higher host
  values), preventing multi-node Kind file-descriptor exhaustion.
- Installs and verifies a pinned `kind` CLI binary (`kind_version`, cached under
  `binary_cache_dir` with SHA-256 verification).
- Reclaims any pre-existing Kind cluster with the same `cluster_name`, renders a
  `kind.x-k8s.io/v1alpha4` cluster config for any Kubernetes version (`release`
  or `node_image`), enables `enable-aggregator-routing` for GPUStack Operator's
  aggregated API server, signs the API server certificate for the remote host
  (plus loopback, jump host, and `extra_cert_sans`), and stamps strictly nested
  `topology.kubernetes.io/{region,zone}` and `topology.gpustack.ai/rack` labels
  on worker nodes for Kueue TAS testing.
- Optionally writes `/etc/containerd/certs.d/<registry>/hosts.toml` inside every
  Kind node (`registry_mirrors` or `mirror = "cn"`) and preloads remote Docker
  images into all Kind nodes (`preload_images`).
- Fetches the Kind kubeconfig, rewrites its API server URL to
  `https://<host>:<api_server_port>`, namespaces the context to
  `kind-<cluster_name>-<host>`, merges it into `~/.kube/config` (honoring
  `switch_kube_context`), waits until all nodes are `Ready`, and asserts the
  running Kubernetes server version matches `release`.
- On `terraform destroy`, deletes the remote Kind cluster, removes the merged
  context from `~/.kube/config`, and cleans up `.last-apply.auto.tfvars.json`.

## Prerequisites

1. A remote Linux host with Docker running and enough CPU/RAM for the requested
   node count, reachable on `ssh_port` and `api_server_port` (default `6443`).
   By default `api_server_address` binds `0.0.0.0` (matching remote lab and
   NAT'd hosts; kube-apiserver requires mTLS client certificates). On
   multi-homed or internet-facing hosts, pass `-var='api_server_address=<lan-ip>'`
   to restrict listening to a private interface.
2. Key-based (passwordless) SSH login configured. The default key is
   `~/.ssh/id_ed25519`.
3. Passwordless `sudo` on the remote host (used for installing `/usr/local/bin/kind`,
   adjusting inotify sysctls, and accessing Docker if the SSH user is not in the
   `docker` group).
4. `terraform`, `kubectl`, `ssh`, and `jq` installed locally on your workstation.

> **Note:** `null_resource.kind_cluster` always starts clean (`kind delete cluster`
> before `kind create cluster`). Re-applying with modified cluster inputs (such as
> `release`, `workers`, `registry_mirrors`, or `preload_images`) replaces the
> cluster and deletes any workloads running in it. To load images into an existing
> cluster without recreating it, run `kind load docker-image` over SSH as shown below.

## Usage

```bash
cd testing/infra/clusters/kind
terraform init

# 1 control-plane + 3 workers on Kubernetes v1.34.11 (default)
terraform apply \
  -var='host=ubuntu@192.168.50.17' \
  -var='workers=3'

# Any Kubernetes version (e.g. v1.35.8 or v1.32.11) with custom topology and preloaded image
terraform apply \
  -var='host=ubuntu@192.168.50.17' \
  -var='cluster_name=gpustack-scale' \
  -var='release=v1.35.8' \
  -var='workers=6' \
  -var='topology_zones=2' \
  -var='topology_racks_per_zone=2' \
  -var='preload_images=["gpustack/gpustack-operator:dev"]' \
  -var='switch_kube_context=false'
```

After apply:

```bash
kubectl --context "$(terraform output -raw context_name)" get nodes --show-labels
# Or use the standalone kubeconfig in this directory:
KUBECONFIG=./kubeconfig kubectl get nodes
```

Load a freshly built image on the remote host into every Kind node later:

```bash
ssh ubuntu@192.168.50.17 'kind load docker-image gpustack/gpustack-operator:dev --name gpustack-kind'
```

Tear down (no `-var` needed; `host` is carried across from `.last-apply.auto.tfvars.json`):

```bash
terraform destroy
```

## Variables

| Variable | Description | Default |
|---|---|---|
| `host` | Remote Docker host (`host` or `user@host`) | (required) |
| `ssh_user` | Fallback SSH user when `host` has no `user@` prefix | `root` |
| `ssh_port` | SSH port of the remote host | `22` |
| `ssh_private_key` | Path to the SSH private key | `~/.ssh/id_ed25519` |
| `ssh_jumper` | Optional SSH jump host (`host` or `user@host`) | `""` |
| `ssh_jumper_port` | SSH port of the jump host | `22` |
| `cluster_name` | Kind cluster name on the remote host | `gpustack-kind` |
| `release` | Kubernetes version tag for `kindest/node` (any `vX.Y.Z`, optional `@sha256:...`) | `v1.34.11` |
| `node_image_repository` | OCI repository combined with `release` | `kindest/node` |
| `node_image` | Optional full node image override (supersedes `node_image_repository` and `release`) | `""` |
| `kind_version` | Pinned `kind` CLI version installed on the remote host | `v0.33.0` |
| `control_planes` | Number of Kind control-plane nodes | `1` |
| `workers` | Number of Kind worker nodes (`0` schedules on control-plane) | `3` |
| `topology_region` | `topology.kubernetes.io/region` label on workers (`""` disables topology labels) | `kind-region` |
| `topology_zones` | Number of zones (`zone-1..zone-Z`) across workers | `2` |
| `topology_racks_per_zone` | Number of racks per zone (strictly nested for Kueue TAS) | `2` |
| `api_server_address` | Host bind address for the Kind API server port | `0.0.0.0` |
| `api_server_port` | Host TCP port for the Kubernetes API server | `6443` |
| `extra_cert_sans` | Extra TLS SANs added to the API server certificate | `[]` |
| `cluster_cidr` | Pod subnet (`networking.podSubnet`) | `10.244.0.0/16` |
| `service_cidr` | Service subnet (`networking.serviceSubnet`) | `10.96.0.0/12` |
| `disable_default_cni` | Disable Kind's built-in `kindnetd` CNI | `false` |
| `mirror` | `""` or `"cn"` (derives community `docker.io` containerd mirrors inside nodes) | `""` |
| `registry_mirrors` | Per-registry mirror endpoints written to `/etc/containerd/certs.d` inside nodes | unset |
| `preload_images` | Remote Docker images loaded into all Kind nodes during apply | `[]` |
| `extra_mounts` | Extra host bind mounts attached to worker nodes | `[]` |
| `kubelet_extra_args` | Extra kubelet arguments merged into kubeadm config patches | `{}` |
| `binary_cache_dir` | Remote cache directory for pinned `kind` binaries (`""` disables cache) | `/var/lib/kind-cache` |
| `inotify_max_user_instances` | Minimum host `fs.inotify.max_user_instances` | `8192` |
| `inotify_max_user_watches` | Minimum host `fs.inotify.max_user_watches` | `524288` |
| `switch_kube_context` | Make the merged context current; `false` restores the previous one | `true` |

## Outputs

| Output | Description |
|---|---|
| `context_name` | The kubectl context merged into `~/.kube/config` |
| `cluster_name` | The Kind cluster name on the remote host |
| `host` | The remote Docker host address |
| `node_image` | Effective `kindest/node` image reference |
| `node_count` | Total Kind nodes (`control_planes + workers`) |
| `kubeconfig_path` | Path to the standalone kubeconfig in the module directory |
| `kind_config_yaml` | Rendered `kind.x-k8s.io/v1alpha4` Cluster configuration YAML |
