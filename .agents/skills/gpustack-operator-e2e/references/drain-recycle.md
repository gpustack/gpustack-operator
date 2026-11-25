# Case rationale: why a real cluster, and how CPU-only Mock Device Manager drives accelerated cases

Background for CASES 2–5 (`specs/2026-06-29-instancetype-unified-pool-refactor.md` and
`specs/2026-11-24-cpu-mock-device-manager-and-e2e-consolidation.md`). Read this before running or
debugging those cases.

Every `F…` label below — F3a, F3b, F3d, F5d — is a feature row of the unified-pool refactor spec,
spelled out once here rather than left as an unanchored shorthand.

Post-refactor shape the cases assume: unit specs are **Queue/`InstanceType`-managed** (not on the
node); all modes fold into **one isolated `ClusterQueue` per pool** (**Cohort removed** — there is
no borrow topology and **no `schedule.gpustack.ai/drain` tombstone** anymore); `InstanceType` is a
**real CRD** whose `.status` four-view is materialized by `InstanceTypeReconciler`; teardown flows
through the `InstanceType` **finalizer** (`HoldAndDrain` the backing `ClusterQueue`, then delete it).

## CPU-only Mock Device Manager recipe (CASES 3–5, plus CASE 7 accelerated legs)

Earlier iterations of the suite hand-crafted a fake accelerator `NodeFeature`, patched `Devices.status`
through `devices.v1alpha1.worker.gpustack.ai --subresource=status`, and created synthetic charging
Pods by `spec.nodeName` — bypassing `device-manager` detection, monitoring, unhealthy retention, and
kubelet `DevicePlugin` allocation completely.

Cases 3–5 replace that approximation with the real opt-in **CPU-only Mock `Detector` + Mock
`Allocator`** inside `gpustack-operator device-manager` (`deviceManager.mock.enabled=true` in the
Helm chart, mounting a declarative `ConfigMap` via `--mock-devices-file` /
`GPUSTACK_MOCK_DEVICES_FILE`):

1. **Real `Detector` loop (`pkg/devicemanager/detector/mock`)** — resolves built-in multi-vendor
   hardware presets (`nvidia-h100`, `nvidia-h100-mig`, `amd-mi300x`, etc.) and per-node overrides
   (`default` + `nodes.<nodeName>`), stamps `device.gpustack.ai/mock: "true"` on `Devices`, publishes
   `<node>-gpustack-device-manager` `NodeFeature`s for NFD to merge onto `Node.Labels`, and drives
   the exact per-slot unhealthy-retention cache as production hardware.
2. **Real `Allocator` & `pkg/deviceplugin.ResourceServer` (`pkg/devicemanager/allocator/mock`)** —
   registers kubelet device plugins for `exclusive`, `shared`, `sliced`, `partitioned`, and
   `visibility` (plus optional mock `rdma`), validates allocated devices through
   `deviceplugin.AllocatedAccelerators`, maintains an in-memory virtual partition ledger with
   `deviceplugin.RunReclaimLoop`, and injects `GPUSTACK_MOCK_ALLOCATED_DEVICES`,
   `GPUSTACK_MOCK_ALLOCATION_MODE`, `GPUSTACK_MOCK_MANUFACTURER`, and vendor `*_VISIBLE_DEVICES`
   with **zero** `/dev/*` mounts and **zero** `ld.so.preload` shims.
3. **Hot-reload control plane (`cases/_mock-dm-lib.sh`)** — `mock_dm_detect_env` locates the running
   mock `DaemonSet` (`gpustack.ai/manufacturer=mock`) and its `ConfigMap`; `mock_dm_snapshot_cm` and
   `mock_dm_restore_cm` preserve and restore exact `ConfigMap` bytes inside `trap … EXIT`; and
   `mock_dm_apply_spec` updates `devices.yaml` and nudges kubelet volume sync without restarting
   `device-manager` Pods.

