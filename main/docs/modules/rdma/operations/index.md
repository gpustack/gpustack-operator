# RDMA Operations

Choose a *remote direct memory access (RDMA)* resource key, set the kubelet's
*non-uniform memory access (NUMA)* policy, and check the `Pod`'s
placement. [Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md) explains how endpoints become available;
[Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) lists the admission rules.

## Contents

- [Choosing a key](#choosing-a-key)
- [Injected devices](#injected-devices)
- [Selection policy](#selection-policy)
- [Example manifest](#example-manifest)
- [Enabling NUMA alignment](#enabling-numa-alignment)
- [Verification](#verification)
- [Kueue and RDMA keys](#kueue-and-rdma-keys)
- [EFA engine images](#efa-engine-images)
- [Troubleshooting](#troubleshooting)

## Choosing a key

The three keys differ in allocation mode, in what a quantity of a key means, and in how many tokens
one endpoint carries. [Network
Topology](network-topology.md#rdma-resource-keys) describes those
differences. Choose the key for your workload:

| Key | Target scenario |
|---|---|
| `device.gpustack.ai/rdma` | each requested token must name a different endpoint within this family |
| `device.gpustack.ai/rdma.shared` | everything else |
| `device.gpustack.ai/rdma.partitioned` | the node's adapters are physical functions with virtual functions configured |

**Each interface's configuration determines which families it serves.** A *single-root I/O virtualization (SR-IOV)* physical
function with virtual functions configured serves the partitioned key **only**; every other
interface serves the exclusive and shared keys ([which interface serves
what](network-topology.md#rdma-resource-keys)).
Read the fleet before writing the manifest:

```bash
kubectl get nodes -o json |
  jq -r '.items[] | .metadata.name as $n | .status.allocatable
         | with_entries(select(.key | startswith("device.gpustack.ai/rdma")))
         | select(length > 0) | {node: $n} + .'
```

Ask for the shared key unless the workload needs distinct exclusive-family endpoints. An adapter carries far more
concurrent queue pairs than the tokens an endpoint is advertised for, and several processes on
one adapter is ordinary *RDMA* practice: the isolation is the firmware's and the kernel's, not the
token count's.

The exclusive and shared families account for allocations separately. An exclusive
token reserves an endpoint within that family; the shared family can still grant the same HCA.

For a managed `ModelDeployment`, set *`spec.roles[].resources.interface`* instead of naming the key
in a `Pod` template. [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) states which key a
count selects, and the mixed-fabric refusal. The field allocates a device to each rendered engine
`Pod` and does not assert that the engine has transferred bytes.

**An EFA path also needs an EFA build of Mooncake in the engine image**, which no runner image
carries; see [what an EFA path needs from the engine
image](#efa-engine-images).

### Endpoints per accelerator

**Ask for one *RDMA* endpoint per accelerator the container requests.** A host built for collective
traffic carries one adapter beside each accelerator, so `N` accelerators and `N` endpoints is the
count that matches the machine. Fewer puts several accelerators behind one adapter's bandwidth; more
hands the container adapters that no accelerator sits beside.

> **Why it is not a pairing** — nothing here binds a particular accelerator to a particular adapter.
> The accelerator plugin's tokens name accelerators and the *RDMA* plugin's tokens name endpoints;
> each publishes its own *NUMA* node and nothing finer. The kubelet's TopologyManager is the only
> thing that relates the two sides, and the unit it relates them in is the *NUMA* node.

What each policy leaves you with:

- `none`, the default: the two allocations are **independent**. A container can be handed every
  accelerator on one *NUMA* node and every adapter on the other. Nothing reports it; the `Pod` runs,
  and only the throughput says so.
- `single-numa-node` or `restricted`: both sides land on **one** *NUMA* node, and that is the whole
  guarantee. Where a *NUMA* node carries four accelerators and four adapters, this narrows the choice
  from eight adapters to four and stops: which adapter ends up beside which accelerator is
  unconstrained.

No request can name "this accelerator with the adapter next to it". A workload
that needs that pins itself to a machine shape it has verified and pairs the devices itself, from
the granted names below.

The operator's own `rdma.distance` and `rdma.numa` labels cannot close the gap either. Both describe
the whole node, so neither can say which accelerator ended up beside which adapter in this
container ([what a label can carry](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#three-node-labels)).

## Injected devices

Each allocation response names its granted *host channel adapters (HCAs)* and exposes their devices:

| Container contents | Count |
|---|---|
| the granted HCA's verbs character device, `/dev/infiniband/uverbs<N>` | one per distinct granted HCA name |
| the node-level connection-manager device, `/dev/infiniband/rdma_cm` | **one per response**, however many endpoints were granted, and only where the host has one |
| `NCCL_IB_HCA` | sorted, unique granted HCA names, comma-joined with one leading `=` for exact matching; each name carries a `:<port>` suffix when the active selection policy pins a port |
| `GPUSTACK_RDMA_DEVICES` | the same sorted, unique comma-joined names without `=`, never carrying a port suffix |

All three resource keys, `device.gpustack.ai/rdma`, `device.gpustack.ai/rdma.shared` and
`device.gpustack.ai/rdma.partitioned`, inject `GPUSTACK_RDMA_DEVICES` and `NCCL_IB_HCA`.
Switching modes does not change the grant variable names.

```bash
# Inside the container: granted uverbs nodes and rdma_cm when available.
ls /dev/infiniband
# Exact NCCL filter; for example, =mlx5_1,mlx5_10.
echo "$NCCL_IB_HCA"
# Plain grant list in any mode; for example, mlx5_1,mlx5_10.
echo "$GPUSTACK_RDMA_DEVICES"
```

The example values are illustrative. Repeated tokens or endpoints naming one HCA produce one name
and one `uverbs` mapping. This response deduplication leaves advertised IDs and capacity unchanged.
Allocation fails if a granted HCA's device node cannot be resolved; it returns no partial grant.

When the active `DevicesRDMASelectionPolicy` pins a port, `NCCL_IB_HCA` names each granted HCA
with that port, as in `=mlx5_1:1,mlx5_10:1`, while `GPUSTACK_RDMA_DEVICES` stays bare. NCCL then
opens the port the policy selected rather than any port of the adapter.

When no HCA remains eligible for a grant, the response injects `GPUSTACK_RDMA_DEVICES=""` and
`NCCL_IB_HCA=""` with no character device nodes. This can happen when the active
`DevicesRDMASelectionPolicy` excludes every granted endpoint. Already-running containers remain
untouched.

A container requesting multiple families receives independent responses setting the same variable
names; their merged values do not promise a union. An application startup adapter must build the
selector its transport accepts. A `Pod`'s `env.value` cannot expand variables injected later by the
device plugin.

These values apply to new allocation responses; existing containers and cached responses are not
rewritten. Generic grant variables do not configure HIXL, NIXL or another consumer automatically.
Each consumer needs a supported startup adapter and separate runtime qualification.

The injected set is evidenced for RoCE and for nothing else; what that leaves open on a classic
InfiniBand fabric is [what an allocation hands over](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#allocation-response).

## Selection policy

`DevicesRDMASelectionPolicy` is a cluster-scoped configuration API at `worker.gpustack.ai/v1`.
The Device Plugin controller evaluates it against the node's local *RDMA* inventory. Policy edits
do not trigger hardware rescans in the Detector.

A valid policy update takes effect immediately for subsequent `ListAndWatch` advertisements and
`Allocate` / `GetPreferredAllocation` calls. Unselected endpoints retain their token IDs but report
`Unhealthy`. No node drain is required, and already-allocated containers remain untouched.
Continue to size requests from the node's actual allocatable resources.

Consider an eight-GPU (`NVIDIA B300`) host that exposes 13 verbs devices under
`/sys/class/infiniband`:

- `mlx5_0` through `mlx5_3`: four 100 Gb/s InfiniBand adapters at `SYS` distance to all GPUs, with
  no IPoIB netdev.
- `mlx5_4` through `mlx5_11`: eight 800 Gb/s InfiniBand adapters paired `PIX` 1:1 with the eight
  GPUs across *NUMA* 0 (`mlx5_8..mlx5_11`) and *NUMA* 1 (`mlx5_4..mlx5_7`), with no IPoIB netdev.
- `mlx5_12`: one 400 Gb/s Ethernet adapter backing host `eth0`.

To restrict workloads to the eight 800 Gb/s GPU-local compute adapters (`mlx5_4..mlx5_11`), create a
policy with `include`. Replace the example node label with a label present on each intended node.
A policy can set at most one of `include` and `exclude`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: DevicesRDMASelectionPolicy
metadata:
  name: b300-compute-hcas  # Cluster-scoped policy name.
spec:
  # Required, non-null Kubernetes label selector; {} matches every node.
  nodeSelector:
    matchLabels:
      gpu.gpustack.ai/model: b300
  # Exact HCA names to include; missing or null leaves inclusion unrestricted.
  include:
    - mlx5_4
    - mlx5_5
    - mlx5_6
    - mlx5_7
    - mlx5_8
    - mlx5_9
    - mlx5_10
    - mlx5_11
```

- **`nodeSelector`**: Required and non-null; an empty selector matches every node.
- **`include`**: Optional; list exact HCA names present on each selected node.
- **`exclude`**: Optional; list exact HCA names present on each selected node.
- **`port`**: Optional; select a positive physical port present on each selected HCA.

| Condition | Reason | Resolution |
|---|---|---|
| Both `include` and `exclude` are set | The filters are mutually exclusive | Supply only one filter. |
| More than one policy matches a node | Policies do not merge | Adjust selectors so one policy matches. |
| A named HCA or selected port is absent | The policy cannot resolve on that node | Correct the names, port, or selector. |

Alternatively, configure the same selection with `exclude` to filter out the four 100 Gb/s adapters
and the host Ethernet adapter. Remove `include` when switching to this form; do not apply both forms
as separate matching policies:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: DevicesRDMASelectionPolicy
metadata:
  name: b300-compute-hcas
spec:
  nodeSelector:
    matchLabels:
      gpu.gpustack.ai/model: b300
  # Exact HCA names to exclude; missing or null excludes none.
  exclude:
    - mlx5_0
    - mlx5_1
    - mlx5_2
    - mlx5_3
    - mlx5_12
```

To take `mlx5_11` out of rotation in the `include` form, update the policy manifest and apply it
again:

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
```

To pin every selected adapter to one physical port, add `port` to the same policy:

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
+  # 1-based physical port; every selected HCA must carry it on the node.
+  port: 1
```

Read the policy and each node's `Devices` resource:

```shell
kubectl get rdmasel b300-compute-hcas -o yaml
kubectl get devices <node-name> -o yaml
```

Confirm that `RDMASelectionResolved=True`. Compare *`policyRef`* name, UID and generation with the
policy's metadata. This illustrative excerpt shows `mlx5_4..mlx5_11` selected:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Devices
metadata:
  name: b300-node-0
status:
  interfaces:
    rdmaSelectionPolicy:
      policyRef:  # Policy name, UID and generation selected for resolution.
        name: b300-compute-hcas
        uid: example-policy-uid
        generation: 1
      excluded:  # HCAs excluded by the selection and their reasons.
        - name: mlx5_0
          reason: ExcludedByPolicy
        - name: mlx5_1
          reason: ExcludedByPolicy
        - name: mlx5_2
          reason: ExcludedByPolicy
        - name: mlx5_3
          reason: ExcludedByPolicy
        - name: mlx5_12
          reason: ExcludedByPolicy
  conditions:
    - type: RDMASelectionResolved  # Reports whether the node's selection is resolved.
      status: "True"
      reason: PolicyResolved
```

With no matching policy, successful resolution selects all otherwise eligible candidates.
If a policy is invalid on the node, multiple policies match, or resolution is unknown, the node
fails closed. It selects no HCA, and all *RDMA* tokens report `Unhealthy`. The
`RDMASelectionResolved` condition is `False` or `Unknown`; no previous selection is retained.
Already-allocated containers remain untouched.

Names identify PF or VF HCAs independently. Wildcards, prefix matches and port suffixes are invalid.
Setting both `include` and `exclude` is invalid, even when their names do not overlap. Naming an
HCA in `include` or `exclude` that is absent from a matched node's complete verbs inventory is
also invalid on that node and fails it closed, so a selector spanning nodes with different HCA sets
must not name adapters missing on any matched node.

An optional `port` further restricts the selection to one physical RDMA port per adapter. The
number is 1-based; zero and negatives are refused at admission. Every HCA the policy selects must
observe that port in the node's verbs inventory, so a policy whose selected devices do not carry
it fails the node closed, like any other invalid resolution. Excluded and externally owned devices
are never asked to carry the port.

With `port` set, `GPUSTACK_RDMA_DEVICES` still lists bare HCA names, and the NCCL filter names
each granted HCA with that port. Omit the field, or set it to null, to leave every active port of
each selected HCA eligible.

Multiple policies matching a node conflict; no priority or merge rule selects one. A policy cannot
override an externally owned device's boundary.

The CRD schema requires each supplied list to contain at least one name. Omit a field or set it to
null to remove its restriction. When both fields are absent or null, the policy imposes no HCA filter.
Do not use an empty list: typed forwarding can omit it before the CRD validates the object. Generated
apply builders also omit empty lists. Apply requests retain Kubernetes field ownership and conflict rules.

## Example manifest

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>  # without it, no request rule is checked at all
spec:
  nodeSelector:
    <your-own-label>: <a-machine-shape-you-verified>
  containers:
    - name: trainer
      resources:
        limits:
          nvidia.com/gpu: "8"           # eight whole accelerators
          device.gpustack.ai/rdma: "8"  # eight distinct endpoints, same container
```

Four details in this manifest are load-bearing:

- **Both requests in one container.** The kubelet aligns per container by default, so a request
  split across two containers is aligned by nothing at all.
- **One endpoint per accelerator**, for the reasons above, on the **exclusive** key. This key
  reserves whole endpoints within the exclusive family. `rdma.shared` shares each HCA's token pool
  across workloads. Its `GetPreferredAllocation` and advertised token order use outer rounds over
  endpoints: each round takes one token per eligible endpoint before repeating an endpoint.
  Requesting `N` shared units with `N` healthy, eligible HCAs therefore selects one token per HCA
  first.
- **A nodeSelector on a label you set yourself.** Nothing this operator publishes asserts "one
  adapter per accelerator, evenly split across *NUMA* nodes"; `rdma.capable` says only that at least
  one endpoint on the node is usable. Label the node pools whose shape you have verified, and select
  on that label.
- **Whole accelerators.** An accelerator partition token publishes no *NUMA* hint, so pairing a
  partition profile with an *RDMA* key aligns the *RDMA* side alone; the full condition list is
  [co-locating an accelerator and an *RDMA*
  interface](../devices/requests.md#co-location-conditions).

## Enabling NUMA alignment

### Step 1: Check the policy

Do not assume a cluster already has it: no Kubernetes release turns it on. The TopologyManager is
stable and enabled, but its policy field's default is `none`, which discards every hint a device
plugin publishes. Until somebody sets it, the accelerator and the adapter handed to one container
are two independent picks.

### Step 2: Configure alignment

Drain the node before changing its kubelet policy. Set these fields in `KubeletConfiguration`:

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
topologyManagerPolicy: restricted  # Recommended; default none discards the hint.
topologyManagerScope: container  # Default; accelerator and adapter share a container.
```

See [what each policy does](/gpustack-operator/main/docs/modules/devices/preflight/index.md#reading-the-result).

**Why `restricted` and not `single-numa-node`.** Both gate admission on the hint; they differ on a
container too wide for one *NUMA* node:

- `single-numa-node` admits only an allocation inside one *NUMA* node. On a two-socket host with four
  accelerators per socket it refuses every container asking for eight, with `TopologyAffinityError`;
  a `Pod` the kubelet refuses is not rescheduled.
- `restricted` admits the allocation spanning the fewest *NUMA* nodes that fit. The eight-accelerator
  container lands on both sockets, while a one-accelerator, one-adapter container is still held to a
  single *NUMA* node.

Reach for `single-numa-node` only where every container fits in one *NUMA* node.

`pod` scope merges every container's requests into one alignment, which only makes admission
harder: a `ModelDeployment` renders the accelerator and the adapter into the same main container, so
per-container alignment already covers the pair.

Where those fields go depends on the distribution, and there are three shapes:

- **`/var/lib/kubelet/config.yaml`**, the kubelet configuration file a kubeadm-style install reads.
- **A kubelet drop-in directory.** The kubelet merges every `*.conf` below the directory it is
  pointed at, a later file overriding an earlier one. A distribution that embeds the kubelet
  regenerates its own tree on every start and offers a separate directory for your files, whose
  contents it copies in; a file written into the generated tree is gone at the next restart.
- **The kubelet command line**, `--topology-manager-policy=`. Some distributions expose it as a
  pass-through argument in their own configuration file rather than as a kubelet field.

### Step 3: Drain and restart

**Drain the node before changing it.** The policy decides what the kubelet admits, and the
containers already running were admitted under the old one; a node restarted into a stricter policy
can refuse `Pod`s it was happily running.

On a managed cluster, check that you can set the policy at all before planning a topology around
it. A managed Kubernetes offering commonly does not expose the kubelet's configuration, and
failure is silent: the `Pod` is admitted, the two sides are picked independently, and only the
throughput says so.

## Verification

### Verify the kubelet policy

Editing a file is not the kubelet running what it says. Read it back from the kubelet itself, which
needs permission on the `nodes/proxy` subresource:

```bash
kubectl get --raw /api/v1/nodes/<node>/proxy/configz |
  jq -r '.kubeletconfig | {topologyManagerPolicy, topologyManagerScope}'
```

That is the running kubelet's **effective** configuration, defaults included, so it always answers.
A `none` from it therefore does not distinguish "nobody set it" from "somebody set it to `none`";
it answers what the kubelet will do, which is the question to ask after a change.

`device-manager preflight` answers the other question. Its *`topology`* section reports only a policy
that some readable kubelet configuration **names**, and `unknown` when none does, never the
kubelet's default ([why it refuses to fill one
in](../devices/preflight.md#reading-the-result)). Use it on a bare host before anything is installed, and to
tell a node that was configured from one that was left alone.

The two therefore differ in exactly one case, and it is not a contradiction: on a node nobody
configured, `configz` says `none` and preflight says `unknown`.

## Kueue and RDMA keys

Do not read a queue's admission as an *RDMA* reservation. The *RDMA* keys sit outside every accelerator
family, and the same classifier drives Kueue's node-devices admission, so an *RDMA* request
contributes nothing to a `Workload`'s demand, and no `ResourceFlavor` and no credit can be expressed
for it.

A queued `Workload` can therefore be admitted far past the fleet's real *RDMA* capacity, and the excess
surfaces as **`Pod`s that will not schedule** rather than as a queue that waits. The queue reports the
`Workload` admitted while its `Pod` sits `Pending`, because no node advertises a free endpoint.

Three things to do instead of a quota:

- **Size the accelerator quota so its accelerators never need more endpoints than the fleet
  carries.** One endpoint per accelerator makes the two counts the same number, which is part of why
  that is the count to ask for.
- **Watch Pending `Pod`s belonging to admitted `Workload`s**, not queue depth. That pair is where the
  over-subscription becomes visible; queue depth stays at zero throughout.
- **Remember which key runs out first.** An endpoint carries one exclusive token and many more
  shared ones ([how many of each](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#rdma-resource-keys)),
  so a fleet exhausts the exclusive key long before the shared one on the same hardware.

## EFA engine images

Which engine can use EFA on which path, beside every other transport, is [the transport
matrix](../model-deployment/engine-versions.md#transport-compatibility). For a cell marked
"own image", build an engine image with EFA support as follows.

The engine image needs an EFA build of Mooncake, and no runner image carries one. The
runner images carry the standard build, which has no EFA transport. Granted an EFA device, an
engine on that build falls to the verbs path, fails creating its completion queue with `Operation
not supported`, and never starts: the `Pod` restarts in a loop. Without the grant it moves KV over TCP
and says nothing.

**Both transfer paths require the engine's EFA build.** A store group on `EFA` hands the engines it serves the
protocol `EFA`, and `kvTransfer.protocol: EFA` renders `efa` into both ends of a direct pair, so
every engine `Pod` on either path needs the EFA build. The store members do not: the
[default member image](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#image) already carries EFA.

**Nothing more is needed from the operator.** An image differing from the vLLM runner image only by
the EFA build of the same client, plus libfabric, completed a direct transfer and a store write over
EFA with only the device grant the operator renders: no host network, no mount, and no added
capability.

**The operator does not pick that image.** Build it, then name it in *`roles[].image`* on every role
that moves KV over EFA. A role naming no image gets the runner image, whatever its path's protocol.

### Building the image

Derive it from the runner image the deployment would otherwise run:

```dockerfile
FROM gpustack/runner:cuda13.0-vllm0.29.0
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
ARG AWS_EFA_VERSION=1.44.0
RUN curl --retry 3 --retry-connrefused -fL "https://efa-installer.amazonaws.com/aws-efa-installer-${AWS_EFA_VERSION}.tar.gz" | tar -zx -C /tmp \
 && cd /tmp/aws-efa-installer && ./efa_installer.sh -y --skip-kmod \
 && rm -f /opt/amazon/efa/lib/libfabric.a && ldconfig \
 && rm -rf /tmp/aws-efa-installer /var/cache/apt /var/lib/apt/lists/*
ENV PATH="${PATH}:/opt/amazon/efa/bin"
# Same Mooncake version as the base, EFA build. Remove the standard build under either name;
# on a CUDA 12 base install mooncake-transfer-engine-efa instead of the -cuda13 wheel.
RUN python3 -m pip uninstall -y mooncake-transfer-engine mooncake-transfer-engine-cuda13 \
 && python3 -m pip install --no-cache-dir --no-deps mooncake-transfer-engine-efa-cuda13==0.3.13.post1
```

The image also needs libfabric. The EFA wheel links `libfabric.so.1` and does not bundle
it, so without the installer step Mooncake fails at import. `--skip-kmod` installs the user-space
half only; the kernel module belongs to the node.

Check the build before deploying it, and the engine once it runs:

```bash
# inside the image: non-zero means the EFA transport is compiled in
MC="$(python3 -c 'import importlib.util as u; print(u.find_spec("mooncake").submodule_search_locations[0])')"
grep -ac EfaTransport "$MC/engine.so"
# the engine's log once it has started
kubectl logs <engine-pod> | grep 'The Mooncake Transfer Engine is using efa'
```

### Image maintenance

The image is yours from then on, and nothing in the operator tracks it:

- **The base.** Rebuild whenever the deployment moves to another runner image or engine version; the
  operator keeps rendering the image you named, not the one it would assemble.
- **The Mooncake version.** Install the EFA build of the version the base carries (which client
  each runner image carries is [its table](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md#minimum-versions))
  and keep the store on that client's minor line: [the store version must match the engine's
  client](../kv-cache/backend.md#image).
- **libfabric**, from the EFA installer, whose version the recipe pins.
- **The image on every EFA role.** A new role, or a role you re-create, runs the runner image until
  you name yours on it.

### Limitations

Measured on the `0.3.13.post1` wheels. These are why the runner images keep the standard build:

- **x86_64 only.** No aarch64 EFA wheel is published.
- **No intra-node NVLink transport**, which the standard build carries.
- **Missing Mooncake extensions required by SGLang**, so this recipe covers vLLM images only.
- **No silent fallback.** Asked for `efa` on a node without an EFA device it fails, where the
  standard build falls back to TCP. With libfabric installed, `tcp`, `rdma` and the store still work
  on such a node.
- **Not measured:** the *RDMA* data path on InfiniBand or RoCE hardware with the `rdma-core` the
  installer brings.

### SGLang and EFA

No SGLang path reaches EFA. On the direct path the operator grants SGLang no fabric
device: SGLang does not hand the declared protocol to its transfer engine, so the declaration cannot
select a grant, and a positive *`resources.interface`* with no other fabric path is refused at
admission.

On the store path the operator renders the pool's protocol into SGLang's `MOONCAKE_PROTOCOL`, but
SGLang's runner image carries a Mooncake with no EFA transport, and the EFA build lacks the modules
SGLang loads. Run SGLang on AWS over `TCP`, which has been run there.

### RDMA on AWS

**Choose `EFA` or `TCP` on AWS.** The Device Manager discovers no *RDMA* device on an EFA node, so
the `device.gpustack.ai/rdma` keys are allocatable at zero there, and a member group or engine that
asks for them stays `Pending`. Granting the EFA device instead does not help `rdma` either: the
verbs path fails creating its completion queue, as above.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| no node advertises the key at all | Status: the key is absent from every node's allocatable | The node has no *RDMA*-capable interface, or that family is switched off on the Device Manager. Run the allocatable read above, and check the `--no-shared` and `--no-partitioned` switches |
| the key is advertised, every token unhealthy | Status: endpoint link `Failed` or HCA unselected | Check link verdicts and the [selection policy status](#selection-policy). Failed or unselected endpoints retain their tokens but report `Unhealthy`. [Read the inventory](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#inspecting-the-record) |
| the `Workload` is admitted and its `Pod` stays `Pending` | Status: `Pod` phase `Pending` | Over-subscription: nothing meters these keys. See [the section above](#kueue-and-rdma-keys) |
| the `Pod` is scheduled, then refused on the node and not rescheduled | The kubelet refuses the `Pod` on the node | The node's totals sufficed but its devices sit on two *NUMA* nodes. Check the node's policy, and that both requests sit in one container |
| the container starts with fewer devices than expected | Fewer `uverbs` entries than granted endpoints | One `rdma_cm` is correct — count the `uverbs` entries. See [the injected set](#injected-devices) |
| a node drops out of scheduling without changing | Status: the node's link state in its `Devices` record | The hardware stays, the label does not. [Read it yourself](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md#inspecting-the-record) |

---

**See also** — [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) (the normative request contract
these keys sit beside, and the rules they are exempt from) ·
[Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md) (what the keys are, which endpoint serves
each, and what an allocation resolves) · [Preflight Checks](/gpustack-operator/main/docs/modules/devices/preflight/index.md) (reading a node's link
verdicts and its topology policy before anything is installed)

**Next** → [Preflight Checks](/gpustack-operator/main/docs/modules/devices/preflight/index.md) — what a node can detect, slice and manage, read on
the bare host.
