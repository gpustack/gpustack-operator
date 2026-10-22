# Spec: DP Group Router Supervisor for ModelDeployment

Status: Building
Blocked on: implementation, local verification, and a separately approved Kubernetes acceptance run.
Type: Feature

## Summary

Add DP-group supervision to `ModelDeployment` routing. The operator and Router must treat one
DP group as one serving unit when its ranks run in separate Pods or expose separate HTTP
endpoints. A group enters the Router selection pool only after every member is healthy and the
engine-specific group path passes validation. One failed or unverified rank removes the whole
group from new traffic. Prefill and Decode groups may share one Router, but each role keeps an
independent group state.

## Motivation

### Goals

1. Protect requests from partial DP groups.
2. Make External DP safe when one rank is exposed by one Pod.
3. Apply the same group contract to vLLM External DP and SGLang P/D deployments.
4. Allow one Router to supervise both Prefill and Decode roles.
5. Restore traffic only after all members recover and a real group-level probe succeeds.
6. Report the group state and the reason for withdrawal.
7. Preserve correct ordinary, streaming, structured, English and Chinese responses.

The target users are platform operators and SREs who configure `ModelDeployment`, inspect
Router state, or respond to rank failures.

The current source already has whole-replica qualification and endpoint eligibility labels.
The relevant paths are:

- `pkg/worker/controllers/worker/model_deployment_health.go`
- `pkg/worker/controllers/worker/model_deployment_group_forward_probe.go`
- `pkg/worker/controllers/worker/model_deployment_router.go`
- `pkg/worker/controllers/worker/model_deployment_router_observation.go`
- `pkg/worker/controllers/worker/model_deployment_service.go`

The current group-forward probe supports ordinary multi-member server replicas. Prefill and
Decode group probing is currently reported as unsupported. The current routing guide also keeps
Prefill and Decode discovery at the leader member.

A prior single-node H100 investigation used vLLM `0.30.0`, SGLang `0.5.18` and Gateway
`0.3.1`. It established these facts:

- vLLM External DP exposed one HTTP endpoint per rank.
- When one vLLM rank became NotReady, both endpoints of its group left the Router selection
  set. Another replica group continued to serve.
- Normal responses, SSE responses, exact Chinese copying, arithmetic and structured output
  passed during the vLLM withdrawal window.
- A SGLang P/D Decode member became NotReady while the Decode head remained in Gateway
  selection. This allowed the Router to continue advertising a partial group.
- SGLang P/D recovery returned correct output after all Decode members recovered.
- Directed P/D probes carried request IDs and returned correct paired results.

Those observations were made against a previously built operator image on one H100 node. They
do not prove the current local HEAD, multi-node networking, or final Kubernetes behavior. The
cluster was deleted. A new cluster is a post-implementation acceptance resource.

### Non-Goals

- Do not add a new inference engine or change vLLM or SGLang internals.
- Do not make every SGLang engine-internal DP rank a public HTTP endpoint.
- Do not claim that a logical SGLang rank suffix represents a separate port or Pod.
- Do not move EP ranks between Pods when the engine requires a different topology.
- Do not add performance targets.
- Do not prove multi-host RDMA, EP networking or cloud scheduling in this spec.
- Do not restore or manage workloads outside the `ModelDeployment` under test.
- Do not implement a second Router or a new Router API.

## Proposal

A DP group is the set of Pods that form one logical serving replica. Its identity contains the
role, replica ordinal and the current sorted member UID set. A member replacement creates a new
group generation.

A group is eligible when all of the following hold:

1. The expected member set is complete.
2. Every member is `Ready=True`.
3. The engine-specific group-forward or P/D transfer probe succeeds.
4. The group is not being retired.
5. The group has no pending replacement.

The Router receives the eligible group view. It may select a rank endpoint inside a qualified
External DP group. It may select a role leader for a qualified ordinary group. It must never
select a rank from a partial group.

A Prefill group and a Decode group are separate qualification domains. One Router may supervise
both domains. A P/D request may enter the public serving path only when the selected Prefill
group and Decode group both satisfy their own barriers.

### User Stories

#### Story 1

As a platform operator, I want one failed DP rank to remove its whole group from new traffic,
so that requests do not enter a partially functioning collective.

#### Story 2

