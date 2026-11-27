# RDMA Operations

Select a *remote direct memory access (RDMA)* resource key, filter *host channel adapters (HCAs)*
with `DevicesRDMASelectionPolicy`, align kubelet *non-uniform memory access (NUMA)* placement, and
verify granted devices. See [Accelerator Requests](../devices/requests.md) for `Pod` admission
rules.

## Contents

- [RDMA resource requests](#rdma-resource-requests)
- [Selection policy](#selection-policy)
- [Enabling NUMA alignment](#enabling-numa-alignment)
- [Verification](#verification)
- [EFA engine images](#efa-engine-images)
- [Troubleshooting](#troubleshooting)

## RDMA resource requests

Device Manager advertises three RDMA resource keys depending on each interface's configuration:

| Key | Mode | Request unit | Tokens |
|---|---|---|---|
| `device.gpustack.ai/rdma` | Exclusive | One whole interface | `1` per endpoint |
| `device.gpustack.ai/rdma.shared` | Shared | One concurrent share of an interface | `64` per endpoint |
| `device.gpustack.ai/rdma.partitioned` | Partitioned | One *SR-IOV* virtual function | `1` per virtual function |

A *single-root I/O virtualization (SR-IOV)* physical function with virtual functions serves
`device.gpustack.ai/rdma.partitioned` **only**; all other RDMA interfaces serve `rdma` and
`rdma.shared` (accounting for allocations independently across the two families). AWS Elastic Fabric
Adapter (EFA) devices use the AWS plugin key `vpc.amazonaws.com/efa`.

Inspect allocatable RDMA keys across the cluster before writing manifests:

```bash
kubectl get nodes -o json |
  jq -r '.items[] | .metadata.name as $n | .status.allocatable
         | with_entries(select(.key | startswith("device.gpustack.ai/rdma")))
         | select(length > 0) | {node: $n} + .'
```

Prefer `device.gpustack.ai/rdma.shared` unless the workload requires exclusive family reservations
(hardware queue pairs isolate concurrent processes on one adapter). On managed `ModelDeployment`s,
set *`spec.roles[].resources.interface`* instead of raw `Pod` keys
([Model Deployment Configuration](../model-deployment/configuration.md)); EFA paths also require a
custom engine image ([EFA engine images](#efa-engine-images)).

### Endpoints per accelerator

Request **one RDMA endpoint per requested accelerator** (`N` accelerators and `N` endpoints in the
same container). Device plugins publish only *NUMA*-node hints rather than 1:1 PCIe switch pairings,
so kubelet `TopologyManager` controls locality:

- **`none` (Kubernetes default)**: Allocates accelerators and HCAs independently, allowing cross-socket
  assignments with silent throughput loss.
- **`restricted` or `single-numa-node`**: Constrains both allocations to matching *NUMA* node(s),
  leaving exact HCA-to-GPU pairing inside a *NUMA* node to the workload.

Node labels `rdma.distance` and `rdma.numa` are informational node summaries;
`feature.gpustack.ai/rdma.capable=true` gates whether at least one usable RDMA endpoint exists on
the `Node`.

### GPU and RDMA requests

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rdma-trainer
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>  # required for accelerator webhook checks
spec:
  nodeSelector:
    gpu.gpustack.ai/model: b300  # user label identifying verified NIC/GPU hardware layout
  containers:
    - name: trainer
      image: nvcr.io/nvidia/pytorch:24.10-py3
      resources:
        limits:
          nvidia.com/gpu: "8"  # whole accelerators (partition tokens publish no NUMA hint)
          device.gpustack.ai/rdma: "8"  # same container so container-scoped TopologyManager aligns both
```

On `device.gpustack.ai/rdma.shared`, preferred allocation spreads tokens one per healthy HCA in
outer rounds before reusing an HCA, so requesting `N` shared units across `N` healthy HCAs assigns
distinct adapters first.

### Injected devices

Every RDMA allocation (`rdma`, `rdma.shared`, or `rdma.partitioned`) injects deduplicated character
devices and HCA environment variables into the container:

| Injected item | Value |
|---|---|
| `/dev/infiniband/uverbs<N>` | One character device per distinct granted HCA (allocation fails whole if any node is missing). |
| `/dev/infiniband/rdma_cm` | At most one connection-manager device per allocation response, when present on the host. |
| `NCCL_IB_HCA` | Sorted, comma-joined HCA names prefixed with `=` (such as `=mlx5_1,mlx5_10`, or `=mlx5_1:1,mlx5_10:1` when a selection policy pins `port`). |
| `GPUSTACK_RDMA_DEVICES` | Sorted, comma-joined bare HCA names without `=` or port suffixes (`mlx5_1,mlx5_10`). |

```bash
ls /dev/infiniband
echo "$NCCL_IB_HCA"
echo "$GPUSTACK_RDMA_DEVICES"
```

- **Policy exclusion after grant**: If an active `DevicesRDMASelectionPolicy` later excludes all
  endpoints of a new grant, the response injects empty `NCCL_IB_HCA=""` and
  `GPUSTACK_RDMA_DEVICES=""` with no character devices; already-running containers are untouched.
- **Consumer adapters**: `Pod` `env[].value` cannot expand plugin-injected variables, and HIXL or
  NIXL consumers require their own startup adapters.

### RDMA quota limits

Kueue's `ResourceFlavor`s and `gpustack-node-devices` check do not meter `device.gpustack.ai/rdma*`
or `vpc.amazonaws.com/efa`. Over-subscribed `Workload`s therefore become `Admitted` in Kueue while
their `Pod`s wait `Pending` in kube-scheduler:

- **Match GPU and RDMA ratios**: Requesting `1` endpoint per accelerator keeps accelerator quota aligned
  with physical NIC count (exclusive `rdma` exhausts in `1` claim per NIC versus `64` on
  `rdma.shared`).
- **Monitor Pending `Pod`s**: Alert on `Pending` `Pod`s owned by `Admitted` `Workload`s rather than
  queue depth alone.

### Software RDMA

Soft-RoCE (`rxe`) runs *RDMA* in the kernel over an ordinary interface, such as a bond or a plain
NIC. Device Manager records such an interface with `rdma: true`, `rdmaSoftware: true` and a link
verdict, and sets `feature.gpustack.ai/rdma.software=true` when every usable *RDMA* endpoint on the
`Node` is software-carried. The device itself is advertised under its own name, such as `rxe0`.

A `Node` can also carry `feature.gpustack.ai/rdma.external=true`: it has an *RDMA* device under
another owner's allocation, such as the AWS EFA driver's, which GPUStack never allocates. The key
is independent of `rdma.software`, and a `Node` with such a device beside a software one carries
both.

A VLAN device cannot carry Soft-RoCE: the kernel's `rxe_newlink` refuses an 8021Q or 8021ad device
with `rxe creation allowed on top of a real device only`.

Performance is far below a hardware adapter's. To keep a hardware pool off such `Node`s, write a node
affinity that excludes `feature.gpustack.ai/rdma.software`, or exclude the `rxe` devices with a
[selection policy](#selection-policy).

`rxe` is discovered this way. `siw` (iWARP) is not: its ports name no interface in sysfs, so a `Node`
whose only *RDMA* is `siw` carries neither `rdma.capable` nor `rdma.software`.

## Selection policy

`DevicesRDMASelectionPolicy` (`rdmasel`, cluster-scoped) filters which HCAs and physical ports the
RDMA device plugin advertises as healthy on matched `Node`s. Policy updates apply immediately to new
allocations (marking excluded tokens `Unhealthy`) without draining `Node`s or restarting running
containers.

To restrict an 8-GPU host with 13 verbs devices (`mlx5_0..mlx5_3` slow IB, `mlx5_4..mlx5_11`
GPU-local 800 Gb/s IB, `mlx5_12` management Ethernet) to `mlx5_4..mlx5_11`, set either *`include`*
or *`exclude`* (never both):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: DevicesRDMASelectionPolicy  # cluster-scoped, short name rdmasel
metadata:
  name: b300-compute-hcas
spec:
  nodeSelector:  # required, non-null; {} matches all Nodes
    matchLabels:
      gpu.gpustack.ai/model: b300
  include:  # optional non-empty list of exact PF/VF HCA names (mutually exclusive with exclude)
    - mlx5_4
    - mlx5_5
    - mlx5_6
    - mlx5_7
    - mlx5_8
    - mlx5_9
    - mlx5_10
    - mlx5_11
  # exclude:  # alternative to include; omit or set null when unused (never [])
  #   - mlx5_0
  #   - mlx5_12
```

Remove an HCA or pin a 1-based physical RDMA port across all selected HCAs by patching the policy:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: DevicesRDMASelectionPolicy
 metadata:
   name: b300-compute-hcas
 spec:
   nodeSelector:
     matchLabels:
       gpu.gpustack.ai/model: b300
   include:
     - mlx5_4
     - mlx5_5
     - mlx5_6
     - mlx5_7
     - mlx5_8
     - mlx5_9
     - mlx5_10
-    - mlx5_11
+  port: 1  # optional 1-based port; every selected HCA on matched Nodes must expose it
```

Verify resolution on the policy and each `Node`'s `Devices` status:

```bash
kubectl get rdmasel b300-compute-hcas -o yaml
kubectl get devices <node-name> -o yaml
```

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Devices
metadata:
  name: b300-node-0
status:
  interfaces:
    rdmaSelectionPolicy:
      policyRef:
        name: b300-compute-hcas
        uid: example-policy-uid
        generation: 1
      excluded:
        - name: mlx5_0
          reason: ExcludedByPolicy
  conditions:
    - type: RDMASelectionResolved
      status: "True"
      reason: PolicyResolved
```

**Fail-closed rules:** If multiple policies match one `Node`, both *`include`* and *`exclude`* are
set, a named HCA (or wildcards/port suffixes) is absent from a matched `Node`'s verbs inventory, or a
selected HCA lacks *`port`*, that `Node` sets `RDMASelectionResolved` to `False` or `Unknown` and
marks **all** RDMA tokens `Unhealthy`. Omit unused *`include`* / *`exclude`* fields rather than
writing `[]`.

## Enabling NUMA alignment

### Step 1: Check kubelet policy

Kubelet defaults `topologyManagerPolicy` to `none`. Confirm your Kubernetes distribution allows
editing kubelet configuration before relying on *NUMA* alignment.

### Step 2: Configure alignment

Set `topologyManagerPolicy: restricted` and `topologyManagerScope: container` in
`KubeletConfiguration` (via `/var/lib/kubelet/config.yaml`, your distribution's kubelet drop-in
directory, or `--topology-manager-policy`):

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
topologyManagerPolicy: restricted  # aligns single-socket Pods and admits multi-socket 8-GPU Pods
topologyManagerScope: container  # default; aligns GPU and RDMA requests inside each container
```

Prefer `restricted` over `single-numa-node`: `single-numa-node` rejects any container spanning two
sockets (such as an 8-GPU job on a dual-socket host) with terminal `TopologyAffinityError`. See
[Result format](../devices/preflight.md#result-format) for all four policies.

### Step 3: Drain node, restart kubelet

Drain the `Node` before restarting kubelet so existing `Pod`s admitted under `none` are not rejected
in place after restart.

## Verification

### Kubelet policy check

Read the live effective kubelet configuration via `nodes/proxy` (`configz` includes defaults, whereas
`device-manager preflight` reports `unknown` when no file explicitly sets a policy):

```bash
kubectl get --raw /api/v1/nodes/<node>/proxy/configz |
  jq -r '.kubeletconfig | {topologyManagerPolicy, topologyManagerScope}'
```

### NIC record inspection

When a `Node` loses `feature.gpustack.ai/rdma.capable=true` or stops scheduling RDMA `Pod`s, inspect
node labels and the `Devices` NIC inventory:

```bash
kubectl get nodes -l feature.gpustack.ai/rdma.capable=true
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, rdmaSoftware, link})'
```

- **Healthy (`rdma.capable=true`)**: At least one physical NIC, SR-IOV VF or software RDMA interface
  (a bond or NIC with `rdmaSoftware: true`) reports `link.state` `OK` or `Unverified` (or `rdma: true`
  without a link verdict).
- **Missing (`rdma.capable` absent)**: Every endpoint that carries an RDMA record reports
  `link.state: Failed` (with port state and `firstSeen`), or no interface carries one.
- **Recovery**: Repair physical links, transceivers, or the IB subnet manager on `Failed` ports,
  verify verbs driver binding when a physical NIC has `rdmaDevice` absent, and run
  `device-manager preflight` on the host.

## EFA engine images

Official GPUStack runner images carry standard Mooncake wheels without EFA support (while the
`-cpu` [KV cache backend image](../kv-cache/backend.md#backend-images) already includes EFA). Granting an EFA
device to a standard runner causes verbs CQ creation to fail with `Operation not supported`.

For any vLLM role transferring KV over EFA (via an `EFA` store pool or `aws-efa-enabled: true`
direct transfer; see [Engine Compatibility](../model-deployment/engine-versions.md#transport-compatibility)),
build a custom image and set it on *`roles[].image`*.

### Image build

```dockerfile
FROM gpustack/runner:cuda13.0-vllm0.29.0
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
ARG AWS_EFA_VERSION=1.44.0
RUN curl --retry 3 --retry-connrefused -fL "https://efa-installer.amazonaws.com/aws-efa-installer-${AWS_EFA_VERSION}.tar.gz" | tar -zx -C /tmp \
 && cd /tmp/aws-efa-installer && ./efa_installer.sh -y --skip-kmod \
 && rm -f /opt/amazon/efa/lib/libfabric.a && ldconfig \
 && rm -rf /tmp/aws-efa-installer /var/cache/apt /var/lib/apt/lists/*
ENV PATH="${PATH}:/opt/amazon/efa/bin"
# Use mooncake-transfer-engine-efa on CUDA 12 bases.
RUN python3 -m pip uninstall -y mooncake-transfer-engine mooncake-transfer-engine-cuda13 \
 && python3 -m pip install --no-cache-dir --no-deps mooncake-transfer-engine-efa-cuda13==0.3.13.post1
```

Verify `EfaTransport` inside the built image and in engine startup logs:

```bash
MC="$(python3 -c 'import importlib.util as u; print(u.find_spec("mooncake").submodule_search_locations[0])')"
grep -ac EfaTransport "$MC/engine.so"
kubectl logs <engine-pod> | grep 'The Mooncake Transfer Engine is using efa'
```

### Image maintenance

Rebuild *`roles[].image`* whenever upgrading the runner base, `libfabric`, or the Mooncake minor
line ([Version compatibility](../kv-cache/backend.md#version-compatibility)).

### Limitations

- **x86_64 vLLM only**: No `aarch64` EFA wheel is published, intra-node NVLink is omitted, and
  SGLang extensions are absent.
- **Fail-fast without EFA**: Requesting `efa` on a `Node` without EFA hardware fails instead of
  falling back to TCP (`tcp` and `rdma` protocols still work). InfiniBand/RoCE data paths with the
  bundled `rdma-core` are unmeasured.

### SGLang and EFA

SGLang does not support EFA on either direct or store KV paths (direct interface requests without
another fabric are refused at admission, and EFA wheels lack SGLang modules). Run SGLang over `TCP`
on AWS.

### AWS fabric choices

Use `EFA` (`vpc.amazonaws.com/efa`) or `TCP` on AWS EFA instances; `device.gpustack.ai/rdma*`
allocatable counts remain `0` on EFA-only hosts.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| RDMA key absent on all `Node`s | Zero allocatable `device.gpustack.ai/rdma*` | Verify verbs hardware and Device Manager `--no-shared` / `--no-partitioned` flags. |
| Key present, `0` allocatable | Link `Failed` or `RDMASelectionResolved!=True` | Check [Selection policy](#selection-policy) and [NIC record inspection](#nic-record-inspection). |
| `Workload` `Admitted`, `Pod` `Pending` | Unmet `device.gpustack.ai/rdma*` in scheduler events | Free RDMA endpoints or switch to `rdma.shared` ([RDMA quota limits](#rdma-quota-limits)). |
| `Pod` fails with `TopologyAffinityError` | Kubelet rejects cross-socket allocation | Use `topologyManagerPolicy: restricted` and keep GPU and RDMA requests in one container. |
| Fewer `/dev/infiniband` entries than requested | Shared HCA tokens deduplicated | Count `uverbs<N>` nodes (`rdma_cm` appears once); check [Injected devices](#injected-devices). |
| `Node` loses `rdma.capable` label | All NIC/VF links `Failed` | Run [NIC record inspection](#nic-record-inspection) and repair physical links. |

---

**See also** — [Accelerator Requests](../devices/requests.md) (`Pod` request rules and co-location conditions) · [RDMA Networking Walkthrough](../../walkthroughs/rdma/network-endpoints.md) (end-to-end endpoint verification) · [Preflight Checks](../devices/preflight.md) (bare-host RDMA link and kubelet topology checks)

**Next** → [Preflight Checks](../devices/preflight.md) — validate node hardware, slicing, and RDMA links before workloads arrive.
