# Spec: ModelDeployment S1 — Router Endpoint Qualification, Serving Confirmation, and Abortable Fixed-Instance Retirement

Status: Building
Blocked on: root staged-gate acceptance of the working-copy diff before commit (per-task gates below; a task stays unchecked until its gate accepts it)
Type: Feature

## Summary

A `ModelDeployment` scale-down today deletes the replica first
(`pkg/worker/controllers/worker/model_deployment.go:496-527`) and leaves the post-delete preStop
hook (`pkg/worker/controllers/worker/model_deployment_drain.go:26-185`) to negotiate with traffic
still arriving; its reader counts a valid metrics body missing the expected gauge series as idle,
and a request in flight at the hook deadline is cut by design. The two Rust Routers never stop
selecting a worker on readiness loss — removal fires only on the deletion timestamp — and no
status field separates "the operator disqualified this instance" from "Routers actually stopped
selecting it". This spec adds: a reversible per-instance eligibility signal with same-signal
ordinary-Service convergence; the two Rust discovery fixes plus an llm-d same-event regression;
per-Router-process, identity-bound serving confirmation with deduplicated-union counting and
explicit Unknown/NotConfigured/confirmed-zero states; a whole-group health gate with an observable
group-forward predicate; an abortable pre-delete retirement protocol whose timeout retains
capacity (including today's explicit `replicas: 0` manual stop); and the status fields — declared
parallelism provenance, eligibility, actual serving confirmation — that make it observable without
renaming an eligibility count into a serving claim. Automatic scaling and Elastic/EP width changes
are out of scope; no engine placement physics is derived from Pod counts.

## Motivation

### Goals

1. **Reversible disqualification.** After an aborted or timed-out retirement, a healthy retained
   instance serves again without a rollout, and its return to actual Router pools is confirmed by
   the same identity-bound views that confirmed its exit.
2. **Actual per-Router confirmation.** The operator reads every Router process that may accept new
   requests and counts, by physical endpoint (deduplicated union), whether the target remains in
   the pool a new request is actually drawn from. A registry listing is not that proof.
3. **Zero only when proven.** Confirmed zero requires complete, fresh, identity-bound coverage of
   every qualifying Router process; missing views, stale generations, unsupported routers, and
   lagging replicas yield Unknown or NotConverged — never zero. An unconfigured router yields
   NotConfigured, not a fabricated zero.
4. **Abortable retirement that retains capacity.** A retirement that cannot finish inside its
   budget stops, keeps every member Pod and the Workload, records the step and reason, and lets a
   healthy instance requalify — including a surplus instance the spec no longer declares.
5. **Discovery fixes tested against placement, not listings.** vLLM Router and the SGLang gateway
   stop selecting NotReady/disqualified workers; llm-d keeps its removal semantics under the same
   event matrix; acceptance reads actual request placement.
6. **Honest status.** Declared parallelism (with provenance and Unknown sources), endpoint
   eligibility, and actual serving confirmation are separate fields; zero is never omitted and
   Unknown is never rendered as zero.

### Non-Goals

- Automatic scaling (T06): no replica writer or adapter entry; but the explicit `replicas: 0`
  manual stop is existing semantics, carried by this protocol (Feature 4), not deferred.
- Elastic/EP width (S2): `roles[].size` stays immutable; no engine prepare/commit paths.
- New engine placement physics: no External DP profile, rank layout, or model placement is derived
  from Pod counts; engine shape support remains a per-engine question.
- Engine-side changes: no new engine flags, no synthetic identity labels in engine metrics, no
  assumed engine pause or sidecar capability.
- Replacing the post-delete preStop hook: it stays as last-line protection after deletion.

## Source contracts this composes (verified at worktree HEAD 4b81d9b1)

- **Selectors.** The published role selector and router discovery selector name the leader member
  per replica (`pkg/worker/controllers/worker/model_deployment_router.go:126-191`); argv-configured
  Routers match equality terms split on "=" only. New selector terms must stay equalities rendered
  from the same source map that status publishes.
- **Services.** Per-replica headless Services select the ordinal and publish unready members
  (`pkg/worker/controllers/worker/model_deployment_service.go:72-90`); the ordinary deployment/role
  Services narrow to leaders above size one (`:119-146`). EndpointSlice convergence stops
  new-connection selection only; established keepalive/HTTP/2 connections keep delivering requests.
- **Hash boundary.** Rollout compares each live Pod's stored hash annotation to the desired render
  (`pkg/worker/controllers/worker/model_deployment.go:558-570`); runtime label writes sit outside
  it, while labels enter the desired render hash
  (`pkg/worker/controllers/worker/model_deployment_render.go:1556-1587`) — a dynamically
  maintained eligibility key does not roll Pods; a template key does, once.
- **Readiness status.** A replica counts Ready only when complete and all-ready
  (`pkg/worker/controllers/worker/model_deployment_status.go:355-434`); bookkeeping, not a traffic
  gate; unchanged by this spec.
- **Engine gauges.** Per-engine in-flight gauge sets including P/D transfer queues are enumerated
  (`pkg/worker/controllers/worker/model_deployment_drain.go:64-77`); the current rendered reader
  exits 0 on a body missing them (measured, below).
- **Parallelism parse.** Degrees default to 1; Modes and Wiring are presence lists; declared local
  zero stays distinct from undeclared
  (`pkg/worker/controllers/worker/model_deployment_parallelism.go:17-143`); the same parse feeds
  the P/D transfer document (`pkg/worker/controllers/worker/model_deployment.go:1289-1328`), whose
  numeric outputs must not change.

## Proposal

### Feature 1 — Reversible instance eligibility, Service convergence, backfill

The reconciler maintains one operator-owned, namespaced eligibility **label**
(`modeldeployment.gpustack.ai/endpoint-eligible: "true"`, written only by the single
ModelDeployment reconciler write path) on exactly the
endpoints a Router or ordinary Service may select — leader-only for leader-served roles, every
rank-carrying member for External-DP-shaped roles. It must be a label, not an annotation: only a
label can satisfy a Kubernetes Service selector. It is a runtime write outside the hash boundary
(no template change, no rollout), removed to disqualify and restored only through Feature 3's
verified path. The ordinary Services' selectors gain the same equality term from the same source
map; internal headless replica Services keep selecting every member with
`publishNotReadyAddresses` untouched. The existing ready-instance status fields are preserved
unchanged.

- **Backfill before narrowing.** Before any Service or Router selector gains the key, the
  reconciler backfills it onto every existing healthy qualifying endpoint — no window in which a
  new selector term matches nothing and healthy endpoints disappear from selection. A static
  endpoint-classification label, if later chosen, stays an explicit one-time rollout cost.
- **Single writer.** The ModelDeployment reconciler is the only writer of the key; conflicts are
  observed state, not silently overwritten.

**Acceptance.**

- AC-1.1 Removing/restoring the key changes no Pod generation, UID, Workload, or stored hash.
- AC-1.2 While removed, no Router selects the instance's endpoints for new requests (per-request
  placement evidence, not registry reads); after restore, all reselect.
