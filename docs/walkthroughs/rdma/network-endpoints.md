# RDMA Network Endpoints Walkthrough

Request and verify RDMA endpoints for workloads, align network adapters with GPUs on NUMA nodes,
and configure multi-tenant and cache fabric interconnects.

On a cluster with RDMA-capable hardware and GPUStack installed, follow these steps to verify
interface discovery, container injection, NUMA alignment, and deployment integration.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: request RDMA from a Pod](#step-1-request-rdma-from-a-pod)
- [Step 2: NUMA alignment with kubelet](#step-2-numa-alignment-with-kubelet)
- [Step 3: prefill and decode interface requests](#step-3-prefill-and-decode-interface-requests)
- [Step 4: KV cache fabric over RDMA](#step-4-kv-cache-fabric-over-rdma)
- [Step 5: AWS EFA differences](#step-5-aws-efa-differences)
- [Troubleshooting](#troubleshooting)

## Prerequisites

Device Manager discovers network interfaces from `/sys/class/net`, resolves bound RDMA devices, and
verifies link status before publishing endpoints.

When at least one endpoint is active, the node receives the `feature.gpustack.ai/rdma.capable=true`
label (see [Network Topology](../../modules/rdma/network-topology.md)).
Device Manager registers three extended resource keys with the kubelet
([The RDMA resource keys](../../modules/rdma/network-topology.md#the-rdma-resource-keys)):

| Resource key | Mode | Quantity `1` represents | Capacity per port |
|---|---|---|---|
| `device.gpustack.ai/rdma.shared` | Shared | Concurrent share on an adapter | 64 |
| `device.gpustack.ai/rdma` | Exclusive | One dedicated physical adapter | 1 |
| `device.gpustack.ai/rdma.partitioned` | Partitioned | One SR-IOV Virtual Function (VF) | 1 per VF |

Inspect discovered interfaces and allocatable capacities:

```bash
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, link})'

kubectl get node <node> -o json |
  jq '.status.allocatable | with_entries(select(.key | contains("gpustack.ai/rdma")))'
```

## Step 1: request RDMA from a Pod

A workload requests whole accelerators and RDMA endpoints within the same container.
Pods must carry the `kueue.x-k8s.io/queue-name` label to participate in admission.
Name the entrance `LocalQueue` of the pool that holds the accelerators.
LocalQueue names look like `gpustack-fnv64-<hash>`. List them with `kubectl -n team-a get localqueues`:

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
(see [Injected devices](../../modules/rdma/operations.md#injected-devices)).

Prefer `device.gpustack.ai/rdma.shared` when a container needs a single endpoint. Use the exclusive
key `device.gpustack.ai/rdma` when requesting multiple distinct network endpoints.

## Step 2: NUMA alignment with kubelet

Kubernetes does not pair specific accelerators with adjacent network adapters by default.
Enable NUMA alignment by setting the TopologyManager policy in `/var/lib/kubelet/config.yaml`
(see [Enabling NUMA alignment on the kubelet](../../modules/rdma/operations.md#enabling-numa-alignment-on-the-kubelet)):

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

`restricted` ensures that single-socket requests land on one NUMA node, while multi-GPU workloads
spanning multiple sockets are packed into the minimum number of NUMA nodes.

## Step 3: prefill and decode interface requests

In a `ModelDeployment`, roles do not declare raw resource keys. Specify the number of network
interfaces under `roles[].resources.interface`.
The operator refuses an interface request when no RDMA or EFA transfer leg uses it.
A prefill and decode pair with `spec.kvTransfer.protocol: RDMA` is such a leg:

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
    version: "0.29.0"
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
(see [A minimal deployment](../../modules/model-deployment/deployment.md#a-minimal-deployment)).

The same request without `router`, `kvTransfer` or a cache backend that uses RDMA fails at admission with
"no effective RDMA or EFA transfer leg uses the requested interfaces".
A cache backend on RDMA is the other leg, and Step 4 sets it up.

## Step 4: KV cache fabric over RDMA

To route KV cache transport over an RDMA fabric instead of TCP, edit the `mooncake-dram` backend
from [Step 1 of the Shared Cache walkthrough](../kv-cache/shared-cache.md#step-1-the-store).
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

Member DaemonSet Pods automatically receive the network devices, host privileges, and limits
required for high-performance transport (see [The members](../../modules/kv-cache/backend.md#the-members)).

## Step 5: AWS EFA differences

On AWS Elastic Fabric Adapter (EFA) instances, RDMA devices bypass `/sys/class/net` and do not expose
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

## Troubleshooting

| Symptom | Root cause | Check & Mitigation |
|---|---|---|
| RDMA resource keys present but all `Unhealthy` | All port links failed or unreadable | Inspect `kubectl get devices <node> -o json` for `.spec.interfaces[].link`; verify physical cable and subnet manager |
| Workload admitted by Kueue but Pod remains `Pending` | Over-admission: Kueue does not meter network resources | Inspect cluster allocatable RDMA keys (`device.gpustack.ai/rdma*`); add nodes or wait for active pods to finish |
| Pod rejected with `TopologyAffinityError` | Free GPUs and NICs reside on mismatched NUMA nodes | Ensure kubelet has `topologyManagerPolicy: restricted`; verify GPU and NIC requests are declared in the same container |
| Fewer `uverbs` devices inside container than expected | Shared tokens coalesced into duplicate endpoints | Requesting `rdma.shared: N` (N>1) may merge tokens from the same NIC; use `device.gpustack.ai/rdma: N` for distinct adapters |
| Node unexpectedly loses `feature.gpustack.ai/rdma.capable` | Physical link dropped on all ports | Check kernel dmesg and interface carrier status; operator removes label when all ports report `Failed` |
| AWS EFA workload remains `Pending` indefinitely | Workload requested `device.gpustack.ai/rdma` instead of EFA | EFA nodes expose 0 allocatable for standard RDMA keys; configure `transport.protocol: EFA` or `TCP` |
| `ModelDeployment` rejected with "no effective RDMA or EFA transfer leg" | Interface requested with no RDMA or EFA leg | Pair the request with `kvTransfer.protocol: RDMA` or `EFA` on a prefill and decode pair, or with a `KVCacheBackend` on that transport. A role with its own `command` has no managed direct leg |

---

**See also** — [Network Topology](../../modules/rdma/network-topology.md) (discovery and link states) ·
[RDMA Operations](../../modules/rdma/operations.md) (resource keys, limits, and EFA configuration) ·
[Scheduling Walkthrough](../devices/scheduling.md)

**Next** → [Hierarchical Placement Walkthrough](../topology/hierarchical-placement.md)
