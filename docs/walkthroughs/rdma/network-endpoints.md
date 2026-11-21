# RDMA Networking Walkthrough

Request and verify *remote direct memory access (RDMA)* endpoints alongside accelerators, align NICs
and GPUs on the same *non-uniform memory access (NUMA)* socket through kubelet, and attach fabric
interfaces to *prefill/decode (PD)* roles and KV cache backends.

## Contents

- [Summary](#summary)
- [Node configuration](#node-configuration)
- [Workload configuration](#workload-configuration)
- [AWS EFA configuration](#aws-efa-configuration)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

Device Manager discovers NICs from `/sys/class/net` and InfiniBand verbs devices from
`/sys/class/infiniband`, checks link health, and advertises shared (`rdma.shared`), exclusive (`rdma`),
and *single-root I/O virtualization (SR-IOV)* resource keys. Nodes with at least one healthy endpoint
carry `feature.gpustack.ai/rdma.capable=true` ([RDMA Operations](../../modules/rdma/operations.md#rdma-resource-requests)).

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `Pod` | namespace | user | Standalone workload requesting *RDMA* endpoints alongside GPUs |
| `ModelDeployment` | namespace | user | Managed serving roles requesting fabric counts via *`roles[].resources.interface`* |
| `KVCacheBackend` | cluster | administrator | Shared Mooncake cache configured for `RDMA` or `EFA` transport |

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack installed (illustrative configuration examples) |
| Hardware | Workload `Node`s equipped with active *RDMA*-capable NICs (or AWS EFA adapters) |
| Tools | `kubectl`, `jq`, and node SSH access for Step 2 kubelet configuration |

## Node configuration

### Step 1: Request RDMA endpoints

Inspect discovered *RDMA* interfaces in `Devices` and verify positive allocatable `rdma` keys on your
target `Node`:

```bash
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, link})'

kubectl get node <node> -o json |
  jq '.status.allocatable | with_entries(select(.key | contains("gpustack.ai/rdma")))'
```

Example output (illustrative):

```json
{
  "device.gpustack.ai/rdma": "2",
  "device.gpustack.ai/rdma.shared": "200"
}
```

If allocatable counts are `"0"` or absent, inspect link states and any `DevicesRDMASelectionPolicy`
in [Troubleshooting](#troubleshooting).

Next, look up the pool's `LocalQueue` in `team-a` (`kubectl -n team-a get localqueues`), substitute
its `gpustack-fnv64-<hash>` name, and apply `trainer-0.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: trainer-0
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-<hash>  # required for Kueue admission
spec:
  restartPolicy: Never
  containers:
    - name: trainer
      image: ubuntu:24.04
      command:
        - sleep
        - "3600"
      resources:
        limits:
          nvidia.com/gpu: "1"
          device.gpustack.ai/rdma.shared: "1"  # prefer rdma.shared unless exclusive HCA ownership is needed
```

```bash
kubectl apply -f trainer-0.yaml
kubectl -n team-a wait --for=condition=Ready pod/trainer-0 --timeout=120s
kubectl -n team-a exec trainer-0 -- sh -c 'env | grep -E "^(GPUSTACK_RDMA_DEVICES|NCCL_IB_HCA)=" && ls /dev/infiniband'
```

Example output (illustrative):

```text
GPUSTACK_RDMA_DEVICES=mlx5_0
NCCL_IB_HCA==mlx5_0
rdma_cm
uverbs0
```

Confirm `GPUSTACK_RDMA_DEVICES` is non-empty and `/dev/infiniband/uverbs*` exists; see
[Injected devices](../../modules/rdma/operations.md#injected-devices). `GetPreferredAllocation` spreads
`rdma.shared` tokens across distinct eligible HCAs before repeating an HCA, so requesting more shared
units than eligible HCAs yields one `uverbs` node per HCA.

### Step 2: Configure kubelet alignment

By default, kubelet allocates GPUs and NICs independently across sockets. Enforce *NUMA* co-location
in `/var/lib/kubelet/config.yaml` on each GPU node
([Enabling NUMA alignment](../../modules/rdma/operations.md#enabling-numa-alignment)):

```diff
 apiVersion: kubelet.config.k8s.io/v1beta1
 kind: KubeletConfiguration
+topologyManagerPolicy: restricted
+topologyManagerScope: container
```

Restart `kubelet` on the host and verify the active policy from the API server:

```bash
systemctl restart kubelet
kubectl get --raw /api/v1/nodes/<node>/proxy/configz |
  jq '.kubeletconfig | {topologyManagerPolicy, topologyManagerScope}'
```

Example output (illustrative):

```json
{
  "topologyManagerPolicy": "restricted",
  "topologyManagerScope": "container"
}
```

With `restricted` and `container` scope, single-socket requests bind GPUs and NICs on one *NUMA* node,
and multi-socket requests pack into the fewest *NUMA* nodes. If a `Pod` fails placement with
`TopologyAffinityError`, free co-located GPU/NIC pairs or verify both resources sit in the same
container.

## Workload configuration

### Step 3: Request interfaces per role

In `ModelDeployment`, request fabric adapters through *`roles[].resources.interface`* rather than raw
extended resource keys. Admission accepts *`interface`* only when an active *RDMA* or EFA transfer path
uses it (such as a managed *PD* pair or an *RDMA*/*EFA* `KVCacheBackend`). Save and apply `qwen-pd.yaml`:

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
    backend: Mooncake
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: "2"
        interface: "2"  # 1 -> rdma.shared: 1; >1 -> rdma: N distinct physical HCAs
    - name: decode
      kind: Decode
      replicas: 1
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: "2"
        interface: "2"
```

```bash
kubectl apply -f qwen-pd.yaml
kubectl -n team-a get pods -l app.kubernetes.io/instance=qwen-pd,app.kubernetes.io/component=prefill \
  -o json | jq '.items[0].spec.containers[0].resources.limits'
```

Example output (illustrative):

```json
{
  "device.gpustack.ai/rdma": "2",
  "nvidia.com/gpu": "2"
}
```

If admission rejects the manifest with `no effective RDMA or EFA transfer leg uses the requested interfaces`,
ensure both *Prefill* and *Decode* roles are managed (no custom *`command`*) with a router, or attach
an *RDMA* KV cache pool from Step 4.

### Step 4: Configure RDMA cache

Switch `mooncake-dram` from [Step 1 of Shared Cache](../kv-cache/shared-cache.md#step-1-create-cache-backend)
from TCP to `RDMA` and allocate two fabric interfaces per member:

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

Verify that the backend rolls its member `DaemonSet` and returns to `Ready` with expected capacity:

```bash
kubectl get kvcb mooncake-dram
```

If `MembersMounted` turns `False`, check that every node matched by *`nodeSelector`* advertises at
least two healthy `device.gpustack.ai/rdma` endpoints ([Member groups](../../modules/kv-cache/backend.md#member-groups)).

## AWS EFA configuration

AWS Elastic Fabric Adapter (EFA) devices expose `vpc.amazonaws.com/efa` rather than standard verbs
completion queues, so `device.gpustack.ai/rdma*` stays zero on EFA nodes. Set `transport.protocol: EFA`
on `KVCacheBackend`:

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

For `ModelDeployment` roles on EFA, enable `Setting/aws-efa-enabled` in `gpustack-system` and supply
runner images built with `libfabric` and EFA-enabled Mooncake:

```bash
kubectl -n gpustack-system patch setting aws-efa-enabled --type merge -p '{"spec":{"value":"true"}}'
kubectl -n gpustack-system get setting aws-efa-enabled -o jsonpath='{.spec.value}{"\n"}'
```

## Limitations

- Kueue does not meter `device.gpustack.ai/rdma*` or `vpc.amazonaws.com/efa` in `ClusterQueue` quotas,
  so Kueue can admit a `Workload` that then waits `Pending` until node fabric frees up
  ([RDMA Operations](../../modules/rdma/operations.md)).
- Kubelet *NUMA* policy trade-offs (`none`, `restricted`, `single-numa-node`) are detailed in
  [Enabling NUMA alignment](../../modules/rdma/operations.md#enabling-numa-alignment) and
  [Result format](../../modules/devices/preflight.md#result-format).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `rdma` keys report `0` allocatable | Links `Failed` or filtered out by policy | Inspect `kubectl get devices <node> -o json` and `DevicesRDMASelectionPolicy` ([Selection policy](../../modules/rdma/operations.md#selection-policy)); fix cabling, subnet manager, or policy filters (never hand-edit `Devices` status). |
| `Workload` admitted by Kueue, `Pod` stays `Pending` | Insufficient free node `device.gpustack.ai/rdma*` | Kueue does not meter network keys; check `Node.status.allocatable` and active `Pod` requests, then free endpoints or lower *`resources.interface`*. |
| `Pod` fails placement with `TopologyAffinityError` | Free GPUs and NICs sit on different *NUMA* sockets | Confirm `topologyManagerPolicy: restricted` in `/var/lib/kubelet/config.yaml` and keep GPU and *RDMA* requests in the same container. |
| Fewer `uverbs` devices mounted than `rdma.shared` count | Shared units exceed eligible HCA count | Expected round-robin behavior; switch to `device.gpustack.ai/rdma` (`interface > 1`) when each unit requires its own physical HCA. |
| `Node` loses `feature.gpustack.ai/rdma.capable` | Every endpoint on the host is `Failed` or unbound | Check host `dmesg` and NIC carrier state ([NIC record inspection](../../modules/rdma/operations.md#nic-record-inspection)); Device Manager restores the label automatically once a link recovers. |
| AWS EFA `Pod` stays `Pending` | Requested `device.gpustack.ai/rdma*` on EFA host | Patch `Setting/aws-efa-enabled` to `"true"` for `ModelDeployment`, or set `transport.protocol: EFA` on `KVCacheBackend`. |
| `ModelDeployment` rejected: `no effective RDMA or EFA transfer leg uses the requested interfaces` | *`resources.interface`* set without an active fabric leg | Pair managed *Prefill* and *Decode* roles behind a router, attach an `RDMA`/`EFA` KV cache pool, or remove *`resources.interface`*. |

---

**See also** — [RDMA Operations](../../modules/rdma/operations.md) (resource keys, selection policies,
and EFA setup) · [Accelerator Requests](../../modules/devices/requests.md#rdma-resource-requests)
(combining accelerator and fabric requests) · [Heterogeneous Devices Walkthrough](../devices/scheduling.md)
(pool scheduling run)

**Next** → [Hierarchical Placement Walkthrough](../topology/hierarchical-placement.md) — constrain serving replicas across zones and racks.