- AC-1.3 First enable over an existing deployment never drops ordinary-Service selection to zero
  healthy endpoints (backfill precedes narrowing; tested as the upgrade negative).
- AC-1.4 Controller restart / watch reconnect re-derives the key level-based, without flapping.
- AC-1.5 A new or replaced member becomes selectable only after its own eligibility verification;
  PodReady alone never produces a selectable-but-unqualified endpoint, and a previous member's UID
  verification does not carry over to its same-name replacement.

### Feature 2 — Actual serving confirmation, per Router process

- **Coverage.** Direct per-process access to every Router process that may accept new requests;
  never sampling through a load-balanced Service. A Router replica withdrawn from the entry
  Service but able to accept on existing connections is in scope or explicitly confirmed stopped.
- **Identity binding.** Each view records Router process identity (Pod UID; boot/config generation
  where exposed), observation freshness, and per-endpoint source Pod UID/instance identity/URL/
  port/role. Endpoint identity comes from the transport/observation envelope the operator controls
  (Kubernetes-resolved Pod UID and member identity bound at collection time) — never from an
  invented native metric label. URL equality never proves identity across an IP reuse; an
  incomplete or indeterminate binding makes the view Unknown. Direct per-Router Pod discovery and
  UID-bound collection are implementation tasks of this spec, not existing Service capabilities.
- **Per-Router reality and patches.** Today's surfaces are not confirmation: vLLM Router
  `GET /workers` lists the registry while request-time selection adds health and circuit-breaker
  filters; the SGLang gateway has the same split; llm-d exposes a Pod-list datastore, a pool-level
  ready metric, request-subset filtering, and an optional per-plugin debug dump that can return
  HTTP 200 with unsupported/error payloads. Two separated patch classes are allowed in the
  operator-owned Router build: (a) **discovery fixes** that change withdrawal/selection behavior —
  mandatory for the two Rust Routers (NotReady and eligibility-key removal), regression-guarded on
  llm-d; (b) **read-only observer additions** exposing a uniform identity-bound membership view,
  which must not alter selection policy. No other policy changes ride along. Build ownership is
  explicit: today `pack/llm-router` applies source patches on the vLLM Router stage only; adding
  patch application to the SGLang and llm-d builder stages where needed is in scope for this
  spec, with source pins and lockfile policy fixed in the plan — no new unrelated toolchain. The
  operator source comment claiming every supported router drops a replica only on its deletion
  timestamp is stale against llm-d's pinned source (which removes on NotReady or label loss) and
  is corrected as part of implementation; the CPU NotReady arrival-export gap stays unverified.
- **Aggregation.** Serving count = deduplicated union of the instance's physical endpoints across
  all complete views (virtual ranks fold to physical listeners). Per-Router numbers are never
  summed; views are never intersected (a lagging replica would zero an intersection and hide a
  leak). Replica disagreement reports NotConverged; any missing/stale required view is Unknown.
