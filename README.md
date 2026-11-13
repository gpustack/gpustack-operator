# GPUStack Operator

[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?logo=apache&label=License)](./LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/gpustack/gpustack-operator?logo=semanticrelease&label=Release&include_prereleases)](https://github.com/gpustack/gpustack-operator/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator?logo=docker&logoColor=fff&label=Docker%20Pulls)](https://hub.docker.com/r/gpustack/gpustack-operator)
[![Ask DeepWiki](https://img.shields.io/badge/Ask_DeepWiki-purple?logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAACXBIWXMAAAPoAAAD6AG1e1JrAAACIUlEQVRYw+2XP2gUQRTGv7d3JhYWQawkIBaCVWzETos0gmAnVoKNTSoLK0E7EQQrC20sBVFBtLPQRgTBtBaKjYgIQUSxiJq7fT+LvCGPJZdNLt4dQj5Y3u7sznzfzLw/s9IO/icANiniCujEfWdsQgBLxAbsbraPZcmBfcAt4DNwKQsZNfEUsAB8YRV12LfA6ZHuedg7AO7ed/c+UBcbQhbiu+6wXIM6EnZWkszMJe2Ke0n6I2la0v51hFeSMLN6OwIK6jJwEBdUYb2MAxRSz6sY4geiahFgLe1Hwq6YWe3us8AN4FQQU4QM64SPY69Xyr43fADgNjAPXHb3peSsD4FDQ0VLEjAHvIhBPS6AHvA1bBO9JPAXcHFbIRtJ5yzwIQZ9CZwAusDJENIkLqsG8DpNyIZJwSUk9wLHgakcesC1JCCjiHm1kYDNOEjptCxpSVK/8f73OFLxPLAYM3oCzEX7MXf/lJbc/8kWpA4HgQdpYI9IWAbeh83whi84cHXL+58EPBoQhnmm94EzwE3gZ2p/Dhzdbhg+TaRr01x7ftb4/jBwFzjXrCvDpmLW9crVLNeR9CaapoGemb2TdCERW1tNaBNQ8jktwmozqwtpqRNtdWAjARYk39IS12ZWRX7vmJmA71nQZgi36gN7gCvAj5xc3P0jcH7kx7Ik5IC73wsh14GZsZySow5002l4Jr0b6+m4eSyvpAn8lEzsx2QHo8RfUrlN+uPq4ksAAAAASUVORK5CYII=)](https://deepwiki.com/gpustack/gpustack-operator)

<img src="./site/assets/images/logo.png" width="100" height="100" alt="GPUStack Operator logo">

GPUStack Operator makes GPUs, NPUs and other accelerators available to Kubernetes workloads. It
discovers the hardware on each node, groups similar capacity into pools and uses Kueue to admit
workloads when a pool has room.

Read the [architecture overview](./docs/getting-started/architecture.md) to see how the pieces fit
together, or browse the [documentation](./docs/README.md).

## Architecture

![Four stages from hardware discovery to workload serving](./site/assets/images/architecture-flow.svg)
<!--
flowchart LR
  Discover["01 · Discover: NFD + Device Manager (Accelerators & RDMA)"] -> Schedule["02 · Schedule: Worker Pools, Kueue Queues, TAS & 5 Admission Gates"]
  Schedule -> Deliver["03 · Deliver: ModelArtifact Node/CSI Cache & Shared KVCachePool"]
  Deliver -> Run["04 · Run: ModelDeployment, ModelDeploymentScaler & GPU Instances"]
-->

1. **Discover:** NFD and Device Manager identify accelerators and RDMA interfaces. See [Device Discovery](./docs/modules/devices/discovery.md).
2. **Schedule:** The worker builds pools, `AcceleratorType`s and `InstanceType`s; Kueue and admission checks place workloads. See [Admission](./docs/modules/devices/admission.md).
3. **Deliver:** Optional model prefetch and shared KV cache support workloads. See [Model Delivery](./docs/modules/model-delivery/_index.md) and [KV Cache](./docs/modules/kv-cache/_index.md).
4. **Run:** Model deployments and instances serve workloads. See [Model Deployment](./docs/modules/model-deployment/_index.md) and [GPU Instances](./docs/modules/instances/_index.md).

See the [architecture overview](./docs/getting-started/architecture.md) for the complete flow.

## Features

- **[Heterogeneous Devices](./docs/modules/devices/_index.md)** — discover accelerators across
  manufacturers and allocate whole devices, shared devices, logical slices or hardware partitions.
- **[RDMA Networking](./docs/modules/rdma/_index.md)** — give workloads RDMA network interfaces
  alongside their accelerators.
- **[Topology](./docs/modules/topology/_index.md)** — place related workloads within the
  requested network or location domain.
- **[KV Cache](./docs/modules/kv-cache/_index.md)** — share inference cache across workloads.
- **[Model Delivery](./docs/modules/model-delivery/_index.md)** — fetch, verify and cache model weights
  on nodes before workloads need them.
- **[Model Deployment](./docs/modules/model-deployment/_index.md)** — run model serving replicas with
  routing and optional prefill/decode roles.
- **[GPU Instances](./docs/modules/instances/_index.md)** — enter accelerator-backed container
  workspaces over SSH.

## Installation

Use Kubernetes 1.29 or newer for scheduling and node model delivery, Helm 3.8 or newer, and
cluster-admin access. The chart accepts Kubernetes 1.23 and newer, but simple allocation on
1.23–1.28 is untested; see [prerequisites](./docs/operate/installation-modes.md#prerequisites).

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
[subchart migration guide](./docs/operate/migration/bundled-subcharts.md).

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

![Hardware discovery, pool admission and GPU allocation](./docs/assets/diagrams/devices-flow.svg)
<!--
flowchart LR
  Devices["Node Hardware & Devices CR: kubectl get devices"] -> Queue["Pool Queue: InstanceType ENTRANCE gpustack-fnv64-..."]
  Queue -> Admission["Kueue & Admission"] -> Pod["Pod Container: nvidia.com/gpu: 1"]
-->

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

The queue name is an example; use the `ENTRANCE` of your chosen pool. The default NVIDIA injection
requires a configured NVIDIA runtime. See [NVIDIA prerequisites](./docs/getting-started/vendor-prerequisites.md#nvidia)
and [Accelerator Requests](./docs/modules/devices/requests.md) for other request modes.

### RDMA Networking

![GPU and RDMA endpoint allocation for a container](./docs/assets/diagrams/rdma-flow.svg)
<!--
flowchart LR
  Request["Pod Container Request: nvidia.com/gpu: 1 and device.gpustack.ai/rdma.shared: 1"] -> Kubelet["Kubelet & Device Plugin: NUMA alignment depends on policy; endpoint selection"]
  Kubelet -> Allocated["Co-allocated GPU + RDMA HCA Endpoint"] -> Workload["Workload"]
-->

To give that Pod a network endpoint, put both requests in the same container:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-workload
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-c4680bb149644f1c
spec:
  containers:
    - name: worker
      resources:
        limits:
          nvidia.com/gpu: "1"
          device.gpustack.ai/rdma.shared: "1"
```

Replace the `resources` block in the Pod above with the one shown here. A shared endpoint suits a container that needs one
interface. An exclusive request reserves the endpoint only within the exclusive family; shared
requests can still use it. The node needs a usable endpoint, and NUMA alignment depends on its kubelet policy. See
[RDMA Operations](./docs/modules/rdma/operations.md) for the other resource keys and checks.

### Topology

![Topology-aware scheduling with two example replica placements](./docs/assets/diagrams/topology-flow.svg)
<!--
flowchart LR
  Source["TopologySource cloud-zones: region to zone"] -> TAS["Kueue TAS Domain"] -> Deployment["ModelDeployment qwen3-8b: replicas 2, size 4, requiredLevel zone"]
  Deployment -> A["Example Zone A: Replica 0, 4 Pods"]
  Deployment -> B["Example Zone B: Replica 1, 4 Pods"]
-->

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

For a role whose replica spans multiple Pods, add a zone requirement to its existing deployment:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen3-8b
 spec:
   roles:
     - name: server
       replicas: 2
       size: 4
+      topology:
+        requiredLevel: topology.kubernetes.io/zone
```

The engine still needs its own distributed execution settings. Use
[Topology Operations](./docs/modules/topology/operations.md) to verify the source and the Kueue
topology before depending on that placement.

### KV Cache

![Shared KV cache backend, pool, binding and serving Pods](./docs/assets/diagrams/kv-cache-flow.svg)
<!--
flowchart LR
  Backend["KVCacheBackend mooncake-dram: DRAM 8Gi"] -> Pool["KVCachePool shared-dram: total 8Gi"]
  Pool -> Binding["KVCachePoolBinding qwen-cache in team-a: ceiling 4Gi, bfloat16, block 64"] -> Pods["Serving Pods reuse shared KV blocks"]
-->

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
The [Shared Cache Walkthrough](./docs/walkthroughs/kv-cache/shared-cache.md) explains what to check at each step.

### Model Delivery

![Model weights from Hugging Face through the node cache to a serving Pod](./docs/assets/diagrams/model-delivery-flow.svg)
<!--
flowchart LR
  Source["Hugging Face: Qwen/Qwen2.5-7B-Instruct at main"] -> Artifact["ModelArtifact qwen-7b: resolved digest and revision"]
  Artifact -> Cache["Node Cache: NodeModelStore and CSI driver"] -> Pod["Serving Pod: read-only weight mount"]
-->

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

![Model, cache and accelerator inputs to a serving deployment](./docs/assets/diagrams/model-deployment-flow.svg)
<!--
flowchart LR
  Artifact["ModelArtifact qwen-7b"] -> Deployment["ModelDeployment qwen-chat: vLLM"]
  Binding["KVCachePoolBinding qwen-cache"] -> Deployment
  AcceleratorType["AcceleratorType nvidia-a10g"] -> Deployment
  Deployment -> Replica["Serving Replica Pod: accelerator 1"] -> Inference["Inference"]
-->

This deployment uses the artifact and cache binding above. List serving types with `kubectl get acctype`. Replace `acceleratorType` with one from your
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
    image: gpustack/runner:cuda13.0-vllm0.29.0
  kvCache:
    poolRef:
      name: qwen-cache
  roles:
    - name: server
      replicas: 1
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: 1
```

Role resources (`cpu`, `ram`, `localStorage`) and CPU-only roles omitting `acceleratorType` are covered in
[Model Deployment](./docs/modules/model-deployment/configuration.md). [Prefill and
Decode](./docs/modules/model-deployment/prefill-decode.md) shows how to split the serving roles.

### GPU Instances

![Slice-backed GPU instance and interactive SSH access](./docs/assets/diagrams/instances-flow.svg)
<!--
flowchart LR
  Key["InstanceSSHPublicKey workspace-key"] -> Instance["Instance sliced-demo: 20% memory, 100% cores, 1Gi volume"]
  Type["InstanceType: SL slice capacity"] -> Instance
  Instance -> Forward["kubectl port-forward 2222:22"] -> SSH["ssh -p 2222 root@127.0.0.1: interactive workspace"]
-->

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

```bash
kubectl -n team-a wait --for=condition=Ready pod/sliced-demo --timeout=5m
kubectl -n team-a port-forward pod/sliced-demo 2222:22
```

```bash
ssh -i ~/.ssh/id_ed25519 -p 2222 root@127.0.0.1
```

The SSH session enters the workload container, where the accelerator and workspace are available.
The [walkthrough](./docs/walkthroughs/devices/scheduling.md) shows the resulting resources. Pods can
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
