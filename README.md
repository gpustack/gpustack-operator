# GPUStack Operator

[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?label=License)](./LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/gpustack/gpustack-operator?label=Release&include_prereleases)](https://github.com/gpustack/gpustack-operator/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator?label=Docker%20Pulls)](https://hub.docker.com/r/gpustack/gpustack-operator)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/gpustack/gpustack-operator)

<img src="./site/assets/images/logo.png" width="100" height="100" alt="GPUStack Operator logo">

GPUStack Operator makes GPUs, NPUs and other accelerators available to Kubernetes workloads. It
discovers the hardware on each node, groups similar capacity into pools and uses Kueue to admit
workloads when a pool has room.

Read the [architecture overview](./docs/getting-started/architecture.md) to see how the pieces fit
together, or browse the [documentation](./docs/README.md).

## Features

- [Heterogeneous Devices](./docs/modules/devices/_index.md) — discover accelerators across
  manufacturers and allocate whole devices, shared devices, logical slices or hardware partitions.
- [RDMA Networking](./docs/modules/rdma/_index.md) — give workloads RDMA network interfaces
  alongside their accelerators.
- [Topology Management](./docs/modules/topology/_index.md) — place related workloads within the
  requested network or location domain.
- [KV Cache](./docs/modules/kv-cache/_index.md) — share inference cache across workloads.
- [Model Delivery](./docs/modules/model-delivery/_index.md) — fetch, verify and cache model weights
  on nodes before workloads need them.
- [Model Deployment](./docs/modules/model-deployment/_index.md) — run model serving replicas with
  routing and optional prefill/decode roles.
- [Accelerated Instances](./docs/modules/instances/_index.md) — enter accelerator-backed container
  workspaces over SSH.

## Installation

Use Kubernetes 1.29 or newer for scheduling and node model delivery, Helm 3.8 or newer, and
cluster-admin access. The chart accepts Kubernetes 1.23 and newer, but simple allocation on
1.23–1.28 is untested; see [version requirements](./docs/operate/installation-modes.md#kubernetes-requirements).

Install the manufacturer's driver on each accelerator node. Some devices also need a container
toolkit; see [Vendor Prerequisites](./docs/getting-started/vendor-prerequisites.md).

```bash
helm repo add gpustack https://docs.gpustack.ai/gpustack-operator/charts
helm install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace
```

The chart includes Node Feature Discovery, Kueue and the storage drivers. See
[Installation Modes](./docs/operate/installation-modes.md) if your cluster already runs any of them.
For upgrades from v0.7.x or earlier, follow the
[subchart migration guide](./docs/operate/migration/to-subcharts.md).

### Uninstallation

```bash
helm uninstall gpustack-operator --namespace gpustack-system
```

Removing the bundled Kueue chart also removes its CRDs and the queues and workloads stored in
them. To use a separately managed Kueue installation, install GPUStack with
`--set kueue.enabled=false`. To remove resources created by the worker during uninstall, set
`cleanupOnUninstall=true` when installing the chart.

## Usage

The examples below assume a namespace named `team-a`, accelerator nodes with their drivers
installed, and an `InstanceType` with available capacity. Replace the queue and `InstanceType`
names with values from `kubectl get instancetypes` before applying a workload.

### Heterogeneous Devices

`kubectl get devices` shows the discovered hardware. The `ENTRANCE` column of
`kubectl get instancetypes` gives the queue name for a Pod. This one asks for a whole NVIDIA GPU:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-workload
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-c4680bb149644f1c
spec:
  restartPolicy: Never
  containers:
    - name: worker
      image: ubuntu:24.04
      command:
        - "sleep"
        - "3600"
      resources:
        limits:
          nvidia.com/gpu: "1"
```

The queue name is an example; use the `ENTRANCE` of your chosen pool. See
[Accelerator Requests](./docs/modules/devices/requests.md) for shared, sliced and partitioned Pods.

### RDMA Networking

To give that Pod a network endpoint, put both requests in the same container:

```yaml
resources:
  limits:
    nvidia.com/gpu: "1"
    device.gpustack.ai/rdma.shared: "1"
```

This replaces the `resources` block above. A shared endpoint suits a container that needs one
interface; use an exclusive request when it must be the adapter's only tenant. The node needs a
usable endpoint, and NUMA alignment depends on its kubelet policy. See
[RDMA Operations](./docs/modules/rdma/operations.md) for the other resource keys and checks.

### Topology Management

When nodes already carry region and zone labels, an administrator can publish their hierarchy:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: TopologySource
metadata:
  name: cloud-zones
spec:
  nodeSelector: {}
  levels:
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
  nodeLabels: {}
```

For a role whose replica spans multiple Pods, request one zone for each replica group:

```yaml
roles:
  - name: server
    replicas: 2
    size: 4
    topology:
      requiredLevel: topology.kubernetes.io/zone
```

This is a role fragment; the engine still needs its own distributed execution settings. Use
[Topology Operations](./docs/modules/topology/operations.md) to verify the source and the Kueue
topology before depending on that placement.

### KV Cache

An administrator creates the store, its capacity pool and a grant for `team-a`, in that order:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-dram
spec:
  type: Mooncake
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector:
            kubernetes.io/os: linux
          medium: DRAM
          capacityPerMember: 8Gi
---
apiVersion: worker.gpustack.ai/v1
kind: KVCachePool
metadata:
  name: shared-dram
spec:
  backends:
    - mooncake-dram
  quota:
    total: 8Gi
---
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: qwen-cache
  namespace: team-a
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 4Gi
  domain:
    blockSize: 64
    dtype: bfloat16
```

The `ModelDeployment` below names the binding, which grants this namespace access to the cache.
Choose member nodes, quotas, block size and dtype for your engine before applying these resources.
The [KV Cache Walkthrough](./docs/modules/kv-cache/walkthrough.md) explains what to check at each step.

### Model Delivery

A `ModelArtifact` names the weights that the serving workload will use:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: qwen-7b
  namespace: team-a
spec:
  source:
    huggingFace:
      repository: Qwen/Qwen2.5-7B-Instruct
      revision: main
```

The deployment below refers to this artifact. The operator resolves its revision before starting
the serving Pods. [Model Artifact](./docs/modules/model-delivery/artifact.md) covers PVC and image
sources; [Model Store Operations](./docs/modules/model-delivery/operations.md) covers node delivery.

### Model Deployment

This deployment uses the artifact and cache binding above. Replace `instanceType` with one from your
cluster:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
    artifactRef:
      name: qwen-7b
  engine:
    name: vLLM
    version: "0.29.0"
  kvCache:
    poolRef:
      name: qwen-cache
  roles:
    - name: server
      replicas: 1
      instanceType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: 1
```

For a standalone serving example, read
[Model Deployment](./docs/modules/model-deployment/deployment.md). [Prefill and
Decode](./docs/modules/model-deployment/prefill-decode.md) shows how to split the serving roles.

### Accelerated Instances

Choose an `InstanceType` from the pool list whose `SL` capacity has room for a slice. Replace
`spec.type` with that name and `spec.data` with your SSH public key. This workspace gives you SSH
access to 20% of one accelerator's memory:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceSSHPublicKey
metadata:
  name: workspace-key
  namespace: team-a
spec:
  data: ssh-ed25519 <paste-your-public-key-here>
---
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: sliced-demo
  namespace: team-a
spec:
  type: gpustack--nvidia-a10g-linux-amd64
  image: ubuntu:24.04
  command:
    - "tail"
    - "-f"
    - "/dev/null"
  sshPublicKey:
    name: workspace-key
  resources:
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 20
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

Save it as `instance.yaml`, then run `kubectl apply -f instance.yaml`. Once the Pod is ready,
forward its SSH port in one terminal and connect with the matching private key in another:

```shell
kubectl -n team-a wait --for=condition=Ready pod/sliced-demo --timeout=5m
kubectl -n team-a port-forward pod/sliced-demo 2222:22
```

```shell
ssh -i ~/.ssh/id_ed25519 -p 2222 root@127.0.0.1
```

The SSH session enters the workload container, where the accelerator and workspace are available.
The [walkthrough](./docs/getting-started/walkthrough.md) shows the resulting resources. Pods can
request accelerators directly; see
[Accelerator Requests](./docs/modules/devices/requests.md) for complete manifests.

### Accelerator Support

All manufacturers support whole-device and shared requests. Logical slicing shares a device
through the manufacturer's software facilities. Physical partitioning uses hardware partitions
enabled by an administrator; its availability depends on the device and its current mode.

| Manufacturer | Class | Kubernetes resource | Logical slicing | Physical partitioning |
|---|---|---|---|---|
| AMD | GPU | `amd.com/gpu` | Yes | — |
| Cambricon | MLU | `cambricon.com/mlu` | Yes | — |
| Huawei Ascend | NPU | `huawei.com/npu` | Yes | — |
| Hygon | DCU | `hygon.com/dcu` | Yes | Yes (MIG) |
| Iluvatar | GPU | `iluvatar.com/gpu` | Yes | — |
| MetaX | GPU | `metax-tech.com/gpu` | Yes | — |
| Moore Threads | GPU | `mthreads.com/gpu` | Yes | — |
| NVIDIA | GPU | `nvidia.com/gpu` | Yes | Yes (MIG) |
| T-Head | PPU | `alibabacloud.com/ppu` | Yes | Yes (MIG) |

A Pod requests one resource family in one container group. For NVIDIA, the request forms are:

| Request | `resources.limits` |
|---|---|
| Whole device | `nvidia.com/gpu: "1"` |
| Shared device | `nvidia.com/gpu.shared: "1"` |
| Logical slice | `nvidia.com/gpu.sliced: "1"`<br>`nvidia.com/gpu.sliced.memory-percentage: "20"`<br>`nvidia.com/gpu.sliced.cores-percentage: "40"` |
| MIG partition | `nvidia.com/gpu.partitioned: "1"`<br>`nvidia.com/gpu.partitioned.mig-3g.40gb: "1"` |

See [Accelerator Requests](./docs/modules/devices/requests.md) for the exact keys and rules, and
[Device Discovery](./docs/modules/devices/discovery.md) for each manufacturer's slicing behavior.

## License

Copyright (c) 2026 The GPUStack Authors. Licensed under the
[Apache License 2.0](./LICENSE).