- **Confirmed zero** requires: coverage proven; every qualifying view complete, fresh,
  identity-matched; no view holds a target endpoint; prior-generation views converged; no
  in-window dispatch to the target. It asserts observed withdrawal only — not collective/KV
  drainage (Feature 3/4's gates) and not request completion.
- **No router configured.** Confirmation reports the distinct state NotConfigured — neither zero
  nor Unknown-by-absence; retirement is then governed by the Service-only gate plus the
  existing-connection hold (Feature 4).

**Acceptance.**

- AC-2.1 Single-Router replica, target disqualified: the view confirms zero for the target and
  reports survivors; the same view observes requalification after restore.
- AC-2.2 A Router replica killed/restarted/rolled mid-observation yields Unknown or NotConverged —
  never a zero from the missing view, never a count hiding the other replica's residual.
- AC-2.3 Same-name replacement (new UID, reused URL/IP) is a different identity; stale views are
  Unknown for the old instance.
- AC-2.4 Observation unreachable (port/auth/404/partial dump): Unknown; retirement does not
  proceed to deletion on that absence.
- AC-2.5 Negative: a worker registered-and-healthy but rejected by the actual selection predicate
  (circuit breaker open / availability false) is not counted as serving (vLLM Router, gateway).
- AC-2.6 An unconfigured router reports NotConfigured; no serving number is synthesized.

### Feature 3 — Whole-instance health gate with an observable group predicate

Eligibility requires, at instance granularity: member set complete; every member ready per the
existing probes; an observable group forward/recovery predicate — engine-level evidence that the
group's collective forward path works (the deployment's own engine health observation composed
with per-member probes); no planned retirement; no pending member replacement or topology
inconsistency. One failed member revokes the whole instance. Where an engine offers no verifiable
group-forward observation, that leg is Unknown or unsupported and **eligibility stays held with
the reason** — native full-group health is never faked. Restore requires the strongest verifiable
predicate on the same membership generation: one Pod returning Ready, a same-name replacement, or
a label flipping back is not sufficient.

**Capability activation is gated on its predicates.** On first enable or upgrade, if the required
group predicates (health/group-forward observations) are unavailable for the deployment's engine,
qualification and retirement capability activation **fails before any selector narrowing** — the
status reports Unsupported/Unknown and existing routing is preserved untouched; an Unknown
whole-group predicate must never silently blackhole a healthy deployment. Definite health-fault
withdrawal (the Rust NotReady/eligibility removal fixes) remains required regardless. In supported
mode, a new member waits for the verified group predicate before it is admitted to selection; the
safety is never weaker after activation than before it.

**Acceptance.**

- AC-3.1 A single member's crash/NotReady revokes instance eligibility on all Routers and the
  ordinary Services within the observation bound.
- AC-3.2 Restore happens only after the whole-group predicate passes on the same membership; held
  state reports which leg is unverified.
- AC-3.3 A member replaced with a new UID resets the verification generation.
- AC-3.4 With no verifiable group-forward observation for an engine, restore is held unsupported
  with reason — never granted on health-HTTP alone.
- AC-3.5 First enable with unavailable engine group predicates: capability activation fails before
  selector narrowing, existing routing is preserved end to end, and status reports
  Unsupported/Unknown; definite health-fault withdrawal still functions.

### Feature 4 — Abortable retirement with a persistent operation reservation

Planned retirement is a level-based protocol whose progress is one **persisted reservation**,
`status.retirement` — an optional object; absent means no operation:

`roleName string; replicaOrdinal int32 (0 explicitly encoded); observedGeneration int64;
targetMemberUIDs []string; targetWorkloadUID string; state enum(Admitted, Disqualified,
Withdrawing, Draining, Deleting, Settling, Aborted, Completed); reason string; startedAt,
deadline, phaseStartedAt metav1.Time; lastConsumedRetryToken string`.

The reservation identity binds the ModelDeployment UID, the admitting `metadata.generation`
(no invented operation counter), role, ordinal, and the target member and Workload UIDs.
`phaseStartedAt` is persisted so a controller restart never resets a phase budget. Fixed
conservative initial budgets, reviewed here: withdrawal 30s, drain 240s, settle 30s, overall
300s; a phase ends at min(overall deadline, phase start + phase budget); the settle phase runs
after the committed deletion and its expiry is reporting, not a further safety gate. Deletion
requires a nonempty current target set, and every Pod and Workload delete carries a UID
precondition.

**Reservation ownership covers every owner-driven deletion path, not just scale-down.** The
reconciler today deletes replicas and Workloads on several paths: ordinal ≥ declared removal
(`pkg/worker/controllers/worker/model_deployment.go:461-472`), surplus shedding (`:485-491`),
the replica Pod + Workload delete (`:496-527`), the departed-role stranded-Workload release
(`:400-408`), rollout replacement, and the stranded sweep. Every one of these intercepts a
reserved or aborted live capacity through the same reservation: a path whose target carries the
reservation's role/ordinal/UIDs holds at the protocol instead of deleting. A new scale-up intent
re-declaring a held ordinal adopts and cancels the reservation when the retained group is the
same UID and verifiably healthy and complete — it never deadlocks the create gate and never
creates a duplicate; a new UID resets the verification generation. An unavoidable external
termination (preemption, node failure, a hand deleting a Pod) is recorded as externally
terminated — the protocol reports what it could not retain instead of claiming all failures
retain. At most one reservation is in flight per deployment; a conflicting operation is refused,
never raced.

1. **Admit.** Write the reservation (new generation); verify no conflicting operation.
2. **Disqualify.** Remove eligibility; Services converge (Feature 1).
3. **Confirm withdrawal.** Wait for Feature 2 to reach confirmed zero for the target, including
   the propagation tail; Unknown or residuals hold here. With no router configured, this step is
   the Service-only gate plus the existing-connection hold.
