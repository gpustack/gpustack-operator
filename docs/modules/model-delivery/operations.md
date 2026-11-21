# Model Store Operations

The `model-manager` `DaemonSet` keeps one model cache per node. It downloads each Hugging Face or ModelScope
`ModelArtifact` digest once per node and mounts it for workloads. Each mount is authorized against a
`ModelArtifact` in the `Pod`'s namespace and served read-only by `model.csi.gpustack.ai`.

## Contents

- [Prerequisites](#prerequisites)
- [Configuration](#configuration)
- [Read a node](#read-a-node)
- [Capacity rule](#capacity-rule)
- [Switch delivery](#switch-delivery)
- [Replica placement](#replica-placement)
- [Upgrade notes](#upgrade-notes)
- [Removal and cache migration](#removal-and-cache-migration)
- [Troubleshooting](#troubleshooting)

## Prerequisites

- **The operator namespace must admit privileged `Pod`s.** The plugin container is privileged, so an
  enforced `pod-security.kubernetes.io/enforce` label there must be `privileged`, as the Device
  Manager already needs. Tenant namespaces may enforce `restricted`: it admits `csi` volumes.
- **Image mode** installs it unless `--disable-applications` names `model-manager` (values key
  `modelManager`); see [Installation Modes](../../operate/installation-modes.md). Its overlay
  sets only that switch: every other value keeps the chart's default, and no delivery is seeded, so
  `model-artifact-delivery-mode` stays `Engine` until you set it.
- **A kubelet with another root directory** (some distributions use `/var/snap/...` or
  `/var/lib/k0s/kubelet`) needs `modelManager.kubeletDir` set to it, or no mount reaches a `Pod`. Image
  mode cannot set it, so there the plugin only works with the standard `/var/lib/kubelet`.

## Configuration

The chart deploys it by default (`modelManager.enabled: true`) with the `CSIDriver`
`model.csi.gpustack.ai`, one `DaemonSet` `Pod` per node, a `ServiceAccount` and a `ClusterRole` that grants
only get, list and watch on `modelartifacts` and `nodemodelstores`, update on
`nodemodelstores/status`, and `ConfigMap`s in the operator namespace. It reads no `Secret`: kubelet
hands each mount its artifact's `Secret`.

The following chart values show the defaults:

```yaml
modelManager:
  enabled: true
  rootPath: /var/lib/gpustack/models  # Node cache hostPath; mount a dedicated filesystem here.
  kubeletDir: /var/lib/kubelet  # Kubelet root.
  securePort: 32444  # Metrics, readiness and liveness.
  registrar:
    image:
      repository: docker.io/gpustack/mirrored-csi-node-driver-registrar
      tag: v2.17.0
  nodeSelector: {}  # Every node by default.
  tolerations:
    - operator: Exists  # Every taint by default.
```

The registrar sidecar registers the plugin with kubelet. The plugin mounts the kubelet `pods`
directory with `Bidirectional` propagation and mounts its own plugin directory separately.
A node without the plugin cannot mount node-delivered weights.

| Layer | Carrier | Owner | Scope |
| --- | --- | --- | --- |
| Deploy time | chart values; in image mode the worker's overlay, which sets only the switch | installer | the switch, the cache root, the kubelet directory, images, resources and placement, the delivery mode |
| Cluster runtime | `Setting`s (`kubectl -n gpustack-system get settings`) | administrator | delivery mode, Hub endpoint, proxy, no-proxy, CA bundle, watermarks, download concurrency and bandwidth |
| Object | the `ModelArtifact` | tenant | source, revision, patterns, `Secret`; immutable |

The worker merges the runtime layer into every node's `NodeModelStore.spec` within a minute of a
change, and the plugin reads only that object and the CA `ConfigMap` it names. Nothing a tenant
writes chooses where a node connects.

### Settings and precedence

A non-blank `GPUSTACK_*` environment variable in the worker `Deployment` overwrites its `Setting` on
every worker start. Chart-rendered variables (`modelManager.enabled: true` sets
`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE=Node`; `modelTransfer.enabled: false` sets
`GPUSTACK_MODEL_ARTIFACT_REMOTE_DELIVERY_POLICY=Disabled`) revert runtime `Setting` edits on restart,
so change those via Helm values.

```bash
kubectl -n gpustack-system get settings
kubectl -n gpustack-system get deploy -o yaml | grep -A1 'GPUSTACK_'
kubectl -n gpustack-system logs deploy/gpustack-operator-worker | grep 'setting overridden on start'
```

| `Setting` | Default | Constraints |
| --- | --- | --- |
| `model-artifact-delivery-mode` (`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE`) | `Engine` (`Node` via chart) | Immediate; `Engine` or `Node` (`Node` requires `CSIDriver`). |
| `model-artifact-remote-delivery-policy` (`GPUSTACK_MODEL_ARTIFACT_REMOTE_DELIVERY_POLICY`) | `Prefer` | Immediate; `Disabled`, `Require`, or `Prefer`. |
| `model-artifact-huggingface-endpoint` (`GPUSTACK_MODEL_ARTIFACT_HUGGINGFACE_ENDPOINT`) | `https://huggingface.co` | Next trigger; hub URL for resolution and downloads. |
| `model-artifact-modelscope-endpoint` (`GPUSTACK_MODEL_ARTIFACT_MODELSCOPE_ENDPOINT`) | `https://www.modelscope.cn` | Next trigger; bare host passed to engines as `MODELSCOPE_DOMAIN`. |
| `model-artifact-https-proxy` (`GPUSTACK_MODEL_ARTIFACT_HTTPS_PROXY`) | *(blank)* | Next trigger; `http`/`https` URL without credentials. |
| `model-artifact-no-proxy` (`GPUSTACK_MODEL_ARTIFACT_NO_PROXY`) | *(blank)* | Next trigger; comma-separated hosts bypassing proxy. |
| `model-artifact-ca-bundle` (`GPUSTACK_MODEL_ARTIFACT_CA_BUNDLE`) | *(blank)* | Next trigger; `ConfigMap` (`ca.crt`) in operator namespace (not tenant `Pod`s). |
| `model-artifact-revalidate-interval` (`GPUSTACK_MODEL_ARTIFACT_REVALIDATE_INTERVAL`) | `24h` | Next trigger; minimum `1m`. |
| `model-store-high-watermark` (`GPUSTACK_MODEL_STORE_HIGH_WATERMARK`) | `80` | Immediate; integer above low watermark, max `95`. |
| `model-store-low-watermark` (`GPUSTACK_MODEL_STORE_LOW_WATERMARK`) | `70` | Immediate; integer `>= 1`, below high watermark. |
| `model-store-download-concurrency` (`GPUSTACK_MODEL_STORE_DOWNLOAD_CONCURRENCY`) | `8` | Immediate; concurrent HTTP requests per node (`1` to `64`). |
| `model-store-download-bandwidth` (`GPUSTACK_MODEL_STORE_DOWNLOAD_BANDWIDTH`) | `0` | Immediate; bytes per second quantity (`200Mi`); `0` is unlimited. |
| `model-store-peer-sync` (`GPUSTACK_MODEL_STORE_PEER_SYNC`) | `true` | Immediate; enables peer-node pulls on port `32445`. |
| `model-prefetch-warmup-image` (`GPUSTACK_MODEL_PREFETCH_WARMUP_IMAGE`) | `gpustack/mirrored-python:3.12-alpine` | Immediate; warm-up `Pod` image containing shell, `find`, and `sha256sum`. |

The endpoint, proxy, no-proxy and CA bundle `Setting`s a `ModelArtifact` already resolves through feed
*`spec.hub`* as well, so the controller, an engine and the plugin reach the same Hub the same way.
The CA bundle **is** given to the plugin, which runs in the operator namespace.

**A default is checked where it is used.** A value seeded from a `GPUSTACK_*` variable skips
admission, so the worker refuses to start on an invalid one, never writes one into *`spec`*, and the
plugin checks *`spec`* again: an invalid one sets `Ready=False`, `InvalidConfiguration`, and no
download starts.

```bash
kubectl -n gpustack-system patch setting model-store-download-bandwidth --type merge -p '{"spec":{"value":"200Mi"}}'
```

The patch sets the value to `200Mi`:

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: model-store-download-bandwidth
   namespace: gpustack-system
 spec:
-  value: <current-value>
+  value: "200Mi"
```

### Pool layer

A [`ModelStore`](prefetch.md) is a fourth layer above the cluster `Setting`s: a
cluster-scoped object whose *`nodeSelector`* picks a pool and whose watermarks and download limits
override the matched nodes field by field. A field the store leaves out keeps the cluster default,
and an explicit `bytesPerSecond: 0` means "unlimited on this pool". The winner's name lands in the
node's *`spec.store`*; nodes no store matches keep the cluster defaults.

Two stores whose selectors match one node never merge silently: both report `SelectorOverlap`, and
the alphabetically first name wins the shared nodes. Fix the selectors rather than relying on the
order; it is a tie-break, not a policy.

## Read a node

```bash
kubectl get nms                                    # Ready and Used per node
kubectl get nms gpu-node-01 -o yaml                # spec: what the plugin applies; status: what it holds
kubectl get nms gpu-node-01 -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
kubectl describe pod <consumer>                    # FailedMount events carry refusals and download progress
kubectl get nodemodelstores.v1.worker.gpustack.ai  # Ready, Used, Models, Downloading
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modelartifacts/<name>/progress"
kubectl get --raw "/api/v1/namespaces/gpustack-system/pods/https:<plugin-pod>:32444/proxy/metrics"
kubectl -n gpustack-system logs -l app.kubernetes.io/component=model-manager --tail=100
```

- ***`spec`* is the effective configuration**, and *`observedGeneration`* equal to the object's
  `generation` means the plugin applies it. The `Ready` message says when the high watermark was
  capped, and from what.
- ***`status.models`*** lists each digest the node holds, is downloading or failed on, and whether a
  `Pod` mounts it; a download's *`downloadedBytes`* updates every 30 seconds once a download moves by 5%.
  It names no tenant: find a deployment's digest in its *`status.model.manifestDigest`*, and an
  artifact's nodes in its *`status.nodes`* or its
  [progress subresource](artifact.md#progress-subresource).
- **A `Failed` digest** carries its reason and *`retryTime`*. Mounts of it do not download again
  before then; fix the cause (the `Secret`, the proxy, the CA) and the next attempt after *`retryTime`*
  picks it up.
- **A stale object**: a node the plugin stopped running on keeps its object, with a `Ready`
  condition that no longer changes.

### Model Manager command

The `model-manager` (alias `mm`) service runs on every cache node beside `node-driver-registrar` and
reads its runtime configuration from `NodeModelStore.spec`, not from flags. Only the worker's
`--model-manager-service-account` (default `gpustack-operator-model-manager`), bound to its `Pod`,
may update `nodemodelstores/status`.

```bash
gpustack-operator model-manager --node-name="$(hostname)" --cache-root=/mnt/models
```

| Flag | Default | Purpose |
|---|---|---|
| `--node-name` | — | `Node` and `NodeModelStore` name. |
| `--kubelet-dir` | `/var/lib/kubelet` | Kubelet root holding `pods` mount targets. |
| `--cache-root` | `/var/lib/gpustack/models` | Node cache root directory. |
| `--csi-socket` | `<kubelet-dir>/plugins/model.csi.gpustack.ai/csi.sock` | CSI Unix domain socket path. |
| `--secure-port` | `32444` | HTTPS port for `/metrics`, `/readyz`, and `/livez`. |

### Cache collection rules

Collection runs when disk usage passes the high watermark, when a new reservation does not fit, and
every five minutes. It removes oldest *`lastUsedTime`* entries first down to the low watermark,
skipping mounted trees and digests pinned via `ModelPrefetch` (*`spec.pinned`*):

- **Published trees**: Eligible 10 minutes after the last `Pod` unmounts them.
- **Stale partials**: Partial downloads with no active mount attempt expire after 24 hours.
- **Failure records**: Removed from *`status.models`* 24 hours after the last failure when absent from disk.
- **Invalid configuration**: `Ready=False` (`InvalidConfiguration`) halts new downloads and collection while keeping existing published mounts working.

## Capacity rule

The watermarks are percentages of the cache filesystem's usage **by everything on it**, with the
blocks reserved for root counted as used, the way kubelet reads a filesystem's available space. Collection
starts above the high one and removes unreferenced content down to the low one, never a tree a `Pod`
mounts; with nothing removable it sets `CapacityLow=True`.

**Give the cache its own filesystem**, mounted at `modelManager.rootPath`. There the `Setting`s apply
as written. When the cache shares the filesystem holding kubelet's `pods` directory (the two paths
report the same device), a cache near the watermark would push kubelet into disk-pressure eviction
or image collection, so the plugin caps the high watermark:

- It takes `evictionHard["nodefs.available"]`, `evictionHard["imagefs.available"]` and
  *`imageGCHighThresholdPercent`* from the node's *`spec.kubelet`*, which the worker reads from kubelet's
  `configz` endpoint: kubelet's effective configuration, its flags, configuration file and drop-ins
  merged. The plugin reads no kubelet file. A quantity is converted to a percentage of the
  filesystem.
- The cap is the lowest of `100 - nodefs.available - 5`, `100 - imagefs.available - 5` and
  `imageGCHighThresholdPercent - 5`. The image thresholds count because the plugin cannot see where
  the image store is, so it assumes the same filesystem.
- A threshold kubelet does not set takes kubelet's default, `10%`, `15%` and `85`, which cap at
  `80`; so do all three while *`spec.kubelet`* is absent (configz disabled or unreachable), and the
  `Ready` message says the effective configuration could not be read. A quantity at or above the
  filesystem's size cannot be a share of it: it takes the default and the message names it, rather
  than drive the cap to its 2% floor.
- A change of the thresholds reaches the plugin through its *`spec`*, without a restart.

The low watermark is lowered with the cap when it would reach it. When the cap lowers the `Setting`,
the `Ready` message says so and where the thresholds came from; with the defaults the `Setting`'s own
`80` is not lowered.

Measured on a managed node whose cache shares a 265 GB boot disk with kubelet (`nodefs.available`
10%, image collection 85/80): filled to 78.5% under the 80% watermark, the next 65.5 GB download
was refused, kubelet never reported `DiskPressure`, evicted nothing and collected no image.
Reservations count the downloads still running, so several starting together stay under the
watermark as one would.

## Switch delivery

`model-artifact-delivery-mode` chooses how a hub artifact reaches a `ModelDeployment`:
`Engine`, the engine downloads its commit into its own cache, or `Node`, the plugin mounts it at
`/var/lib/gpustack/model`. A claim artifact is always mounted directly, and an `Instance` always
uses the plugin for a hub artifact, whatever the `Setting` says.

- **Changing it rolls every deployment using a hub artifact once**, the way an image change does. Replicas
  on the old and the new delivery serve the same weights under the same served name.
- **vLLM's KV store key also carries the last path segment of `--model`**, which differs between the
  two (`model` against the repository's name), so KV blocks written before the switch are not hit
  after it.
- **`Node` is refused while the `CSIDriver` does not exist.** A value that reaches the store anyway,
  from the environment or a plugin removed later, leaves consumers with `WeightsReady=False`,
  `NodeDeliveryUnavailable`, and no new `Pod`; running `Pod`s are not touched.
- **A filtered artifact needs `Node`.** Under `Engine`, a deployment on an artifact with
  *`allowPatterns`* or *`ignorePatterns`* creates no `Pod` and reports `FilterNeedsNodeDelivery`.

[Remote weight delivery](remote.md) is a second, opt-in policy beside this one: it chooses what a
qualified replica does inside the delivery the `Setting` above already admits, and it never changes
what `Engine` and `Node` mean.

## Replica placement

A node-delivered `Pod` prefers the nodes holding its digest when they can take it, while compute
always comes first so a warm cache never holds a `Pod` back. The webhook adds preferred node
affinity for cached nodes unless the `Pod` requests a preferred topology level; see
[Topology Operations](../topology/operations.md).

```bash
kubectl get pod <replica> -o jsonpath='{.spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution}{"\n"}{.spec.nodeName}{"\n"}'
kubectl get nms -o custom-columns='NODE:.metadata.name,DIGESTS:.status.models[*].digest'
```

A replica on a node that downloads again is expected when the first command shows one of these
(each rule is in the mechanism section linked above):

- **a term naming nodes other than *`spec.nodeName`***: those hot nodes had no room for it;
- **no term at all**: no node held the digest when the `Pod` was created, the `Pod` is not
  node-delivered, or it asks for a preferred topology level;
- **the gate is off**, in a string you override or in a Kueue this chart does not install.

To turn the preference off, set `TASRespectNodeAffinityPreferred: false` in
`kueue.managerConfig.controllerManagerConfigYaml`, copying the whole string, since Helm replaces a
string value whole. The gate is Kueue's, so it also stops Kueue honoring a preferred node affinity
any other author wrote in a TAS queue. `Pod`s keep their terms, which then change nothing. There is no
`Setting` for it.

**The gate is alpha** in the bundled Kueue. A Kueue that does not know a gate its configuration
names refuses to start, so a Kueue upgrade checks it first.

## Upgrade notes

**From the version before node delivery.** With `modelManager.enabled: true`, the chart declares
`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE=Node`. The worker reasserts that value on every start, so
a runtime edit of `model-artifact-delivery-mode` to `Engine` is reverted at the next worker start;
it does not avoid the upgrade roll. Every `ModelDeployment` using a hub artifact rolls once, to
`Node` delivery.

To keep `Engine` through the upgrade, set `modelManager.enabled=false`; this also removes the
plugin and its `CSIDriver`. If you enable the plugin later, the chart declares `Node` again and the
worker writes it at its next start.

**To the version with the placement preference.** Kueue restarts with `TASRespectNodeAffinityPreferred`
on. Running replicas are not touched; new node-delivered `Pod`s carry the preference. A `Pod` in a TAS
queue whose author wrote a preferred node affinity is now ranked by it, where before it was
ignored. If you override `controllerManagerConfigYaml`, your string keeps its own gates: add the
line to get the preference.

What the gate is, and how to turn it off, is in [Replica placement](#replica-placement).

**Rolling the plugin.** The `DaemonSet` rolls one node at a time. Mounted `Pod`s keep running and reading
throughout: a mount is a kernel bind mount. A mount asked for while a node's plugin restarts fails,
kubelet retries it, and it succeeds once the new `Pod` serves.

## Removal and cache migration

- **Disabling the plugin** (`modelManager.enabled=false`) removes the `CSIDriver`, and the worker then
  deletes every `NodeModelStore`. Consumers under `Node` delivery stop creating `Pod`s with
  `NodeDeliveryUnavailable`; switch the `Setting` to `Engine` first to keep them scaling.
- **`helm uninstall` with `cleanupOnUninstall=true`** also removes the CRD with its objects, as for
  every CRD of this group.
- **The cache stays on each node.** Remove it by hand once nothing mounts from it, on every node:
  `rm -rf /var/lib/gpustack/models` (or your `rootPath`).
- **Changing `rootPath`** points the plugin at an empty cache. Nothing is migrated or removed from
  the old path, and `Pod`s that mount from it keep their mounts. Treat it as a migration: move the
  directory yourself while no `Pod` on the node mounts from it, or let the new path fill on demand
  and remove the old one afterwards.

## Troubleshooting

`WeightsReady` on the `ModelDeployment` says which side to look at; the full table is in the
[Model Artifact](artifact.md#status).

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| `WeightsReady=False`, no new `Pod` under `Node` delivery | Reason: `NodeDeliveryUnavailable` | the `CSIDriver` `model.csi.gpustack.ai` and `modelManager.enabled` |
| A patterned artifact under `Engine` delivery creates no `Pod` | Reason: `FilterNeedsNodeDelivery` | the delivery `Setting`, or the artifact's patterns |
| `WeightsReady=False` while the node lists the digest `Downloading` | Reason: `Materializing` | the `Pod`'s `FailedMount` events for bytes received; a `Pod` starts up to about two minutes after the download ends |
| `WeightsReady=False` and the node lists the digest `Failed` | Reason: `MaterializationFailed` | the node's *`status.models`* entry: reason, message and *`retryTime`* |
| The `Pod`'s `PodReadyToStartContainers` is not True yet | Reason: `WeightsNotMounted` | the `Pod`'s `FailedMount` events: a `PermissionDenied` names the authorization rule that failed |

### Inspecting failed mounts

When weights fail to mount, inspect the deployment condition, the `Pod` `FailedMount` events, the
`NodeModelStore` entry for the artifact's `manifestDigest`, and the plugin logs:

```bash
kubectl -n <ns> get md <deployment> -o jsonpath='{.status.conditions[?(@.type=="WeightsReady")]}{"\n"}'
kubectl -n <ns> get events --field-selector involvedObject.name=<pod>,reason=FailedMount
kubectl get nms <node> -o yaml
kubectl -n gpustack-system logs <model-manager-pod> --tail=100
```

### Mount refusal vs unavailability

Read the `FailedMount` event and `NodeModelStore` `status.models[]` to separate permanent
authorization refusals from transient download or backoff states:

| Event or signal | Cause | Next action |
|---|---|---|
| `PermissionDenied` in `FailedMount` | Missing inline ephemeral CSI volume, missing `ModelArtifact` in `Pod` namespace, UID/`manifestDigest` mismatch, or `Resolved!=True` | Verify `ModelArtifact` in the `Pod` namespace is `Resolved=True`; recreate the `Pod` or deployment if the artifact UID or digest changed. |
| `Aborted` (`materializing sha256:...: X of Y received`) | First mount started background download | Wait for kubelet retry; the `Pod` starts up to ~2 minutes after publication completes. |
| `Unavailable` right after plugin restart | Plugin informer has not finished initial artifact sync | Confirm the plugin `Pod` is `Ready` and allow kubelet to retry. |
| `Unavailable` with `Failed` digest and `retryTime` | Earlier download failed; backoff runs 1 minute to 1 hour | Check `NodeModelStore` *`reason`*/*`message`* and plugin logs, fix the cause, and wait past *`retryTime`*. |

`NodeModelStore.status.models[]` reports one of these failure reasons:

| Reason | Meaning | Runtime effect |
|---|---|---|
| `InvalidRequest` | Invalid endpoint, proxy, or CA configuration | Waits for *`NodeModelStore.spec`* to change (`Ready=False/InvalidConfiguration` blocks new downloads). |
| `AccessDenied` | Hub refused the credential | Enters backoff (`1m` to `1h`); fix the artifact `Secret` token. |
| `SourceUnavailable` | Hub unreachable, 5xx/429, stalled byte ranges, or no peer holds an `Expected` digest | Enters backoff; check network/proxy or seed a peer node for `Expected` anchors. |
| `IntegrityMismatch` | File hash/size or manifest digest mismatch | Discards file and enters backoff; check mirror integrity. |
| `InsufficientCapacity` | Reservation exceeds high watermark after collection | Enters backoff (`CapacityLow=True/NothingToRemove`); free space or lower concurrent mounts. |
| `Canceled` | No mount requested the digest for 5 minutes | Keeps partial files; next mount resumes immediately without backoff. |

---

**See also** — [Model Artifact](artifact.md) for the artifact contract, patterns, and `progress` RBAC ·
[Model Prefetch](prefetch.md) for cache warming and budgets ·
[Node-to-Node Sync](node-to-node-sync.md) for peer weight distribution ·
[Remote Weight Delivery](remote.md) for source-memory remote loading.

**Next** → [Model Artifact](artifact.md)
