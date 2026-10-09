# Spec: ModelDeployment Role Configuration Mutability

Status: Building
Blocked on: completion of authorized runtime testing before PR creation.
Type: Feature

Supersedes: the size-freeze rule in "Role Replica Admission Unit", Summary and Motivation / Goals 7.
Also supersedes ordinary-role freezes in "KV Cache and ModelDeployment — Per-Group Transport, API Consolidation, and Replaceable Replicas".
The affected sections are Motivation / Non-Goals, Notes / Constraints / Caveats, and Boundaries.
Only size, resources, instanceType, and command mutability changes.
Preserve the earlier specifications as historical records.

## Summary

Allow ordinary ModelDeployment roles to update size, resources, instanceType, and command.
Apply changed configuration through whole-replica Recreate with fresh Kueue admission.
Preserve healthy old replicas until their replacement starts.
Keep role identity and validation rules.
Document shared prefill/decode effects and preserve Elastic EP restrictions.

## Motivation

### Goals

- Let workload owners change instance shape and accelerator requests without creating another deployment.
- Support pool changes and managed-command transitions.
- Replace affected replicas one at a time.
- Preserve unrelated Pods, Workloads, and serving selection.
- Keep quota accounting, group cleanup, and update status accurate.

The source baseline is commit `5ca7e36bc1d6667654aeb98faa2034cf913e9b37`.
The supplied handoff requests mutable role configuration.
Source inspection found these additional requirements:

- The [update webhook](../pkg/worker/webhooks/worker/model_deployment.go) freezes all four fields.
  Interface validation currently reruns only for router or transfer changes.
- [Convergence](../pkg/worker/controllers/worker/model_deployment.go) compares existing member counts with desired size.
  A size difference therefore enters incomplete-group repair and bypasses the admission guard.
- [Qualification](../pkg/worker/controllers/worker/model_deployment_health.go) also uses desired size.
  All outdated ordinals enter pending replacement.
  Active qualification selectors can therefore withdraw every old managed Server endpoint together.
- [Service rendering](../pkg/worker/controllers/worker/model_deployment_service.go) uses desired size for peer DNS and leader selection.
  A decrease to one member can remove old peer Services and expose old followers through legacy selectors.
- [Retirement](../pkg/worker/controllers/worker/model_deployment_retirement.go) requires old replicas to match the current role's shape.
  Size or command changes can prevent classification.
- [The existing pair test](../pkg/worker/controllers/worker/model_deployment_test.go), TestModelDeployment_DegreeEditRollsThePair, asserts shared effects.
  Ascend TP/DP changes move both roles' hashes; ordinary argument changes affect one role.

These are observations from the source baseline before implementation.

### Non-Goals

- Changing the role map key or allowing role additions, removals, renames, or kind changes.
- Relaxing model, engine, cache, or router identity rules.
- Adding surge settings, revision APIs, or another workload controller.
- Resizing a running engine collective in place.
- Adding Elastic EP configuration replacement.
- Guaranteeing uninterrupted inference during Recreate or shared transfer changes.

## Proposal

Make the four fields editable for ordinary Server, Prefill, and Decode roles.
New configuration determines replacement Pods and their admission.
Existing Pods keep their deployed configuration.

Different ordinals can run different configurations during replacement.
Replace every member of a selected replica and remove its old Workload.
Create its replacement after the old group and Workload leave.
Continue only after fresh Kueue admission.

A role is affected when its rendered configuration changes.
Shared transfer configuration can affect both roles.
Keep an unrelated role unchanged when its rendered configuration stays unchanged.

### User Stories

#### Story 1

As a workload owner, I want editable size and resources, so that I can change instance shape and allocation.

#### Story 2

As a workload owner, I want editable instanceType and command, so that I can change placement and execution.

#### Story 3

As a prefill/decode operator, I want gradual replacement, so that unrelated capacity remains available.

#### Story 4

As an operator, I want documented shared effects and constraints, so that I can predict update behavior.

### Core Features & Acceptance Criteria

#### Mutability and identity

