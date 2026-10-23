# Settings & Environment Variables

GPUStack Operator reads configuration from two places: the `Setting` resource, which you can change
at runtime, and environment variables, which the worker reads when it starts.

> **The worker aligns every `Setting` on every start.** A `GPUSTACK_*` variable present in the worker
> process environment is a standing declaration rather than a one-time default: while it holds a
> non-blank value, the worker writes it to the `Setting` on every start and the stored value loses. A
> `Setting` named in `settings-default-tracking` is written with the operator's built-in default on every
> start. Every other `Setting` keeps the value you stored. [Who owns a value](#who-owns-a-value) states
> the four rules in the order they run, and [Finding the owning
> rule](#finding-the-owning-rule) tells you which of them owns yours.

## Contents

- [Who owns a value](#who-owns-a-value)
- [Settings](#settings)
- [Troubleshooting](#troubleshooting)
- [Deployment environment variables](#deployment-environment-variables)

## Who owns a value

Every registered `Setting` lives as one key of a single delegated `Secret`, `gpustack-settings` in the
system namespace. The worker aligns that `Secret` once per worker start and asks one question about each
key: does the operator own this value, or does the administrator? A value the operator owns is written
on that start and logged; a value it does not own is left exactly as it was stored.

Write `D` for a `Setting`'s **seed**: the value of `GPUSTACK_<UPPER_SNAKE_NAME>` when that variable is
present in the worker process environment, and the built-in default when the variable is absent. A
variable present with an empty value yields a blank seed rather than the built-in default, and `{}` and
`[]` read as blank too.

The alignment applies four rules, in this order:

1. **Backfill.** A key the `Secret` does not carry is added with its seed. A blank seed is a key like any
   other, so a `Setting` with a blank default still appears in `kubectl get settings`.
2. **A declared variable wins.** If the worker environment declares the variable for the `Setting` and
   the seed is not blank, the seed replaces the stored value.
3. **A tracked `Setting` follows the operator's default.** If `settings-default-tracking` names the
   `Setting` and no declared variable has claimed it, the built-in default replaces the stored value.
4. **Otherwise the stored value is the administrator's.** Nothing writes it, a blank one included.

A `Setting` that is both declared and tracked is written by rule 2, with the variable's value. The two
rules cannot disagree, because a `Setting` has one seed and both of them write it.

Rules 2, 3 and 4 never write a blank value, rule 4 writing nothing at all, which is why a variable set
to the empty string takes a `Setting` over nothing and a tracked `Setting` whose default is blank keeps what
you stored. Only backfill writes a blank, and it runs first so that a newly registered `Setting` appears
in the same pass that decides whether the operator owns it.

Deleting the `Secret` is the case with nothing to preserve: the next worker start creates it again from
every seed, and `settings-default-tracking` returns to the six names it ships with.

### Every override is logged

A start that changes a value writes one line per `Setting` to the worker log, naming the `Setting`, the
value it replaced, the value it wrote and the rule that fired:

```text
setting overridden on start setting="container-registry" stored="registry.example" value="mirror.example" rule="env-declared" envName="GPUSTACK_CONTAINER_REGISTRY"
```

`rule` is `env-declared` for rule 2 and `default-tracked` for rule 3, and `envName` names the variable
when the `Setting` reads one. A start that changes nothing logs nothing, and a stored value that already
equals the seed is not an override.

A `Setting` the operator marks sensitive logs neither value. Its line carries the name, the rule and the
variable — enough to tell you which `Setting` changed and why — and replaces both values with
`values="[withheld: sensitive setting]"`. The log reaches a wider audience than this `Secret`, so a value
kept out of the `Secret` stays out of the log. No registered `Setting` carries that mark today; the operator
sets it on a `Setting` whose value should not appear in a log at all.

```bash
kubectl -n gpustack-system logs deploy/gpustack-operator-worker | grep 'setting overridden on start'
```

### Tracking list

`settings-default-tracking` holds a comma-separated list of `Setting` names. The worker reads it from the
`Secret`, and the backfill has already written the six names the operator ships into a `Secret` that carries
no such key, so a cluster that predates the `Setting` is tracked from the first start that backfills the
key.

A stored value cannot silently change what is tracked. Each segment is trimmed, and a segment left
empty, a duplicate, the list's own name and a name this operator does not register are dropped, each
logged once at verbosity 4. A write that names the list itself is refused outright, because a `Setting`
tracking its own membership would reconcile that membership against itself.

### Finding the owning rule

Two checks place any `Setting` you did not write yourself, and they are the two the rules read:

```bash
# 1. Does the worker Deployment render a variable for it, and with what value?
kubectl -n gpustack-system get deploy -o yaml | grep -A1 'GPUSTACK_'

# 2. Does the tracking list name it?
kubectl -n gpustack-system get setting settings-default-tracking -o yaml
```

- The `Deployment` renders the variable with a non-blank value, so rule 2 owns the `Setting` whatever is
  stored. The [Settings](#settings) list identifies the chart-rendered variables.
- The `Deployment` does not render it and the tracking list names it, so rule 3 owns the `Setting`.
- Neither, so rule 4 applies and the value you stored stands until you change it yourself.

## Settings

A fixed catalog of named values, served as the namespaced `Setting` aggregated API resource
(`gpustack.ai/v1`, short name `set`, category `gpustack`) and read at runtime with `kubectl`; when a new value
applies is the Effect window on each row. Fixed means the resource serves
`get,list,watch,apply,update,patch` and no create/delete: you edit a `Setting`'s **value**, not its existence.

The name in parentheses beside a `Setting` is the variable it reads. Every one of them is
authoritative on every worker start while the variable is present and holds a non-blank value, whether
the chart renders it or you add it yourself ([who owns a value](#who-owns-a-value)). A variable marked
*chart-rendered* is one the chart writes onto the worker `Deployment` itself, so the chart re-asserts
it on every start and every upgrade.

```bash
# List every setting and its current value
kubectl -n gpustack-system get settings          # or: kubectl -n gpustack-system get set

# Inspect / change one
kubectl -n gpustack-system get setting node-management-manual -o yaml
kubectl -n gpustack-system edit setting node-management-manual
kubectl -n gpustack-system patch setting instance-type-derived-from-node --type merge -p '{"spec":{"value":"false"}}'
```

**Effect windows** — **Immediate**: the change does its own work. A consuming controller watches the
delegated settings `Secret` and reconciles what the `Setting` governs, or an admission webhook starts
judging every call by the new value. Values reach a process through a thirty-second read cache, so
Immediate means within about that.

**Next trigger** — nothing moves on its own. The new value applies when the next natural event named
in its Usage line arrives — a reconcile, a `Pod` render, a resolution or revalidation tick, or a
request that reaches a webhook. Neither window needs a restart.

| `Setting` | Description |
|---|---|
| `container-namespace`<br/>(`GPUSTACK_CONTAINER_NAMESPACE`, *chart-rendered*) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Namespace to pull images from, for all built-in applications' deployments. |
| `container-registry`<br/>(`GPUSTACK_CONTAINER_REGISTRY`, *chart-rendered*) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Registry to pull images from, for all built-in applications' deployments. |
| `image-pull-policy`<br/>(`GPUSTACK_IMAGE_PULL_POLICY`, *chart-rendered*) | Effect window: Next trigger<br/>Default: `IfNotPresent`<br/>Usage: Image pull policy for all built-in applications' deployments, on the same boundary as the secret above: a controller-rendered workload carries its own. The chart renders it, so a runtime edit to this `Setting` is reverted at the next worker start; change it through `global.imagePullPolicy` or *`image.pullPolicy`* and leave this `Setting` alone. |
| `image-pull-secrets`<br/>(`GPUSTACK_IMAGE_PULL_SECRETS`, *chart-rendered*) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Image pull secret for pulling images, for all built-in applications' deployments — the subcharts the chart or the worker installs. It does NOT reach a workload a controller renders from a custom resource: a [KV cache backend](../modules/kv-cache/backend.md) names its own *`spec.imagePullSecrets`*. |
| `instance-access-static-address`<br/>(`GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS`) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Static access address for all `Instance`s; when unset, the access address is generated from host IPs. |
| `instance-access-wildcard-dns`<br/>(`GPUSTACK_INSTANCE_ACCESS_WILDCARD_DNS`) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Wildcard DNS for all `Instance`s (e.g. `traefik.me`), used to build a per-`Instance` domain `<instance-host-ip>.<wildcard-dns>`. Only effective when `instance-access-static-address` is not set. |
| `instance-general-resources-overcommit`<br/>(`GPUSTACK_INSTANCE_GENERAL_RESOURCES_OVERCOMMIT`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Overcommit an `Instance`'s general resources: when enabled, a general unit requests 800m CPU / 128Mi RAM and one-eighth local storage, an accelerated unit 100m CPU / 128Mi RAM and one-eighth local storage — so e.g. a 1C/4Gi + 128Gi type requesting 2 accelerators and 64Gi storage resolves to 200m/256Mi + 8Gi. A CPU limit whose whole cores cost more than the limit itself (e.g. `100m`, or `1500m` whose half core rounds up) requests the limit instead — a request may never exceed its limit. |
| `instance-host-path-volume-allowed`<br/>(`GPUSTACK_INSTANCE_HOST_PATH_VOLUME_ALLOWED`) | Effect window: Immediate<br/>Default: `false`<br/>Usage: Whether an `Instance` may mount a hostPath volume (*`spec.additionalVolumes[*].hostPath`*), which reaches the node's filesystem. Kept separate from `instance-privileged-allowed` because it grants strictly less — the filesystem, but not the node's devices or kernel — so an admin can allow node-path mounts without allowing a container escape. Enforced whenever an `Instance` **takes** a hostPath mount, on the same terms; a mount it already has is matched by value, so reordering the list is not a new grant while repointing an entry at a different node path is. |
| `instance-persistent-volume-placement`<br/>(`GPUSTACK_INSTANCE_PERSISTENT_VOLUME_PLACEMENT`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Whether an `Instance`'s persistent claims (*`spec.volume.persistent`*, *`spec.additionalVolumes[*].persistent`*) decide where its `Pod` runs, by the [claim placement rules](../modules/model-delivery/artifact.md#claim-delivery-and-placement): a bound claim adds its PV's required node affinity, and a claim nothing binds holds the `Pod` back with the reason in *`status.phaseMessage`*. **`false` is the escape path**: the `Pod` is built from the claim names alone, as before, and Kueue's topology-aware scheduling may then assign a node the volume cannot reach, leaving the `Pod` Pending while its `Workload` holds quota. Read when a `Pod` is created, so a change never moves a running `Instance`; stop and start one to apply it. |
| `instance-privileged-allowed`<br/>(`GPUSTACK_INSTANCE_PRIVILEGED_ALLOWED`) | Effect window: Immediate<br/>Default: `false`<br/>Usage: Whether an `Instance` may request privileged mode (*`spec.privileged`*), which escapes the container boundary and exposes the node's devices and kernel surface. Enforced by the `Instance` admission webhook whenever an `Instance` **takes** privileged mode — at creation, or through a later change, including one made while it is stopped. An `Instance` that already runs privileged keeps it: with this off it stays updatable, editable while stopped, and restartable. Like every setting here, a change takes up to 30s to reach the webhook, so an `Instance` created in that window is judged against the previous value. |
| `instance-ssh-server-image`<br/>(`GPUSTACK_INSTANCE_SSH_SERVER_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/ssh-server:v1.3.0`<br/>Usage: Image of the SSH server used when deploying `Instance`s. |
| `instance-isolation-pod-cidrs`<br/>(`GPUSTACK_INSTANCE_ISOLATION_POD_CIDRS`) | Effect window: Immediate<br/>Default: *(blank)*<br/>Usage: The IPv4 pod CIDRs the Instance tenant-isolation `NetworkPolicy` carves out of its 0.0.0.0/0 external rule: `Instance` `Pod`s stay reachable from their own namespace and from outside the cluster, and never from another namespace's `Pod`s — see [Network isolation](../modules/instances/_index.md#network-isolation). **Blank (default) renders no policy at all**: isolation is opt-in, and emptying the list collects the rendered policy again, never a same-named one written by hand. Needs a CNI that enforces `NetworkPolicy`; find the values with `kubectl get ippools.crd.projectcalico.org` on a Calico-IPAM cluster, or `kubectl get nodes -o jsonpath='{.items[*].spec.podCIDRs}'` where the nodes carve the pod addresses. Read by the namespace-isolation controller, which watches the delegated settings Secret: a change reconciles every non-reserved namespace within seconds. The chart carries the same value as `worker.instanceIsolationPodCIDRs`. |
| `instance-type-aware-cpu-manufacturer`<br/>(`GPUSTACK_INSTANCE_TYPE_AWARE_CPU_MANUFACTURER`) | Effect window: Next trigger<br/>Default: `false`<br/>Usage: Whether the derived `ClusterQueue`/`InstanceType`/`InstanceTypeFlavor` aggregation splits by CPU manufacturer. When `false`, non-accelerated flavors collapse into one `generic` pool per os/arch and accelerated flavors pool per accelerator (CPU ignored); when `true`, every pool splits by the CPU key (`gpustack--${gKey}-…` / `gpustack--${gKey}--${aKey}-…`) and the `InstanceType` records the raw CPU detail. The `ResourceFlavor`s themselves are unaffected — they always carry the CPU key, so a flip only re-groups the aggregation layer. Read per-reconcile. |
| `instance-type-derived-from-node`<br/>(`GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Whether the operator auto-derives the `InstanceType` (and its backing `ClusterQueue`) from node hardware. When `true`, the `NodeFlavorReconciler` authors the derived `InstanceType` (create-only); when `false`, it only aligns the `ResourceFlavor` and the administrator defines the `InstanceType` via the API, and owns feasibility with it — see [Authoring the InstanceType](#authoring-the-instancetype). Read per-reconcile. |
| `instance-type-drain-when-no-flavors`<br/>(`GPUSTACK_INSTANCE_TYPE_DRAIN_WHEN_NO_FLAVORS`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Whether a `ClusterQueue` whose pool has lost all its `ResourceFlavor`s is drained (`HoldAndDrain`, so Kueue evicts admitted workloads) before its resource groups are emptied. When `true`, the queue is drained first; when `false`, the operator waits for the reservations to clear on their own, then empties. Either way the groups are emptied only once every reservation is zero, so Kueue's counters never go negative. Read per-reconcile. |
| `instance-type-mixed-on-node`<br/>(`GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Whether one node may surface both a GPU and a CPU-only `InstanceType`. When `true`, a node is summarized into every type it can serve; when `false`, a node with accelerators yields only a GPU `InstanceType` and a CPU-only node only a general one. Read per-reconcile. |
| `node-management-manual`<br/>(`GPUSTACK_NODE_MANAGEMENT_MANUAL`) | Effect window: Next trigger<br/>Default: `false`<br/>Usage: Skip auto-managing nodes. When `false`, the operator auto-onboards discovered nodes by injecting the `gpustack.ai/managed=true` label; when `true`, an administrator must opt nodes in manually. Read per-reconcile. |
| `kv-cache-backend-image`<br/>(`GPUSTACK_KV_CACHE_BACKEND_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/mirrored-mooncake:0.3.13.post1-cpu`<br/>Usage: Image every role of a [KV cache backend](../modules/kv-cache/backend.md) runs when the object does not name one itself. The default is this project's own build — a CPU build carrying TCP and EFA over DRAM, and the only image that can run [`leader.electionBackend`](../modules/kv-cache/leader.md#high-availability). The per-vendor variant builds, their tags and which one a VRAM group needs are under [GPUStack build variants](../modules/kv-cache/backend.md#gpustack-build-variants). **It does not fit every backend**: a backend on another transport or runtime names its own *`spec.image`*, which always wins over this. **Clearing this `Setting` restores the admission refusal** for a backend naming no image anywhere. Why one default cannot fit every backend, and what having one costs, is under [Image](../modules/kv-cache/backend.md#image). |
| `model-artifact-ca-bundle`<br/>(`GPUSTACK_MODEL_ARTIFACT_CA_BUNDLE`) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Name of a `ConfigMap` in the worker's namespace whose `ca.crt` the resolution and the node plugin trust beside the system pool. **It is not given to engine `Pod`s**: they run in tenant namespaces, which cannot mount it. |
| `model-artifact-delivery-mode`<br/>(`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE`, *chart-rendered*) | Effect window: Immediate<br/>Default: `Engine`<br/>Usage: How a hub artifact's weights reach a `ModelDeployment`: `Engine`, the engine downloads them, or `Node`, the node's [`model-manager` plugin](../modules/model-delivery/node-store.md) mounts a verified copy. The chart declares `Node` when it deploys the plugin, and the chart owns the `Setting` from then on: a runtime edit is reverted at the next worker start. Deploying or removing the plugin (`modelManager.enabled`) is what changes the value. `Node` is refused while the `CSIDriver` `model.csi.gpustack.ai` does not exist. **Changing it rolls every deployment using a hub artifact once**; see [Switch delivery](../modules/model-delivery/operations.md#switch-delivery). |
| `model-artifact-https-proxy`<br/>(`GPUSTACK_MODEL_ARTIFACT_HTTPS_PROXY`) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: HTTPS proxy for resolution, revalidation and the node plugin's downloads, and the `HTTPS_PROXY` given to an engine downloading the weights, where a role's own value wins. Blank keeps the worker's own environment and renders nothing. Only an `http` or `https` URL without credentials is accepted, because the value reaches tenant `Pod`s; a value from the environment that is not refuses the worker's start. |
| `model-artifact-huggingface-endpoint`<br/>(`GPUSTACK_MODEL_ARTIFACT_HUGGINGFACE_ENDPOINT`) | Effect window: Next trigger<br/>Default: `https://huggingface.co`<br/>Usage: The Hugging Face Hub a [`ModelArtifact`](../modules/model-delivery/artifact.md) resolves and revalidates against, and the `HF_ENDPOINT` an engine downloading the weights is given; the node plugin downloads from it too. What changing it does is under [Engine delivery](../modules/model-delivery/artifact.md#engine-delivery). |
| `model-artifact-modelscope-endpoint`<br/>(`GPUSTACK_MODEL_ARTIFACT_MODELSCOPE_ENDPOINT`) | Effect window: Next trigger<br/>Default: `https://www.modelscope.cn`<br/>Usage: The ModelScope hub a [`ModelArtifact`](../modules/model-delivery/artifact.md) resolves and revalidates against and the node plugin downloads from; an engine downloading the weights gets its host as `MODELSCOPE_DOMAIN` — the runners' SDK takes a bare host. What changing it does is under [Engine delivery](../modules/model-delivery/artifact.md#engine-delivery). |
| `model-artifact-no-proxy`<br/>(`GPUSTACK_MODEL_ARTIFACT_NO_PROXY`) | Effect window: Next trigger<br/>Default: *(blank)*<br/>Usage: Comma-separated hosts that bypass `model-artifact-https-proxy`, for the node plugin too, given to engines as `NO_PROXY` on the same terms. |
| `model-artifact-revalidate-interval`<br/>(`GPUSTACK_MODEL_ARTIFACT_REVALIDATE_INTERVAL`) | Effect window: Next trigger<br/>Default: `24h`<br/>Usage: How often a resolved hub artifact's access is checked again, at least `1m`; a `Secret` change checks it at once. What a refusal does is under [Resolution and revalidation](../modules/model-delivery/artifact.md#resolution-and-revalidation). |
| `model-deployment-kv-cache-dtype-owned`<br/>(`GPUSTACK_MODEL_DEPLOYMENT_KV_CACHE_DTYPE_OWNED`) | Effect window: Next trigger<br/>Default: `true`<br/>Usage: Hand every `KVCachePoolBinding`'s *`spec.domain.dtype`* to the engines attached through it as `--kv-cache-dtype`, verbatim, and refuse a pool-attached role or injected `Pod` naming that flag itself, and a new Binding declaring `auto` — see [The dtype is handed to the engine](../modules/kv-cache/pool.md#engine-dtype). **It is the escape from a Binding whose spelling the engine rejects**, which makes every new `Pod` fail argument parsing: `false` renders and refuses nothing, as before the flag was owned. Flipping it recreates every pool-attached replica at its next reconcile. The recovery is under [Upgrading to an enforced Binding dtype](../operate/migration/kv-cache-dtype.md). |
| `model-deployment-router-image`<br/>(`GPUSTACK_MODEL_DEPLOYMENT_ROUTER_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/llm-router:v0.3.1`<br/>Usage: Image a managed router runs when the `ModelDeployment` does not name one in *`spec.router.image`*. **It carries every router this operator supports, and which binary runs is decided by the rendered command rather than by the image** — *`spec.router.name`* picks the binary, and this setting only says where the binaries come from. Its default is safe for every cluster at once in a way `kv-cache-backend-image`'s is not: it runs no model, so it links no accelerator runtime and cannot be paired with the wrong one. The default is this project's own build rather than an upstream tag, because the three routers are compiled from three separate sources and one of them carries a patch this repository ships, so no upstream image holds them together. An image named on the object is used verbatim and is never redirected to the cluster mirror, because it is the user's own reference. |
| `model-deployment-router-proxy-image`<br/>(`GPUSTACK_MODEL_DEPLOYMENT_ROUTER_PROXY_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/mirrored-envoy:distroless-v1.33.2`<br/>Usage: Proxy fronting a managed router's endpoint picker. It has **no field on the API** to override it: this operator renders the proxy's configuration against one proxy's configuration schema, so swapping the binary would mean swapping that configuration too. The setting exists for registry redirection and for pinning a release back, not for running a different proxy. |
| `model-deployment-routing-sidecar-image`<br/>(`GPUSTACK_MODEL_DEPLOYMENT_ROUTING_SIDECAR_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0`<br/>Usage: Sidecar a decoder runs to accept a remote *prefill* handoff. Same terms as the proxy above: this operator renders its arguments, so the setting is for redirection and pinning rather than for a different implementation. |
| `model-deployment-tcp-tw-reuse`<br/>(`GPUSTACK_MODEL_DEPLOYMENT_TCP_TW_REUSE`) | Effect window: Next trigger<br/>Default: `false`<br/>Usage: Render `net.ipv4.tcp_tw_reuse=1` on the *prefill* half of every SGLang *prefill*/*decode* pair, which otherwise [runs out of local ports](../modules/model-deployment/engine-versions.md#limitations) under sustained load. **Allow the sysctl on the kubelet of every node that can run such a `Pod` before turning it on**: without that the `Pod` fails with `SysctlForbidden`, and so does every replacement. The steps, which `Pod`s it reaches and how to confirm the refusal are under [SGLang TIME-WAIT reuse](#sglang-time-wait-reuse). |
| `model-prefetch-warmup-image`<br/>(`GPUSTACK_MODEL_PREFETCH_WARMUP_IMAGE`) | Effect window: Immediate<br/>Default: `gpustack/mirrored-python:3.12-alpine`<br/>Usage: Image the [`ModelPrefetch`](../modules/model-delivery/prefetch.md) warm-up `Pod` runs on each target node. The `Pod` mounts the artifact's volume, verifies every file reads back and exits, so the image needs only a shell, `find` and `sha256sum` — it never runs a model. It is a setting because the one thing it must guarantee is that the node can pull it, which only the cluster's own registry arrangement decides; an air-gapped cluster points it at its mirror. |
| `model-store-download-bandwidth`<br/>(`GPUSTACK_MODEL_STORE_DOWNLOAD_BANDWIDTH`) | Effect window: Immediate<br/>Default: `0`<br/>Usage: Per-node download rate limit, a quantity of bytes per second (`200Mi`); `0` is unlimited. |
| `model-store-download-concurrency`<br/>(`GPUSTACK_MODEL_STORE_DOWNLOAD_CONCURRENCY`) | Effect window: Immediate<br/>Default: `8`<br/>Usage: Concurrent HTTP requests the plugin makes per node, across every download; `1` to `64`. |
| `model-store-high-watermark`<br/>(`GPUSTACK_MODEL_STORE_HIGH_WATERMARK`) | Effect window: Immediate<br/>Default: `80`<br/>Usage: The node cache filesystem's usage percent above which the plugin removes content no `Pod` mounts; above the low watermark, at most `95`. On a filesystem shared with kubelet the plugin caps it below kubelet's thresholds ([the capacity rule](../modules/model-delivery/operations.md#capacity-rule)). |
| `model-store-low-watermark`<br/>(`GPUSTACK_MODEL_STORE_LOW_WATERMARK`) | Effect window: Immediate<br/>Default: `70`<br/>Usage: The usage percent a collection removes down to; at least `1`, below the high watermark. |
| `model-store-peer-sync`<br/>(`GPUSTACK_MODEL_STORE_PEER_SYNC`) | Effect window: Immediate<br/>Default: `true`<br/>Usage: Whether a node's plugin may pull a model's content from the plugins of other nodes that hold it published, instead of the hub. |
| `settings-default-tracking` | Effect window: Next trigger<br/>Default: `instance-ssh-server-image`, `kv-cache-backend-image`, `model-deployment-router-image`, `model-deployment-router-proxy-image`, `model-deployment-routing-sidecar-image`, `model-prefetch-warmup-image`<br/>Usage: Comma-separated names of the `Setting`s the operator rewrites with its built-in default on every worker start, so that a default shipped in a later release reaches a cluster that never chose a value. A name in the list makes its `Setting` follow rule 3 ([who owns a value](#who-owns-a-value)); removing a name is what keeps an image pin across an operator upgrade ([keeping a pin](#keeping-an-image-pin)). An empty value tracks nothing, and whitespace, duplicates and unregistered names are dropped ([the tracking list](#tracking-list)). No environment variable reads this `Setting`, so nothing on the `Deployment` can take the list over. |
| `workload-fit-affinity`<br/>(`GPUSTACK_WORKLOAD_FIT_AFFINITY`) | Effect window: Immediate<br/>Default: `true`<br/>Usage: Whether the `Workload` webhook pins a logical slice, or a shared request of two or more accelerators, on an operator queue to nodes whose [fit labels](../modules/devices/scheduling.md#per-card-fit-labels) show that one accelerator can still host it. When `true`, Kueue's topology-aware scheduling skips a fragmented node; when `false`, `Workload`s pass unchanged while the fit labels are still published, so the pin can be switched off on its own. Read on every webhook call, so a change takes up to 30s to reach it, and it applies to `Workload`s created or updated after that. |

### Authoring the InstanceType

With `instance-type-derived-from-node=false` the operator authors no `InstanceType`, so the
administrator owns both halves of a pool: which `InstanceType`s exist, and whether what their queue
admits can actually be placed. Flipping the setting off does not retire the types the operator
already authored; the derived marker is provenance, and nothing auto-removes a type.

No `ClusterQueue` gains a reference to the node-devices feasibility gate in this mode. That
[AdmissionCheck](../modules/devices/admission.md#gate-3-per-accelerator-admissioncheck) has one
writer, [`NodeQueueReconciler`](../modules/devices/scheduling.md#queue-quota-and-draining),
which reads this setting as a cluster-wide switch, not as "was this queue derived".

So with the switch on, every accelerated queue carries the reference once the check reports `Active`,
whoever authored its `InstanceType`; with it off none is added, and a queue that already carries one
drops it the next time its resource groups are refilled, not at the moment of the flip.

> **Why it is not attached in this mode anyway** — the gate changes what a queue admits, and in the
> mode where the administrator authors every `InstanceType` that is theirs to decide.

The failure shape to expect: without that gate, a `Workload` is admitted on quota alone and its `Pod`s
then sit `Pending`, which reads as a full cluster while the usual truth is a request no node can
place, because quota is a scalar total and cannot see per-accelerator fragmentation. When the `Pod`s
of an admitted `Workload` stay `Pending`, compare the request against the per-accelerator ledger
(`kubectl get devices <node> -o yaml`) before adding capacity.

The joint-admission check is attached in both modes. Every queue backing an `InstanceType`
references `gpustack-model-deployment-joint` once it reports `Active`, whoever authored the type and
whatever this setting says.

It answers `Ready` at once for every `Workload` that is not a replica of a multi-role `ModelDeployment`,
so the only thing it changes in this mode is that such a deployment is admitted as a set; see
[Prefill and decode](../modules/model-deployment/deployment.md#prefill-and-decode), and
[Status](../modules/model-deployment/status.md#status) for a set that stays short of quota.

Write the `InstanceType` against the `InstanceTypeFlavor` catalog. It is the read-only,
os/arch-agnostic view of the pools that actually exist (one entry per grouping the settings above
produce), and its *`spec`* carries the identity fields an `InstanceType` is built from:
*`acceleratorGroup`*, *`generalGroup`*, `acceleratable`, `manufacturer`, `product`, `family`, `memory`,
`cores`.

```bash
kubectl get instancetypeflavors                               # short name: instypeflavor
kubectl get instancetypeflavor gpustack--nvidia-a10g -o yaml
```

An `InstanceType` whose identity matches no entry there backs a `ClusterQueue` that selects no
`ResourceFlavor`, so the queue is left with empty resource groups and admits nothing.

### SGLang TIME-WAIT reuse

`model-deployment-tcp-tw-reuse` renders `net.ipv4.tcp_tw_reuse=1` into the `Pod`
*`securityContext.sysctls`* of the *prefill* half of every SGLang *prefill*/*decode* pair, and of no other
`Pod`. The sysctl lets the kernel reuse ports held by `TIME-WAIT` sockets for new outgoing
connections, in that `Pod`'s own network namespace only.

The *prefill* half opens a new TCP connection for every transfer, so over `TCP`, with or without a
store, its `TIME-WAIT` sockets [use up its local
ports](../modules/model-deployment/engine-versions.md#limitations) under sustained load.

It does not reach the *decode* half, which accepts transfers rather than opening them; a vLLM role,
which keeps its transfer connections open; or the members of a [KV cache backend](../modules/kv-cache/backend.md).
A role that replaced its *`command`* gets nothing either, since the operator did not build what runs
there. Whether an SGLang server with a store runs out of ports is not measured, and the setting
does not reach it.

1. Allow the sysctl on the kubelet of every node that can run such a `Pod`. It is not on the
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
  *`spec.kubelet.config`* as above or as the flag under *`spec.kubelet.flags`*; see the [`nodeadm`
  API](https://awslabs.github.io/amazon-eks-ami/nodeadm/doc/api/). On Amazon Linux 2, pass the flag
  through the bootstrap script's `--kubelet-extra-args`.
- **AKS** — `allowedUnsafeSysctls` in the kubelet configuration a node pool is created with; see
  [custom node configuration](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration).
- **GKE** — *`kubeletConfig.allowedUnsafeSysctls`* in the node system configuration of a node pool;
  see [node system
  configuration](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/node-system-config).
- A platform that exposes no kubelet configuration cannot run this setting; leave it off there.

2. Turn the setting on.

```bash
kubectl -n gpustack-system patch setting model-deployment-tcp-tw-reuse --type merge -p '{"spec":{"value":"true"}}'
```

**Flipping it recreates the *prefill* replicas it reaches**, in either direction, because the sysctl
is part of the `Pod` spec their fingerprint covers. Each deployment picks the value up on its next
reconcile, and a setting change does not wake one. To apply it at once, delete one of the
deployment's *prefill* `Pod`s: the reconcile that replaces it renders the new value, and the rollout
replaces the other *prefill* replicas. A pair that has already locked up recovers the same way.

If the kubelet was not changed, the *prefill* replica never starts. The node's kubelet refuses the
`Pod`, which ends in phase `Failed` with reason `SysctlForbidden`; the operator deletes it and logs
`removing replica that failed` with that reason, and the replacement is refused the same way, over
and over. The events outlive the `Pod`s, and their message names the sysctl:

```bash
kubectl -n <namespace> get events --field-selector reason=SysctlForbidden
kubectl -n <namespace> get pods -w \
  -l app.kubernetes.io/instance=<deployment>,app.kubernetes.io/component=<prefill-role>
```

The second command shows the *prefill* `Pod`s cycling through `SysctlForbidden`. Allow the sysctl on
those nodes or turn the setting off; either ends the loop at the next replacement.

A namespace enforcing the `baseline` or `restricted` `Pod` Security Standard refuses the `Pod` at
creation instead, since the sysctl is not on that standard's allowed list either: no `Pod` appears,
and the operator logs the refusal.

## Troubleshooting

Both symptoms below are rules 2 and 3 seen from the other side. A value that changed without your
action was written by the operator at a worker start, and the override log line says which rule wrote
it, along with the variable when one did.

### Stored value stopped applying

Rule 2 wrote a variable's value over the one you stored. The log line reports it as
`rule="env-declared"` and names the variable in `envName`, so you do not have to work out which
declaration claimed the `Setting`.

The way out is the variable, not the `Secret`:

- A variable you set yourself, through the chart's `worker.env` or by editing the worker `Deployment`.
  Change it or remove it and your stored value stands at the next worker start. A hand-edited
  `Deployment` is outside the chart's record, so a rollback or an upgrade run with
  `--reset-then-reuse-values` drops it; a variable meant to last belongs in a chart value.
- A variable the chart renders, the five rows marked above. Change the chart value, or clear it so the
  chart renders an empty one, and `helm upgrade`. A blank declaration claims nothing, so clearing the
  chart value is what hands the `Setting` back to you.

| Chart-rendered variable | Chart value that produces it | Chart default |
|---|---|---|
| `GPUSTACK_CONTAINER_REGISTRY` | `global.imageRegistry`, falling back to `global.hub` | *(empty)* |
| `GPUSTACK_CONTAINER_NAMESPACE` | `global.imageNamespace` | *(empty)* |
| `GPUSTACK_IMAGE_PULL_SECRETS` | `global.imagePullSecrets` | *(empty)* |
| `GPUSTACK_IMAGE_PULL_POLICY` | `global.imagePullPolicy`, falling back to `worker.image.pullPolicy` and then *`image.pullPolicy`* | `IfNotPresent` |
| `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE` | `modelManager.enabled`, which renders `Node` while the plugin is deployed | *(not rendered)* |

Setting the chart value to an empty string is the same as clearing it. With `modelManager.enabled: false`
the chart stops rendering the delivery mode entirely, and the stored value is then nobody's to write.

### Edit reverted on restart

Either rule wrote over the edit, and `rule` in the log line says which one:

- `rule="env-declared"` means a variable holds the value. Changing or clearing that variable is the way
  out, as above.
- `rule="default-tracked"` means the `Setting`'s name is in `settings-default-tracking`. Removing the name
  from that list is the way out.

Your edit holds until the next worker start, and nothing reverts it while the worker runs. A node drain,
a crash, a restart and an upgrade each start a worker, so each of them applies the rules again.

### Keeping an image pin

An image pin does not survive an operator upgrade: the six image `Setting`s are tracked by default, so
the operator writes each of them with its built-in
default on **every** worker start, not only on the start where a release changes that default. A pin
made with `kubectl patch` holds until the next start, and reverts to the default at that one:

```bash
kubectl -n gpustack-system patch setting kv-cache-backend-image --type merge \
  -p '{"spec":{"value":"mirror.example/kv-cache:1.2.3"}}'
```

Untracking the `Setting` is the only thing that keeps the pin, and nothing else stands in for it: a
variable set to an empty value is no declaration, and a default that is blank is never written at all.

```bash
# Read the list, drop the one name whose Setting you want to keep your own value for,
# and write the rest back. This removes model-prefetch-warmup-image and tracks the other five.
kubectl -n gpustack-system get setting settings-default-tracking -o yaml
kubectl -n gpustack-system patch setting settings-default-tracking --type merge \
  -p '{"spec":{"value":"instance-ssh-server-image,kv-cache-backend-image,model-deployment-router-image,model-deployment-router-proxy-image,model-deployment-routing-sidecar-image"}}'
```

A pin that predates this behavior is replaced on the first worker start of the release that introduces
it, and the log line names both values when it happens. Deleting the `Secret` has the same effect on the
list, which comes back as its six default names.

## Deployment environment variables

These environment variables are read when the process starts, so changing one means restarting or
redeploying the affected component.
A variable a `Setting` reads is re-read on each of those starts rather than applied once
([who owns a value](#who-owns-a-value)); the rest are plain process configuration.

The Worker (WK) copies every `GPUSTACK_`-prefixed variable from its own `Pod` spec onto the Device Manager
(DM) `DaemonSet`s, so setting one on the Worker `Deployment` reaches the DMs automatically.

### General variables

| Variable | Default | Component | Effect |
|----------|---------|-----------|--------|
| `GPUSTACK_DATA_DIR` | `/var/lib/gpustack` | all | Root directory for data storage. |
| `GPUSTACK_CONF_DIR` | `/etc/gpustack` | all | Root directory for configuration and metadata, e.g. bundled Helm charts. |
| `GPUSTACK_PCI_CLASS_PREFIXES` | `02,03,0b,12` | DM | Comma-separated PCI class prefixes treated as display/accelerator devices (see the [PCI class registry](https://admin.pci-ids.ucw.cz/read/PD)). Applied to the DM's local sysfs PCI scan, and to nothing else. The same list appears twice more, and neither reads this variable: the chart value `node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist` decides which classes NFD labels, and `pkg/nodefeature` is what the `gpustack-cpu-info` `NodeFeatureRule` matches (a Go test holds those two equal). Change one and change all three. |
| `GPUSTACK_DEVICES_GROUP_ID_WITH_MEMORY` | `false` | DM | When `true`, the devices group ID gains a memory-size suffix (e.g. `nvidia-tesla-t4-16g` instead of `nvidia-tesla-t4`), so same-model devices with different VRAM sizes form distinct groups. |
| `GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE` | `true` | DM | Whether `Allocate` refuses a logical slice its accelerator cannot hold: one with no free slot, or without the units the slice needs. The `Pod` fails with `UnexpectedAdmissionError` and its controller recreates it. `false` allocates it anyway and clamps the ledger at zero, as before the refusal existed; see [Disabling an allocator safeguard](#disabling-an-allocator-safeguard). |
| `GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET` | `true` | DM | Whether `Allocate` asks the kubelet's pod-resources API which container it serves. `false` identifies it by the pending-`Pod` heuristic alone, as before the lookup existed; a node whose kubelet cannot be reached falls back to that heuristic by itself. See [Disabling an allocator safeguard](#disabling-an-allocator-safeguard). |
| `GPUSTACK_LEDGER_RELEASE_TERMINATED_PODS` | `true` | WK, DM | Whether the accelerator ledger stops charging a `Pod` in phase `Succeeded` or `Failed` for the exclusive cards, shared cards and logical slices kubelet has taken back from it, so a finished `Job` that nobody deletes no longer keeps its card from the fit labels, the `InstanceType` views and the node-devices check. A hardware partition, and a MetaX or Cambricon slice, stay charged until the `Pod` object is gone, because only then are they destroyed. `false` charges every finished `Pod` until its object is gone, as before, and makes the sliced `Allocate` gate charge it too, which it did not do before; set it through the chart value `ledgerReleaseTerminatedPods`, which renders it on the worker and every Device Manager so the two sides agree. |

### Disabling an allocator safeguard

Two device-plugin safeguards default on, and each has a switch for the case where it misjudges a
node. Set it through the chart, which renders `deviceManager.env` onto every Device Manager `DaemonSet`:

```bash
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reset-then-reuse-values \
  --set-string deviceManager.env.GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=false
```

- `GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=false` is for a slice refused although its accelerator
  has room: the `Pod` keeps coming back `UnexpectedAdmissionError` naming that accelerator. With it
  off, the slice is allocated and the ledger clamps that accelerator's `Remaining` at zero.
- `GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET=false` is for allocations recorded on the wrong `Pod` while
  the kubelet is reachable. With it off, the Device Manager picks the oldest pending `Pod` the request
  could be for, which can record two `Pod`s bound together on each other.

The change rolls the Device Manager `Pod`s, one node at a time. Running workloads are not touched: a
Device Manager restart does not reach a started container, and its allocation records live on the
`Pod`s, where the restarted process reads them. A `Pod` admitted while its node's Device Manager is
restarting can fail admission, as across any Device Manager restart, and its controller recreates it.

To switch the safeguard back on, remove the value. Set it to `null` with `--set`, not `--set-string`,
which would store the string `"null"`:

```bash
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reset-then-reuse-values \
  --set deviceManager.env.GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE=null
```

### Per-manufacturer overrides

Override patterns expand for every known manufacturer (`amd`, `ascend`, `cambricon`, `hygon`,
`iluvatar`, `metax`, `mthreads`, `nvidia`, `thead`); both the WK and the DM read them, so the propagation
above keeps the two sides consistent.

**Installed by the chart, set the `global.manufacturers` row, not these variables.** The overrides
here are fields of that row (`pciVendorID`, `resourceName`, `runtimeName`, `partitionKind`), which the
chart fans out as the matching variable to the worker *and* the Device Managers, along with what a variable
cannot reach: the `DaemonSet` node selectors, the RuntimeClasses it creates, Kueue's credits mapping. The
variable alone leaves those stale; the next render overwrites it.

- `GPUSTACK_${MANUFACTURER}_PCI_VENDOR_ID` — the PCI vendor ID used for NFD node selection and device scanning. Accepts `${vendor}` or `${class}_${vendor}`.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RESOURCE_NAME` — the extended resource name the scheduling chain allocates against.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RUNTIME_NAME` — the container runtime class name used for accelerated workloads. A `RuntimeClass` of that name is attached only when one exists in the cluster; the chart creates one only where the `global.manufacturers` row sets a `runtimeInjectsDriver` or `runtimeInjectsDevices` fact (see `global.manufacturers`) — never for a manufacturer setting neither, whose default below holds only if something else created the class.

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

`nvidia` is the only one with a default: `mig`, giving `nvidia.com/gpu.partitioned.mig-${profile}`.
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

Prefer `auto` over naming the CDI channel yourself. The CDI channel needs a container engine that
resolves CDI requests (containerd 2.x does; containerd 1.7 only with `enable_cdi = true`). Ask for it on
an engine that does not and the request is simply ignored: no variable is set either, and the container
starts with no accelerator and no error, which is the failure this setting exists to remove. `auto` reads
the engine first and keeps the variable when the answer is no.

Two independent questions decide which value a runtime wants: does the engine resolve CDI
requests at all, and can `auto` tell that it does? `auto` reads both answers out of containerd's
`config.toml`, so a runtime that keeps them anywhere else is invisible to it. That file is where the
detection looks; it is not something the channel itself needs.

| Container runtime | Resolves CDI | Detectable by `auto` | Recommended |
|---|---|---|---|
| containerd 2.x (configuration version 3) | Yes, always | Yes, from the version | `auto` |
| containerd 1.7 with `enable_cdi = true` | Yes | Yes, from the key | `auto` |
| containerd 1.7, `enable_cdi` unset | No | Yes | `auto`, which keeps the variable. Naming `cdi-annotations` here is the silent no-accelerator case above |
| CRI-O | Yes, natively | **No** — it has no `config.toml` | `cdi-annotations` |

`cdi-annotations` is the answer on exactly one row: a runtime that does resolve CDI requests but that
`auto` has no way to see. Everywhere else `auto` reaches the same channel by itself, or correctly
declines to; naming the channel where the engine ignores it is how a container ends up running
with nothing.

Where to set it: the name embeds the manufacturer in upper case, so it is not a literal you will
find spelled out anywhere. Pass it through the chart's `deviceManager.env`, which lands on every
Device Manager `DaemonSet`:

```bash
helm upgrade ... --set deviceManager.env.GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY=auto
```

It is read once, when that manufacturer's allocator is constructed, so a change takes effect only when
the `DaemonSet` restarts. A value that is not one of the supported choices is reported and the node keeps `envvar`:
refusing to start the allocator over it would take the node's accelerators with it.

`auto` also keeps the variable when the engine's own default runtime is already the vendor runtime.
That is the usual shape on a distribution that ships the GPU toolkit for you. There every `Pod` runs under
that runtime whether it asks to or not, so the variable works and a CDI request would only add a second
injection path. `auto` doing nothing on such a node is the correct answer, not a misconfiguration.

Which channel a node settled on, and why, is logged once per answer, but at the allocator's own
verbosity, above what the `DaemonSet` ships with. [Runtime log
verbosity](../contribute/development.md#runtime-log-verbosity) raises it on a running `Pod`.

One node-level variable feeds the same decision:

- `GPUSTACK_CONTAINERD_CONFIG_DIR` — the directory holding the container engine's `config.toml`, from
  the chart's `deviceManager.containerdConfigDir` (default `/etc/containerd`). `auto` reads it to learn
  whether the engine resolves CDI requests and which runtime handler a `Pod` naming no
  *`runtimeClassName`* runs under; unreadable keeps the node on `envvar`.

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

Populated by the `Pod` specs GPUStack Operator renders (Downward API or `Service` environment); not
user-facing settings, listed for completeness.

| Variable | Default | Effect |
|----------|---------|--------|
| `KUBERNETES_NODE_NAME` | — (required) | Name of the node the `Pod` runs on; the DM uses it to name its `NodeFeature`/`Devices` objects. |
| `KUBERNETES_POD_NAME` | — | The WK's own `Pod` name, used to read back its container spec (image, pull policy, `GPUSTACK_*` env) for rendering the DM `DaemonSet`s. |
| `KUBERNETES_POD_NAMESPACE` | `gpustack-system` | System namespace where managed resources live. |
| `KUBERNETES_POD_IP` | — | Overrides the auto-detected primary host IP in topology discovery. |
| `KUBERNETES_SERVICE_NAME` | `gpustack-operator-worker` | `Service` name used for system routing. |
| `KUBERNETES_SERVICE_HOST` | — | Standard in-cluster marker; its presence tells the embedded runner it is inside a cluster. |

### Proxy and internal flags

| Variable | Default | Effect |
|----------|---------|--------|
| `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | — | Standard proxy settings, passed through to the embedded Kubernetes installer. |
| `NO_PROXY` / `no_proxy` | — | Also parsed (hosts, IPs, CIDRs) to bypass the proxy on direct HTTP calls. |
| `_RUNNING_INSIDE_CONTAINER_` | `false` | Internal marker baked into the container image; switches data/conf paths to their absolute in-container locations. Not intended to be set by users. |

---

**See also** — [Architecture](../getting-started/architecture.md) (what each setting regroups) ·
[Installation Modes](../operate/installation-modes.md) (which flags a mode sets for you) ·
[High Availability Operations](../operate/high-availability.md)

**Next** → [Walkthrough](../walkthroughs/devices/scheduling.md) — a setting flipped on a live cluster, before and after.
