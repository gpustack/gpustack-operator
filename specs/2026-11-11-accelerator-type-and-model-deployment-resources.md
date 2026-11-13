# Spec: AcceleratorType and Explicit Host Resources for ModelDeployment

Status: Shipped
Type: Feature

- **Related Specs**: [2026-06-29-instancetype-unified-pool-refactor.md](2026-06-29-instancetype-unified-pool-refactor.md) · [2026-09-18-kvcache-and-model-deployment-consolidation.md](2026-09-18-kvcache-and-model-deployment-consolidation.md) · [2026-09-27-model-prefetch.md](2026-09-27-model-prefetch.md) · [2026-10-21-model-deployment-scaler.md](2026-10-21-model-deployment-scaler.md)

## Summary

Introduce a cluster-scoped `AcceleratorType` resource (`worker.gpustack.ai/v1alpha1` storage with `worker.gpustack.ai/v1` proxy) that represents an accelerated hardware pool (`gpustack--${aKey}-${os}-${arch}`) without workspace CPU, memory, or local-storage bundles. Migrate non-`Instance` workloads (`ModelDeployment`, `ModelPrefetch`, `ModelDeploymentScaler`, and `model-transfer` source intent resolution) from `InstanceType` to `AcceleratorType`, while keeping `acceleratorType` optional on `ModelDeployment` roles so CPU-only roles bypass Kueue and `gpustack--generic` queues and schedule directly via Kubernetes `kube-scheduler`. Add optional `cpu`, `ram`, and `localStorage` fields to `ModelDeploymentRoleResources` and render only explicitly configured host resources onto serving Pods. Preserve all existing `ClusterQueue`, `LocalQueue`, `Instance`, `InstanceType`, `InstanceTypeFlavor`, and `worker-gateway` contracts without recreating or renaming live queues.

## Motivation

`InstanceType` currently bundles two distinct concerns into one CRD ([`api/worker/v1alpha1/instance_type.go`](../api/worker/v1alpha1/instance_type.go)):
1. **Pool scheduling identity and capacity** (`acceleratorGroup`, `generalGroup`, `os`, `arch`, Kueue `ClusterQueue`, `status.entrance`, and four-view accelerator capacity).
2. **Workspace host resource presets** (`spec.unitResources.cpu`, `spec.unitResources.ram`, and `spec.localStorage`).

Coupling non-`Instance` serving workloads to `InstanceType` causes four problems:

1. **Domain mismatch on serving workloads**: `ModelDeployment` ([`api/worker/v1alpha1/model_deployment.go`](../api/worker/v1alpha1/model_deployment.go)) and `ModelPrefetch` ([`api/worker/v1alpha1/model_prefetch.go`](../api/worker/v1alpha1/model_prefetch.go)) target accelerator hardware pools (such as NVIDIA B300), not interactive SSH workspace tiers (`InstanceType`). Meanwhile, CPU-only `InstanceType`s (`gpustack--generic-${os}-${arch}`) exist solely to size CPU `Instance` workspaces.
2. **Opaque and rigid host resource sizing in `ModelDeployment`**: `ModelDeploymentRoleResources` forbids explicit `cpu`, `ram`, and `localStorage` fields today. Instead, [`deriveModelDeploymentResources`](../pkg/worker/controllers/worker/model_deployment_render.go) silently multiplies `InstanceType.spec.unitResources` at Pod render time and caps ephemeral storage at `15Gi`. Users cannot inspect host resource requests in the `ModelDeployment` spec, cannot raise host RAM for CPU-offloaded models without creating a new `InstanceType`, and cannot request more than `1 CPU / 2Gi RAM` on CPU-only deployments.
3. **Kueue quota double-counting across multiple `InstanceType` tiers**: [`buildResourceGroups`](../pkg/worker/controllers/worker/node_queue.go) meters only `credits.gpustack.ai/<manufacturer>` on accelerated `ClusterQueue`s. However, [`InstanceTypeReconciler`](../pkg/worker/controllers/worker/instance_type.go) enforces `ClusterQueue.Name == InstanceType.Name`. If an administrator defines two `InstanceType` tiers on the same B300 pool (for example, `32C/64Gi` and `64C/128Gi`), the operator creates two separate `ClusterQueue`s that each claim 100% of the pool's accelerator credits, oversubscribing Kueue admission by 200%.
4. **External compatibility lock on `Instance` APIs**: External control planes and `worker-gateway` actively consume `Instance`, `InstanceType`, and the read-only `InstanceTypeFlavor` catalog ([`api/worker/v1/instance_type_flavor.go`](../api/worker/v1/instance_type_flavor.go)). Meanwhile, `ModelDeployment` and `ModelPrefetch` carry no external compatibility lock and can adopt clean field names directly.

### Goals

- Introduce `AcceleratorType` in `worker.gpustack.ai/v1alpha1` and `worker.gpustack.ai/v1` for accelerated hardware pools only, derived automatically from node flavors with 1:1 naming alignment to existing accelerated `ClusterQueue`s (`gpustack--${aKey}-${os}-${arch}`, or `gpustack--${gKey}--${aKey}-${os}-${arch}` when CPU manufacturer awareness is enabled).
- Keep all existing operator-derived `ClusterQueue` and `LocalQueue` objects in place with zero rename, zero recreation, and zero workload eviction during upgrade.
- Route `AcceleratorType` and all `InstanceType` objects sharing the same `(acceleratable, generalGroup, acceleratorGroup, os, arch)` pool identity to the single canonical pool `ClusterQueue`, eliminating accelerator credit double-counting.
- Replace `ModelDeployment.spec.roles[].instanceType` with optional `acceleratorType`. When `acceleratorType` is omitted (CPU-only role), bypass Kueue and `gpustack--generic` queues completely and schedule the role directly via `kube-scheduler` onto managed nodes.
- Add optional `cpu`, `ram`, and `localStorage` fields to `ModelDeployment.spec.roles[].resources`. Render only explicitly specified host resource fields into the serving container across both accelerated and CPU-only roles.
- Replace `ModelPrefetch.spec.placement.instanceTypes` with `acceleratorTypes` and update [`pkg/modelstore/source_placement.go`](../pkg/modelstore/source_placement.go), [`pkg/modeltransfer/intents.go`](../pkg/modeltransfer/intents.go), `ModelStoreBinding`, and `ModelDeploymentScaler` hardware prior resolution accordingly.
- Preserve `Instance`, `InstanceType`, `InstanceTypeFlavor`, and `worker-gateway` public schemas and endpoints unchanged.

### Non-Goals

- Adding synthetic `CPU` or `NoneAccelerated` entries into `AcceleratorType`, or routing CPU-only `ModelDeployment` roles through `gpustack--generic-${os}-${arch}` Kueue queues.
- Renaming, migrating, or deleting existing operator-derived `ClusterQueue` or `LocalQueue` objects.
- Modifying `Instance`, `InstanceType`, `InstanceTypeFlavor`, or `worker-gateway` REST endpoints and wire schemas for existing `Instance` consumers.
- Injecting default `cpu`, `ram`, or `localStorage` requests into `ModelDeployment` Pods when the user leaves those fields unset.
- Adding host resource defaults to `ModelDeployment` Router or `routing-proxy` sidecar containers.

## Proposal

