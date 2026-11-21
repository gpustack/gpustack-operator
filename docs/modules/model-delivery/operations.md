# Model Store Operations

The `model-manager` `DaemonSet` maintains one verified weight cache per `Node`. It downloads each hub
`ModelArtifact` digest once and mounts it read-only via `model.csi.gpustack.ai` after verifying
namespace authorization.

## Contents

- [Prerequisites](#prerequisites)
- [Configuration](#configuration)
- [Node cache inspection](#node-cache-inspection)
- [Capacity rule](#capacity-rule)
- [Switch delivery](#switch-delivery)
- [Replica placement](#replica-placement)
- [Upgrade notes](#upgrade-notes)
- [Removal and cache migration](#removal-and-cache-migration)
- [Troubleshooting](#troubleshooting)

## Prerequisites

- **Operator namespace Pod Security**: The plugin container runs privileged (`Bidirectional` mount
  propagation), so the operator namespace requires `pod-security.kubernetes.io/enforce: privileged`.
  Tenant namespaces can enforce `restricted`.
- **Image mode defaults**: Image mode deploys `model-manager` unless `--disable-applications`
  includes `model-manager` ([Installation Modes](../../operate/installation-modes.md)), leaving
  `model-artifact-delivery-mode` at `Engine` until changed and requiring standard `/var/lib/kubelet`.
- **Custom kubelet root**: Distributions using non-standard kubelet paths (such as `/var/snap/...` or
  `/var/lib/k0s/kubelet`) require chart mode with `modelManager.kubeletDir` matching that path.

## Configuration

The Helm chart enables `modelManager.enabled: true` by default, deploying the `model.csi.gpustack.ai`
`CSIDriver`, per-node `DaemonSet`, and least-privilege RBAC (the plugin reads no `Secret`s directly;
kubelet passes each mount's `ModelArtifact` token):

```yaml
modelManager:
  enabled: true
  rootPath: /var/lib/gpustack/models  # mount a dedicated filesystem here in production
  kubeletDir: /var/lib/kubelet
  securePort: 32444  # HTTPS /metrics, /readyz, and /livez
  registrar:
    image:
      repository: docker.io/gpustack/mirrored-csi-node-driver-registrar
      tag: v2.17.0
  nodeSelector: {}
  tolerations:
    - operator: Exists
```

The worker merges cluster `Setting`s and matching `ModelStore` overrides into every
`NodeModelStore.spec` within `1m`; plugins read only `NodeModelStore.spec` and the optional CA
`ConfigMap` in the operator namespace.

### Settings and precedence

Non-empty `GPUSTACK_*` environment variables on the worker `Deployment` overwrite stored `Setting`
values on every worker restart (`modelManager.enabled: true` sets
`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE=Node`; `modelTransfer.enabled: false` sets
`GPUSTACK_MODEL_ARTIFACT_REMOTE_DELIVERY_POLICY=Disabled`). Persist changes to chart-seeded settings
in Helm `values.yaml`, and check overrides on startup:

```bash
kubectl -n gpustack-system get settings
kubectl -n gpustack-system get deploy -o yaml | grep -A1 'GPUSTACK_'
kubectl -n gpustack-system logs deploy/gpustack-operator-worker | grep 'setting overridden on start'
```

| `Setting` (`GPUSTACK_*` env) | Default | Effect and constraints |
|---|---|---|
| `model-artifact-delivery-mode` (`..._DELIVERY_MODE`) | `Engine` (`Node` in chart) | Immediate; `Engine` or `Node` (`Node` requires `CSIDriver`). |
| `model-artifact-remote-delivery-policy` (`..._REMOTE_DELIVERY_POLICY`) | `Prefer` | Immediate; `Disabled`, `Require`, or `Prefer`. |
| `model-artifact-huggingface-endpoint` (`..._HUGGINGFACE_ENDPOINT`) | `https://huggingface.co` | Next trigger; hub URL for resolution and downloads. |
| `model-artifact-modelscope-endpoint` (`..._MODELSCOPE_ENDPOINT`) | `https://www.modelscope.cn` | Next trigger; bare host passed to engines as `MODELSCOPE_DOMAIN`. |
| `model-artifact-https-proxy` (`..._HTTPS_PROXY`) | *(blank)* | Next trigger; `http`/`https` URL without embedded credentials. |
| `model-artifact-no-proxy` (`..._NO_PROXY`) | *(blank)* | Next trigger; comma-separated hosts bypassing the proxy. |
| `model-artifact-ca-bundle` (`..._CA_BUNDLE`) | *(blank)* | Next trigger; `ConfigMap` (`ca.crt`) in operator namespace (plugin and worker only). |
| `model-artifact-revalidate-interval` (`..._REVALIDATE_INTERVAL`) | `24h` | Next trigger; minimum `1m`. |
| `model-store-high-watermark` (`..._STORE_HIGH_WATERMARK`) | `80` | Immediate; integer above low watermark, max `95`. |
| `model-store-low-watermark` (`..._STORE_LOW_WATERMARK`) | `70` | Immediate; integer `>= 1`, below high watermark. |
| `model-store-download-concurrency` (`..._STORE_DOWNLOAD_CONCURRENCY`) | `8` | Immediate; concurrent HTTP requests per `Node` (`1..64`). |
| `model-store-download-bandwidth` (`..._STORE_DOWNLOAD_BANDWIDTH`) | `0` | Immediate; byte rate (`200Mi`); `0` means unlimited. |
| `model-store-peer-sync` (`..._STORE_PEER_SYNC`) | `true` | Immediate; enables peer pulls on port `32445` ([Node-to-node sync](prefetch.md#node-to-node-sync)). |
| `model-prefetch-warmup-image` (`..._PREFETCH_WARMUP_IMAGE`) | `gpustack/mirrored-python:3.12-alpine` | Immediate; warm-up `Pod` image (`sh`, `find`, `sha256sum`). |

Invalid `GPUSTACK_*` seeds stop worker startup; invalid `NodeModelStore.spec` values set
`Ready=False/InvalidConfiguration` on the `Node` and halt new downloads while keeping published
mounts readable. Update an unseeded runtime `Setting` directly:

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: model-store-download-bandwidth
   namespace: gpustack-system
 spec:
-  value: "0"
+  value: 200Mi
```

### Pool layer

A cluster-scoped [`ModelStore`](prefetch.md#cache-resources) overrides watermarks and download
limits field by field on matched `Node`s (writing the winning store name to
`NodeModelStore.spec.store`). If two `ModelStore`s match the same `Node`, both report
`SelectorOverlap` and the alphabetically first name wins; make selectors disjoint instead of relying
on tie-breaking.

## Node cache inspection

```bash
kubectl get nms
kubectl get nms gpu-node-01 -o yaml
kubectl get nms gpu-node-01 -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modelartifacts/<name>/progress"
kubectl get --raw "/api/v1/namespaces/gpustack-system/pods/https:<plugin-pod>:32444/proxy/metrics"
kubectl -n gpustack-system logs -l app.kubernetes.io/component=model-manager --tail=100
```

- **Applied config**: *`status.observedGeneration`* matching *`metadata.generation`* confirms the
  plugin applied *`spec`*; the `Ready` message notes any kubelet watermark cap.
- **Cached digests**: *`status.models`* lists each digest (`Ready`, `Downloading`, or `Failed`),
  *`mounted`* state, and *`downloadedBytes`* (updated every `30 s` on `5%` steps) without exposing
  tenant names.
- **Retry backoff**: `Failed` entries record *`reason`* and *`retryTime`*; fix the underlying cause
  and wait past *`retryTime`* for the next mount retry.

### Model Manager command

`model-manager` (alias `mm`) runs beside `node-driver-registrar` and reads runtime policy from
`NodeModelStore.spec`. Only `--model-manager-service-account` (default
`gpustack-operator-model-manager`) can update `nodemodelstores/status`:

```bash
gpustack-operator model-manager --node-name="$(hostname)" --cache-root=/mnt/models
```

| Flag | Default | Purpose |
|---|---|---|
| `--node-name` | — | Target `Node` and `NodeModelStore` name. |
| `--kubelet-dir` | `/var/lib/kubelet` | Kubelet root containing `pods` mount directories. |
| `--cache-root` | `/var/lib/gpustack/models` | Host directory storing cached model trees. |
| `--csi-socket` | `<kubelet-dir>/plugins/model.csi.gpustack.ai/csi.sock` | CSI Unix domain socket path. |
| `--secure-port` | `32444` | HTTPS port serving `/metrics`, `/readyz`, and `/livez`. |

### Cache collection rules

Collection runs above the high watermark, when a reservation does not fit, and every `5m`. It evicts
oldest *`lastUsedTime`* entries down to the low watermark while skipping mounted trees and
`ModelPrefetch` pins (*`spec.pinned`*):

- **Published trees**: Eligible `10m` after the last `Pod` unmounts them.
- **Stale partials and failures**: Idle partial downloads and disk-absent `Failed` status entries
  expire after `24h`.

## Capacity rule

Watermarks measure total filesystem usage (including root-reserved blocks), and active downloads
reserve their full size upfront. If nothing unpinned and unmounted can be evicted, the `Node` sets
`CapacityLow=True`.

**Mount a dedicated filesystem at `modelManager.rootPath`.** When `rootPath` shares kubelet's `pods`
filesystem, the plugin caps the high watermark at the lowest of `100 - nodefs.available - 5`,
`100 - imagefs.available - 5`, and `imageGCHighThresholdPercent - 5`.

The worker reads those thresholds from kubelet `configz` (*`spec.kubelet`*) and defaults to an `80%`
cap (`10%`, `15%`, `85`) when unset or unreachable. Model downloads never trigger kubelet
`DiskPressure` or image GC (verified on a shared `265 GB` boot disk).

## Switch delivery

Switching `model-artifact-delivery-mode` between `Engine` and `Node` rolls hub-backed
`ModelDeployment`s once. PVC artifacts always mount directly, and `Instance`s always use `Node` CSI
for hub artifacts:

- **KV cache miss on switch**: vLLM's KV key includes the final segment of `--model` (`model` under
  `Node` versus the repository name under `Engine`), so switching invalidates prior vLLM KV hits.
- **Guardrails**: `Node` without `model.csi.gpustack.ai` blocks new `Pod`s with
  `NodeDeliveryUnavailable`; `Engine` on patterned artifacts blocks new `Pod`s with
  `FilterNeedsNodeDelivery`. [Remote Weight Delivery](remote.md) layers inside the active delivery
  mode without changing `Engine` or `Node` semantics.

## Replica placement

Node-delivered `Pod`s receive preferred node affinity toward `Node`s holding their digest (unless the
`Pod` sets a preferred topology level; see [Topology Operations](../topology/operations.md)). Compute
capacity keeps higher priority than cache locality:

```bash
kubectl get pod <replica> -o jsonpath='{.spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution}{"\n"}{.spec.nodeName}{"\n"}'
kubectl get nms -o custom-columns='NODE:.metadata.name,DIGESTS:.status.models[*].digest'
```

Cold downloads occur when warm `Node`s are full or no `Node` held the digest at `Pod` creation. They
also occur if `TASRespectNodeAffinityPreferred` (enabled by default) is disabled in custom Kueue YAML.

## Upgrade notes

- **Chart `Node` default**: With `modelManager.enabled: true`, the chart seeds
  `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE=Node` on every worker start. Upgrades from pre-node-delivery
  releases roll hub deployments once unless `modelManager.enabled=false` is set.
- **Rolling `model-manager`**: Rolling the `DaemonSet` leaves running `Pod` bind mounts active;
  concurrent new mounts retry automatically once the replacement plugin `Pod` becomes `Ready`.

## Removal and cache migration

- **Disabling `modelManager`**: Switch `model-artifact-delivery-mode` to `Engine` before setting
  `modelManager.enabled=false` so hub deployments keep scaling after `CSIDriver` and
  `NodeModelStore` removal.
- **Host disk cleanup or `rootPath` change**: Uninstalling or repathing `rootPath` leaves existing
  files on disk; remove `/var/lib/gpustack/models` manually after all `Pod`s unmount it.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| No new `Pod` under `Node` delivery | `WeightsReady=False` (`NodeDeliveryUnavailable`) | Verify `modelManager.enabled: true` and `CSIDriver` `model.csi.gpustack.ai`. |
| Patterned artifact creates no `Pod` | `WeightsReady=False` (`FilterNeedsNodeDelivery`) | Set `model-artifact-delivery-mode` to `Node` or remove patterns. |
| `Pod` waiting on cold cache | `WeightsReady=False` (`Materializing`) | Check `FailedMount` byte progress; `Pod` starts within `~2m` of download completion. |
| Node cache download failed | `WeightsReady=False` (`MaterializationFailed`) | Inspect `NodeModelStore.status.models[]` *`reason`*, *`message`*, and *`retryTime`*. |
| Container mount blocked | `WeightsReady=False` (`WeightsNotMounted`) | Inspect `Pod` `FailedMount` events (`PermissionDenied` or image pull error). |

### Failed mount inspection

```bash
kubectl -n <ns> get md <deployment> -o jsonpath='{.status.conditions[?(@.type=="WeightsReady")]}{"\n"}'
kubectl -n <ns> get events --field-selector involvedObject.name=<pod>,reason=FailedMount
kubectl get nms <node> -o yaml
kubectl -n gpustack-system logs <model-manager-pod> --tail=100
```

### Mount refusal vs unavailability

| `FailedMount` signal | Cause | Next action |
|---|---|---|
| `PermissionDenied` | Missing inline CSI volume, missing or unresolved `ModelArtifact` in `Pod` namespace, or UID/*`manifestDigest`* mismatch | Ensure `ModelArtifact` in the `Pod` namespace is `Resolved=True`; recreate the `Pod` if the artifact was recreated. |
| `Aborted` (`materializing sha256:...`) | Background download in progress | Wait for kubelet retry (`Pod` starts up to `~2m` after publication). |
| `Unavailable` right after plugin restart | Plugin informer still syncing initial artifacts | Confirm `model-manager` `Pod` is `Ready` and wait for kubelet retry. |
| `Unavailable` with `Failed` digest | Earlier download failed (`1m` to `1h` backoff) | Fix the `NodeModelStore` failure cause below and wait past *`retryTime`*. |

| `NodeModelStore` reason | Cause | Resolution |
|---|---|---|
| `InvalidRequest` | Invalid hub URL, proxy, or CA bundle (`Ready=False/InvalidConfiguration`) | Fix the `Setting` or `ModelStore` so `NodeModelStore.spec` updates. |
| `AccessDenied` | Hub rejected token (`1m`–`1h` backoff) | Update the `ModelArtifact` `Secret` `token` and wait past *`retryTime`*. |
| `SourceUnavailable` | Hub unreachable/5xx/429, or no peer holds an `Expected` digest | Check network/proxy or seed one peer `Node` for `Expected` anchors. |
| `IntegrityMismatch` | File or manifest hash mismatch | Check upstream mirror integrity; corrupted partials are discarded. |
| `InsufficientCapacity` | Download exceeds high watermark (`CapacityLow=True/NothingToRemove`) | Unpin unused models, add disk space, or reduce concurrent cold mounts. |
| `Canceled` | No `Pod` requested the digest for `5m` | None required; partial files remain and resume immediately on the next mount. |

---

**See also** — [Model Artifact](artifact.md) (artifact contract and `progress` RBAC) · [Model Prefetch](prefetch.md) (cache budgets, warm-up, and peer sync) · [Remote Weight Delivery](remote.md) (host-memory remote weight loading)

**Next** → [Remote Weight Delivery](remote.md) — stream weights from prepared source nodes into serving replicas.
