# Spec: ModelDeployment PD Roles — Cross-Role Atomic Admission

Status: Shipped
Type: Feature

## Summary

A `ModelDeployment` today runs one role. This spec lets it run several — `prefill` and `decode`
above all — and admits them **atomically**: one Kueue Workload covering every role, so a pool that
cannot fit both leaves both queued instead of admitting the prefillers and stranding them waiting for
decoders that will never come.

There is **no new CRD**. `spec.roles` is already a list in the single-role spec, validated as
length 1; this spec lifts that bound to 1..10 and adds three per-role fields — `kind` (what the
engine is told it is), `acceleratorKey` (which accelerator model inside the pool this role wants) and
nothing else. The Pods of a deployment become one **Kueue pod group**, one PodSet per role, and the
existing per-accelerator AdmissionCheck grows the one thing a multi-role Workload needs from it:
each role's demand is judged against the cards of the flavor **that role** was assigned, not against
every card in the pool.

Two hard constraints shape the whole design, both verified in this tree:

- **A Workload carries one `queueName`.** Atomic admission therefore requires every role in one
  ClusterQueue, which requires `roles[*].instanceType` to be identical. This spec does not relax that
  and cannot.
- **Kueue assigns a ResourceFlavor per PodSet.** So *inside* one ClusterQueue, two roles can still
  land on two different accelerator models — which is what makes heterogeneous P/D expressible at all
  once the pool spans models.

Heterogeneous P/D across **manufacturers** stays impossible, and the reason is structural rather
than a missing feature: a ClusterQueue's accelerator quota is `credits.gpustack.ai/<manufacturer>`,
one resource name per manufacturer, and one resource group covers one set of resources.

## Motivation

Prefill and decode have opposite shapes. Prefill is compute-bound and bursty; decode is
memory-bandwidth-bound and steady. Running them as one process makes every decode step wait behind
somebody's prefill, and the standard answer is to split them into two pools of processes that hand KV
blocks from one to the other.

The split only works if **both halves are running**. A prefiller with no decoder produces KV blocks
nobody consumes; a decoder with no prefiller has nothing to decode. So the scheduling requirement is
not "schedule two things" but "**schedule two things or neither**".

The single-role `ModelDeployment` spec creates N independent Kueue Workloads, one per replica. That
is correct there — replicas of one role are independently useful — and it is exactly wrong here: with
four independent Workloads on a pool that fits two, Kueue admits two, and which two is a race.

### What already exists, and what is missing

Verified in this repository and against the Kueue the chart deploys — `hack/deps.sh` pinned **0.18.4**
when this was written, while `go.mod` resolves an older `sigs.k8s.io/kueue`, so what is recorded below
is the **deployed** version's behaviour. The pin moves on its own schedule; *Notes* carries the
standing rule for reading a Kueue fact:

**The pod-group mechanism exists and needs nothing from us.** Kueue's `pod` integration is enabled in
the deployed configuration (`kueue.managerConfig.controllerManagerConfigYaml` in the chart's
`values.yaml` lists `"pod"` among `integrations.frameworks`). A set of Pods sharing the
`kueue.x-k8s.io/pod-group-name` label becomes one Workload, and
`constructGroupPodSets` (`pkg/controller/jobs/pod/pod_controller.go`) builds **one PodSet per distinct
role hash**, each with its own `Count`.

**Per-role flavor assignment exists and needs nothing from us.** In
`pkg/scheduler/flavorassigner/flavorassigner.go`, each PodSet forms its own assignment group by
default (`groupKey := strconv.Itoa(i)`), and `checkFlavorForPodSets` filters candidate flavors by
matching the PodSet's own `nodeSelector` against that flavor's `spec.NodeLabels`. An accelerated
`ResourceFlavor` in this repo carries a **model-level** node label —
`acceleratable.feature.gpustack.ai/<manufacturer>-<model>: "true"`, written by
`nodefeature.ExtractNodeFlavors` — so a PodSet carrying that key as a nodeSelector can only be
assigned that model's flavor.

**What is missing is the field that expresses it.** Neither of the two places a user could write it
today will do:

- `roles[*].instanceType` must be **identical** across roles (one Workload, one `queueName`), so it
  cannot discriminate models.
- `roles[].template` is an `InstanceTemplate` overlay, and `InstanceTemplate`'s complete field set is
  `Image / ImagePullPolicy / Command / Privileged / Ports / Env / Resources / VolumeMount /
  ImagePullSecret / AdditionalVolumes` (`api/worker/v1alpha1/instance.go:77`). **There is no node
  selection field of any kind in it.**
