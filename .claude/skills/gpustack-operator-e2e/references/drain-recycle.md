# Drain-recycle: why a real cluster, and how the cases work

Background for CASE 2 (Instance drain-recycle), CASE 3 (managed-toggle), and CASE 4
(accelerated chain). Read this before running or debugging those cases.

## CASE 2 — Instance ↔ InstanceType contract

`pkg/worker/controllers/worker/instance.go` and `pkg/worker/webhooks/worker/instance.go`
are not covered by CASE 1. Core behavior: when the `InstanceType` a *running* Instance
references is drained — its backing `ClusterQueue` goes `HoldAndDrain` so the InstanceType
reports `Inactive`, or the type is removed — the `InstanceReconciler` must **stop** the
Instance (`spec.stop=true`), *not* recreate its Pod; it may restart only once a live
InstanceType exists again.

Why a real cluster is required:

- `InstanceType` is a live projection of a `ClusterQueue` (`instance_type.go`); its
  `status.phase` comes from `apistatus.GetSummaryOfClusterQueue` — `Active` condition
  `True`→`Active`, `False`→`Inactive`.
- The Instance's Pod carries the Kueue `kueue.x-k8s.io/queue-name` label, so it is
  admission-managed: `HoldAndDrain` evicts the Pod → `pod==nil` → the reconciler
  re-evaluates and stops the Instance.
- **Unit-test blind spot:** the fake client cannot store the aggregated `InstanceType`, so
  `Get(InstanceType)` returns NotFound and the "Inactive" unit test silently degrades into
  the "type gone" path. The `phase==Inactive` branch is exercised **only** here, on a real
  cluster. A buggy `Phase != Inactive` condition would recreate the Pod instead of stopping
  the Instance — that is the regression CASE 2 exists to catch.

**Drain injection (CPU-only, no accelerator):** bump the `ram` capacity label in the
Worker-authored `<node>-gpustack-worker` NodeFeature. The node then matches a *new* general
profile and the *old* profile (the one the Instance's InstanceType is built from) drains.
This is stable: `ConstructNodeCapacityLabels` prefers the node's existing capacity label over
`Status.Capacity` (`pkg/nodefeature/helper.go`), and `NodeFeatureReconciler` watches Node-label
changes only (not the NodeFeature), so the edit is not reconciled away. The value must differ
and be an even Gi.

> A harmless `create service … spec.ports: Required value` error appears because the test
> Instance declares no ports; it is unrelated to the drain path.

## CASE 3 — managed-toggle is a *different trigger on a different code path*

Excluding a node from management (`gpustack.ai/managed=false`) must drain its single-node
ResourceFlavors with the **same** chain as CASE 2. What is non-obvious:

- A CASE 2 capacity reshape changes a *feature label*, so any feature-prefix predicate fires.
  **A managed toggle changes only `gpustack.ai/managed`** — no feature label — so it drains
  **only if** the `ResourceFlavorReconciler`/`CohortReconciler` Node-watch `UpdateFunc`
  predicates include `systemname.ManagedLabelKey` in their `mapx.EqualWithStringPrefix(...)`
  (`pkg/worker/controllers/worker/{resourceflavor,cohort}.go`). Missing it is the historical
  bug: the flavor is never enqueued or drained, while the ClusterQueue silently recomputes to
  a misleading `0/-1` (Active but negative-remaining) quota and the Instance keeps running.
- **Restart masks it.** The `For`-watch start-up resync re-reconciles every ResourceFlavor, so
  a freshly (re)started operator drains the orphan regardless of the predicate. Verify against
  a **continuously running** operator — do not restart between the toggle and the assertion.
- Toggle via the NodeFeature, not the node (NFD reverts a direct node label). The unit cases
  `unmanaged node drains flavor` / `unmanaged node deletes cohort` only guard the index filter,
  **not** the predicate — so this live check is the only guard for the enqueue path.

## CASE 4 — accelerated chain injection recipe (validated)

The Worker derives accelerated profiles from the `acceleratable.feature.gpustack.ai/*` labels
that NFD merges onto the node from the DM's `<node>-gpustack-device-manager` NodeFeature. CASE 4
simulates this by creating a NodeFeature carrying those labels and letting NFD merge it — no real
`Devices` CR or device-plugin allocation, so it validates the controller/label algebra, not
physical device handling.

**Minimal label set** (validated against `ConstructNodeCapacityLabels` in `pkg/nodefeature/helper.go`):

```
acceleratable.feature.gpustack.ai/<manu>-<id>        = "true"   # <manu> must be a known
                                                                # acceleratable manufacturer (e.g. nvidia)
acceleratable.feature.gpustack.ai/<manu>-<id>.count  = "1"      # > 0; this is what GATES derivation
```

`ExtractAcceleratableNodeKeys` keys off `<manu>-<id>=true` with a known manufacturer (the
top-level `feature.gpustack.ai/acceleratable` flag is *not* the gate). `.count > 0` is required;
`.cpu`/`.ram`/`.storage` fall back to the node's `Status.Capacity`, so the Worker derives
`.z-flavor=<cpu>c-<ram>g-<stg>g-<acc>d` (and `.z-queue`/`.z-cohort`). The `-<acc>d` segment is what
the accelerated chain names carry (e.g. `gpustack--generic-ln-a64-10c-24g-680g--nvidia-t4-1d`).

The NodeFeature must carry `metadata.labels: nfd.node.kubernetes.io/node-name: <node>` so NFD
merges it onto that node. `case-4.sh` injects exactly this set (with extra product/memory/cores
for realism, which are not consumed by the derivation) and removes it to drain.

Drain step: remove the injected NodeFeature so the profile no longer matches any node. The
ResourceFlavor is **not** deleted — it becomes a draining, zero-quota tombstone
(`schedule.gpustack.ai/drain=true`), which is the **durable** drain-recycle signal CASE 4 asserts.
The ClusterQueue's `HoldAndDrain` is only a transient step on its way to removal (it is reclaimed
once no reservation remains), so it is not a reliable post-drain assertion; the flavor tombstone is
the contract. The Cohort is reclaimed only once no node AND no ClusterQueue still reference it.

## Skill-specific troubleshooting

- **CASE 2 ram edit reverts / old profile never drains** — the patch must land on the
  `<node>-gpustack-worker` NodeFeature (Worker-authored), not on the Node directly (NFD
  overwrites Node labels). Confirm NFD merged it: `kubectl get node <node> -o json | grep
  '<gKey>.ram'` should show the new value. If a new profile never appears, the chosen value
  matched the old one (must differ and be even Gi).
- **CASE 2 Instance not stopped after drain** — check the Pod was actually admitted (held
  quota) before the drain; an unadmitted/finished Workload leaves nothing for `HoldAndDrain`
  to evict, so `pod==nil` may never recur. Keep the container alive (`sleep`). If the
  InstanceType went straight to *gone* rather than *Inactive*, the `phase==Inactive` branch
  was skipped — re-run and confirm `Inactive` is observed while the ClusterQueue is
  `HoldAndDrain`.
- **CASE 2 symptom of a stale image** — an orphaned `ResourceFlavor` getting *deleted* instead
  of marked `schedule.gpustack.ai/drain=true` (pre-drain-recycle behavior). Rebuild from HEAD.
