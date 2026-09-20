# Spec: Role Replica Admission Unit

Status: Built
Blocked on: the end-to-end suite, and nothing else. Every other gate is green — the unit tests, both
lint targets, and four rounds of measurement on a live cluster, each recorded beside the claim it
settles. What is owed is e2e: five existing cases assert the shape this spec replaces (45, 49, 50,
51 and 68), and one new case has no stand-in anywhere — the scale that leaves a surviving replica's
UID untouched, which is the whole point of the change and which nothing currently asserts at any
level. Case 68 is the instructive one: it passes today and will keep passing, because its fixtures
are all single-replica and its assertions count group names rather than name them, so it is blind
to the change rather than broken by it. Running any of this needs an operator image built and
deployed, which is why it is a step rather than a missing conclusion.
Type: Feature

## Summary

A `ModelDeployment` role today admits all of its replicas as one Kueue pod group, so changing
`spec.roles[].replicas` — in either direction, including **growing** it — deletes and recreates
every running replica of that role. Each one reloads its model weights and the role passes through
a window with no serving capacity at all. This spec narrows the admission unit from *a role* to
*one replica*, so a replica count change adds or removes exactly the replicas it names and leaves
every other one running. It also splits the one number a role carries today into the two it has
always meant — **how many independent serving instances** and **how large one instance is** — so
that a future multi-Pod instance (tensor/pipeline/expert parallelism across hosts) has a field to
live in rather than being conflated with the replica count.

## Motivation

### Goals

1. **A replica count change must not restart a running replica.** Growing `replicas` from 2 to 3
   creates one Pod; shrinking from 2 to 1 deletes one Pod. In neither case does a surviving replica
   get a new UID, reload weights, or lose its prefix cache.
2. **A role's two numbers become two fields.** `replicas` means *how many independent instances*;
   a new field means *how many Pods form one instance*. The first is safe to change at any time;
   the second is fate-sharing and changing it is a rebuild by construction.
3. **Creating a replica becomes idempotent at the API server** — the marker that tells a retry from
   a first attempt lives where a lost response cannot hide it, rather than in a controller's own
   memory. The marker is an **ordinal label written into the create request**, checked through an
   uncached, label-selected read before each create. A deterministic name would be the other way to
   get the same property, and F3 records why this one was taken instead: the name a `GenerateName`
   create receives arrives only in the create *response*, which is exactly what is lost in the
   failure this goal is about, whereas a label the client itself wrote is already on the persisted
   object. This goal states the property; F3 states the implementation.
4. **Joint admission keeps its current promise.** A `ModelDeployment` spread over several roles
   still admits as a whole or not at all; only the unit the barrier enumerates changes.
5. **The status can report a partially admitted role.** With one Workload per replica, "2 of 3
   admitted" becomes a reachable state and must be readable.

### Non-Goals

1. **Rendering a multi-Pod instance.** This spec defines the field and pins its semantics; the
   render path for an instance larger than one Pod — per-member hostnames, a headless Service,
   member index and instance size reaching the engine — is deliberately deferred. The field ships
   now and is validated to `1` until that path exists; the reasoning is recorded in Open Question 3.
2. **Switching the workload substrate.** Replicas stay operator-created `core.Pod` objects. This
   spec deliberately moves the admission unit to where a LeaderWorkerSet would put it, so that a
   later substrate change is an increment rather than a rewrite, but it does not make that change.
3. **A configurable rollout policy.** The cadence stays one replica per role per pass. `maxSurge`
   and `maxUnavailable` are not introduced: surge on an accelerated pool costs a whole extra card
   of quota for the duration of the rollout, and a surge replica that cannot be admitted stalls
   the rollout with no way forward. That trade deserves its own spec.
4. **A gang-versus-at-least-one switch.** Admitting a role before all of its replicas hold quota
   is a change to what "this deployment is serving" means, not a change to the admission unit.
   It is argued in Alternatives and left out.
5. **Backward compatibility.** `ModelDeployment` has never shipped in a release, so fields are
   renamed, re-documented, and renumbered freely.

## Proposal

The deployment keeps rendering one Pod per replica and keeps admitting through Kueue, and Pod names
keep coming from the API server. What changes is the size of the group each Pod joins, the fact that
each replica now carries an ordinal identifying it, and the unit the joint-admission barrier counts.

### User Stories

#### Story 1

As an operator running a disaggregated deployment, I want to grow `prefill` from 2 replicas to 3,
so that I add capacity without the two already-serving prefillers reloading their weights and
dropping their caches.

#### Story 2

As an operator reducing cost, I want to shrink `decode` from 4 replicas to 2, so that exactly two
replicas drain and the other two keep serving uninterrupted.

#### Story 3

As an operator adding a second role to a deployment that had only one, I want the existing role's
replicas left alone, so that introducing disaggregation does not take the currently serving role
offline.

#### Story 4

As an operator watching a deployment start on a busy cluster, I want to see how many of a role's
replicas have been admitted, so that "waiting for capacity" is distinguishable from "stuck".

#### Story 5

As a platform engineer, I want a role that will later run one model instance across several hosts
to declare that shape in its own field, so that the instance size and the instance count are never
confused — and so that changing the safe one stays safe.

### Core Features & Acceptance Criteria

#### F1 — One Kueue pod group per replica

Every replica joins a group of its own. The group's declared total is always `1`.

- **AC1.1** Each rendered Pod carries `kueue.x-k8s.io/pod-group-total-count: "1"`, for every role
  and every replica count.
- **AC1.2** Each replica of a role carries a distinct `kueue.x-k8s.io/pod-group-name`.
- **AC1.3** `kueue.x-k8s.io/role-hash` remains the role's own name on every replica. Per-role
  flavor assignment and per-role status attribution both join a Workload's PodSet to a role by
  this name; a digest there breaks both joins while nothing errors.
- **AC1.4** Growing a role's `replicas` from 2 to 3: the two pre-existing Pods are the same
  objects afterwards. Asserted with a marker the renderer never writes — not with the Pod name and
  not with the UID, both of which a fake client leaves free to coincide.
- **AC1.5** Shrinking a role's `replicas` from 2 to 1 deletes **the Pod and that replica's own
  Workload**, and the surviving Pod is the same object.
  ⚠️ **The Workload delete is not an optimization; without it the quota leaks permanently.**
  A serving group's Pod finalizer is released only by its Workload being deleted
  (`pod_controller.go:491-531`), and a terminating Pod that still carries a `nodeName` counts as
  **active** (`:887-894`) — so a group of one active Pod against `Count: 1` shows Kueue no excess,
  and it finalizes nothing. Orphan collection does not rescue it either: a group's Workload carries
  no controller reference, so `canFinish` stays false. Pod-delete-only therefore leaves one
  Workload Admitted and holding quota forever, with its Pod stuck Terminating behind the finalizer.
  The delete accounting in the test counts **by object kind** — one Pod delete and one Workload
  delete per removed replica — because a test counting deletes alone passes against the leak.
- **AC1.6** Adding a second role to a single-role deployment: the first role's Pods are the same
  objects afterwards, and the deployment never passes through a state where that role's Pods sit
  in two different groups.

#### F2 — Group names no longer depend on how many roles exist

The group name is derived per replica and does not change shape with the number of roles.

- **AC2.1** A deployment with one role and a deployment with two roles derive the first role's
  replica group names identically.
- **AC2.2** Two deployments of the same name in different namespaces never derive the same group
  name.
- **AC2.3** The derived name is a **hash on every path**, carrying the prefix that already marks a
  derived name in this operator, over `(namespace, deployment, role, ordinal)`.
  ⚠️ **There is deliberately no readable path and no `sole` branch.** A readable composite cannot
  carry the uniqueness requirement: the operator's own names and role names both admit `-`, so
  `a/b-c` and `a-b/c` collide, and the consequence of a collision is two replicas landing in one
  Kueue group — the exact thing this spec exists to prevent. A separator that resolves the ambiguity
  is not available either: ✅ **MEASURED**, Kueue's own webhook refuses a group name that is not an
  RFC 1123 subdomain at **Pod admission time**, because for a group the Workload's name *is* the
  group name verbatim (`pod_controller.go:1171` via `GetPodGroupName`), so `_` never reaches the
  cluster. Nor is a resolvable name wanted: Boundaries forbids parsing an ordinal back out of it.
  Readability is served by the ordinal label and the role's resource note instead.
  One consequence is a simplification: with a single path, AC2.1 holds by construction.

The rule this replaces is the source of the AC1.6 failure: today a sole role's group takes the
deployment's own name and a non-sole role's takes a hash, so gaining a second role renames the
first role's group without moving any count — and the rebuild predicate, which compares only the
declared total, does not catch it.

#### F3 — Creating a replica is idempotent per ordinal

A replica's identity is an ordinal the operator writes into the create request as a label, and every
create is preceded by an uncached read selecting on that label. Pod names stay assigned by the API
server through `GenerateName`.

- **AC3.1** Every rendered Pod carries a label naming which replica of its role it is. Two reconcile
  passes over an unchanged spec assign the same ordinal to the same replica, and once a role has
  converged its live replicas occupy `0..replicas-1` with no gaps.
  ⚠️ **The ordinal is a label and not a resource note, and the reason is AC3.2.** This operator's
  resource notes are annotations (`pkg/systemmeta/resource.go:46-58`), and an annotation cannot be
  selected on, so a check built on one would have to list a role's Pods and filter client-side.
  AC3.2's criterion has to be evaluated by the API server; a label is what lets it be.