- `InstanceSpec.NodeName` exists but pins to *one named node* ("rendered as the backing Pod's
  nodeSelector on kubernetes.io/hostname", `instance.go:64`). That is neither a model-level choice nor
  meaningful for a role with several replicas.

**The per-accelerator AdmissionCheck was not role-aware when this spec was written.**
`NodeDevicesAdmissionReconciler` (`pkg/worker/controllers/worker/node_devices_admission.go`) walked
every PodSet (`parseFamilyDemands`) and unioned every assigned flavor's `Devices`
(`candidateDevices`) — and then **flattened both**: `collectCards` produced one undifferentiated card
list with no flavor provenance, and `nodeDevicesFeasibility` fitted the merged demands against it.
Its doc comment stated the assumption it rested on — *"already scoped to one flavor pool by label"* —
which held for a single-flavor Workload and not for a two-model group, so the consequence was a wrong
verdict rather than a missing feature (see F6).

**That is T10, and it shipped on its own in `4d88673b` before the rest of this spec started**:
`cardLedger` now carries `flavors`, `fitScalarDemand` gates on `coveredBy(d.flavor)`, and
`verdictMessage` names the podset that fell short. The paragraph above is kept in the past tense
rather than deleted because it is the problem statement T10 was written against.

### Goals

- **G1 (primary)** A two-role `ModelDeployment` is admitted **all-or-nothing**. On a pool that fits
  one role's demand but not both, **neither** role's Pods start, and the deployment reports why. This
  is the claim the spec exists to make demonstrable; the observation is recorded (F9, Test Plan).
- **G2** Two roles of one deployment can be placed on **two different accelerator models of the same
  manufacturer**, inside one ClusterQueue, through a field that says exactly that and nothing else
  (F4).
- **G3** Nothing new is added to the admission chain: no sixth gate, no second AdmissionCheck, no new
  controller. The pod group is a property of the Pods this deployment already renders (F1).
- **G4** The per-accelerator feasibility check tells the truth for a multi-role Workload: a role's
  demand is never satisfied by a card its own assigned flavor does not cover (F6).
- **G5** A group that is only partly created is a **named, reported state**, not a hang. Kueue does
  not build a Workload for an incomplete group, and the deployment must say so rather than sit in
  `Starting` forever (F2).
- **G6** Every refusal names the constraint and the field. "Two roles with different `instanceType`"
  and "a model this pool does not offer" are both configuration mistakes an operator can fix in one
  edit, and both must arrive as an admission error, never as a Workload that is admitted and then
  Pending forever (F3, F4).
- **G7** The CR stays thin (the single-role spec's G7). This spec adds **three** per-role fields and
  no top-level field; everything else it needs is metadata on the rendered Pods.

### Non-Goals

- **Cross-manufacturer heterogeneous P/D.** Prefill on NVIDIA and decode on Ascend is not
  expressible, and the reason is structural: `buildResourceGroups`
  (`pkg/worker/controllers/worker/node_queue.go:261`) sets an accelerated queue's `CoveredResources`
  to the single resource `credits.gpustack.ai/<manufacturer>`, derived from the pool's one
  `manufacturer` note. A ClusterQueue resource group covers one set of resources, and Kueue's own
  ClusterQueue webhook rejects a second group repeating a covered resource
  (`validateResourceGroups`). Two manufacturers' credits cannot be one queue's quota, and one
  `queueName` per Workload does the rest.
  - Same-manufacturer, **cross-model** heterogeneous P/D *is* in scope (G2). The two are not the
    same restriction and must not be stated as one.
- **Making the pool itself span models.** A ClusterQueue whose identity is manufacturer-level rather
  than model-level — the ResourceFlavor label, the `PoolScheduleLabels` input and the unit spec of
  such a pool — is a change to **every** pool's behaviour in the existing scheduling chain, and it is
  not this spec's. This spec adds one field to one CRD. See *Dependencies* for what the model-level pool work delivers with
  and without it.
- **The P/D data path in every form.** This spec makes the two roles **distinguishable in
  configuration** (F5) and stops there. It does **not** deliver, and asserts nothing about:
  - **transport negotiation** between a prefiller and a decoder;
  - **pairing** — which prefiller hands its blocks to which decoder;
  - **routing** — which prefiller a request lands on, prefix-affinity routing, KV-event consumption,
    and the router that would do it.

  The single-role spec's plain round-robin `Service` is unchanged; a P/D deployment gets one
  `Service` per role plus the deployment-wide one, and nothing steers between them. This boundary
  exists to agree with the one below: a spec that cannot verify the transport must not specify it.
- **Throughput, latency and any RDMA claim.** The KV hand-off between prefill and decode goes over
  the pool; its transport (`rdma_devices`, NIXL, RoCE/IB) cannot be verified on the hardware available
  to this spec. **No performance number is asserted anywhere in it.** The verification is functional:
  the group is admitted atomically, the roles land where they were told to, and the connector carries
  the role discriminator.
- **Multi-Pod replicas.** "One replica = leader + workers across nodes" (tensor parallelism spanning
  nodes) is a different axis from "several roles" and is not added here. One role's replica is still
  one Pod. `roles[].replicas` is a Pod count, as before.
- **A rollout policy softer than recreate.** Any change to the group's shape rebuilds the group
  (F10). Surge and unavailable knobs stay deferred, for the same reason the single-role spec deferred
  them.
- **A `PodSetGroup` / topology-aware co-placement of the two roles.** Kueue can co-schedule PodSets
  through `TopologyRequest.PodSetGroupName`, which would let prefill and decode be placed on the same
  rack. That is a placement optimisation with a real cost (it constrains the assignment), it needs
  Topology-Aware Scheduling configured, and it is not what atomic admission means. Not here.
- **Changing the `Instance` CRD or the `InstanceTemplate` type.** This spec is additive to
  `ModelDeployment` and touches neither (see Alternatives).

### Dependencies

**This spec depends on the single-role `ModelDeployment` spec, which is now BUILT AND MERGED** —
`specs/2026-08-28-model-deployment.md`, landed as `dff92144`. The CRD, the reconciler, the render
path, the Service and the status all come from there, and the shape this spec assumes is the shape
that one shipped:

```yaml
roles:                          # a list from its first version; webhook: exactly 1 entry
- name: server
  replicas: 4
  instanceType: <the pool's InstanceType name>
  extraArgs: []
  env: []
  template: { ... }             # an InstanceTemplate overlay
```

That spec also states, at the `Name` field, that *"the P/D spec gives the name meaning"* — F1 is
where that happens. Its webhook message for the length-1 rule names `specs/*-pd-atomic-admission.md`,
which is this file.

**Cross-model pools are a separate, independent change.** Making a ClusterQueue's identity
manufacturer-level rather than model-level is not owned here. The split is clean:

| | Without cross-model pools | With them |
|---|---|---|
| Homogeneous P/D (both roles on one model) | fully delivered by this spec | unchanged |
| `roles[].acceleratorKey` | accepted, validated against the pool's offered keys, renders the selector; a key the pool does not offer is rejected | the same field, now discriminating between models |
| Heterogeneous P/D | not reachable — a model-level pool offers one model | reachable |

So every feature below lands and is testable on today's pools; F4's field is the seam that makes the
heterogeneous case work the day the pool spans models, and F6 is the correctness fix that case
requires.

**G1's second half depends on a cluster setting, and it can be off.** Atomicity has two parts, and
only one of them is Kueue's. `validatePodGroupMetadata` gives *quota* atomicity — an incomplete group
composes no Workload at all, so nothing of it is admitted. The judgement G1 is actually about, *this
pool fits one role's demand but not both*, is made only by the node-devices AdmissionCheck (F6), and
a ClusterQueue references that check only where `NodeQueueReconciler.fillClusterQueue`
(`pkg/worker/controllers/worker/node_queue.go:167`) writes `spec.admissionChecksStrategy` — which it
does when the pool is accelerated, the check reports `Active`, **and the `instance-type-derived-from-node`
setting is `true`**. That is the default, but `false` is a documented deployment mode
(`docs/settings.md` uses it as its worked example of editing a setting), and on that path the
administrator authors the ClusterQueue through the InstanceType API, whose
`InstanceTypeReconciler.createClusterQueue` does not set the field. No other site in the repository
writes it.

So on a cluster with the setting `false`, a two-role deployment gets quota atomicity and **not**
feasibility atomicity: the group is still all-or-nothing against the credits budget, and the "fits
one but not both" verdict G1 demonstrates is not made. Every acceptance criterion below assumes the
default. Closing that gap belongs to whoever owns the administrator-authored ClusterQueue path, not
to this spec — it changes the behaviour of every hand-written InstanceType, which is a separate
decision from adding P/D roles.

### F6 shipped first, on its own

**F6 / T10 — the per-role feasibility fix — depended on neither the single-role spec nor cross-model
pools, and shipped as its own pull request ahead of everything else here (`4d88673b`).** It was
planned as one file, `pkg/worker/controllers/worker/node_devices_admission.go`, and landed as two
packages: review found the label predicate it needed restated there was a private copy of
`pkg/nodefeature`'s, so that package gained `ExtractAcceleratableKeys` and
`IsAcceleratableCreditsResourceName` and the controller calls them. No CRD, no field, no new
dependency — it reads only Kueue types this repository already imports.

It was separated because **it fixed a defect that already existed when this spec was written**,
and an existing defect on the admission path should not have waited for a CRD that did not exist
then. Its failure mode was the reason to hurry rather than to bundle: the check reported `Ready`,
Kueue admitted, and the Pod was then `Pending` forever — a shape that looks like a full cluster and
reports itself as a healthy admission.

Everything else in this spec (F1–F5, F7–F10) waited for the single-role spec, as *Dependencies*
states. That spec has since shipped, so what remains is unblocked.

## Proposal

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: { name: qwen-72b-pd, namespace: team-a }
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct
  engine: vllm
  kvCache:
    poolRef: { name: shared-kv }
    connector: auto
  roles:                                  # webhook: 1..10 entries, unique names,
  - name: prefill                         #          IDENTICAL instanceType across all
    kind: prefill                         # NEW — what the engine is told it is
    replicas: 2
    instanceType: gpustack--nvidia-h20-linux-amd64
    acceleratorKey: nvidia-h20            # NEW — which model inside the pool
    extraArgs: []
    env: []
    template: { ... }
  - name: decode
    kind: decode                          # NEW
    replicas: 2
    instanceType: gpustack--nvidia-h20-linux-amd64   # identical — one Workload, one queueName
    acceleratorKey: nvidia-l40s           # NEW — a different model, same pool (needs a cross-model pool)
    template: { ... }
status:
  phase: Ready
  endpoint: "http://qwen-72b-pd.team-a.svc:8000"
  roles:
  - name: prefill
    kind: prefill
    desired: 2
    ready: 2
    unmanaged: false
    assignedFlavor: gpustack--amd-epyc-9004--nvidia-h20-linux-amd64-8d   # NIL until assigned
  - name: decode
    kind: decode
    desired: 2
    ready: 2
    unmanaged: false
    assignedFlavor: gpustack--amd-epyc-9004--nvidia-l40s-linux-amd64-8d
  kvCache: { ... }
  conditions:
  - type: DomainRegistered
  - type: QuotaReserved            # now the GROUP's single Workload, all-or-nothing
  - type: CacheAttached
```

Rendered onto **every** Pod of the group:

| Key | Value | Why |
|---|---|---|
| label `kueue.x-k8s.io/pod-group-name` | the group name (F1) | membership; Kueue reads the label |
| annotation `kueue.x-k8s.io/pod-group-total-count` | Σ `replicas` over all roles | Kueue refuses to compose a Workload until it sees this many |
| annotation `kueue.x-k8s.io/role-hash` | the role's `name` | **load-bearing**: it is the PodSet name, and it is what keeps two identically-shaped roles from collapsing into one PodSet |
| annotation `kueue.x-k8s.io/pod-group-serving` | `"true"` | an inference deployment never finishes; without it Kueue applies batch completion and reclaim semantics |
| label `kueue.x-k8s.io/queue-name` | `FormatLocalQueueName(instanceType)` | unchanged; Kueue rejects a group whose Pods disagree on it |
| label `app.kubernetes.io/component` | the role's `name` | unchanged; the single-role spec already renders it, it is what `Service` selectors carry, and it is what status reads a Pod's role from |
| `spec.nodeSelector[acceleratable.feature.gpustack.ai/<acceleratorKey>]` | `"true"` | what makes Kueue assign this PodSet that model's flavor (F4) |

**`kueue.x-k8s.io/pod-group-fast-admission` is never set.** See F1.

### User Stories

#### Story 1

As a platform user running disaggregated inference, I want my prefillers and my decoders to be
admitted together or not at all, so that a pool with room for only half of them leaves the whole
deployment queued instead of burning quota on prefillers that can never serve a request.

#### Story 2

As a platform user, I want prefill on the compute-heavy cards and decode on the memory-bandwidth
cards of the same manufacturer, declared per role in one field, so that I do not have to split one
model deployment across two objects that nobody admits together.

#### Story 3

As an operator, I want a deployment whose roles name different `instanceType`s to be **refused with a
message that says why it cannot work** — one Workload carries one queue name — so that I stop looking
for the bug in my quota.

#### Story 4

As an operator, I want `kubectl get modeldeployment -o yaml` to tell me **which accelerator model
each role actually landed on**, so that I can tell "decode got the cards I asked for" from "decode
got whatever was free" without reading the Workload.

#### Story 5

As an operator whose deployment is stuck in `Starting`, I want the reason to distinguish "the pool has
no room" from "not all of the group's Pods exist yet", because the second one has no Workload at all
and so shows nothing in `kubectl get workloads` — the state that looks most like a bug in the
operator.

#### Story 6

As an operator, I want a role asking for an accelerator model this pool does not offer to be refused
at admission with the offered set named, rather than admitted and left Pending, because a Pending Pod
with a nodeSelector nobody satisfies is indistinguishable from a cluster that is merely full.

### Core Features & Acceptance Criteria

#### F1 — One pod group per deployment, one PodSet per role

Every Pod the deployment renders joins one Kueue pod group. Kueue then builds **one** Workload whose
PodSets are the roles, and admits it as a unit — which is the entire point of the spec.

The mechanism, verified in Kueue 0.18.4:

- `constructGroupPodSets` (`pkg/controller/jobs/pod/pod_controller.go:785`) walks the group's runnable
  Pods, computes each one's role hash, and produces **one PodSet per distinct hash**, `Count`
  incremented per Pod. PodSets are sorted by name, so the Workload is stable across reconciles.
- `getRoleHash` (`:663`) returns the `kueue.x-k8s.io/role-hash` annotation **verbatim** when present,
  and otherwise derives an 8-hex digest of the Pod spec's *shape* (`GenerateRoleHash` →
  `SpecShape`, which covers containers, `nodeSelector`, `affinity`, `tolerations`, and more).
- `PodSetReference` is `MaxLength=63`, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`; `NewPodSetReference`
  lowercases its input and does nothing else.

**The `role-hash` annotation is load-bearing, not cosmetic.** Two roles whose Pod specs happen to be
identical — same image, same requests, same (or no) `acceleratorKey` — produce the same derived hash
and would collapse into **one** PodSet of `2+2` Pods. Per-role counting, per-role flavor assignment
and per-role status all disappear at that point, silently and without an error. Writing the role's
`name` into the annotation makes the PodSet identity the role's identity by construction. It is also
why the role name is validated to the `PodSetReference` pattern and to uniqueness (F7).

**`pod-group-serving: "true"` is set on every Pod.** `isServing()` (`:1345`) makes `Finished()`
return "not finished" unconditionally and makes `isReclaimable()` false. Without it Kueue applies
batch semantics to a serving deployment: a Pod reaching `Succeeded` would be reported as a
reclaimable pod and its quota given back while the deployment is still meant to be running.

**`pod-group-fast-admission` is never set, and a task asserts it is not.**
`constructGroupPodSetsFast` (`:764`) takes the **first** runnable Pod of the group, sets that single
PodSet's `Count` to the whole group's total, and returns. One PodSet then carries every role's Pods:
the per-role split this spec exists to create is erased, and with it per-role flavor assignment. The
annotation is a trap for exactly this design and must not appear.

**The group name.** The deployment's `metadata.name` when it is a valid label value (≤ 63 characters,
label syntax), otherwise `gpustack-fnv64-<fnv64a(namespace/name)>` — the same escape the
`LocalQueue` already takes for an over-long `ClusterQueue` name
(`nodefeature.FormatLocalQueueName`, using `stringx.SumByFNV64a`). The readable form is kept where it
fits because this label is the first thing an operator greps for.

Acceptance:

- A deployment with roles `prefill: 2` and `decode: 2` produces **four** Pods, all carrying the same
  `pod-group-name` label, the same `queue-name` label and `pod-group-total-count: "4"`, and each
  carrying `role-hash` equal to its own role's name.
- Kueue builds **one** Workload with **two** PodSets named `prefill` and `decode`, of `Count` 2 each
  — asserted against the live Workload, not against our render.
- Two roles with **identical Pod specs** still produce two PodSets. This is the test that would pass
  by accident without the annotation, so it is written with two roles whose only difference is their
  name.
- `pod-group-serving` is present on every Pod; `pod-group-fast-admission` is present on none, asserted
  by a render test that names the annotation and states why.
- The group name of a deployment whose name exceeds the label limit is the hashed form, and every Pod
  agrees on it.

#### F2 — The group is created in one pass, and an incomplete group is a named state

**Kueue does not build a Workload for a group it has not fully seen.** `validatePodGroupMetadata`
(`:839`): when fast admission is off — which it always is here (F1) — and
`len(activePods) < groupTotalCount`, it emits a `Warning` event with reason `ErrWorkloadCompose`
("group has fewer runnable pods than expected") and returns an **unretryable** error. No Workload is
composed. The deployment has Pods, they are gated, `kubectl get workloads` shows nothing, and no
condition explains it.

Two obligations follow.

**The reconciler creates every Pod of the group in one reconcile pass.** All roles, all replicas, one
pass; a create failure part-way through is retried on the next pass and the pass is not treated as
successful. This is a level-based loop, so "one pass" is a statement about not *deliberately* staging
the creation (role by role, or replica by replica behind a readiness wait), not a claim that a
partial state is impossible.

**A group that is short of its total is reported.** `QuotaReserved` goes `False` with reason
`PodGroupIncomplete`, and the message names how many Pods exist against how many the group declares.
The predicate is observed: the number of the deployment's own Pods that exist, against Σ `replicas`.

Acceptance:

- Creating a two-role deployment issues the creates for all four Pods before the reconcile returns;
  a fake client that fails the third create leaves the deployment reporting `PodGroupIncomplete` and
  the next pass creates the missing Pod.
- With one Pod of the group deleted and its recreation blocked, `QuotaReserved` is `False` with
  reason `PodGroupIncomplete` and a message naming `3/4`; it is **not** left `Unknown`.
- Once the fourth Pod exists, a Workload appears and the reason clears — asserted end to end, since
  the failure it guards against is that no Workload is ever created.

#### F3 — `roles[*].instanceType` must be identical, and the message says why

A Kueue Workload has one `spec.queueName`. Kueue enforces the corresponding rule on the Pods
directly: `validatePodGroupMetadata` (`:851`) returns an unretryable error when two Pods of a group
carry different queue names. Since the queue name is derived from `instanceType`
(`FormatLocalQueueName`), two roles on two `instanceType`s cannot be one group, and therefore cannot
be admitted atomically — the property this spec exists to provide.

The webhook rejects it rather than letting the group form and fail. The message states the cause
(one Workload carries one queue name) and names the field, and it points at `acceleratorKey` as the
way to differentiate hardware **within** one pool.

Acceptance:

- Two roles with different `instanceType` are rejected at admission; the message names
  `roles[*].instanceType`, states the one-queue-name reason, and names `roles[].acceleratorKey`.
- Two roles with the same `instanceType` are accepted, whatever their other differences.
- A single-role deployment is unaffected — the rule is vacuous at length 1, asserted so that the
  single-role behaviour cannot regress.

#### F4 — `roles[].acceleratorKey`: per-role model selection inside one pool

`acceleratorKey` is the accelerator device key (the docs' `aKey`) — `<manufacturer>-<model>`, e.g.
`nvidia-h20` — of the accelerator this role's Pods must land on. It renders as **one nodeSelector
entry**, `acceleratable.feature.gpustack.ai/<acceleratorKey>: "true"`, on that role's Pods and
nothing else.

That single entry is what drives Kueue's per-PodSet flavor assignment:

1. Each PodSet forms its own assignment group by default (`groupKey := strconv.Itoa(i)`,
   `flavorassigner.go:655`), so **two PodSets of one Workload can be assigned two different
   flavors**.
2. `checkFlavorForPodSets` (`:958`) evaluates a candidate flavor per PodSet, matching
   `flavorSelector(&podSpec, flavorLabelKeys)` against `flavor.Spec.NodeLabels`.
3. An accelerated `ResourceFlavor`'s `spec.nodeLabels` carries exactly this key
   (`nodefeature.ExtractNodeFlavors`: `systemname.ManagedLabelKey`, `kubernetes.io/os|arch`, the
   paired CPU key, `acceleratable.feature.gpustack.ai/<aKey>` and its `.count` sibling).

**Why the field is validated against the pool's offered keys rather than left free-form.** Step 2 is
the reason: `flavorSelector` (`:1027`) keeps only those nodeSelector keys that appear in **that
flavor's own** `NodeLabels` and **drops the rest**. A key no flavor carries is therefore not a
constraint that fails — it is a constraint that is ignored. Kueue would assign an arbitrary flavor,
the Workload would be admitted, and the Pod would then sit `Pending` at the scheduler because the real
Node label does not match. The failure surfaces two gates downstream of the mistake, with nothing
naming it.

So the webhook resolves the role's `instanceType` to its ClusterQueue, reads the pool's live
`ResourceFlavor`s, and rejects a key none of them offers, naming the offered set. A pool that has no
flavors yet (a fresh cluster) is **not** a rejection — the key may become valid in a minute — it is
accepted, and the mismatch then shows up as F6's `Retry`, which is the transient-shortage path
already.

Acceptance:

- A role with `acceleratorKey: nvidia-h20` renders exactly one added nodeSelector entry,
  `acceleratable.feature.gpustack.ai/nvidia-h20: "true"`, and no other Pod-spec change.
- Omitting `acceleratorKey` renders no nodeSelector entry: the role takes whatever the pool assigns,
  which is the single-role behaviour and stays the default.
- A key the pool's flavors do not offer is **rejected at admission**, and the message lists the keys
  the pool does offer.
- A key that is well-formed but the pool has **no flavors at all** is accepted (no rejection on an
  empty read — an empty list is not evidence of absence when the pool is still being built).
- Given two flavors of two models in one ClusterQueue and two roles naming them, the Workload's
  `status.admission.podSetAssignments` shows **two different flavors** — asserted in envtest against
  fake flavors, since the local cluster carries one model.
- The field is **not** expressible through `roles[].template`: a template carrying a nodeSelector is
  not possible (the type has no such field) and this spec does not add one.

> **Whether the field survives is open.** On a pool derived the default way it has nothing to choose
> between: an InstanceType's identity is model-level, so the queue offers flavors of one accelerator
> model and the only key the field can legally carry is the one the pool would have assigned anyway.
> It discriminates only inside a pool an administrator authored to span models.
>
> If per-role queues land instead (the route recorded in the heterogeneous-P/D issue, which takes
> `roles[*].instanceType` off its must-agree rule), the role's own `instanceType` carries the hardware
> choice and this field has no remaining reader. Withdrawing it is free for exactly as long as
> `ModelDeployment` stays unreleased, so the decision is **not urgent and should not be made by
> default** — but it is open, and the field's presence is not evidence that it is load-bearing.

#### F5 — `roles[].kind`, and the connector term it selects

**Why this is in the spec at all, stated as sharply as it can be: without it G1 is not falsifiable,
and an acceptance criterion that cannot fail is not an acceptance criterion.** G1 claims a two-role
deployment is admitted atomically. If both roles render the same engine configuration, the two roles
are not prefill and decode — they are four replicas of one role that happen to share a Workload — and
the demonstration in F9 would pass identically for a deployment that has nothing to do with P/D. The
field is therefore not scope that could be trimmed; it is what makes the spec's own headline claim
checkable. It is a **definitional** part of the scope, not a convenience.

`kind` is what the operator tells the engine this role is: `server` (default, the single-role shape),
`prefill`, or `decode`. It is a **structured enum, not the role's name**, for the reason the
single-role spec gave for putting the reuse domain on an admin object: a semantic that can be reached
by typing a string is a semantic one typo away from silently changing. `name` is free-form and
identifies the PodSet; `kind` is closed and selects behaviour. A deployment may run two decode roles
with different names and one `kind`.

**And the depth is capped, deliberately.** The test is: this spec must make the two roles
**distinguishable in configuration**, and nothing more. It must **not** make them *actually hand KV
blocks to each other* — that needs a transport this spec has no hardware to verify, and the Non-Goals
already say no RDMA claim is made. The two statements have to agree, so they are stated together:

| | In scope here | Not here (a later spec) |
|---|---|---|
| the engine is told which role it is | one rendered discriminator per `kind` | |
| transport negotiation between the roles | | yes |
| pairing a prefiller with a decoder | | yes |
| a router that steers a request between them | | yes |

Under `connector: auto`, the operator's synthesized transfer configuration gains **one term** — the
role discriminator the engine's KV-transfer configuration takes (vLLM's `kv_role`, and its
per-engine equivalents). Everything else is the single-role spec's table, unchanged, and the key is
added to that spec's **owned-key** catalogue keyed by `(engine, key)`, so a user-supplied duplicate
is rejected rather than merged — the same rule, one more row.

**What is asserted is exactly the top row of that table, and it is narrow and checkable:** the
rendered configuration carries the role discriminator its `kind` selects, each engine's form is
pinned by a golden fixture, and a `kind` the engine's rendering does not know is refused at admission
rather than rendered into a config the engine will reject at startup.

`kind: server` alongside any other kind is refused: "one plain server plus a prefiller" is not a
shape anything consumes, and accepting it would mean rendering a connector configuration whose
meaning is undefined.

Acceptance:

- `kind` defaults to `server` through a CRD schema marker; a deployment written before this spec (one
  role, no `kind`) renders **byte-identically** to what the single-role spec renders — asserted
  against that spec's golden fixture, so this spec provably does not change single-role behaviour.
- For each supported engine, a `prefill` role and a `decode` role render the engine's own role
  discriminator, asserted against one golden fixture per (engine, kind).
- The discriminator key is in the owned-key table: a user supplying it in `extraArgs` or `env` is
  rejected naming the key, exactly as the other owned keys are.
- A `kind` the current engine rendering has no term for is rejected at admission, naming the engine
  and the kind — not rendered and left to fail at container start.
- `kind: server` **beside a non-server kind** is rejected: a server serves whole requests by itself,
  so the combination describes no arrangement. Several roles that are all `server` are accepted —
  they are ordinary replicas of one shape, which is what a deployment written before disaggregation
  existed looks like when it grows a second role.

#### F6 — Gate 3 becomes per-role: a demand is judged against its own role's cards

> **Shipped independently, ahead of the rest of this spec, as `4d88673b`.** One file, no API
> change, no dependency on the single-role spec or on cross-model pools — it repaired a defect that
> was reachable before it. See [F6 shipped first, on its own](#f6-shipped-first-on-its-own).

`NodeDevicesAdmissionReconciler` reads the pool's `Devices` ledger after Kueue reserves quota and
reports `Ready` or holds with `Retry`. **When this was written** it was sound for a single-flavor
Workload and **unsound for a multi-flavor one**, in two independent ways, both read off the code at
the time. Both are closed by T10 (`4d88673b`); the analysis is kept because it is that task's design
record, and the tense is past because a reader who lands here from a grep never sees the banner
above:

1. **Demands lose their PodSet.** `parseFamilyDemands` (`:75`) walks every PodSet and folds each
   one's demand into a flat list; `mergeDemand` (`:198`) sums the card counts of two PodSets whose
   family, per-card units and profile agree. For two roles on one flavor that is correct — they
   genuinely compete for the same cards. For two roles on two flavors it merges demands that must be
   satisfied from **disjoint** card populations.
2. **Cards lose their flavor.** `candidateDevices` (`:554`) lists the `Devices` of every node matching
   **any** assigned flavor's `nodeLabels`, and `collectCards` (`:273`) flattens every accelerator of
   every one of those nodes into one list with no provenance — not even the accelerator's model.
   `servesFamily` then filters by *family* only.

Together: a Workload whose prefill role needs 2 H20 cards and whose decode role needs 2 L40S cards is
reported `Ready` when the pool has 4 free L40S and 0 free H20. Kueue admits it, and the prefill Pods
sit `Pending`. The over-admission the check exists to prevent, in the case the check was written
before.

The second way was also reachable **then** without any of this spec: a node carrying two accelerator
models publishes one `Devices` object holding both models' cards, so a single-flavor Workload's card
population already included cards of a model its flavor is not about. T10 fixed both, because
fixing the first without the second leaves the correlation meaningless.

The change, stated as behaviour rather than as code:

- A demand is carried **with the PodSet it came from**, and two PodSets' demands merge only when they
  were assigned the **same flavor** (in addition to today's family/units/profile agreement) — so
  co-located roles still share a budget and disjoint roles do not.
- A card is carried **with the flavor(s) whose node selector and accelerator key it satisfies**, and a
  demand is fitted only against cards its own PodSet's flavor covers.
- The **per-card budget stays global to the Workload**. Two roles assigned the same flavor must not
  both spend the same card, and a card that backs two flavors (a mixed-model node) must not be spent
  twice. The existing `[]cardBudget`, indexed per card, already expresses this; only the eligibility
  filter changes.
- The verdict is still **`Retry`, never `Reject`**, and the message names the role whose demand did
  not fit. A group-wide "not enough cards" that does not say which role is a message an operator
  cannot act on.

Acceptance:

- Two roles on two flavors, cards free only under one of them → `Retry`, and the message names the
  starved role. Today's code returns `Ready` for this input; the test is written to fail before the
  change.
- Two roles on the **same** flavor, cards enough for one role only → `Retry` (the budget is shared,
  as today).
- Two roles on the same flavor, cards enough for both → `Ready` (the budget is shared and sufficient).
- A single-role Workload's verdict is **unchanged** on every existing case — the existing table is
  re-run untouched, and it is the guard that this is an extension and not a rewrite.
- A mixed-model node: a demand under flavor A is not satisfied by a card of model B on the same node.
  This case fails today and is the one that is not conditional on multi-role.
- The check still skips an admitted, evicted, finished or deactivated Workload exactly as today; the
  eviction interlock is untouched.

#### F7 — `roles` becomes 1..10: where the bound lives, and why 10

The single-role spec put `minItems=1` in the CRD schema and the "exactly 1" rule in the **webhook**,
so that lifting it is a webhook edit rather than a schema change every stored object must survive,
and so the refusal can carry an actionable message. This spec spends that seam: the length-1 predicate
is deleted and replaced by an upper bound of **10**.

Ten is Kueue's, not ours: `WorkloadSpec.PodSets` carries `+kubebuilder:validation:MaxItems=10`, as
does `Admission.PodSetAssignments`. An eleventh role produces a Workload the API server will not
store, which surfaces as a Workload that is never created — F2's silent shape again. The bound stays
in the **webhook** for the same reason the length-1 rule did: it is an upstream-derived number that a
Kueue bump can move, and tracking it should not require a CRD schema change.

Additionally the webhook enforces, on `roles`:

- **names are unique** and match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` with `maxLength=63` — the
  `PodSetReference` constraints, because the name becomes the PodSet name (F1). A duplicate name
  would merge two roles into one PodSet, which is F1's silent failure.
- `replicas ≥ 1` per role (already the schema's `minimum=1`), and Σ `replicas` is what
  `pod-group-total-count` carries.

Acceptance:

- Two roles are accepted; eleven are rejected with a message naming Kueue's 10-PodSet limit as the
  cause, not merely stating the number.
- Duplicate role names are rejected, naming both the field and the PodSet-collapse consequence.
- A role name that is a valid Kubernetes name but not a valid `PodSetReference` (an uppercase letter,
  a dot) is rejected, naming the pattern.
- The single-role spec's unit test asserting that "the remaining validation accepts two roles" now
  asserts the full path accepts them — the seam it left is consumed, and the test is updated rather
  than deleted.

#### F8 — Status: per-role kind, per-role assigned flavor, one group-level `QuotaReserved`

`status.roles[]` gains two fields and `QuotaReserved` gains a reason.

- **`kind`** echoes the spec, so an operator reading status alone sees which role is which.
- **`assignedFlavor`** is a **pointer**, read from the group Workload's
  `status.admission.podSetAssignments[<role>].flavors`. It answers Story 4: *which accelerator model
  did this role actually get*. It is `nil` — **absent, not empty-string** — while no assignment exists,
  because "not assigned yet" and "assigned to a flavor named the empty string" are different facts and
  a zero value would collapse them. This follows the convention the KV cache backend spec established
  and verified on real hardware: an observed field read from outside uses a pointer to express
  "not read", and staleness is declared by a condition rather than by clearing the value.
- **`QuotaReserved`** is now a statement about the group's **one** Workload — `True` when that
  Workload has a quota reservation, which by construction covers every role. Its `False` reasons gain
  `PodGroupIncomplete` (F2: fewer Pods than the group declares, so no Workload exists at all); the
  existing reason naming the ClusterQueue is unchanged.

No new condition type is declared, and status is still rebuilt wholesale from observed state every
reconcile.

Acceptance:

- `status.roles[i]` carries `name`, `kind`, `desired`, `ready`, `unmanaged` and `assignedFlavor`.
- `assignedFlavor` is **absent** before admission and **present** after, on the same object across two
  reconciles; a test asserts it is a pointer and that the un-assigned case is `nil`, not `""`.
- A group with 3 of 4 Pods has `QuotaReserved=False` / `PodGroupIncomplete`; a full group whose
  Workload is inadmissible has `QuotaReserved=False` with the reason naming the ClusterQueue. The two
  are distinguishable from status alone.
- `QuotaReserved=True` implies **every** role is covered — asserted by a case with two roles where the
  single Workload holds the reservation, so the condition cannot be true for one role and not the
  other.

#### F9 — Atomic admission is the claim, and it is demonstrated by starvation

The all-or-nothing property (G1) cannot be shown by a happy path: a deployment that fits is admitted
whether admission is atomic or not. It is shown by a pool that **cannot** fit the whole group.

The demonstration, on a local Kubernetes cluster: a pool with room for one role's demand, a two-role
deployment needing both, and the assertion that **no** Pod of either role is ungated and the
deployment reports `QuotaReserved=False` naming the ClusterQueue. Then room is made, and both roles
start.

The falsifiable half is the first one: under the independent-Workload behaviour the single-role spec
produces, one role would start. The case therefore asserts a **negative** — zero running Pods across
both roles while the pool is short — and records the observed Workload state.

Acceptance:

- On a pool short of the group's demand: zero Pods of either role are running, one Workload exists
  with two PodSets, and it holds no quota reservation. Recorded in the Test Plan.
- After the pool gains room, both roles reach `ready == desired` and `phase` is `Ready` — asserted in
  the same case, so the first half cannot pass by the deployment simply being broken.
- The per-accelerator check's `Retry` path is exercised in the same shape: quota sufficient, cards
  fragmented, verdict `Retry`, and the message names the starved role (F6).

#### F10 — A change to the group's shape rebuilds the group

Any change to a role's `replicas`, or to the set of roles, changes
`pod-group-total-count`, which **every** Pod of the group carries and which
`validatePodGroupMetadata` (`:857`) requires them all to agree on. A group whose Pods disagree is an
unretryable compose error — the same no-Workload state as F2.

This spec's policy is therefore **rebuild**: such a change deletes the group's Pods and recreates them
with the new total. It is the single-role spec's recreate rollout policy applied to the axis this
spec adds, and it inherits that policy's stated cost — a replica leaving loses its cached KV blocks to
its siblings, and the deployment records an event for it.

A softer in-place path (grow the total, patch the annotation on every existing Pod, add the new Pods)
is **not** specified here, because whether Kueue 0.18.4 accepts a growing count on an admitted serving
Workload without evicting it is not established from the sources — `equivalentToWorkload` compares
PodSet counts, and what it does on a mismatch for a serving group is a behaviour to observe, not to
assume. See Open Questions.

Acceptance:

- Changing `roles[0].replicas` from 2 to 3 deletes and recreates the group's Pods; every Pod ends up
  carrying `pod-group-total-count: "5"`, and the deployment returns to `Ready`.
- At no point do two live Pods of the group carry different `pod-group-total-count` values —
  asserted by observing the intermediate state, because that is the state that would produce the F2
  hang.
- Adding a role rebuilds the group; removing one rebuilds it; each records the per-replica event the
  single-role spec's rollout policy defines.

### Verification

**Hardware: a local Kubernetes cluster is sufficient for everything this spec asserts. No RDMA, no
cloud, and no second accelerator model is required.**

Two things are deliberately *not* verified on hardware, and both are Non-Goals:

- **Heterogeneous P/D placement.** It needs two accelerator models of one manufacturer in one pool,
  and it needs the cross-model pool that is not this spec's. It is verified in **envtest against two
  fake `ResourceFlavor`s**, which is where the property actually lives — the assertion is "Kueue
  assigned two different flavors to the two PodSets", and that is a Workload-status assertion, not a
  hardware one.
- **Anything about KV transfer rate or transport.** No number is claimed.

The ladder, cheapest first:

| Level | Vehicle | What it settles |
|---|---|---|
| unit | table-driven tests over the render, the webhook rules, the per-role feasibility fit and the status build | every rule in F1–F8 that needs no cluster |
| integration | envtest: the reconciler and the webhook against fake flavors and a fake pool | the two-flavor assignment (F4), the incomplete-group state (F2), the group rebuild (F10) |
| e2e | the dev image on a local cluster, `.claude/skills/gpustack-operator-e2e/cases/` | the cases in the Test Plan, including F9's starvation demonstration |

**F9's starvation case is the one that cannot be faked at a lower level.** A fake client can be made
to report anything; only a real Kueue on a real pool proves that a short pool leaves both roles
queued. Its observations are recorded in the Test Plan when the case runs.

### Notes / Constraints / Caveats

- **`Status: Specified` is a drafting state and must not reach `main`.** Across every spec in
  this repository the statuses used on first landing are `Shipped`, `Building`, `Planned` and
  `Built` — **`Specified` has never appeared on `main`**. Nothing enforces this (`make lint docs`
  does not cover `specs/`), so whoever lands this file sets the status to what is then true:
  `Planned` if the work has not started, `Building` if it has. This spec's own history is the
  cautionary case — it was opened as a pull request while still `Specified`, and closed again for
  that reason among others.
- **The Go type library and the Kueue running in the cluster are not pinned to the same version, and
  the deployed one moves on its own schedule.** `hack/deps.sh` pins the chart's Kueue subchart;
  `go.mod` resolves `sigs.k8s.io/kueue` separately, and behind it. That is a standing condition
  rather than a defect this spec closes — the cluster's version is scheduled by whoever runs the
  cluster, and it is expected to move again. Every runtime behaviour this spec depends on — the
  pod-group PodSet construction, the fast-admission trap, the serving annotation, the PodSet cap,
  the per-PodSet flavor assignment — is a property of the **running** one. So read any "how many" or
  "is this supported" answer off the installed CRD and the running controller, never off the marker
  in the vendored type; take a Go **constant** from the version being compiled against, because that
  is the one that has to compile. The two are not interchangeable, and a bump of either is a
  re-verification.

  Three consequences for the tasks below. None of them is a version detail to track: each is stated
  as the rule that survives the next bump.

  - **Take the PodSet cap from the cluster, never from go-to-definition.** F1's 1..10 rule is built
    on what `Workload.spec.podSets` accepts on the **installed CRD**. The Kueue types this
    repository compiles against carry a **smaller** cap than the deployed CRD enforces, so an
    implementer who checks the number the obvious way — following `kueue.PodSet` through this
    repository's own imports, which is what an editor's go-to-definition does — reads a number too
    small and concludes the rule is wrong by two. Read it from
    `kubectl get crd workloads.kueue.x-k8s.io`, and read it again after any Kueue bump. The
    criterion is *whatever the running one accepts*, not a number written here.
  - **Write the group name on the LABEL.** Kueue also accepts it as an annotation of the same name
    and prefers the annotation where that is configured, but every reader — the field indexer, both
    event handlers, the validating webhook — falls back to the label. The label is therefore the one
    carrier read on every configuration; a Pod carrying only the annotation is invisible to some of
    them. F1 writes the label for that reason, not by preference.
  - **The four functions G1 rests on do not move.** `constructGroupPodSets`,
    `constructGroupPodSetsFast`, `validatePodGroupMetadata`'s `UnretryableError` branch with its
    per-Pod `queue-name` agreement loop, and `QueueNameForObject` were compared across the versions
    in play and are the same code, down to two extracted helpers and an event-recorder API rename.
    Recorded so that G1 is known not to rest on a coincidence of one release — not as an invitation
    to re-open the comparison at the next bump.
- **`pod-group-fast-admission` must never be set on these Pods.** It is not a performance knob
  here, it is a correctness break: it collapses the whole group into one PodSet. Recorded as an
  acceptance item of F1 rather than only as a note, because a note is read once.
- **A pre-existing defect in `buildResourceGroups`, recorded for whoever makes pools span models —
  and the limit it is built on is misread.** Two facts, both from Kueue 0.18.4's
  `apis/kueue/v1beta1/clusterqueue_types.go`:

  | Field | Limit | The comment beside it |
  |---|---|---|
  | `ClusterQueueSpec.ResourceGroups` | `MaxItems=16` | *"resourceGroups can be up to 16, with a max of 256 total flavors across all groups"* |
  | `ResourceGroup.Flavors` | `MaxItems=64` | *"can contain up to 64 flavors, with a max of 256 total flavors across all resource groups"* |

  **16 bounds the number of resource groups; a single group holds up to 64 flavors.** This
  repository reads it the other way round — `node_queue.go:280` states *"A resource group holds at
  most 16 flavors"* — and `buildResourceGroups` therefore opens a **second group with the same
  `CoveredResources`** at 16 flavors. That update cannot succeed: `validateResourceGroups` declares
  its `seenResources` set **outside** the per-group loop, so a resource name repeated in a later
  group is a `field.Duplicate` and Kueue's ClusterQueue webhook rejects the `Update`.

  So the fix is **not** to give the second group a different resource name. A queue in this
  repository's model covers exactly one resource (`credits.gpustack.ai/<manufacturer>`, or `cpu`),
  and since no two groups may cover the same resource, such a queue **can only ever have one
  resource group**. The splitting is the bug; the real ceiling is **64 flavors in that one group**,
  and that is the bound that needs handling.

  Unreachable at a model-level pool — nobody has 17 flavors of one model — but a manufacturer-level
  pool aggregating every model across every node-count is far closer to 64 than a model-level one is
  to 17. **This is a finding about the existing scheduling chain, not a change this spec makes**, and
  it is not in any task's `Owns` here. It is recorded because the cross-model-pool work is what would
  trip it, and that work needs the corrected number before it starts, not after.
- **Credits are a card count and were never an equivalence claim.** `CreditsPerAccelerator` equals the
  global denominator (`nodefeature.knowns.go`), and `AcceleratorsToCredits(n) = n × B` — one H20 and
  one L40S are each exactly `B` credits. So a cross-model pool's `credits` total means "how many cards
  of this manufacturer are in this queue", which is precise. It has never meant "how much compute", so
  summing across models does not distort a meaning it was carrying. This is why gate 2 stays coarse
  and gate 3 (F6) does the per-card work — the layering the five gates were designed around.
- **`CoveredResources` needs no change for cross-model pools.** `buildResourceGroups` derives it from
  `acceleratable` and the pool's `manufacturer` note alone; both are identical across models of one
  manufacturer. Kueue's "flavors in one group must cover the same resources" rule is therefore
  satisfied without any change to that function.
- **A ClusterQueue resolves its flavors by labels, not by name.** `node_queue.go` states it: *"Admin
  queue names are arbitrary, so the pool is resolved by labels, not by name."* Nothing in this spec
  keys on a queue name.
- **Gate 1 is unchanged and still applies per Pod.** The Pod webhook's objectSelector is the presence
  of the `kueue.x-k8s.io/queue-name` label (`pkg/worker/webhooks/worker/pod.go:38`), which every group
  Pod carries. The seven request rules, the memory fold and the "exactly 1" cap on `.sliced` /
  `.partitioned` apply to each role's Pods exactly as to any other Pod. This spec adds no Pod-webhook
  rule.
- **Gate 3's file lives under `controllers`, not `webhooks`.** It is
  `pkg/worker/controllers/worker/node_devices_admission.go` — an AdmissionCheck *controller*
  (`NodeDevicesAdmissionReconciler`), not a webhook. There is no
  `pkg/worker/webhooks/worker/node_devices_admission.go`.
- **No chart files and no chart RBAC**, for the same reasons the single-role spec records: CRDs are
  generated into `api/worker/v1alpha1/zz_generated.crds.go` and installed by the worker at startup;
  webhook configurations are generated from `+k8s:webhook-gen:` markers; the worker's ServiceAccount
  is bound to `cluster-admin`. `deploy/gpustack-operator/chart/**` is in no task's `Owns`.
- **Defaults go in the CRD schema**, so the validating webhook stays the whole admission surface:
  `+k8s:validation:default="server"` on `kind`, and `+k8s:validation:enum` for its values.
- **`make generate` is run once for the API change**, and it regenerates deepcopy, register,
  apiservice, CRDs, conversion, protobuf and the webhook registration. It must run from a
  module-suffixed physical path (a worktree path does not satisfy it) and it drifts the binding tree,
  so the API-shaped work is deliberately batched into a single task.
- **Go names stay snake_case per file** (`model_deployment_pod_group.go`), never flat-concatenated.
- **One test name to correct the next time F6's tests are opened — deliberately recorded here
  rather than on a checklist.** The case `a partition is not feasible against an unpartitioned card`
  (`node_devices_admission_test.go`) overstates its own scope: what it constructs is a card **with
  no partition profile**, which is a *capability*, while "unpartitioned" reads as the allocation
  *mode*. Those are different fields — `Mode` is status, the profile ledger is capability — and a
  card can be `Mode: None` (not currently partitioned) while still being partition-**capable** with
  free placements, which a sibling case relies on. Renaming the case, or the `unpartitioned`
  fixture, to say *no partition profile* removes the conflation.

  It causes no defect today, so it is not a merge gate. It sits in this spec's Notes rather than in
  a task list because the fix belongs to whoever next opens F6's tests — and that person will read
  this file, whereas nobody is obliged to read a checklist at the right moment. It is the same rule
  as the wording-scope one below, with the overstated wording being a **test name** rather than an
  error message.

- **Every message that claims a scope needs an assertion pinning that scope.** A verdict's wording
  and the predicate behind it drift apart silently: a partition verdict computed over *one role's
  own model* once read as "this pool has no partitioned card", which sends an operator to audit a
  pool that is healthy. A message asserted only through the *state* it accompanies is unpinned in
  the same way — several independent exclusions produce one state, so a case claiming a particular
  exclusion passes on any of them. Both halves are needed: say the scope the predicate actually
  checked, and assert it.

- **External references:**
  - Kueue — plain Pods and pod groups: <https://kueue.sigs.k8s.io/docs/tasks/run/plain_pods/>
  - Kueue — ClusterQueue resource groups and flavors:
    <https://kueue.sigs.k8s.io/docs/concepts/cluster_queue/>
  - Kueue — admission checks: <https://kueue.sigs.k8s.io/docs/concepts/admission_check/>
  - vLLM — disaggregated prefilling / KV transfer configuration: <https://docs.vllm.ai/>
  - Mooncake Store design: <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html>

### Boundaries

- **Always:** keep every role of one deployment in one ClusterQueue and one Workload; write the role
  name into `role-hash` so PodSet identity is role identity; set `pod-group-serving`; create the whole
  group in one pass; correlate a feasibility demand with the flavor its own PodSet was assigned; state
  a refusal's cause, not just its rule; express an unread observation as an absent pointer.
- **Ask first:** anything that adds a field to `Instance`, `InstanceSpec` or `InstanceTemplate` (both
  CRDs move); anything that changes how a pool's identity or its ResourceFlavor labels are computed
  (that is the cross-model-pool work, not this); adding a chart manifest or RBAC rule; adding a second
  admission check; adding a mutating webhook; making `roles[].template` carry any scheduling input.
- **Never:** set `pod-group-fast-admission`; let two roles carry different `instanceType`; let a role
  name repeat within a deployment; render a nodeSelector key no flavor of the pool declares; judge a
  role's feasibility against another role's cards; report `QuotaReserved=True` while any role is
  unadmitted; express "no flavor assigned yet" as an empty string; assert a throughput or transport
  claim this spec cannot verify.

### Risks and Mitigations

- **A replica that is asked to leave cannot leave** → measured, then fixed. Kueue holds a finalizer on
  every Pod of a group and releases it when the group finishes or when the Workload is deleted, and a
  group annotated `pod-group-serving` is one Kueue defines as never finished. The Workload is owned by
  those same Pods without a controller reference, so garbage collection is blocked behind them in
  turn. Deleting a replica and waiting is a wait with no end, on the teardown path, the F10 rebuild
  and an ordinary template edit alike — with nothing erroring on either side. The operator now deletes
  that Workload. What that costs is no longer a risk but a **declared contract**: any departure
  rebuilds the group, decided under *Kueue-native replacement instead of rebuilding the group* in
  Alternatives, and stated for operators in the table in
  [`docs/reference/model-deployment.md`](../docs/reference/model-deployment.md) naming the four
  departures an operator does not initiate.
- **Two identically-shaped roles collapse into one PodSet** → the `role-hash` annotation makes PodSet
  identity explicit, and the test for it is written with two roles whose *only* difference is their
  name, so it fails if the annotation is dropped. Without it the failure is silent: a working
  deployment with wrong accounting.
- **A partially created group produces no Workload and no diagnosis** → F2 makes it a named condition
  reason with counts, and the reconciler creates the whole group in one pass. This is the failure the
  single-role spec's Alternatives already identified as *"a silent hang, not an error"*; it is now
  observed to emit a `Warning` event with reason `ErrWorkloadCompose`, so it is not entirely silent —
  but there is still no Workload, which is what the condition is for.
- **`acceleratorKey` is written for a model the pool does not offer** → rejected at admission with the
  offered set named. Left unvalidated, `flavorSelector` would silently drop the key and the Pod would
  be admitted then left Pending, two gates from the mistake.
- **Gate 3's change regresses the single-role verdict** → the existing test table is re-run unchanged
  as an explicit acceptance item, and the new behaviour is additive filtering rather than a rewrite of
  the fit.
- **The heterogeneous path cannot be exercised on hardware** → it is verified in envtest against two
  fake flavors, and the spec says so rather than implying a coverage it does not have. The property
  being verified (two PodSets, two flavor assignments) is a Workload-status property, so the fake is
  faithful to what is being claimed.
- **A `kind` term is rendered for an engine version whose flag has moved** → the three-tier override
  the single-role spec built is the mitigation, and the key is `owned`, so a user who needs to replace
  it takes over the command line and the role is marked `Unmanaged` — visibly, rather than by a silent
  merge.
- **The group rebuild costs more cache than an in-place scale would** → stated rather than hidden
  (F10), and the softer path is an Open Question with the specific behaviour that has to be observed
  before it can be chosen.
- **A Kueue bump changes a behaviour this spec reads** → every such behaviour is cited by file and
  symbol so a bump has a checklist, and the skew between the deployed chart and the Go module is
  recorded in *Notes* as a standing condition rather than discovered later.
- **The cross-model pool work trips `buildResourceGroups`' group split** → recorded in Notes with the
  exact Kueue validation that would reject it **and with the limit corrected**: 16 bounds resource
  *groups*, not flavors per group, and a single group holds 64. The repository reads it the other way
  round, so the split it performs is itself the bug and the real ceiling is a different number. Known
  before that work starts rather than found by a ClusterQueue update failing in a live cluster.
- **This spec's own justification is only fully exercised with the cross-model pool** → the split in
  *Dependencies* is explicit: atomic admission, the pod group, the per-role feasibility fix and every
  refusal land and are testable today; only heterogeneous placement waits.

## Design Details

### Commands

Build and test run locally on darwin; nothing here is CGO or linux-only.

```bash
go build ./api/... ./pkg/worker/...
go test ./api/worker/v1alpha1/... \
        ./pkg/worker/controllers/worker/... \
        ./pkg/worker/webhooks/worker/...
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
make lint                     # golangci-lint over the whole module
make lint docs                # the documentation contract
```

Code generation runs from a module-suffixed physical path; a worktree path does not satisfy it, so
the generator runs in the main checkout and the delta is synced back. When syncing with `rsync`, use
`--filter='P .git'` and **not** `--exclude '.git/'` — a worktree's `.git` is a *file*, which the
latter misses, and combined with `--delete` it destroys the receiver's repository.

```bash
make generate                 # T1 only — from the main checkout
git diff --stat api/ pkg/worker/webhooks/
```

End-to-end runs against a local Kubernetes cluster through the existing e2e skill:

```bash
# from .claude/skills/gpustack-operator-e2e/
bash cases/case-49.sh    # the group forms: one Workload, two PodSets, both roles Ready
bash cases/case-50.sh    # F9: a short pool leaves BOTH roles queued; room is made; both start
bash cases/case-51.sh    # every refusal fires, and the group rebuild converges
```

**These case numbers are re-checked at the start of implementation.** `cases/` currently holds
1–32 and 34–42 (33 is retired and is not reused); 43–44 and 45–48 are claimed by two specs drafted
the same week as this one. Three drafts taking numbers from one snapshot have collided before, so the
first task of the e2e work re-reads the directory and re-numbers if needed.

### Project Structure

```
api/worker/v1alpha1/
  model_deployment.go                       # + Kind, AcceleratorKey on the role; + AssignedFlavor,
                                            #   Kind on the role status (T1)
  zz_generated.crds.go / .deepcopy.go /
  zz_generated.register.go /
  generated.proto / generated.pb.go         # regenerated (T1)

pkg/worker/controllers/worker/
  model_deployment.go                       # whole-group creation, the rebuild policy (T4, T9)
  model_deployment_pod_group.go             # the group's labels/annotations + the group name (T3)
  model_deployment_render.go                # + the acceleratorKey nodeSelector (T5)
  model_deployment_connector.go             # + the per-kind discriminator and its owned key (T6)
  model_deployment_service.go               # + one Service per role (T7)
  model_deployment_status.go                # + kind, assignedFlavor, PodGroupIncomplete (T8)
  node_devices_admission.go                 # per-role feasibility (T10)

pkg/worker/webhooks/worker/
  model_deployment.go                       # roles 1..10, unique names, identical instanceType,
                                            #   acceleratorKey against the pool, kind rules (T2)
  zz_generated.webhooks.go                  # unchanged: the registration already exists, and T2
                                            #   adds rules rather than a webhook

docs/
  reference/model-deployment.md             # the P/D section (T11)
  architecture/admission.md                 # gate 3 is per-role (T11)
  architecture/scheduling-chain.md          # per-PodSet flavor assignment (T11)

.claude/skills/gpustack-operator-e2e/cases/
  case-49.sh .. case-51.sh                  # T12, T13
```

Every controller file except `node_devices_admission.go` is created by the single-role spec; this
spec extends them. `model_deployment_pod_group.go` is new so that the group's metadata — the one
thing a reviewer must be able to read in one place — is not scattered through the render.

### Code Style

The API additions, following the file's discipline: a doc comment states behaviour and the reason
for it rather than restating the field name, and a rule that exists because of an upstream mechanism
names that mechanism.

```go
// ModelDeploymentRoleKind is what the operator tells the engine a role is. It is a closed enum
// rather than the role's free-form Name because a semantic reachable by typing a string is a
// semantic one typo away from changing: Name identifies the PodSet, Kind selects behaviour.
//
// +k8s:validation:enum=["server","prefill","decode"]
type ModelDeploymentRoleKind string

const (
	// ModelDeploymentRoleKindServer is a role that both prefills and decodes. It is the default and
	// the only kind a single-role deployment may carry.
	ModelDeploymentRoleKindServer ModelDeploymentRoleKind = "server"
	// ModelDeploymentRoleKindPrefill produces KV blocks for a decode role to consume.
	ModelDeploymentRoleKindPrefill ModelDeploymentRoleKind = "prefill"
	// ModelDeploymentRoleKindDecode consumes KV blocks a prefill role produced.
	ModelDeploymentRoleKindDecode ModelDeploymentRoleKind = "decode"
)

// ModelDeploymentRole is one engine role and its replicas.
//
// EVERY ROLE OF ONE DEPLOYMENT SHARES ONE InstanceType, AND THAT IS NOT A STYLE RULE. All the roles'
// Pods form one Kueue pod group, which becomes one Workload, and a Workload carries a single
// queueName — so roles on two InstanceTypes cannot be admitted atomically, which is the entire
// reason the group exists. Differentiating the hardware WITHIN one pool is AcceleratorKey's job.
type ModelDeploymentRole struct {
	// Name identifies the role. It becomes the Kueue PodSet name, so it is constrained to Kueue's
	// PodSetReference shape and must be unique within the deployment: two roles sharing a name would
	// be folded into one PodSet, losing per-role counting, flavor assignment and status without
	// producing an error.
	//
	// +required
	// +k8s:validation:maxLength=63
	// +k8s:validation:pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Kind is what the engine is told this role is; it selects the role discriminator the operator
	// adds to the synthesized KV-transfer configuration. "server" is the single-role shape and may
	// not be mixed with the others: a deployment of one plain server plus a prefiller has no defined
	// transfer configuration.
	//
	// +k8s:validation:default="server"
	Kind ModelDeploymentRoleKind `json:"kind,omitempty" protobuf:"bytes,7,opt,name=kind"`

	// AcceleratorKey is the accelerator device key — "<manufacturer>-<model>", e.g. "nvidia-h20" —
	// this role's Pods must land on. It renders as the single nodeSelector entry
	// "acceleratable.feature.gpustack.ai/<AcceleratorKey>=true", which is the label an accelerated
	// ResourceFlavor carries in its spec.nodeLabels; Kueue assigns a flavor per PodSet, so that entry
	// is what confines this role to that model while every role stays in one ClusterQueue.
	//
	// It is validated against the keys the pool's live ResourceFlavors offer, because Kueue's flavor
	// assignment DROPS a nodeSelector key no flavor declares rather than failing on it: an unvalidated
	// key would be ignored at admission and only surface as a Pod left Pending by the scheduler, two
	// gates away from the mistake. Empty means the role takes whatever the pool assigns.
	//
	// +k8s:validation:maxLength=63
	AcceleratorKey string `json:"acceleratorKey,omitempty" protobuf:"bytes,8,opt,name=acceleratorKey"`
}

// ModelDeploymentRoleStatus is one role's observed state.
type ModelDeploymentRoleStatus struct {
	// AssignedFlavor is the ResourceFlavor Kueue assigned to this role's PodSet, read back from the
	// group Workload's admission. It is a POINTER because "no assignment yet" and "assigned to a
	// flavor named the empty string" are different facts, and a zero value would collapse an
	// in-flight state into a legal one.
	AssignedFlavor *string `json:"assignedFlavor,omitempty" protobuf:"bytes,5,opt,name=assignedFlavor"`
}
```

Field numbers above are illustrative; the generator's actual numbering is settled in T1, appending to
the existing message rather than renumbering it.

### Implementation Plan

**T10 went first and alone**, in its own pull request, and is merged: it repaired an existing defect
on the admission path, depended on nothing here, and did not wait for a CRD that did not exist then
([F6 shipped first, on its own](#f6-shipped-first-on-its-own)).

Everything else waited for the single-role spec, which has since shipped. Then: T1 lands the API shape alone, so the one
`make generate` this spec needs produces a diff reviewable by itself. T2 (the webhook) and T3 (the
group metadata) fan out from it. T4 is the join — whole-group creation depends on the metadata — and
everything user-visible follows it.

Checkpoints: after T10 (a starved role gets the right verdict — **reached, `4d88673b`**); after T1 (the CRD
accepts the new fields); after T4 (a two-role deployment produces one Workload with two PodSets);
after T8 (status tells the truth); after T13 (atomic admission is demonstrated and recorded).

- [x] **T1 · The API shape and one `make generate`, nothing else**
  Blocked by: the single-role spec's API task
  Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated.*`,
  `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`
  Gate: review
  Acceptance: `ModelDeploymentRoleKind` with its three constants; `Kind` and `AcceleratorKey` on
  `ModelDeploymentRole`; `Kind` and a **pointer** `AssignedFlavor` on `ModelDeploymentRoleStatus`.
  `roles` keeps `minItems=1` and **still carries no `maxItems`** — the 1..10 bound is the webhook's
  (F7). `kind` defaults to `"server"` through a schema marker and carries the enum; `name` carries
  the `PodSetReference` pattern and `maxLength=63`. No controller, no webhook, no `setup.go` change.
  Verify: from the main checkout `make generate`, sync back, then `make generate && git diff
  --exit-code`; `go build ./api/...`; `git diff --stat api/` shows only the type and its regenerated
  files.

- [x] **T2 · The webhook: 1..10 roles, unique names, one instanceType, a valid acceleratorKey**
  Blocked by: T1
  Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test,
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go` — and, as landed,
  `pkg/worker/kvcache/inject/types.go` + `sglang.go` and
  `pkg/worker/controllers/worker/model_deployment_connector.go`, which is where the answer to
  "does this engine's rendering have a term for this kind" lives. Restating it in the webhook would
  have been a second table, agreeing today and diverging on the next engine release.
  Gate: review
  Acceptance: the length-1 predicate is **deleted** and replaced by `1..10`, whose message names
  Kueue's 10-PodSet `MaxItems` as the cause. Role names unique and `PodSetReference`-shaped.
  `roles[*].instanceType` identical, the message stating the one-queue-name reason and naming
  `acceleratorKey` as the alternative. `acceleratorKey` resolved against the pool's live
  ResourceFlavors and rejected with the offered set named — but **accepted when the pool has no
  flavors at all**, asserted by its own case so an empty read is never read as a refusal.
  `kind: server` refused alongside another kind; a `kind` with no rendering term refused naming the
  engine. The single-role spec's "two roles pass the remaining validation" seam test is updated to
  assert the full path now accepts them.
  Verify: `go test ./pkg/worker/webhooks/worker/...`; from the main checkout `make generate`, then
  `git diff pkg/worker/webhooks/worker/zz_generated.webhooks.go` is empty (the registration already
  exists; this task adds rules, not a webhook).

- [x] **T3 · The group's metadata, in one file**
  Blocked by: T1
  Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go` + its test
  Gate: review
  Acceptance: a pure function over the deployment returns, per role and per replica, the group label,
  `pod-group-total-count` (Σ replicas), `role-hash` (the role's name) and `pod-group-serving: "true"`
  — and **no label of ours naming the role**, asserted by pinning the exact key set the group
  contributes. `app.kubernetes.io/component` already carries the role's name on every replica, is
  already what a `Service` selector holds, and is already what status reads a Pod's role from, so a
  second selectable carrier would be two answers to one question written by two functions. Adding a
  key in this project's own domain stays available and is additive; removing a published one is not.
  The group name is `metadata.name` when label-valid, else
  `gpustack-fnv64-<fnv64a(namespace/name)>`, with a case for each. A test asserts
  `pod-group-fast-admission` is **absent** and names, in the test's own message, that setting it
  collapses every role into one PodSet.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentPodGroup`

- [x] **T4 · Whole-group creation, and the rebuild policy**
  Blocked by: T3
  Owns: `pkg/worker/controllers/worker/model_deployment.go` + its test — and, as landed,
  `model_deployment_render.go` (the group metadata goes on the Pod before the fingerprint, where the
  connector's annotations already do), `model_deployment_pod_group.go` (the resize predicate belongs
  beside the total it compares against), and `api/worker/v1alpha1/model_deployment.go`, whose
  `Replicas` comment said this version creates no pod group — true until this task, false after it.
  Three tests the single-role spec wrote assert the pre-group scaling policy and are updated here:
  a scale is now a whole-group rebuild, so it takes two passes and deletes every replica rather than
  the trimmed ones.
  Gate: review
  Acceptance: one reconcile pass issues the creates for **every** role's every replica; the pass does
  not stage them behind readiness. Pods are named `<deployment>-<role>-<ordinal>` and owned by the
  deployment. A change to any role's `replicas`, or to the set of roles, **rebuilds** the group
  (F10), and no intermediate state has two live Pods carrying different `pod-group-total-count` —
  asserted by observing the intermediate state, not only the end state. Level-based and idempotent:
  an unchanged spec issues no writes; a hand-deleted Pod is recreated.
  Verify: `go test ./pkg/worker/controllers/worker/ -run 'ModelDeployment_|ModelDeploymentReconciler_'`
  and the envtest convergence case. The second alternative is not decoration: the task's own cases are
  named `TestModelDeployment_…`, but the behaviour it changes is asserted by the single-role spec's
  `TestModelDeploymentReconciler_…` cases, which `ModelDeployment_` alone does not match — and a `-run`
  pattern that matches nothing still exits 0.

- [x] **T5 · Render the per-role node selector**
  Blocked by: T1, T4
  Owns: `pkg/worker/controllers/worker/model_deployment_render.go` + its test
  Gate: review
  Acceptance: a non-empty `acceleratorKey` adds exactly one nodeSelector entry,
  `acceleratable.feature.gpustack.ai/<key>: "true"`, and changes nothing else in the Pod spec —
  asserted by diffing against the render with the field empty. An empty `acceleratorKey` adds
  nothing. The three-tier override and the owned-key rules are untouched.
  Verify: `go test ./pkg/worker/controllers/worker/ -run RenderModelDeploymentPod` — the render's
  cases are named after the function, `TestRenderModelDeploymentPod_…`, which `ModelDeploymentRender`
  does not match. A `-run` pattern that matches nothing still exits 0.

- [x] **T6 · The per-kind connector discriminator**
  Blocked by: T1, T5
  Owns: `pkg/worker/controllers/worker/model_deployment_connector.go` + its test — and, as landed,
  the one line of `model_deployment.go` that hands the role's kind to the synthesis. The
  owned-key half needed no table change: the discriminator is a field inside vLLM's
  `--kv-transfer-config` document, and that argument is already owned, so the webhook already
  refuses a user-supplied one. A case asserts that by DERIVING the carrying argument from the render
  rather than restating its name.
  Gate: review
  Acceptance: the synthesis function takes `kind` and emits the engine's role discriminator, one
  golden fixture per (engine, kind). The key joins the **owned**-key table, so the webhook and the
  renderer read the same data and a user-supplied duplicate is rejected. A `kind: server` render is
  **byte-identical** to the single-role spec's fixture for the same input — the regression guard that
  this spec does not change single-role behaviour.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentConnector`

- [x] **T7 · One Service per role, beside the deployment's**
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_service.go` + its test — and, as landed, the
  reconciler's `syncModelDeploymentService`, which now converges N+1 Services and prunes the ones a
  removed role left behind. An owner reference does not collect those: the deployment still exists,
  so nothing about the reference is stale.
  Gate: review
  Acceptance: in addition to the deployment-wide `Service`, one `ClusterIP` Service per role
  selecting that role's Pods through `modelDeploymentSelectorLabels` — the three identity labels the
  renderer already writes, whose `app.kubernetes.io/component` is the role's name. A selector on the
  role alone would not do: it must also name the deployment, or two deployments in one namespace each
  running a role called `decode` would share endpoints.
  **The "deployment-wide" Service fronts the FIRST role, and stays that way.**
  `renderModelDeploymentService` already selects `modelDeploymentSelectorLabels(md, roles[0])`; the
  single-role spec left the choice here, and this spec's Non-Goals answer it by declining to route.
  No role is a front door in a sense that would make choosing between them meaningful, and a Service
  selecting EVERY role would be worse than arbitrary — it would round-robin a request onto a process
  configured as a producer and one configured as a consumer, the silent wrong answer `kind` exists to
  prevent. A real front door needs the router, which is a later spec.
  Each per-role Service is owned by the deployment and
  named `<deployment>-<role>`. `status.endpoint` stays the deployment-wide one. A single-role
  deployment's rendered Services are unchanged from the single-role spec plus exactly one per-role
  Service, and a test states why a P/D deployment needs per-role addressability at all (a decoder
  must be reachable as a decoder) while nothing in this spec routes between them.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentService`

- [x] **T8 · Status: kind, assignedFlavor, `PodGroupIncomplete`**
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_status.go` + its test — and, as landed, a
  shared `ModelDeploymentEffectiveRoleKind` beside the connector's mapping, which the webhook's own
  copy was replaced by.
  **It also repaired a defect T4 introduced.** The Workload lookup required a CONTROLLER reference,
  which is what Kueue sets for a single Pod; for a pod group it sets PLAIN owner references to every
  member and no controller at all. Once every deployment became a group, that filter matched nothing
  and every deployment would have reported `AdmissionInFlight` forever while being admitted normally,
  with nothing erroring.
  Gate: review
  Acceptance: `status.roles[i]` carries `kind` and `assignedFlavor`, the latter read from the group
  Workload's `status.admission.podSetAssignments` by PodSet name and left **`nil`** when absent — a
  test asserts `nil`, not `""`. `QuotaReserved` is computed from the group's single Workload and
  gains reason `PodGroupIncomplete` with a `<have>/<want>` message when fewer Pods exist than the
  group declares. Status is still rebuilt wholesale each reconcile.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentStatus`

- [x] **T9 · The incomplete-group path, as far as this tree reaches**
  Blocked by: T4, T8
  Owns: the group cases in `pkg/worker/controllers/worker/model_deployment_group_test.go` — and, as
  landed, the create loop in `model_deployment.go`, which had to stop returning at the first failed
  create: the one pass that knows the group is short of its total was the one pass that wrote no
  status.
  Gate: review
  Acceptance: with the group short of its total, **no Workload exists** and the deployment reports
  `QuotaReserved=False` / `PodGroupIncomplete` with a `<have>/<want>` message; the reason clears once
  the group completes. This is the task that proves F2's failure mode is observable rather than a
  hang, so its assertion is the *absence* of a Workload, verified by a List that is first shown to
  work against a control object — a Workload in the same namespace belonging to somebody else,
  without which "no Workload for this deployment" and "the List is broken" are the same result.
  **NOT COVERED HERE, and no case in this file fills it: "once the last Pod is created a Workload
  APPEARS with the expected PodSets."** That is Kueue composing it, and this repository has NO
  envtest harness — `grep -r envtest` over the tree returns nothing, and envtest alone would not
  suffice anyway, since it runs an API server and not Kueue's pod integration. Standing up either is
  a project-level decision rather than this task's. A fake Workload placed by the test would assert
  the test's own placement, so the cases stop at "the group is complete and Kueue has not answered
  yet". The composition itself is case-49's, on a cluster (T12).
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentPodGroupIncomplete` — two
  cases, both seen to run with `-v`. Confirm that they matched rather than trusting the exit code: a
  `-run` pattern matching nothing still exits 0, and two of this file's patterns turned out to match
  nothing at all.

- [x] **T10 · Gate 3 becomes per-role** — **shipped ahead of the rest, on its own: `4d88673b`**
  Blocked by: **nothing.** Not the single-role spec, not T1, not cross-model pools. It was the one
  task here that repaired an existing defect rather than building a new feature, so it did not
  queue behind a CRD that did not exist then.
  Landed as `4d88673b` (the squash of https://github.com/gpustack/gpustack-operator/pull/159), and **that commit message is the design record** — the
  mechanism, both reachable shapes, the measured evidence, and the review rounds that followed. Those
  rounds added repairs this cell did not anticipate: the node-batch (`.count`) half of a flavor's
  identity, an ambiguous flavor or assignment resolving to no population rather than to an arbitrary
  one, and a server-side-apply payload having to carry every owned check. Read the commit rather than
  this cell for what the code does.
  Owns: `pkg/worker/controllers/worker/node_devices_admission.go` + its test — and, as merged,
  `pkg/nodefeature/helper.go` + its test, which is where the label algebra it reads belongs
  Gate: review — and a **code review before merge**, because this is the admission path and the
  failure mode it repairs (`Ready` → admitted → `Pending` forever) is the kind a test suite is worst
  at catching: nothing errors, nothing crashes, and the wrong answer is a legal one.
  Acceptance: a demand carries the PodSet it came from and the flavor that PodSet was assigned; two
  PodSets' demands merge only when the flavor also agrees; a card is eligible for a demand only when
  its own accelerator key is one the demand's flavor covers; the per-card budget stays **global to the
  Workload**, so two roles on one flavor cannot spend one card twice. Verdict stays `Retry`, never
  `Reject`, and the message names the starved role. **The existing test table is re-run unchanged and
  passes** — the guard that this is an extension. Two new cases failed before the change: two roles on
  two flavors with room under only one; and a mixed-model node whose other model's cards must not
  satisfy the demand.
  Verify: `go test ./pkg/worker/controllers/worker/ -run NodeDevicesAdmission`

- [x] **T11 · Documentation**
  Blocked by: T2, T5, T6, T8, T10
  Owns: `docs/reference/model-deployment.md`, `docs/architecture/admission.md`,
  `docs/architecture/scheduling-chain.md`
  Gate: review
  Acceptance: the reference page gains the P/D section — the pod group and what each label and
  annotation is for, why `pod-group-fast-admission` must never be set, the identical-`instanceType`
  rule and its cause, `acceleratorKey` and why it is validated against the pool, `kind` and its
  connector term, the rebuild policy, and every new refusal message. `admission.md`'s gate 3 section
  states that feasibility is per-role. `scheduling-chain.md` records that Kueue assigns a flavor per
  PodSet, which is what lets one pool serve two models. Routed through the `gpustack-operator-docs`
  skill, which owns the header blocks, the Contents lists and the index.
  Verify: `make lint docs`

- [x] **T12 · e2e: the group forms, and every refusal fires**
  Blocked by: T2, T5, T6, T7, T8, T10
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-49.sh`,
  `.claude/skills/gpustack-operator-e2e/cases/case-51.sh`,
  `.claude/skills/gpustack-operator-e2e/SKILL.md`
  Also touched, recorded here: `pkg/worker/controllers/worker/model_deployment{,_pod_group}.go`,
  `pkg/worker/controllers/worker/model_deployment_group_test.go`,
  `pkg/worker/webhooks/worker/model_deployment.go`, `api/worker/v1alpha1/model_deployment.go` and its
  generated artifacts — the run found two defects and one wrong comment, all below.
  Gate: a local Kubernetes cluster; the case numbers re-checked against `cases/` first
  Acceptance: **case-49** — a two-role deployment produces one Workload with two PodSets named for
  the roles, of the right counts; every Pod carries the group label, the serving annotation and its
  role hash, and **none** carries `pod-group-fast-admission`. **case-51** — the refusals, in one
  pass: eleven roles, duplicate role names, differing `instanceType`, an `acceleratorKey` the pool
  does not offer, `kind: server` mixed with `prefill`; then a `replicas` change, asserting the group
  rebuilds and converges.
  Verify: `bash cases/case-49.sh default; bash cases/case-51.sh default`, each PASS

  **Landed with three amendments, each forced by the live run rather than by review.**

  1. *`both roles reach ready == desired` is NOT covered, and case-49 SKIPs it out loud.* It needs an
     engine image that serves, which needs an accelerator; the cluster available for this spec is
     single-node and CPU-only. Its replicas carry a placeholder image and never serve. **What does
     not fill this gap:** any run on that cluster, since the ceiling is the hardware; and any unit
     test, since the claim is about a container starting. It needs a coordinated pass on real
     hardware, which is a separate task rather than a rerun of this one.
  2. *The duplicate-role-name refusal belongs to the SCHEMA, not to the webhook.* `roles` is
     `+listType=map +listMapKey=name`, so the API server refuses the duplicate during validation and
     T2's rule never runs — measured, `spec.roles[1]: Duplicate value: {"name":"worker"}`. The case
     asserts the schema's wording and the rule keeps its comment corrected; the rule stays as the
     backstop for the marker being dropped.
  3. *Deleting a ModelDeployment deadlocked, and so did every rebuild.* Fixed in
     `0e9da340`; the reasoning is in that commit and summarized under Risks. case-49 gained a row
     asserting the deletion completes, and that row is the one the pre-fix image fails while the
     other five stay green — run against `s7-pd` to check the row still discriminates.

- [x] **T13 · e2e: atomic admission, demonstrated by starvation and recorded**
  Blocked by: T12
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-50.sh`, this spec's Test Plan
  Gate: case-49 green
  Acceptance: a pool with room for one role's demand and a deployment needing both. Asserted: **zero**
  Pods of **either** role running; one Workload with two PodSets; no quota reservation;
  `QuotaReserved=False` naming the ClusterQueue. Then room is made and both roles reach
  `ready == desired`. The observations — Pods running per role while short, the Workload's PodSet
  names and counts, the condition reason, and the time to converge once room appeared — are
  **recorded** in the Test Plan's table, not left in a run log. A run that cannot record them is not a
  pass, because the claim is a negative and a negative with no recorded evidence is an assertion about
  nothing.
  Verify: `bash cases/case-50.sh default`, PASS, and the table below filled

  **The shortage is made by OCCUPYING the quota, never by editing the ClusterQueue.** A filler
  deployment sized from the pool's own numbers holds all but one role's width, so the case changes no
  cluster-scoped object and restores the baseline by deleting its own deployments. It also asserts the
  window it created — one role fits in what is left, two do not — and SKIPs, printing both numbers,
  when the pool cannot make one; a shortage deep enough to starve both roles for lack of room would
  let the headline row pass with atomicity doing nothing.

  `ready == desired` after room appears is **not** covered here either, for T12's reason. What is
  covered instead is the gate: both replicas keep `kueue.x-k8s.io/admission` while short and lose it
  together, which is the observable that tells one Workload from two.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/controllers/worker` needs **two fake `ResourceFlavor`s of two accelerator models in one
  ClusterQueue**, and a fake Workload carrying two `PodSetAssignments` with two flavors. Neither
  exists; both are what makes the heterogeneous claim testable without the hardware.
- A **fake `Devices` fixture for a mixed-model node** — one `Devices` object whose `Status.Groups`
  carry two manufacturers' or two models' accelerators. The existing fixtures are single-model, which
  is why F6's second defect has never been caught.
- The single-role spec's golden fixtures are reused **as regression baselines**: T6 asserts a
  `kind: server` render is byte-identical to them.
- The e2e suite needs a way to shrink a pool's effective capacity for T13. Deactivating a node's
  managed mark, or requesting more cards than the pool has, both work; the case picks one and states
  which, because "the pool is short" must be a fact the case creates rather than one it hopes for.

#### Unit tests

New and extended packages carry table-driven coverage of every case below and must not regress the
package they live beside.

**Validation cases** (`pkg/worker/webhooks/worker`) — every "reject" case is the point.

| Case | Fixture | Expected |
|---|---|---|
| `roles_one` | one role, no `kind` | accept; `kind` defaults to `server` |
| `roles_two` | prefill + decode, same `instanceType` | accept |
| `roles_ten` | ten roles | accept |
| `roles_eleven` | eleven roles | reject; the message names Kueue's 10-PodSet limit as the cause |
| `roles_zero` | empty list | rejected by the schema (`minItems=1`) |
| `role_names_duplicate` | two roles named `decode` | reject; names the PodSet-collapse consequence |
| `role_name_uppercase` | `Prefill` | reject; names the `PodSetReference` pattern |
| `instance_type_differs` | two roles, two `instanceType`s | reject; states one-queue-name, names `acceleratorKey` |
| `instance_type_same` | two roles, one `instanceType` | accept |
| `accelerator_key_offered` | a key one of the pool's flavors carries | accept |
| `accelerator_key_not_offered` | a key no flavor carries | reject; the message lists the offered keys |
| `accelerator_key_pool_has_no_flavors` | pool with zero flavors | **accept** — an empty read is not a refusal |
| `accelerator_key_empty` | omitted | accept; no selector rendered |
| `kind_server_alone` | one role, `server` | accept |
| `kind_server_mixed` | `server` + `prefill` | reject |
| `kind_unknown_for_engine` | a kind with no rendering term | reject; names the engine and the kind |
| `two_roles_after_length_rule_removed` | the single-role spec's seam test | now the **full** path accepts two roles |

**Pod-group metadata cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `group_label_on_every_pod` | prefill 2 + decode 2 | four Pods, one group label value |
| `total_count_is_sum` | 2 + 2 | `pod-group-total-count: "4"` on all four |
| `role_hash_is_role_name` | any | each Pod's `role-hash` is its role's name |
| `identical_specs_two_podsets` | two roles differing **only** in name | two distinct `role-hash` values — the case that would pass by accident without the annotation |
| `serving_annotation_present` | any group | `pod-group-serving: "true"` on every Pod |
| `fast_admission_absent` | any group | the annotation is absent; the test message states it collapses all roles into one PodSet |
| `queue_name_identical` | two roles | one `queue-name` value across the group |
| `group_name_readable` | short deployment name | the group name is the deployment name |
| `group_name_hashed` | name over the label limit | `gpustack-fnv64-<hash>`, identical on every Pod |

**Render cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `accelerator_key_renders_one_selector` | `acceleratorKey: nvidia-h20` | exactly one added nodeSelector entry, nothing else differs |
| `accelerator_key_empty_renders_none` | omitted | no nodeSelector entry added |
| `kind_server_matches_single_role_golden` | one `server` role | byte-identical to the single-role spec's fixture |
| `kind_prefill_discriminator` | `prefill`, per engine | the engine's producer term |
| `kind_decode_discriminator` | `decode`, per engine | the engine's consumer term |
| `kind_key_is_owned` | the key in `extraArgs` | rejected, naming the key |

**Reconcile cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `whole_group_created_in_one_pass` | two roles, 2+2 | four creates issued in the same pass |
| `partial_create_retried` | the third create fails | the pass is not successful; the next pass creates it |
| `replicas_change_rebuilds` | 2 → 3 on one role | group rebuilt; all Pods end at total 5 |
| `no_mixed_total_count_intermediate` | during a rebuild | never two live Pods with different totals |
| `role_added_rebuilds` | a role appended | group rebuilt |
| `idempotent` | unchanged spec, second pass | no writes |

**Status cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `assigned_flavor_absent_before_admission` | no admission | `assignedFlavor` is `nil`, **not** `""` |
| `assigned_flavor_per_role` | two PodSets, two flavors | each role reports its own flavor |
| `quota_reserved_group_incomplete` | 3 of 4 Pods | `False` / `PodGroupIncomplete`, message `3/4` |
| `quota_reserved_inadmissible` | full group, no reservation | `False`; the reason names the ClusterQueue |
| `quota_reserved_true_covers_all_roles` | full group, reserved | `True`; asserted to be one Workload covering both roles |
| `kind_echoed` | any | `status.roles[i].kind` echoes the spec |
| `status_rebuilt_wholesale` | a stale field stored | overwritten from observed state |

**Per-role feasibility cases** (`pkg/worker/controllers/worker`) — the first two failed before T10.

| Case | Condition | Expected |
|---|---|---|
| `two_flavors_room_under_one` | prefill on A (0 free), decode on B (enough) | `Retry`; the message names the prefill role |
| `mixed_model_node_wrong_model` | one node, models A and B; demand under A, only B free | `Retry` |
| `two_roles_one_flavor_room_for_one` | same flavor, cards for one role | `Retry` — the budget is shared |
| `two_roles_one_flavor_room_for_both` | same flavor, cards for both | `Ready` |
| `two_flavors_room_under_both` | both roles satisfied from their own flavor | `Ready` |
| `single_role_table_unchanged` | every existing single-PodSet case | identical verdicts and messages |
| `admitted_workload_skipped` | admitted | untouched, as today |
| `evicted_workload_skipped` | evicted with a stale reservation | untouched, as today |

#### Integration tests

- **envtest, two fake flavors of two models in one ClusterQueue**, a two-role deployment naming both:
  the Workload's `podSetAssignments` carries **two different flavors**. This is where G2 is verified;
  the hardware cannot show it.
- **envtest, the incomplete group** (T9): the group short of its total ⇒ **no Workload exists** and
  the deployment reports `PodGroupIncomplete`; completing the group produces the Workload and clears
  the reason. The "no Workload" assertion is made with a List that is first proven to work against a
  control object, so an empty result is evidence and not a broken query.
- **envtest, the rebuild** (F10): a `replicas` change rebuilds the group and the deployment returns to
  `Ready`, with no intermediate disagreement on `pod-group-total-count`.
- **envtest, the webhook**: every rejection in the validation table arrives as an `Invalid` status
  error carrying the message the table names.

#### e2e tests

Run against a local Kubernetes cluster. No RDMA, no cloud, one accelerator model.

- **case-49 — the group forms, and comes down.** Two roles; one Workload with two PodSets named for
  the roles and of the right counts; the group's labels and annotations on every Pod and
  `pod-group-fast-admission` on none; two roles differing ONLY in name stay two PodSets; and deleting
  the group completes. `ready == desired` in the serving sense SKIPs — see T12.
- **case-50 — atomic admission, by starvation.** A pool short of the group's demand: **zero** Pods of
  either role admitted, one Workload, no reservation, `QuotaReserved=False`. Then room is made and
  both roles are admitted together. Recorded below.
- **case-51 — every refusal, and the rebuild.** Eleven roles; duplicate role names (refused by the
  SCHEMA, not the webhook — see T12); differing `instanceType`; an `acceleratorKey` the pool does not
  offer; `kind: server` mixed with `prefill`. Then a `replicas` change: the group rebuilds and
  converges.

**Observation record (filled by T13).**

Measured on a single-node Kubernetes cluster, CPU-only. One pool,
`gpustack--generic-linux-arm64`, nominal quota **10 CPU** on flavor
`gpustack--generic-15-0-linux-arm64-10c`; one replica requests **800m**. The shortage is a filler
deployment of 11 replicas holding 8800m, leaving **1200m** — enough for one role, short for two.

| Observation | While the pool is short | After room is made |
|---|---|---|
| Pods running, prefill | 0 (Pending, gated) | 1 (Running) |
| Pods running, decode | 0 (Pending, gated) | 1 (Running) |
| Workloads for the deployment | 1 | 1 (the same one) |
| PodSet names / counts | `prefill=1 decode=1` | unchanged |
| `podSetAssignments` | none | both, each `cpu` on `…-linux-arm64-10c` |
| Scheduling gates on every replica | `kueue.x-k8s.io/admission`, `kueue.x-k8s.io/topology` | none |
| Workload `QuotaReserved` | `False`, reason `Pending` | `True`, reason `QuotaReserved` |
| Workload `Admitted` | absent | `True` |
| `QuotaReserved` message | `couldn't assign flavors to pod set prefill: insufficient unused quota for cpu in flavor gpustack--generic-15-0-linux-arm64-10c, 400m more needed` | — |
| ModelDeployment phase / conditions | `Starting`; `QuotaReserved=False` reason `Pending` | `Ready` |
| Time from the filler's deletion to both assignments | — | **3s** |

**The gate row is the one that carries the claim.** 1200m was free and prefill alone needed 800m, so
a per-role admission would have started prefill and queued decode — the shape that leaves prefill
holding accelerators it cannot serve from, and holding the very quota decode is waiting for. Both
replicas stayed gated, and both were released in the same admission.

**What is NOT in this table, and why.** `ready == desired` in the serving sense, and any measurement
of time to serve: the replicas here run a placeholder image, because a CPU-only InstanceType has
observed no accelerator and the operator can synthesize no engine image. The admission half is
complete; the serving half needs real hardware and is a separate task. A rerun on this cluster does
not fill it.

## Alternatives

- **Add `nodeSelector` to `InstanceTemplate`.** Rejected, on the type rather than on taste.
  `InstanceTemplate` is inlined into `InstanceSpec`, where it is marked `Immutable after creation`
  (`api/worker/v1alpha1/instance.go:44`), so a scheduling field added to it would be immutable on an
  `Instance` — sitting beside `InstanceSpec.NodeName`, whose own rule is *"Immutable unless the
  Instance is stopped"*. Two node-placement fields on one type under two different mutability rules is
  a contract nobody can state in one sentence. It would also move both CRDs for a field only one of
  them needs, which the single-role spec's Boundaries put behind "ask first". A per-role field is more
  cohesive: hardware preference and container shape are different decisions and belong in different
  fields.
- **Let `acceleratorKey` be a free-form `nodeSelector map[string]string`.** Rejected, on a mechanism
  rather than on minimalism. `flavorSelector` keeps only the nodeSelector keys a candidate flavor
  declares in its own `NodeLabels` and **drops the rest** — so a key no flavor carries is not a
  constraint that fails, it is a constraint that is ignored. A free-form map hands users a field whose
  most likely wrong value produces silence at admission and a Pending Pod two gates later. A single
  typed key can be checked against the pool's live flavors and refused with the offered set named.
  Widening it later — adding a `nodeSelector` beside it — stays a compatible change; narrowing a map
  into a key would not be.
- **Kueue-native replacement instead of rebuilding the group.** Rejected, and the rejection is what
  makes *any departure rebuilds the group* a contract rather than a shortcoming. Kueue does support
  replacing one member: a terminal Pod is held as `WaitingForReplacementPods` until one carrying the
  same role hash appears. The only blocker is the NAME — replicas are named
  `<deployment>-<role>-<ordinal>` and recreated under the same name, which cannot be created while the
  held Pod still exists — so replacements would have to carry fresh names.

  That would change the replica naming contract, which is what an operator greps for, and the
  ordinal-based scale-down that removes the highest ordinals. **What it would buy is small on this
  chain:** the case it saves is a template edit rolling one Pod instead of the deployment, and a
  template edit replaces the process — whose in-process KV is lost either way. This whole design
  exists to keep the KV in the pool rather than in a replica's memory, so the thing replacement
  protects is the thing already moved out of the blast radius. A whole-group rebuild is also visible,
  where a half-replaced group is not.

- **Reuse `roles[].name` as the kind (make the name an enum).** Rejected: it would forbid two roles of
  one kind (`decode-long-ctx` and `decode-short-ctx`), and it would make a typo in a name silently
  change what the operator tells the engine the role is. The name identifies the PodSet; the kind
  selects behaviour; they are two facts.
- **Relax `roles[*].instanceType` and put the roles in different ClusterQueues.** Not rejected on
  preference — it is unreachable. A Workload has one `queueName`, and Kueue enforces it on the Pods
  directly (`validatePodGroupMetadata` errors when two Pods of a group disagree). Two queues means two
  Workloads means no atomic admission, which is the property the spec exists to deliver.
- **Use `pod-group-fast-admission` so the Workload forms before every Pod exists.** Rejected on
  measured code: `constructGroupPodSetsFast` takes the first runnable Pod, sets that one PodSet's
  count to the group total, and returns. Every role's Pods land in one PodSet, so per-role counting
  and per-role flavor assignment both vanish — the two things this spec adds. It would fix F2's
  ordering problem by deleting the feature.
- **Model P/D as two `ModelDeployment`s plus a grouping object.** Rejected: it recreates the problem
  one layer up. Two deployments are two sets of Pods; making them one Workload still means one pod
  group across both, which means one object writing the group's total count — so the grouping object
  becomes the deployment and the two deployments become roles. That is this spec, with an extra CRD.
- **A `PodSetGroup` so prefill and decode are co-placed by topology.** Deferred, not rejected. Kueue
  supports it through `TopologyRequest.PodSetGroupName`, which makes several PodSets share one flavor
  assignment group — but it needs Topology-Aware Scheduling configured, it constrains assignment
  (which is the opposite of what F4 wants), and co-placement is a latency optimisation, not the
  atomicity this spec is about. It is worth having once there is a measurement that says the hop
  between roles matters.
- **Fix Gate 3 by scoping the check to one flavor and running it once per flavor.** Rejected: the
  budget must be shared. Two roles assigned the same flavor compete for the same cards, and a node
  carrying two models backs two flavors, so per-flavor runs with independent budgets would let one
  card satisfy two demands. The correct shape is one pass with a global per-card budget and a
  per-demand eligibility filter, which is what F6 specifies.
- **Report the assigned flavor as an empty string when unassigned.** Rejected: a legal value would be
  used to express an abnormal state, and the two would then be indistinguishable — the failure shape
  the KV cache backend spec's status modelling was written to avoid, on evidence from real hardware.
- **Grow the group in place on a scale-up instead of rebuilding.** Deferred to an Open Question rather
  than chosen, because the deciding fact is not established: `pod-group-total-count` must agree across
  every Pod of the group, and what Kueue 0.18.4 does to an *admitted serving* Workload while that
  agreement is being re-established is a behaviour to observe. Choosing it on an assumption would risk
  evicting a serving deployment on every scale.

- **Derive the parallelism degree from the card count, instead of carrying it as a field.** Rejected,
  and the alternative it loses to is a `roles[]` field. Nothing validates the contiguous port window a
  disaggregated deployment needs, and that window is sized by the engine, the card count and the
  parallelism; the first two are on the API and the third is on none of the four CRs, which is what
  blocks the validator. The field, the per-engine formulas and the validator are separate work,
  tracked at <https://github.com/gpustack/gpustack-operator/issues/203>.

  **Deriving is the tempting option because it is right in the common case**, and that is precisely
  its defect: under pipeline parallelism the card count is the product of two degrees, so a check
  built on it is correct exactly where it is not needed and silently permissive on the deployments
  large enough to need it. A formula missing an input is worse than no formula, because it passes.

  **The decisive fact is that parallelism is already being consumed.** SGLang divides the KV segment
  by it — `global_segment_size // tp_scale_factor`, v0.5.18 `mooncake_store.py:413-416` — so adding
  the field is not inventing an input to make a check possible; it is declaring a quantity that is
  already changing behaviour.

  Per role rather than per deployment, since prefill and decode can run different degrees while
  `spec.engineVersion` sits one level up. The engine's own parallelism argument joins the
  operator-owned key set, so it is refused in `roles[].extraArgs` instead of silently disagreeing
  with the field — the rule that already governs every other owned key.

  ⚠️ The Ascend window needs only the card count (`[20000, 20000 + npu_per_node * 1000)`), so that
  half is writable today — and shipping only it would leave a validator that looks general and covers
  one vendor.

## Open Questions

> **One entry below is not a question.** The `status.roles[].kind` enum closes with a code change, not
> with an answer, so its carrier is an issue; it is listed here because this is where a reader looks
> for unfinished business, and marked so nobody tries to close it by deciding something.

- **[DEBT, not a question] `status.roles[].kind` carries no enum.** The spec field does — the
  generated CRD gives `spec.roles[].kind` `Enum: ["server","prefill","decode"]` and
  `Default: "server"` — while the status field of the same Go type is plain `Type: "string"`. The
  marker sits on the field, not on `ModelDeploymentRoleKind`, and the type's own `+enum` is a Go-level
  marker that does not become CRD validation. The ORDER was the constraint and it is now satisfied:
  the field is required, so an enum added while the controller could still write `""` would have made
  every status write fail, and T8 fills it from `ModelDeploymentEffectiveRoleKind`. Adding the enum is
  therefore safe and is left to a follow-up, because it changes what the API server accepts on a
  status write and wants its own verification rather than a rider on this one.

- **Can a serving pod group be scaled in place?** F10 rebuilds the group because
  `validatePodGroupMetadata` requires every Pod to carry the same `pod-group-total-count`, and
  `equivalentToWorkload` compares PodSet counts. What Kueue 0.18.4 actually does to an admitted
  serving Workload while the annotation is being rolled across the Pods — tolerate, re-admit, or evict
  — is not established from the sources. T4 may settle it experimentally; until it does, rebuild is
  the policy, and it is a policy rather than an omission.
- **What exactly does each engine call the role discriminator, at the version this repository ships?**
  F5 pins the *shape* (one owned key per engine, one golden fixture per (engine, kind)) but the
  literal flag has moved before and will again. T6 settles it against the shipped engine images and
  freezes the fixtures; the three-tier override is what absorbs the next move.
- **Does a replacement Pod rejoin an admitted serving group without re-admission?** Kueue carries a
  `WorkloadWaitingForReplacementPods` condition, so the mechanism exists, but whether a replacement
  keeps the group's reservation — and how that interacts with the 30-second KV lease a leaving replica
  costs — is not established. It decides how expensive a single replica crash is for a P/D deployment,
  which is worth knowing before anyone tunes a rollout policy.
- **Should `QuotaReserved` split into a group-formation condition and a quota condition?** This spec
  folds "no Workload exists because the group is incomplete" into `QuotaReserved`'s reasons, on the
  grounds that the CR stays thin and the reason string carries the distinction. If operators
  repeatedly misread it, a separate `PodGroupFormed` condition is the obvious answer — but adding it
  now would be designing for a confusion that has not been observed.
- **How should a role with zero available cards of its `acceleratorKey` be surfaced, beyond `Retry`?**
  F6 makes the AdmissionCheck message name the starved role, which is a message on the Workload. Where
  that belongs on the `ModelDeployment` — a condition, an event, or nothing — depends on how visible
  the Workload is to the people who run these, and that is worth deciding after the first operator
  has to debug one.
