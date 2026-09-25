# Settings & Environment Variables

> **Purpose** — the two configuration surfaces: settings an administrator changes at runtime with
> `kubectl`, and the `GPUSTACK_*` environment read once at process startup.
> **Audience** operators · **Prerequisites** none · **Read time** ~10 min

GPUStack Operator is configured two ways, and the distinction matters operationally.

> **First-deploy seeding vs. runtime changes.** On first deploy, `settings.Initialize` creates the delegated
> Secret `gpustack-settings` in the system namespace and seeds every Setting from
> `GPUSTACK_<UPPER_SNAKE_NAME>`, or the built-in default when that variable is unset. Later restarts only
> **backfill missing** Settings and **never overwrite** a stored value: a `GPUSTACK_*` variable is an
> *initial seed only*, so after the first deploy change the `Setting` resource, not the environment.

## Contents

- [Online-adjustable settings](#online-adjustable-settings)
- [Deploy-time environment variables](#deploy-time-environment-variables)

## Online-adjustable settings

A fixed catalog of named values, served as the namespaced `Setting` aggregated API resource
(`gpustack.ai/v1`, short name `set`, category `gpustack`) and read at runtime with `kubectl`; the operator
picks a new value up on its next reconcile. Fixed means the resource serves
`get,list,watch,apply,update,patch`, no create/delete — you edit a Setting's **value**, not its existence.

```bash
# List every setting and its current value
kubectl -n gpustack-system get settings          # or: kubectl -n gpustack-system get set

# Inspect / change one
kubectl -n gpustack-system get setting node-management-manual -o yaml
kubectl -n gpustack-system edit setting node-management-manual
kubectl -n gpustack-system patch setting instance-type-derived-from-node --type merge -p '{"spec":{"value":"false"}}'
```

| Setting | Env seed (first deploy) | Default | Effect |
|---------|-------------------------|---------|--------|
| `container-registry` | `GPUSTACK_CONTAINER_REGISTRY` | *(blank)* | Registry to pull images from, for all built-in applications' deployments. |
| `container-namespace` | `GPUSTACK_CONTAINER_NAMESPACE` | *(blank)* | Namespace to pull images from, for all built-in applications' deployments. |
| `image-pull-secrets` | `GPUSTACK_IMAGE_PULL_SECRETS` | *(blank)* | Image pull secret for pulling images, for all built-in applications' deployments — the subcharts the chart or the worker installs. It does NOT reach a workload a controller renders from a custom resource: a [KV cache backend](kv-cache/backend.md) names its own `spec.imagePullSecrets`. |
| `image-pull-policy` | `GPUSTACK_IMAGE_PULL_POLICY` | `IfNotPresent` | Image pull policy for all built-in applications' deployments, on the same boundary as the secret above: a controller-rendered workload carries its own. |
| `instance-general-resources-overcommit` | `GPUSTACK_INSTANCE_GENERAL_RESOURCES_OVERCOMMIT` | `true` | Overcommit an Instance's general resources: when enabled, a general unit requests 800m CPU / 128Mi RAM and one-eighth local storage, an accelerated unit 100m CPU / 128Mi RAM and one-eighth local storage — so e.g. a 1C/4Gi + 128Gi type requesting 2 accelerators and 64Gi storage resolves to 200m/256Mi + 8Gi. |
| `instance-ssh-server-image` | `GPUSTACK_INSTANCE_SSH_SERVER_IMAGE` | `gpustack/ssh-server:v1.3.0` | Image of the SSH server used when deploying Instances. |
| `kv-cache-backend-image` | `GPUSTACK_KV_CACHE_BACKEND_IMAGE` | `gpustack/mirrored-mooncake:0.3.13.post1-cpu` | Image every role of a [KV cache backend](kv-cache/backend.md) runs when the object does not name one itself. The default is this project's own build — a CPU build carrying TCP and EFA over DRAM, and the only image that can run [`leader.highAvailability`](kv-cache/leader.md#high-availability). The per-vendor variant builds, their tags and which one a VRAM group needs are under [The project's own build variants](kv-cache/backend.md#the-projects-own-build-variants). **It does not fit every backend**: a backend on another transport or runtime names its own `spec.image`, which always wins over this. **Clearing this Setting restores the admission refusal** for a backend naming no image anywhere. Why one default cannot fit every backend, and what having one costs, is under [The image](kv-cache/backend.md#the-image). |
| `model-deployment-router-image` | `GPUSTACK_MODEL_DEPLOYMENT_ROUTER_IMAGE` | `gpustack/llm-router:v0.1.0` | Image a managed router runs when the `ModelDeployment` does not name one in `spec.router.image`. **It carries every router this operator supports, and which binary runs is decided by the rendered command rather than by the image** — `spec.router.name` picks the binary, and this setting only says where the binaries come from. Its default is safe for every cluster at once in a way `kv-cache-backend-image`'s is not: it runs no model, so it links no accelerator runtime and cannot be paired with the wrong one. The default is this project's own build rather than an upstream tag, because the three routers are compiled from three separate sources and one of them carries a patch this repository ships, so no upstream image holds them together. An image named on the object is used verbatim and is never redirected to the cluster mirror, because it is the user's own reference. |
| `model-deployment-router-proxy-image` | `GPUSTACK_MODEL_DEPLOYMENT_ROUTER_PROXY_IMAGE` | `gpustack/mirrored-envoy:distroless-v1.33.2` | Proxy fronting a managed router's endpoint picker. It has **no field on the API** to override it: this operator renders the proxy's configuration against one proxy's configuration schema, so swapping the binary would mean swapping that configuration too. The setting exists for registry redirection and for pinning a release back, not for running a different proxy. |
| `model-deployment-routing-sidecar-image` | `GPUSTACK_MODEL_DEPLOYMENT_ROUTING_SIDECAR_IMAGE` | `gpustack/mirrored-llm-d-router-disagg-sidecar:v0.10.0` | Sidecar a decoder runs to accept a remote prefill handoff. Same terms as the proxy above: this operator renders its arguments, so the setting is for redirection and pinning rather than for a different implementation. |
| `model-deployment-tcp-tw-reuse` | `GPUSTACK_MODEL_DEPLOYMENT_TCP_TW_REUSE` | `false` | Render `net.ipv4.tcp_tw_reuse=1` on the prefill half of every SGLang prefill/decode pair, which otherwise [runs out of local ports](reference/engine-versions.md#known-failures-at-the-minimum) under sustained load. **Allow the sysctl on the kubelet of every node that can run such a Pod before turning it on**: without that the Pod fails with `SysctlForbidden`, and so does every replacement. The steps, which Pods it reaches and how to confirm the refusal are under [Letting SGLang prefill Pods reuse TIME-WAIT ports](#letting-sglang-prefill-pods-reuse-time-wait-ports). |
| `model-artifact-huggingface-endpoint` | `GPUSTACK_MODEL_ARTIFACT_HUGGINGFACE_ENDPOINT` | `https://huggingface.co` | The Hugging Face Hub a [`ModelArtifact`](reference/model-artifact.md) resolves and revalidates against, and the `HF_ENDPOINT` an engine downloading the weights is given. What changing it does is under [Engine delivery](reference/model-artifact.md#engine-delivery). |
| `model-artifact-https-proxy` | `GPUSTACK_MODEL_ARTIFACT_HTTPS_PROXY` | *(blank)* | HTTPS proxy for resolution and revalidation, and the `HTTPS_PROXY` given to an engine downloading the weights, where a role's own value wins. Blank keeps the worker's own environment and renders nothing. Only an `http` or `https` URL without credentials is accepted, because the value reaches tenant Pods; a value from the environment that is not refuses the worker's start. |
| `model-artifact-no-proxy` | `GPUSTACK_MODEL_ARTIFACT_NO_PROXY` | *(blank)* | Comma-separated hosts that bypass `model-artifact-https-proxy`, given to engines as `NO_PROXY` on the same terms. |
| `model-artifact-ca-bundle` | `GPUSTACK_MODEL_ARTIFACT_CA_BUNDLE` | *(blank)* | Name of a ConfigMap in the worker's namespace whose `ca.crt` the resolution trusts beside the system pool. **It is not given to engine Pods**: they run in tenant namespaces, which cannot mount it. |
| `model-artifact-revalidate-interval` | `GPUSTACK_MODEL_ARTIFACT_REVALIDATE_INTERVAL` | `24h` | How often a resolved Hugging Face artifact's access is checked again, at least `1m`; a Secret change checks it at once. What a refusal does is under [Resolution and revalidation](reference/model-artifact.md#resolution-and-revalidation). |
| `instance-access-static-address` | `GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS` | *(blank)* | Static access address for all Instances; when unset, the access address is generated from host IPs. |
| `instance-access-wildcard-dns` | `GPUSTACK_INSTANCE_ACCESS_WILDCARD_DNS` | *(blank)* | Wildcard DNS for all Instances (e.g. `traefik.me`), used to build a per-Instance domain `<instance-host-ip>.<wildcard-dns>`. Only effective when `instance-access-static-address` is not set. |
| `instance-privileged-allowed` | `GPUSTACK_INSTANCE_PRIVILEGED_ALLOWED` | `false` | Whether an Instance may request privileged mode (`spec.privileged`), which escapes the container boundary and exposes the node's devices and kernel surface. Enforced by the Instance admission webhook whenever an Instance **takes** privileged mode — at creation, or through a later change, including one made while it is stopped. An Instance that already runs privileged keeps it: with this off it stays updatable, editable while stopped, and restartable. Like every setting here, a change takes up to 30s to reach the webhook, so an Instance created in that window is judged against the previous value. |
| `instance-host-path-volume-allowed` | `GPUSTACK_INSTANCE_HOST_PATH_VOLUME_ALLOWED` | `false` | Whether an Instance may mount a hostPath volume (`spec.additionalVolumes[*].hostPath`), which reaches the node's filesystem. Kept separate from `instance-privileged-allowed` because it grants strictly less — the filesystem, but not the node's devices or kernel — so an admin can allow node-path mounts without allowing a container escape. Enforced whenever an Instance **takes** a hostPath mount, on the same terms; a mount it already has is matched by value, so reordering the list is not a new grant while repointing an entry at a different node path is. |
| `workload-fit-affinity` | `GPUSTACK_WORKLOAD_FIT_AFFINITY` | `true` | Whether the Workload webhook pins a logical slice, or a shared request of two or more accelerators, on an operator queue to nodes whose [fit labels](architecture/scheduling-chain.md#per-card-fit-labels) show that one accelerator can still host it. When `true`, Kueue's topology-aware scheduling skips a fragmented node; when `false`, Workloads pass unchanged while the fit labels are still published, so the pin can be switched off on its own. Read on every webhook call, so a change takes up to 30s to reach it, and it applies to Workloads created or updated after that. |
| `node-management-manual` | `GPUSTACK_NODE_MANAGEMENT_MANUAL` | `false` | Skip auto-managing nodes. When `false`, the operator auto-onboards discovered nodes by injecting the `gpustack.ai/managed=true` label; when `true`, an administrator must opt nodes in manually. Read per-reconcile. |
| `instance-type-mixed-on-node` | `GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` | `true` | Whether one node may surface both a GPU and a CPU-only InstanceType. When `true`, a node is summarized into every type it can serve; when `false`, a node with accelerators yields only a GPU InstanceType and a CPU-only node only a general one. Read per-reconcile. |
| `instance-type-derived-from-node` | `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE` | `true` | Whether the operator auto-derives the InstanceType (and its backing ClusterQueue) from node hardware. When `true`, the `NodeFlavorReconciler` authors the derived InstanceType (create-only); when `false`, it only aligns the ResourceFlavor and the administrator defines the InstanceType via the API, and owns feasibility with it — see [Authoring the InstanceType yourself](#authoring-the-instancetype-yourself). Read per-reconcile. |
| `instance-type-drain-when-no-flavors` | `GPUSTACK_INSTANCE_TYPE_DRAIN_WHEN_NO_FLAVORS` | `true` | Whether a ClusterQueue whose pool has lost all its ResourceFlavors is drained (`HoldAndDrain`, so Kueue evicts admitted workloads) before its resource groups are emptied. When `true`, the queue is drained first; when `false`, the operator waits for the reservations to clear on their own, then empties. Either way the groups are emptied only once every reservation is zero, so Kueue's counters never go negative. Read per-reconcile. |
| `instance-type-aware-cpu-manufacturer` | `GPUSTACK_INSTANCE_TYPE_AWARE_CPU_MANUFACTURER` | `false` | Whether the derived ClusterQueue/InstanceType/InstanceTypeFlavor aggregation splits by CPU manufacturer. When `false`, non-accelerated flavors collapse into one `generic` pool per os/arch and accelerated flavors pool per accelerator (CPU ignored); when `true`, every pool splits by the CPU key (`gpustack--${gKey}-…` / `gpustack--${gKey}--${aKey}-…`) and the InstanceType records the raw CPU detail. The `ResourceFlavor`s themselves are unaffected — they always carry the CPU key, so a flip only re-groups the aggregation layer. Read per-reconcile. |

The last five — `node-management-manual`, `instance-type-mixed-on-node`, `instance-type-derived-from-node`,
`instance-type-drain-when-no-flavors`, `instance-type-aware-cpu-manufacturer` — are read **per-reconcile**
(`ShouldValueBool(ctx)`): flipping one re-converges the scheduling chain next reconcile, no restart.

### Authoring the InstanceType yourself

With `instance-type-derived-from-node=false` the operator authors no InstanceType, so the
administrator owns both halves of a pool: which InstanceTypes exist, and whether what their queue
admits can actually be placed. Flipping the setting off does not retire the types the operator
already authored — the derived marker is provenance, and nothing auto-removes a type.

**No ClusterQueue gains a reference to the node-devices feasibility gate in this mode.** That
[AdmissionCheck](architecture/admission.md#gate-3--the-per-accelerator-admissioncheck) has one
writer, [`NodeQueueReconciler`](architecture/scheduling-chain.md#nodequeuereconciler-node_queuego),
which reads this setting as a cluster-wide switch, not as "was this queue derived".

So with the switch on, every accelerated queue carries the reference once the check reports `Active`,
whoever authored its InstanceType; with it off none is added, and a queue that already carries one
drops it the next time its resource groups are refilled — not at the moment of the flip.

> **Why it is not attached in this mode anyway** — the gate changes what a queue admits, and in the
> mode where the administrator authors every InstanceType that is theirs to decide.

**The failure shape to expect.** Without that gate a Workload is admitted on quota alone and its Pods
then sit `Pending` — which reads as a full cluster, while the truth is usually a request no node can
place: quota is a scalar total and cannot see per-accelerator fragmentation. When the Pods of an
admitted Workload stay `Pending`, compare the request against the per-accelerator ledger
(`kubectl get devices <node> -o yaml`) before adding capacity.

**Write the InstanceType against the `InstanceTypeFlavor` catalog.** It is the read-only,
os/arch-agnostic view of the pools that actually exist — one entry per grouping the settings above
produce — and its `spec` carries the identity fields an InstanceType is built from:
`acceleratorGroup`, `generalGroup`, `acceleratable`, `manufacturer`, `product`, `family`, `memory`,
`cores`.

```bash
kubectl get instancetypeflavors                               # short name: instypeflavor
kubectl get instancetypeflavor gpustack--nvidia-a10g -o yaml
```

An InstanceType whose identity matches no entry there backs a ClusterQueue that selects no
`ResourceFlavor`, so the queue is left with empty resource groups and admits nothing.

### Letting SGLang prefill Pods reuse TIME-WAIT ports

**`model-deployment-tcp-tw-reuse` renders `net.ipv4.tcp_tw_reuse=1` into the Pod
`securityContext.sysctls` of the prefill half of every SGLang prefill/decode pair, and of no other
Pod.** The sysctl lets the kernel reuse ports held by `TIME-WAIT` sockets for new outgoing
connections, in that Pod's own network namespace only.

The prefill half opens a new TCP connection for every transfer, so over `TCP`, with or without a
store, its `TIME-WAIT` sockets [use up its local
ports](reference/engine-versions.md#known-failures-at-the-minimum) under sustained load.

It does not reach the decode half, which accepts transfers rather than opening them; a vLLM role,
which keeps its transfer connections open; or a [KV cache backend](kv-cache/backend.md)'s members.
A role that replaced its `command` gets nothing either, since the operator did not build what runs
there. Whether an SGLang server with a store runs out of ports is not measured, and the setting
does not reach it.

**1. Allow the sysctl on the kubelet of every node that can run such a Pod.** It is not on the
Kubernetes [safe list](https://kubernetes.io/docs/tasks/administer-cluster/sysctl-cluster/#safe-and-unsafe-sysctls),
so a kubelet refuses it until told otherwise. Add it to the kubelet configuration and restart the
kubelet:

```yaml
# KubeletConfiguration
allowedUnsafeSysctls:
  - net.ipv4.tcp_tw_reuse
```

The command-line form is `--allowed-unsafe-sysctls=net.ipv4.tcp_tw_reuse`. It is one list, so keep
any name already on it.

**On a managed cluster, set it where the node pool's kubelet configuration comes from**, so a node
added or replaced later keeps it; an edit made on a running node leaves with that node. On each of
these it takes effect on newly created nodes:

- **EKS on Amazon Linux 2023** — the launch template's `nodeadm` `NodeConfig`, under
  `spec.kubelet.config` as above or as the flag under `spec.kubelet.flags`; see the [`nodeadm`
  API](https://awslabs.github.io/amazon-eks-ami/nodeadm/doc/api/). On Amazon Linux 2, pass the flag
  through the bootstrap script's `--kubelet-extra-args`.
- **AKS** — `allowedUnsafeSysctls` in the kubelet configuration a node pool is created with; see
  [custom node configuration](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration).
- **GKE** — `kubeletConfig.allowedUnsafeSysctls` in the node system configuration of a node pool;
  see [node system
  configuration](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/node-system-config).
- A platform that exposes no kubelet configuration cannot run this setting; leave it off there.

**2. Turn the setting on.**

```bash
kubectl -n gpustack-system patch setting model-deployment-tcp-tw-reuse --type merge -p '{"spec":{"value":"true"}}'
```

**Flipping it recreates the prefill replicas it reaches**, in either direction, because the sysctl
is part of the Pod spec their fingerprint covers. Each deployment picks the value up on its next
reconcile, and a setting change does not wake one. To apply it at once, delete one of the
deployment's prefill Pods: the reconcile that replaces it renders the new value, and the rollout
replaces the other prefill replicas. A pair that has already locked up recovers the same way.

**If the kubelet was not changed, the prefill replica never starts.** The node's kubelet refuses the
Pod, which ends in phase `Failed` with reason `SysctlForbidden`; the operator deletes it and logs
`removing replica that failed` with that reason, and the replacement is refused the same way, over
and over. The events outlive the Pods, and their message names the sysctl:

```bash
kubectl -n <namespace> get events --field-selector reason=SysctlForbidden
kubectl -n <namespace> get pods -w \
  -l app.kubernetes.io/instance=<deployment>,app.kubernetes.io/component=<prefill-role>
```

The second command shows the prefill Pods cycling through `SysctlForbidden`. Allow the sysctl on
those nodes or turn the setting off; either ends the loop at the next replacement.

A namespace enforcing the `baseline` or `restricted` Pod Security Standard refuses the Pod at
creation instead, since the sysctl is not on that standard's allowed list either: no Pod appears,
and the operator logs the refusal.

## Deploy-time environment variables

Read **once at process startup**, so changing one means restarting or redeploying the affected component.
The Worker (WK) copies every `GPUSTACK_`-prefixed variable from its own Pod spec onto the Device Manager
(DM) DaemonSets, so setting one on the Worker Deployment reaches the DMs automatically.

### Configuration knobs

| Variable | Default | Component | Effect |
|----------|---------|-----------|--------|
| `GPUSTACK_DATA_DIR` | `/var/lib/gpustack` | all | Root directory for data storage. |
| `GPUSTACK_CONF_DIR` | `/etc/gpustack` | all | Root directory for configuration and metadata, e.g. bundled Helm charts. |
| `GPUSTACK_PCI_CLASS_PREFIXES` | `02,03,0b,12` | DM | Comma-separated PCI class prefixes treated as display/accelerator devices (see the [PCI class registry](https://admin.pci-ids.ucw.cz/read/PD)). Applied to the DM's local sysfs PCI scan, and to nothing else. The same list appears twice more, and neither reads this variable: the chart value `node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist` decides which classes NFD labels, and `pkg/nodefeature` is what the `gpustack-cpu-info` NodeFeatureRule matches (a Go test holds those two equal). Change one and change all three. |
| `GPUSTACK_DEVICES_GROUP_ID_WITH_MEMORY` | `false` | DM | When `true`, the devices group ID gains a memory-size suffix (e.g. `nvidia-tesla-t4-16g` instead of `nvidia-tesla-t4`), so same-model devices with different VRAM sizes form distinct groups. |
| `GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE` | `true` | DM | Whether `Allocate` refuses a logical slice its accelerator cannot hold: one with no free slot, or without the units the slice needs. The Pod fails with `UnexpectedAdmissionError` and its controller recreates it. `false` allocates it anyway and clamps the ledger at zero, as before the refusal existed; see [Switching an allocator safeguard off](#switching-an-allocator-safeguard-off). |
| `GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET` | `true` | DM | Whether `Allocate` asks the kubelet's pod-resources API which container it serves. `false` identifies it by the pending-Pod heuristic alone, as before the lookup existed; a node whose kubelet cannot be reached falls back to that heuristic by itself. See [Switching an allocator safeguard off](#switching-an-allocator-safeguard-off). |

### Switching an allocator safeguard off

Two device-plugin safeguards default on, and each has a switch for the case where it misjudges a
node. Set it through the chart, which renders `deviceManager.env` onto every device-manager DaemonSet:

```bash
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reset-then-reuse-values \
  --set-string deviceManager.env.GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=false
```

- `GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=false` is for a slice refused although its accelerator
  has room — the Pod keeps coming back `UnexpectedAdmissionError` naming that accelerator. With it
  off, the slice is allocated and the ledger clamps that accelerator's `Remaining` at zero.
- `GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET=false` is for allocations recorded on the wrong Pod while
  the kubelet is reachable. With it off, the device manager picks the oldest pending Pod the request
  could be for, which can record two Pods bound together on each other.

The change rolls the device-manager Pods, one node at a time. Running workloads are not touched: a
device-manager restart does not reach a started container, and its allocation records live on the
Pods, where the restarted process reads them. A Pod admitted while its node's device manager is
restarting can fail admission, as across any device-manager restart, and its controller recreates it.

To switch the safeguard back on, remove the value. Set it to `null` with `--set`, not `--set-string`,
which would store the string `"null"`:

```bash
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reset-then-reuse-values \
  --set deviceManager.env.GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=null
```

### Per-manufacturer overrides

Three override patterns expand for every known manufacturer (`amd`, `ascend`, `cambricon`, `hygon`,
`iluvatar`, `metax`, `mthreads`, `nvidia`, `thead`); both the WK and the DM read them, so the propagation
above keeps the two sides consistent.

**Installed by the chart, set the `global.manufacturers` row, not these variables.** All four overrides
here are fields of that row (`pciVendorID`, `resourceName`, `runtimeName`, `partitionKind`), which the
chart fans out as the matching variable to the worker *and* the device-managers, along with what a variable
cannot reach: the DaemonSet node selectors, the RuntimeClasses it creates, Kueue's credits mapping. The
variable alone leaves those stale; the next render overwrites it.

- `GPUSTACK_${MANUFACTURER}_PCI_VENDOR_ID` — the PCI vendor ID used for NFD node selection and device scanning. Accepts `${vendor}` or `${class}_${vendor}`.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RESOURCE_NAME` — the extended resource name the scheduling chain allocates against.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RUNTIME_NAME` — the container runtime class name used for accelerated workloads. A RuntimeClass of that name is attached only when one exists in the cluster; the chart creates one only where the `global.manufacturers` row sets a `runtimeInjectsDriver` or `runtimeInjectsDevices` fact (see `global.manufacturers`) — never for a manufacturer setting neither, whose default below holds only if something else created the class.

Defaults:

| Manufacturer | PCI vendor ID | Resource name | Runtime name |
|--------------|---------------|---------------|--------------|
| `amd` | `1002` | `amd.com/gpu` | `amd` |
| `ascend` | `19e5` | `huawei.com/npu` | `ascend` |
| `cambricon` | `cabc` | `cambricon.com/mlu` | `cambricon` |
| `hygon` | `1d94` | `hygon.com/dcu` | `hygon` |
| `iluvatar` | `1e3e` | `iluvatar.com/gpu` | `iluvatar` |
| `metax` | `9999` | `metax-tech.com/gpu` | `metax` |
| `mthreads` | `1ed5` | `mthreads.com/gpu` | `mthreads` |
| `nvidia` | `10de` | `nvidia.com/gpu` | `nvidia` |
| `thead` | `1ded` | `alibabacloud.com/ppu` | — (none) |

> T-Head has no default runtime name, but `GPUSTACK_THEAD_ACCELERATABLE_RUNTIME_NAME` is still honored and can supply one.

A fourth override applies only to a manufacturer that has hardware partitioning at all:

- `GPUSTACK_${MANUFACTURER}_PARTITION_KIND` — the manufacturer's own name for hardware partitioning, which becomes the segment prefix of its per-profile resource key.

`nvidia` is the only one with a default — `mig`, giving `nvidia.com/gpu.partitioned.mig-${profile}`.
Elsewhere the variable does nothing: without hardware partitioning there is no `.partitioned` family, so
no key segment to rename.

A fifth override is per manufacturer, and today only NVIDIA reads it:

- `GPUSTACK_${MANUFACTURER}_DEVICE_INJECTION_STRATEGY` — which channel that manufacturer's allocator
  uses to make a granted accelerator reach a container: `envvar` (**default**, today's
  `*_VISIBLE_DEVICES` behavior, byte-identical), `cdi-annotations` (the granted accelerator is
  requested as a `cdi.k8s.io/*` device-plugin annotation), or `auto` (opt-in detection, falling back to
  `envvar` with the reason logged whenever it cannot confirm CDI is safe on that node).

A manufacturer whose allocator injects `/dev` nodes itself, or that ships no CDI generator, has nothing
for the CDI channel to resolve, so the variable does nothing there. The partitioned (MIG) family and
partition-backed visibility always use `envvar`, whatever the strategy names, since a MIG instance is
materialized at `Allocate` time and no pre-generated specification names it.

**Prefer `auto` over naming the CDI channel yourself.** The CDI channel needs a container engine that
resolves CDI requests — containerd 2.x does, containerd 1.7 only with `enable_cdi = true`. Ask for it on
an engine that does not and the request is simply ignored: no variable is set either, and the container
starts with no accelerator and no error, which is the failure this setting exists to remove. `auto` reads
the engine first and keeps the variable when the answer is no.

**Which value a runtime wants.** Two independent questions decide it: does the engine resolve CDI
requests at all, and can `auto` tell that it does? `auto` reads both answers out of containerd's
`config.toml`, so a runtime that keeps them anywhere else is invisible to it. That file is where the
detection looks; it is not something the channel itself needs.

| Container runtime | Resolves CDI | `auto` can tell | Set |
|---|---|---|---|
| containerd 2.x (configuration version 3) | Yes, always | Yes, from the version | `auto` |
| containerd 1.7 with `enable_cdi = true` | Yes | Yes, from the key | `auto` |
| containerd 1.7, `enable_cdi` unset | No | Yes | `auto`, which keeps the variable. Naming `cdi-annotations` here is the silent no-accelerator case above |
| CRI-O | Yes, natively | **No** — it has no `config.toml` | `cdi-annotations` |

`cdi-annotations` is the answer on exactly one row: a runtime that does resolve CDI requests but that
`auto` has no way to see. Everywhere else `auto` reaches the same channel by itself, or correctly
declines to — and naming the channel where the engine ignores it is how a container ends up running
with nothing.

**Where to set it.** The name embeds the manufacturer in upper case, so it is not a literal you will
find spelled out anywhere — pass it through the chart's `deviceManager.env`, which lands on every
device-manager DaemonSet:

```bash
helm upgrade ... --set deviceManager.env.GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY=auto
```

It is read once, when that manufacturer's allocator is constructed, so a change takes effect only when
the DaemonSet restarts. A value that is not one of the three is reported and the node keeps `envvar`:
refusing to start the allocator over it would take the node's accelerators with it.

`auto` also keeps the variable wherever the engine's own default runtime is already the vendor runtime,
which is the usual shape on a distribution that ships the GPU toolkit for you. There every Pod runs under
that runtime whether it asks to or not, so the variable works and a CDI request would only add a second
injection path. `auto` doing nothing on such a node is the correct answer, not a misconfiguration.

Which channel a node settled on, and why, is logged once per answer — but at the allocator's own
verbosity, above what the DaemonSet ships with. [Runtime log
verbosity](development.md#runtime-log-verbosity) raises it on a running Pod.

One node-level variable feeds the same decision:

- `GPUSTACK_CONTAINERD_CONFIG_DIR` — the directory holding the container engine's `config.toml`, from
  the chart's `deviceManager.containerdConfigDir` (default `/etc/containerd`). `auto` reads it to learn
  whether the engine resolves CDI requests and which runtime handler a Pod naming no
  `runtimeClassName` runs under; unreadable keeps the node on `envvar`.

### Manufacturer toolkit paths

The DM device bindings locate manufacturer libraries through conventional toolkit-home variables, each
falling back to the listed default when unset.

| Variable | Default | Manufacturer | Effect |
|----------|---------|--------------|--------|
| `ROCM_HOME`, then `ROCM_PATH` | `/opt/rocm` | AMD | ROCm root, searched for `librocm_smi64.so` / `libamd_smi.so` / `libhsa-runtime64.so`. |
| `ROCM_SMI_LIB_PATH` | — | AMD | Extra directory searched for `librocm_smi64.so` before the ROCm root. |
| `AMD_SMI_LIB_PATH` | — | AMD | Extra directory searched for `libamd_smi.so` before the ROCm root. |
| `CANN_HOME` | `/usr/local/Ascend` | Ascend | Driver root, searched for `libdcmi.so`. |
| `ASCEND_TOOLKIT_HOME` | `/usr/local/Ascend/cann`, falling back to `/usr/local/Ascend/ascend-toolkit/latest/runtime` | Ascend | CANN toolkit root used by the Ascend detector. |
| `NEUWARE_HOME` | `/usr/local/neuware` | Cambricon | Neuware root, searched for `libcndev.so`. |
| `PPU_HOME` | `/usr/local/PPU_SDK` | T-Head | PPU SDK root, searched for `libhgml.so`. |
| `COREX_HOME` | `/usr/local/corex` | Iluvatar | CoreX root, searched for `libixml.so`. |
| `MACA_HOME` | `/opt/maca` | MetaX | MACA root, searched for `libmxsml.so`. |
| `LD_LIBRARY_PATH` | — | all | Standard library search path, consulted as an additional source of candidate library directories. |

### Kubernetes-injected variables

Populated by the Pod specs GPUStack Operator renders (Downward API or Service environment); not
user-facing knobs, listed for completeness.

| Variable | Default | Effect |
|----------|---------|--------|
| `KUBERNETES_NODE_NAME` | — (required) | Name of the node the Pod runs on; the DM uses it to name its NodeFeature/Devices objects. |
| `KUBERNETES_POD_NAME` | — | The WK's own Pod name, used to read back its container spec (image, pull policy, `GPUSTACK_*` env) for rendering the DM DaemonSets. |
| `KUBERNETES_POD_NAMESPACE` | `gpustack-system` | System namespace where managed resources live. |
| `KUBERNETES_POD_IP` | — | Overrides the auto-detected primary host IP in topology discovery. |
| `KUBERNETES_SERVICE_NAME` | `gpustack-operator-worker` | Service name used for system routing. |
| `KUBERNETES_SERVICE_HOST` | — | Standard in-cluster marker; its presence tells the embedded runner it is inside a cluster. |

### Proxy and internal flags

| Variable | Default | Effect |
|----------|---------|--------|
| `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | — | Standard proxy settings, passed through to the embedded Kubernetes installer. |
| `NO_PROXY` / `no_proxy` | — | Also parsed (hosts, IPs, CIDRs) to bypass the proxy on direct HTTP calls. |
| `_RUNNING_INSIDE_CONTAINER_` | `false` | Internal marker baked into the container image; switches data/conf paths to their absolute in-container locations. Not intended to be set by users. |

---

**See also** — [Architecture](./architecture.md) (what each setting regroups) ·
[Installation Modes](./architecture/installation-modes.md) (which flags a mode sets for you) ·
[High Availability Operations](./operation/high-availability.md)

**Next** → [Walkthrough](./walkthrough.md) — a setting flipped on a live cluster, before and after.