- Accept valid size, resources, instanceType, and command updates on ordinary roles.
- Cover size increases, decreases, and transitions across one member.
- Cover accelerator count, slicing, partition, and interface changes when valid.
- Cover managed-to-command, command-to-managed, and command-to-command transitions.
- Keep roles keyed by unique name. List reordering preserves identity.
- Reject additions, removals, renames, and kind changes.
- Allow repeated Server roles. Allow at most one Prefill and one Decode.
- Reject mixing Server with Prefill or Decode.
- Keep size within 1 through 64 and preserve all configuration validity rules.

#### Replacement and admission

- Replace whole replicas. Do not trim or append members within an old admitted group.
- Permit at most one unresolved configuration replacement per affected role.
- Wait for fresh replacement admission before deleting another healthy old replica.
- A queued replacement does not authorize further healthy deletions.
- Read an old replica's completeness from its deployed shape.
- Desired-size differences alone do not qualify for incomplete-group repair.
- Preserve recovery for actual member loss and interrupted writes.
- Do not require obsolete, unadmitted configuration to become admitted before it can be replaced.
- Apply this rule to complete groups waiting for first admission, including those without a replacement slot.
- Select these groups before admitted siblings and use the same single-slot cleanup path.
- Verify current member admission gates and exclude earlier execution, eviction, and preemption history.
- Recovery preserves replacement limits and does not spend additional healthy serving capacity.
- Reconciliation retries and restarts never overlap old and new members in one ordinal.
- Combined replica-count and configuration edits preserve these ownership and cadence rules.

#### Cleanup and mixed configurations

- Remove every selected old member and its Workload, including when old size exceeds new size.
- Verify old group vacancy and Workload removal before creating a replacement.
- Compose a new Workload with the new count, template, resource request, and queue.
- Do not reuse the old reservation for changed scheduling inputs.
- Keep untouched replicas' Workloads and reservations.
- Preserve accurate resource attribution during pool changes and mixed configurations.
- Distinguish outdated configuration from an active replacement.
- Keep healthy old replicas eligible until their individual replacement starts.
- Preserve peer DNS until all old members that need it have left.
- Keep legacy Service leader selection correct while multi-member old replicas remain.
- Give new multi-member replicas the required peer DNS and rank metadata.
- Preserve External DP answering-member rules.
- Read old replica size and execution shape for retirement.
- Desired configuration differences alone do not prevent retirement.
- Preserve health revocation, drain, ownership, and unsupported-observation rules.
- Joint admission recognizes existing groups by their deployed shape.
- A configuration edit does not park or revoke an already-admitted sibling by itself.

#### Shared effects, validation, and documentation

- For independent edits, unchanged roles retain Pod UIDs, hashes, member sets, and Workload UIDs.
- Their serving selection and peer DNS remain correct.
- For shared edits, replace all roles whose rendered configuration changes.
- Preserve Ascend transfer TP/DP propagation, including command-supplied degrees.
- Keep shared configuration in fingerprints.
- Document possible transfer incompatibility while both roles converge.
- Retain defaulting, resource-mode, parallel-width, interface, transport, port, naming, host-access, and owned-key checks.
- Recheck dependencies changed by resource, pool, and command updates.
- Preserve escape hatches for unchanged configuration when external dependencies drift.
- Rejected updates change no child resources.
- Preserve Elastic EP's managed command, size=1, replicas=1, and accelerator-to-TP constraint.
- Preserve Elastic EP presence, TP, maximum DP, and width-update restrictions.
- Ordinary relaxation does not enable previously forbidden Elastic EP instanceType or resources edits.
- Update the owning guide, API comments, and generated descriptions.
- Explain role identity, shared effects, cadence, quota waits, and Recreate downtime.
- Explain merge-patch list replacement and omitted-field effects under the mutable contract.
- Register this spec in the Model Deployment module map.
- Verify actual Pods, Workloads, selectors, endpoint membership, and peer Services.
- Hash comparisons alone do not establish safe replacement.

### Notes / Constraints / Caveats

