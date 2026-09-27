# Model Prefetch Reference

> **Purpose** — the three objects that warm a model onto nodes before any Pod asks, the budgets and pins that bound them, and what runs on the node.
> **Audience** users, operators, contributors · **Prerequisites** [Model Artifact Reference](model-artifact.md) · **Read time** ~9 min

Warming is declared, not performed: a tenant names an artifact and a target set, and the operator
lands the weights through the node cache's ordinary delivery. Three objects divide the concerns —

| Object | Scope | Author | Carries |
|---|---|---|---|
| `ModelStore` | cluster | admin | one node pool's cache policy: watermarks and download limits that override only the fields they state |
| `ModelStoreBinding` | namespaced | admin | a namespace's byte budget and its right to pin, over named stores |
| `ModelPrefetch` | namespaced | tenant | one artifact's residency intent: a target set, how many nodes make it available, and its retention |

The node's effective configuration is still `NodeModelStore` — the store layer merges above the
cluster defaults under [Model Store Operations](../operation/model-store.md), and the plugin side of
every field is in the [Node Model Store Reference](node-model-store.md).

## Contents

- [The three objects](#the-three-objects)
- [Placement](#placement)
- [The warm-up pod](#the-warm-up-pod)
- [Budgets and admission](#budgets-and-admission)
- [Pinning and expiry](#pinning-and-expiry)
- [Status and views](#status-and-views)

## The three objects

A `ModelStore` selects its pool with `spec.nodeSelector` and states overrides field by field: a
field left out keeps the cluster default, and an explicit `bytesPerSecond: 0` is "unlimited on this
pool", not an omission. Two stores whose selectors match one node are a reported misconfiguration,
not a silent merge — [the pool layer](../operation/model-store.md#the-pool-layer) defines the
overlap condition and the tie-break.

A `ModelStoreBinding` is the provisioning point: creating one in a namespace is what grants that
namespace a `quota.bytes` budget and, with `allowPinned`, the right to pin. The grant is explicit —
there is no default pool a binding falls back to — and frozen: `storeRefs` cannot move, the budget
can only grow, and `allowPinned` only turns on.

> **Why** — a grant that shrank under consumption would retroactively make an admitted prefetch
> over budget, and a re-pointed grant would strand the accounting of everything admitted before the
> edit. Both rules are admission-time refusals, so the namespace sees the refusal at the moment of
> the edit, not as drift later.

A `ModelPrefetch` names an `artifactRef` and a `bindingRef` in its own namespace. Deleting it
revokes the intent: the warm-up pods go with their owner references, the pins leave on the next
union pass, and the bytes are reclaimed by the node cache's own collection once the content is
unreferenced and past grace.

## Placement

`spec.placement` picks the target set one of three ways, and admission and delivery share one
expansion (`PrefetchTargetNodes` in `pkg/worker/controllers/worker/model_prefetch.go`), so the two
can never disagree about what a prefetch would warm:

| Form | Field | Target set |
|---|---|---|
| by pool | `placement.instanceTypes` | every node carrying one of the named InstanceTypes' flavors |
| by node | `placement.nodeSelector` | every node the selector matches |
| derived | both absent | the InstanceTypes of the namespace's deployments that reference the artifact |

Setting both kinds is refused; the schema cannot see it because neither field is required.

## The warm-up pod

Delivery is one warm-up pod per target node, rendered by the prefetch controller. It mounts the
artifact's own CSI volume — the same volume a consumer mounts, with the same artifact, UID and
digest attributes — under a restricted pod security context, reads every file back, and exits. The
node cache downloads, verifies and publishes exactly once per node; the pod is the delivery
operation, and there is no separate job state machine.

A warm-up pod never carries a `kueue.x-k8s.io/queue-name` label.

A warm-up pod never carries a `kueue.x-k8s.io/queue-name` label: a labeled pod is held by Kueue's
admission and topology gates and never scheduled, and this operator's own pod webhook ignores
unlabeled pods. The label-free shape is what keeps warming outside every scheduler's way.

The image is a [Setting](../settings.md): the pod needs only a shell,
`find` and `sha256sum`, and the one thing the default cannot guarantee is that a given cluster's
registry has it.

## Budgets and admission

A grant's budget counts filesystem usage: each distinct digest at its full size, for every node
holding it, whatever the entry's state. A tree two namespaces share counts fully against each of
them — conservative, and impossible to game by warming what another tenant already warmed. The
counting runs in `model_store_binding.go` and lands on the binding's status:

- `status.usedBytes` — the measured figure, absent while any referenced artifact has not resolved
  (an absent figure was not measured; zero is a measurement).
- `OverQuota` — True when the footprint passed the grant, Unknown when it could not be counted.

Admission (`model_prefetch.go` in `pkg/worker/webhooks/worker`) refuses the shapes that cannot
work: a missing artifact or grant, a target set outside every store the grant names, pinning
without the grant's permission, and a projection past the budget.

The projection is the accounting at admission time — per distinct digest, the resolved size times
the widest node count asked of it. An artifact that has not resolved yet contributes no size, and
the accounting reports the drift that projection could not see.

## Pinning and expiry

`spec.retention.pinned` keeps the content on every target node against collection: a pinned digest
is never a collection candidate, though it still counts toward usage, so a cache full of pinned
content reports its saturation exactly as a cache full of references does (`gc.Collector.Pinned`).

Pinning requires the grant. The nodes' pin lists are written as one union by the prefetch
controller — the field's single writer — so two namespaces pinning one node keep both digests; how
a pinned digest behaves under collection is the [node model store
reference](node-model-store.md#references-restart-and-collection)'s fact.

`spec.retention.ttlAfterLastUse` unpins a node's copy once nothing mounted it for that long. It is
enforced at the hour granularity the node already reports in `lastUsedTime`.

> **Why** — retention does not need finer precision, and finer recording would multiply the node's
> status writes for a decision that cannot tell the difference.

## Status and views

The prefetch's status aggregates the target nodes' own reports — `desiredNodes`, `readyNodes`,
`downloadingNodes`, and `Progressing` / `Available` / `Degraded`, where `Available` means
`readyNodes` reached `minReady` (0 = all of them). The [v1 view](model-artifact-views.md) proxies
every verb and adds no subresource: the per-node facts already live on the nodes' reports, and the
table prints who warms what and how far.

---

**See also** — [Model Artifact Reference](model-artifact.md) (what a prefetch warms) · [Node Model Store Reference](node-model-store.md) (the node side of every field named here) · [Model Store Operations](../operation/model-store.md) (the pool layer's knobs in operation)

**Next** → [Node Model Store Reference](node-model-store.md) — the node object the plugin serves.
