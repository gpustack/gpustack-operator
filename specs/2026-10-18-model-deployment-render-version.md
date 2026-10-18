# Spec: ModelDeployment Explicit Spec Hash and Implicit Render Versioning

Status: Building
Type: Feature

## Summary

Decouple `ModelDeployment` Pod rollout checks from operator-internal dynamic Pod rendering changes ([gpustack/gpustack-operator#760](https://github.com/gpustack/gpustack-operator/issues/760)). Compute `modeldeployment.gpustack.ai/pod-spec-hash` strictly from explicit user-declared spec inputs and resolved external resource bindings (`InstanceType`, `KVCache`, `ModelArtifact`, `InterfaceResource`, `RuntimeClassName`, and seat identity). Version the operator's dynamically injected Pod fields (`Command` and `Args` synthesis, implicit labels and annotations, probes, drain hooks, default auxiliary images, and `Settings` defaults) with `modeldeployment.gpustack.ai/render-version` and a minimum supported version floor. Skip replacing running Pods whose explicit spec hash matches and whose render version meets the effective floor.

## Motivation

### Goals

- Keep running `ModelDeployment` Pods in place when an operator upgrade or a `Settings` default change only alters dynamically injected Pod fields.
- Trigger replica replacement only when explicit user-declared spec inputs or resolved external bindings change, when `modelDeploymentMinSupportedRenderVersion` rises above a Pod's render version, or when a user sets `modeldeployment.gpustack.ai/min-render-version`.
- Report in `ReplicasUpToDate` when running replicas use an older compatible render version.
- Enforce render-version discipline in CI with a golden tripwire test and a multi-version coexistence test.

The source baseline is commit `7d419d419653869138cef42c7475c472a2461dfa`.
Inspection of the current checkout establishes these facts:

- [`modelDeploymentPodSpecHash`](../pkg/worker/controllers/worker/model_deployment_render.go) hashes the entire rendered `core.Pod` (`Labels`, `Annotations`, and `Spec`).
- Any change to operator-synthesized `Command` or `Args`, implicit labels such as `role-kind`, probe parameters, preStop drain scripts, or `Settings` defaults changes `pod-spec-hash` for every running replica and triggers a full recreate rollout.
- Hash comparisons appear in four places: convergence in [`model_deployment.go`](../pkg/worker/controllers/worker/model_deployment.go), surplus seat sorting in [`model_deployment.go`](../pkg/worker/controllers/worker/model_deployment.go), partial-group fill in [`model_deployment_replacement_slot.go`](../pkg/worker/controllers/worker/model_deployment_replacement_slot.go), and replacement matching and supersession in [`model_deployment_replacement_slot.go`](../pkg/worker/controllers/worker/model_deployment_replacement_slot.go).
- [`TestModelDeploymentLegacyRender_GoldenPods`](../pkg/worker/controllers/worker/model_deployment_legacy_render_test.go) already freezes full rendered Pods under `testdata/legacy-render/`.

### Non-Goals

- Preserving compatibility with pre-feature Pods that carry no `modeldeployment.gpustack.ai/render-version` annotation.
- Maintaining multiple historical Pod-rendering code branches (`renderV1`, `renderV2`) in the controller.
- Changing the `worker.gpustack.ai/v1` or `worker.gpustack.ai/v1alpha1` CRD schemas.
- Altering whole-replica Recreate rollout cadence, Kueue admission gating, or retirement protocol rules.

## Proposal

Split replica currency into two checks: an explicit spec hash and an implicit render version.

Stamp every rendered `ModelDeployment` Pod with two annotations:
- `modeldeployment.gpustack.ai/pod-spec-hash`: SHA-256 digest of the canonical explicit spec and external binding input for that replica member.
- `modeldeployment.gpustack.ai/render-version`: decimal string of `modelDeploymentRenderVersion`.

Define two integer constants in the controller:
- `modelDeploymentRenderVersion`: current version of the operator's dynamically injected Pod shape (initial `1`).
- `modelDeploymentMinSupportedRenderVersion`: lowest render version that running Pods may keep without rolling (initial `1`, always `1 <= min <= current`).

Compute a deployment's effective minimum render version as `max(modelDeploymentMinSupportedRenderVersion, parsedMinRenderVersionAnnotation)`, capped at `modelDeploymentRenderVersion`, where `modeldeployment.gpustack.ai/min-render-version` is an optional annotation on `ModelDeployment`.

During reconciliation, evaluate each seated Pod against its desired member render:
1. If `pod-spec-hash` differs from the desired member's `pod-spec-hash`, mark the replica outdated and roll it.
2. If `pod-spec-hash` matches and the Pod's `render-version` is missing, unreadable, or below the effective minimum render version, mark the replica outdated and roll it.
3. If `pod-spec-hash` matches and the Pod's `render-version` is greater than or equal to the effective minimum render version, treat the Pod as current and leave it running in place.

### User Stories

#### Story 1

As a cluster operator, I want an operator upgrade that only changes internal Pod rendering structure to leave running `ModelDeployment` Pods in place, so that active inference replicas keep serving without losing warm weights or KV cache.

#### Story 2

As an operator maintainer, I want an explicit structure baseline gated by a golden tripwire test and a mandatory-baseline floor, so that accidental render changes fail in tests while non-breaking structure changes skip running Pods and critical fixes still roll them.

#### Story 3

As a workload owner, I want `ModelDeployment` status to show when replicas run on an older structure baseline and a switch to force-roll them, so that I can tell why a new render shape has not applied and roll into it on my own schedule.

### Core Features & Acceptance Criteria

#### Explicit Spec Hash Subject

- Compute `modeldeployment.gpustack.ai/pod-spec-hash` from a canonical input structure rather than the rendered `core.Pod`.
- Include the deployment-level explicit fields that affect the role: `Namespace`, `Name`, `Spec.Model` (`Name`, `ArtifactRef`), `Spec.Engine` (`Name`, `Image`), `Spec.KVCache` (`PoolRef`, `Connector`), `Spec.KVTransfer` (`Protocol`), `Spec.Router` (`Name`), whether the deployment declares a prefill/decode pair, resolved pair parallelism (`ParallelismPair`), `Encode` role names affecting EPD `PodAffinity`, and resolved encoder URLs and encoder-cache leg when an `Encode` role is present.
- Include the role's explicit fields: `Name`, effective `Kind`, `ReplicaSize`, `InstanceType`, pre-overcommit derived `Resources` from [`deriveModelDeploymentResources`](../pkg/worker/controllers/worker/model_deployment_render.go) (`CPU`, `RAM`, `LocalStorage`, and accelerator request), `Image`, `ImagePullPolicy`, `ImagePullSecrets`, `Privileged`, `Ports`, `AdditionalVolumes`, `ShmSize`, `Command`, `ExtraArgs`, `Env`, `Topology`, `TerminationGracePeriodSeconds`, and `ElasticEP` (or elastic master boot width / worker role for elastic Pods).
- Include the resolved external bindings for the role: `InstanceType.Spec.Acceleratable`, `InstanceType.Status.Entrance`, `InstanceType.Status.Detail`, resolved KVCache binding input (`Domain`, `Dtype`, `CachePrefix`, `MasterServerAddress`, `Protocols`, `KVTransfer`, `KVTransferProtocol`, `PublishKVEvents`, `KVEventsHost`, `RoutingSidecar`), resolved [`ModelDeploymentArtifactRender`](../pkg/worker/controllers/worker/model_deployment_artifact.go) weight identity and delivery spec (excluding `Endpoint`, `HTTPSProxy`, and `NoProxy`), `InterfaceResource`, `RuntimeClassName`, and the member's seat (`Ordinal`, `Member`).
- When a role replaces its command (`len(role.Command) > 0`), omit `ExtraArgs`, connector inputs, sibling `ParallelismPair`, and `Engine`-delivery artifact inputs from that role's `pod-spec-hash` subject so take-over roles stay isolated from inputs `renderModelDeploymentPodTemplate` ignores.
- Exclude operator-internal dynamic rendering outputs and cluster `Settings` defaults from `pod-spec-hash`: synthesized container `Command` and `Args` strings, synthesized Mooncake/engine default `Env` entries, default `/dev/shm` volume when `ShmSize` is unset, probe paths/timeouts/thresholds, preStop drain hook scripts, default routing sidecar image and flags, `TCPTWReuse` sysctl, `GeneralResourcesOvercommit` scaling factor, `NativeSidecar` container placement, `ModelHub*` endpoint/proxy settings, and operator-added Pod labels or annotations.
- Normalize optional fields with `omitempty` and canonical defaults so adding a new unset field to `ModelDeploymentRole` does not change `pod-spec-hash`.

#### Implicit Render Version and Skip Rule

- Stamp `modeldeployment.gpustack.ai/render-version` on every Pod rendered by [`stampModelDeploymentPod`](../pkg/worker/controllers/worker/model_deployment_render.go) and [`model_deployment_elastic_realization.go`](../pkg/worker/controllers/worker/model_deployment_elastic_realization.go).
- Apply a single helper for currency checks across [`model_deployment.go`](../pkg/worker/controllers/worker/model_deployment.go) and [`model_deployment_replacement_slot.go`](../pkg/worker/controllers/worker/model_deployment_replacement_slot.go):
  - In main convergence (`model_deployment.go`), keep a seated Pod when its `pod-spec-hash` matches the desired member's `pod-spec-hash` and its `render-version >= effectiveMinRenderVersion`; for a multi-member replica (`ReplicaSize > 1`), also mark the ordinal outdated if its standing members do not all carry the same `render-version`.
  - In surplus seat sorting (`modelDeploymentSurplusReplicas`), prefer a seated Pod that satisfies the same currency helper; when two occupants on one seat both satisfy it, prefer the `render-version` shared by the ordinal's other seated members (when `ReplicaSize > 1`) and otherwise the higher `render-version` before falling back to Pod name.
  - In partial-group fill (`modelDeploymentPartialGroupMembers`), require exact match on both `pod-spec-hash` and `render-version` against the current render so a replacement group never mixes members across render versions.
  - In active replacement slot evaluation (`replacementMembersMatch`, `replacementSuperseded`), require every standing member to match the desired member's `pod-spec-hash`, carry a uniform `render-version >= effectiveMinRenderVersion` (and equal to `modelDeploymentRenderVersion` when the standing group is still partial), so complete in-flight replacements survive non-mandatory upgrades while floor bumps or partial older groups are superseded.

#### Force-Roll Annotation and Status Visibility

- Read `modeldeployment.gpustack.ai/min-render-version` on `ModelDeployment.Annotations` and pass changes to that annotation through the primary `ModelDeployment` watch predicate in [`SetupController`](../pkg/worker/controllers/worker/model_deployment.go).
- If the annotation holds a valid positive integer, set `effectiveMinRenderVersion = min(modelDeploymentRenderVersion, max(modelDeploymentMinSupportedRenderVersion, parsed))`.
- If the annotation is absent, empty, non-numeric, or less than `modelDeploymentMinSupportedRenderVersion`, use `modelDeploymentMinSupportedRenderVersion`.
- Track the count of current replicas running on `render-version < modelDeploymentRenderVersion` in `modelDeploymentRollout`.
- In [`observeModelDeploymentRollout`](../pkg/worker/controllers/worker/model_deployment_rollout.go), when `ReplicasUpToDate` is `True` (`Reason: UpToDate`) and at least one replica runs on an older compatible `render-version`, include the count of replicas on older render versions, the current `modelDeploymentRenderVersion`, and the `modeldeployment.gpustack.ai/min-render-version` annotation key in the condition `Message`.

#### Service, Router, and Coexistence Invariants

- Keep Service and Router identity selector keys (`app.kubernetes.io/name`, `app.kubernetes.io/instance`, `app.kubernetes.io/component`, `modeldeployment.gpustack.ai/role-kind`, `modeldeployment.gpustack.ai/member-index`, and `modeldeployment.gpustack.ai/endpoint-eligible`) invariant across non-mandatory render versions.
- If a future change modifies selector keys or requires a Pod-side contract that older render versions lack, require bumping `modelDeploymentMinSupportedRenderVersion` to `modelDeploymentRenderVersion`.
- Add a coexistence test proving that a deployment with replicas on version `1` and version `2` serves both through its Service and Router selectors, keeps the version `1` replica untouched during scale-up and scale-down, rolls both when an explicit role field changes, and rolls the version `1` replica when `modeldeployment.gpustack.ai/min-render-version: "2"` is set.

#### Golden Tripwire Test

- Extend [`model_deployment_legacy_render_test.go`](../pkg/worker/controllers/worker/model_deployment_legacy_render_test.go) and `testdata/legacy-render/` with a recorded version manifest (`version.json`) storing `renderVersion`, `minSupportedRenderVersion`, and each fixture's rendered Pod digest alongside the fixture's `pod-spec-hash`.
- Assert `1 <= modelDeploymentMinSupportedRenderVersion <= modelDeploymentRenderVersion`.
- If any fixture's rendered Pod JSON changes while `modelDeploymentRenderVersion` equals the recorded `renderVersion` in `version.json`, fail the test with a message instructing the author to bump `modelDeploymentRenderVersion` before running `-update`.
- Verify that changing a dynamically injected field or `Settings` default moves the rendered Pod bytes while leaving `pod-spec-hash` unchanged.

### Notes / Constraints / Caveats

- Use the existing Go, Kubernetes, controller-runtime, and Kueue stack.
- Do not modify `api/worker/v1` or `api/worker/v1alpha1` schemas.
- `Settings` changes (`model-deployment-routing-sidecar-image`, `model-deployment-tcp-tw-reuse`, `instance-general-resources-overcommit`, `model-hub-*`, `container-registry`, `container-namespace`) apply to newly created or replaced Pods; existing Pods keep their running configuration until replaced or force-rolled via `modeldeployment.gpustack.ai/min-render-version`.

### Boundaries

- **Always:** keep `pod-spec-hash` deterministic and independent of dynamically injected Pod fields; require all members of a single replica ordinal to share one render version; update the owning documentation and `docs/README.md`.
- **Ask first:** change any CRD schema field, alter Service/Router selector labels, or run cluster builds and end-to-end tests.
- **Never:** mix members of different `render-version`s inside a single multi-member replica group, or silently rewrite golden fixtures without a `modelDeploymentRenderVersion` bump when rendered Pod bytes change.

### Risks and Mitigations

- A maintainer changes `renderModelDeploymentPodTemplate` without bumping `modelDeploymentRenderVersion` → the golden tripwire test compares rendered Pod bytes against `testdata/legacy-render/` and fails whenever bytes change without a version bump.
- A maintainer adds a new user-facing field to `ModelDeploymentRole` and forgets to include it in `pod-spec-hash` → add a reflection/completeness unit test over `ModelDeploymentRole` and `ModelDeploymentSpec` fields that fails if a struct field is neither included in the canonical spec-hash input nor explicitly listed as non-Pod-affecting (`Replicas`).
- Partial-group recovery or duplicate-seat trimming leaves a multi-member replica with mixed `render-version`s → `modelDeploymentPartialGroupMembers` requires matching `(pod-spec-hash, render-version)`, `modelDeploymentSurplusReplicas` tie-breaks toward the ordinal's peer `render-version`, and main convergence marks any multi-member ordinal with non-uniform `render-version`s outdated.
- An operator sets `modeldeployment.gpustack.ai/min-render-version` without editing `.spec`, so `GenerationChangedPredicate` would ignore the event → combine `GenerationChangedPredicate` with a predicate watching changes to `modeldeployment.gpustack.ai/min-render-version` in `SetupController`.
- An invalid value in `modeldeployment.gpustack.ai/min-render-version` stalls reconciliation → ignore non-positive or unparseable values and fall back to `modelDeploymentMinSupportedRenderVersion`.

## Design Details

### Commands

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./pkg/worker/controllers/worker/...
make lint
make lint docs
make build
```

### Project Structure

- `pkg/worker/controllers/worker/model_deployment_render.go`: canonical explicit `pod-spec-hash` computation, `render-version` constants and annotations, and Pod stamping.
- `pkg/worker/controllers/worker/model_deployment_connector.go`: explicit KVCache binding input preserved on `ModelDeploymentConnectorRender`.
- `pkg/worker/controllers/worker/model_deployment_elastic_realization.go`: elastic master, worker, and head Pod `pod-spec-hash` and `render-version` stamping.
- `pkg/worker/controllers/worker/model_deployment.go`: effective render-version floor evaluation, replica currency evaluation, intra-ordinal `render-version` uniformity check, surplus seat sorting, rollout accounting, and `min-render-version` watch predicate.
- `pkg/worker/controllers/worker/model_deployment_replacement_slot.go`: partial-group fill and replacement slot matching and supersession rules across render versions.
- `pkg/worker/controllers/worker/model_deployment_rollout.go`: `ReplicasUpToDate` condition message for older compatible render versions.
- `pkg/worker/controllers/worker/model_deployment_render_test.go`, `pkg/worker/controllers/worker/model_deployment_legacy_render_test.go`, `pkg/worker/controllers/worker/testdata/legacy-render/`, `pkg/worker/controllers/worker/model_deployment_rollout_test.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go`, and `pkg/worker/controllers/worker/model_deployment_test.go`: unit, golden tripwire, replacement-slot, status, and multi-version coexistence tests.
- `docs/modules/model-deployment/deployment.md`, `docs/modules/model-deployment/status.md`, and `docs/README.md`: rollout contract, `min-render-version` annotation, `ReplicasUpToDate` message, and module map registration.

### Code Style

Follow existing controller conventions: pure render helpers, level-based reconciliation, plain comments without task IDs, and table-driven tests using the fake client.

Existing [`modelDeploymentPodSpecHash`](../pkg/worker/controllers/worker/model_deployment_render.go) serializes a canonical struct with `encoding/json` and hashes it with SHA-256:

```go
encoded, err := json.Marshal(subject)
if err != nil {
	return "unhashable-" + err.Error()
}

sum := sha256.Sum256(encoded)

return hex.EncodeToString(sum[:])
```

Pre-compute the role-template's explicit hash inside [`renderModelDeploymentPodTemplate`](../pkg/worker/controllers/worker/model_deployment_render.go) and combine it with `(ordinal, member)` inside [`stampModelDeploymentPod`](../pkg/worker/controllers/worker/model_deployment_render.go) while stamping `modeldeployment.gpustack.ai/render-version`, keeping `stampModelDeploymentPod`'s signature unchanged.

### Implementation Plan

- [x] **T1: Implement canonical explicit `pod-spec-hash`, `render-version` Pod stamping, and render unit tests**
  - Blocked by: none
  - Owns: `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_connector.go`, `pkg/worker/controllers/worker/model_deployment_elastic_realization.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`
  - Gate: review
  - Acceptance:
    - `stampModelDeploymentPod` and elastic Pod renderers stamp `modeldeployment.gpustack.ai/render-version: "1"` and compute `modeldeployment.gpustack.ai/pod-spec-hash` strictly from canonical explicit spec fields and resolved external bindings (`InstanceType`, `KVCache` binding input, `ModelArtifact` excluding hub `Settings`, `InterfaceResource`, `RuntimeClassName`, and `(Ordinal, Member)`), isolating take-over roles (`len(role.Command) > 0`) from unused inputs.
    - Dynamically injected Pod fields and `Settings` (`TCPTWReuse`, `GeneralResourcesOvercommit`, `NativeSidecar`, `ModelDeploymentRoutingSidecarImage`, `ModelHub*` endpoint/proxy, `ContainerRegistry`, `ContainerNamespace`) do not change `pod-spec-hash`.
    - Reflection completeness test verifies every field on `ModelDeploymentSpec` and `ModelDeploymentRole` is accounted for in the canonical spec-hash input or explicitly excluded (`Replicas`).
  - Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 -run 'TestModelDeploymentPodSpecHash|TestRenderModelDeploymentPod' ./pkg/worker/controllers/worker`

- [x] **T2: Re-baseline golden render fixtures and add the `version.json` render-version tripwire test**
  - Blocked by: [T1]
  - Owns: `pkg/worker/controllers/worker/model_deployment_legacy_render_test.go`, `pkg/worker/controllers/worker/testdata/legacy-render/*`
  - Acceptance:
    - `testdata/legacy-render/version.json` records `renderVersion`, `minSupportedRenderVersion`, and each fixture's `podSpecHash` and rendered Pod SHA-256 digest alongside the re-baselined golden JSON files.
    - `TestModelDeploymentLegacyRender_GoldenPods` asserts `1 <= modelDeploymentMinSupportedRenderVersion <= modelDeploymentRenderVersion` and fails with an instruction to bump `modelDeploymentRenderVersion` if any rendered Pod bytes change without a version bump.
  - Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 -run 'TestModelDeploymentLegacyRender' ./pkg/worker/controllers/worker`

- [x] **T3: Implement render-version floor evaluation, convergence skip rule, replacement slot matching, watch predicate, and `ReplicasUpToDate` reporting**
  - Blocked by: [T1]
  - Owns: `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot.go`, `pkg/worker/controllers/worker/model_deployment_rollout.go`
  - Gate: review
  - Acceptance:
    - `convergeModelDeployment` skips replacing seated Pods whose `pod-spec-hash` matches and whose `render-version >= effectiveMinRenderVersion`, condemns multi-member ordinals with mixed `render-version`s, preserves intra-ordinal `render-version` uniformity in `modelDeploymentSurplusReplicas`, and wakes on `modeldeployment.gpustack.ai/min-render-version` annotation updates via `SetupController`.
    - `modelDeploymentPartialGroupMembers`, `replacementMembersMatch`, and `replacementSuperseded` prevent mixing `render-version`s inside partial groups while keeping complete in-flight replacements valid across non-mandatory upgrades and superseding them when `effectiveMinRenderVersion` advances past their `render-version`.
    - `observeModelDeploymentRollout` reports the count of replicas running on older compatible render versions, the current `modelDeploymentRenderVersion`, and `modeldeployment.gpustack.ai/min-render-version` in the `ReplicasUpToDate` (`True` / `UpToDate`) condition message.
  - Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 -run 'TestModelDeploymentRollout|TestObserveModelDeploymentRollout|TestModelDeploymentReplacement' ./pkg/worker/controllers/worker`

- [ ] **T4: Add controller convergence, replacement-slot, status, and multi-version coexistence tests**
  - Blocked by: [T2, T3]
  - Owns: `pkg/worker/controllers/worker/model_deployment_test.go`, `pkg/worker/controllers/worker/model_deployment_rollout_test.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go`
  - Acceptance:
    - Unit and controller tests verify skip-on-compatible-render-version, mandatory floor rollout, `modeldeployment.gpustack.ai/min-render-version` force-roll and invalid-value fallback, missing `render-version` rollout, multi-member intra-ordinal uniformity, replacement-slot upgrade/supersession behavior, and `RolloutHeldByCache` when `connection` becomes `nil`.
    - Coexistence test proves a deployment with `render-version: "1"` and `"2"` replicas serves both through Service and Router selectors, preserves the `"1"` replica across scale-up and scale-down, rolls both on an explicit role field edit, and rolls the `"1"` replica when `modeldeployment.gpustack.ai/min-render-version: "2"` is set.
  - Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./pkg/worker/controllers/worker/...`

- [ ] **T5: Update `ModelDeployment` documentation and verify repo-wide lint and docs gates**
  - Blocked by: [T3]
  - Owns: `docs/modules/model-deployment/deployment.md`, `docs/modules/model-deployment/status.md`, `docs/README.md`
  - Acceptance:
    - `docs/modules/model-deployment/deployment.md` and `docs/modules/model-deployment/status.md` document the split between `pod-spec-hash` and `render-version`, the `modeldeployment.gpustack.ai/min-render-version` annotation, `Settings` non-rollout semantics, and the `ReplicasUpToDate` older-baseline message.
    - `docs/README.md` indexes `specs/2026-10-18-model-deployment-render-version.md` under `Model Deployment`, and `make lint` and `make lint docs` pass.
  - Verify: `make lint && make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

- `worker`: [`pkg/worker/controllers/worker/model_deployment_render_test.go:2359`](../pkg/worker/controllers/worker/model_deployment_render_test.go#L2359) - Update the 9 pinned Pod JSON SHA-256 digests in `TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender` and the `TCPTWReuse` hash assertion at [`pkg/worker/controllers/worker/model_deployment_render_test.go:3046`](../pkg/worker/controllers/worker/model_deployment_render_test.go#L3046) to reflect the explicit `pod-spec-hash` and `modeldeployment.gpustack.ai/render-version: "1"` annotation.
- `worker`: [`pkg/worker/controllers/worker/model_deployment_legacy_render_test.go:189`](../pkg/worker/controllers/worker/model_deployment_legacy_render_test.go#L189) - Re-baseline `testdata/legacy-render/*.json` and add `testdata/legacy-render/version.json` so `TestModelDeploymentLegacyRender_GoldenPods` enforces render-version bumps whenever rendered Pod bytes change.

##### Unit tests

- `worker/render`: [`pkg/worker/controllers/worker/model_deployment_render_test.go:1745`](../pkg/worker/controllers/worker/model_deployment_render_test.go#L1745) - Verify `modelDeploymentPodSpecHash` moves on every explicit `ModelDeploymentSpec`, `ModelDeploymentRole`, and external binding input (`InstanceType` entrance/manufacturer/unit resources, `KVCache` endpoint/domain/raw dtype/cache prefix, `ModelArtifact` delivery/revision/digest, `InterfaceResource`, `RuntimeClassName`, and seat `Ordinal`/`Member`), stays unchanged across dynamically injected Pod changes and `Settings` toggles (`TCPTWReuse`, `GeneralResourcesOvercommit`, `NativeSidecar`, `ModelDeploymentKVCacheDtypeOwned`, `ModelDeploymentRoutingSidecarImage`, `ModelHub*` endpoint/proxy, `ContainerRegistry`, `ContainerNamespace`), isolates take-over roles (`len(role.Command) > 0`), stamps Elastic EP master/worker/head Pods, and passes a reflection completeness check over `ModelDeploymentSpec` and `ModelDeploymentRole` struct fields.
- `worker/legacy-render`: [`pkg/worker/controllers/worker/model_deployment_legacy_render_test.go:189`](../pkg/worker/controllers/worker/model_deployment_legacy_render_test.go#L189) - Verify `1 <= modelDeploymentMinSupportedRenderVersion <= modelDeploymentRenderVersion`, fail when any fixture's rendered Pod bytes differ from `testdata/legacy-render/` while `modelDeploymentRenderVersion` equals `version.json`, and verify a simulated dynamic render change moves the Pod digest while preserving `pod-spec-hash`.
- `worker/rollout`: [`pkg/worker/controllers/worker/model_deployment_rollout_test.go:40`](../pkg/worker/controllers/worker/model_deployment_rollout_test.go#L40) - Verify `observeModelDeploymentRollout` keeps `ReplicasUpToDate=True (UpToDate)` with the standard message when all replicas match `modelDeploymentRenderVersion`, and appends the older-version replica count, current `modelDeploymentRenderVersion`, and `modeldeployment.gpustack.ai/min-render-version` annotation hint when one or more current replicas run on an older compatible `render-version`.
- `worker/replacement-slot`: [`pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go:31`](../pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go#L31) - Verify `modelDeploymentPartialGroupMembers` refuses to top up a partial group whose standing members carry an older `render-version`, while `replacementMembersMatch` and `replacementSuperseded` keep a complete in-flight replacement at `render-version >= effectiveMinRenderVersion` valid across non-mandatory upgrades and supersede it when `effectiveMinRenderVersion` rises above its `render-version`.

##### Integration tests

- `worker/reconciler`: [`pkg/worker/controllers/worker/model_deployment_test.go:875`](../pkg/worker/controllers/worker/model_deployment_test.go#L875) - Drive `ModelDeploymentReconciler.Reconcile` over the fake client to verify: (1) seated Pods with matching `pod-spec-hash` and `render-version >= effectiveMinRenderVersion` are not replaced when dynamic render fields change at a higher `modelDeploymentRenderVersion`, (2) Pods with missing `render-version` or `render-version < effectiveMinRenderVersion` roll one per role per pass, (3) `modeldeployment.gpustack.ai/min-render-version` force-rolls older replicas and ignores invalid/below-floor values, (4) multi-member ordinals with mixed `render-version`s are marked outdated and duplicate-seat surplus shedding preserves intra-ordinal `render-version` uniformity, (5) `RolloutHeldByCache` still holds when `connection` drops to `nil` on a `Server` deployment, and (6) a deployment with `render-version: "1"` and `"2"` replicas coexists across Service and Router selectors, scale-up, scale-down, explicit spec edits, and annotation force-roll.

##### e2e tests

- Covered by controller-runtime fake-client reconciliation and golden fixture tests in `pkg/worker/controllers/worker`; cluster e2e (`gpustack-operator-e2e`) is gated behind user confirmation per project rules.

## Alternatives

- **Maintain historical Pod renderers per version (`renderV1`, `renderV2`) and hash the full `core.Pod`**: rejected because every change to synthesized arguments, probes, sidecars, or connector flags would require duplicating or branching the renderer indefinitely.
- **Strip a blocklist of structural fields from a rendered `core.Pod` before hashing**: rejected because synthesized engine flags, environment variables, and volumes are merged into the same container slices as user-declared values and cannot be reliably separated after rendering.
- **Persist the render version on `ModelDeployment.Spec.Roles[]` via the mutating webhook**: rejected because it requires a CRD schema change and still requires historical render branches in the controller to produce the older Pod shape.

## Open Questions

None.