Separate hardware-pool scheduling from `Instance` sizing packages while keeping the underlying Kueue pool queues intact:

1. **Pool-level `AcceleratorType` CRD**: The worker derives one `AcceleratorType` per accelerated `(generalGroup, acceleratorGroup, os, arch)` pool. It exposes pool identity, `inactive` state, hardware `detail`, four-view accelerator capacity, and `status.entrance`, with no `unitResources` or `localStorage` fields.
2. **Shared canonical pool `ClusterQueue`**: Each hardware pool owns exactly one canonical `ClusterQueue` (`gpustack--${aKey}-${os}-${arch}` for accelerated pools; `gpustack--generic-${os}-${arch}` for CPU-only `Instance` pools). Both `AcceleratorType` and multiple `InstanceType`s (such as `B300 / 32C / 64Gi` and `B300 / 64C / 128Gi`) resolve `status.entrance` to that shared pool queue. Kueue meters accelerator credits once per physical pool, while `kube-scheduler` filters per-node CPU, memory, and ephemeral storage.
3. **Clean `ModelDeployment` and `ModelPrefetch` contracts**: `ModelDeployment` roles select an optional `acceleratorType` and declare optional `cpu`, `ram`, and `localStorage` in `spec.roles[].resources`. When `acceleratorType` is omitted, the role runs unqueued without Kueue labels (matching `ModelDeployment` Router and Elastic Head Pods) and schedules directly onto managed nodes. Unset host resource fields are omitted from rendered Pod specs rather than silently derived. `ModelPrefetch` selects `spec.placement.acceleratorTypes` and defaults from same-namespace `ModelDeployment` `acceleratorType` references.

### User Stories

#### Story 1
As an inference platform user, I want to reference `acceleratorType` and declare optional `cpu`, `ram`, and `localStorage` directly on `ModelDeployment` roles (or omit `acceleratorType` for direct CPU-only scheduling), so that model serving is decoupled from `InstanceType` workspace presets and never hits hidden host resource caps.

#### Story 2
As a cluster administrator, I want multiple `InstanceType` CPU/RAM packages and `ModelDeployment` workloads on the same GPU pool to share one canonical Kueue `ClusterQueue`, so that physical accelerator quota is accounted once without modifying existing `ClusterQueue`s.

#### Story 3
As an external platform or `worker-gateway` consumer, I want `Instance`, `InstanceType`, and `InstanceTypeFlavor` APIs to remain backward-compatible, so that existing workspace workflows continue working across upgrades without client changes.

### Core Features & Acceptance Criteria

1. **`AcceleratorType` API and Auto-Derivation**
   - `worker.gpustack.ai/v1alpha1` defines cluster-scoped CRD `AcceleratorType`, and `worker.gpustack.ai/v1` exposes it through `extensionapi.WithCurdProxy`.
   - `AcceleratorTypeSpec` carries `displayName`, `description`, `inactive`, `acceleratorGroup` (required), `generalGroup`, `os` (required), and `arch` (required). It contains no `acceleratable`, `unitResources`, or `localStorage` fields.
   - `AcceleratorTypeStatus` carries `conditions`, `detail` (accelerator hardware descriptor), `allocated` / `remaining` four-view accelerator capacity (`exclusive`, `shared`, `sliced`, `partitioned`), and `entrance` (`LocalQueue` name).
   - When node flavor reconciliation sees an accelerated flavor, it creates the derived `AcceleratorType` (`gpustack--${aKey}-${os}-${arch}`, or `gpustack--${gKey}--${aKey}-${os}-${arch}` when `instance-type-aware-cpu-manufacturer=true`) alongside the derived `InstanceType` (when `instance-type-derived-from-node=true`). Non-accelerated flavors never produce an `AcceleratorType`.

2. **In-Place Shared Pool `ClusterQueue` and `Inactive` Isolation**
   - Canonical pool `ClusterQueue` names (`gpustack--${aKey}-${os}-${arch}` and `gpustack--generic-${os}-${arch}`) and `LocalQueue` names (`gpustack-fnv64-<hash>`) remain unchanged. Existing `ClusterQueue` objects with `gpustack.ai/resource-type=instancetypes` continue to reconcile in place without recreation or drain.
   - Every `AcceleratorType` and every `InstanceType` (derived or admin-created) resolves its backing `ClusterQueue` and `status.entrance` from its canonical pool identity `(acceleratable, generalGroup, acceleratorGroup, os, arch)` rather than `metadata.name`. Creating a second `InstanceType` on the same pool does not create a second `ClusterQueue`.
   - Deleting a single `InstanceType` stops (`spec.stop=true`) any active `Instance`s referencing that `InstanceType` before releasing its finalizer, removes only that object, and deletes the shared pool `ClusterQueue` only when zero active (non-deleting) `AcceleratorType` and `InstanceType` objects remain for that pool identity. Deleting an `AcceleratorType` likewise deletes the shared pool `ClusterQueue` only when zero active fronting objects remain on that pool.
   - Admission enforces `InstanceType.spec.inactive=true` in [`InstanceWebhook`](../pkg/worker/webhooks/worker/instance.go) (blocking new `Instance` creation and stopped-to-running start) and `AcceleratorType.spec.inactive=true` in [`ModelDeploymentWebhook`](../pkg/worker/webhooks/worker/model_deployment.go) (blocking new roles or role switches targeting that `AcceleratorType`) without halting active sibling types on the same pool. The shared pool `ClusterQueue` sets `StopPolicy: Hold` only when every fronting `AcceleratorType` and `InstanceType` for that pool is `inactive=true` (preserving single-`InstanceType` CPU-pool `Hold` semantics).
   - [`PodWebhook`](../pkg/worker/webhooks/worker/pod.go) and [`WorkloadWebhook`](../pkg/worker/webhooks/worker/workload.go) resolve per-card VRAM, partition profiles, and `acceleratorGroup` from the pool (`AcceleratorType` or canonical pool `ClusterQueue` schedule labels) instead of failing when multiple `InstanceType`s share one `LocalQueue`.

