# Model Prefetch

Declare model cache policies, namespace byte budgets, and warm-up targets across `ModelStore`,
`ModelStoreBinding`, and `ModelPrefetch`. Once one `Node` caches a digest, peer `model-manager`
plugins distribute it over TCP without repeating upstream hub downloads.

| Resource | Scope | Owner | Purpose |
|---|---|---|---|
| `ModelStore` | Cluster | Administrator | Overrides cache watermarks and download limits on matched `Node`s. |
| `ModelStoreBinding` | Namespaced | Administrator | Grants a namespace a byte budget, pinning rights, and optional source RAM over named stores. |
| `ModelPrefetch` | Namespaced | Tenant | Warms an artifact onto target `Node`s, manages retention, and optionally prepares remote sources. |

![Model prefetch peer sync and external download flow](../../assets/diagrams/model-delivery-prefetch.svg)

<!--
flowchart TD
    PREFETCH["ModelPrefetch configured for selected nodes"]
    PEER_HIT["Peer already has model -> in-cluster TCP sync to all selected nodes"]
    PEER_MISS["No peer has model -> download from external hub + sync to selected nodes"]
    PREFETCH -> PEER_HIT
    PREFETCH -> PEER_MISS
-->

## Contents

- [Cache resources](#cache-resources)
- [Prefetch placement](#prefetch-placement)
- [Cache quota and retention](#cache-quota-and-retention)
- [Prefetch status](#prefetch-status)
- [Node-to-node sync](#node-to-node-sync)

## Cache resources

A `ModelStore` selects cache `Node`s via *`spec.nodeSelector`* and overrides cluster `Setting`s
field by field. `kubectl get modelstores` shows policy watermarks; check kubelet-capped watermarks on
[`NodeModelStore`](operations.md#node-cache-inspection).

A `ModelStoreBinding` grants store access, a monotonic *`quota.bytes`* budget, optional
*`allowPinned`*, and optional *`sourceMemory`* ([Source preparation](remote.md#source-preparation)):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelStore  # cluster-scoped
metadata:
  name: rtx6000-models
spec:
  nodeSelector:  # required; avoid overlapping selectors across ModelStores
    matchLabels:
      kubernetes.io/hostname: gpu-node
  # watermarks:  # optional overrides; omitted fields inherit cluster Settings
  #   highPercent: 80
  #   lowPercent: 70
  # download:
  #   concurrency: 8
  #   bytesPerSecond: 0  # 0 means unlimited on this pool
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStoreBinding  # namespaced
metadata:
  name: rtx6000-models
  namespace: gpustack-elastic
spec:
  storeRefs:  # required; 1..8 ModelStore names, immutable
    - name: rtx6000-models
  quota:
    bytes: 64Gi  # required; can grow, cannot shrink
  allowPinned: false  # optional, default: false; can turn true, cannot turn false
```

| Condition | Reason | Resolution |
|---|---|---|
| Multiple `ModelStore`s match one `Node` | `SelectorOverlap` | Make *`spec.nodeSelector`* sets disjoint across stores. |
| `ModelPrefetch` targets ungranted `Node`s | Target outside *`storeRefs`* | Restrict prefetch placement or create a Binding covering those stores. |
| Update shrinks *`quota.bytes`* or clears *`allowPinned`* | Monotonic grant rule | Keep or increase *`quota.bytes`* (recreate the Binding to reduce it). |

Deleting a `ModelPrefetch` deletes its warm-up `Pod`s, clears its pins on the next union pass, and
leaves cached bytes to ordinary watermark collection.

## Prefetch placement

*`spec.placement`* selects `Node`s via *`acceleratorTypes`* or *`nodeSelector`* (mutually exclusive).
Omitting both inherits accelerated pools from same-namespace `ModelDeployment`s referencing the
artifact (CPU-only roles excluded):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: qwen-prefetch
  namespace: gpustack-elastic
spec:
  artifactRef:  # required; ModelArtifact in this namespace
    name: qwen-7b
  bindingRef:  # required; ModelStoreBinding in this namespace
    name: rtx6000-models
  placement:  # set at most one of acceleratorTypes or nodeSelector
    acceleratorTypes:
      - gpu-pool
    # nodeSelector:
    #   matchLabels:
    #     accelerator: "true"
```

Explicit *`placement`* is required whenever *`spec.source`* is set for
[remote weight delivery](remote.md#source-preparation).

### Warm-up Pod

The prefetch controller creates one restricted warm-up `Pod` per target `Node` using the
`model-prefetch-warmup-image` `Setting` (requires `sh`, `find`, and `sha256sum`). Each `Pod` mounts
the artifact's CSI volume, verifies files, and exits once the `Node` publishes the digest.

Warm-up `Pod`s omit `kueue.x-k8s.io/queue-name` so Kueue and accelerator `Pod` webhooks never hold
cache warming behind compute quota.

## Cache quota and retention

A Binding's budget charges the full resolved byte size per distinct digest on every target `Node`
(shared digests across namespaces charge each namespace independently). On `ModelStoreBinding`:

- *`status.usedBytes`* reports measured usage (omitted while any referenced artifact is unresolved).
- `OverQuota` turns `True` when measured usage exceeds *`quota.bytes`* (`Unknown` while unresolved).

Admission rejects missing artifacts or Bindings, ungranted target `Node`s, and unauthorized pins. It
also rejects projected footprints (`resolved size × widest node count` per digest) above
*`quota.bytes`*.

### Pinning and expiry

*`spec.retention.pinned`* (requires `allowPinned: true`) protects digests from eviction while still
counting toward disk usage ([Cache collection rules](operations.md#cache-collection-rules)). The
prefetch controller is the sole writer of `NodeModelStore.spec.pinned`, merging `ModelTransfer`
source digests, active/rollback `ModelKernelArtifact` digests, and `ModelPrefetch` retention pins
under a per-node `256`-entry cap.

*`spec.retention.ttlAfterLastUse`* unpins a `Node`'s copy once unmounted past that duration
(evaluated at the 1-hour granularity of *`lastUsedTime`*).

## Prefetch status

`ModelPrefetch` reports *`desiredNodes`*, *`readyNodes`*, *`downloadingNodes`*, and conditions
`Progressing`, `Available`, `Degraded`, and `Lapsed`. `Available` checks *`readyNodes >= minReady`*
(`0` means all desired `Node`s); `Lapsed` indicates *`ttlAfterLastUse`* unpinned copies below
*`minReady`*.

With *`spec.source`* set, `SourceReady` (`SourceServing`) and *`status.transfer`* track active
host-memory source leases. Up to 16 generations display (`SourceSummaryTruncated=True` when capped),
while full observations drive readiness and *`reservedBytes`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: warm-model
  namespace: team-a
status:
  transfer:
    generations: []  # capped at 16 display entries
    serving:
      artifactUID: artifact-uid
      artifactGeneration: 1
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      readyLeaseExpiresAt: "2026-11-07T12:01:00Z"
  conditions:
    - type: SourceReady
      status: "True"
      observedGeneration: 2
      reason: SourceServing
    - type: SourceSummaryTruncated
      status: "False"
      observedGeneration: 2
      reason: CompleteSummary
```

## Node-to-node sync

With `modelManager.enabled: true`, plugins serve published cache trees to cold peers over TCP port
`32445`. One seed `Node` downloads from the hub, and cold `Node`s pull from peers even during hub
outages.

![Node-to-node peer sync over TCP](../../assets/diagrams/model-delivery-peer-sync.svg)

<!--
flowchart TD
    HUB["Upstream model hub (1x download)"]
    SEED["node-01: seed cache"]
    PEER2["node-02: peer cache over TCP :32445"]
    PEER3["node-03: peer cache over TCP :32445"]
    HUB -> SEED
    SEED -> PEER2
    SEED -> PEER3
    PEER2 -> PEER3
-->

```yaml
modelManager:
  port: 32445  # peer TCP port; 0 disables peer serving and pulling
  peerSync:
    maxServingStreams: 8  # concurrent outbound file streams per seed node
    streamsPerSource: 4  # concurrent inbound range streams per puller
```

The `model-store-peer-sync` `Setting` defaults to `true`; set `false` to pull from the hub without
restarting plugins. Standalone `model-manager` keeps peer sync off until port and peer auth flags are
set.

### Peer authentication and network

Pulling plugins send a projected `ServiceAccount` token (`gpustack-model-peer` audience). Serving
plugins verify it via `TokenReview`, caching positive answers until the earlier of 2 minutes and
token expiry, and never caching refusals. Peer clients skip TLS verification on the plugin's
self-signed certificate; use trusted `Pod` networks or set `modelManager.port: 0`.

The default `NetworkPolicy` limits plugin ingress to the worker and peer `model-manager` `Pod`s. On
CNIs where `hostNetwork` bypasses `podSelector` rules, `TokenReview` still blocks unauthorized
callers; add scrapers under `modelManager.networkPolicy.scrapers`.

### Checkpoint resume and metrics

Before pulling bytes, a cold `Node` verifies the peer's manifest against the artifact's
*`manifestDigest`* (skipping older manifestless trees). Downloads stream HTTP byte ranges from one
peer and checkpoint hashes every `64 MiB`, resuming on another peer or the hub if a seed fails.

- **Source tracking**: `NodeModelStore.status.models[].source` reports `Hub`, `Peer`, or `Forge`
  (for ingested kernel bundles), and plugin `:32444/metrics` exposes
  `gpustack_model_manager_download_bytes_total` split by the `source` label (`hub` or `peer`).
- **Trade-offs**: Peer sync cuts hub egress and hub dependency rather than fan-out time on slower
  east-west fabrics. Each concurrent puller uses `~0.25 vCPU` on 2-vCPU seed `Node`s.

---

**See also** — [Model Artifact](artifact.md) (artifact digests and `progress` subresource) · [Model Store Operations](operations.md) (settings, node status, and watermark collection) · [Remote Weight Delivery](remote.md) (host-memory sources prepared via `ModelPrefetch`)

**Next** → [Model Store Operations](operations.md) — configure node cache settings and inspect `NodeModelStore` state.