> **Note on `devices` status subresource writes.** If debugging or manually inspecting `Devices`,
> remember that unversioned `devices` resolves to the read-only `worker.gpustack.ai/v1` aggregated
> API; direct status writes use `devices.v1alpha1.worker.gpustack.ai --subresource=status` (the same
> typed `v1alpha1` client `device-manager` uses in production).

Card tokens in `Devices.status`: `mode` `0`=free, `1`=exclusive, `2`=shared, `3`=logically sliced,
`4`=physically partitioned; `remaining` in credit units out of `D = 1,600,000` per whole card
(`50%` logically sliced ⇒ `remaining = 50 × D/100`).

## CASE 2 — CPU `Instance` admits on its queue, drain stops it, and `managed` scopes onboarding

`pkg/worker/controllers/worker/{instance,node_flavor,node_queue,instance_type}.go` +
`pkg/worker/webhooks/worker/instance.go` + `pkg/worker/kuberess/apps_kueue.go`, not covered by
CASE 1:

1. **Admission.** A general `Instance`'s `Workload` also requests `memory`/`ephemeral-storage` that
   the CPU-only `ClusterQueue` does not cover (it covers only `cpu`, F3b). Kueue refuses to assign a
   flavor for an uncovered resource unless the deployed configuration sets
   `quotaCheckStrategy: IgnoreUndeclared` (F3d). CASE 2 creates a portless CPU `Instance` (asserting
   zero `Service` created) and verifies its `Workload` reaches `Admitted=True`.
2. **Drain removes the flavor without deleting the `InstanceType`.** Toggling
   `gpustack.ai/managed=false` on the `<node>-gpustack-worker` `NodeFeature` drops the node from the
   flavor index, deleting the general `ResourceFlavor` (F3a). `NodeQueueReconciler` puts the backing
   `ClusterQueue` on `HoldAndDrain`, empties its resource groups, and keeps the derived
   `InstanceType` alive while held; restoring `gpustack.ai/managed=true` refills the queue and
   reactivates the `InstanceType`. Verify against a **continuously running** operator (a restart's
   resync would mask a missing `ManagedLabelKey` watch predicate).
3. **The running `Instance` is stopped, not recreated.** As the `ClusterQueue` enters `HoldAndDrain`
   and Kueue evicts the admitted Pod, `instance.go` observes `StopPolicy=HoldAndDrain` before
   recreating the Pod, sets `spec.stop=true`, deletes the Pod, and marks the `Instance` `Stopped`.
4. **Catalog, CPU awareness, pinning, volumes, and host-access gates.** Phases 2 and 3 of CASE 2
   additionally verify `InstanceTypeFlavor` projection, declarative `ClusterQueue` recreation and
   delete-then-wait finalizer ordering, `instance-type-aware-cpu-manufacturer` split/collapse,
   `spec.nodeName` Kueue-compatible pinning, `additionalVolumes`, and
   `instance-privileged-allowed` / `instance-host-path-volume-allowed` gates.

## CASE 3 — Pod slice-% webhook (Phase A) + Mock full scheduling, `AdmissionCheck`, cross-mode & fit labels (Phase B)

- **Phase A (any CPU cluster, no Mock DM required):**
  - Asserts `gpustack-node-devices` `AdmissionCheck` is `Active=True`.
  - Pure GPU-less check of the `pods` CREATE webhook (`gpustack-worker-mutation` sorting ahead of
    `kueue-mutating-webhook-configuration` so our fold runs before Kueue hashes container resources):
    `.sliced.memory-percentage: 20` mutates to `.sliced.units = 320000` (`×16000`) with
    `.sliced.cores-percentage = 100`, while a memoryless `.sliced` Pod is rejected.
  - Verifies `InstanceType` `unitResources` / `localStorage` immutability after CREATE.