4. **Drain in place.** Preserve every member Pod and the Workload; read engine in-flight gauges
   under the hardened contract — every expected series present, two consecutive complete
   envelope-matched zero reads end the drain. **Transport is concrete, not Pod-IP-alone:** the
   controller collects through a named channel (exec into the member's Pod, or the API-server
   proxy to the member's engine port) with Pod UID and container identity checked before and
   after each read, inside a bounded watch window. **Collectors map the actual profile:** a
   member without its own Prometheus HTTP listener is not pretended to have one; where the
   engine's native activity contract is proven, the serving member's (e.g. leader group)
   aggregate may represent the group's activity — it can never assert a per-member individual
   zero; when every serving member has its own HTTP listener, each is collected. Missing native
   coverage holds Unsupported/Unknown — no series or endpoint is invented; replacement,
   watch-gap, or indeterminate bindings are Unknown; transport failures are Unknown and polled
   on. Streaming in-flight and P/D transfer queues are inside this reading; a queue that cannot
   be attributed to the instance identity holds as Unknown. P/D paired retirement additionally
   requires an observed decoder-side release; until such evidence exists, P/D retirement reports
   unsupported rather than deleting on prefill gauges.
5. **Re-verify intent.** Spec change, role deletion, rollout arrival, quota preemption, controller
   restart, or watch gap re-enters the protocol at the observed state.
6. **Delete uniformly.** Only after confirmed withdrawal plus verified drain: delete the group's
   Pods and Workload as one decision; preStop remains post-delete protection.
7. **Settle.** Observe actual accelerator release and quota convergence; completion is recorded
   against observations.

**Abort, restore, retry.** A budget exhausted at any pre-delete step aborts: the reservation is
retained with its step and reason; members, Workload, and capacity stay. The target is a retained
surplus the spec no longer declares — restore does **not** require the spec to declare it: a
healthy retained instance requalifies through Feature 3. An aborted reservation is **not** retried
on every reconcile under unchanged intent. **Retry is an explicit directive with a crash-safe
token:** the annotation `modeldeployment.gpustack.ai/retirement-retry` carries the value
`roleName:replicaOrdinal:opaqueRetryToken`; it is parsed against the CURRENT Aborted reservation
and the same UID target —
malformed values and wrong targets are refused; the reconciler persists the consumed token into
the reservation (`lastConsumedRetryToken`) atomically BEFORE clearing the annotation. The crash
ordering is exact: a crash after the status persistence leaves the token CONSUMED in the
reservation (the annotation may remain) and any replay is a no-op; a crash before it leaves the
token unconsumed, and it may be consumed once. Copy negatives cover three shapes: replaying an
already-consumed token, a token naming a different role or ordinal, and a token naming a
new-UID target. The token is opaque — its origin is not provable across ModelDeployments and no
authentication provenance is invented for it. A new
token explicitly requests a new retry. No second replica writer exists. A new
`metadata.generation` re-evaluates the current desired shape and role before admission; harmless
unrelated metadata changes are not intent. A retained healthy group that the spec re-declares
cancels the old withdrawal and requalifies — it is never retired as a duplicate. Health
revocation and member replacement still revoke eligibility independently of any reservation.

**Explicit `replicas: 0`.** Manual stop is existing semantics and takes the same protocol: desired
reads 0 while a retirement holds; a timeout retains the running capacity with desired=0 /
observed>0 recorded; nothing is force-dropped to satisfy the number.

**EndpointSlice precision and the existing-connection hold.** Service convergence stops
new-connection selection; established keepalive/HTTP/2 connections keep delivering new requests.
The Router path is the controlled entry where per-request withdrawal is provable (each new request
consults the current candidate pool). Direct-Service existing-connection capacity may retire only
when actual ingress quiescence is proven by observation; otherwise the protocol holds at step 3.
No engine pause or sidecar capability is invented to claim otherwise.

**Acceptance.**

- AC-4.1 A 2→1 retirement completes: pre-delete confirmed zero, preserved members through drain,
  uniform deletion, no first-attempt loss attributable to the retirement, accelerator release
  observed.
- AC-4.2 A drain-budget expiry aborts: members and Workload survive, no deletion, status names the
  step; the healthy instance requalifies and serves with no rollout — and is **not** re-drained by
  subsequent reconciles until new intent is issued.
- AC-4.3 Controller restart mid-protocol resumes from the persisted reservation and fresh
  observations; a second conflicting operation is refused.
- AC-4.4 A drain-time metrics body missing a required series holds as Unknown and never deletes; a
  collection that cannot be bound to the target member's envelope (replacement, watch gap,
  indeterminate) never counts as idle; two complete envelope-matched zero reads proceed.
- AC-4.5 A union residual held by any one Router view blocks deletion and is visible in status.
- AC-4.6 P/D retirement without a verifiable decoder-side release reports unsupported and does not
  delete on prefill gauges alone.
- AC-4.7 `replicas: 0` through the protocol: while held, desired=0 with running capacity preserved
  and observable; no forced drop at budget expiry.
- AC-4.8 With no router configured, retirement is gated by Service-only withdrawal plus the
  existing-connection hold; unquiesced established direct-Service connections hold the protocol.
- AC-4.9 Every owner-driven deletion path (ordinal ≥ declared, surplus shed, replica/Workload
  delete, departed-role release, rollout replacement, stranded sweep) intercepts a reserved or
  aborted target: restart, repeated reconcile, a new scale-up, role deletion, rollout, and
  stranded cleanup cannot bypass the reservation; a re-declared same-UID healthy held group
  adopts/cancels instead of deadlocking create or duplicating; an externally terminated group is
  recorded as such.

### Feature 5 — Status and identity contract