- Use the existing Go, Kubernetes, controller-runtime, and Kueue stack.
- Keep public resources at worker.gpustack.ai/v1; v1alpha1 remains internal storage.
- Keep size, resources, and instanceType as structured scheduling inputs.
- Use rendered fingerprints to identify configuration differences.
- Use observed group metadata and ownership to establish deployed facts.
- Fresh admission can wait for capacity. Keep additional healthy replicas while it waits.
- A single-replica role can lose capacity during Recreate.
- Admission advances replacement before engine readiness. Multiple replicas do not guarantee continuous serving capacity.
- Changed ports, commands, hardware, or shared transfer configuration can disrupt requests.
- An unchanged sibling Pod does not guarantee complete inference while its peer is unavailable.
- Elastic EP keeps its existing behavior outside the new replacement contract.

### Boundaries

- **Always:** preserve identity, validation, whole-group replacement, ownership, quota accounting, and cleanup.
- **Always:** distinguish observed state, desired configuration, replacement progress, and serving capacity.
- **Ask first:** expand Elastic EP replacement, change public identity rules, or run cluster builds, deployments, and end-to-end verification.
- **Never:** mix old and new members within one admitted group or reuse old scheduling admission.
- **Never:** weaken fingerprints or health checks to conceal incompatible configuration.

### Risks and Mitigations

- Desired size condemns healthy old groups → evaluate deployed completeness separately.
- Repair bypasses rollout cadence → reserve repair for actual broken membership.
- Peer DNS or leader selection changes early → retain old dependencies through group departure.
- A queued replacement drains remaining capacity → wait for fresh admission before further healthy replacement.
- Changed requests reuse stale admission → remove the old Workload and compose a new one.
- Command changes confuse retirement → read old execution from deployed Pods.
- Shared degrees become stale on one side → retain coupled replacement and document the compatibility window.
- Elastic edits are accepted but ignored → preserve profile-specific refusals.
- A restart loses replacement selection → persist the active slot before deleting members.
- Stale named Workloads lose their owners → bind cleanup to proven member and Workload UIDs.
- A shape edit interrupts retirement → classify the reserved members from their deployed facts.
- Surplus arithmetic trims old groups → identify duplicate seats separately from desired-size differences.

## Design Details

### Commands

Validation runs locally, using the current worktree's source.
The user confirmed unit tests, lint, generation, and compilation.
The user separately authorized cluster tests on owned temporary accelerator infrastructure.