3. **`ModelDeployment` Optional `acceleratorType` and Explicit Host Resources**
   - `ModelDeploymentRole.instanceType` is replaced by optional `acceleratorType` (`json:"acceleratorType,omitempty"`, protobuf tag `21`).
   - If `acceleratorType` is set:
     - `ModelDeploymentWebhook` defaults `resources.accelerator` to `1` when no accelerator request is set, validates `acceleratorType` existence, `inactive=false`, slicing/partition support, per-node card ceilings, and Ascend/NIXL/HIXL compatibility against `AcceleratorType`.
     - Pod rendering sets `kueue.x-k8s.io/queue-name` to `AcceleratorType.status.entrance` and renders Kueue PodGroup metadata.
   - If `acceleratorType` is empty (CPU-only role):
     - `ModelDeploymentWebhook` rejects any accelerator resource claim (`accelerator`, slicing percentages, or partition profile) and requires no mandatory host resource fields.
     - Pod rendering omits `kueue.x-k8s.io/queue-name` and Kueue PodGroup labels (`kueue.x-k8s.io/pod-group-name`, `pod-group-serving`, `role-hash`, `pod-group-required-topology`), retains `ModelDeployment`'s own replica ordinal, member index, and replica-size metadata, and selects managed nodes (`gpustack.ai/managed=true`, plus `feature.gpustack.ai/cpu-only=true` when `instance-type-mixed-on-node=false`) with operator tolerations so `kube-scheduler` places the Pod directly.
     - Rollout slot occupancy ([`model_deployment.go`](../pkg/worker/controllers/worker/model_deployment.go), [`model_deployment_rollout.go`](../pkg/worker/controllers/worker/model_deployment_rollout.go)), replacement admission ([`model_deployment_replacement_slot.go`](../pkg/worker/controllers/worker/model_deployment_replacement_slot.go)), joint admission ([`model_deployment_joint_admission.go`](../pkg/worker/controllers/worker/model_deployment_joint_admission.go)), and quota status observation ([`model_deployment_status.go`](../pkg/worker/controllers/worker/model_deployment_status.go)) treat unqueued replicas (`acceleratorType == ""`) as non-Kueue-gated and scheduled once all member Pods carry `spec.nodeName != ""`. When all roles omit `acceleratorType`, `QuotaReserved` reports `True` (`Reason: NotApplicable`).
   - `ModelDeploymentRoleResources` adds optional `cpu` (tag `6`), `ram` (tag `7`), and `localStorage` (tag `8`) as `*resource.Quantity`.
   - During Pod rendering, only non-nil `cpu`, `ram`, and `localStorage` values are written to the main container's `Limits` and `Requests` (applying `kuberequest.ScaleToOvercommit` to `Requests` when `instance-general-resources-overcommit=true`). Unset fields are not rendered into the container `ResourceRequirements` at all.

4. **`ModelPrefetch`, `ModelStoreBinding`, `model-transfer`, Chart, and `ModelDeploymentScaler` Alignment**
   - `ModelPrefetchPlacement.instanceTypes` is replaced by `acceleratorTypes` (`json:"acceleratorTypes,omitempty"`, protobuf tag `3`).
   - When `ModelPrefetch.spec.placement` is omitted, target pools derive from same-namespace `ModelDeployment` roles that set a non-empty `acceleratorType` for the same `ModelArtifact`. CPU-only roles (`acceleratorType == ""`) contribute no accelerator pool; prefetching onto CPU-only nodes uses `spec.placement.nodeSelector`.
   - [`pkg/modeltransfer/intents.go`](../pkg/modeltransfer/intents.go) and [`pkg/modeltransfer/live_intent.go`](../pkg/modeltransfer/live_intent.go) resolve source placement pools from `placement.acceleratorTypes`, and [`deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml`](../deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml) grants read access on `acceleratortypes`.
   - [`deploy/gpustack-operator/chart/files/drain.sh`](../deploy/gpustack-operator/chart/files/drain.sh) drains `acceleratortypes.worker.gpustack.ai` with `schedule.gpustack.ai/derived-from-node!=true` so Helm uninstall never stalls on derived `AcceleratorType` recreation.
   - `ModelDeploymentScaler` hardware prior lookup ([`pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`](../pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go)) reads `AcceleratorType.status.detail.product` when `role.acceleratorType` is set, and skips accelerator priors cleanly when `role.acceleratorType` is empty. Replica rebalance in [`pkg/worker/controllers/worker/model_deployment_scaler_rebalance.go`](../pkg/worker/controllers/worker/model_deployment_scaler_rebalance.go) requires matching `role.acceleratorType`.

5. **External `Instance` Ecosystem Compatibility**
   - `Instance`, `InstanceType`, and `InstanceTypeFlavor` schemas in `worker.gpustack.ai/v1` and `v1alpha1`, plus all `worker-gateway` types and HTTP routes (`/instancetypeflavors`, `/instancetypes`, `/instances`), remain unchanged.

### Notes / Constraints / Caveats

- **Overturning the prior role-resource comment**: [`api/worker/v1alpha1/model_deployment.go`](../api/worker/v1alpha1/model_deployment.go) previously forbade CPU, memory, and ephemeral storage fields on `ModelDeploymentRoleResources` because those values derived from `InstanceType.spec.unitResources`. Decoupling `ModelDeployment` from `InstanceType` explicitly overturns that restriction.
- **Protobuf wire tag hygiene**: Assign fresh protobuf field numbers (`21` for `ModelDeploymentRole.AcceleratorType`, `6`–`8` for `ModelDeploymentRoleResources.{CPU,RAM,LocalStorage}`, and `3` for `ModelPrefetchPlacement.AcceleratorTypes`) rather than reusing retired tags `3` and `1`. Also keep Go mechanics vocabulary (`pointer`, `struct`, `nil-able`, `compile-time`, `import cycle`) out of API doc comments enforced by [`hack/check/api-descriptions.sh`](../hack/check/api-descriptions.sh).
- **Pod QoS, Namespace Quotas, and Render Digests**: When a user omits `cpu` and `ram` on a `ModelDeployment` role, the rendered Pod omits CPU and memory requests/limits (`BestEffort` QoS on CPU-only roles). If a target namespace configures a Kubernetes `ResourceQuota` covering CPU or memory without a `LimitRange` default, Pod creation fails until the user supplies `cpu` and `ram` on the role. Omitting implicit host resources also moves the default rendered Pod digest, so pinned render-digest unit tests must be re-baselined intentionally.
- **Unqueued CPU-only `ModelDeployment` scheduling**: CPU-only `ClusterQueue`s (`gpustack--generic-${os}-${arch}`) exist only for `Instance` workspace admission. Omitting `acceleratorType` on a `ModelDeployment` role skips Kueue altogether rather than guessing an `${os}-${arch}` generic queue, avoiding both OS/Arch ambiguity and artificial `resources.cpu` requirements.
- **Code generation**: Run `make lint` before `make generate` from a checkout path ending in `gpustack.ai/gpustack` (or via a `$HOME/.gpustack-gen/gpustack.ai/gpustack` detached worktree when working in a child worktree) so formatted sources and generated deepcopy, CRD, OpenAPI, protobuf, and webhook artifacts match CI's `api.yml` gate.

### Boundaries

- **Always:**
  - Preserve existing canonical `ClusterQueue` and `LocalQueue` names, specs, and labels in place on upgrade.
  - Keep `Instance`, `InstanceType`, `InstanceTypeFlavor`, and `worker-gateway` APIs backward-compatible.
  - Bypass Kueue (`kueue.x-k8s.io/queue-name` and Kueue PodGroup labels omitted) when `ModelDeployment.spec.roles[].acceleratorType` is empty.
  - Run `make lint`, `make generate`, `bash hack/check/api-descriptions.sh`, `make lint chart`, `make lint agents-shell`, `make lint docs`, and unit tests after code, API, E2E/benchmark script, and documentation edits.
- **Ask first:**
  - Any change to `Instance`, `InstanceType`, or `InstanceTypeFlavor` wire fields or `worker-gateway` HTTP payloads.
  - Adding new settings or environment variables in `pkg/worker/settings/`.
  - Changing Kueue `ClusterQueue` label keys, `LocalQueue` FNV-64a hashing, or `credits.gpustack.ai/*` transformation rules.