As an SRE, I want a group to remain withdrawn while only some ranks recover, so that a
restarting rank cannot receive traffic before its peers are ready.

#### Story 3

As a platform operator, I want one Router to supervise Prefill and Decode groups separately,
so that a Decode failure does not make the Prefill state look healthy.

#### Story 4

As an SRE, I want status to name the failed member, group generation and blocking condition,
so that I can distinguish a real engine fault from an unavailable observation.

## Core Features & Acceptance Criteria

### Feature 1 — External DP group contract

The operator identifies an External DP role from the role's parsed parallelism configuration.
The existing parallelism parser remains the source of truth.

Acceptance criteria:

- A role that exposes one HTTP endpoint per DP rank requires a configured Router.
- Admission or status reports a clear `RouterRequired` result when External DP has no Router.
- All rank endpoints belong to one logical group identified by role and replica ordinal.
- A qualified External DP group may publish all of its rank endpoints.
- A group with an incomplete member set publishes no rank endpoint for new traffic.
- A single rank failure removes every endpoint in that group.
- A different healthy group remains selectable.
- A Router is not required for SGLang engine-internal DP that exposes one physical listener and
  does not advertise per-rank external endpoints.

### Feature 2 — Router group supervision

The Router observation and selection contract must carry group identity. A Router may have one
process and one Service for several roles, but selection is evaluated by role and group.

Acceptance criteria:

- The Router never treats one healthy head as proof that all members of its group are healthy.
- The Router removes every endpoint in a group when any member is NotReady or otherwise known
  unhealthy.
- The Router keeps healthy groups in the selection pool while another group is withdrawn.
- Prefill and Decode groups have independent selection state.
- A P/D request is routed only through a qualified Prefill group and a qualified Decode group.
- The deployment-wide Service never round-robins requests across a Prefill process and a Decode
  process.
- The Router and ordinary Service selectors use the same group eligibility decision.
- A Router process health result is not used as a substitute for engine group health.

### Feature 3 — Recovery barrier

Recovery is fail-closed. A group remains withdrawn until its full recovery barrier passes.

Acceptance criteria:

- One recovered member does not restore any endpoint while another member is not Ready.
- All members must report `Ready=True`.
- The member UID set must match the generation used for the qualification attempt.
- A replaced member invalidates the previous qualification result.
- The engine-specific group probe must exercise the real serving path and bind its response to
  the probe request.
- A P/D probe must validate the paired transfer path. A health response from only the head is
  insufficient.
- A probe failure or unsupported probe blocks restoration and records its reason.
- A group is restored only after the probe succeeds for the complete current group.
- Restoration does not restart a healthy engine or Router.

For a member health result, `Ready=True` is the only positive answer. `Ready=False` and
`Ready=Unknown` are both unsafe for group eligibility and keep the group withdrawn. This
fail-closed rule is the proposed replacement for the current distinction that preserves prior
eligibility on every `Unknown` result.

If the operator cannot read a Router observation, it must block restoration and report an
observation-unknown state. An already eligible group keeps its current endpoint eligibility, but
its serving confirmation becomes `Unknown`. The group cannot restore after a generation change or
withdrawal until a fresh, complete observation arrives.

### Feature 4 — Status and observability

Status must describe qualification at group level. It must not collapse Router process health,
engine member health and serving confirmation into one boolean.

The observable contract must include:

- role name and replica ordinal;
- group generation derived from the sorted member UID set;
- group state, such as `Eligible`, `Withdrawn`, `AwaitingMembers`, `AwaitingReady`,
  `AwaitingProbe`, `Unsupported` or `Unknown`;
- the blocking reason;
- current member identities and readiness;
- last qualification time;
- Router serving confirmation, when a Router is configured.

The public status shape adds `status.roles[].groups[]`. Each group entry carries the role name,
replica ordinal, generation, state, reason, current member identities and readiness, last
qualification time and serving confirmation. The existing role endpoint counts remain derived
views. They must not replace the group entries.

Acceptance criteria:

- A rank failure identifies the affected group and member.
- A partial recovery reports that the group is still withdrawn.
- A probe failure is distinguishable from a missing member.
- A missing Router observation is distinguishable from an engine failure.
- Status never reports a confirmed serving count from a stale, partial or disagreeing Router view.
- Status does not claim per-rank SGLang external serving when only one physical listener exists.