Run focused task tests from the repository root:

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./api/worker/v1alpha1 ./pkg/worker/webhooks/worker ./pkg/worker/controllers/worker
make lint
make lint docs
make build
```

Collect package coverage with `go test -cover` during final validation.
General lint can edit source. Run it after all parallel writers settle.
Regenerate after its final source changes.

Generation requires a resolved checkout path ending in `gpustack.ai/gpustack`.
Use a detached local generation worktree under a task-specific directory beneath `${HOME}/.gpustack-gen/`.
Pin it to the current source commit after local source checks pass.
Confirm its commit and edited source content before invoking `make generate`.
Copy only generator outputs back to the implementation worktree.
Run generation twice and compare output contents to prove deterministic parity.
Preserve the shared main checkout.

The build uses the host platform unless BUILD_PLATFORMS is set.
Use the authorized remote builder for packaged runtime validation.
Publish a development image tied to the source commit and record its tested digest.
Run gpustack-operator-e2e against the isolated temporary cluster.

### Project Structure

- `api/worker/v1alpha1/model_deployment.go`: internal contracts and generation inputs.
- `api/worker/v1/model_deployment.go`: public resource type.
- `pkg/worker/webhooks/worker/model_deployment*.go`: identity, profile, and configuration validation.
- `pkg/worker/controllers/worker/model_deployment*.go`: render, rollout, admission, health, retirement, Services, and status.
- `docs/modules/model-deployment/deployment.md`: owning update and rollout guide.
- `docs/modules/model-deployment/prefill-decode.md`: shared transfer behavior.
- `docs/modules/model-deployment/shutdown.md`: departure behavior.
- `docs/modules/model-deployment/status.md`: progress and capacity interpretation.
- `docs/modules/model-deployment/elastic-ep.md`: retained profile restrictions.
- `docs/README.md`: shared routing.
- `specs/`: versioned design records.

### Code Style

Use focused Go helpers, explicit errors, table-driven tests, and fake clients.
Assert observable state. Keep reconciliation level-based and repeatable.
Avoid a public revision field for internal bookkeeping.

Existing [group cleanup](../pkg/worker/controllers/worker/model_deployment_pod_group.go) treats repeated absence as success:

```go
for _, wl := range wls {
    if err = r.Client.Delete(ctx, wl); err != nil && !kerrors.IsNotFound(err) {
        return fmt.Errorf("delete workload %s: %w", wl.Name, err)
    }
}
```

### Implementation Design

Keep desired rendering separate from deployed replica facts.
Leave modelDeploymentRoleSize as the desired-size helper.
Add a private replica reader for observed membership and execution.

Read the group total, member indices, ownership, queue, and matching Workload.
Require consistent totals and unique member seats.
Preserve the legacy single-member fallback only when its metadata supports that shape.
Unknown or contradictory observations do not authorize deleting healthy capacity.
Unreadable shape or execution keeps health and cache observations Unknown.
It does not grant new endpoint eligibility.
Preserve existing eligibility through the established unknown-observation policy.
Readable member loss remains a positive health failure.

Use deployed facts in health, retirement, joint admission, ready counts, and cache observation.
Preserve deployed command mode. A desired managed command does not make an old takeover Pod managed.
A changed desired size or command does not rewrite those facts.
Preserve all supported-version, health-revocation, and drain guards.

Record one active replacement slot per affected role before deleting its old members.
Use private controller metadata on ModelDeployment when observation alone cannot recover that slot.
Keep the record small and use optimistic concurrency.
It is internal bookkeeping, not a user setting or public revision API.

Recover the same slot through partial deletion, vacancy, creation, and admission.
Read Pods and Workloads through the API reader before reusing an ordinal.
Remove old Workloads by proven ownership and verify their absence.
A derived group name locates a Workload; it does not authorize deleting a foreign object.
Bind cleanup to captured member and Workload UIDs.
A new Workload must own the replacement members and describe their scheduling inputs.

Clear the active slot only after complete current members receive fresh Workload admission.
A newer configuration can replace an obsolete queued configuration in the same active slot.
It does not authorize another healthy old replica's deletion.
Real membership failures remain repairable without spending other healthy capacity.
A broken old replica can wait for the role's active replacement to receive admission.
Then replace the broken replica as a whole before selecting another healthy old replica.
Do not add new-configuration members beside surviving old-configuration members, even when their sizes match.
An interrupted create can fill missing members only within its current replacement configuration.
Record new member UIDs during creation, before the next reconciliation observation.
If the active replacement loses members, recover that same slot as a whole.
Retain its captured Workload identity when all members disappear or a cleanup response is lost.
Replica-count edits cancel or finish only the affected slot after cleanup.

Qualification distinguishes an outdated replica from a selected replacement.
Keep old healthy eligibility until selection.
During command-mode changes, preserve each deployed mode's routing rules.
Managed replicas keep engine qualification. Takeover replicas keep legacy Service membership and remain Unmanaged.
Preserve deployed answering selection from the first edit, before the replacement configuration appears.
If a shared selector needs private routing labels, write those labels before switching the Service.
Routing membership does not establish engine health or cache qualification.
Retain peer Services required by actual members, including terminating members.
Read those members uncached before synchronizing Services, so informer lag cannot retire their peer DNS.
Keep leader selection valid while old multi-member and new single-member groups coexist.
Preserve External DP answering-member selection.
Role-level parallelism remains declared configuration; it does not describe every running replica during replacement.

Revalidate interface and transport inputs when their dependencies change.
Compare roles by name, so list reordering does not trigger a false dependency change.
Retain unchanged-configuration escape hatches for external dependency drift.
Keep Elastic EP restrictions through an explicit profile-specific identity path.

### Implementation Plan

The user authorized unattended implementation with mcode and qwen.
Keep the current working branch and use local validation.
Workers own disjoint files. The coordinator owns commits and generated artifacts.
The implementation tasks do not create a PR.
Cluster acceptance follows separate user authorization.

- [x] **T1 · Deployed replica facts and observation**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group*.go`, `pkg/worker/controllers/worker/model_deployment_group_shape*.go`, `pkg/worker/controllers/worker/model_deployment_render*.go`, `pkg/worker/controllers/worker/model_deployment_health*.go`, `pkg/worker/controllers/worker/model_deployment_retirement*.go`, `pkg/worker/controllers/worker/model_deployment_joint_admission*.go`, `pkg/worker/controllers/worker/model_deployment_cache_attached*.go`, `pkg/worker/controllers/worker/model_deployment_status*.go`, `pkg/worker/controllers/worker/model_deployment_answering_shape_test.go`, `pkg/worker/controllers/worker/model_deployment_service_test.go`
      Gate: review
      Acceptance: Read deployed size and execution without desired-shape substitution. Reject duplicate seats and contradictory group totals. Preserve existing unchanged-shape behavior and unknown-observation guards. Demonstrate old healthy groups remain complete and observable after a desired size or command edit.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./pkg/worker/controllers/worker`

Checkpoint: validate the replica reader and its negative controls before changing replacement cadence.

- [x] **T2 · Ordinary role update contract**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_deployment_test.go`, `pkg/worker/webhooks/worker/model_deployment*.go`, `docs/modules/model-deployment/deployment.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/modules/model-deployment/shutdown.md`, `docs/modules/model-deployment/status.md`, `docs/modules/model-deployment/elastic-ep.md`
      Gate: review
      Acceptance: Accept all four ordinary fields while preserving name-map identity and kind rules. Cover size bounds, resources, pool dependencies, and all command transitions. Revalidate changed dependencies without rejecting unchanged inputs due to external drift. Preserve every Elastic EP refusal. Document mixed configurations, whole-group Recreate, shared effects, merge-patch omission, and quota waits.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./api/worker/v1alpha1 ./pkg/worker/webhooks/worker`

- [x] **T3 · Recoverable whole-replica replacement**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_deployment*.go`, supporting `go.mod` and `go.sum` updates through the coordinator
      Gate: review
      Acceptance: Persist and recover one unresolved slot per affected role. Verify vacancy and old Workload absence before creation. Wait for fresh admission before another healthy deletion. Replace obsolete queued configuration within the same slot. Preserve actual member-loss recovery, untouched roles, mixed-shape Services, peer DNS, truthful status, and joint admission. Cover lost responses, restarts, partial deletion, repeated edits, and concurrent replica-count changes with observable object assertions.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags 'goccy netgo' -race -count=1 ./pkg/worker/controllers/worker`

Checkpoint: T2 and T3 must both pass before the mutable contract is ready for use.

- [x] **T4 · Integrated validation and generated contract**
      Blocked by: T2, T3
      Owns: `api/**/zz_generated.*`, `api/**/generated.pb.go`, `api/**/generated.proto`, `pkg/worker/webhooks/worker/zz_generated.*`, `pkg/kubeclients/**`, `specs/2026-10-08-model-deployment-role-mutability.md`
      Gate: review
      Acceptance: Review both Standards and Spec against the integrated diff. Resolve material defects through the owning tasks. Run local race tests, general lint, code generation, documentation lint, and compilation. Confirm a second generation leaves no artifact drift. Record runtime scenarios still awaiting user testing. Stop before PR creation.
      Verify: run the complete Commands sequence and review the generated diff.

- [x] **T5 · Workload placement pins and replacement admission**
      Blocked by: T3
      Owns: `pkg/worker/controllers/worker/model_deployment_replacement_slot.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go`
      Gate: review
      Acceptance: Accept Workload-only fit and model-manager pins. Preserve member placement constraints, template, resource, owner, count and queue checks. Demonstrate RED/GREEN regressions and compare an actual admitted slice snapshot without modifying it.
      Verify: focused admission regressions, `make test`, `make lint`, and `make build`.

- [ ] **T6 · Authorized runtime acceptance**
      Blocked by: T5
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-115.sh`, runtime evidence, affected guides and this spec
      Gate: review
      Acceptance: Include T5 in the tested image. Verify whole-card to slice, percentage changes, and slice to whole-card replacement with fresh objects and slot release. Finish the authorized test matrix, record failures and geometry limits, run MIG last, and verify owned cloud resource removal before PR preparation.
      Verify: case 115, actual engine inference, individual case receipts, documentation lint, and cleanup readback.

### Test Plan

[x] The involved component owners may require updates to existing tests before this enhancement can land.

#### Prerequisite testing updates

Add an honest Kueue composition fixture for new rollout tests.
It waits for the declared member total and consistent scheduling inputs.
Reuse the pinned Kueue group constructor when building PodSets.
Keep its required API dependencies at the versions selected by the existing Kueue pin.
Preserve role-hash grouping; member commands can differ within one role.
Give every Pod and Workload a distinct UID.
Keep existing Workload ownership and finalizers visible.
Prove the fixture rejects incomplete groups and stale reservation adoption.
Use API-reader/client separation and intercepted writes for lost-response tests.

#### Unit tests

Measured package coverage appears under Local validation.
No pre-change baseline was measured.

- `api/worker/v1alpha1`: size defaults and bounds; unchanged role list-map identity.
- `pkg/worker/webhooks/worker`: ordinary four-field edits, role reorder, invalid identity/kind, resource modes, command transitions, changed interface/transport dependencies, drift escape hatches, and Elastic restrictions.
- `pkg/worker/controllers/worker`: consistent deployed facts, legacy groups, duplicate seats, missing members, old execution, qualification, status attribution, joint admission, cache observation, peer Services, and leader/External DP selectors.
- `pkg/worker/controllers/worker`: preserved pair fingerprint tests for independent and shared Ascend changes.

Each changed contract has a repeatable regression:

| Contract | Local regression source | Live check |
|---|---|---|
| Ordinary size, resources, instanceType and command updates | [Webhook update cases](../pkg/worker/webhooks/worker/model_deployment_test.go) and [four-field lifecycle cases](../pkg/worker/controllers/worker/model_deployment_lifecycle_test.go) | Case 115; real-engine inference is checked separately |
| Initially queued resource or pool updates without a replacement slot | [Initial-queue regressions](../pkg/worker/controllers/worker/model_deployment_initial_queue_test.go) | Case 115 queued resource reduction |
| Independent P/D edits preserve the sibling | [Sibling lifecycle cases](../pkg/worker/controllers/worker/model_deployment_lifecycle_test.go) | Case 115 CPU P/D size and command checks |
| Shared rendered degrees may replace both roles | [Pair fingerprint cases](../pkg/worker/controllers/worker/model_deployment_test.go) | Shared Ascend behavior remains outside NVIDIA runtime evidence |
| Queued edits, restart recovery and fresh admission | [Lifecycle recovery cases](../pkg/worker/controllers/worker/model_deployment_lifecycle_test.go) and [admission controls](../pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go) | Case 115 checks current admission and complete group identities |
| Workload-only placement pins release an admitted replacement | [Admission pin regressions](../pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go) | Case 115 whole-card to slice, percentage changes and slice to whole-card |
| Role map identity, kind rules and Elastic EP restrictions | [API schema cases](../api/worker/v1alpha1/model_deployment_test.go), [identity cases](../pkg/worker/webhooks/worker/model_deployment_test.go) and [Elastic cases](../pkg/worker/webhooks/worker/model_deployment_elastic_test.go) | Existing admission cases remain controls; case 115 does not exercise Elastic EP |

#### Integration tests

Use fake-client reconciliation sequences for these local integration checks:

- Change size through increase, decrease, one-to-many, and many-to-one.
- Wait with a queued replacement while every other healthy old group remains.
- Replace resources and instanceType with a new Workload request and queue.
- Exercise managed-to-command, command-to-managed, and command-to-command updates.
- Restart after intent persistence, partial member deletion, old Workload deletion, and partial creation.
- Recover lost delete/create responses and an API-reader/cache disagreement.
- Edit configuration again while its replacement is queued.
- Reduce resources or change pools while a complete initial group waits without a replacement slot.
- Keep admitted siblings intact when an initial queued group changes configuration.
- Reject initial-queue classification after execution, eviction, preemption, or removal of the admission gate.
- Combine configuration changes with replica growth and shrink.
- Preserve unrelated P/D child UIDs, selectors, endpoints, and DNS.
- Replace both roles when command-supplied shared degrees alter both renders.
- Recover genuine member loss during an active configuration replacement.
- Recover all active replacement members disappearing before the next pass without losing Workload cleanup authority.
- Repair an ordinal-less legacy Pod while an active replacement holds its slot.
- Retain peer DNS when the cache omits standing or terminating members still present on the API server.
- Compare complete HTTP Service selectors against deployed answering members, including the first edit, mixed modes, qualification revocation, and Unknown observations.

These tests exercise reconciliation and objects. They do not establish runtime engine or Kueue behavior.

#### Local validation

Checks passed on implementation commit `dc81b055f77cd07ebde0382e531dacba442d0a51`.

- `make test`: all 99 packages with tests passed, with race detection, shuffled order, and coverage.
- `make lint`: passed after all writers settled.
- `make lint docs`: passed, including site links and module routing.
- `make generate`: two passes passed on the exact implementation commit in a detached generation worktree.
  All 1,468 generated file contents matched between passes and matched the implementation worktree.
  No artifact drift remained after the final source lint.
- `make build`: passed for the host target, darwin/arm64, with CGO enabled and goccy/netgo tags.
- `git diff --check`: passed.

Measured coverage: controller 84.2%; webhook 90.2%; API storage package 4.9%; repository total 25.5%.
The repository aggregate includes generated code. No pre-change baseline supports a coverage comparison.

Standards review corrections aligned Service reads and status documentation with their implemented contracts.
Spec review corrections passed regressions for legacy Pods, first-edit routing, and peer DNS during cache lag.
HTTP routing tests assert complete selectors against both qualified and revoked members.

These results establish local object behavior and build validity.
Runtime acceptance is in progress on the separately authorized temporary cluster.
Local results alone do not establish Kueue, engine, network, or inference behavior.

#### e2e tests

The user authorized the operator e2e procedure before PR creation.
The campaign covers Instance, ModelDeployment, and KVCache capabilities.
Run MIG cases after the other cases, then destroy the owned cluster.
Keep the tested source commit, image digest, individual results, and cleanup receipts in runtime evidence.

Use a deployment with at least two replicas and enough accelerators for multi-member groups.
Observe Pod and Workload UIDs, PodSet counts/templates, queue assignments, admission, and reserved resources.
Cover all size transitions, pool/resource updates, command transitions, queue contention, and interrupted cleanup.

Case 115 supplies repeatable live scheduling checks for ordinary role updates.
It covers an initially queued resource edit without a replacement slot, complete size replacements, and takeover command changes.
Logical slice capacity enables whole-card to slice, percentage changes, and slice to whole-card updates.
These legs require fresh Pods and Workloads, matching slice resources, and replacement slot release.
A second accelerator group enables its cross-pool InstanceType leg.
A CPU type enables P/D size and command updates while verifying the sibling keeps both Pod and Workload UIDs.
Each update reads the same deployment UID, fresh Pod and Workload UIDs, PodSet count and template, queue, and current admission.
Its initially queued resource leg needs two total whole cards; its size leg needs two free cards.
A one-card pool still checks command, cross-pool, and CPU P/D updates.
It asserts cleanup and reports missing geometry as a limited SKIP.
It uses placeholder commands, so engine inference remains a separate runtime check.

Local lifecycle regressions cover queued replacements across passes and restarts, a second edit, captured cleanup, simultaneous four-field updates, and sibling-role identity.
The initially queued regressions include positive replacements and execution-history guards.
Webhook tests retain role identity, kind rules, configuration validity, and Elastic EP constraints.
Verify role endpoints select answering members and old peer DNS lasts through departure.
Verify independent P/D edits preserve the sibling and shared Ascend edits roll both roles.
Exercise real joint admission and finalizer delays.
Keep a supported Elastic EP deployment as the unchanged-profile control.
Record inference interruption and shared transfer incompatibility without claiming continuous service.

#### Recorded runtime checks

The campaign remains in progress. The following checks have completed on implementation commit
`8fba3f3ac6635accd0ace52c05e0dcaa463ef740`.
The running operator image digest is
`sha256:db20543ed10da503749391602b422b8d098d40ad751342a818e3ed1fa7052f16`.
The engine fixture uses Qwen/Qwen2.5-0.5B-Instruct with vLLM 0.29.0 on NVIDIA RTX PRO 6000 Blackwell Server Edition.
Each accelerator node has one whole GPU.

| Check | Observed result | Evidence limit |
|---|---|---|
| Initially queued accelerator request changes from two to one | Same deployment UID; obsolete Pod and Workload removed; fresh group admitted; ordinary and SSE inference passed | The pool had two total cards across one-card nodes; no replacement slot existed before the edit |
| Two-replica Server size changes from one to two and back | Each selected replica was replaced completely; fresh admission, member selection and inference passed | Four accelerator nodes were available for the two-member shape |
| One-replica Server size ladder, one to two to three to two to one | Fresh groups admitted and ordinary and SSE inference passed at each step | Recreate has a serving gap; these checks do not establish continuous inference |
| Managed command to takeover, takeover to managed, and takeover argument changes | Deployed commands, group replacement and ordinary and SSE inference passed | The takeover fixture supplies its own pipeline-parallel launch; operator status remains unmanaged |
| Case 115 cross-pool InstanceType update | Fresh Workload uses the new queue; allocation UUIDs belong to the selected physical node and accelerator group | Placeholder command; no engine inference in this leg |
| Case 115 Prefill size increase, decrease and command update | Decode Pod and Workload UIDs remain unchanged while Prefill receives a fresh admitted group | CPU placeholders establish sibling identity preservation, not live P/D inference or DNS resolution |
| No-router Server replicas decrease from two to one | Retirement ended as Aborted; captured members, Workload and capacity remained | Existing retirement protection for executed replicas; this is not a successful scale-down |

Case 115 also verifies full cleanup, including Workloads whose owner Pod has already disappeared.
Its final two-pool run passed all applicable legs and cleanup.
The two one-card pools cannot run its two-card resource and size legs; those rows remain explicit SKIPs.
Earlier checks ran these legs while the larger pool was available.

Shared Ascend degree changes remain covered by local regressions only.
This hardware has no Ascend devices or native RDMA transport.
MIG profile discovery has completed; live partition allocation remains pending the final campaign stage.
Other Instance and KVCache cases do not extend the role-mutability acceptance claims above.

## Alternatives

- Remove only webhook freezes: rejected because cadence, health, retirement, and Services still assume immutable size.
- Require another deployment: rejected because it does not provide updates to the existing object.
- Resize admitted groups in place: rejected because membership and reservation belong to the old configuration.
- Add a rollout API or another controller: deferred because existing replica admission provides the required boundary.
- Use only per-pass deletion counts: rejected because queued groups can accumulate unresolved replacements across passes.
- Use private controller metadata: chosen for replacement progress that must survive an empty ordinal.
- Guarantee isolation for every edit: rejected because shared transfer configuration must remain consistent.
- Include Elastic EP replacement: deferred by the confirmed scope decision.

## Open Questions

No product-scope questions remain.
The implementation plan and local test environment are resolved.
The authorized runtime campaign is in progress. PR creation remains pending its results.
