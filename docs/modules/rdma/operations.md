# RDMA Operations

Choose an RDMA resource key for the workload, set the kubelet's NUMA policy, and check the Pod's
placement. [Network Topology](network-topology.md) explains how endpoints become available;
[Accelerator Requests](../devices/requests.md) lists the admission rules.

## Contents

- [Which key a workload asks for](#which-key-a-workload-asks-for)
- [How many endpoints beside N accelerators](#how-many-endpoints-beside-n-accelerators)
- [Injected devices](#injected-devices)
- [Example manifest](#example-manifest)
- [Enabling NUMA alignment on the kubelet](#enabling-numa-alignment-on-the-kubelet)
- [Verifying the policy](#verifying-the-policy)
- [Kueue and the RDMA keys](#kueue-and-the-rdma-keys)
- [EFA engine images](#efa-engine-images)
- [When a Pod does not schedule](#when-a-pod-does-not-schedule)

## Which key a workload asks for

The three keys differ in allocation mode, in what a quantity of a key means, and in how many tokens
one endpoint carries. [Network
Topology](network-topology.md#the-rdma-resource-keys) describes those
differences. Choose the key for your workload:

| Key | Use when |
|---|---|
| `device.gpustack.ai/rdma` | the workload must be the adapter's only tenant |
| `device.gpustack.ai/rdma.shared` | everything else |
| `device.gpustack.ai/rdma.partitioned` | the node's adapters are physical functions with virtual functions configured |

**A node serves one branch or the other, and nothing in a manifest chooses it.** An SR-IOV physical
function with virtual functions configured serves the partitioned key **only**; every other
interface serves the exclusive and shared keys ([which interface serves
what](network-topology.md#the-rdma-resource-keys)).
Read the fleet before writing the manifest:

```bash
kubectl get nodes -o json |
  jq -r '.items[] | .metadata.name as $n | .status.allocatable
         | with_entries(select(.key | startswith("device.gpustack.ai/rdma")))
         | select(length > 0) | {node: $n} + .'
```

Ask for the shared key unless the workload needs sole tenancy. An adapter carries far more
concurrent queue pairs than the tokens an endpoint is advertised for, and several processes on
one adapter is ordinary RDMA practice: the isolation is the firmware's and the kernel's, not the
token count's. Taking a whole adapter exclusively removes it from every other tenant on the node and
buys nothing the hardware was not already doing.

For a managed `ModelDeployment`, set `spec.roles[].resources.interface` instead of naming the key
in a Pod template. [Model Deployment](../model-deployment/deployment.md) states which key a
count selects, and the mixed-fabric refusal. The field allocates a device to each rendered engine
Pod and does not assert that the engine has transferred bytes.

**An EFA leg also needs an EFA build of Mooncake in the engine image**, which no runner image
carries; see [what an EFA leg needs from the engine
image](#efa-engine-images).

## How many endpoints beside N accelerators

**Ask for one RDMA endpoint per accelerator the container requests.** A host built for collective
traffic carries one adapter beside each accelerator, so `N` accelerators and `N` endpoints is the
count that matches the machine. Fewer puts several accelerators behind one adapter's bandwidth; more
hands the container adapters that no accelerator sits beside.

> **Why it is not a pairing** — nothing here binds a particular accelerator to a particular adapter.
> The accelerator plugin's tokens name accelerators and the RDMA plugin's tokens name endpoints;
> each publishes its own NUMA node and nothing finer. The kubelet's TopologyManager is the only
> thing that relates the two sides, and the unit it relates them in is the NUMA node.

What each policy leaves you with:

- `none`, the default: the two allocations are **independent**. A container can be handed every
  accelerator on one NUMA node and every adapter on the other. Nothing reports it; the Pod runs,
  and only the throughput says so.
- `single-numa-node` or `restricted`: both sides land on **one** NUMA node, and that is the whole
  guarantee. Where a NUMA node carries four accelerators and four adapters, this narrows the choice
  from eight adapters to four and stops: which adapter ends up beside which accelerator is
  unconstrained.

No request can name "this accelerator with the adapter next to it". A workload
that needs that pins itself to a machine shape it has verified and pairs the devices itself, from
the granted names below.

The operator's own `rdma.distance` and `rdma.numa` labels cannot close the gap either. Both describe
the whole node, so neither can say which accelerator ended up beside which adapter in this
container ([what a label can carry](network-topology.md#the-three-node-labels)).

## Injected devices

| Container contents | Count |
|---|---|
| the endpoint's verbs character device, `/dev/infiniband/uverbs<N>` | one per granted endpoint |
| the node-level connection-manager device, `/dev/infiniband/rdma_cm` | **one per response**, however many endpoints were granted, and only where the host has one |
| the `NCCL_IB_HCA` environment variable | one, naming the granted RDMA devices, comma-joined |

```bash
# inside the container: one uverbs entry per granted endpoint, and exactly one rdma_cm
ls /dev/infiniband
# the same devices by name, comma-joined
echo "$NCCL_IB_HCA"
```

Two granted endpoints therefore show **two** `uverbs` entries, **one** `rdma_cm`, and two names in
`NCCL_IB_HCA`. The single connection manager is the correct answer rather than a half grant: it
belongs to the node, not to an endpoint, so it is injected once whatever was granted ([what an
allocation hands over](network-topology.md#the-allocation-response)).

The injected set is evidenced for RoCE and for nothing else; what that leaves open on a classic
InfiniBand fabric is [what an allocation hands over](network-topology.md#the-allocation-response).

## Example manifest

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>       # without it, no request rule is checked at all
spec:
  nodeSelector:
    <your-own-label>: <a-machine-shape-you-verified>
  containers:
    - name: trainer
      resources:
        limits:
          nvidia.com/gpu: "8"                      # eight whole accelerators
          device.gpustack.ai/rdma: "8"             # eight distinct endpoints, same container
```

Four details in this manifest are load-bearing:

- **Both requests in one container.** The kubelet aligns per container by default, so a request
  split across two containers is aligned by nothing at all.
- **One endpoint per accelerator**, for the reasons above, on the **exclusive** key. A shared-key
  quantity above one does not promise distinct endpoints: the RDMA plugin offers no preferred
  allocation, so kubelet may hand one container several shared tokens of the same endpoint, and
  Allocate collapses them into that one endpoint; `rdma.shared: "8"` can report satisfied with
  fewer than eight `uverbs` devices. The shared key is right for a container that needs one.
- **A nodeSelector on a label you set yourself.** Nothing this operator publishes asserts "one
  adapter per accelerator, evenly split across NUMA nodes"; `rdma.capable` says only that at least
  one endpoint on the node is usable. Label the node pools whose shape you have verified, and select
  on that label.
- **Whole accelerators.** An accelerator partition token publishes no NUMA hint, so pairing a
  partition profile with an RDMA key aligns the RDMA side alone; the full condition list is
  [co-locating an accelerator and an RDMA
  interface](../devices/requests.md#co-locating-an-accelerator-and-an-rdma-interface).

## Enabling NUMA alignment on the kubelet

Do not assume a cluster already has it: no Kubernetes release turns it on. The TopologyManager is
stable and enabled, but its policy field's default is `none`, which discards every hint a device
plugin publishes. Until somebody sets it, the accelerator and the adapter handed to one container
are two independent picks.

Two fields, in the kubelet's own `KubeletConfiguration`:

| Field | Default | Set to |
|---|---|---|
| `topologyManagerPolicy` | `none` — the hint is discarded | **`restricted`**; [what each of the four does](../devices/preflight.md#reading-the-result) |
| `topologyManagerScope` | `container` — each container is aligned on its own | leave it: the accelerator and the adapter already share a container |

**Why `restricted` and not `single-numa-node`.** Both gate admission on the hint; they differ on a
container too wide for one NUMA node:

- `single-numa-node` admits only an allocation inside one NUMA node. On a two-socket host with four
  accelerators per socket it refuses every container asking for eight, with `TopologyAffinityError`;
  a Pod the kubelet refuses is not rescheduled.
- `restricted` admits the allocation spanning the fewest NUMA nodes that fit. The eight-accelerator
  container lands on both sockets, while a one-accelerator, one-adapter container is still held to a
  single NUMA node.

Reach for `single-numa-node` only where every container fits in one NUMA node.

`pod` scope merges every container's requests into one alignment, which only makes admission
harder: a ModelDeployment renders the accelerator and the adapter into the same main container, so
per-container alignment already covers the pair.

Where those fields go depends on the distribution, and there are three shapes:

- **`/var/lib/kubelet/config.yaml`**, the kubelet configuration file a kubeadm-style install reads.
- **A kubelet drop-in directory.** The kubelet merges every `*.conf` below the directory it is
  pointed at, a later file overriding an earlier one. A distribution that embeds the kubelet
  regenerates its own tree on every start and offers a separate directory for your files, whose
  contents it copies in; a file written into the generated tree is gone at the next restart.
- **The kubelet command line**, `--topology-manager-policy=`. Some distributions expose it as a
  pass-through argument in their own configuration file rather than as a kubelet field.

**Drain the node before changing it.** The policy decides what the kubelet admits, and the
containers already running were admitted under the old one; a node restarted into a stricter policy
can refuse Pods it was happily running.

On a managed cluster, check that you can set the policy at all before planning a topology around
it. A managed Kubernetes offering commonly does not expose the kubelet's configuration, and
failure is silent: the Pod is admitted, the two sides are picked independently, and only the
throughput says so.

## Verifying the policy

Editing a file is not the kubelet running what it says. Read it back from the kubelet itself, which
needs permission on the `nodes/proxy` subresource:

```bash
kubectl get --raw /api/v1/nodes/<node>/proxy/configz |
  jq -r '.kubeletconfig | {topologyManagerPolicy, topologyManagerScope}'
```

That is the running kubelet's **effective** configuration, defaults included, so it always answers.
A `none` from it therefore does not distinguish "nobody set it" from "somebody set it to `none`";
it answers what the kubelet will do, which is the question to ask after a change.

`device-manager preflight` answers the other question. Its `topology` section reports only a policy
that some readable kubelet configuration **names**, and `unknown` when none does, never the
kubelet's default ([why it refuses to fill one
in](../devices/preflight.md#reading-the-result)). Use it on a bare host before anything is installed, and to
tell a node that was configured from one that was left alone.

The two therefore differ in exactly one case, and it is not a contradiction: on a node nobody
configured, `configz` says `none` and preflight says `unknown`.

## Kueue and the RDMA keys

Do not read a queue's admission as an RDMA reservation. The RDMA keys sit outside every accelerator
family, and the same classifier drives Kueue's node-devices admission, so an RDMA request
contributes nothing to a Workload's demand, and no ResourceFlavor and no credit can be expressed
for it.

A queued Workload can therefore be admitted far past the fleet's real RDMA capacity, and the excess
surfaces as **Pods that will not schedule** rather than as a queue that waits. The queue reports the
Workload admitted while its Pod sits `Pending`, because no node advertises a free endpoint.

Three things to do instead of a quota:

- **Size the accelerator quota so its accelerators never need more endpoints than the fleet
  carries.** One endpoint per accelerator makes the two counts the same number, which is part of why
  that is the count to ask for.
- **Watch Pending Pods belonging to admitted Workloads**, not queue depth. That pair is where the
  over-subscription becomes visible; queue depth stays at zero throughout.
- **Remember which key runs out first.** An endpoint carries one exclusive token and many more
  shared ones ([how many of each](network-topology.md#the-rdma-resource-keys)),
  so a fleet exhausts the exclusive key long before the shared one on the same hardware.

## EFA engine images

Which engine can use EFA on which leg, beside every other transport, is [the transport
matrix](../model-deployment/engine-versions.md#which-transport-each-engine-can-use). This section is what
the cells marked "own image" there ask of you.

The engine image needs an EFA build of Mooncake, and no runner image carries one. The
runner images carry the standard build, which has no EFA transport. Granted an EFA device, an
engine on that build falls to the verbs path, fails creating its completion queue with `Operation
not supported`, and never starts: the Pod restarts in a loop. Without the grant it moves KV over TCP
and says nothing.

**Both legs read the engine's build.** A store group on `EFA` hands the engines it serves the
protocol `EFA`, and `kvTransfer.protocol: EFA` renders `efa` into both ends of a direct pair, so
every engine Pod on either leg needs the EFA build. The store members do not: the
[default member image](../kv-cache/backend.md#the-image) already carries EFA.

**Nothing more is needed from the operator.** An image differing from the vLLM runner image only by
the EFA build of the same client, plus libfabric, completed a direct transfer and a store write over
EFA with only the device grant the operator renders: no host network, no mount, and no added
capability.

**The operator does not pick that image.** Build it, then name it in `roles[].image` on every role
that moves KV over EFA. A role naming no image gets the runner image, whatever its leg's protocol.

### Building the image

Derive it from the runner image the deployment would otherwise run:

```dockerfile
# Pin the base by digest.
FROM gpustack/runner:cuda13.0-vllm0.29.0@sha256:<digest>
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
  each runner image carries is [its table](../model-deployment/engine-versions.md#the-minimum-per-shape))
  and keep the store on that client's minor line: [the store version must match the engine's
  client](../kv-cache/backend.md#the-store-version-must-match-the-engines-client).
- **libfabric**, from the EFA installer, whose version the recipe pins.
- **The image on every EFA role.** A new role, or a role you re-create, runs the runner image until
  you name yours on it.

### Limitations

Measured on the `0.3.13.post1` wheels. These are why the runner images keep the standard build:

- **x86_64 only.** No aarch64 EFA wheel is published.
- **No intra-node NVLink transport**, which the standard build carries.
- **No Mooncake EP or PG modules**, which SGLang uses, so this recipe covers vLLM images only.
- **No silent fallback.** Asked for `efa` on a node without an EFA device it fails, where the
  standard build falls back to TCP. With libfabric installed, `tcp`, `rdma` and the store still work
  on such a node.
- **Not measured:** the RDMA data path on InfiniBand or RoCE hardware with the `rdma-core` the
  installer brings.

### SGLang and EFA

No SGLang leg reaches EFA. On the direct leg the operator grants SGLang no fabric
device: SGLang does not hand the declared protocol to its transfer engine, so the declaration cannot
select a grant, and a positive `resources.interface` with no other fabric leg is refused at
admission.

On the store leg the operator renders the pool's protocol into SGLang's `MOONCAKE_PROTOCOL`, but
SGLang's runner image carries a Mooncake with no EFA transport, and the EFA build lacks the modules
SGLang loads. Run SGLang on AWS over `TCP`, which has been run there.

### RDMA on AWS

**Choose `EFA` or `TCP` on AWS.** The Device Manager discovers no RDMA device on an EFA node, so
the `device.gpustack.ai/rdma` keys are allocatable at zero there, and a member group or engine that
asks for them stays `Pending`. Granting the EFA device instead does not help `rdma` either: the
verbs path fails creating its completion queue, as above.

## When a Pod does not schedule

| Symptom | Meaning | Check |
|---|---|---|
| no node advertises the key at all | the node has no RDMA-capable interface, or that family is switched off on the Device Manager | the allocatable read above; the `--no-shared` and `--no-partitioned` switches |
| the key is advertised, every token unhealthy | the node judged every endpoint's link `Failed`, which withholds nothing but marks the tokens | [reading it yourself](network-topology.md#inspecting-the-record) |
| the Workload is admitted and its Pod stays `Pending` | over-subscription: nothing meters these keys | [the section above](#kueue-and-the-rdma-keys) |
| the Pod is scheduled, then refused on the node and not rescheduled | the node's totals sufficed but its devices sit on two NUMA nodes | the node's policy, and that both requests sit in one container |
| the container starts with fewer devices than expected | one `rdma_cm` is correct — count the `uverbs` entries | [the injected set](#injected-devices) |
| a node drops out of scheduling without changing | its link broke: the hardware stays, the label does not | [reading it yourself](network-topology.md#inspecting-the-record) |

---

**See also** — [Accelerator Requests](../devices/requests.md) (the normative request contract
these keys sit beside, and the seven rules they are exempt from) ·
[Network Topology](network-topology.md) (what the keys are, which endpoint serves
each, and what an allocation resolves) · [Preflight Operations](../devices/preflight.md) (reading a node's link
verdicts and its topology policy before anything is installed)

**Next** → [Preflight Operations](../devices/preflight.md) — what a node can detect, slice and manage, read on
the bare host.
