# Spec: ModelDeployment S2 — Elastic EP Resize (vLLM Ray): Four-Layer Elastic Realization, Engine Resize Lifecycle, and Continuity/Observation Separation

Status: Shipped
Type: Feature
Owning program: dp-ep-router-and-scaling (S2 formal stage, D11-delegated)

## Delivery and remaining validation

PR #714 merged the S2 source, the managed Router image default (llm-router v0.2.0) and the owning guides.
All implementation tasks below are complete.
Whole-source lint, generation without drift and affected-package race tests pass on the merged head.
Four effective review rounds are consumed; every actionable finding is fixed or declined with evidence in the PR threads.

Physical acceptance ran in a chartered 4xH100 window against the earlier bidirectional source
surface. That evidence remains historical; the current API rejects Elastic EP scale-down until
issue #741 is resolved.

Physical acceptance evidence:

- E7 (S10): two consecutive product `2→4→2` campaigns PASS, 421s wall for the repeated campaign,
  expansion to four servers 1/1, narrowing retiring to exactly the original survivors;
  bookend captures (Pods, Devices, Workloads, GCS, shared memory, forwards, allocation ledger)
  are clean before and after.
- E9: whole-GPU admission, identity-captured retirement and observed GPU/Kueue release are
  established by those bookends.
- E10 funded cells: an external engine-Pod delete reconverges immediately — the replacement Pod
  is observed at 0.0s and the terminal Pod's finalizer and Workload self-heal in 9.1s (the #719
  live regression); kubelet device-plugin checkpoint loss leaves the annotation allocated,
  never publishes an occupied card as free, and converges node allocatable byte-identical to
  its sibling node (the #718 live regression, completed manually after a harness driver defect).

Explicit gaps the shipped source does not establish: all-three-Router composition (S9),
resize-under-load performance (S11) and ≥20-cycle repeatability. S8 now has a measured
W10 PASS-5 result rather than an unrun result: the 2→4 leg passed, but the 4→2 patch
failed with `503 ServiceUnavailable` while the operator extension APIService restarted
after the scheduled operator-pod deletion. The independent survivor completed 10/10
first attempts, with a maximum 10.119-second gap against the 30-second candidate budget.
The S8 retry was not run because recovery left a `Recorded`, `commandSent=false` operation
holding the deleted member identities; rebuilding the primary Pods did not clear it. The
operation remained held with `ElasticResize=NoRefusal` and the observation reason
`a captured member identity no longer matches; the operation holds`. Therefore S8 is a
banked **FAIL**, not a continuity pass. S9, S11, R20 and the post-bookend remain unrun.
The same PASS-5 engine-log review found no #736 `KeyError`, `exc_info` or traceback
signature. These observations do not establish the unrun cells or turn the control-plane
503 into an engine refusal.
Issues #713, #718 and #719 close with this delivery.
Issues #715, #716 and #717 were bounded follow-ups outside it and are now closed.

## Summary

Elastic expert-parallel width changes inside one vLLM Ray serving instance are a different resource
problem from S1's fixed-instance scaling: the engine itself reconfigures a collective (prepare →
drain → commit) while Kubernetes, Kueue, Ray, and the accelerator ledger must stay reconciled
around it. This spec defines that capability in four features: (1) the elastic workload
realization — one ModelDeployment instance as one dedicated logical Ray cluster with a CPU head,
one fixed TP group per worker Pod, and a four-layer reconciliation (desired spec, admitted Pods, Ray members, engine-effective
world) where scale-down retires only captured, runtime-proven actor-free Pod identities; (2) the
engine resize lifecycle with its failure and recovery contract — explicit orderings in both
directions, the resize 503 window as a measured outage to be quantified, probe coordination, a
client-side request ledger because engine metrics reset across a resize, and no blind replay after
timeout or commit failure; (3) the separation of single-instance interruption observation from
whole-service continuity claims, which require an independent surviving instance; and (4) state
observability reporting desired / admitted / Ray / effective as distinct fields where Unknown is an
explicit value. Every load-bearing engine or hardware claim below is graded: **READ** (pinned
source or program analysis), **MEASURED** (observed in a program PoC activity, described in words),
or **NOT-established** (open risk carried deliberately). The current shipped API's scale-down
physical gate — vLLM Ray 2→4→2 with real EP inference and release verification against this API
surface — has not run. The earlier bidirectional evidence listed above is historical and does not
validate scale-down for the current API. This document encodes the design so it can be reviewed
and gated, and freezes nothing that evidence has not earned. Where this spec
describes program-local observations, the substance a reader needs is stated in the sentence or in
the "Evidence grades and measurement limits" appendix; program evidence itself is retained
program-side and is intentionally not linked here.

