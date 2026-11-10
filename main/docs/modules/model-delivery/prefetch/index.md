# Model Prefetch

Warming is declared, not performed: a tenant names an artifact and a target set, and the operator
lands the weights through the node cache's ordinary delivery. These resources divide the responsibilities:

| Object | Scope | Author | Carries |
|---|---|---|---|
| `ModelStore` | cluster | admin | one node pool's cache policy: watermarks and download limits that override only the fields they state |
| `ModelStoreBinding` | namespaced | admin | a namespace's byte budget and its right to pin, over named stores |
| `ModelPrefetch` | namespaced | tenant | one artifact's residency intent: a target set, how many nodes make it available, and its retention |

The node's effective configuration is still `NodeModelStore`: the store layer merges above the
cluster defaults under [Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md), and the plugin side of
every field is in the [Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md).

![Model prefetch peer sync and external download flow](/gpustack-operator/main/docs/assets/diagrams/model-delivery-prefetch.svg)

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
- [Placement](#placement)
- [Warm-up Pod](#warm-up-pod)
- [Budgets and admission](#budgets-and-admission)
- [Pinning and expiry](#pinning-and-expiry)
- [Status and views](#status-and-views)

## Cache resources

A `ModelStore` selects its pool with *`spec.nodeSelector`* and states watermarks and download
limits as overrides field by field; the [pool layer](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md#pool-layer)
defines the override semantics, the overlap condition and the tie-break.

`kubectl get modelstores` prints the pool's configured `High` and `Low` watermarks. A
pool that states *`spec.watermarks`* shows its own values; a pool that states none shows the cluster
`Setting`s' `model-store-high-watermark` and `model-store-low-watermark`, falling back to their
defaults when the `Setting`s carry no value.

The columns are the pool's policy, not what a node has already applied: a node's own kubelet
thresholds can cap the high watermark lower, which only that node's
[NodeModelStore](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md) shows. `Setting`s changes appear on the next `kubectl get`.
A `Setting`s-only change does not trigger an update in an existing `kubectl get --watch`.

A `ModelStoreBinding` is the provisioning point: creating one in a namespace is what grants that
namespace a *`quota.bytes`* budget and, with *`allowPinned`*, the right to pin. The grant is explicit
(there is no default pool a binding falls back to) and frozen: *`storeRefs`* cannot move, the budget
can only grow, and *`allowPinned`* only turns on.

Optional *`spec.sourceMemory`* caps source reservations shared by this Binding's prefetches
across nodes and manufacturers. Omit or remove it to use node limits. While present, bytes and
registrations can only grow; sessions can change in either direction.

The following store and binding use the same shape as the
[Model Prefetch Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md). Create the
`gpustack-elastic` namespace first. Replace the hostname with the target node's label.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelStore
metadata:
  name: rtx6000-models
spec:
  nodeSelector:  # Required; selects the cache nodes.
    matchLabels:
      kubernetes.io/hostname: gpu-node
  # Optional overrides; omitted fields use cluster settings.
  # watermarks:
  #   highPercent: 80
  #   lowPercent: 70
  # download:
  #   concurrency: 8
  #   bytesPerSecond: 0  # Zero means unlimited.
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStoreBinding
metadata:
  name: rtx6000-models
  namespace: gpustack-elastic
spec:
  storeRefs:  # Required; immutable store grant.
    - name: rtx6000-models
  quota:
    bytes: 64Gi  # Required; the budget may grow but cannot shrink.
```

- **Selector**: A store selects nodes. Avoid overlapping store selectors.
- **Overrides**: Omitted watermarks and download limits use cluster settings.
- **Store references**: Grant one to eight stores. The list is immutable.
- **Quota**: Set a positive byte budget. It can grow, but cannot shrink.
- **Pinning**: *`allowPinned`* defaults to false. It can be enabled, but not disabled later.

| Condition | Reason | Resolution |
|---|---|---|
| Stores select the same node | Their policies overlap | Change the selectors to avoid overlap. |
| A prefetch targets nodes outside the grant | No granted store covers those nodes | Adjust placement or create another binding. |
| A quota update reduces the budget | Admitted prefetches may depend on the grant | Keep or increase the budget. |

The [source-memory examples](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md#source-memory-examples) use `Qwen3-8B` and
`Qwen3.6-35B-A3B` to distinguish node capacity, per-model windows and a shared Binding grant.

> **Why** — a grant that shrank under consumption would retroactively make an admitted prefetch
> over budget, and a re-pointed grant would strand the accounting of everything admitted before the
> edit. Both rules are admission-time refusals, so the namespace sees the refusal at the moment of
> the edit, not as drift later.

A `ModelPrefetch` names an *`artifactRef`* and a *`bindingRef`* in its own namespace. Deleting it
revokes the intent: the warm-up `Pod`s go with their owner references, the pins leave on the next
union pass, and the bytes are reclaimed by the node cache's own collection once the content is
unreferenced and past grace.

## Placement

*`spec.placement`* picks the target set using the options below, and admission and delivery share one
expansion, so the two can never disagree about what a prefetch would warm:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: qwen-prefetch
spec:
  artifactRef:  # Required; the artifact is in this namespace.
    name: qwen-7b
  bindingRef:  # Required; the binding is in this namespace.
    name: store-binding
  placement:
    instanceTypes:  # Optional; every node carrying a named InstanceType's flavor.
      - gpu-pool
```

- **Artifact**: Reference a `ModelArtifact` in the same namespace.
- **Binding**: Reference a `ModelStoreBinding` that covers the target nodes.
- **Placement**: Set either *`instanceTypes`* or *`nodeSelector`*, or omit both.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: qwen-prefetch
spec:
  artifactRef:  # Required; the artifact is in this namespace.
    name: qwen-7b
  bindingRef:  # Required; the binding is in this namespace.
    name: store-binding
  placement:
    nodeSelector:  # Optional; every node matching this selector.
      matchLabels:
        accelerator: "true"
```

- **Node selector**: Select nodes with labels that are present in the cluster.
- **Default placement**: Omit both fields to derive types from deployments that reference the artifact.
- **Source preparation**: Explicit placement is required when *`source`* is set.

| Condition | Reason | Resolution |
|---|---|---|
| Both placement fields are set | The choices are mutually exclusive | Keep one placement field. |
| Source preparation has no explicit placement | Source memory needs a fixed target selection | Set one placement field. |

## Warm-up Pod

Delivery is one warm-up `Pod` per target node, rendered by the prefetch controller. It mounts the
artifact's own CSI volume (the same volume a consumer mounts, with the same artifact, UID and
digest attributes) under a restricted `Pod` security context, reads every file back, and exits. The
node cache downloads, verifies and publishes exactly once per node; the `Pod` is the delivery
operation, and there is no separate job state machine.

A warm-up `Pod` never carries a `kueue.x-k8s.io/queue-name` label: a labeled `Pod` is held by Kueue's
admission and topology gates and never scheduled, and this operator's own `Pod` webhook ignores
unlabeled `Pod`s.

The image is a [Setting](/gpustack-operator/main/docs/reference/settings/index.md): the `Pod` needs only a shell,
`find` and `sha256sum`, and the one thing the default cannot guarantee is that a given cluster's
registry has it.

## Budgets and admission

A grant's budget counts filesystem usage: each distinct digest at its full size, for every node
holding it, whatever the entry's state. A tree two namespaces share counts fully against each of
them; that is conservative, and impossible to game by warming what another tenant already warmed.
The measured footprint lands on the binding's status:

- *`status.usedBytes`*: the measured figure, absent while any referenced artifact has not resolved
  (an absent figure was not measured; zero is a measurement).
- `OverQuota`: True when the footprint passed the grant, Unknown when it could not be counted.

Admission refuses the shapes that cannot work: a missing artifact or grant, a target set outside
every store the grant names, pinning without the grant's permission, and a projection past the
budget.

The projection is the accounting at admission time: per distinct digest, the resolved size times
the widest node count asked of it. An artifact that has not resolved yet contributes no size, and
the accounting reports the drift that projection could not see.

## Pinning and expiry

*`spec.retention.pinned`* keeps the content on every target node against collection: a pinned digest
is never a collection candidate, though it still counts toward usage, so a cache full of pinned
content reports its saturation exactly as a cache full of references does.

Pinning requires the grant. The nodes' pin lists are written as one union by the prefetch
controller, the field's single writer, so two namespaces pinning one node keep both digests. See
[Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md#references-restart-collection) for how a pinned digest behaves
under collection.

*`spec.retention.ttlAfterLastUse`* unpins a node's copy once nothing mounted it for that long. It is
enforced at the hour granularity the node already reports in *`lastUsedTime`*.

> **Why** — retention does not need finer precision, and finer recording would multiply the node's
> status writes for a decision that cannot tell the difference.

## Status and views

The prefetch's status aggregates the target nodes' own reports: *`desiredNodes`*, *`readyNodes`*,
*`downloadingNodes`*, and `Progressing` / `Available` / `Degraded` / `Lapsed`, where `Available`
means *`readyNodes`* reached *`minReady`* (0 = all of them).

`Lapsed` turns True when a shortfall below that bar is the retention TTL's doing: the copies that
would close the gap lapsed past *`ttlAfterLastUse`*, were unpinned and are not re-warmed, and the
message says how many. It stays False while the shortfall has any other cause, so a lapse never
masks a warm-up still running or a delivery that failed.

A source prefetch also publishes `SourceReady` for its current revision. Its serving observation
binds the artifact UID, artifact revision and digest to an unexpired source lease. Missing or expired
proof provides no source readiness. The broker checks authorization live when a client connects.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: warm-model
  namespace: team-a
status:
  transfer:
    # Display summary: at most 16 generations, serving entries first, then by identity.
    generations: []
    # Readiness and reservedBytes use the full observation, even when the summary is truncated.
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
    # True means some generation entries were omitted from the display summary.
    - type: SourceSummaryTruncated
      status: "False"
      observedGeneration: 2
      reason: CompleteSummary
```

The [ModelPrefetch API](/gpustack-operator/main/docs/modules/model-delivery/artifact-api/index.md#resources) supports resource operations and adds no subresource: the per-node
facts already live on the nodes' reports, and the table prints who warms what and how far.

---

**See also** — [Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) (what a prefetch warms) · [Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md) (the node side of every field named here) · [Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) (the pool layer's settings in operation)

**Next** → [Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md) — the node object the plugin serves.