- **AC3.2** A `Create` that the API server persisted but whose response was lost leaves exactly one
  Pod for that ordinal after the next pass. The criterion is the object count for that ordinal, not
  the absence of an error — the retry finds the persisted Pod by its ordinal label and creates
  nothing.
  ⚠️ **The marker rides in the request, and the read that checks it is uncached.** The name a
  `GenerateName` create receives exists only in the create *response*, which is the thing this
  failure loses; a label the client wrote itself is already on the persisted object. The check
  therefore reads through `APIReader` — the reconciler already holds one — because the informer
  cache can still be missing a Pod the API server has, and that gap is exactly this window. The read
  is selected down to one ordinal, so it returns zero objects or one.
  Reconciles for one `ModelDeployment` are serialized by controller-runtime, so nothing else creates
  for that ordinal between the read and the create.
  ⛔ Recording the name returned by an earlier `Create` is not an alternative: on the path this
  closes, that response never arrived. ⛔ Neither is a ReplicaSet-style expectations store: it
  records how many creates were issued rather than which ones, and a controller restart clears it.
- **AC3.3** A replacement for an ordinal is created only once **no Pod for that ordinal is readable
  through `APIReader`** — the same read AC3.2 already performs, so one read serves both purposes.
  ⚠️ **What must be waited out is the departing Pod's existence, not the issuing of its delete.** A
  per-replica group declares `Count: 1`, so a replacement created while the departing member is
  still listed makes two active members, and Kueue removes the excess by deleting the newest gated
  Pod — which is the replacement itself (`pod_controller.go:1254-1257`). Deleting that replica's
  Workload is what makes the departing Pod go away: it drives `stopJob(WorkloadDeleted)` → `Stop()`,
  which deletes the group's Pods and then finalizes them (`:538-543`) — and T1 measured the Pod
  object gone one second later.
  ⚠️ **The departing Pod does not step aside on its own, so there is nothing else to wait for.**
  ✅ **MEASURED.** `isPodRunnableOrSucceeded` counts a scheduled Pod as active unless its phase is
  `Failed` (`:887-894`) — the function's own name says which phases those are — and Kueue finalizes
  an inactive member only when the group is over `Count` counting active and inactive together
  (`:1262`), which one departing member against `Count: 1` never is.
  On the cluster, a `RestartPolicy: Always` Pod (what this operator renders,
  `model_deployment_render.go:407`) in a serving group of one was deleted with its Workload left
  standing. It held `Running` through the 40s drain, then moved to **`Succeeded`** — a phase Kueue
  still reads as active, and one of the two terminal phases a departing replica can reach, see
  below — and was **still present at 154s**, finalizered, with its Workload still
  Admitted. The positive baseline in the same run: deleting the Workload made the same kind of Pod
  disappear, so the instrument can report a disappearance.
  ⚠️ **That baseline took 36s, where T1's took 1s.** T1's container exited immediately on SIGTERM;
  a draining one does not. What a replacement waits out is the drain, so the create gate must be
  written against an observed state and ⛔ never against a timeout.
  ⚠️ **WHICH TERMINAL PHASE A DEPARTING REPLICA REACHES IS THE CONTAINER'S TO DECIDE, and the two
  behave oppositely.** ✅ **MEASURED**, four controlled groups. A container killed past its grace
  period lands in `Failed`, which *is* inactive; one exiting cleanly on SIGTERM lands in
  `Succeeded` (exit 0, `Completed`), which Kueue reads as active. Against `Count: 1` neither is over
  the count, so neither is finalized on its own: both sat with their Workload still Admitted past
  100s. `kubectl delete pod` and the Eviction API the drain calls were **identical field for
  field** — deletion timestamp, finalizer, phase track, Workload state.
  ⚠️ **They diverge the moment a replacement appears**, and that is what leaves exactly one remedy.
  Beside a `Failed` member the group counts active 1 + inactive 1 over `Count: 1`, so Kueue
  finalizes the dead member: the replacement survives, the Workload is never re-queued (measured at
  9s). Beside a `Succeeded` member both read active, so Kueue deletes the newer gated Pod — the
  **replacement itself** — and creating around it loops forever. Deleting the Workload covers both,
  and released both groups immediately once their containers had terminated.
  So the replacement path cannot branch on the exit code, and the create gate ⛔ cannot be relaxed
  to step around a departing member.
- **AC3.4** Which replica a scale-down removes, and which replica a rollout replaces first, are
  decided by ordinal rather than by creation timestamp.

#### F4 — `replicas` means instances, and a second field means instance size

- **AC4.1** `spec.roles[].replicas` is documented as the number of **independent serving
  instances**, with the statement that changing it does not restart a running instance.
- **AC4.2** The role gains a field for how many Pods form one instance, defaulting to `1`. The API
  field is `size`; its Go identifier is `ReplicaSize`, because gogo protobuf already puts a
  `Size()` method on the type and Go forbids the collision. The criterion is what the **generated
  CRD** calls the field, not what the Go struct calls it.
- **AC4.3** That field is documented as fate-sharing: the Pods of one instance start together and
  are replaced together, and changing it replaces every instance of the role.
- **AC4.4** `spec.roles[].resources.accelerator` is re-documented as what **one Pod** asks for,
  not what one replica asks for. At instance size 1 the two readings coincide; at any larger size
  they do not, and the field is per-Pod.
- **AC4.5** Validation refuses an instance size other than `1` for as long as the render path for
  a multi-Pod instance does not exist, with a message that names the limitation rather than the
  field's bounds.

#### F5 — Joint admission counts replicas

The barrier that holds a multi-role deployment until its whole set has reserved quota keeps its
verdict vocabulary and its bounds; only what it enumerates changes.

- **AC5.1** The barrier answers `Ready` only when every replica of every role of the deployment
  holds a quota reservation.
- **AC5.2** It answers `Pending` — never `Retry` and never `Reject` — while any replica has not.
  A `Retry` makes Kueue evict the Workload and drop its reservation, and two siblings waiting on
  each other through `Retry` trade the same quota forever.
- **AC5.3** A Workload that is already admitted is skipped, so a rollout does not drag an admitted
  deployment back to `Pending`.
- **AC5.4** A replica whose Workload is momentarily absent **because this operator is replacing
  it** counts as present for the barrier's purposes — **and the rule carries a discriminator that
  says which kind of absence it is**.
  ⚠️ **The deadlock this was first written for does not exist**, and the reason it does not is why
  the rule still needs care: Pod creation is not barrier-gated, and a Workload reserves quota
  *before* its AdmissionChecks are evaluated, so a recomposed Workload reserves on its own and the
  verdict loop re-runs. Replacing a replica cannot wedge the barrier.
  **The real defect is that "absent" conflates several states.** A Workload absent because a rollout
  just deleted it is one thing; one that was never composed because that role's creates are erroring
  is another — and treating the second as present releases the surviving sibling exactly when the set
  cannot assemble, which is the partial admission the barrier exists to prevent.
  **The discriminator reads Workload shape, not a hash.** A replica's slot reads as a rollout when
  the group has a terminating member, **no Workload owns any member of that group**, and the
  deployment is not itself being deleted. Anything else stays `Pending`.
  ⚠️ **An earlier draft of this criterion said "a terminating predecessor carrying a stale hash",
  and that is not implementable here.** The expected hash exists only where the convergence loop
  renders it, out of the InstanceType, the KV connection and settings; computing it inside the
  barrier would re-read all of them on every Workload event and duplicate the convergence loop's
  job. The shape test needs no extra read — the barrier already holds the namespace Workload list —
  and it separates two states the hash test gets wrong: **preemption** (member terminating, Workload
  still present but its quota revoked) reads `Pending` rather than as a rollout, and **teardown**
  (the deployment is going away) is excluded by the deletion-timestamp term rather than being read
  as a rollout that will never finish.
  ⚠️ **Replacing a replica passes through two windows, and only the first is the discriminator's.**
  While the departing Pod is still draining with its Workload already deleted, the slot holds a
  terminating member that no Workload owns — the discriminator fires and the deployment may stay
  `Ready`. That window is not brief: a replica that drains for 40s keeps its Pod object for a
  further 36s after the Workload delete (see Risks). Once that Pod is gone and the replacement has
  not been created yet, the slot holds nothing, the discriminator correctly does **not** fire, and
  the answer is `Pending` — which AC5.6 then keeps from becoming a park, because a deployment one
  short of its own replica count is assembling rather than infeasible. A reading that the
  discriminator is invisible under AC3.3 comes from looking only at the second window; and an
  implementation whose assembling branch is evaluated **before** the discriminator swallows the
  first window, which is an ordering defect rather than a reason to change the discriminator.
- **AC5.5** Any Workload in an operator-owned queue that does not belong to a multi-role
  `ModelDeployment` is answered `Ready` at once. The check is referenced from every operator-owned
  ClusterQueue, so a controller that answered only about its own objects would leave every other
  Workload in the cluster `Pending` forever with nothing naming the cause.
  ⚠️ **This exemption stays counted in ROLES, not in the new per-replica groups.** Today it reads
  `len(modelDeploymentPodGroups(md)) < 2` (`model_deployment_joint_admission.go:261`), and groups
  are roles — so a single-role deployment never enters the barrier at all. Under per-replica
  enumeration that same expression turns a single-role two-replica deployment into two "groups" and
  drags it inside, where it newly acquires partial-quota holds and exposure to the 30-minute park
  it was previously exempt from. Nothing in this spec's goals asks for that: the barrier exists for
  atomicity **across roles**. The verdict counts replicas; the exemption counts roles.
- **AC5.6** The bound that turns a hold into a park still applies, and still only to a deployment
  that has stopped changing shape — one short of its own replicas is assembling, not infeasible.

#### F6 — Workloads are reached by ownership, with no derived name in the path

- **AC6.1** Every path that needs a replica's Workload finds it by the Pods it owns.
- **AC6.2** There is exactly one function answering "which Workload belongs to this" in the
  codebase.
