# Model Store Operations

> **Purpose** — running node delivery: enabling the `model-manager` plugin, where its configuration
> comes from, reading what a node applied and holds, keeping the cache away from kubelet's eviction,
> switching delivery, upgrading, and removing it.
> **Audience** operators · **Prerequisites** [Node Model Store
> Reference](../reference/node-model-store.md) · **Read time** ~10 min

The `model-manager` DaemonSet keeps one model cache per node and mounts a Hugging Face
`ModelArtifact` from it, downloading a digest once per node. What it does on each mount is in the
[reference](../reference/node-model-store.md); this page is what an administrator sets and reads.

## Contents

- [Enable it](#enable-it)
- [Where the configuration comes from](#where-the-configuration-comes-from)
- [Read a node](#read-a-node)
- [The capacity rule](#the-capacity-rule)
- [Switch delivery](#switch-delivery)
- [Upgrade notes](#upgrade-notes)
- [Uninstall, and moving the cache](#uninstall-and-moving-the-cache)
- [A deployment waiting for its weights](#a-deployment-waiting-for-its-weights)

## Enable it

The chart deploys it by default (`modelManager.enabled: true`) with the CSIDriver
`model.csi.gpustack.ai`, one DaemonSet Pod per node, a ServiceAccount and a ClusterRole that grants
only get, list and watch on `modelartifacts` and `nodemodelstores`, update on
`nodemodelstores/status`, and ConfigMaps in the operator namespace. It reads no Secret: kubelet
hands each mount its artifact's Secret.

| Value | Default | Meaning |
| --- | --- | --- |
| `modelManager.rootPath` | `/var/lib/gpustack/models` | the node's cache directory, a hostPath; mount a dedicated filesystem here |
| `modelManager.kubeletDir` | `/var/lib/kubelet` | kubelet's root directory, mounted with `Bidirectional` propagation |
| `modelManager.securePort` | `32444` | metrics, readiness and liveness |
| `modelManager.registrar.image` | `docker.io/gpustack/mirrored-csi-node-driver-registrar:v2.17.0` | the sidecar that registers the plugin with kubelet |
| `modelManager.nodeSelector`, `tolerations` | every node, every taint | where it runs; a node without it cannot mount node-delivered weights |

- **The operator namespace must admit privileged Pods.** The plugin container is privileged, so an
  enforced `pod-security.kubernetes.io/enforce` label there must be `privileged`, as the device
  manager already needs. Tenant namespaces may enforce `restricted`: it admits `csi` volumes.
- **Image mode** installs it unless `--disable-applications` names `model-manager` (values key
  `modelManager`); see [Installation Modes](../architecture/installation-modes.md). Its overlay
  sets only that switch: every other value keeps the chart's default, and no delivery is seeded, so
  `model-artifact-delivery-mode` stays `Engine` until you set it.
- **A kubelet with another root directory** (some distributions use `/var/snap/...` or
  `/var/lib/k0s/kubelet`) needs `modelManager.kubeletDir` set to it, or no mount reaches a Pod. Image
  mode cannot set it, so there the plugin only works with the standard `/var/lib/kubelet`.

## Where the configuration comes from

| Layer | Carrier | Who changes it | What |
| --- | --- | --- | --- |
| Deploy time | chart values; in image mode the worker's overlay, which sets only the switch | installer | the switch, the cache root, the kubelet directory, images, resources and placement, the delivery seed |
| Cluster runtime | [Settings](../settings.md#online-adjustable-settings) in the operator namespace | administrator | delivery mode, Hub endpoint, proxy, no-proxy, CA bundle, watermarks, download concurrency and bandwidth |
| Object | the `ModelArtifact` | tenant | source, revision, patterns, Secret; immutable |

The worker merges the runtime layer into every node's `NodeModelStore.spec` within a minute of a
change, and the plugin reads only that object and the CA ConfigMap it names. Nothing a tenant
writes chooses where a node connects. The Settings this adds:

| Setting | Default | Check |
| --- | --- | --- |
| `model-artifact-delivery-mode` | `Engine`; the chart seeds `Node` with the plugin | `Engine` or `Node`; `Node` needs the CSIDriver |
| `model-store-high-watermark` | `80` | integer, above the low watermark, at most `95` |
| `model-store-low-watermark` | `70` | integer, at least `1`, below the high watermark |
| `model-store-download-concurrency` | `8` | concurrent requests per node, `1` to `64` |
| `model-store-download-bandwidth` | `0` | bytes per second per node as a quantity (`200Mi`); `0` is unlimited |

The endpoint, proxy, no-proxy and CA bundle Settings a `ModelArtifact` already resolves through feed
`spec.hub` as well, so the controller, an engine and the plugin reach the same Hub the same way.
The CA bundle **is** given to the plugin, which runs in the operator namespace.

**A default is checked where it is used.** A value seeded from a `GPUSTACK_*` variable skips
admission, so the worker refuses to start on an invalid one, never writes one into `spec`, and the
plugin checks `spec` again: an invalid one sets `Ready=False`, `InvalidConfiguration`, and no
download starts.

```bash
kubectl -n gpustack-system patch setting model-store-download-bandwidth --type merge -p '{"spec":{"value":"200Mi"}}'
```

## Read a node

```bash
kubectl get nms                                    # Ready and Used per node
kubectl get nms gpu-node-01 -o yaml                # spec: what the plugin applies; status: what it holds
kubectl get nms gpu-node-01 -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
kubectl describe pod <consumer>                    # FailedMount events carry refusals and download progress
```

- **`spec` is the effective configuration**, and `observedGeneration` equal to the object's
  `generation` means the plugin applies it. The `Ready` message says when the high watermark was
  capped, and from what.
- **`status.models`** lists each digest the node holds, is downloading or failed on, and whether a
  Pod mounts it. It names no tenant: find a deployment's digest in its `status.model.manifestDigest`.
- **A `Failed` digest** carries its reason and `retryTime`. Mounts of it do not download again
  before then; fix the cause (the Secret, the proxy, the CA) and the next attempt after `retryTime`
  picks it up.
- **A stale object**: a node the plugin stopped running on keeps its object, with a `Ready`
  condition that no longer changes.
- **Metrics** are on the plugin's port: `kubectl get --raw
  "/api/v1/namespaces/gpustack-system/pods/https:<plugin-pod>:32444/proxy/metrics"`.

## The capacity rule

The watermarks are percentages of the cache filesystem's usage **by everything on it**, with the
blocks reserved for root counted as used, the way kubelet reads a filesystem's available space. Collection
starts above the high one and removes unreferenced content down to the low one, never a tree a Pod
mounts; with nothing removable it sets `CapacityLow=True`.

**Give the cache its own filesystem**, mounted at `modelManager.rootPath`. There the Settings apply
as written. When the cache shares kubelet's filesystem (the two paths report the same device), a
cache near the watermark would push kubelet into disk-pressure eviction or image collection, so the
plugin caps the high watermark:

- It reads `evictionHard["nodefs.available"]`, `evictionHard["imagefs.available"]` and
  `imageGCHighThresholdPercent` from `<kubeletDir>/config.yaml`, converting a quantity to a
  percentage of the filesystem.
- The cap is the lowest of `100 - nodefs.available - 5`, `100 - imagefs.available - 5` and
  `imageGCHighThresholdPercent - 5`. The image thresholds count because the plugin cannot see where
  the image store is, so it assumes the same filesystem.
- A value the file does not set, an absent file and an unparseable one take kubelet's defaults,
  `10%`, `15%` and `85`, which cap at `80`. **Kubelet flags that override the file are not seen**:
  a node configured by flags alone is capped from the defaults.

The low watermark is lowered with the cap when it would reach it. When the cap lowers the Setting,
the `Ready` message says so and whether it came from the file or the defaults; with the defaults
the Setting's own `80` is not lowered.

## Switch delivery

`model-artifact-delivery-mode` chooses how a Hugging Face artifact reaches a `ModelDeployment`:
`Engine`, the engine downloads its commit into its own cache, or `Node`, the plugin mounts it at
`/var/lib/gpustack/model`. A claim artifact is always mounted directly, and an `Instance` always
uses the plugin for a Hugging Face artifact, whatever the Setting says.

- **Changing it rolls every Hugging Face deployment once**, the way an image change does. Replicas
  on the old and the new delivery serve the same weights under the same served name.
- **vLLM's KV store key also carries the last path segment of `--model`**, which differs between the
  two (`model` against the repository's name), so KV blocks written before the switch are not hit
  after it.
- **`Node` is refused while the CSIDriver does not exist.** A value that reaches the store anyway,
  from the environment or a plugin removed later, leaves consumers with `WeightsReady=False`,
  `NodeDeliveryUnavailable`, and no new Pod; running Pods are not touched.
- **A filtered artifact needs `Node`.** Under `Engine`, a deployment on an artifact with
  `allowPatterns` or `ignorePatterns` creates no Pod and reports `FilterNeedsNodeDelivery`.

## Upgrade notes

**From the version before node delivery.** With the plugin enabled, the upgrade seeds
`model-artifact-delivery-mode=Node`, and every `ModelDeployment` on a Hugging Face artifact rolls
once, to Node delivery. A seed fills only a Setting the cluster does not have yet, so there are two
ways to avoid the roll:

1. set the Setting to `Engine` before upgrading, which the seed then leaves alone; or
2. upgrade with `--set modelManager.enabled=false`, which seeds nothing and keeps `Engine`.

Either way you can switch later, at a time of your choosing. A later `helm upgrade` never overrides
the Setting.

**Rolling the plugin.** The DaemonSet rolls one node at a time. Mounted Pods keep running and reading
throughout: a mount is a kernel bind mount. A mount asked for while a node's plugin restarts fails,
kubelet retries it, and it succeeds once the new Pod serves.

## Uninstall, and moving the cache

- **Disabling the plugin** (`modelManager.enabled=false`) removes the CSIDriver, and the worker then
  deletes every `NodeModelStore`. Consumers under Node delivery stop creating Pods with
  `NodeDeliveryUnavailable`; switch the Setting to `Engine` first to keep them scaling.
- **`helm uninstall` with `cleanupOnUninstall=true`** also removes the CRD with its objects, as for
  every CRD of this group.
- **The cache stays on each node.** Remove it by hand once nothing mounts from it, on every node:
  `rm -rf /var/lib/gpustack/models` (or your `rootPath`).
- **Changing `rootPath`** points the plugin at an empty cache. Nothing is migrated or removed from
  the old path, and Pods that mount from it keep their mounts. Treat it as a migration: move the
  directory yourself while no Pod on the node mounts from it, or let the new path fill on demand
  and remove the old one afterwards.

## A deployment waiting for its weights

`WeightsReady` on the `ModelDeployment` says which side to look at; the full table is in the
[Model Artifact Reference](../reference/model-artifact.md#status).

| Reason | Look at |
| --- | --- |
| `NodeDeliveryUnavailable` | the CSIDriver `model.csi.gpustack.ai` and `modelManager.enabled` |
| `FilterNeedsNodeDelivery` | the delivery Setting, or the artifact's patterns |
| `Materializing` | the Pod's `FailedMount` events for bytes received; a Pod starts up to about two minutes after the download ends |
| `MaterializationFailed` | the node's `status.models` entry: reason, message and `retryTime` |
| `WeightsNotMounted` | the Pod's `FailedMount` events: a `PermissionDenied` names the authorization rule that failed |

---

**See also** — [Node Model Store Reference](../reference/node-model-store.md) for every field and
reason · [Model Artifact Reference](../reference/model-artifact.md) for the artifact and its
patterns · [Settings](../settings.md#online-adjustable-settings) for every Setting.

**Next** → [Model Artifact Reference](../reference/model-artifact.md)