The current shipped API deliberately narrows this design surface: Elastic EP updates may keep the
current width or increase it, while a decrease is rejected by ModelDeployment admission. The
scale-down lifecycle below remains the design and follow-up scope for [issue #741](https://github.com/gpustack/gpustack-operator/issues/741), not a current user-visible capability.

**Program labels used in this document** (each defined by this document's own content, referencing
nothing external): E7, E9, and E10 are the program's three gate checks this spec is blocked on —
E7: a native vLLM Ray 2→4→2 elastic-EP resize with real EP inference and release verification;
E9: runtime four-layer resource accounting (desired/admitted/Ray/effective) with identity-captured
retirement and observed release; E10: failure/recovery measurement over the AC-2.1 cell list.
T06 is the S1 role-scale adapter workstream, which must never write elastic width. T07 and T08
are the program tasks corresponding to this spec's Feature 1 (elastic resource realization and
admission) and Feature 2 (engine resize lifecycle integration). T09 is the SGLang merged scale-up
research branch, a separate spec. E8 is the External Elastic EP research track (upstream PR
#43202). D07 is the pending root ruling on whole-GPU versus sliced/partitioned allocation scope.
SC01 and SC03 are the program's acceptance scenario requirements — net SLO-capacity gain, and the
separation of continuity claims from instance-interruption characterization.

## Authorization and remaining acceptance gates

1. **E7/E9/E10 native physical gate: RUN in a chartered 4xH100 cloud window.** E7 —
   **HISTORICAL MEASURED** PASS on the earlier bidirectional source: two consecutive native vLLM
   Ray `2→4→2` campaigns with real rank forwards and identity-bound survivors; E9 —
   **HISTORICAL MEASURED** PASS via the campaign bookend ledgers (whole-GPU admission,
   identity-captured retirement, observed release); E10 — funded cells **HISTORICAL MEASURED**
   PASS (external Pod delete, kubelet checkpoint loss). The ≥20-cycle
   repeatability candidate and the S9/S11 cells are not established and stay explicit gaps.
   S8 is a measured banked FAIL from PASS-5: its 4→2 command raced the operator
   extension APIService restart, and the subsequent stale-member operation held without
   a corrective drive. The independent survivor stayed healthy, but that does not make
   the resizing instance's control-plane recovery a pass.
   Earlier failed fixture attempts remain recorded in the program's PoC disposition; no section
   of this spec may be read as claiming the unestablished cells.
2. **Source implementation is authorized.** The program owner approved proceeding with S2
   implementation after opening the S1 PR, carrying remaining S1 tests and validation into S2.
   This permits offline planning and implementation before the physical gate; it does not mark
   any physical criterion as passed or freeze an unsupported public API.
3. **Physical acceptance ran under the program's chartered-window discipline.** Independent
   cleanup/watchdog checks, budget and identity safeguards applied; teardown verified typed
   absence, byte-equal foreign baselines and zero retained spend. Candidate thresholds remain
   candidates until the unfunded cells run.
4. Cross-spec prerequisite: the serving-side contracts this spec composes (reversible endpoint
   eligibility, per-Router serving confirmation, whole-instance health gate) are the S1 spec's
   deliverables ([S1 spec](2026-10-01-s1-router-qualification-and-drain.md), included in
   this branch's committed ancestry). **READ** — this spec depends on them; it does not redefine them.

## Motivation

### Goals

1. **Reconcile four distinct states, not two.** A resize is only as safe as the agreement between
   what the spec declares, what Kueue admitted, what Ray actually runs, and what the engine
   reports as effective — each observable separately, none inferable from another (**READ**: the
   program's elastic-EP design analysis; the S2 formal-stage charter).
2. **Retire by identity, never by count.** A scale-down deletes exactly the captured worker Pod
   identities proven actor-free at runtime, and never the API/DP-master member (**READ**: the
   program's design analysis of direct RayCluster worker-count shrinking — lowering the count
   alone can delete Pods that still hold actors — cross-checked against the KubeRay v1.7.1
   retirement interface at source).
3. **Quantify the interruption instead of assuming it away.** The all-endpoint 503 window during a
   vLLM elastic resize is a measured outage to be characterized in the native campaign, with probe
   coordination so a healthy reconfiguration is not killed as liveness failure (**READ**: pinned
   vLLM v0.29.0 middleware behavior; the program's continuity scenario requirement SC03).
4. **Keep serving claims honest.** A 503 on the resizing instance is an observed interruption;
   continuity is a property only an independent surviving instance can demonstrate, verified on
   first-attempt outcomes with retries recorded separately (**READ**: the program's acceptance-
   measurement analysis, including its first-attempt discipline; scenario requirement SC03).
5. **Recover without guessing.** After timeout, commit failure, or controller restart, the
   controller reconstructs native state from desired spec + operation generation + effective +
   actual allocation observations, and refuses blind replay (**READ**: the S2 formal-stage
   charter; the program's read-path failure analysis of the pinned engine's commit path).
6. **Freeze only evidence-earned surface.** Public API changes follow the S1 precedent — semantics
   here, wire frozen at the S2 my-plan gate; the observational effective-width field is explicitly
   not a frozen API commitment.

### Non-Goals

- **Current API scale-down.** The shipped ModelDeployment API accepts Elastic EP width increases
  and unchanged values only. A decrease is rejected at admission; safe scale-down remains the
  follow-up tracked by [issue #741](https://github.com/gpustack/gpustack-operator/issues/741).
- **SGLang elastic scale-down.** Pinned SGLang v0.5.18 supports merged scale-up only; scale-down
  has no merged contract and is never claimed here or anywhere in this program (**READ**: the
  pinned SGLang v0.5.18 upstream — merged scale-up only; the S2 formal-stage charter). SGLang
  merged scale-up remains an equally required separate research branch (T09, its own spec); this
  spec covers the vLLM Ray engine only.
- **E8 External Elastic PR (#43202).** Optional isolated research at a fixed SHA; formal support
  never depends on it (**READ**: the program standard's external-research rule — optional, fixed
  SHA, isolated, never a support dependency).
- **No dependency on T06.** Elastic width is not driven by, and cannot map into, the S1 role scale
  adapter; no autoscaler writes the Ray worker dimension (**READ**: the program's design analysis
  of the S1 role-scale adapter versus native width).
- **`roles[].size` immutability preserved.** The webhook freezes instance shape at create time
  (**READ**: `pkg/worker/webhooks/worker/model_deployment.go:661-670`,
  `modelDeploymentReplicaSizeFrozenMessage`); elastic width is realized in the Ray worker
  dimension of the elastic profile, never by editing a fixed profile's size (accepted program
  direction: two initial profiles — fixed External DP rank endpoints and a Ray Elastic
  single-instance entry — with size immutable).
- **Whole-GPU allocation only** until ruling D07 says otherwise; whole-GPU pass does not imply
  sliced/partitioned pass (**READ**: the program's resource-accounting analysis; the S2
  formal-stage charter).
- **No cloud node-provisioning elasticity.** Pre-provisioning physical nodes is capacity
  preparation, not an admission or resize event, and is never counted as elastic scaling
  (**READ**: the program's native-schedule option analysis, which explicitly distinguishes
  pre-existing physical capacity from allocated width and engine width and retains node
  provisioning 2→4 as a separate, deferred case).
- **No new engine flags, no synthetic identity labels in engine metrics, no engine-side patches**
  to the pinned vLLM v0.29.0; every engine capability claim is bound to the pinned version.

## Source contracts this composes (READ, repository source and pinned upstream)

- **Pins.** vLLM v0.29.0, commit `98dff2a81d747d1dba01a47f939f48c3526d4206` (three Ray-recipe
  files byte-verified against the v0.29.0 Git object in the program's source-provenance review:
  the Ray recipes in `pkg/parallel/parallel.py`, the Elastic EP API router, and the native serving
  benchmark). SGLang v0.5.18 commit `71de97b264b04dcd514cf904003028aefe9775c8` (referenced for
  Non-Goals only). Kueue: packaged v0.18.10, Go dependency v0.17.1 — versions must not be mixed
  when proving runtime capability (the program's resource-accounting analysis). The operator owns
  the Ray head and worker Pods directly.
  KubeRay v1.7.1 was evaluated and rejected because its deletion paths do not carry captured
  Pod UID preconditions. No KubeRay installation is required by this profile.
- **Engine mode constraints.** vLLM v0.29.0 Elastic EP requires Internal mode, Ray backend, EPLB,
  PP=1, and collapses API server count >1 to 1; External/Hybrid are rejected (**READ**: pinned
  vLLM v0.29.0 source).
- **Elastic EP API surface.** `POST /scale_elastic_ep` with body
  `{"new_data_parallel_size":N,"drain_timeout":T}`; `drain_timeout` 120 is the upstream default
  candidate, not an availability SLO; `VLLM_ELASTIC_EP_DRAIN_REQUESTS=1` is set explicitly
  (**READ**: pinned vLLM v0.29.0 source and its Elastic test recipes). `/is_scaling_elastic_ep` is
  actually a POST returning `{"is_scaling_elastic_ep":bool}` (`api_router.py:83-85` in v0.29.0) —
  a boolean status flag only; `/server_info` is a dev configuration readout.
- **Failure-path findings (source read, not yet reproduced).** Prepare executes before drain; the
  drain timeout only clears the local pause flag in the read path; the scaling boolean has no
  unified finally-reset on commit exception in the read path; scale-up rebuilds the stats logger
  and the code notes a Prometheus metrics reset; the v0.29.0 middleware returns 503 on all HTTP
  endpoints (including health/metrics/status) while the scaling flag is set; the current
  operator's 10s×6 liveness would restart a normally-reconfiguring API container after roughly a
  minute of continuous failure (**READ**: pinned vLLM v0.29.0 read-path source, plus failure
  scenarios inferred in the program's analysis; reproduction is E10 work).
- **Four-layer accounting questions (E9).** The program's four-layer accounting analysis asks:
  (1) workload owner's PodTemplate requests/identity/queue label; (2) effective Kueue Workload /
  WorkloadSlice PodSet requests and flavor; (3) actual Pods after webhook — requests, gating,
  node, assignment; (4) the Devices ledger versus InstanceType occupancy and release. Kueue
  slicing is implemented as annotated ordinary `Workload` objects, not a separate WorkloadSlice
  CRD (**READ**: upstream Kueue v0.18.10 controller/slicing sources).
- **Allocation identity.** GPUStack writes per-container `devices`/`deviceIDs` under
  `device.gpustack.ai/accelerator.allocated`; `AllocatedAcceleratorsOf` reads it; the node ledger
  rebuild retains terminating holders, so a deleted/terminating Pod is not by itself proof of GPU
  release (**READ**: GPUStack's accelerator-ledger source, reviewed in the program's source-
  review activity). Accounting units: `ResourceMaxUnits=1,600,000` is the common per-accelerator
  basis; `SlicedResourceMaxSize=512` bounds partition count, not units per card (**READ**:
  GPUStack accounting source, same review).
- **KubeRay retirement surface.** `ScaleStrategy.WorkersToDelete` selects Pod names before the
  replica-count fallback; multi-host handling expands a selected name to its replica group;
  lowering only replicas can select arbitrary groups — the targeted API's existence does not prove
  safe ordering (**READ**: KubeRay v1.7.1 controller source).
- **Serving-side prerequisites.** Reversible eligibility, per-Router serving confirmation with
  deduplicated-union counting and Unknown/NotConfigured/confirmed-zero states, the whole-instance
  health gate, and the abortable retirement reservation are defined by the S1 spec and composed
  here unchanged (**READ**: the S1 spec document named in "Blocked on", same base HEAD).

## Proposal

### Elastic request contract

The managed profile adds optional `roles[].elasticEp` with one required field:

| Field | Meaning | Update rule |
| --- | --- | --- |
| `width` | Total GPU engines, including the reserved API/DP-master; integer from 2 through 64. | Mutable. |

Profile presence is immutable. The deployment has one managed vLLM Server role, with
`replicas=1` and `size=1`. Those fields retain their existing meanings. This profile
supports the pinned vLLM release with PP=1 and prefill context parallel size one.
TP is read from `extraArgs` and stays fixed while DP width changes.
An explicit `--elastic-ep-max-dp-size` in `extraArgs` bounds the target width on create and update.
The effective maximum is fixed at creation; adding, removing or changing it requires a new deployment.
Equivalent argument forms are accepted. The operator does not add a duplicate API field or inject the flag.
The argument is available starting with vLLM v0.30.0:
[CLI definition](https://github.com/vllm-project/vllm/blob/ced6857afa0ea7b2e3f0846a62e1394e90f15607/vllm/engine/arg_utils.py#L1213-L1216).
Omission preserves legacy admission. In vLLM v0.31.0, the omitted maximum equals initial DP,
so deployments using that version must set a larger maximum before startup to expand online.
This version contract is source-checked, not GPU-verified:
[configuration](https://github.com/vllm-project/vllm/blob/db9527a46873454610df6dbedf79a36d6bf1a7f6/vllm/config/parallel.py#L971-L981),
[resize admission](https://github.com/vllm-project/vllm/blob/db9527a46873454610df6dbedf79a36d6bf1a7f6/vllm/v1/engine/core_client.py#L1746-L1762).
Each member requests the whole GPU count of its TP group; omitted TP defaults to one.
Command takeover, slicing, partitioning and fabric interface requests are refused.
Arguments or environment values that override operator-owned elastic settings are refused.

The head is a directly managed auxiliary Pod with no InstanceType or LocalQueue dependency.
Its template declares no container or Pod resource requests or limits, including CPU, memory and GPU.
Namespace admission can inject defaults; the controller does not add them.
It consumes no GPU width. Only the reserved master supplies API endpoints and serving status.
Initial boot width is retained separately from the mutable target. A target or deployment
annotation change cannot replace a serving master. A smaller target alone cannot delete workers.
This request contract does not freeze a public effective-width status field or prove native support.


### Feature 1 — Elastic workload realization with four-layer resource reconciliation (E9 / T07)

**Shape.** One ModelDeployment instance maps to one dedicated logical Ray cluster. The operator
creates and owns its head and worker Pods. The Ray head is CPU-only
and carries only the Ray control plane. It starts with `--num-gpus=0`.
GPU master and worker commands omit `--num-gpus` so Ray discovers container-visible devices.
GPU requests, device allocation and TP remain unchanged.
The existing observer rejects any live GPU Ray node whose logical GPU capacity differs from TP.
Logical Ray capacity does not prove actual device allocation or Pod identity.
Auto-discovery must also be verified in the pinned runtime; no slicing or multi-vendor support is inferred. Each worker Pod holds one complete TP group with
`numOfHosts=1`. The vLLM API/DP-master role is pinned to a reserved GPU worker Pod with explicit
`data_parallel_size_local=1` in Internal mode (strict placement requires a positive local size on
a GPU-bearing node; a CPU head hosting the API master with strict placement and `local=0` allocates
zero ranks elsewhere and is kept as a configuration negative, not a profile — **READ**: the
program's elastic placement analysis, consistent with pinned vLLM v0.29.0 strict-placement rules).
The API/DP-master worker Pod is never in the retirement set; it retires only with the whole
instance. Single writer per resource: exactly one resize controller (Feature 2) writes the Ray
worker dimension; Kueue, T06 and any autoscaler are non-writers of width.

**Four layers, reconciled as distinct states.** The controller reconciles and reports four states
separately, and an engine-level action is permitted only when the layers below it agree:
(1) **desired** — the declared elastic width in the instance spec; (2) **admitted** — GPU worker
Pods actually admitted and allocated through Kueue (PodTemplate → Workload/PodSet → webhook →
node/assignment); (3) **Ray** — workers actually registered in the Ray cluster (nodes/actors);
(4) **effective** — the engine's effective DP world, including the rank/actor/Pod identity
mapping. Admission completes before engine-level actions: scale-up applies new GPU capacity only
after Kueue admission, real allocation, and Ray registration are observed; scale-up into
unadmitted capacity is forbidden (**READ**: the program's E9 four-layer design).

**Scale-down retires only captured, actor-free identities.** A retirement candidate must be (a)
captured by identity — Pod name + UID recorded before the operation, deleted by the
operator with a UID precondition; (b) proven actor-free at runtime after the engine commit
(the actor→Pod mapping is read,
not assumed); and (c) not the API/DP-master member. "Worker count now equals the target" is an
observation, never a retirement condition. A captured identity that still holds actors at
retirement time blocks the retirement and is reported.

**Resource accounting closure.** Every retirement is followed by observed release — the
accelerator ledger and Kueue quota convergence are read from current state; because the node
ledger retains terminating holders, deletion events alone do not close the release (**READ**:
GPUStack's accelerator-ledger source). Quota insufficiency at scale-up holds the instance at its
old width with the old group serving; desired ≠ admitted is reported state, never an error loop.
No existing fixed-shape Workload opportunistically gains Pods: the elastic profile is its own
workload realization, and the PoC's direct GPU head+API Jobs are explicitly not proof of a
KubeRay/Kueue realization — the implementation must name its actual controller/framework/version
and derive all four layers from created objects (**READ**: the S2 formal-stage charter;
**MEASURED** in the program's fixed-DP baseline PoC activity: two ordinary-DP whole-GPU Workloads
admitted with 1,600k accounting credits each, actual Pod UIDs and one GPU UUID captured —
whole-GPU admission only; Ray/elastic realization **NOT-established**).

**Acceptance.**

- AC-1.1 Scale-up 2→4: all four layers observed agreeing at the new width, with the
  rank/actor/Pod identity map recorded; each layer's transition is independently observable
  (admitted before Ray-registered before effective).
- AC-1.2 No engine resize action starts on unadmitted capacity: with Kueue admission withheld,
  desired changes and admitted/Ray/effective stay at the old width; the old group keeps serving.
- AC-1.3 Scale-down retires exactly the captured identity set: each deleted Pod matches a captured
  name+UID precondition; a candidate still holding actors blocks retirement with a readable
  reason; the API/DP-master Pod is never a candidate.
- AC-1.4 After retirement, observed release closes: accelerator ledger and quota converge to the
  post-retirement baseline within the pre-registered candidate window (5 minutes — **candidate
  threshold** from the program's pre-registered candidate advice; not an accepted SLO), and
  deletion events without ledger convergence do not count as release.
- AC-1.5 Controller restart re-derives all four layers from current objects (PodTemplate,
  Workload/PodSet, Pods, Devices ledger, Ray API, engine observation) with no in-memory state and
  no duplicate width change.
- AC-1.6 The realization names its controller/framework/version in configuration, and the four
  layers are derived from objects that realization created (negative: a fixture that only counts
  Pods cannot satisfy any four-layer check).

**Shared memory is a realization prerequisite.**
**MEASURED** — the pinned native profile failed before inference: its `/dev/shm` buffer required 160 MiB, with 64 MiB available.
Ordinary role rendering provides each Pod with a memory-backed `EmptyDir` at `/dev/shm`.
`roles[].shmSize` is an optional positive Kubernetes quantity. Omission renders `16Gi`.
Ordinary roles, take-over roles, Elastic GPU members and the auxiliary CPU head share this volume policy.
The volume enters the Pod spec hash. Its capacity does not reserve RAM or raise the memory request.
Only the main container mounts it; used shared memory counts toward that container's memory limit.
An explicit `/dev/shm` mount takes precedence; its capacity and backing remain the user's responsibility.
An ordinary capacity change follows recreate rollout. Existing Elastic members remain unchanged;
new or replacement members use the current role value. No host path is introduced.
The default replaces the earlier fixed 512 MiB member volume. It is a project choice, not an upstream engine guarantee.
**NOT-established** — full engine sufficiency and the dynamic TP>1 failure's root cause still need physical validation.
No native resize pass is claimed.

### Feature 2 — Engine resize lifecycle with failure and recovery contract (E7 / E10 / T08)

**One authoritative resize controller.** Exactly one controller owns a resize operation, bound to
the ModelDeployment UID, the observed `metadata.generation`, the target width, and the captured
member/allocation identities. A second conflicting operation — including a repeated request with
a different target — is refused while an operation is unresolved. Operation generation, not wall
time, distinguishes a stale from a current instruction.

**Orderings (explicit, both directions).**
- *Scale-up:* Kueue admission → real allocation → Ray registration → engine prepare (new capacity
  prepared before the native scale-up) → commit → restore entry/qualification for the instance.
  Newly admitted capacity is prepared before the engine commits it into the collective (**READ**:
  the program's design analysis; the S2 formal-stage charter).
- *Scale-down:* withdraw the instance's traffic eligibility first (S1 eligibility + serving
  confirmation contracts), preserve the whole serving process group (all members of the group stay
  up while withdrawn/reconfiguring), then engine prepare → drain → commit at the lower width,
  verify effective width AND a real forward at the new width, and only then retire the captured
  actor-free identities (Feature 1) and release quota. Engine prepare runs before drain; the
  retire step never precedes commit (**READ**: the program's design analysis; the S2 formal-stage
  charter).
- Entry restoration after a scale-up happens only after the new target is effective — never
  during prepare/drain.

**The 503 window is a measured outage.** While the scaling flag is set, v0.29.0 returns 503 on all
HTTP endpoints including health/metrics/status (**READ**: pinned vLLM v0.29.0 middleware source).
The window is instrumented and quantified per operation phase (withdraw → prepare → drain →
commit → restore) in the native campaign; it is never assumed away, averaged away, or described
as "no downtime". Probe coordination is part of the contract: readiness/traffic qualification is
distinct from health, and an all-503 reconfiguration must not be treated as liveness death — the
current operator liveness of 10-second probes with a 6-failure threshold would restart a healthy
reconfiguring container (**READ**: the program's probe-budget analysis; exact trigger timing
**NOT-established**, E10 measurement).

**Metrics reset → client-side ledger.** A scale-up rebuilds the engine stats logger and resets
Prometheus counters (**READ**: pinned vLLM v0.29.0 source). Long-window goodput, failure, and
continuity accounting therefore come from the independent first-attempt client request ledger —
the instrument whose per-attempt completeness and SLO-capacity accounting was RED/GREEN verified
in the program's instrument-review activity, with the accounting cohort including the drain tail
(**MEASURED**) — never from engine cumulative gauges/counter deltas across a resize.

**Drain timeout and commit failure: no blind replay.** In the read path, prepare runs before
drain and the drain timeout only clears the local pause flag; leftover prepared state, new actors,
and the refusal behavior toward a different second target are unknown until run and are explicit
E10 cells (**READ**: pinned vLLM v0.29.0 read-path source; reproduction **NOT-established**).
After a timeout, a commit exception/500, or an unreadable state, the controller reconstructs
native state from the operation record plus observed members/actors/Pods/allocations, and never
blindly re-sends the same request; a different target is not applied while the previous operation
is unresolved. When state cannot be reconstructed, the operation holds as Unknown and capacity is
retained — Unknown never releases a Pod.

**Restart recovery.** Controller restart (and rank/API-worker restart during a resize, which is an
explicit negative-acceptance cell) rebuilds from: desired spec + operation generation + effective
observation + actual allocation state. No memory-only callback, no "executed OK" boolean is
trusted across a restart (**READ**: the program's reconciliation design; the S2 formal-stage
charter).

**Boolean status is not authorization.** `is_scaling_elastic_ep=false` is a status flag; combined
with `/server_info` it still cannot authorize retirement — only Feature 1's captured-identity +
actor-free proof can (**READ**: pinned vLLM v0.29.0 API surface; the S2 formal-stage charter).

**Pre-registered candidate thresholds.** The following are candidate gates for the native
measurement campaign, registered now to prevent post-hoc threshold movement; none is an accepted
SLO and none can pass before the physical gate opens (**READ** as candidates: the program's
pre-registered candidate advice and its acceptance-measurement analysis): repeatability ≥20
complete 2→4→2 cycles for vLLM; resource/ledger convergence ≤5 minutes after operation completion
(AC-1.4); traffic-withdrawal convergence ≤5 s (S1-shared); SC01 net SLO-capacity gain ≥10%;
allocated GPU-hours per million qualifying output tokens ≥10% below a runnable fixed-EP group at
equal SLO, with ≥3 paired repeats; `drain_timeout` 120 s as the engine default candidate
parameter only.

**Acceptance.**

- AC-2.1 Every registered failure cell — operation-generation conflict; concurrent/conflicting
  target; controller restart mid-prepare/drain/commit; rank or API-worker restart mid-resize;
  resource starvation mid-resize (quota revoked/preemption); worker capacity that never joins the
  Ray cluster (the Ray analog of a missing joiner); probe-induced restart of a healthy
  reconfiguring API container — has exactly one readable, recoverable post-state, or an explicit
  Unknown that retains all capacity.
- AC-2.2 Blind replay is refused (negative): after an injected commit-500 and without
  reconstruction, a repeated identical `POST /scale_elastic_ep` is not sent; the controller's
  next action is reconstruction, and the refusal is observable.
- AC-2.3 The 503 window is reported per phase for every resize run (longest no-success window,
  per-phase durations); a run without this report cannot close any continuity or interruption
  claim (Feature 3).
- AC-2.4 Probe coordination is proven by extending a reconfiguration window beyond the current
  liveness budget with no container restart, in a native run.
- AC-2.5 Across one resize, the client ledger's per-attempt accounting remains complete and
  consistent while the engine's own counters reset; no acceptance metric in this spec reads a
  cumulative engine gauge across the operation boundary.
- AC-2.6 Post-commit verification reads effective width AND a real forward at the new width;
  engine API 200 alone, `is_scaling=false`, or `/server_info` never satisfies it.
- AC-2.8 A generation change while the bound operation is unresolved forces reconstruction
  and capacity retention; neither the previous nor a new target is dispatched blindly.
- AC-2.7 After controller restart mid-operation, the resumed controller reaches the same state as
  an uninterrupted run on the same fixtures (recovery equivalence), and never issues a duplicate
  engine request for an already-committed generation.

### Feature 3 — Service continuity versus instance-interruption observation

**Two concepts, two acceptance checks, never merged.**
- *Instance interruption* is what a resize does to the resizing instance: the measured 503/pause
  window (Feature 2, AC-2.3). A single-instance arm characterizes this window and can never
  record a continuity pass.
- *Service continuity* is a property of the deployment: during the target's resize, an
  independent surviving complete instance (fixed width, separate failure domain) keeps accepting
  and completing new requests at SLO on first-attempt outcomes, and enough admitted GPUs exist
  for both. A continuity pass requires an explicit backup-instance arm; the program's continuity
  scenario requirement states that a 4-GPU single-instance matrix without a backup can never
  record a service-continuity pass, so an EKS 2→4→2 arm alone records instance characterization
  only (**READ**: the program's scenario requirements).

**First-attempt discipline.** Acceptance of both checks reads first-attempt outcomes; client or
Router retries are recorded in a separate ledger and never convert an observed 503 into success
(**READ**: the program's first-attempt acceptance discipline). Smaller-shape continuity
candidates (e.g., vLLM 1→2→1 plus a fixed DP=1 backup instance, peak 3 GPUs) remain conditional
on their own fit gate and never substitute for the main 2→4→2 matrix's interruption
characterization (**READ**: the program's cluster-shape scenario requirements).

**Acceptance.**

- AC-3.1 A run with only the resizing instance reports interruption metrics (AC-2.3 outputs) and
  is labeled instance-characterization; any continuity wording in its results is a spec violation.
- AC-3.2 A continuity pass requires, as retained evidence: survivor instance identity and
  routability observed before/during/after; survivor first-attempt success at SLO across the whole
  operation window; retries ledgered separately; and per-GPU admission covering both instances.
- AC-3.3 A 503 observed on the resizing instance is never reported as zero interruption on the
  basis that clients retried successfully.

### Feature 4 — State observability: desired / admitted / Ray / effective as distinct states

**Four fields, one per layer of Feature 1**, reported per instance: `desired` (declared width),
`admitted` (Kueue-admitted/allocated workers), `ray` (registered cluster members), `effective`
(engine-effective world with identity mapping). Each field carries its own explicit **Unknown**
value when its observation is missing, stale, or unattributable; Unknown is never coalesced into
another field's value, never rendered as zero, and a known zero (e.g., zero registered workers) is
a real observation distinct from Unknown (**READ**: the program's state-reporting analysis; the
S2 formal-stage charter).

**The effective field is observational, not a frozen API commitment.** An
`parallelism.effective`-like field is defined as the observational output of the four-layer
reconciliation, supported by engine native state, operation/generation, member identity, and
successful-inference evidence — Pod count, startup argv, or an API 200 are insufficient
substitutes (**READ**: the program's runtime-parallelism analysis, which defines what may count as
an effective-width observation). Its public wire form (name, type, defaults, `+optional`
treatment) is frozen at the S2 my-plan gate under the S1 precedent, and only if the evidence
supports it; until then no public observational effective-width field is added to `api/`.
The elastic request fields above have their own accepted contract.

**Booleans and config readouts are not states.** `is_scaling_elastic_ep` and `/server_info` may
appear in controller logic but never satisfy a state field, never authorize retirement (AC-2.6),
and never stand in for a missing observation (**READ**: pinned vLLM v0.29.0 API surface).

**Acceptance.**

- AC-4.1 During a native resize, the four fields transition independently and observably
  (admitted moves before ray, ray before effective); a forced layer disagreement (admitted but not
  Ray-registered; Ray-registered but not yet effective) shows each field at its own true value.
- AC-4.2 Each field renders Unknown for its own missing/stale/unattributable observation while
  other fields keep values; no field inherits or imputes another's value; known zero and Unknown
  are distinct renderings.
- AC-4.3 No public observational effective-width field is added before its own freeze gate.
  The observational effective concept remains non-frozen wherever it is described.
  This restriction does not remove the accepted elastic request fields.

## User Stories

1. As a platform operator, I want elastic width changes to proceed only through admitted, Ray-
   registered capacity and to retire only proven actor-free Pods by captured identity — so a
   resize can never delete a Pod still carrying actors or strand GPU quota.
2. As an operator watching a resize, I want desired/admitted/Ray/effective reported separately
   with explicit Unknown — so I can see which layer is lagging instead of guessing from a Pod
   count.
3. As a service owner, I want the resize 503 window measured and my continuity claims backed by an
   independent surviving instance — so "we stayed up" is evidence, not phrasing.
4. As the operator on call after a resize failure, I want the controller to reconstruct native
   state and refuse blind replay — so a timeout or commit-500 cannot turn into a duplicate or
   conflicting engine operation.

## Evidence grades and measurement limits

- **READ (this worktree, HEAD 4b81d9b1):** role-size freeze webhook
  (`pkg/worker/webhooks/worker/model_deployment.go:661-670`); S1 spec contracts (sibling
  committed ancestry) composed unchanged.
- **READ (pinned upstream):** vLLM v0.29.0 commit `98dff2a8…` (mode constraints, API surface,
  failure-path findings, metrics reset, 503 middleware; three files byte-verified against the Git
  object in the program's source-provenance review); Kueue v0.18.10 slicing-as-Workload-annotation
  (upstream controller/slicing sources); KubeRay v1.7.1 `WorkersToDelete` semantics (upstream
  controller source); GPUStack allocation annotation and accounting units (GPUStack source in this
  repository, reviewed in the program's source-review activity).
- **MEASURED (program PoC activities, no elastic/EP runtime; substances inlined here, raw
  retained program-side):** in the hardware-observation activity on a single node — one 8×H100
  80GB node, driver 580.173.02, NV18 peer links on all pairs, no InfiniBand device; in the
  fixed-DP baseline activity — two ordinary-DP whole-GPU Workloads admitted at 1,600k accounting
  credits each with actual Pod UID + GPU UUID captured, and one nonempty 16-token ordinary DP=2
  completion whose argv contained no EP/Elastic/Ray flags (not EP evidence); in the vLLM Ray
  fixture attempt — the official pinned vLLM image lacks Ray, and the hash-pinned Ray 2.56.1
  overlay was preempted before execution (E7 FAILED FIXTURE, per the program's
  dependency-and-bounded-verification ruling); in the instrument-review activity — the
  client-request ledger passed RED/GREEN verification for per-attempt completeness and
  SLO-capacity accounting with the cohort including the drain tail; in the S1 template-review
  activity — the drain reader exited 0 on missing gauge series, the caution behind AC-2.5's
  ledger rule.
- **NOT-established (open risks, deliberately carried):** Ray 2→4→2 elastic EP with real EP
  inference (E7); four-layer runtime accounting incl. actor→Pod mapping and release (E9 runtime);
  completion/timeout/restart recovery behavior (E10); the 503 window's actual duration and probe
  interaction; prepared-state leftovers after drain timeout; commit-exception boolean residue;
  sliced/partitioned
  allocation modes; all performance candidate thresholds; Qwen3-30B-A3B fit and NIXL/kernel
  behavior on target GPUs. None of these may be upgraded by wording anywhere in this spec; they
  gate physical acceptance and any evidence-dependent API freeze.

- **MEASURED (later baseline and setup attempts):** stock vLLM 0.29.0 with full Ray 2.56.1
  establishes width-two inference only with the earlier temporary shared-memory workaround.
  Its native rank-zero and rank-one requests complete; no width change is issued.
  The product-default attempt verifies the deployed operator through its owning Deployment,
  ReplicaSet, Pod image digest and full binary revision.
  A fixed-revision 61,084,187,391-byte model cache becomes Ready with the expected manifest digest.
  Both native deployments receive width-two admission, allocation and registered Ray GPU capacity.
  Each cluster exposes two placement groups with co-located GPU and CPU bundles.
  Native effective width remains Unknown when the user requests closeout during model loading.
  No later inference, resize, continuity or release acceptance follows from those setup observations.

## Constraints and Boundaries

- **Always:** keep the four states distinct with explicit Unknown; capture identity before any
  retirement; prove actor-freeness at runtime before retiring; keep the API/DP-master member out
  of every retirement set; preserve the whole process group while withdrawn/reconfiguring; verify
  effective width and a real forward before declaring completion; account metrics from the
  client-side ledger across a resize; retain raw evidence per arm with identity, phases, and
  per-attempt outcomes program-side.
- **Ask first (root ruling):** D07 whole-GPU vs sliced/partitioned scope; acceptance or revision
  of any pre-registered candidate threshold after the native campaign; the public wire freeze for
  the observational effective field; whether cloud node provisioning 2→4 becomes a chartered case
  (currently Non-Goal); any change to the selected operator-owned realization.
- **Never (boundary violations this spec explicitly forbids):**
  - deleting workers by count instead of by captured identity;
  - treating `is_scaling=false` as a retirement credential;
  - claiming SGLang scale-down capability;
  - presenting 4 pre-started nodes (or any pre-provisioned physical capacity) as proof of 2→4
    elastic scaling;
  - blind-replaying `scale_elastic_ep` after Unknown/timeout/commit-500;
  - treating the all-endpoint 503 scaling window as liveness death;
  - trusting cumulative engine gauges/counters across a resize boundary;
  - starting engine-level actions on unadmitted capacity, or retiring before commit;
  - reading a registry listing, `is_scaling` flag, or `/server_info` as a membership, effective-
    width, or retirement-authorization observation;
  - recording a continuity pass without an independent surviving instance;
  - deriving elastic support from the ordinary DP=2 completion or the direct GPU head+API Jobs
    (neither enabled EP/Elastic/Ray — **MEASURED** in the program's fixed-DP baseline activity).
- **Public API changes** (when a later freeze gate opens them) require generated
  deepcopy/CRD/conversion code, the owning guide `docs/modules/model-deployment/status.md`,
  `docs/README.md` sync in the same change, and `make lint docs`; Go subjects additionally run
  `make generate` / `make lint`.

## Risks and Mitigations

- **The physical gate fails (engine or resources).** Physical acceptance stays Blocked; failed
  native capability stays failed/blocked — scope is never silently narrowed to whatever ran
  (S2 formal-stage charter). Mitigation: each feature's acceptance names its gate evidence.
- **KubeRay semantics drift or unfit** (e.g., `WorkersToDelete` group expansion interacts badly
  with one-GPU Pods). Mitigation: the realization must name its controller; identity capture +
  actor-free proof are independent of the mechanism; alternatives re-examined at the gate.
- **503 window interacts badly with probes or Routers.** Mitigation: probe coordination is an
  acceptance cell (AC-2.4); withdrawal/qualification follow S1's confirmed contracts.
- **Metrics reset corrupts long-window accounting.** Mitigation: client-side ledger only
  (AC-2.5); already RED/GREEN-verified instrument.
- **Budget/preemption interrupts the native campaign mid-resize.** Mitigation: every arm retains
  phase timelines and raw evidence; interrupted runs are recorded as such, never re-framed.
- **Status field pressure ("just expose effective now").** Mitigation: Non-Goal + AC-4.3; wire
  freeze only at the my-plan gate with evidence.

## Design Details

### Commands

```sh
make lint docs    # required Markdown gate for this spec
```

### Project Structure

- This document: `specs/2026-10-01-s2-elastic-ep-resize.md` (the only file this spec creates).
- Implementation surfaces: elastic workload
  realization and resize controller under `pkg/worker/controllers/worker/` (serial ownership with
  the `model_deployment*.go` surfaces); status contract under `api/worker/` at the freeze gate;
  owning guides `docs/modules/model-deployment/` and `docs/README.md` in the same change as any
  shipped behavior.

### Implementation Plan

The first implementation tranche is offline and has no public API additions. Its internal
operation record and native client are implementation details, not proof of a deployed elastic
profile. Current local Go toolchain and repository dependencies are the execution environment;
workers run focused tests and builds, and the coordinator runs whole-tree lint after editors stop.
The existing S1 source/design review is adopted. A failed bounded delivery stops for a concrete
disposition rather than opening another general review or automatic retry.

- [x] **T1 · Native vLLM resize client**
      Blocked by: None
      Owns: `pkg/worker/elasticengine/`
      Acceptance: actual HTTP calls use POST for both native endpoints, enforce deadlines and
      bounded responses, classify resize 503 and ambiguous failures, and never retry mutation
      automatically. Neither status false nor HTTP 200 proves effective width.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -race ./pkg/worker/elasticengine/...`
- [x] **T2 · Durable operation and four-layer transition kernel**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_elastic_operation*.go`
      Acceptance: an internal operation is bound to deployment UID, generation and captured
      allocation/member identities. A Kubernetes-backed record uses optimistic concurrency.
      Decisions preserve distinct Unknown layers, require admitted/allocated/Ray capacity before
      engine action, persist command intent before dispatch, reconstruct after ambiguity/restart,
      and only permit captured actor-free non-master identities after effective-plus-forward proof.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -race ./pkg/worker/controllers/worker/...`
- [x] **T3 · Complete S1 serving and drain integration in the S2 branch**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_deployment_router_observation*.go`,
      `pkg/worker/controllers/worker/model_deployment_drain_collector*.go`,
      `pkg/worker/controllers/worker/model_deployment_retirement_test.go`
      Acceptance: uncached deployment and before/after membership observations; complete
      Pod→ReplicaSet→Deployment ownership; terminating-process coverage; custom role, listener
      port and decimal-rank identity binding; physical endpoint union and per-role counting;
      contradictory selectable/non-selectable rows refuse convergence; llm-d admin 9090 and
      removed-endpoint recent-dispatch evidence hold withdrawal until safely cleared; drain
      identity checks require the expected API kind/version and a running container. No status
      wire or accepted budgets change.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -race ./pkg/worker/controllers/worker/...`
      Offline source integration is complete. Uncached caller and membership guards, live
      Router ownership, terminating-process coverage, physical per-role counts and post-exec
      drain identity checks pass focused controls and the full package race suite. Only Confirmed
      serving status carries a count. Same-name apps owner replacement controls now read
      fresh UIDs and fail when either identity comparison is removed. CPU checks do not prove physical EP behavior;
      native Router placement, drain and resource-release validation remain in T6.
- [x] **T4p · Native observation protocol spike and client tracer**
      Blocked by: T1
      Gate: review
      Owns: `pkg/worker/elasticengine/effective_observation*.go` and the package description
      in `pkg/worker/elasticengine/client.go`
      Acceptance: verify the pinned native rank-header validation and error serialization.
      The candidate sends a non-streaming completion with `X-data-parallel-rank: -1`.
      A strict native out-of-range response observes the frontend rank bound independently
      from desired width. It is insufficient by itself. Completion requires real forwards
      for every expected rank, another equal boundary read, captured master identity and a
      complete Ray rank-to-member map. The client reports native observations only; the
      controller owns the identity and mapping checks. Generic errors, ignored headers,
      changed bounds, incomplete forwards and unsupported schemas refuse confirmation.
      Prepared TCPStore keys, config readouts and actor counts cannot replace this protocol.
      First gate: reached pinned-source producer and error-handler controls before Go edits.
      If that gate fails, retain the exact blocker and do not implement an invented endpoint.
      Offline acceptance: focused HTTP negative controls, bounded calls, no resize replay,
      actual package build and race tests. Physical acceptance remains in T6; source fixtures
      do not prove a native engine or satisfy AC-2.6.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -race ./pkg/worker/elasticengine/...`
- [x] **T4 · Ray workload, admission and identity-safe retirement realization**
      Blocked by: T2, T3
      Owns: `pkg/worker/controllers/worker/model_deployment_elastic_workload*.go` and
      the explicitly selected controller integration configuration
      Acceptance: name and verify the deployed controller version, the width-delta admission
      object and actual Ray node/actor→Pod UID mapping. The API/master is a GPU worker with
      the S1 endpoint/eligibility/lifecycle ownership rendered explicitly. CPU-only head and
      one-GPU single-host workers retain independent accounting. A same-name replacement
      between capture and delete survives; names-only WorkersToDelete or a preflight UID
      check cannot satisfy deletion safety. Unknown mapping retains workers.
      Verify: focused realization tests plus actual E9 object/ledger observations in a
      separately gated physical window.
- [x] **T5 · Wire resize reconciliation and probe coordination**
      Blocked by: T1, T2, T3, T4
      Owns: elastic controller wiring, selected profile rendering and relevant admission checks
      Acceptance: freeze any required public request wire separately before API edits; implement
      ordered scale-up/down using durable records, single-writer ownership and reconstruction.
      A generation change while unresolved holds/reconstructs and never dispatches a second
      resize. Liveness tolerates a healthy native 503 window. Effective width requires native
      identity-bound evidence and a real forward; no fabricated endpoint or engine patch.
      Verify: focused controller tests, build, lint and native failure/probe cells.
- [x] **T6 · S1 integration validation and S2 physical acceptance**
      Blocked by: T3, T4, T5
      Owns: bounded integration scenarios and retained validation artifacts
      Acceptance: validate all three routers with actual request placement across eligibility,
      readiness and failure events; streaming drain and real GPU/quota release; native EP
      2→4→2 and restart/timeout/commit-failure cells. Record scale-up and scale-down phases
      separately. A continuity arm records the survivor deployment and instance identities;
      a single-instance arm reports interruption only. Missing or failed cells remain explicit.
      Verify: the physical Test Plan below within the granted resource/time/budget window.
- [x] **T7 · Documentation, generated outputs and S2 submission**
      Blocked by: T1, T2, T3, T4, T5, T6
      Owns: owning model-deployment guides, `docs/README.md`, generated outputs when needed
      Acceptance: guides describe tested behavior and limitations; module routing and generated
      files match the final code; a reviewable S2 PR retains explicit failed/unverified cells.
      Verify: `make lint`, `make lint docs`, affected race tests and `make generate` when
      API/webhook source changes; generation runs in a canonical scratch checkout.

### Test Plan

- **Prerequisites (the physical gate itself, in program terms):** E9 four-layer runtime
  accounting with real GPU allocation and observed release; E7 native 2→4→2 with real EP
  inference, retained phase timelines, and the ≥20-cycle repeatability candidate; E10 failure/
  recovery cells (AC-2.1's list) in a chartered GPU window. Environment: the program's fixed
  shapes — one/two 8×H100 nodes or up to four 1-GPU RTX Pro6000 nodes; Qwen/Qwen3-30B-A3B BF16,
  revision `ad44e777bcd18fa416d9da3bd8f70d33ebb85d39`, fixed manifest, local tokenizer,
  ModelPrefetch/CSI in the same artifact namespace; native `vllm bench serve` with the thin
  first-attempt record layer; EPLB candidate parameters as pre-registered observability
  configuration, re-registered before performance arms (**READ**: the program's measurement-plan
  requirements; the S2 formal-stage charter).
- **Offline mechanism and subsequent runtime validation:** four-layer agreement fixtures (AC-1.1, 1.2, 1.5, 1.6);
  identity-precondition retirement including actor-holding negative (AC-1.3); ledger-convergence
  release (AC-1.4); failure-cell matrix with one readable state each (AC-2.1); blind-replay
  refusal (AC-2.2); probe-coordination window (AC-2.4); ledger-vs-counter reset (AC-2.5);
  effective+forward verification (AC-2.6); restart equivalence (AC-2.7); observability matrix
  (AC-4.1, 4.2, 4.3).
- **Continuity (chartered window with backup instance):** AC-3.1–3.3 with the survivor evidence
  set; single-instance runs confined to characterization.
- **Negative controls (each shown red before its green counts):** count-based deletion attempt on
  an actor-holding Pod must block; `is_scaling=false` + `/server_info` presented as retirement
  authorization must be refused; engine gauge deltas across a resize must not enter any acceptance
  metric; unadmitted scale-up must not reach the engine; a continuity claim from a
  single-instance run must be rejected by the reporting contract.
- **Evidence:** every run retains manifest, phase timeline, per-attempt ledger, object snapshots
  with UIDs, engine raw responses, and cleanup receipts in the program's retained evidence, kept
  program-side; no absolute local paths, credentials, IPs, or hostnames in committed documents.

## Alternatives

- **SGLang elastic EP as the first S2 implementation.** Rejected for this spec: pinned v0.5.18
  supports merged scale-up only; scale-down has no merged contract. The merged scale-up remains
  T09's equally required separate branch.
- **vLLM External Elastic EP (PR #43202).** Closer to the Pod/rank model and S1's endpoint
  identity, but unmerged; E8 keeps it as optional isolated research at a fixed SHA; formal support
  never depends on it.
- **CPU head hosting the API/DP-master** (plain Internal DP does this). Kept as a configuration
  negative, not a profile: strict Elastic placement requires positive local size on GPU workers
  (**READ**: the program's elastic placement analysis, consistent with pinned v0.29.0
  strict-placement rules); changing pack strategy or local size is an independent experiment, not
  derivable from the non-elastic example.
- **Widening the existing fixed Workload in place.** Rejected: no current fixed Workload gains
  Pods opportunistically; the elastic profile is its own realization with its own four-layer
  accounting (the S2 formal-stage charter).
- **Treating `is_scaling` transitions as the operation state machine.** Rejected: it is one
  boolean on the engine's HTTP surface, unversioned and finally-unreset on the commit path; the
  operation record with generation and captured identities is authoritative.

## Open Questions

Resolved here and not open: the four-feature decomposition; scale-up/scale-down orderings; the
Never list; whole-GPU initial scope (pending D07 only for any widening); SGLang and E8 as
Non-Goals; source implementation authorized; physical acceptance and public-wire freeze remain evidence-gated.

1. Native observation protocol and the exact operator build that realizes the cluster.
   The CPU protocol spike and physical campaign must establish these before support is claimed.
2. The native campaign's measured 503-window shape and probe interaction (AC-2.3/2.4 inputs).
3. Drain-timeout leftovers: prepared state, actors, and second-target refusal behavior in the
   pinned engine (explicit E10 cells before any timeout policy freezes).
4. Whether the D07 ruling widens scope to sliced/partitioned allocation, which would reopen
   Feature 1's accounting.
5. Candidate-threshold confirmation or revision after the native measurement campaign (all
   thresholds in this spec are pre-registered candidates).