Public API additions; the **wire definition (names, types, defaults) was frozen at the
my-plan gate before build and stands as reviewed**; the contract below is that frozen wire. Per role, alongside
the existing unchanged ready-instance count:

- `parallelism.declared` — concrete pointer fields (`*int32`), present only when the role
  actually declares that degree; explicit 1 and explicit local 0 are preserved values; a nil
  field means undeclared, never zero. Canonical names, spelled as the engines register them:
  `tensorParallel`, `pipelineParallel`, `dataParallel`, `dataParallelLocal`,
  `prefillContextParallel`, `decodeContextParallel`, `expertParallel`,
  `attentionContextParallel`, `moeDpSize` (`--moe-dp-size`), `dwdpSize` (`--dwdp-size`).
- `parallelism.modes` — `map[string]bool` over present-only canonical engine keys (an observed
  mode carries its explicit boolean; absence of a key is not false), never a bare `[]string`
  that loses false/unknown.
- `parallelism.loadBalance` — derived enum: Internal / External / Hybrid / MultiPort / Unknown;
  default Unknown when not derivable (the vLLM external-lb-via-rank derivation feeds External);
  Unsupported is a support judgment recorded in reason fields, not a mode value.
- `parallelism.source` — `kind enum(ExtraArgs, Command, UnmanagedCommand, Unknown)`,
  `complete bool`, `unreadableReason string`; an unreadable source is kind Unknown with
  `complete=false` and a reason — never silently 1.
- `endpoints.eligible` — `*int32`, set only when the qualified list is complete; explicit 0 is a
  real empty set; when the list is not complete the field is nil and the associated condition
  carries Unknown. Named for eligibility; never presented as serving.
- `endpoints.serving` — `{state enum(Confirmed, NotConverged, Unknown, NotConfigured),
  value *int32}` from Feature 2's union; `value` is required only when `state=Confirmed`
  (explicit zero included); the other states never encode a fake 0; virtual ranks folded;
  residual identities keep the protocol holding regardless of any reported number.

**Wire amendment accepted at the T1 independent-review gate.** Making the two new outer
role-status objects required in the generated CRD schema rejects the next write of any role
object stored before they existed; the aggregated worker/v1 handler proxies the real v1alpha1
CRD storage, so the validation window is real. The outer `parallelism` and `endpoints` role
fields therefore carry `+optional`: Go value types, protobuf slots 8/9, the always-initialized
controller write, and nil-count semantics are unchanged, and no inner field's requirement
changed. An absent outer object names "stored before this view existed", never a defaulted
reading; the operator's next status write fills it again.

The current presence-list parser is not the final API truth; the shape above is the contract
this spec freezes together with the `status.retirement` reservation types of Feature 4 and the
`lastConsumedRetryToken` string on that reservation. P/D transfer outputs are protected: any
parser refactor keeps their numeric results identical on all existing fixtures. Task-internal
identifiers need not become public API.

**Acceptance.**

- AC-5.1 Declared provenance matches rendered argv parsing across explicit-1, explicit-local-0,
  wiring-flag, and unreadable-source cases; transfer outputs identical on existing fixtures.
- AC-5.2 In a disqualified state, `eligible` is 0 while `serving` is Unknown (incomplete views) or
  Confirmed zero — the fields never collapse.
- AC-5.3 A take-over command role leaves `source` Unknown with reason; no fabricated degrees.

## User Stories

1. As a platform operator scaling a fixed serving deployment down, I want traffic stopped first,
   withdrawal proven per Router, the group drained in place, and only then deletion — so
   scale-downs never cut user-visible requests or strand quota.
2. As an operator whose retirement hit its budget, I want it to stop, keep the capacity, and say
   which step blocked — so I can fix the cause or let a healthy instance serve again.
3. As a user reading status, I want eligibility and actual Router confirmation as separate fields
   where zero is an observed fact and Unknown is an observability gap — never the same number.
4. As an operator running streaming or P/D workloads, I want a retirement that cannot prove its
   queues empty to hold instead of delete — so in-flight generations and KV transfers are never
   abandoned by a scale-down that cannot see them.

## Evidence grades and measurement limits

- **READ (this worktree, HEAD 4b81d9b1):** every source contract cited above; line anchors
  re-verified in this checkout.
- **READ (pinned upstream snapshots, byte-verified):** vllm-router v0.1.15
  (`src/service_discovery.rs`: readiness in Eq/Hash; removal only on the deletion-timestamp
  branch; healthy-add-only event handling), gateway-v0.3.1 (same shape), llm-d v0.10.0
  (`pkg/epp/controller/pod_reconciler.go`: removes on NotReady or label loss), plus their
  registry/observation endpoints lacking a complete per-request availability predicate.
- **MEASURED (CPU mock, no GPU).** Procedure: three HTTP backends behind one Router replica; every
  client request carried a unique body marker; each backend logged arrivals as marker + its own
  Pod UID; arms removed the eligibility label or set kubelet NotReady; responses and arrival logs
  joined per marker; 10-second client timeout. Results, with limits: vLLM Router label-off — 79
  post-bound new requests all completed, 26 reached the original target UID (the accepted
  measurement; other fault arms of that older round are inconclusive and not cited as measured).
  SGLang gateway label-off and NotReady — the victim received 110 of 330 in each, beyond the
  5-second settle. llm-d, most recent round: baseline 330 unique responses joined 112/108/110 to
  the three backend UIDs; label-removal — 330 attempts, 328 succeeded to the two survivors, 2
  client timeouts, zero target arrivals in the retained arrival records; NotReady — 328 succeeded
  plus 2 timeouts, but all three backend arrival exports came back empty, so the target-arrival
  zero cell is **unverified** and no zero-leak claim is made. Drain reader: the current rendered
  reader exits 0 (idle) on a valid Prometheus body lacking both expected vLLM gauges; a corrected
  prototype classifies missing-series/wrong-identity as Unknown and idle only on two consecutive
  complete identity-matched zero reads — the prototype demonstrates the contract; it is not
  delivered code.