- **Never:**
  - Create synthetic `CPU` or `NoneAccelerated` objects inside `AcceleratorType` or route `ModelDeployment` roles into `gpustack--generic-*` queues.
  - Inject implicit default `cpu`, `ram`, or `localStorage` into `ModelDeployment` Pods when omitted by the user.
  - Create duplicate `ClusterQueue`s for multiple `InstanceType`s backed by the same hardware pool.
  - Hand-edit generated `zz_generated.*`, `generated.pb.go`, or `generated.proto` files.

### Risks and Mitigations

- **Multiple `InstanceType`s sharing one `LocalQueue` break `PodWebhook.frontingInstanceType`** → Resolve per-card VRAM and partition profiles from `AcceleratorType` (or deduplicated pool detail via the `LocalQueue` -> `ClusterQueue` schedule labels) instead of requiring a single `InstanceType` per `LocalQueue`.
- **Deactivating `InstanceType` or `AcceleratorType` halts active sibling workloads on the shared `ClusterQueue`** → Enforce `InstanceType.spec.inactive` in `InstanceWebhook` and `AcceleratorType.spec.inactive` in `ModelDeploymentWebhook`, and set `StopPolicy: Hold` on the shared `ClusterQueue` only when every fronting `AcceleratorType` and `InstanceType` on that pool is `inactive=true`.
- **Deleting one `InstanceType` tears down the shared pool `ClusterQueue` or skips stopping bound `Instance`s** → Delete the canonical pool `ClusterQueue` only when zero active (non-deleting) `AcceleratorType` and `InstanceType` objects remain for that pool identity, and stop (`spec.stop=true`) active `Instance`s referencing a deleting `InstanceType` (including handling `DeleteFunc` on `InstanceReconciler`'s `InstanceType` watch) before releasing the `InstanceType` finalizer so retiring a derived `InstanceType` under `instance-type-derived-from-node=false` deterministically evicts its running `Instance`s while keeping `AcceleratorType` active.
- **Helm pre-delete `drain.sh` stalls waiting on recreated derived `AcceleratorType` objects, or `model-transfer` loses pool read RBAC** → Add `acceleratortypes.worker.gpustack.ai` with `schedule.gpustack.ai/derived-from-node!=true` to [`deploy/gpustack-operator/chart/files/drain.sh`](../deploy/gpustack-operator/chart/files/drain.sh) and grant `acceleratortypes` (`get`, `list`, `watch`) in [`deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml`](../deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml).
- **Unqueued CPU-only `ModelDeployment` role stalls rollout or joint-admission checks that expect a `kueue.Workload`** → Treat roles with `acceleratorType == ""` as non-Kueue-gated across rollout occupancy, replacement-slot admission (`spec.nodeName != ""`), joint-admission barrier checks, and `QuotaReserved` status observation.
- **Shared memory (`/dev/shm`, default `16Gi`) exceeds explicit low `ram` on `ModelDeployment`** → Document `/dev/shm` memory cgroup accounting clearly in [`docs/modules/model-deployment/configuration.md`](../docs/modules/model-deployment/configuration.md) and validate explicit `shmSize > ram` mismatches in `ModelDeploymentWebhook`.
- **Existing E2E, benchmark, and documentation manifests reference removed `instanceType` / `instanceTypes` fields or 1:1 `ClusterQueue` names** → Audit and align every `ModelDeployment` (`acceleratorType`), `ModelPrefetch` (`acceleratorTypes`), and shared-pool `ClusterQueue` lookup across `.agents/skills/_e2e-lib/`, `.agents/skills/gpustack-operator-e2e/`, `.agents/skills/gpustack-operator-benchmark/`, `README.md`, and `docs/`.

## Design Details

### Commands

**Environment**:
- **Build, code generation, lint, chart, docs, and unit tests**: Local macOS checkout ending in `gpustack.ai/gpustack` (`.sbin` toolbox pre-installed). If building in a parallel git worktree whose path does not end in `gpustack.ai/gpustack`, run `make generate` via a detached worktree at `$HOME/.gpustack-gen/gpustack.ai/gpustack` per `gpustack-operator-generate`.
- **End-to-end cluster verification (`gpustack-operator-e2e` / `gpustack-operator-chart-e2e`)**: Remote Kubernetes cluster (access credentials / `kubeconfig` supplied at E2E execution time; upgrade baseline pinned to `v0.10.0`).

```bash
# Format and lint Go and shell sources first (make lint rewrites imports/formatting)
make lint

# Regenerate deepcopy, register, CRDs, apiservice, OpenAPI, protobuf, and webhook manifests
make generate

# Verify generated OpenAPI and CRD descriptions contain no Go mechanics leaks
bash hack/check/api-descriptions.sh

# Lint and unit-test Helm chart templates and RBAC/drain scripts
make lint chart
make test chart

# Lint .agents E2E and benchmark shell scripts and run benchmark Python unit tests
make lint agents-shell
python3 -m unittest discover -s .agents/skills/gpustack-operator-benchmark/scripts -p 'test_*.py'

# Lint documentation, Hugo site build, and spec cross-references
make lint docs

# Run targeted unit tests across affected packages
RACE=false go test ./pkg/nodefeature/... ./pkg/modelstore/... ./pkg/modeltransfer/... ./pkg/worker/extensionapis/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... ./pkg/workergateway/...
```

### Project Structure

```text
api/worker/
  v1/
    accelerator_type.go             # Public v1 AcceleratorType type alias and APIService registration
    instance_type.go                # Unchanged public v1 InstanceType
    instance_type_flavor.go         # Unchanged read-only InstanceTypeFlavor catalog
  v1alpha1/
    accelerator_type.go             # Storage CRD schema for AcceleratorType
    model_deployment.go             # Role acceleratorType + optional cpu/ram/localStorage in Resources
    model_prefetch.go               # Placement acceleratorTypes field
pkg/nodefeature/
  helper.go                         # Canonical pool identity and ClusterQueue/LocalQueue naming helpers
pkg/modelstore/
  source_placement.go               # Pool flavor and target node resolution over AcceleratorType
pkg/modeltransfer/
  intents.go                        # Candidate and live source placement resolution via acceleratorTypes
  live_intent.go                    # Session source intent verification via acceleratorTypes
pkg/worker/
  extensionapis/
    setup.go                        # Register AcceleratorTypeHandler on v1.worker.gpustack.ai
    worker/
      accelerator_type.go           # CRUD proxy handler and kubectl table columns for v1.AcceleratorType
  webhooks/
    setup.go                        # Register AcceleratorTypeWebhook
    worker/
      accelerator_type.go           # Defaulting and validation webhook for AcceleratorType
      instance_type.go              # Pool-shared queue-entrance label alignment
      instance.go                   # Enforce InstanceType.spec.inactive on Instance admission
      model_deployment*.go          # Validate optional acceleratorType and explicit host resources
      model_prefetch.go             # Validate placement.acceleratorTypes
      pod.go                        # Pool-level VRAM and partition lookup without 1:1 InstanceType assertion
      workload.go                   # Read acceleratorGroup from canonical pool ClusterQueue / AcceleratorType
  controllers/
    setup.go                        # Register AcceleratorTypeReconciler
    worker/
      accelerator_type.go           # Reconcile AcceleratorType status, entrance, and shared pool ClusterQueue
      node_flavor.go                # Derive AcceleratorType for accelerated pools alongside InstanceType
      instance_type.go              # Resolve shared pool ClusterQueue and coordinate multi-fronting Inactive/teardown
      instance.go                   # Watch pool ClusterQueue drain and InstanceType deletion to stop active Instances
      model_deployment.go           # Fetch AcceleratorType or run unqueued CPU role reconciliation
      model_deployment_render.go    # Render optional cpu/ram/localStorage only when set; omit queue on CPU roles
      model_deployment_pod_group.go # Omit Kueue PodGroup metadata on unqueued CPU roles
      model_deployment_rollout.go   # Count scheduled unqueued CPU replicas without Workload lookup
      model_deployment_replacement_slot.go # Admit unqueued CPU replacement slots on node assignment
      model_deployment_joint_admission.go  # Skip unqueued CPU roles in Kueue Workload reservation barrier
      model_deployment_status.go    # Report unqueued CPU role readiness and QuotaReserved cleanly
      model_deployment_scaler_*.go  # Resolve hardware priors and rebalance constraints via acceleratorType
      model_prefetch.go             # Resolve target nodes from acceleratorTypes
      model_store_binding.go        # Account prefetch bytes via acceleratorTypes
deploy/gpustack-operator/chart/
  ci/test-model-transfer.sh         # Assert acceleratortypes RBAC on model-transfer ClusterRole
  files/drain.sh                    # Drain acceleratortypes with derived-from-node!=true wait selector
  templates/model-transfer/serviceaccount.yaml # Grant get/list/watch on acceleratortypes
.agents/skills/
  _e2e-lib/scripts/                 # Align serving-lib.sh vendor/pool readers with acceleratorType
  gpustack-operator-e2e/            # Align ModelDeployment/ModelPrefetch cases and shared-queue assertions
  gpustack-operator-benchmark/      # Align benchmark manifests, case scripts, and qualification helpers
README.md                           # Quickstart ModelDeployment manifest using acceleratorType
docs/
  README.md                         # Shared documentation index and module map updates
  getting-started/architecture.md   # Scheduling chain overview update
  modules/devices/                  # Scheduling chain and admission documentation updates
  modules/model-delivery/           # ModelPrefetch acceleratorTypes configuration updates
  modules/model-deployment/         # Role acceleratorType, unqueued CPU roles, host resources, and benchmarks
  reference/settings.md             # Clarify instance-type-derived-from-node vs derived AcceleratorType
  walkthroughs/                     # End-to-end walkthrough YAML updates from instanceType to acceleratorType
```

### Code Style

Follow repository Go and Kubernetes controller conventions (`snake_case` multi-word filenames, explicit error handling, short plain comments without decorative symbols or Go-mechanics leaks in API docs, and table-driven fake-client tests). Reuse canonical pool naming from [`pkg/nodefeature/helper.go`](../pkg/nodefeature/helper.go):

```go
// CanonicalPoolQueueName returns the canonical ClusterQueue name for a hardware pool identity.
func CanonicalPoolQueueName(
	acceleratable, aware bool, generalGroup, acceleratorGroup, osName, arch string,
) string {
	if acceleratable {
		if aware && generalGroup != "" && generalGroup != GeneralGroupGeneric {
			return fmt.Sprintf("gpustack--%s--%s-%s-%s", generalGroup, acceleratorGroup, osName, arch)
		}
		return fmt.Sprintf("gpustack--%s-%s-%s", acceleratorGroup, osName, arch)
	}
	if aware && generalGroup != "" {
		return fmt.Sprintf("gpustack--%s-%s-%s", generalGroup, osName, arch)
	}
	return fmt.Sprintf("gpustack--%s-%s-%s", GeneralGroupGeneric, osName, arch)
}
```

### Implementation Plan

Because removing `ModelDeploymentRole.InstanceType` and `ModelPrefetchPlacement.InstanceTypes` touches call sites across five packages, the plan uses **prefactor + expand -> parallel vertical migrations -> contract**: `T1` and `T2` expand the helpers and schemas without breaking existing callers, unlocking `T3`, `T5`, `T6`, and `T7` in parallel with disjoint `Owns:`, followed by `T4` (pool webhooks) and `T8` (schema contraction), and finally `T9` (E2E and benchmark suites) in parallel with `T10` (documentation).

- [x] **T1 · Prefactor canonical pool queue identity helpers in `pkg/nodefeature`**
      Blocked by: None
      Owns: `pkg/nodefeature/helper.go`, `pkg/nodefeature/helper_test.go`
      Gate: review
      Acceptance: Add `CanonicalPoolQueueName`, `(NodeFlavor).DerivedAcceleratorTypeIdentity` (returning empty values when `!f.Acceleratable`), and canonical pool schedule-label helpers in `pkg/nodefeature` so `AcceleratorType`, `InstanceType`, `NodeFlavor`, `NodeQueue`, and webhooks compute identical pool `ClusterQueue` (`gpustack--${aKey}-${os}-${arch}` / `gpustack--${gKey}--${aKey}-${os}-${arch}` / `gpustack--generic-${os}-${arch}`) and FNV-64a `LocalQueue` names from pool identity rather than `metadata.name`; table-driven unit tests cover accelerated, CPU-only, and CPU-manufacturer-aware combinations.
      Verify: `RACE=false go test ./pkg/nodefeature/... && make lint`

- [x] **T2 · Expand API schemas with `AcceleratorType`, transitional role/prefetch fields, and `v1` extension-API proxy**
      Blocked by: None
      Owns: `api/worker/v1/accelerator_type.go`, `api/worker/v1alpha1/accelerator_type.go`, `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_prefetch.go`, `api/worker/v1/zz_generated.*.go`, `api/worker/v1alpha1/zz_generated.*.go`, `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/**`, `pkg/worker/extensionapis/setup.go`, `pkg/worker/extensionapis/worker/accelerator_type*.go`
      Gate: review
      Acceptance: Add cluster-scoped `AcceleratorType` (`AcceleratorTypeSpec` with `displayName`, `description`, `inactive`, required `acceleratorGroup`, `generalGroup`, required `os`, required `arch`; `AcceleratorTypeStatus` with `conditions`, `detail`, four-view accelerator capacity, and `entrance`) in `v1alpha1` and `v1`; expand `ModelDeploymentRole` with optional `AcceleratorType` (protobuf tag `21`), expand `ModelDeploymentRoleResources` with optional `CPU` (tag `6`), `RAM` (tag `7`), and `LocalStorage` (tag `8`), and expand `ModelPrefetchPlacement` with optional `AcceleratorTypes` (tag `3`), while temporarily relaxing `InstanceType` / `InstanceTypes` to optional so existing `pkg/` callers continue compiling cleanly; register `AcceleratorTypeHandler` (`WithCurdProxy`, serving delete and kubectl table columns) in `pkg/worker/extensionapis/setup.go`; `Instance`, `InstanceType`, `InstanceTypeFlavor`, and `worker-gateway` remain untouched; `make lint`, `make generate`, `bash hack/check/api-descriptions.sh`, and `TestEveryCRDViewServesDelete` pass.
      Verify: `make lint && make generate && bash hack/check/api-descriptions.sh && RACE=false go test ./pkg/worker/extensionapis/...`

- [x] **T3 · Reconcile `AcceleratorType`, derive from `NodeFlavor`, and share canonical pool `ClusterQueue`s across `InstanceType`s**
      Blocked by: T1, T2
      Owns: `pkg/worker/controllers/setup.go`, `pkg/worker/controllers/worker/accelerator_type*.go`, `pkg/worker/controllers/worker/node_flavor*.go`, `pkg/worker/controllers/worker/instance_type*.go`, `pkg/worker/controllers/worker/instance.go`, `pkg/worker/controllers/worker/instance_test.go`, `pkg/worker/controllers/worker/node_queue*.go`
      Gate: review
      Acceptance: Add `AcceleratorTypeReconciler` (registered in `pkg/worker/controllers/setup.go`) to reconcile canonical accelerated pool `ClusterQueue`s (preserving existing `resource.gpustack.ai/type=instancetypes` labels and live queues in place, automatically deleting a standalone derived `AcceleratorType` when its bound `ClusterQueue` is deleted while recreating the queue for admin-authored types or shared pools), four-view accelerator capacity, hardware `detail`, and `status.entrance`, and export `AcceleratorTypeCardVRAMMib` and pool-queue lookup helpers; update `NodeFlavorReconciler` to always derive `AcceleratorType` for accelerated flavors alongside derived `InstanceType`s (which remain gated on `instance-type-derived-from-node=true`); update `InstanceTypeReconciler` and `InstanceReconciler` so every `InstanceType` resolves its backing `ClusterQueue` and `status.entrance` from canonical pool identity rather than `metadata.name`, deleting an `InstanceType` stops (`spec.stop=true`) all active `Instance`s referencing that `InstanceType` (with `DeleteFunc: true` on `InstanceReconciler`'s `InstanceType` watch) before unlocking its finalizer and deletes the canonical `ClusterQueue` only when zero active fronting `AcceleratorType` and `InstanceType` objects remain on that pool, and `StopPolicy: Hold` is set on the shared `ClusterQueue` only when every fronting object on that pool is `inactive=true`.
      Verify: `RACE=false go test ./pkg/worker/controllers/worker/... -run 'TestAcceleratorType|TestInstanceType|TestNodeFlavor|TestNodeQueue|TestInstance' && make lint`

- [x] **T4 · Add `AcceleratorTypeWebhook` and decouple `Instance`, `InstanceType`, `Pod`, and `Workload` webhooks from 1:1 queue assumptions**
      Blocked by: T3
      Owns: `pkg/worker/webhooks/setup.go`, `pkg/worker/webhooks/worker/accelerator_type*.go`, `pkg/worker/webhooks/worker/instance_type*.go`, `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/webhooks/worker/instance_test.go`, `pkg/worker/webhooks/worker/pod.go`, `pkg/worker/webhooks/worker/pod_test.go`, `pkg/worker/webhooks/worker/workload*.go`, `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
      Gate: review
      Acceptance: Register `AcceleratorTypeWebhook` in `pkg/worker/webhooks/setup.go` (defaulting schedule and `queue-entrance` labels from canonical pool identity; validating required `acceleratorGroup`/`os`/`arch` and immutability of pool identity fields); update `InstanceTypeWebhook` to stamp `queue-entrance` from canonical pool identity; enforce `InstanceType.spec.inactive=true` refusal on new `Instance` creation and stopped-to-running start in `InstanceWebhook`; update `PodWebhook` (`cardVRAMMib`, `partitionContainerUnits`, multi-card shared count pin) and `WorkloadWebhook` (`acceleratorGroup`) to resolve pool VRAM, partition inventory, and `acceleratorGroup` via `AcceleratorType` / canonical pool `ClusterQueue` without failing when multiple `InstanceType`s share one `LocalQueue`; regenerate `zz_generated.webhooks.go`.
      Verify: `make lint && make generate && RACE=false go test ./pkg/worker/webhooks/worker/... -run 'TestAcceleratorType|TestInstanceType|TestInstance|TestPod|TestWorkload'`

- [x] **T5 · Migrate `ModelPrefetch`, `pkg/modelstore`, `ModelStoreBinding`, `pkg/modeltransfer`, and Helm chart RBAC/drain to `AcceleratorType`**
      Blocked by: T1, T2
      Owns: `pkg/modelstore/**`, `pkg/modeltransfer/**`, `pkg/worker/controllers/worker/model_prefetch*.go`, `pkg/worker/controllers/worker/model_store_binding*.go`, `pkg/worker/webhooks/worker/model_prefetch*.go`, `deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml`, `deploy/gpustack-operator/chart/files/drain.sh`, `deploy/gpustack-operator/chart/ci/test-model-transfer.sh`
      Gate: review
      Acceptance: Switch `pkg/modelstore/source_placement.go`, `pkg/modeltransfer/{intents.go,live_intent.go}`, `ModelPrefetchReconciler`, `ModelStoreBindingReconciler`, and `ModelPrefetchWebhook` from `InstanceType` / `placement.instanceTypes` to `AcceleratorType` / `placement.acceleratorTypes`; default omitted `ModelPrefetch.spec.placement` from same-namespace `ModelDeployment` roles carrying non-empty `acceleratorType` (ignoring CPU-only roles); grant `acceleratortypes` (`get`, `list`, `watch`) in `deploy/gpustack-operator/chart/templates/model-transfer/serviceaccount.yaml` and assert it in `deploy/gpustack-operator/chart/ci/test-model-transfer.sh`; add `acceleratortypes.worker.gpustack.ai` with `schedule.gpustack.ai/derived-from-node!=true` to `deploy/gpustack-operator/chart/files/drain.sh`.
      Verify: `RACE=false go test ./pkg/modelstore/... ./pkg/modeltransfer/... && RACE=false go test ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestModelPrefetch|TestModelStoreBinding' && make lint && make lint chart && make test chart`

- [x] **T6 · Migrate `ModelDeployment` controllers, Pod rendering, unqueued CPU-only roles, and `ModelDeploymentScaler` to `AcceleratorType` and explicit host resources**
      Blocked by: T1, T2
      Owns: `pkg/worker/controllers/worker/model_deployment*.go`, `pkg/worker/controllers/worker/model_artifact_placement_test.go`
      Gate: review
      Acceptance: Switch `ModelDeploymentReconciler`, Pod rendering, compile-cache / fabric / connector / router / transfer helpers, and `ModelDeploymentScaler` (`model_deployment_scaler_prior_identity.go`, `model_deployment_scaler_rebalance.go`) from `InstanceType` to `AcceleratorType`; render only explicitly non-nil `resources.cpu`, `resources.ram`, and `resources.localStorage` onto the serving container (with `ScaleToOvercommit` on `Requests` when `instance-general-resources-overcommit=true`) and re-baseline pinned Pod render-digest tests intentionally; when `role.acceleratorType` is empty, omit `kueue.x-k8s.io/queue-name` and Kueue PodGroup metadata while retaining `ModelDeployment` ordinal/member/size metadata, stamp `gpustack.ai/managed=true` (plus `feature.gpustack.ai/cpu-only=true` when `instance-type-mixed-on-node=false`) and operator tolerations, and treat unqueued replicas as non-Kueue-gated across rollout occupancy, replacement-slot admission (`spec.nodeName != ""`), joint-admission barrier checks, and `QuotaReserved` status (`True` / `NotApplicable` when all roles omit `acceleratorType`).
      Verify: `RACE=false go test ./pkg/worker/controllers/worker/... -run 'TestModelDeployment|TestRenderModelDeployment|TestScaler' && make lint`

- [x] **T7 · Migrate `ModelDeploymentWebhook` to validate optional `acceleratorType` and explicit host resources**
      Blocked by: T1, T2
      Owns: `pkg/worker/webhooks/worker/model_deployment*.go`
      Gate: review
      Acceptance: Update `ModelDeploymentWebhook` (`model_deployment.go`, `model_deployment_epd.go`, `model_deployment_elastic.go`) to validate against `AcceleratorType` instead of `InstanceType`: when `acceleratorType` is set, default `resources.accelerator` to `1` when unset and validate `AcceleratorType` existence, `inactive=false`, slicing/partition capability, card ceilings, Elastic EP freeze rules, and EPD/NIXL/HIXL manufacturer rules; when `acceleratorType` is empty, reject any accelerator request (`accelerator`, slicing percentages, partition profile) and reject Elastic EP (which requires whole GPUs); validate optional positive `resources.cpu`, `resources.ram`, and `resources.localStorage` quantities (including `shmSize <= ram` when both are explicitly set).
      Verify: `RACE=false go test ./pkg/worker/webhooks/worker/... -run 'TestModelDeployment|TestValidateModelDeployment' && make lint`

- [x] **T8 · Contract legacy `InstanceType` / `InstanceTypes` fields on `ModelDeployment` and `ModelPrefetch` and regenerate schemas**
      Blocked by: T5, T6, T7
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_prefetch.go`, `api/worker/v1/zz_generated.*.go`, `api/worker/v1alpha1/zz_generated.*.go`, `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/**`, `pkg/modelstore/source_placement.go`, `pkg/worker/controllers/worker/model_prefetch_transfer_test.go`, `pkg/worker/webhooks/worker/model_deployment*_test.go`
      Gate: review
      Acceptance: Delete transitional `ModelDeploymentRole.InstanceType` and `ModelPrefetchPlacement.InstanceTypes` fields without renumbering remaining protobuf tags; regenerate CRD, OpenAPI, applyconfiguration, and protobuf artifacts; verify zero remaining `ModelDeployment` / `ModelPrefetch` `InstanceType` references across Go packages and pass `make lint`, `make generate`, `bash hack/check/api-descriptions.sh`, and the full unit test suite across all modified packages.
      Verify: `make lint && make generate && bash hack/check/api-descriptions.sh && RACE=false go test ./pkg/nodefeature/... ./pkg/modelstore/... ./pkg/modeltransfer/... ./pkg/worker/... ./pkg/workergateway/...`

- [x] **T9 · Align E2E cases, Benchmark cases, shared shell libraries, and benchmark manifests with `AcceleratorType`**
      Blocked by: T4, T8
      Owns: `.agents/skills/_e2e-lib/**`, `.agents/skills/gpustack-operator-e2e/**`, `.agents/skills/gpustack-operator-chart-e2e/**`, `.agents/skills/gpustack-operator-benchmark/**`
      Acceptance: Audit and migrate every `ModelDeployment` (`spec.roles[].instanceType` -> `spec.roles[].acceleratorType`, optional `resources.{cpu,ram,localStorage}`) and `ModelPrefetch` (`spec.placement.instanceTypes` -> `spec.placement.acceleratorTypes`) API field reference across `_e2e-lib/scripts/serving-lib.sh`, `gpustack-operator-e2e/cases/{_md-lib.sh,_epd-lib.sh,_topology-tas-lib.sh,case-*.sh}`, `gpustack-operator-e2e/SKILL.md`, and `gpustack-operator-benchmark/{SKILL.md,cases/*.sh,manifests/*.yaml,scripts/*.py}`; align `case-16.sh` and `case-50.sh` admin-authored `InstanceType` queue lookups with canonical pool `ClusterQueue` resolution (`status.entrance` -> `LocalQueue.spec.clusterQueue`) while keeping `Instance` E2E cases (`case-1`, `case-2`, `case-8`, `case-9`, `case-11`–`case-42`) on `InstanceType`; pass `make lint agents-shell` and benchmark Python unit tests.
      Verify: `make lint agents-shell && python3 -m unittest discover -s .agents/skills/gpustack-operator-benchmark/scripts -p 'test_*.py'`

- [x] **T10 · Update operator documentation, `README.md`, module map, and walkthrough/benchmark manifests**
      Blocked by: T4, T8
      Owns: `README.md`, `docs/**`, `.agents/skills/gpustack-operator-docs/**`, `.agents/skills/gpustack-operator-overview/SKILL.md`
      Acceptance: Audit and update every API field reference and Pretty YAML manifest across `README.md`, `docs/getting-started/architecture.md`, `docs/modules/devices/{_index,scheduling,admission,discovery,requests,nvidia-mig,thead-mig}.md`, `docs/modules/model-deployment/*.md` (including `*-benchmark.md`), `docs/modules/model-delivery/{artifact,prefetch,remote}.md`, `docs/reference/settings.md` (clarifying `instance-type-derived-from-node` vs operator-derived `AcceleratorType`), `docs/walkthroughs/**`, `docs/README.md` (`#module-map` and `Specs`), `.agents/skills/gpustack-operator-overview/SKILL.md`, and `gpustack-operator-docs` routing references; `make lint docs` exits 0 across both `T9` and `T10` Markdown changes.
      Verify: `bash .agents/skills/gpustack-operator-docs/scripts/check-specs.sh && make lint docs`

### Test Plan
[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None. Existing fake-client controller, webhook, `modelstore`, `modeltransfer`, Helm chart, and E2E/benchmark shell harnesses already support table-driven tests and in-cluster verification without base framework changes.

#### Unit tests
- `gpustack.ai/gpustack/pkg/nodefeature`: `2026-11-11` - `85%+` (covers `CanonicalPoolQueueName`, `DerivedAcceleratorTypeIdentity`, and pool schedule-label derivation across accelerated, CPU-only, and CPU-manufacturer-aware settings).
- `gpustack.ai/gpustack/pkg/worker/extensionapis`: `2026-11-11` - `80%+` (covers `AcceleratorTypeHandler` CRUD proxy registration, `TestEveryCRDViewServesDelete`, and kubectl table column rendering).
- `gpustack.ai/gpustack/pkg/worker/controllers/worker`: `2026-11-11` - `80%+` (covers `AcceleratorTypeReconciler` status/entrance/queue sync, `NodeFlavorReconciler` accelerated-only `AcceleratorType` derivation independent of `instance-type-derived-from-node`, multi-`InstanceType` shared `ClusterQueue` quota conservation, `Instance` eviction on `InstanceType` deletion while shared `ClusterQueue` stays active, coordinated `Inactive` -> `StopPolicy: Hold`, explicit optional `cpu`/`ram`/`localStorage` Pod rendering, unqueued CPU-only `ModelDeployment` rollout/replacement/joint-admission/status, `ModelDeploymentScaler` `AcceleratorType` prior lookup, and `ModelPrefetch` / `ModelStoreBinding` `acceleratorTypes` resolution).
- `gpustack.ai/gpustack/pkg/worker/webhooks/worker`: `2026-11-11` - `85%+` (covers `AcceleratorTypeWebhook` defaulting/immutability, `InstanceWebhook` refusal on `InstanceType.spec.inactive=true`, `PodWebhook` and `WorkloadWebhook` VRAM/partition/`acceleratorGroup` lookup when >1 `InstanceType` fronts one `LocalQueue`, `ModelDeploymentWebhook` optional `acceleratorType` and explicit host resource validation, and `ModelPrefetchWebhook` `acceleratorTypes` validation).
- `gpustack.ai/gpustack/pkg/modelstore` and `gpustack.ai/gpustack/pkg/modeltransfer`: `2026-11-11` - `85%+` (covers `ResolvePlacementAcceleratorTypeFlavorNames`, deployment-derived placement skipping CPU-only roles, and source broker intent matching).
- `deploy/gpustack-operator/chart` and `.agents/skills`: `2026-11-11` - `100%` of modified templates and shell/Python helpers (`make test chart`, `make lint chart`, `make lint agents-shell`, and `gpustack-operator-benchmark/scripts/test_*.py`).

#### Integration tests
- Fake-client multi-controller convergence tests in `pkg/worker/controllers/worker/`:
  1. `NodeFlavorReconciler` + `NodeQueueReconciler` + `AcceleratorTypeReconciler` + `InstanceTypeReconciler` converging one derived `AcceleratorType` and two custom `InstanceType` tiers (`32C/64Gi` and `64C/128Gi`) onto a single `gpustack--${aKey}-${os}-${arch}` `ClusterQueue` with 1x accelerator credit quota, zero recreation of pre-existing `ClusterQueue`s, and deterministic `Instance` stop (`spec.stop=true`) when one `InstanceType` is deleted while `AcceleratorType` keeps the shared `ClusterQueue` active.
  2. Multi-role `ModelDeployment` mixing an accelerated role (`acceleratorType` set, queued via Kueue) and a CPU-only role (`acceleratorType` omitted, unqueued) completing joint admission, rolling replacement, and `Ready` / `QuotaReserved` convergence.

#### e2e tests
- Execute on the remote Kubernetes cluster via `gpustack-operator-chart-e2e` and `gpustack-operator-e2e` once cluster credentials are provided:
  1. **In-place upgrade from `v0.10.0` baseline**: Install published `v0.10.0` (`gpustack/gpustack-operator:v0.10.0` chart and images) with a running `Instance` on the derived `InstanceType`, then `helm upgrade` in place to the target build (`CRDs` + `worker` + `device-manager` + `model-transfer`). Verify existing `ClusterQueue`s (`gpustack--*`, UID unchanged) and `LocalQueue`s stay admitted with zero eviction of the running `Instance`, and derived `AcceleratorType` CRs materialize automatically on accelerated pools.
  2. **Post-upgrade `Instance` CRUD regression on `InstanceType`**: Run `gpustack-operator-e2e` `Instance` cases (`case-1.sh`, `case-2.sh` create/status/drain-stop/delete, and `case-9.sh` stop -> sibling `InstanceType` edit/resize -> start -> delete, plus `case-15.sh` on GPU nodes) to verify `Instance` create, update, stop/start, and delete over `InstanceType` remain intact after the upgrade.
  3. **Derived-to-authored `InstanceType` switch, workload eviction, `InstanceTypeFlavor` authoring, and `AcceleratorType` `ModelDeployment`**:
     - With an `Instance` running on the derived `InstanceType`, switch `instance-type-derived-from-node=false` in `gpustack-settings` and delete the derived `InstanceType`; verify the running `Instance` is promptly evicted/stopped (`spec.stop=true`, backing Pod terminated, `status.phase=Stopped`) and the derived `InstanceType` is not re-created, while the accelerated pool's derived `AcceleratorType` and shared `ClusterQueue` remain `Active`.
     - Read pool identity from `InstanceTypeFlavor`, create a new administrator-authored `InstanceType` (custom `unitResources.cpu`, `unitResources.ram`, `localStorage`), verify it binds to the canonical pool `ClusterQueue` (`status.entrance`), and create a new `Instance` on it through `Ready`.
     - Deploy a `ModelDeployment` referencing the new `AcceleratorType` (`spec.roles[].acceleratorType`) with explicit and omitted `spec.roles[].resources.{cpu,ram,localStorage}` (plus a CPU-only `ModelDeployment` omitting `acceleratorType`), verifying both the authored-`InstanceType` `Instance` and the `AcceleratorType`-backed `ModelDeployment` reach `Ready` on the shared pool `ClusterQueue` while CPU-only `ModelDeployment` roles schedule directly without touching `gpustack--generic-*`.

## Alternatives

1. **Separate `ClusterQueue` per `InstanceType` plus a new `ClusterQueue` for `AcceleratorType`**
   - *Rejected*: Kueue `ResourceFlavor` holds no quota of its own; quota lives on each `ClusterQueue`. Giving every `InstanceType` and `AcceleratorType` its own `ClusterQueue` over the same `ResourceFlavor` duplicates 100% of the pool's `credits.gpustack.ai/<manufacturer>` quota per queue (and Kueue `cohortName` sums nominal quotas rather than deduplicating them), causing severe over-admission and TAS domain collisions.

2. **Add a `CPU` or `NoneAccelerated` entry into `AcceleratorType`, or route CPU-only `ModelDeployment` roles into `gpustack--generic-${os}-${arch}`**
   - *Rejected*: `gpustack--generic-${os}-${arch}` queues exist only for `Instance` CPU workspace admission. Routing CPU-only `ModelDeployment` roles into `gpustack--generic` introduces `${os}-${arch}` queue selection ambiguity and forces mandatory `resources.cpu` validation just to satisfy Kueue CPU quota accounting. Letting omitted `acceleratorType` bypass Kueue and schedule directly via `kube-scheduler` matches `ModelDeployment` Router and Elastic Head Pods cleanly.

3. **Name the new resource `DeviceType` (`deviceType`)**
   - *Rejected*: In the repository's hardware vocabulary ([`docs/modules/devices/discovery.md`](../docs/modules/devices/discovery.md)), `Device` encompasses both compute accelerators and RDMA network interfaces (`Devices`, `DevicesRDMASelectionPolicy`), while `Accelerator` specifically denotes compute units (GPU/NPU/TPU/XPU).

4. **OS/Arch-free `AcceleratorType` (`gpustack--nvidia-b300`, matching `InstanceTypeFlavor`)**
   - *Rejected*: Kueue `ClusterQueue` and `LocalQueue` are scoped per `(pool, os, arch)`. An OS/Arch-free `AcceleratorType` maps 1:N to queues on heterogeneous `amd64` + `arm64` clusters, preventing `AcceleratorType.status.entrance` from publishing a single `LocalQueue` name without adding `os` and `arch` fields back onto `ModelDeploymentRole`.

5. **Silently derive default `cpu`, `ram`, and `localStorage` from `unit_resources_preset.yaml` into `ModelDeployment` Pods**
   - *Rejected*: Implicit preset limits cap host memory and ephemeral storage behind the user's back (for example, `16Gi` fallback RAM colliding with `16Gi` `/dev/shm` or CPU-offloaded weights). Exposing optional `cpu`, `ram`, and `localStorage` on `ModelDeploymentRoleResources` and rendering only what the user specifies keeps Pod resource requests explicit and predictable.

## Open Questions

None. All architectural, naming, queue-sharing, compatibility, and host-resource injection decisions are resolved.
