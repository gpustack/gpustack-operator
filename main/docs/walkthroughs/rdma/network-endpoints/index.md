# RDMA Networking Walkthrough

Request and verify *remote direct memory access (RDMA)* endpoints for workloads, align network adapters with GPUs on *non-uniform memory access (NUMA)* nodes,
and configure multi-tenant and cache fabric interconnects.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Request RDMA](#step-1-request-rdma)
- [Step 2: Align NUMA with kubelet](#step-2-align-numa-with-kubelet)
- [Step 3: Request interfaces per role](#step-3-request-interfaces-per-role)
- [Step 4: KV cache over RDMA](#step-4-kv-cache-over-rdma)
- [Step 5: Apply AWS EFA differences](#step-5-apply-aws-efa-differences)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

Device Manager discovers *RDMA* NICs from `/sys/class/net`, resolves bound *RDMA* devices, verifies link
status, and advertises three extended resource keys — shared, exclusive, and *single-root I/O virtualization (SR-IOV)* partitioned — so
a workload can request network fabric beside its accelerators.

When at least one endpoint is active, the node carries `feature.gpustack.ai/rdma.capable=true` (see
[Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md) and
[RDMA resource keys](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#rdma-resource-keys)).

The steps verify interface discovery, container injection, *NUMA* alignment, and deployment
integration.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `Pod` | namespace | user | a workload that requests *RDMA* endpoints beside its accelerator |
| `ModelDeployment` | namespace | user | per-role interface requests under *`roles[].resources.interface`* |
| `KVCacheBackend` | cluster | administrator | the cache transport carried over *RDMA* or EFA |

## Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack installed |
| Hardware | *RDMA*-capable network adapters on the workload nodes |

## Step 1: Request RDMA

Inspect discovered interfaces and allocatable capacities on the target node first:

```bash
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, link})'

kubectl get node <node> -o json |
  jq '.status.allocatable | with_entries(select(.key | contains("gpustack.ai/rdma")))'
```

A workload requests whole accelerators and *RDMA* endpoints within the same container.
`Pod`s must carry the `kueue.x-k8s.io/queue-name` label to participate in admission.
Name the entrance `LocalQueue` of the pool that holds the accelerators.
`LocalQueue` names look like `gpustack-fnv64-<hash>`. List them with `kubectl -n team-a get localqueues`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: trainer-0
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-<hash>
spec:
  restartPolicy: Never
  containers:
    - name: trainer
      image: ubuntu:24.04
      command:
        - "sleep"
        - "3600"
      resources:
        limits:
          nvidia.com/gpu: "1"
          device.gpustack.ai/rdma.shared: "1"
```

The allocated container receives character devices and `NCCL_IB_HCA` configuration
(see [Injected devices](/gpustack-operator/main/docs/modules/rdma/operations/index.md#injected-devices)).

Prefer `device.gpustack.ai/rdma.shared` when a container needs a single endpoint. Use the exclusive
key `device.gpustack.ai/rdma` when requesting multiple distinct network endpoints.

## Step 2: Align NUMA with kubelet

Kubernetes does not pair specific accelerators with adjacent network adapters by default.
Enable *NUMA* alignment by setting the TopologyManager policy in `/var/lib/kubelet/config.yaml`
(see [Enabling NUMA alignment](/gpustack-operator/main/docs/modules/rdma/operations/index.md#enabling-numa-alignment)):

```diff
 apiVersion: kubelet.config.k8s.io/v1beta1
 kind: KubeletConfiguration
+topologyManagerPolicy: restricted
+topologyManagerScope: container
```

Restart the kubelet after saving the configuration:

```bash
systemctl restart kubelet
kubectl get --raw /api/v1/nodes/<node>/proxy/configz |
  jq -r '.kubeletconfig | {topologyManagerPolicy, topologyManagerScope}'
```

`restricted` ensures that single-socket requests land on one *NUMA* node, while multi-GPU workloads
spanning multiple sockets are packed into the minimum number of *NUMA* nodes.

## Step 3: Request interfaces per role

In a `ModelDeployment`, roles do not declare raw resource keys. Specify the number of network
interfaces under *`roles[].resources.interface`*.
The operator refuses an interface request when no *RDMA* or EFA transfer path uses it.
A *prefill* and *decode* pair with `spec.kvTransfer.protocol: RDMA` is such a path:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-pd
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.29.0
  router:
    name: llm-d-router
  kvTransfer:
    protocol: RDMA
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      instanceType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: "2"
        interface: "2"
    - name: decode
      kind: Decode
      replicas: 1
      instanceType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: "2"
        interface: "2"
```

For the `RDMA` protocol, the operator translates `interface: 1` into `device.gpustack.ai/rdma.shared: 1`.
When `interface` is greater than 1, it renders `device.gpustack.ai/rdma: N`.
Each allocated endpoint then corresponds to an independent physical adapter
(see [A minimal deployment](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#minimal-deployment)).

The same request without *`router`*, *`kvTransfer`* or a cache backend that uses *RDMA* fails at admission with
`no effective RDMA or EFA transfer leg uses the requested interfaces`.
A cache backend on *RDMA* is the other path, and Step 4 sets it up.

## Step 4: KV cache over RDMA

To route KV cache transport over an *RDMA* fabric instead of TCP, edit the `mooncake-dram` backend
from [Step 1 of the Shared Cache walkthrough](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md#step-1-create-the-store).
The transport is editable on a running backend:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: mooncake-dram
 spec:
   type: Mooncake
+  transport:
+    protocol: RDMA
   connection:
     managed:
       leader: {}
       members:
         - nodeSelector:
             kubernetes.io/os: linux
           medium: DRAM
           capacityPerMember: 8Gi
+          fabricInterfaceCount: 2
```

Member `DaemonSet` `Pod`s automatically receive the network devices, host privileges, and limits
required for high-performance transport (see [Members](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#members)).

## Step 5: Apply AWS EFA differences

On AWS Elastic Fabric Adapter (EFA) instances, *RDMA* devices bypass `/sys/class/net` and do not expose
standard verbs completion queues. Declare the `EFA` protocol explicitly on a backend for those nodes:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-efa
spec:
  type: Mooncake
  transport:
    protocol: EFA
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector:
            node.kubernetes.io/instance-type: c6in.32xlarge
          medium: DRAM
          capacityPerMember: 64Gi
          fabricInterfaceCount: 2
```

In `ModelDeployment`, set `kvTransfer.protocol: EFA` and specify custom runner images containing
`libfabric` and EFA-enabled Mooncake builds.

## Limitations

- Kubelet *NUMA* alignment policies (`none`, `restricted`, `single-numa-node`) and
  `TopologyAffinityError` admission behavior are covered in
  [Enabling NUMA alignment](/gpustack-operator/main/docs/modules/rdma/operations/index.md#enabling-numa-alignment)
  and [Reading the result](/gpustack-operator/main/docs/modules/devices/preflight/index.md#reading-the-result).
- Kueue network-resource metering and over-admission behavior are covered in
  [RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) and [Troubleshooting](#troubleshooting).
- AWS EFA discovery and transport configuration are covered in
  [Step 5](#step-5-apply-aws-efa-differences) and [Troubleshooting](#troubleshooting).

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| *RDMA* resource keys present but all `Unhealthy` | Status: every port link `Failed` or unreadable | Inspect `kubectl get devices <node> -o json` for `.spec.interfaces[].link`; verify physical cable and subnet manager |
| `Workload` admitted by Kueue but `Pod` remains `Pending` | Over-admission: Kueue does not meter network resources | Inspect cluster allocatable *RDMA* keys (`device.gpustack.ai/rdma*`); add nodes or wait for active `Pod`s to finish |
| `Pod` rejected at placement | Reason: `TopologyAffinityError` — free GPUs and NICs reside on mismatched *NUMA* nodes | Ensure kubelet has `topologyManagerPolicy: restricted`; verify GPU and NIC requests are declared in the same container |
| Fewer `uverbs` devices inside container than expected | Shared tokens coalesced into duplicate endpoints | Requesting `rdma.shared: N` (N>1) may merge tokens from the same NIC; use `device.gpustack.ai/rdma: N` for distinct adapters |
| `Node` unexpectedly loses `feature.gpustack.ai/rdma.capable` | Status: every endpoint on the node reports `Failed` or lacks a bound *RDMA* device | Check kernel dmesg and interface carrier status; operator removes the label when every endpoint reports `Failed` or lacks a bound *RDMA* device. See [Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#three-node-labels) |
| AWS EFA workload remains `Pending` indefinitely | `Workload` requested `device.gpustack.ai/rdma` instead of EFA | EFA nodes expose 0 allocatable for standard *RDMA* keys; configure `transport.protocol: EFA` or `TCP` |
| `ModelDeployment` rejected with `no effective RDMA or EFA transfer leg uses the requested interfaces` | Interface requested with no *RDMA* or EFA path | Pair the request with `kvTransfer.protocol: RDMA` or `EFA` on a *prefill* and *decode* pair, or with a `KVCacheBackend` on that transport. A role with its own *`command`* has no managed direct path |

---

**See also** — [Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md) (discovery and link states) ·
[RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) (resource keys, limits, and EFA configuration) ·
[Heterogeneous Devices Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md)

**Next** → [Hierarchical Placement Walkthrough](/gpustack-operator/main/docs/walkthroughs/topology/hierarchical-placement/index.md)