- **Not established by this evidence:** any GPU behavior, engine collective/EP physics, new
  External DP profile, performance, multi-Router-replica lag, Router rollout mid-protocol,
  UID/IP-reuse live cell, streaming long-request drain, P/D transfer drain, GPU release — my-plan
  Test Plan work; per the program's feasibility gate they are implementation acceptance, never
  pre-spec passes.

## Constraints and Boundaries

- **Always:** preserve residuals on partial view loss; keep Unknown, NotConverged, NotConfigured
  distinct from zero; keep internal headless discovery complete; backfill before selector
  narrowing; rebuild protocol state level-based; retain per-test raw evidence outside committed
  manifests.
- **Ask first (root):** which Routers need observer patches versus read their pinned surfaces;
  any static classification label added to the template.
- **Never:** present registry or eligible counts as serving; treat missing data as zero or idle;
  delete before confirmed withdrawal plus verified drain; restore eligibility to an unverified
  group (including a retained surplus); ride unrelated policy changes inside Router patches;
  derive engine placement physics from Pod counts; claim GPU or new-External support from CPU
  evidence; let the confirmation path mutate Router state.
- **Public API changes** require generated deepcopy/CRD/conversion code, the owning guides
  (`docs/modules/model-deployment/status.md` and touched siblings), `docs/README.md` sync in the
  same change; `make lint docs` is the required Markdown gate; `make generate` / `make lint` apply
  to changed Go subjects.

## Risks and Mitigations

- A pinned Router view cannot be made trustworthy → per-class Unknown/unsupported with a recorded
  blocker escalated to root; scope is not silently narrowed to single-Router or label counts.
- Eligibility key fought by other writers → single-writer reconciler, namespaced key, conflicts
  observed not overwritten (AC-1.4).
- Drain holds accelerators on a wedged engine → bounded budgets with abort-and-retain; held
  capacity is reported state, never silently converted to delete.
- Union residuals over-conservative during Router rollouts → generation-aware staleness plus
  NotConverged reporting; the rollout cell is Test Plan work before defaults freeze.
- Discovery fixes regress llm-d or reappear on upstream rebases → one shared event matrix as the
  regression net; patches live in the owned Router build and rebase deliberately.
- Status schema churn → semantics fixed here, wire definition frozen once at the my-plan gate.

## Design Details

### Commands

```sh
make generate     # after api/ or webhook edits
make lint         # changed Go subjects
make lint docs    # required Markdown gate for this spec and owning guides
```

### Project Structure

- `pkg/worker/controllers/worker/model_deployment*.go` — eligibility, Service convergence,
  retirement protocol, reader-contract correction (serial ownership with T03/T04 surfaces).
- `api/worker/v1alpha1/model_deployment.go` + generated code — status contract (wire frozen at
  my-plan gate).
- `pack/llm-router/` — separated discovery fixes and read-only observer patches per Feature 2.
- `docs/modules/model-deployment/{status,routing,shutdown}.md`, `docs/README.md` — owning-guide
  updates in the same change.
- This document: `specs/2026-10-01-s1-router-qualification-and-drain.md`.

### Code Style

Repo conventions apply (AGENTS.md): comments state the why at contract level, typed errors, no
panics for control flow, level-based reconcile, table-driven tests with one behavior per case.

### Implementation Plan

Shared Go surfaces (`pkg/worker/controllers/worker/model_deployment*.go`) are serial; the Router
build/discovery path (`pack/llm-router/**`) is disjoint. Every task passes a root staged-gate
review; exact verification env follows the repo convention
(`GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ...`).

- [x] **T1 Tracer bullet: public status contract initialization** (first — later Go tasks compile
  against these types). Blocked by: none. Owns: `api/worker/v1alpha1/model_deployment.go`,
  `pkg/worker/controllers/worker/model_deployment_status.go` (controller status producer, not
  unused fields only), generated deepcopy/CRD assets via `make generate`,
  `docs/modules/model-deployment/status.md`. Gate: root staged-gate review of this task's exact
  diff before any commit. Acceptance: `status.retirement` optional object
  (absent = no operation) with the typed wire above including `lastConsumedRetryToken`; role
  `parallelism`/`endpoints` fields initialize Unknown/NotConfigured with nil unknown counts;
  existing ready/quota/parse semantics unchanged; no production eligibility selector activates.
  Verify: `make generate` with every generated path enumerated from the actual run (full diff
  root review); `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./api/...`; explicit-0 vs
  nil encoding tests; old source/P-D numeric output regressions; `make lint docs`.