### Feature 5 — Compatibility

Acceptance criteria:

- A single-rank Server deployment keeps its current behavior.
- An ordinary internal multi-rank Server group keeps leader-only selection unless its mode
  explicitly requires External DP.
- Existing qualified vLLM External DP behavior remains valid.
- Existing Router profiles continue to receive their configured role selectors.
- An engine or custom command with no verified group probe cannot be restored by Pod readiness
  alone.
- Unsupported configurations fail closed and explain the missing capability.

### Feature 6 — Content and protocol validation

The final runtime acceptance must check the response body, not only HTTP status.

Acceptance criteria:

- Ordinary non-streaming requests return the expected arithmetic result.
- SSE requests return a complete stream and the expected assembled content.
- Exact Chinese copy requests preserve every character.
- Structured JSON requests parse and match the expected object.
- The checks pass before failure injection, during unaffected-group service, and after complete
  group recovery.
- A withdrawn group receives no new request through the tested Router path.

## Notes / Constraints / Caveats

- The Router is the DP Supervisor boundary when rank Pods are externally addressable.
- The operator owns endpoint eligibility. The Router owns its native worker selection.
- Group membership must be derived from current Pod identities. A previous generation must never
  qualify a replacement group.
- The implementation must reuse the existing role and replica identity labels.
- The implementation must not invent additional ports. A new port is valid only when the engine
  and Router contract explicitly expose it.
- P/D qualification must remain role-aware. A Decode head cannot represent a complete Decode
  group.
- A `ViaRoutingProxy` pair-probe `Verified` result proves that the decode routing proxy front door
  answered bound to the operator's request ID with the exact copied content while naming the
  prefiller; it does not assert per-rank DP coverage because the proxy generates its own bootstrap
  room and discards the prefill response.
- The final cluster test must use a newly built image from the implementation branch.

## Boundaries

- **Always:** Treat a DP group as the unit of routing safety. Remove all group endpoints on a
  known member fault. Require full readiness and a real probe for restoration. Keep Prefill and
  Decode state separate.
- **Ask first:** Change the active Router image or profile. Start a new cloud test cluster.
- **Never:** Send new traffic to a partially ready group. Restore on head health alone. Treat
  a Router process health check as engine group health. Present an unverified SGLang rank as a
  public port. Use HTTP 200 without content validation as inference proof.

## Risks and Mitigations

- **Risk → A rank failure leaves a head in the selection pool.**
  **Mitigation →** Make group membership part of Router selection and remove every endpoint on
  one member failure.

- **Risk → A recovered rank receives traffic before its peers.**
  **Mitigation →** Require all members Ready and a generation-bound group probe.

- **Risk → A replacement inherits an old qualification result.**
  **Mitigation →** Derive the generation from the sorted current UID set.

- **Risk → P/D health is represented by one head.**
  **Mitigation →** Qualify Prefill and Decode groups independently and require a valid pair.

- **Risk → The probe returns success without testing the real path.**
  **Mitigation →** Bind the response to an operator-generated request ID and use an engine-specific
  P/D transfer probe.

- **Risk → Router observation loss causes a false serving claim.**
  **Mitigation →** Report `Unknown`, block restoration and keep confirmed serving counts absent.

## Design Details