- **AC6.3** A caller reaches a Workload in one lookup. Where a caller outside the changed files
  still consumes a group-name-keyed shape, that shape is kept and **named as the remaining
  coupling**, rather than the keying being pushed back into the finder.

⚠️ **Correcting what this feature was first written against.** The statement here used to be that
two functions answer this question, "one by ownership, one by a name derived from the spec." That is
wrong, and the difference matters for what the work is. **Both match by ownership** —
`modelDeploymentWorkloadByGroup` selects with `modelDeploymentWorkloadOwnsAny`, the same predicate
the other one uses. What separated them was that one additionally **keyed its result by a derived
group name**, so every caller went through `wlByGroup[groupOfRole[role]]`: two lookups, the outer one
on a name this operator computes. The derived name was never the matcher; it was an index sitting in
front of it. That index is what stops being ours the day a substrate names Workloads itself, and
removing it from the path is what this feature is for.

#### F7 — Status reports admission progress per role

- **AC7.1** `status.roles[]` reports how many of the role's replicas hold a quota reservation,
  alongside the existing desired and ready counts. The field is **`quotaReserved`**: it counts
  replicas rather than resources, and it carries Kueue's own word for the condition it summarizes,
  so the two are read without translating between them.
- **AC7.2** The reported figure is an observed count: a failed list writes no status rather than a
  zero.
- **AC7.3** The flavor a role was assigned becomes **`assignedFlavors`, a de-duplicated sorted
  list**: absent means no replica holds an assignment, one element means they agree and reads as the
  single value did, and more than one means they do not. With one Workload per replica the
  disagreement is reachable, where previously one role had one PodSet and therefore one assignment.
  ⚠️ **The shape is a list so that "mixed" and "none" stay two readings.** Reporting a single value
  only when the replicas agree would collapse them into one, and they call for opposite actions —
  investigate versus wait. That is the same rule AC7.2 states about a failed list, not an analogy to
  it. The cost, which the field's own documentation has to state, is that the list does not say
  which replica holds which flavor; that is answered from the Pods.

### Notes / Constraints / Caveats

**Kueue version.** The cluster runs the chart pinned in `hack/deps.sh`, **Kueue v0.18.4**. The
`sigs.k8s.io/kueue` module in `go.mod` is `v0.17.1` and is the compile-time library only.
Capabilities below were read from the v0.18.4 sources.

**Why the current design cannot grow a group.** In Kueue's plain-Pod integration, once a Workload
exists the comparison that decides whether it still matches reads the Workload's own PodSet counts
against the count of live Pods:

```go
// kueue v0.18.4, pkg/controller/jobs/pod/pod_controller.go, equivalentToWorkload
	// Check counts for found pod sets
	if !workloadFinished && wl.Spec.PodSets[j].Count < jobPodSets[i].Count {
		return false
	}
```

More live Pods than the Workload declares makes the Workload not equivalent; `ensureOneWorkload`
then finds no match for a running job and calls `stopJob`, and the Pod integration's `Stop` deletes
every Pod in the group. Fewer live Pods than declared stays equivalent — that asymmetry is what
lets a single replica be replaced today, and it is the whole of the current rollout path.

**Why the total count must agree across a group.** Composing a group's Workload validates that
every Pod in it declares the same total and that at least that many are runnable; either failure is
an unretryable error that composes no Workload at all, with an `ErrWorkloadCompose` event as the
only signal. A group whose declared total is always `1` cannot reach either failure.

**Elastic Workloads do not apply.** Kueue's `ElasticJobsViaWorkloadSlices` feature gate defaults to
`true` at v0.18, but enabling it for an object additionally requires the
`kueue.x-k8s.io/elastic-job: "true"` annotation, and a webhook forbids that annotation on any kind
outside a fixed list — `batch/v1 Job`, `ray.io/v1 RayCluster`, `ray.io/v1 RayJob`,
`ray.io/v1 RayService`. `corev1.Pod` is not in it. The same list is byte-identical at v0.18.4,
at v0.19.5 (the newest release at the time of writing) and on the upstream default branch, so
upgrading Kueue does not lift this.