- [x] **T2 Parser provenance → status.** Blocked by: T1. Owns: parallelism parser read-side
  mapping, `model_deployment_status.go` (globs:
  `pkg/worker/controllers/worker/model_deployment_{parallelism,status}.go`). Gate: root
  staged-gate review of this task's exact diff before any commit. Acceptance: AC-5.1/5.3; transfer fixtures
  byte-identical. Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race
  ./pkg/worker/controllers/worker/...`.
- [x] **T3 Router builder stages + discovery patch application — one complete green bullet.**
  Blocked by: none (isolated ownership; only T1/T3 may initially run in parallel, each in its
  isolated ownership/worktree; any generation-level rewriting serializes against the other
  writer). Owns: `pack/llm-router/**`. Gate: root staged-gate review of this task's exact diff
  before any commit. Acceptance: the sglang stage applies its patches; the llm-d stage applies
  patches only where an actual llm-d patch is needed later — no currently-unused plumbing;
  `git apply` context failure fails the build; per-stage resolved lock/build-dependency evidence
  recorded (upstream SG ships no Cargo.lock; no byte-reproducibility guarantee, no sha256
  framework). Verify (remote authorized packaging host over SSH; no hostname/IP committed):
  `docker buildx build --platform linux/amd64 --target epp-builder -f pack/llm-router/Dockerfile .`
  and `--target vllm-router-builder` / `--target sglang-gateway-builder`; logs/source SHAs/image
  IDs preserved.
- [ ] **T4 Rust discovery fixes + llm-d regression matrix.** Blocked by: T3. Owns:
  `pack/llm-router/patches/{vllm-router,sglang-gateway}/**`. Gate: root staged-gate review of
  this task's exact diff before any commit. Acceptance: AC-1.2 event matrix on
  placement evidence; llm-d unchanged-behavior regression green. Verify: same buildx targets +
  the event-matrix harness (implemented and tested under the root-assigned owned evidence path,
  fixed at dispatch; local script invocation with retained context variables; runtime grants
  separate when budget/caps ready). Acceptance split: the source and build levels of this task
  are committed together with T3 in one commit, because the Dockerfile copies the two patch
  directories this task owns and a split commit would break the build on either side. The
  runtime acceptance moves to T6: the seven-cell event matrix runs there as five cells, and
  the failed-Add re-list and failed-Remove retry cells stay UNVERIFIED under the root ruling
  against fault-injection hooks in the production patches, with the real-cluster phase as the
  revisit trigger. This box stays unchecked until that runtime acceptance lands.
- [x] **T5 Eligibility label, backfill, Service convergence.** Blocked by: T2. Owns: label
  maintenance and service/router selector render paths (globs:
  `pkg/worker/controllers/worker/model_deployment_{service,router}.go` plus the reconciler-owned
  label write in the serial package; exact files fixed at dispatch). Gate: root staged-gate
  review of this task's exact diff before any commit. Acceptance: AC-1.1–1.5 (upgrade
  negative must go red without backfill). Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test
  -race ./pkg/worker/controllers/worker/...`.
- [ ] **T6 Per-Router observation.** Blocked by: T5 and T4 (the Go client needs T5's eligibility
  state, and observer patches land after T4 so no two writers touch `pack/llm-router` at once).
  Owns: observer-only patch additions + Go client/aggregation. Gate: root staged-gate review of
  this task's exact diff before any commit. Acceptance: AC-2.1–2.6 with the listed negatives.
  Verify: package tests + harness negatives.
- [x] **T7 Group health predicate + activation gating.** Blocked by: T6. Owns: controller
  health/eligibility paths (globs: `pkg/worker/controllers/worker/model_deployment*.go`, serial
  package). Gate: root staged-gate review of this task's exact diff before any commit.
  Acceptance: AC-3.1–3.5. Verify: package tests (unsupported-engine
  activation negative).
- [x] **T8 Retirement reservation FSM + interception.** Blocked by: T7 (edges to T2/T5/T6 as
  consumers of their observations). Owns: `model_deployment.go` reconcile paths + new
  reservation files in-package; intercepts ordinal≥declared (`:461-472`), surplus shed
  (`:485-491`), replica/Workload delete (`:496-527`), departed-role release (`:400-408`),
  rollout and stranded sweep. Gate: root staged-gate review of this task's exact diff before any
  commit. Acceptance: AC-4.1–4.9 incl. retry-token crash ordering and
  budget boundary min(overall, phase). Verify: package tests (cannot-bypass table, retry crash
  ordering, budget boundary).
- [x] **T9 Controller-side drain reader.** Blocked by: T8. Owns: drain reader files in-package;
  RBAC/initialization paths if the chosen transport needs them; stale delete-only comment
  corrected (a comment only — the current preStop exit-0 behavior is not thereby claimed safe).
  Gate: root staged-gate review of this task's exact diff before any commit. Acceptance:
  hardened contract with ONE concrete transport — a bounded read-only `pod/exec`
  metrics collector inside the exact engine container on its local engine port (including a
  decode engine behind its proxy), Pod UID + container ID pre/post checks and watch continuity;
  proxy fallback only if factually needed and exposure proven — two transports are not built;
  collector map per actual profile (leader aggregate only where the native activity contract is
  proven, never a per-member individual zero; missing coverage = Unsupported/Unknown hold).
  Verify: fixture matrix (missing series / wrong container / watch-gap / aggregate-only /
  complete zero ×2 / P/D hold; a body missing a required series as the anchor negative).
- [x] **T10 Owning docs + spec sync + full gates.** Blocked by: T1–T9, T11. Owns:
  `docs/modules/model-deployment/{status,routing,shutdown}.md`, `docs/README.md`. Gate: root
  staged-gate review of this task's exact diff before any commit. Acceptance:
  docs match shipped behavior. Verify: `make lint docs`; `make lint`; `make generate` no-diff.
- [x] **T11 Engine group-forward observation.** Blocked by: T7 (the predicate it feeds is
  committed; the serial package admits one in-flight task at a time). Owns:
  `pkg/worker/controllers/worker/model_deployment_health*.go` plus any new engine-observation
  client files in-package; no `pack/llm-router` or retirement-path edits. Gate: root staged-gate
  review of this task's exact diff before any commit. Acceptance: the GroupForward leg gains a
  verifiable observation for each engine where one factually exists at the pinned versions —
  per-engine evidence cited from the pinned engine surfaces, and where none exists the leg stays
  Unsupported with the reason naming the engine; a healthy multi-member group qualifies only
  through that observation, a broken collective still revokes through the definite-fault legs,
  and the single-member path is byte-identical in behavior. Until this task lands, multi-member
  replica groups (TP/PP groups, External-DP multi-member roles, P/D roles) stay held — the
  committed T7 behavior, kept deliberately over granting eligibility on a weaker signal.
  Verify: package tests (per-engine verifiable/unsupported matrix; healthy multi-member group
  admitted only via the observation; broken collective revoked; single-member path unchanged).
- [x] **T12 Shape-aware retirement drain membership.** Blocked by: T9. Owns: drain member
  selection in-package (`model_deployment_retirement*.go` and its tests) plus the two
  multi-member limit paragraphs in `docs/modules/model-deployment/shutdown.md`. Gate: root
  staged-gate review of this task's exact diff before any commit. Acceptance: the retirement
  drain reads the members the replica's shape makes answerable — every member of an External-DP
  replica (each one serves), the leader member only of a leader-served multi-member replica
  (the T9 leader-aggregate contract, mirroring the eligibility rule's per-shape membership), the
  one member at size one — and a replica whose answering members cannot be identified stays held
  with the reason naming the shape. The T9 every-member loop can never finish a leader-served
  multi-member retirement: non-leader members serve no metrics, so the reads hold until the
  drain budget expires and such a replica is never retired. Verify: package tests (leader-served
  size-2 drains through the leader alone and completes; External-DP size-2 still reads every
  member; unidentifiable leadership holds; size-1 path byte-identical).

### Test Plan

- Prerequisites: kind/envtest fixture cluster; pinned Router images from T3; client
  marker/arrival-ledger harness; no GPU required (the GPU release/quota cell runs only under a
  chartered GPU window; otherwise recorded not-run).
- Unit (local owned worktree, pre-build, per-task commands above): provenance mapping; backfill
  ordering; reservation FSM transitions/budgets/retry-token ordering; reader fixture matrix;
  observation aggregation union/Unknown/NotConfigured.
- Integration (requires built operator + T4 images): three-Router event matrix on placement
  evidence; upgrade backfill negative; Router replacement/lag; same-name UID/IP replacement;
  restart mid-protocol resume; conflicting operation refusal; scale-up adopt/cancel.
- E2E (requires built operator): 2→1 retirement with streaming in-flight; budget-expiry
  abort-and-retain with requalify; `replicas: 0` hold; P/D hold as unsupported; GPU
  release/quota settle (chartered window or explicit not-run).
- Negative controls (each shown red before green counts): no-backfill narrowing; missing gauge
  series delete attempt; wrong-target retry; double retry-token consumption (consumed-token
  replay / wrong role-ordinal / new-UID target); intercepted surplus-delete bypass attempt;
  registry-as-confirmation equivalence.
- Evidence: every test YAML and failure/readback artifact preserved outside committed production
  manifests; no future test names claimed as existing; each new load-bearing test
  mutation-validated before it counts.

## Alternatives

- **Readiness-gate qualification instead of a runtime key.** Services would follow automatically
  and llm-d already removes on NotReady. Not first choice: still needs the Rust fixes, changes
  PodSpec on first deploy, risks a readiness loop if derived from PodReady. Retained as fallback.
- **Router worker-management APIs as the withdrawal control.** Explicit calls give receipts, but
  three divergent interfaces, P/D worker-type differences, and possible auto-re-add make them the
  wrong control plane now; their read-only sides still feed Feature 2 where trustworthy.
- **Delete-timestamp plus preStop only (today).** Irreversible, no capacity retention, measured
  defects remain; kept only as the post-delete fallback layer.
- **Trust registry listings as confirmation.** Measured false: the registry is not the selection
  predicate on two of three Routers and carries no identity/generation contract.
- **Ship automatic scaling (T06) here.** Rejected: automatic entry must not share a lifecycle with
  the retirement contract until the manual path is proven.

## Open Questions

The status wire (names, types, defaults), the label mechanism and its exact key
(`modeldeployment.gpustack.ai/endpoint-eligible: "true"` with the single reconciler write path),
and the initial retirement budgets (withdrawal 30s, drain 240s, settle 30s, overall 300s,
Feature 4) are resolved and frozen; they are not open here.

1. Per-Router observer patch necessity per pinned source, and the llm-d debug-dump trust boundary.
2. Ingress-quiescence observation for the direct-Service existing-connection hold — what counts as
   proof, per supported ingress shape.
3. Router boot/config generation exposure for generation-aware staleness (not yet collected).