### Commands

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker ./pkg/worker/webhooks/worker
make generate
make lint
make lint docs
make test
```

Run these commands in the local checkout on the development host. The focused Go command is the
inner loop. Run `make generate` when API or webhook sources change. Run `make lint` only after the
implementation edits are complete because the target can rewrite Go files. Run the full test and
documentation checks before implementation review. The later Kubernetes acceptance runs against a
newly built image through the approved `kubectl` context. It is not part of this local plan.

### Project Structure

- `api/worker/v1alpha1/` — public `ModelDeployment` and status types.
- `pkg/worker/controllers/worker/` — qualification, Router observation, Service selectors and
  reconciliation.
- `pkg/worker/webhooks/worker/` — configuration validation.
- `pack/llm-router/` — Router image and source patches.
- `docs/modules/model-deployment/` — routing, P/D and External DP contracts.
- `testing/infra/` — final Kubernetes validation infrastructure.

### Code Style

Keep the qualification predicate explicit and group-scoped:

```go
func (q modelDeploymentInstanceQualification) Eligible() bool {
    for _, leg := range q.Legs {
        if leg.Verdict != modelDeploymentLegVerified {
            return false
        }
    }
    return true
}
```

Use short functions with one responsibility. Keep health, Router observation and status
projection separate. Use typed states and explicit reasons. Do not use a panic for a health
decision.

### Implementation Plan

- [x] **T1 · Validate the P/D group probe path**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_group_forward_probe.go`, `pkg/worker/controllers/worker/model_deployment_group_forward_probe_test.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`
      Gate: review
      Acceptance: The probe has an explicit result for ordinary groups and for Prefill/Decode groups. A supported P/D probe binds the response to an operator-generated request ID and validates the paired transfer path. An unsupported shape returns a typed fail-closed reason without claiming health.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker -run 'GroupForwardProbe|PD.*Probe'`

- [ ] **T2 · Add the group qualification state machine**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_deployment_health.go`, `pkg/worker/controllers/worker/model_deployment_health_test.go`, `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_test.go`
      Gate: review
      Acceptance: Qualification groups members by role and replica ordinal, derives a generation from sorted member UIDs, requires a complete set and `Ready=True` for every member, and requires a verified T1 result (`NotApplicable` for a one-member ordinary group and `Verified` for a probed group; a Prefill or Decode role never reads `NotApplicable` because its serving path is a pair, and T2 wires the T1 pair probe for it). `Ready=False`, `Ready=Unknown`, missing members, replacement UIDs, retirement and probe failure keep the group withdrawn. A complete current group can become eligible without restarting healthy engines. The endpoint-label writer `modelDeploymentPodEligible` in `model_deployment.go` withdraws on the new fail-closed member result too; it no longer preserves the carried eligibility label for an unactivated group whose member is `Ready=Unknown`. Its signature stays stable because `model_deployment_router_test.go` and `model_deployment_service_test.go` call it.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker -run 'Qualification|Group.*Health|Recovery'`

- [ ] **T3 · Project group eligibility to endpoints and Services**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_deployment_service.go`, `pkg/worker/controllers/worker/model_deployment_service_test.go`
      Acceptance: External DP publishes every rank endpoint only for a qualified group. Any known unsafe member removes every endpoint in that group while healthy groups remain selectable. Ordinary groups keep leader-only selection. Prefill and Decode use independent eligibility views, and the deployment-wide Service cannot round-robin across role processes. `modelDeploymentGroupForwardActivatable` no longer excludes Prefill and Decode roles once T1 reports a supported or typed fail-closed P/D result.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker -run 'Service|Endpoint|Selector'`

- [ ] **T4 · Make Router observation and selection group-aware**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_router_observation.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`, `pkg/worker/controllers/worker/model_deployment_router_observation_test.go`
      Gate: review
      Acceptance: Router observations carry role, group generation and member identity. Selection never uses one healthy head as proof of a group. A missing, stale or disagreeing observation marks serving `Unknown` and blocks restoration; an already eligible group keeps its current endpoint eligibility until the observation is fresh again. Prefill and Decode are supervised by one Router when configured, but their group state remains independent.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker -run 'Router|Observation|Selection'`

- [ ] **T5 · Enforce External DP admission and publish group status**
      Blocked by: T2
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_deployment_test.go`, `pkg/worker/webhooks/worker/model_deployment*.go`, `pkg/worker/webhooks/worker/model_deployment*_test.go`, `pkg/worker/controllers/worker/model_deployment_status.go`, `pkg/worker/controllers/worker/model_deployment_status_test.go`
      Gate: review
      Acceptance: External DP without a Router is rejected with `RouterRequired`; internal engine DP with one physical listener remains valid. `status.roles[].groups[]` reports generation, member readiness, state, reason, qualification time and serving confirmation. Status distinguishes incomplete members, unsafe readiness, probe failure, Router observation loss and confirmed serving. Generated API artifacts stay synchronized.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./api/worker/v1alpha1 ./pkg/worker/webhooks/worker ./pkg/worker/controllers/worker -run 'ModelDeployment.*(Status|Router|Parallelism|Group)' && make generate`

- [ ] **T6 · Update contracts, content checks and cross-component tests**
      Blocked by: T3, T4, T5
      Owns: `docs/modules/model-deployment/routing.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/README.md`, `testing/infra/**`, `pkg/worker/controllers/worker/model_deployment_group_contract_test.go`, `pkg/worker/controllers/worker/model_deployment_content_test.go`
      Acceptance: Documentation describes one Router as the DP Supervisor boundary for externally addressable rank Pods, the fail-closed group barrier and the P/D role boundary. Cross-component tests cover rank failure, partial recovery, UID replacement, Router observation loss and unaffected-group service. Protocol tests validate ordinary, SSE, exact Chinese and structured JSON responses before failure, during unaffected-group service and after complete recovery.
      Verify: `make lint docs && GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker ./pkg/worker/webhooks/worker`

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Add shared fixtures for a role replica ordinal, a complete member UID set, readiness observations,
group generations and Router observations. Add deterministic P/D probe fixtures that carry a request
ID and paired transfer result. Add response helpers that compare assembled SSE text, exact Chinese
text and parsed JSON objects. Keep the fixtures local to the existing worker and webhook test
packages.

#### Unit tests

- `pkg/worker/controllers/worker`: 2026-10-22 - cover every new group state transition, endpoint
  projection branch, Router observation verdict and P/D probe verdict.
- `pkg/worker/webhooks/worker`: 2026-10-22 - cover External DP `RouterRequired`, internal DP
  compatibility and unsupported group shapes.
- `api/worker/v1alpha1`: 2026-10-22 - cover JSON, protobuf and deepcopy round trips for
  `status.roles[].groups[]`, including absent, `Unknown` and confirmed serving states.

#### Integration tests

- A two-rank External DP group loses every endpoint when one rank becomes NotReady. A separate
  healthy group remains selectable.
- One rank recovers while its peer remains unsafe. The group stays withdrawn.
- A replacement rank changes the sorted UID generation. The old qualification does not restore it.
- A complete group recovers and passes the real probe. All endpoints return only after the probe.
- Prefill and Decode fail independently. A P/D request requires one qualified group in each role.
- A missing, stale or disagreeing Router observation reports `Unknown` and blocks restoration while
  preserving current eligibility for an already serving group.
- Response checks cover non-streaming arithmetic, complete SSE assembly, exact Chinese copying and
  structured JSON parsing across the withdrawal and recovery sequence.

#### e2e tests

Defer Kubernetes acceptance until implementation is complete and a new cluster is approved. Use a
newly built implementation image and one Pod per externally addressable rank where the engine
supports that shape. Verify Router and Service endpoint convergence, rank failure withdrawal,
partial recovery, UID replacement and full recovery. Run the content and protocol checks through
the tested Router path. Record unsupported SGLang per-rank external serving as a skipped capability,
not as a pass. A local unit or integration run cannot prove multi-host networking, EP transport or
cloud scheduling.

## Alternatives

### Route each DP rank as an independent replica

Rejected. A rank can be reachable while its collective is incomplete. Independent routing can
send a request into a group that cannot finish the request.

### Route only the head Pod

Insufficient. The head can remain healthy while a non-head rank is NotReady. The SGLang P/D
investigation reproduced this state.

### Put every DP rank in one Pod

Out of scope. EP and engine topology can require different placement. The Router contract must
work when the group spans Pods.

### Use a Service without a Router

Rejected for External DP. A Service can distribute endpoints but cannot enforce the group
failure and recovery barrier.

### Run one Router per role

Not required. One Router can supervise both roles when its worker registry carries role and group
identity. Separate Routers remain possible when an operator chooses them.

## Open Questions

1. **SGLang per-rank external HTTP serving.**
   This spec supports SGLang group supervision for the currently validated P/D and
   engine-internal-DP shapes. Enabling one public port per SGLang DP rank requires a separate
   engine and Gateway validation.

2. **Final probe protocol for each supported engine version.**
   The implementation must pin the supported engine and Router versions before coding the
   engine-specific probe.
   Resolution for the first build: one unified pair-probe driver with a per-engine request builder
   for SGLang and vLLM. The SGLang shape follows the measured directed probe (shared request ID,
   per-leg DP rank, bootstrap room parity equal to the Prefill rank, every rank pair, exact-copy
   content check). The vLLM shape follows the `MooncakeConnector` source protocol. Anything not
   measured is recorded as source-supported only. The final cluster test converges the shape.