- **Phase B (CPU + Mock DM):**
  - **B1 (multi-vendor chain & four-view watch):** verifies `device.gpustack.ai/mock="true"`,
    multi-vendor (`nvidia` + `amd`) discovery, real CPU `alpine` Pods allocating `.exclusive`,
    `.shared`, and `.sliced` with mock diagnostic env vars and zero `/dev/*` or `ld.so.preload`
    mounts, and native `kubectl get instancetype -w` four-view freshness.
  - **B2 (`AdmissionCheck` over-admit hold & release):** 4 × 60% sliced Pods occupy all 4 cards
    while leaving 1.6 cards of coarse Kueue credits free; a 1-card exclusive Pod passes Gate 1
    credits, is held in `Retry` by Gate 3 `gpustack-node-devices`, and admits to `Running` as soon as
    one card frees.
  - **B3 (cross-mode exclusion):** proves `exclusive`, `shared`, and `sliced` claims never co-locate
    on the same physical card, steering to free cards when available and holding when full.
  - **B4 (fit-label fragmentation skip):** verifies `sliced-max-free-units.fit.gpustack.ai/*`
    tracks live per-card capacity and steers past fragmented nodes when `workload-fit-affinity=true`
    (while `false` livelocks on the fragmented node).
  - **B5 (accelerated CPU awareness):** toggles `instance-type-aware-cpu-manufacturer=true` on a
    mock accelerated pool and runs 1-card and 2-card `Instance`s reserving distinct mock card UUIDs.

## CASES 4 & 5 — Dynamic Mock fault injection, logical-slice packing, and virtual MIG

- **CASE 4** hot-reloads the Mock DM `ConfigMap` through six transitions with zero Pod restarts:
  healthy 4-card baseline → single-card `unhealthy: true` (scheduler avoids card 1) → single-card
  `queryState: unqueryable` (lost card retained in place at index 2 with `unhealthy: true` and
  sibling `index` values `0, 1, 3` preserved) → `totalFailure` (`lastDetected` preserved) → recovery
  back to 4 healthy cards → clean shrink to `count: 2` (pruning removed slots cleanly).
- **CASE 5** exercises multi-card logical-slice spread/packing and single-card co-residency +
  over-budget refusal (`Phase A`), followed by virtual MIG (`nvidia-h100-mig`) mixed logical+MIG
  coexistence without `UnexpectedAdmissionError`, live-ledger per-profile capacity, concurrent
  multi-profile carving (`3g.40gb` + `2g.20gb` + `1g.10gb`), `.visibility` sidecar co-allocation,
  and dead-Pod virtual partition reclamation (`Phase B`).

## Skill-specific troubleshooting

- **CASE 2 pool `ResourceFlavor` never drains** — patch `gpustack.ai/managed=false` on the
  `<node>-gpustack-worker` `NodeFeature`, not the `Node` directly (NFD owns and overwrites the node
  label). Confirm NFD merged it (`kubectl get node <n> -o jsonpath='{.metadata.labels.gpustack\.ai/managed}'`).
- **CASE 2 `Instance` not stopped after drain** — confirm the backing `ClusterQueue` moved to
  `StopPolicy: HoldAndDrain` and check worker logs for
  `stop instance as its instance type is gone, deleting, or draining`.
- **CASES 3–5 Phase B auto-skipped** — `mock_dm_detect_env` found no Ready `DaemonSet` with label
  `gpustack.ai/manufacturer=mock` or no mounted `ConfigMap`; verify `deviceManager.mock.enabled=true`
  and `deviceManager.mock.omitRuntimeClass=true` on CPU-only clusters.
- **Mock Pod stuck `ContainerCreating` with `RuntimeClass` error on Kind** — CPU Kind nodes have no
  `nvidia` containerd runtime handler; either set `deviceManager.mock.omitRuntimeClass=true` /
  `global.manufacturers.nvidia.runtimeName=""` or create a `runc`-backed `RuntimeClass`.
- **Mock `ConfigMap` hot-reload slow to reflect in `Devices`** — `_mock-dm-lib.sh` calls
  `mock_dm_touch_pods` to trigger immediate kubelet volume projection sync; if `Devices` still does
  not update within 90s, inspect `kubectl -n <ns> logs ds/gpustack-operator-device-manager-mock` for
  schema validation errors (malformed reloads intentionally retain the last valid spec).