**A standing upstream risk this spec reduces.** The comparison quoted above reads no field of the
Pod template, which is why an image or argv change today rolls replica by replica instead of
rebuilding the group. Upstream tracks that as a bug (kubernetes-sigs/kueue#14374, open at the time
of writing, filed against `v0.20.0-devel`, titled "Pod groups and both workload-slice paths adopt a
Workload without comparing the PodSet template"). If it is fixed by widening the comparison, every
template change on the current design becomes a whole-group restart. With one Workload per replica,
replacing a replica already means deleting that replica's Workload, so the comparison's scope
stops mattering.

**Kubernetes-native gang scheduling does not apply either.** KEP-4671 landed the Workload and
PodGroup APIs in v1.35 and reworked them into `scheduling.k8s.io/v1alpha2` in v1.36, still Alpha,
requiring the `GenericWorkload` gate on both kube-apiserver and kube-scheduler plus `GangScheduling`
on the scheduler. Its `minCount` is not mutable — mutability is listed as a v1.37 goal — so it
carries the same constraint this spec exists to remove, and it governs scheduling rather than quota
admission. The chart declares `kubeVersion: ">=1.23.0-0"`.

**Quota is not released by shrinking an admitted Workload.** `status.admission` is immutable once
set, so an admitted Workload's granted counts cannot be reduced in place. This is why shrinking a
role has to delete the Workloads of the replicas it removes rather than adjust a shared one.

**What the LeaderWorkerSet substrate looks like**, since Non-Goal 2 aims this design at it. It is
StatefulSet-descended rather than Deployment-descended: a group's name carries its index and not its
revision, a rolling update replaces each index in place from the highest down, and a surge borrows a
higher index rather than a new naming scope. The readings behind each of those are in Alternatives,
under the per-revision intermediate object. Two consequences land in this spec. Replacing a replica
in place is the substrate's own cadence rather than a limitation being worked around here. And a
surge policy — a Non-Goal today — would take the same shape when it arrives, an extra ordinal, which
F3's identity model already admits and a revision-scoped naming scheme would have to be unwound for.

### Boundaries

- **Always:** keep `kueue.x-k8s.io/role-hash` equal to the role's name, so a Workload's PodSet
  identity stays the role's identity.
- **Always:** read a replica's role from its resource note and its ordinal from its ordinal label.
  ⛔ Never parse either out of the Pod's name, and ⛔ never parse the ordinal back out of the group
  name. Here the name belongs to the API server, and a later substrate assigns it by its own rules —
  LeaderWorkerSet names a group `<lws>-<index>` — while the labels stay ours.
- **Always:** keep the renderer's output for a role independent of which replica it is rendering.
  The group metadata and the fingerprint are stamped on afterwards.
- **Ask first:** before introducing any new spec field beyond the instance-size field this spec
  names.
- **Ask first:** before changing what the joint-admission barrier promises, as opposed to what it
  counts.
- **Never:** introduce a workload-backend abstraction with one implementation. The shape of such an
  interface has to come from the second implementation.
- **Never:** patch a Kueue-owned `Workload` object's spec to steer admission.
- **Never:** set the pod-group fast-admission annotation. It takes the first runnable Pod of a
  group, sets the PodSet count to the group's whole total and returns, so the Workload exists while
  the group is still short — the opposite of what this design needs.

### Risks and Mitigations

- **A departing replica's Kueue finalizer holds its slot until that replica's Workload is deleted**
  → ✅ **MEASURED, and the assumption holds.** On a two-node k3s cluster running Kueue v0.18.4, hand-applied YAML — no
  operator involved — established two Pods, each its own group of `pod-group-total-count: "1"`
  carrying `pod-group-serving: "true"`, both admitted and **both actually Running** before anything
  was deleted. Readings:

  | Observable | Reading |
  |---|---|
  | Workload name vs group name vs Pod name | identical |
  | **Control** — delete the Pod, leave its Workload | Pod keeps finalizer `["kueue.x-k8s.io/managed"]`, Workload still present, and `kubectl create` of the same name fails: `Error from server (AlreadyExists): object is being deleted` |
  | **Main** — then delete that replica's Workload | Pod object gone **1s** later |
  | Recreate under the same name | succeeded in **0s**, new UID |
  | Sibling replica throughout | UID unchanged, phase Running, its Workload still Admitted |

  The control is what makes this more than a green light: it shows the departing replica's slot
  stays occupied for as long as its Workload stands, and that deleting that Workload is what frees
  it. The same-name recreate in the main leg is incidental to F3 as it now stands — what the leg
  establishes is the **timing**, that the slot is free within a second of the Workload delete.

  ⚠️ **That run's one gap — the drain path — has since been measured and is closed.** T1's
  placeholder exits immediately on SIGTERM, so it never showed what a draining replica does. A
  second run used a container that traps SIGTERM and drains for 40s, in the same serving group of
  one. Readings:

  | Observable | Reading |
  |---|---|
  | Pod deleted, **Workload left standing** | `Running` through the drain, then **`Succeeded`** — and still present, finalizered, Workload still Admitted, at **154s** |
  | Same, but the Workload deleted **while the container was still draining** | Pod gone **36s** later |

  Two things follow. The departing replica **never frees its own slot**, whatever its phase, which
  is what AC3.3 is built on. And the wait is **the drain's length, not a constant** — 36s here
  against T1's 1s, from the same delete on the same kind of object. A later run put a number on the
  other end of that range: with the container already terminated there is no drain left to wait out
  and the release is **immediate**. That run also measured the Eviction API a node drain calls
  (identical to `kubectl delete pod` field for field) and both terminal phases; AC3.3 above carries
  it, because what diverges between those phases is what decides the remedy.
- **Workload count grows from one per role to one per replica** → A deployment with 8 prefill and
  8 decode replicas goes from 2 Workloads to 16, and Kueue admits through a per-ClusterQueue serial
  path. The concern was that this grows quadratically, which would invalidate the approach for
  large roles. ✅ **MEASURED, and it does not.** On the cluster, Pods were applied in one batch and
  the wall clock ran until every Workload of that case read `Admitted`:

  | Shape | N=4 | N=16 |
  |---|---|---|
  | one Workload per replica (this spec) | 0.3s | 0.5s |
  | one Workload of `Count: N` (today) — control | 0.3s | 0.6s |

  ⚠️ **Read only what this resolves.** The poll interval was 0.5s, so these figures sit at the
  instrument's floor and ⛔ do not support a claim that either shape is faster. What they do rule
  out is the thing that mattered: quadratic growth from N=4 to N=16 would have been roughly 16x,
  near 5 seconds, and nothing close to that appears. The counter was scoped to the case's own
  Workloads and asserted to read 0 before each case began — an earlier run of this measurement was
  wrong precisely because a bare namespace count was already satisfied by leftovers, so the loop
  exited before the case had converged.
- ⭐ **A rollout replacement stops being free, and on a full pool the rollout can strip the
  deployment to zero** → This is what one Workload per *role* was quietly providing, and the
  mechanism is exact. A group's Workload name **is** the group name (`pod_controller.go:1171`), so
  a replacement Pod in the same group recomposes a Workload of that same name, and the existing
  admitted Workload is accepted as equivalent because the comparison reads only the PodSet counts
  and never the template (`:1337`). While a role's *other* replicas keep that Workload standing, a
  single replacement rides the existing reservation in for free.
  ⚠️ **One replica per group removes that subsidy, and no choice of naming brings it back.** At
  `Count: 1` no other member holds the Workload up, and the departing member does not step aside on
  its own — AC3.3 carries the three readings behind that. Freeing the slot means deleting that
  replica's Workload, and deleting it drops the reservation. The replacement is therefore a fresh
  admission competing for the quota it just released, and on a contended pool something else can
  take it in between. `[read from source]`
  ⚠️ **A second, pre-existing defect compounds it**: the rollout guard counts **live** Pods, not
  admitted ones (`model_deployment.go:492`). A created-but-still-pending replacement counts as
  live, so later passes keep deleting outdated replicas while nothing can reserve — worst case the
  rollout walks the deployment down to zero admitted replicas and stops there, and the barrier's
  park bound does not cover it because the deployment is not short of its declared count.
  **Mitigation, in two halves that only work together.** `model_deployment.go:492` counts
  **admitted** replicas rather than live ones, reading `Admitted=True` off each replica's own
  Workload — ⛔ not `QuotaReserved`, because the window where a Workload has reserved but is not yet
  admitted is exactly the one the joint-admission barrier holds open, and a guard reading it would
  send the rollout after quota the barrier is waiting on. And the replacement create waits for the
  slot to be **observably empty** — the departing Pod gone and its Workload deleted — with at most
  one admission in flight per role.
  ⚠️ **"Observed quota headroom" deliberately does not mean reading the ClusterQueue.** Under a
  cohort with borrowing, `usage < nominalQuota` does not imply a Workload will be admitted, so that
  reading produces **false positives** — a gate that says "there is room" and then creates a Pod
  that can never be scheduled is worse than no gate. It would also race its own reading and pull
  ClusterQueue status shape into this operator. A replacement that sits gated in the queue costs
  only convergence time, so the gate is biased toward creating late rather than creating wrongly.
  Both halves belong to T7. The system is coherent without T7 (see Checkpoints); it simply rolls less safely on a
  contended pool — which is why T7 is not optional for a production rollout even though it is
  sequenced last.
- **The operator's own barrier is quadratic in replica count** → `jointSiblings` fans one Workload
  event out to every sibling with Pod and Workload lists per event
  (`model_deployment_joint_admission.go:632-693`), and each verdict runs a full namespace Workload
  list (`:414`, `:499-501`) — so an admission wave costs `O(R²)` reconciles of `O(R × W)` each,
  where today R is the number of roles and afterwards it is the number of replicas. Separately,
  the convergence loop's `listWorkloads` (`model_deployment.go:380`) is a genuinely **uncached**
  namespace-wide list through `APIReader`, issued on every pass while any role is short;
  per-replica multiplies both the list width and the number of short windows.
  Measured in T1's companion experiment before T6 is considered done.
- **Every existing deployment rebuilds once on upgrade** → Group names and declared totals both
  move, so replicas rendered before this change are stale everywhere. It happens once and is stated
  in the release notes rather than left to be discovered.
- **Partial admission of a role becomes visible where it was previously impossible** → A role can
  now sit with some replicas admitted and some waiting. F7 makes it readable; the barrier still
  prevents the deployment from serving in that state.

## Design Details

### The API surface, field by field

Five fields moved. Two are new, one replaced a scalar with a list, and two kept their names while
the thing they name changed. `ModelDeployment` is in no released version, so none of this is a
migration.

| Field | Change | What it means now |
|---|---|---|
| `spec.roles[].replicas` | semantics | How many **independent serving instances** the role runs |
| `spec.roles[].size` | **new**, default `1` | How many **Pods form one instance** |
| `spec.roles[].resources.accelerator` | semantics | How many cards **ONE POD** asks for |
| `status.roles[].quotaReserved` | **new**, always present | How many of the role's replicas hold a quota reservation |
| `status.roles[].assignedFlavors` | replaces `assignedFlavor` | The **set** of ResourceFlavors Kueue assigned across the role's replicas |

#### `spec.roles[].replicas` — the count that no longer rebuilds anything

Editing it adds or removes instances **and does nothing else**. Survivors are not restarted, do not
reload their weights, and keep whatever cache they hold. This is the whole point of the spec: before
it, changing `replicas` moved a number every member of the role carried, so Kueue's group had to be
torn down and recomposed, and every replica of that role went with it.

Scale-down sheds the **highest ordinals first** and deletes each departing replica's Workload with
its Pod, so the quota is returned rather than stranded.

- **Use it for** capacity: more load, more instances.
- ⛔ **Do not expect it to change an instance's shape.** That is `size` and the container fields,
  and those do replace instances.

#### `spec.roles[].size` — how big one instance is

The Pods of one instance are fate-sharing: they start together, they are replaced together, and none
of them serves alone. **Today the only accepted value is 1**, and the refusal names the missing
capability rather than a bound — the rendering that builds one instance across several Pods does not
exist yet, so a bounds-shaped message would read as a permanent rule after the day it lands.

⚠️ **Changing it replaces every instance of the role**, because the Pods a running instance is made
of are not the Pods the new size asks for.

- **Use it for** the multi-Pod instance shapes this API is being kept open for (tensor parallelism
  across Pods, a future LeaderWorkerSet substrate).
- The Go identifier is `ReplicaSize`, not `Size` — gogo protobuf puts a `Size()` method on the type.
  The API name is `size`, which matches LeaderWorkerSet's own field.

#### `spec.roles[].resources.accelerator` — per Pod, not per replica

At `size: 1` the two readings coincide, which is why the field could be ambiguous before `size`
existed. It is now stated as per-Pod, and that is what admission and the feasibility check read.

⚠️ **An explicit `0` on an acceleratable InstanceType is refused when another role shares that
type.** The reason is measured rather than theoretical: such a replica reserves, reports `Admitted`,
runs, and the queue charges it **nothing**, while its siblings on that type are charged for every
card they hold. A Pod meant to run without an accelerator belongs on a non-acceleratable
InstanceType, where CPU is what the queue accounts in.

#### `status.roles[].quotaReserved` — the figure that could not exist before

Each replica is its own Kueue Workload, so a role sits at **any count between zero and `desired`**
while capacity arrives. Under one Workload per role the answer was all-or-nothing and there was
nothing to report.

- **Read it to tell** "half the role is waiting for capacity" from "the role is fully placed".
- ⭐ **Its zero is an observed zero.** A pass that could not list Pods or Workloads writes **no
  status at all** rather than a zero, because "could not see" and "none reserved" call for opposite
  actions — one waits, the other investigates.

#### `status.roles[].assignedFlavors` — a set, because replicas can disagree

Kueue assigns a flavor per Workload and a role's replicas are one Workload each, so two replicas of
one role can land on different flavors. A scalar field could only ever report one of them.

| Reading | Meaning |
|---|---|
| absent | No assigned replica named a flavor — covers both "not assigned yet" and "assigned on a pool carrying no accelerator names" |
| one entry | Every assigned replica of the role names it |
| several entries | The replicas were assigned **different** flavors — a signal to investigate |

⛔ **It does not say WHICH replica carries which flavor.** That would key on the ordinal, which is
the converger's internal slotting rather than a promise this API makes; a reader who needs it reads
the replicas' own Pods.

⚠️ **An admitted role may still report nothing here**, and that is the contract rather than a gap:
the answer comes from the same function the per-accelerator admission gate uses, which speaks only
of accelerator credits. A role admitted on a pool carrying no accelerator names a flavor for `cpu`
and nothing here. Reporting a flavor the gate would not fit against would be worse than reporting
none.

#### What did not change, and one consequence that did

`instanceType`, `name` and every container field keep their meanings. But **two roles sharing a
name** now fail differently: the name is part of the group each replica joins, so the two roles'
same-numbered replicas land in one group that declares a single member and Kueue deletes whichever
arrived later. It used to merge them into one PodSet whose count was their sum. The refusal is
unchanged — and unreachable in practice, since `+listType=map +listMapKey=name` makes the API server
reject a duplicate before any webhook runs.

### Commands

```sh
make lint                     # Go and shell; run for every source change
make lint docs                # Markdown; a separate dispatch, 0 from one says nothing about the other
make generate                 # after editing api/ types or webhook sources
go test ./pkg/worker/controllers/worker/...   # the controller's own suite
go test ./api/...                             # API validation and defaulting
```

`make generate` regenerates deepcopy, register, apiservice, CRD, conversion, protobuf and webhook
code. It must run after any change under `api/`, and its output is committed.

⚠️ **`make generate` cannot run from a git worktree, and the two tasks that need it have to route
around it.** `gen/api/main.go`'s `resolveProjectDir` requires the resolved working directory to end
in `/gpustack.ai/gpustack`, resolving symlinks on the way, with no override — a drift guard that
predates this work. A worktree path never ends that way, so the run exits 255. It fails **cleanly**:
it installs a protoc toolchain into the gitignored `.sbin` and touches nothing tracked.

The route around it, for T4 and T8: copy the tree (without `.git`, which in a worktree is a **file**
and is missed by the usual directory exclusion) to a temporary path that does end in
`/gpustack.ai/gpustack`, generate there, and copy the generated files back. The acceptance for that
copy-back has to close **both** directions — every file on the generated list arrives with a matching
hash, **and** `git status --porcelain` afterwards shows exactly that list plus the task's own edited
sources, nothing more. Verifying only the first direction cannot see a file that should not have come
back, and a wrongly-copied generated file looks exactly like a correct one.

⛔ Do not run the generator in the user's primary checkout to sidestep this: other sessions work
there. Lifting the restriction itself is a change to `gen/api/main.go` and belongs to its own
change, not to this spec.

### Project Structure

```
api/worker/v1alpha1/
  model_deployment.go              # ModelDeploymentRole and ModelDeploymentRoleStatus live here
pkg/worker/controllers/worker/
  model_deployment.go              # the convergence loop: render, compare, create, replace
  model_deployment_render.go       # one Pod per role, plus the spec-hash fingerprint
  model_deployment_pod_group.go    # Kueue group metadata; the file F1 and F2 rewrite
  model_deployment_joint_admission.go  # the AdmissionCheck barrier; F5
  model_deployment_status.go       # status projection; F6 and F7
  model_deployment_service.go      # unchanged by this spec
  model_deployment_router.go       # unchanged by this spec
  model_deployment_events.go       # unchanged by this spec
```

### Code Style

The group metadata is returned as one value because a Pod carrying the membership label without the
total count joins a group whose size Kueue cannot learn, and no Workload is composed at all — the
failure is silence, so the two travel together:

```go
// ModelDeploymentPodGroup returns the group metadata for ONE replica.
//
// The declared total is always one. A replica is its own group, so no two members can disagree on
// a total and no group is ever short of one — the two states that compose no Workload at all.
//
// THE ROLE HASH STAYS THE ROLE'S NAME. Kueue reads this annotation verbatim and otherwise derives
// a digest of the Pod spec's shape; per-role flavor assignment and per-role status both join a
// Workload's PodSets to the roles by this name, so a digest breaks the join while nothing errors.
//
// The ordinal is a label rather than an annotation because the create path selects on it: the read
// that decides whether this replica already exists has to be answered by the API server, and an
// annotation cannot be selected on. It is written here, in the same value as the group name, so
// that the two can never describe different replicas.
func ModelDeploymentPodGroup(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal int,
) ModelDeploymentPodGroupMeta {
	return ModelDeploymentPodGroupMeta{
		Labels: map[string]string{
			kueuepodconst.GroupNameLabel:       modelDeploymentReplicaGroupName(md, role.Name, ordinal),
			modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
		},
		Annotations: map[string]string{
			kueuepodconst.GroupTotalCountAnnotation: "1",
			kueuepodconst.RoleHashAnnotation:        role.Name,
			kueuepodconst.GroupServingAnnotationKey: kueuepodconst.GroupServingAnnotationValue,
		},
	}
}
```

Conventions this codebase holds to and this change keeps: comments state the rule and the reason
beside it, never a task identifier from a spec; exported API fields document behavior, expectations
and constraints; a comment explaining why something is written a certain way carries the
counterfactual in the same sentence as the claim.

Because `ModelDeployment` has never been released, field removals renumber protobuf tags
consecutively rather than leaving reserved holes.

### Implementation Plan

Every task is a complete vertical path — types, behavior and tests — and leaves the tree green.
`Owns:` paths are disjoint wherever the work allows, so tasks with no edge between them can run at
the same time. T2, T3 and T4 are unblocked from the start and touch nothing in common.

**De-risk first.** T1 writes no product code and gates T7. It is ordered first because the
assumption it tests is the only one in this spec whose failure changes the design rather than the
implementation.

- [x] **T1 · PoC: does deleting a per-replica Workload free that replica's slot in time** —
      ✅ **DONE, passed.**
      Readings and the boundary of the measurement are in Risks and Mitigations above. T7 is
      unblocked; its one carried-over obligation is to re-measure the drain path against an engine
      image that does not exit immediately on SIGTERM.
      Blocked by: None
      Owns: `<no product files>`
      Gate: review
      Acceptance: On a cluster running Kueue v0.18.4, hand-applied YAML — no operator involvement —
      establishes a Pod in a pod group of `pod-group-total-count: "1"` carrying
      `kueue.x-k8s.io/pod-group-serving: "true"`, lets Kueue admit it, then deletes the Pod and its
      Workload and creates a Pod under the same name. Three readings are recorded: (a) the Kueue
      finalizer is released after the Workload delete and not before; (b) the same-name create
      succeeds, with the elapsed time from Workload delete to successful create; (c) deleting one
      replica's Workload leaves a sibling replica's Workload admitted and its Pod running.
      A fourth reading rules out the failure mode this replaces: with the Workload left in place,
      the same-name create is rejected, so (b) is attributable to the Workload delete.
      Verify: manual, transcript pasted into the spec's Risks section; no automated check exists
      for a claim about another controller's finalizer timing.

- [x] **T2 · Prefactor: split the renderer into template and stamp**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Acceptance: `renderModelDeploymentPod` is split into a function producing everything that does
      not vary by replica (labels, annotations, PodSpec) and a second that stamps the Kueue group
      metadata and writes the spec-hash annotation. No behavior changes: the rendered Pod is
      byte-identical to what the current code produces for the same inputs. A test asserts the
      first function's output is identical across two replicas of one role — this is the invariant
      that keeps replica identity out of the render path.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestRenderModelDeployment' -v`

- [x] **T3 · Prefactor: one way to find a replica's Workload**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_status.go`,
      `pkg/worker/controllers/worker/model_deployment_status_test.go`
      Acceptance: `modelDeploymentGroupOfRole` and `modelDeploymentWorkloadByGroup` — which match a
      Workload by a group name derived from the spec — are removed, and every caller reaches
      Workloads through `findModelDeploymentGroupWorkloads`, which matches by the Pods a Workload
      owns. Status output is unchanged for every existing case. A test covers a Workload whose name
      does not match any name this operator would derive, and asserts it is still attributed
      correctly.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeploymentStatus' -v`

- [x] **T4 · The role's two numbers**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: `spec.roles[].replicas` is re-documented as the count of independent serving
      instances, stating that changing it does not restart a running instance. A `size` field is
      added, defaulting to `1`, documented as the number of Pods forming one instance and as
      fate-sharing. `spec.roles[].resources.accelerator` is re-documented as what one Pod asks for.
      A webhook rejects any `size` other than `1`, with a message naming the missing capability
      rather than the field's bounds. Protobuf tags are renumbered consecutively where a field
      moved; `make generate` output is committed and the tree is clean afterwards.
      Verify: `make generate && git diff --exit-code && go test ./pkg/worker/webhooks/worker/ ./api/... -run 'ModelDeployment'`

- [x] **T5 · One pod group per replica**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go`,
      `pkg/worker/controllers/worker/model_deployment_pod_group_test.go`,
      `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_test.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout.go`,
      `pkg/worker/controllers/worker/model_deployment_render.go` (the stamp function and the render
      input only — the ordinal travels in `ModelDeploymentRenderInput` so that
      `renderModelDeploymentPod` keeps its signature and its out-of-scope callers keep compiling),
      `pkg/worker/controllers/worker/model_deployment_render_test.go` (the stamp's own cases only),
      `pkg/worker/controllers/worker/model_deployment_group_test.go`,
      `pkg/worker/controllers/worker/model_deployment_events_test.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout_test.go`,
      `pkg/worker/controllers/worker/model_deployment_service_test.go`
      Gate: review
      Acceptance: Every rendered Pod declares `pod-group-total-count: "1"` and a group name derived
      per replica whose shape does not depend on how many roles the deployment has. Every rendered
      Pod also carries its ordinal as a **label**, written from the same value as the group name so
      the two can never describe different replicas; T7 is what reads it, but it is stamped here
      because this is where per-replica metadata is written.
      `modelDeploymentGroupsResizing` and the rebuild path it drives are removed; the branch that
      brings down a group whose role was renamed or dropped survives, relocated into the
      convergence loop. Creation is keyed by which ordinal is missing rather than by a shortfall
      count. Scale-up 2→3 leaves both pre-existing Pods as the same objects; scale-down 2→1 issues
      exactly one Delete; adding a second role to a single-role deployment leaves the first role's
      Pods untouched and never puts one role's Pods in two groups. Survival is asserted with a
      marker the renderer never writes, not with names and not with UIDs.
      The existing assertions this inverts are updated rather than deleted: the 2→1 scale that
      asserts two Deletes, and the service test that asserts a scale takes two passes.
      ⚠️ **Two mechanics the convergence loop depends on and that this task must carry:**
      (a) **the desired-render map is keyed by `(role, ordinal)`, not by role.** Today
      `wantHash := desired[role.Name]...` (`model_deployment.go:393`) is per role, while the hash
      covers the labels — which now include a group name that differs per replica. Left keyed by
      role, every comparison runs against the wrong hash.
      (b) **the hash must keep covering the group-name label.** Removing
      `modelDeploymentGroupsResizing` removes the only other net that catches a Pod sitting in a
      group the spec no longer forms; if the hash stops covering that label, Pods carrying
      pre-upgrade group names are never seen as outdated and never roll.
      Scale-down deletes the Pod **and** that replica's Workload (see AC1.5) — the delete-counting
      test asserts one of each kind, because a test counting deletes alone passes against the leak.
      ⚠️ **One coupling T3 could not reach, and it breaks exactly here.** `observeModelDeploymentRollout`
      (`model_deployment_rollout.go:108`) and `modelDeploymentReplicasMissing` (`:233`) still read
      through `wlByGroup[groupOfRole[role.Name]]` — a map from **one role to one group name**. That
      mapping stops existing the moment a role's replicas sit in a group each, and it does not fail
      loudly: the lookup simply misses and the rollout observes nothing. `model_deployment_rollout.go`
      is in this task's `Owns:` for that reason. Reported by T3 `[read from source]`.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeployment' -v`

- [x] **T6 · The barrier counts replicas**
      Blocked by: T3, T5
      Owns: `pkg/worker/controllers/worker/model_deployment_joint_admission.go`,
      `pkg/worker/controllers/worker/model_deployment_joint_admission_test.go`
      Gate: review
      Acceptance: The barrier answers `Ready` only when every replica of every role holds a quota
      reservation, `Pending` otherwise, and never `Retry` or `Reject`. An already-admitted Workload
      is still skipped. A replica whose Workload is absent because this operator is replacing it
      counts as present, and a test drives a rollout to completion with the barrier active to prove
      no deadlock. A Workload belonging to no multi-role `ModelDeployment` is answered `Ready` at
      once. The park bound still applies only to a deployment whose roles all have their declared
      replica count.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'JointAdmission' -v`

- [x] **T7 · Idempotent creation, and a rollout that can survive a full pool**
      Blocked by: T1, T5
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Gate: review
      Acceptance: Two passes over an unchanged spec assign the same ordinal to the same replica
      (the label itself is stamped by T5). Each create is preceded by an `APIReader` read selecting
      on that one ordinal. A `Create` whose response was lost leaves exactly
      one Pod for that ordinal on the next pass — asserted on the object count through a client that
      persists the object and then fails the call, not on the absence of an error. A replacement is
      created only after that ordinal reads empty, and freeing the ordinal deletes that replica's
      Workload. Scale-down and rollout both select by ordinal rather than by creation timestamp, and
      `modelDeploymentSortReplicasNewestFirst` is removed if it has no remaining caller.
      The rollout guard at `model_deployment.go:492` counts **admitted** replicas rather than live
      ones, and the replacement create is conditional on observed quota headroom — without both, a
      rollout on a contended pool walks the deployment toward zero admitted replicas (see Risks).
      ✅ **T1's carried-over measurement is done** — the drain path was measured separately and both
      readings are in Risks. The departing replica never frees its own slot, and the wait is the
      drain's length rather than a constant. ⇒ The create gate here reads observed state;
      ⛔ it must not be written against a timeout.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeployment' -v`

- [x] **T8 · Status reports admission progress**
      Blocked by: T3, T4, T5
      Owns: `pkg/worker/controllers/worker/model_deployment_status.go`,
      `pkg/worker/controllers/worker/model_deployment_status_test.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout_test.go`,
      `api/worker/v1alpha1/model_deployment.go`
      Acceptance: `status.roles[]` reports how many of the role's replicas hold a quota
      reservation, counted from a list that succeeded — a failed list writes no status rather than
      a zero. `assignedFlavor` states and implements what it reports when a role's replicas are not
      all assigned the same flavor. A test covers a role with two replicas assigned different
      flavors, which one role with one PodSet could not previously produce.
      Verify: `make generate && git diff --exit-code && go test ./pkg/worker/controllers/worker/ -run 'TestModelDeploymentStatus' -v`

- [x] **T9 · Documentation**
      Blocked by: T5, T8
      Owns: `docs/reference/model-deployment.md`, `docs/reference/model-deployment-status.md`
      Acceptance: The sentence "The replicas of a role become one Kueue pod group — one group per
      role" is replaced by what is now true. The rollout section states that a replica-count change
      adds or removes replicas rather than rebuilding the role, and which changes still replace
      every replica. The status page documents the admission-progress figure. The page's heading
      budget is respected: `model-deployment.md` carries nine counted `##` headings against a limit
      of ten, so at most one new section may be added and the rollout content belongs in the
      existing "Rollout is a rolling replacement" section rather than a new one.
      Verify: `make lint docs`

**Checkpoints.** After T4 the API is settled and the other tracks can assume it. After T6 the
system is coherent and every goal about replica-count elasticity holds. ⚠️ **It is not shippable
there**: T7 carries both create idempotence and the two changes that keep a rollout from walking a
deployment toward zero admitted replicas on a contended pool (see Risks). What T6 buys is a
reviewable, working intermediate state — not a release. After T9 the documented behavior matches
the built behavior.

**T4 runs first and alone.** It is the only task that runs `make generate`, and the generation tree
is shared across worktrees, so it cannot overlap with anything. T8 also regenerates, and is already
`Blocked by: T4`. Once T4 lands, T2 and T3 have disjoint `Owns:` and run concurrently.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

**The e2e suite's description of the current shape is already stale, and that must be fixed before
this work starts** — otherwise a failure caused by this change cannot be told from one that was
already there. CASE 49 is titled "A multi-role ModelDeployment becomes ONE Kueue Workload with one
PodSet per role", which describes a per-deployment group that no longer exists: a role is its own
group and its own Workload today. Reconcile the case's title and its rows against the current code
first, as a separate change, so this spec's own effect on it is legible.

**Existing unit assertions that this spec inverts** are updated inside the task that inverts them,
never deleted, so the diff shows the behavior change rather than a disappearance:

- the 2→1 scale asserting two Deletes and "the scale rebuilds the group rather than trimming it";
- the service test asserting a replicas change takes two passes because the group is rebuilt;
- the group tests asserting a replicas change or a role-set change rebuilds a whole group.

**A positive baseline is required for the new negative assertions.** "No replica was restarted" is
vacuously true against a reconciler that did nothing, so every survival assertion is paired with a
case in the same test that does restart replicas — an image change — and observes the marker
disappear there.

#### Unit tests

Fake-client controller tests, which is what this repository has: there is no envtest harness, so no
unit test can observe Kueue composing, admitting or evicting a Workload. Every assertion below is
about what this operator writes, and the Kueue-side consequences are e2e's.

Coverage floors are the measured values at the time of planning; no task may lower its package.

- `pkg/worker/controllers/worker`: `2026-09-19` - `79.4%`
- `pkg/worker/webhooks/worker`: `2026-09-19` - `91.2%`

Per task:

- **T2** — the template half of the renderer produces identical output for two replicas of one
  role; the stamped Pod is byte-identical to what the pre-split renderer produced.
- **T3** — a Workload whose name matches nothing this operator would derive is still attributed to
  the replica that owns it; status output is unchanged across every pre-existing case.
- **T4** — `size` defaults to 1; a `size` other than 1 is refused with a message naming the missing
  capability; `make generate` leaves the tree clean.
- **T5** — scale-up 2→3 and scale-down 2→1 leave surviving Pods as the same objects, asserted by a
  marker the renderer never writes; exactly one Delete for a 2→1; a single-role deployment gaining
  a second role leaves the first role's Pods untouched and never places one role's Pods in two
  groups; every rendered Pod declares total count 1 and carries its ordinal label; group names are
  identical whether the deployment has one role or two; two deployments of one name in two
  namespaces derive different group names.
- **T6** — `Ready` only when every replica has reserved; `Pending` otherwise and never `Retry`; an
  admitted Workload is skipped; a rollout runs to completion with the barrier active (the deadlock
  guard); a Workload owned by nothing of ours is `Ready` at once; the park bound does not fire for
  a deployment short of its own replicas.
- **T7** — two passes assign the same ordinal to the same replica; a client that persists a Pod and
  then fails the create leaves exactly one Pod for that ordinal on retry, asserted on the object
  count; a replacement is created only after that ordinal reads empty, asserted on call order; the
  rollout guard holds when a role's replicas are live but not yet admitted.
- **T8** — a role with two replicas assigned different flavors reports what the field documents; a
  failed Pod list writes no status rather than a zero admitted count.

#### Integration tests

None. This repository has no envtest harness, and adding one is out of scope for this spec. The
gap it leaves is explicit: every claim about how Kueue reacts — composing a Workload, admitting it,
releasing a finalizer, evicting on a mismatch — is unverifiable between the unit tests and e2e, and
is therefore assigned to e2e below rather than assumed.

#### e2e tests

This change touches reconcile and an admission webhook, which is the `gpustack-operator-e2e`
trigger. Four existing cases are in its blast radius and one new case is required.

- **CASE 45** (the ModelDeployment admission surface) — must still pass unchanged; T4 adds one
  refusal to it, the `size` rejection, from the webhook layer that owns it.
- **CASE 49** (the group forms, and deleting the deployment completes) — retitled and re-rowed for
  one Workload per replica. Its existing row asserting the operator breaks the
  Workload-holds-finalizer-holds-Workload cycle itself is the row T7 depends on, and it becomes
  per-replica: deleting one replica's Workload must release that replica's Pod and leave a
  sibling's Pod running and admitted.
- **CASE 50** (a short pool starves a P/D deployment whole) — must still pass, and is the e2e proof
  that T6 preserved the barrier's promise. Its verdict is unchanged; only the number of Workloads
  behind it moves.
- **CASE 51** (every P/D refusal fires, and a group shape change rebuilds the group) — the second
  half is inverted by this spec. The row asserting a shape change rebuilds the group becomes a row
  asserting a replicas change does **not**, measured on Pod UIDs across the change, with a control
  row that does rebuild (a role rename) so the observable is proven able to report both values.
- **NEW CASE** — scale a running role up and then down on a real cluster and assert: the
  pre-existing replicas keep their UIDs and their Ready condition throughout; the added replica
  gets its own Workload; the removed replica's Workload is gone; the cluster queue's admitted
  quota moves by exactly one replica's worth in each direction. This is the only place the quota
  arithmetic can be observed at all — a fake client has no ClusterQueue.

The PoC in T1 is not an e2e case: it runs before any code exists, against hand-applied YAML. Its
result is recorded in this spec rather than in the suite.

> Cross-check findings are folded in below once the independent review returns.

## Alternatives

### Keep one group per role and add creation expectations

Adds a ReplicaSet-style expectations store to make replica creation idempotent, and changes nothing
else. Rejected as a solution to this spec's problem: it addresses the create path while the failure
is on the update path, so a replica count change still rebuilds the role. It is also an incomplete
answer to idempotence on its own terms — an expectations store is in-memory client-side state that
a controller restart clears, and it records *how many* creates were issued rather than *which
ones*, so it cannot tell a lost response from a lost write. F3's ordinal annotation puts that
marker on the API server instead.

### Move to a StatefulSet per role

Rejected on three independent grounds, all of them in what Kueue's StatefulSet integration stamps
onto each Pod (`pkg/controller/jobs/statefulset/statefulset_pod_reconciler.go`):

1. `GroupTotalCountAnnotation` is written from `*sts.Spec.Replicas`, and the group name is the one
   Workload name for the whole StatefulSet. A replica count change moves the declared total on
   every Pod — **structurally the same defect this spec exists to remove**, relocated from this
   operator's code into Kueue's.
2. It sets `GroupFastAdmissionAnnotationKey` unconditionally, and there is no switch to turn that
   off. Setting that annotation is a **Never** in this spec's Boundaries, for the reason given
   there: it admits on the first runnable Pod with the PodSet count set to the whole group's total.
3. It sets `RoleHashAnnotation` to `kueue.DefaultPodSetName` — one PodSet per StatefulSet. Per-role
   flavor assignment and per-role status attribution both join a PodSet to a role through that
   annotation, so both joins break, and neither errors when it does.

Running the StatefulSet with `replicas: 1` per replica sidesteps (1) but is the shape a
LeaderWorkerSet already builds, with StatefulSet then contributing only stable names and stable
PVCs — the PVCs are unwanted, and F3 obtains identity without needing names at all. It also costs a
headless Service and an explicit `Parallel` pod-management policy, because `OrderedReady` blocks
forever on a Pod that Kueue has scheduling-gated.

There is a structural obstacle underneath all of it: **a StatefulSet carries one Pod template, and
this design needs a group name that differs per replica.** A template cannot express a value that
varies by ordinal, so the per-replica group metadata would have to be written by a mutating Pod
webhook rewriting Pods some other controller created — more machinery than rendering the Pods
directly, not less.

### Introduce a per-revision ReplicaSet between the deployment and its Pods

A `ModelDeployment` would own a revision-scoped intermediate object — one per template revision —
and each of those would own Pods named within its own namespace, so a rollout would create a new
intermediate and drain the old one while the two never contend for a name. It is a coherent design
and it is the shape `Deployment` itself takes.

Rejected here because it is a bet against the substrate this spec is aiming at. LeaderWorkerSet is
StatefulSet-descended, not Deployment-descended: `[read from source, lws v0.8.0]` it names a group
`fmt.Sprintf("%s-%d", lws.Name, idx)` with no revision in the name
(`pkg/controllers/leaderworkerset_controller.go:574`); its rolling update drives the underlying
StatefulSet's `partition` from the highest index down to zero, replacing each ordinal **in place**
(`rollingUpdateParameters`, `:240-344`); and its `maxSurge` is implemented by temporarily raising
that StatefulSet's replica count, so a surged group borrows a **higher ordinal** rather than a new
naming scope (`:285-298`). A per-revision layer would therefore be built, shipped as a CRD, and
then deleted — along with its revision GC and its template-hash collision handling — the moment the
substrate changed, and Pod names visible to users would move twice.

It should be reconsidered as the primary shape if the decision is made **not** to go to
LeaderWorkerSet, because then nothing else supplies revision-scoped naming. One thing it offers
that F3 does not is a revision label a Service selector can match, which Open Question 5 needs; a
`spec-hash` label on the Pod supplies the same thing without a new kind.

If it were built, its intermediate object should be named by content — `<deployment>-<role>-<hash>`
— rather than by `GenerateName`. A `GenerateName`d intermediate does not remove the lost-response
problem, it relocates it one level up and **amplifies** it: a duplicate Pod costs one accelerator,
a duplicate intermediate costs `replicas` of them.

### Move to a Deployment per role

Rejected because Kueue's Deployment integration copies the queue-name label onto the Pod template
and nothing else: every Pod becomes its own Workload with no gang semantics anywhere. The
joint-admission barrier would have to be rebuilt to cover every Pod, and it would then have to
coexist with the Deployment controller's `progressDeadlineSeconds`, which cannot see Kueue's
scheduling gates and reports a stalled rollout when replicas are merely queued. Its rolling update
would also have to run at `maxSurge: 0` to avoid demanding an extra accelerator's worth of quota
mid-rollout, which reduces it to the recreate cadence already in place.

### Move to a JobSet

Rejected because a JobSet composes one Workload for the whole set, with one PodSet per
replicatedJob sized `replicas × parallelism`. Changing any role's replica count rebuilds that
single Workload, which is worse than the current design — it loses the per-role isolation that
already exists. Job semantics also give no rolling update, and an inference deployment never
finishes.

### Move to a LeaderWorkerSet now

Not rejected on the merits — it is the likely long-term substrate, and the Kueue chart in this
repository already lists `leaderworkerset.x-k8s.io/leaderworkerset` among its enabled integrations,
so no Kueue configuration would change. Kueue's LWS integration names one Workload per replica
group and, on a replica count change, creates or deletes Workloads for the affected indices while
existing ones are only updated for queue name and priority — never for their PodSets. Upstream
documents the same behavior: on scale up, only the newly created group of Pods is gated.

It is deferred rather than taken because it is a substrate change — a new CRD and controller
dependency, a new chart subchart, a new Go module — while the first thing it requires is exactly
what this spec does: move the admission unit to one replica and re-base the barrier on it. Doing
that first makes the substrate question a separate, smaller decision. The render path this spec
keeps intact is the same template an LWS would carry.

### Admit a role once at least one replica has reserved quota

Rejected for this spec. It is a change to what "this deployment is serving" means rather than to
the admission unit, and it is independent of replica-count elasticity: the unit can be one replica
while the barrier still requires every replica. Its one real benefit is a smaller quota-deadlock
window. Its costs are a degraded steady state nothing reports — prefill at 3 and decode at 1 can
run indefinitely with decode as the bottleneck — a `Ready` that means a fraction of the declared
capacity, and the loss of the infeasibility park, which only fires while a deployment is still
held. If this is wanted later, the shape to add is a per-role minimum serving count, of which
"all" and "at least one" are the two extremes, rather than a two-valued switch.

### Expose the choice as a per-role gang switch

Rejected. Pairing "gang or at-least-one" with "supports replica changes" would write a causal link
into the API that does not exist — the admission unit and the barrier's threshold are independent.
The choice it presents also depends on cluster capacity at a given moment rather than on any
property of a model deployment, so the field would be answered by copying whatever the default is.

## Known Gaps

### ✅ A replica deleted by anything other than this operator held its ordinal forever

**Status: FIXED.** `releaseModelDeploymentStrandedWorkloads` in the convergence loop.

This operator deletes a replica's Workload on four paths it drives itself — teardown, a role
disappearing, a scale-down, and a rollout replacement. The convergence loop skips every Pod carrying
a `DeletionTimestamp`. So a replica removed by anything else — `kubectl delete pod`, a node drain, a
kubelet eviction — left its Workload behind. A serving group releases the Kueue finalizer only when
its Workload is deleted, so that Pod sat terminating behind the finalizer with its Workload still
admitted: the ordinal stayed occupied, its quota stayed held, and **no replacement was ever
created**. It took a human deleting that Workload to recover.

Before this spec the same action was survivable: a role's replicas shared one Workload, so deleting
one Pod left the Workload standing on its siblings and the loop created a replacement.

⚠️ **The unit tests did not catch it, and the reason is instructive.** A fake client has no Kueue,
so a deleted Pod carries no finalizer and simply vanishes — the ordinal reads empty and the
replacement appears. `TestModelDeployment_HandDeletedReplicaIsRecreatedWithoutARebuild` passes
against the defect. It was measured on a cluster instead.

#### What the remedy had to cover, and why only one candidate does

✅ **MEASURED**, four controlled groups: `kubectl delete pod` and the Eviction API a node drain calls
are **identical field for field**. The variable that matters is not how the Pod was removed but
**what its container does with SIGTERM**, and the two answers behave oppositely once a replacement
appears — the divergence AC3.3 carries. Relaxing the create gate to step around a departing member
fixes the `Failed` half and makes the `Succeeded` half loop forever, creating replacements Kueue
deletes. **Deleting the Workload is the only remedy covering both**, and it released both groups
immediately.

#### The divergence is a Kueue defect, fixed upstream in v0.18.7

⚠️ **The `Succeeded` half is upstream's bug, and upstream's own word for it is deadlock.** Kueue
counts `Succeeded` as active because a BATCH group needs that for its "all succeeded" accounting; a
serving group has no such accounting, and `isServing()` never reached the member-liveness test. Two
judgements about one group contradicted each other — `Finished()` says a serving group never
finishes, while `isPodRunnableOrSucceeded()` says its finished member still counts.

Kueue PR #14795 (issue #13830) scopes that test to serving groups and ships **with no feature gate**
from **v0.18.7**. ✅ **MEASURED on both versions, same cluster:** under v0.18.4 a replacement created
beside a `Succeeded` member is the one Kueue deletes; under **v0.18.9 the dead member is finalized
and the replacement survives**, its Workload never re-queued. The `FinalizeTerminatingPodGroups`
gate that v0.18.9 adds is Alpha and default-off, but it guards a different change (#15154), not this
one.

⚠️ **The upgrade alone does not close the gap**, which is why the remedy above is written to stand on
both versions. With no replacement in flight a lone inactive member is still not over `Count: 1`, so
Kueue finalizes nothing and `active` stays at zero: the loop that has to break is the create gate's,
and only this operator can break it. What the upgrade buys is the freedom to break it the other way
— relaxing the create gate becomes viable at v0.18.7+, where it is not at v0.18.4. That freedom is
deliberately not taken, because the remedy that works on both is worth more than one admission
round-trip.

#### It was a condition written too wide, not a contract change

⛔ This was first recorded here as a contract change, on the grounds that three cases stated the
opposite rule. That reading was wrong, and the assertion's own words carry the correction:
`TestModelDeployment_ADepartingReplicaIsNotReplacedUntilItsOrdinalReadsEmpty` says *nothing of this
deployment's own deletes it **while its members stand***. The limiting clause is the rule. Those
three fixtures pool every Pod under one Workload, so a member is always still standing in them; the
first attempt deleted unconditionally, reached a Workload with three live members, and turned them
red for the right reason.

The condition is therefore **delete a Workload that owns no Pod still standing**. All three cases
pass unchanged. Two new ones carry the two halves, and the mutation matrix shows each has a red of
its own:

| Mutation | `...GetsItsWorkloadReleased` | `...SurvivesADeparture` | The three above |
|---|---|---|---|
| none | pass | pass | pass |
| guard always skips | **FAIL** | pass | pass |
| guard never skips | pass | **FAIL** | **FAIL** (two of them) |

⚠️ **The UIDs in both new cases are stamped by hand**, and without that neither can fail: a fake
client assigns none, and an ownerReference matches on one — so a departing member and a standing one
are the same Pod to every reader of a Workload's owners. The three older cases pass for that reason
rather than on the merits, which is what the second new case exists to correct.

#### Still open

An **evicted** Pod may be a separate shape: it can reach `Failed` with no `DeletionTimestamp`, and
the loop then reads it as live and neither deletes nor rolls it. The remedy above keys on
`DeletionTimestamp`, so it does not reach that state. Not measured.

### ✅ Two admission rules outlived the reasons they were written for

Narrowing the unit emptied the justification under two webhook rules without changing what either
refuses. Both were kept and re-argued rather than deleted: deleting a rule requires showing the
absence is safe, and in both cases the shape is still worth refusing.

**The ten-role cap** was Kueue's: `Workload.spec.podSets` maxItems, binding while every role was one
PodSet of one shared Workload. Each replica now carries a Workload of a single PodSet, so **no Kueue
bound constrains how many roles a deployment declares**. The cap is restated as this project's own
product bound, and the refusal no longer sends a reader to check a number against the running
cluster. Raising it is a product decision now.

**The zero-accelerator refusal** was written against a Kueue failure mode that can no longer occur:
a scheduler writing fewer assignments than the Workload has PodSets, the API refusing the update,
and the Workload requeueing forever at roughly a hundred cycles a second. A single-PodSet Workload
cannot produce it. ✅ **MEASURED** what the shape does instead, on a cluster carrying the operator's
own Kueue configuration (`resources.quotaCheckStrategy: IgnoreUndeclared` plus the
`QuotaCheckStrategy` gate the chart enables): the replica's Workload **reserves, reports `Admitted`,
its Pod runs, and the queue's usage of those credits stays at zero**. Nothing errors.
⚠️ **The failure got quieter, not smaller** — it went from a parked deployment to a replica holding
accelerators nobody charged it for, competing with siblings on the same type that are charged. That
is the reason now written beside the rule.

⚠️ **Both reasons were found by scanning, not by a test.** Every rule of this webhook whose comment
named a PodSet, a shared Workload or a group was re-read against the per-replica shape; nothing
failed, and nothing would have. A rule whose reason has gone false still passes every test it has.

## Open Questions

The first four are decided. They are kept here rather than deleted because each rests on a reading
the next reader would otherwise have to re-derive; the decision itself is stated in the feature and
the task that carry it.

1. ✅ **Decided — how a replica's create becomes idempotent.** Keep `GenerateName`, write the
   replica's ordinal into the create request as an annotation, and read that ordinal through
   `APIReader` before creating. Stated in Goal 3, specified in F3, built in T7.
   The option it replaces was a deterministic name, and that option does not fail — it closes the
   same window. It was set aside because it makes every replacement wait for the departing Pod's
   *name* to be released rather than only for its slot to be free, and because it forecloses the
   LeaderWorkerSet-style surge onto a higher ordinal, which is the shape a future surge policy would
   take. Nothing is paid for keeping `GenerateName`: the free rollout admission that appeared to
   depend on it does not survive `Count: 1` under any naming scheme — see Risks.
   ✅ **MEASURED, and the reading underneath that last sentence was half wrong.** It said a
   `RestartPolicy: Always` Pod held by the Kueue finalizer never reaches an inactive phase on its
   own. It can: a container killed past its grace period lands in `Failed`, which *is* inactive.
   What does not follow is the conclusion that was hedged on it — a lone inactive member is still
   not over `Count: 1`, so it is never finalized, and the replacement path ⛔ cannot stop deleting
   the Workload. AC3.3 carries the full divergence between the two terminal phases, and Known Gaps
   carries what it decided.
2. ✅ **Decided — what carries a replica's ordinal.** An explicit **label**, not an annotation and
   not this operator's resource note. Two requirements have to be met at once: the ordinal must be
   *readable* off a Pod, which any of the three would satisfy, and it must be *selectable* by the
   API server, which only a label does — AC3.2's check is a server-side read scoped to one ordinal,
   and this operator's resource notes are annotations (`pkg/systemmeta/resource.go:46-58`). The
   group-name label is the only other per-replica carrier and is rejected for a different reason:
   parsing an ordinal back out of it is the same defect as parsing one out of a name, and AC2.3
   allows that name to fall back to a hashed form. Stated in F3 and in Boundaries.
3. ✅ **Decided — the instance-size field ships now, validated to `1`.** The two numbers are
   separated in the API before anything can conflate them. The cost is a field with a single legal
   value; what buys it is AC4.4 — `resources.accelerator` being per-Pod rather than per-replica is
   a documentation change that is only unambiguous once the second number exists. Specified in F4,
   built in T4. The alternatives set aside were re-documenting `replicas` alone and adding the
   field later, and implementing multi-Pod instances in the same change — the second is a Non-Goal.
4. ✅ **Decided — the API field is named `size`; its Go identifier is `ReplicaSize`.** The API name
   matches LeaderWorkerSet's own field exactly, which makes a later substrate change a
   zero-translation mapping. `replicaSize` as the API name would read more self-describing within
   this API, and that was not worth losing the correspondence.
   ⚠️ **The two names differ because they have to.** gogo protobuf generates a `Size() (n int)`
   method on every message type — `api/worker/v1alpha1/generated.pb.go:8029` for this one — and Go
   forbids a field and a method on the same type sharing a name, so a Go field named `Size` does
   not compile. The split costs nothing that OQ4 was protecting: the mapping to a LeaderWorkerSet is
   between CRD fields, and a Go identifier does not appear in it. The tag is
   `json:"size,omitempty" protobuf:"varint,15,opt,name=size"`, and the acceptance evidence for it
   is the **generated CRD**, not the Go struct — reading the struct is what would have missed this
   in the first place.
   **The Go identifier is `ReplicaSize` rather than `InstanceSize` because `InstanceType` is the
   very next field of the same struct and means something unrelated** — the accelerator pool the
   role is admitted against, the name the queue entrance is derived from. Two adjacent identifiers
   sharing a prefix while naming unrelated things is a misreading waiting to happen, and the
   ambiguity exists only in the Go struct: in the API the two are `size` and `instanceType`, which
   share nothing. `ReplicaSize` pairs with `Replicas`, the field it qualifies. This does not
   reopen the paragraph above — what that reasoning protects is the API name, and the API name is
   untouched.
5. **Revision affinity between roles during a rollout.** A request's prefill and decode may need to
   land on replicas built from the same revision, because the KV cache layout and the transfer
   protocol can both move between versions. The role Services select on identity labels only and
   carry no revision or spec-hash, so old and new replicas sit behind one Service during a rollout.
   Whether this is already handled on the router side has not been established, and it bears
   directly on whether a surge-based rollout is ever safe here. It needs its own investigation
   before F3's ordering rules are finalized.
   If affinity turns out to be required, the mechanism is a spec-hash label on the Pod that a
   Service selector can match. This is the one capability the rejected per-revision intermediate
   object would have supplied for free, and it costs one label rather than one new kind.
6. **`assignedFlavor` when a role's replicas disagree.** AC7.3 requires the field to state what it
   means; whether that is a per-replica list, a single value reported only when unanimous, or an
   explicit "mixed" marker is not decided.
7. **Upgrade communication.** Every existing deployment rebuilds once when this lands. Whether that
   warrants anything beyond a release note — a pre-flight warning, a documented maintenance window —
   is not decided.
