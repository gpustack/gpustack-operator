# Spec: Role Replica Admission Unit

Status: Built
Blocked on: nothing. All seventeen tasks are delivered, the unit suite and both lint targets are
green, and the multi-Member shape is measured on a real cluster rather than argued — case 79 passes
on all twelve rows and the regression set beside it is green. What is left is the pull request.
The first half — one replica as the admission unit — is built and has been run end to end on a live
two-node cluster: cases 1, 45, 49, 50, 51, 61 and 68 all execute, and every failure that round
produced was in the suite rather than in the operator (assertions still written for one Workload per
role, three waits whose predicate was expanded before the wait began, and one status read taken a
reconcile too early). Those are fixed. What is owed is the second half: a role's replica may still
only be one Pod, and the field that says otherwise is validated to `1`.
Type: Feature

## Summary

A `ModelDeployment` role today admits all of its replicas as one Kueue pod group, so changing
`spec.roles[].replicas` — in either direction, including **growing** it — deletes and recreates
every running replica of that role. Each one reloads its model weights and the role passes through
a window with no serving capacity at all. This spec narrows the admission unit from *a role* to
*one replica*, so a replica count change adds or removes exactly the replicas it names and leaves
every other one running. It also splits the one number a role carries today into the two it has
always meant — **how many independent serving instances** and **how large one instance is**.

The second number is then made to work. A role's replica may be **several fate-sharing Pods** —
which is what tensor, pipeline, expert or sequence parallelism across hosts requires — rendered as
Pods this operator names itself, so that member `-m0` is durably the leader and every member has a
stable address to find the others by. The two numbers answer different questions and are edited on
different terms: **`replicas` is the horizontal one** and changing it disturbs nothing already
running, **`size` is the shape of one instance** and is frozen after creation. Admission stays
Kueue's: one pod group per replica, `size` members in it, and the existing joint barrier gates the
whole deployment as a set. This is deliberately the capability a LeaderWorkerSet provides, obtained
without adding LeaderWorkerSet as a CRD, controller and chart dependency.

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
6. **A replica may be several Pods that share a fate.** `size: n` renders one replica as `n` Pods
   that are admitted together, become ready together and are replaced together, with a durable
   leader at ordinal `-0` and an address every member can reach its peers on. This is what a model
   too large for one host needs — tensor, pipeline, expert or sequence parallelism all place ranks
   on separate hosts and require the ranks to find each other before any of them serves.
7. **The two numbers are edited on different terms, and the API enforces the difference.**
   `replicas` is the horizontal control and may be changed at any time, disturbing nothing already
   running. `size` is the shape of one instance: it is **frozen after creation**, because every
   member of a running instance was built for the rank layout the old value described. Refusing the
   edit is better than performing it as a mass replacement — the user who wanted a different shape
   wanted a different deployment, and the one who typed it by accident gets told rather than
   charged for a full rebuild.

### Non-Goals

1. **Adopting LeaderWorkerSet.** The capability is in scope; the dependency is not. LWS would
   supply stable names, a leader ordinal and a rolling update, but as a CRD, a controller, a chart
   subchart and a Go module — and its Kueue integration would then own admission, which is where
   the value of this spec lives. Naming the Members and giving them a headless Service supplies the
   same three properties from `core/v1` alone. ⭐ LWS is itself the evidence that the substrate
   buys less than it appears to: it is built on StatefulSets and **still** carries its own Pod
   controller and its own revision bookkeeping, because neither a StatefulSet's replacement nor its
   rolling update survives contact with a Kueue pod group. Alternatives records what is given up:
   LWS's `maxSurge`, its two-template leader/worker split, and its restart-on-member-failure policy.
2. **Driving an engine's parallelism arguments.** The operator renders what a multi-Pod instance
   needs from Kubernetes — members, ordinals, a leader address, a joint admission — and passes the
   rank layout to the container as environment. It does not compose `--tensor-parallel-size` or its
   siblings: the degrees do not decompose from `size` alone (under pipeline parallelism the member
   count is a product of two degrees), and a formula missing an input is worse than no formula
   because it passes. Declaring the degrees as fields is separate work, tracked upstream of this
   spec at <https://github.com/gpustack/gpustack-operator/issues/203>.
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

A deployment admits through Kueue as it does today. What changes is the size of the group each Pod
joins, the fact that each replica carries an ordinal identifying it, the unit the joint-admission
barrier counts, and — for a role whose `size` is above one — the object that carries a replica's
Pods.

### Vocabulary

Four levels, and each has exactly one name. The rest of this spec uses these words and no synonyms.

| Level | Name | What it is | Carried by | Kueue sees |
|---|---|---|---|---|
| 1 | **Deployment** | one `ModelDeployment` | the CR | a set the barrier gates as a whole |
| 2 | **Role** | one entry of `spec.roles[]` — `prefill`, `decode` | nothing of its own | a PodSet name, repeated |
| 3 | **ReplicaGroup** | one independent serving instance; a Role has `replicas` of them | `size` Pods this operator names, plus a headless Service when `size > 1` | **one pod group = one Workload** |
| 4 | **Member** | one Pod inside a ReplicaGroup; a ReplicaGroup has `size` of them | a Pod | one member of that pod group |

Three consequences are worth stating because each is a place a reader could reasonably assume
otherwise:

- **A ReplicaGroup is the admission unit at every size.** `size: 1` and `size: 4` differ in how many
  Members the group declares, not in what a group is. The `pod-group-total-count` annotation carries
  `size`; it carried `1` in the first half of this spec because every group had one Member.
- **`PodGroup` is deliberately not a level here.** Kueue already uses *pod group* for what level 3
  is, and giving the word a second meaning inside the one document that has to reason about Kueue's
  admission is how a sentence ends up true under one reading and false under the other.
- **The leader is a Member, not a level.** It is the Member at member index `0`, which this
  operator assigns by naming it — the name is derived, not elected, so it is fixed before the Pod
  exists and no reader has to wait for a decision. A ReplicaGroup of `size: 1` has a leader too,
  and it is the only Member.

The API keeps its two words — `replicas` counts ReplicaGroups, `size` counts Members — because they
are the user's controls and renaming a field to match an internal vocabulary is a cost paid by
everyone who writes the YAML. `size` also matches LeaderWorkerSet's own field exactly, which keeps
the mapping to that substrate a rename-free one should it ever be taken.

### User Stories

#### Story 1

As an operator running a disaggregated deployment, I want to grow `prefill` from 2 replicas to 3,
so that I add capacity without the two already-serving prefillers reloading their weights and
dropping their caches.

#### Story 2

As an operator reducing cost, I want to shrink `decode` from 4 replicas to 2, so that exactly two
replicas drain and the other two keep serving uninterrupted.

#### Story 3

As an operator removing a role from a disaggregated deployment, I want the roles I keep left alone,
so that retiring one half does not take the other half offline.

#### Story 4

As an operator watching a deployment start on a busy cluster, I want to see how many of a role's
replicas have been admitted, so that "waiting for capacity" is distinguishable from "stuck".

#### Story 5

As a platform engineer, I want a role that runs one model instance across several hosts to declare
that shape in its own field, so that the instance size and the instance count are never confused —
and so that changing the safe one stays safe.

#### Story 6

As a platform engineer deploying a model too large for one host, I want one replica to be several
Pods that are admitted together and can address each other, so that I can run tensor or pipeline
parallelism across hosts without giving up the admission guarantee that the whole deployment
starts or none of it does.

#### Story 7

As an operator whose decode capacity is short, I want to add a second decode instance by changing
`replicas` from 1 to 2, so that I get another entire multi-Pod instance beside the one already
serving — and the one already serving is not touched, even though the thing being added is several
Pods rather than one.

#### Story 8

As an operator who typed the wrong instance size, I want the API to refuse the edit rather than
perform it, so that I find out by being told instead of by watching every instance of the role be
torn down and rebuilt.

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
- **AC1.6** Removing a role leaves every surviving role's Pods as the same objects, and the
  deployment never passes through a state where one role's Pods sit in two different groups.
  **The role set is identity**, so removal is the only direction the API offers: a stored deployment
  cannot gain a role, and an operator who wants one creates a second deployment. The mechanism
  underneath is what makes the survivor safe — a role's group names are derived per `(role,
  ordinal)` and never from how many roles exist, so no sibling's departure can rename them.

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
- **AC4.3** That field is documented as fate-sharing: the Members of one ReplicaGroup start
  together, are admitted together and are replaced together.
- **AC4.3b** The field is **immutable after creation**, and the refusal says so. Changing it is not
  a scale — every Member of a running instance was built for the rank layout the old value
  described, so the only honest implementation is to replace every ReplicaGroup of the role, which
  is a new deployment wearing the name of the old one. Refusing costs the user who meant it one
  `kubectl delete`; performing it costs the user who did not mean it every instance they had.
  ⚠️ This REPLACES the earlier statement that changing the field replaces every instance of the
  role. That statement described the field while nothing rendered it; it is not the contract.
- **AC4.4** `spec.roles[].resources.accelerator` is re-documented as what **one Pod** asks for,
  not what one replica asks for. At instance size 1 the two readings coincide; at any larger size
  they do not, and the field is per-Pod.
- **AC4.5** An instance size above `1` is **accepted and rendered**, per F8. The refusal that stood
  in for the missing render path is deleted rather than reworded, and the e2e row asserting it
  becomes an acceptance — a deleted rule still needs something reporting the day it comes back.

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

#### F8 — A ReplicaGroup above one Member is `size` named Pods and a headless Service

The render path this spec used to defer as a Non-Goal, now in scope. **No workload controller sits
between this operator and the Members**: it names them, creates them and replaces them, exactly as
it already does for the single-Member groups of the first half. Alternatives records why the two
substrates that look like they would carry this — one StatefulSet per Role, and one per
ReplicaGroup — do not, and both reasons are measured rather than argued.

- **AC8.1** A role with `size: n > 1` renders **n Pods per ReplicaGroup**, named
  `<deployment>-<role>-r<replica>-m<member>`. A role with `size: 1` renders exactly the Pod it
  renders today, byte for byte. The two paths are one rendering with `n = 1` as its ordinary case,
  not two admission models: a ReplicaGroup is one pod group and one Workload either way.
- **AC8.2** **The Members are named rather than generated**, and that is what the shape is bought
  for: a rank cannot join a collective it cannot address, and a Pod created by `GenerateName` has
  no name anything can predict. The name is a pure function of (deployment, role, replica, member),
  so every reader — the renderer, the converger, a sibling Member's entrypoint — derives the same
  string without reading anything.
- **AC8.2b** ⚠️ **Naming the Members spends a length budget, and admission is where that is
  refused.** A Member's name is its hostname, so it is a DNS-1123 label of 63 characters, while the
  rule that already exists measures only `<deployment>-<role>` — a 61-character pair is legal there
  and implies a 67-character Member name. Without a rule the deployment is admitted, every Pod
  create is rejected, and the reconciler retries forever with the cause two objects away from the
  field that caused it. The rule measures the **longest name the declared counts can currently
  produce** rather than a worst case over `int32`, and it runs on **update as well as create**,
  because `replicas` is a field a user is invited to change and a scale can lengthen every name.
- **AC8.3** Every Member of a ReplicaGroup is created in one pass, never one-after-another. A
  Member that Kueue has scheduling-gated is never Ready until the group is admitted, and the group
  is not admitted until every Member exists — so any rendering that waits for Member *i* before
  creating *i+1* deadlocks. Both halves of that circle were measured, not reasoned: see F8's
  premise, measured.
- **AC8.4** A **headless Service per ReplicaGroup**, plus `hostname` and `subdomain` on every
  Member, is what turns those names into DNS. ⭐ This is `core/v1` behaviour and owes nothing to
  any controller: measured on a live cluster, two ordinary Pods carrying `hostname`/`subdomain`
  behind a headless Service resolved each other by name with no StatefulSet anywhere.
- **AC8.5** Every Member of a ReplicaGroup carries the **same** `pod-group-name` and a
  `pod-group-total-count` of `size`, so Kueue composes **one Workload with one PodSet of `size`** —
  the role's name, per F1. The joint barrier keeps counting ReplicaGroups, so F5 is unchanged: what
  moves is how many Members a group declares, not how many groups a deployment has.
- **AC8.6** The rank layout reaches the container as **environment**: the leader's address, the
  group size, and this Member's own index. The first two are **constants of the ReplicaGroup** —
  `<deployment>-<role>-r<replica>-m0.<headless-service>` and `size`. ⭐ The third is read through
  the **downward API from a label**, not written as a literal: the template declares
  `fieldRef: metadata.labels['<member-index-label>']` and the per-Member stamp writes that label.
  The declaration is therefore identical in every Member's Pod while the value differs, which is
  what keeps one template per ReplicaGroup and keeps the fingerprint comparable; see Boundaries.
  ⛔ The operator composes **no** engine parallelism argument (Non-Goal 2): it publishes the facts
  an entrypoint or an engine needs to compose its own.
- **AC8.7** The role's **Service selects only the leader**. Every Member carries the role's identity
  labels, so a selector written for `size: 1` fronts all `size` Members — and every Member but the
  leader serves no API at all, making a plain round-robin fail for `(size-1)/size` of requests.
- **AC8.8** Deleting one ReplicaGroup — its Members, its headless Service and its Workload — does
  not disturb any other ReplicaGroup of the role. This is Goal 1 at the new size, and it is the row
  the e2e suite has to carry because nothing below e2e has a ClusterQueue to be charged against.
- **AC8.9** **Kueue's pod integration composes the group, and the shape it composes is measured.**
  One Workload named for the `pod-group-name` the Members share, owned by **the Member Pods**, with
  **one PodSet named for the role-hash** and a count of `size`. The fast-admission annotation is
  **absent**, which this spec's Boundaries name as a **Never**. Kueue's `statefulset` and
  `deployment` integrations never participate, for the simplest possible reason: this operator
  creates no such object. The measurement, and the control that gives it meaning, are recorded
  below.
- **AC8.10** ⭐ **Replacing any one Member replaces the whole ReplicaGroup, and this is Kueue's
  constraint rather than a choice this spec makes.** Measured: deleting one Member of an admitted
  group leaves it on the API server indefinitely — Kueue's finalizer holds it, phase reaches
  `Failed` — while the Workload stays `Admitted` and gains `WaitingForReplacementPods`. Deleting
  that Workload is the only release, and doing so makes Kueue stop **every surviving Member** of
  the group (its own event says `Stopped: Workload is deleted`). So a rollout, a Member crash and a
  spec change all take the same path: the ReplicaGroup goes as a unit and comes back as a unit.
  This is F5's fate-sharing arriving as a mechanism instead of an intention.

#### F8's premise, measured

Hand-applied YAML on a cluster running this chart's Kueue, with no operator involvement: a headless
Service and a StatefulSet of two Pods whose **template** carries the queue-name label, the pod-group
name and a total count of two, against a control differing in exactly one line — the same queue-name
label **on the StatefulSet object** as well.

| Reading | Queue-name on the Members only | Control: also on the object |
| --- | --- | --- |
| Workload name | the pod group's own name | `statefulset-<name>-<hash>` |
| Workload owner | the two **Pods** | the **StatefulSet** |
| PodSet | one named for the role-hash, count 2 | one named `main`, count from `spec.replicas` |
| Fast-admission annotation | **absent** | — |
| Role-hash annotation | **survives** | — |
| What the StatefulSet webhook added to the template | **nothing** | `pod-suspending-parent: statefulset` |

The control is what makes the left column mean anything: it produced the StatefulSet-integration
shape, so "the integration did not claim it" is not "the integration is not running". ⭐ Read the
left column again with F8's shape in mind: **Kueue never saw the StatefulSet at all** — it saw two
Pods carrying the right metadata and composed exactly the group F8 wants. That is what made the
substrate look optional, and the third reading below is what settled it.

Three further readings came out of the same cluster, and each one moved a decision:

- **⭐ A pod group's Workload does not exist until every declared Member does.** With one Member of
  a declared two, there was no Workload at all and the Member sat gated. Adding the second — by
  `spec.replicas`, leaving the template untouched — assembled the Workload, admitted it, and
  **ungated both Members together**, inside one four-second sampling interval. This is AC8.3's
  argument as an observation rather than a deduction: any creation order that waits for Member *i*
  to be Ready before creating *i+1* cannot terminate, because Readiness is downstream of an
  admission that is downstream of *i+1* existing.
- **⚠️ Editing the Pod template of a group that is not yet admitted breaks that ReplicaGroup.** A
  request change applied in place left a Member `Failed` while still gated, nothing replaced it,
  and the Workload settled on `WaitingForReplacementPods`. AC8.10 is why the shape this spec builds
  never does that.
- **⭐⭐ A deleted Member of an admitted group does not go, and nothing replaces it.** This is the
  reading that chose the substrate. Deleting one Member of a two-Member admitted group: it stayed
  on the API server with Kueue's finalizer and a `deletionTimestamp`, phase walking to `Failed`,
  for as long as it was watched; the Workload stayed `Admitted` and gained
  `WaitingForReplacementPods`; **and the StatefulSet never issued a second `SuccessfulCreate`** —
  it cannot, because the name is still taken. Deleting the Workload released it, and Kueue then
  stopped the **surviving** Member too, saying so in an event: `Stopped: Workload is deleted`. The
  whole group came back with new UIDs about half a minute later.

  ⇒ Both of the things a StatefulSet would have been carried for — replacing a lost Member, and
  rolling Members one at a time — are **inert** inside a Kueue serving group. What is left once
  they are removed is naming, and naming is a few lines of rendering. AC8.10 states this as a rule;
  Alternatives records it as the adjudication.

### Notes / Constraints / Caveats

**Kueue version.** The cluster runs the chart pinned in `hack/deps.sh`, **Kueue v0.18.9**. The
`sigs.k8s.io/kueue` module in `go.mod` is `v0.17.1` and is the compile-time library only.

Every capability and measurement below names the version it was read on, and those numbers are
provenance rather than a claim about the pin: a reading taken against **v0.18.4** stays labelled
v0.18.4. Where the two versions differ, Known Gaps says so, and the remedies stand on both.

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

**What the LeaderWorkerSet substrate looks like**, since Non-Goal 1 declines the dependency while
F8 takes the capability, and a later substrate change stays possible. It is
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
  ⭐ **This SURVIVES multi-Member ReplicaGroups, and the reason is worth stating because the
  opposite is the obvious assumption.** Members differ by index and a rank layout, which looks
  like output that must vary per Member — but everything that varies is carried by **metadata the
  stamp writes and the template merely points at**: the member index is a label, reached from the
  template through `fieldRef`, so the container spec is identical in every Member while the value
  differs; and the leader's address is a **constant of the ReplicaGroup**, not a per-Member value.
  So the renderer still emits one template per ReplicaGroup and the fingerprint stays comparable.
  ⛔ A design that needs the renderer to emit
  per-Member argv has left this boundary, and that is the signal to stop rather than a detail to
  work around — a fingerprint that differs per Member makes every Member read as a pending rollout.
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
of them serves alone. **Any value of 1 or more is accepted**, and F8 renders it: above one, a
ReplicaGroup is `size` named Members behind a headless Service, with member `m0` the leader.

⚠️ **It cannot be changed after creation** (AC4.3b). The Pods a running instance is made of are not
the Pods a different size asks for, so there is no edit that does not replace every instance of the
role at once — and per AC8.10 that is true of the substrate too, not just of this operator's
preference. Scaling is `replicas`, which disturbs nothing already running.

- **Use it for** the multi-Pod instance shapes this API exists for: tensor, pipeline, expert or
  sequence parallelism spread across hosts. ⛔ The operator publishes the rank layout and composes
  none of the engine's arguments from it (Non-Goal 2).
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

- [x] **T10 · PoC: measure what Kueue does with a multi-Member pod group, and what a workload
      controller does or does not add on top** — everything after it is shaped by the answer.
      Blocked by: None
      Owns: `<no product files>`
      Gate: review
      **Answered, and the answer changed F8's substrate.** The run did what it was written to do:
      a StatefulSet whose Pod template alone carried the Kueue metadata was **not** claimed by
      Kueue's `statefulset` integration, against a control — the same object with the queue-name
      label on it as well — that was. But the same readings showed Kueue composing the group from
      **the Pods**, with the StatefulSet nowhere in the Workload; and a follow-up measured that a
      deleted Member of an admitted group is held by Kueue's finalizer, so the StatefulSet can
      never replace it, and that releasing it stops the whole group. ⇒ The two capabilities the
      substrate was for are inert here, so F8 now renders Members directly. Everything is recorded
      in *F8's premise, measured*; AC8.3, AC8.4, AC8.9 and AC8.10 rest on it.
      ⚠️ The task as originally written also named a fallback — dropping `statefulset` from the
      chart's framework list if the integration claimed the object either way. It was never needed
      and the framework list is untouched.
      Verify: manual, readings recorded in this spec beside F8.

- [x] **T11 · `size` becomes immutable, and above one becomes legal**
      Blocked by: T10
      Owns: `pkg/worker/webhooks/worker/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`
      Acceptance: `validateModelDeploymentRoleSize` is deleted. An update changing
      `spec.roles[].size` on an existing deployment is refused, naming the field and saying that an
      instance's shape is fixed at creation; an update changing `replicas` on the same object is
      accepted in the same test. Creation with any `size >= 1` is accepted. The immutability rule
      sits beside the existing frozen-field rules rather than in a new pass.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run 'TestModelDeployment' -v`
      Done: the rule sits with the other frozen fields and carries its own message rather than the
      identity one, because the reason differs — the deployment is the same one, and what cannot
      happen is this edit to it. The refusal names `replicas` so it is not a dead end. The test was
      mutation-checked: with the rule removed it fails on the assertion, not on the build.

- [x] **T12 · A ReplicaGroup above one Member renders as `size` named Members**
      Blocked by: T10, T11
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_pod_group.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Acceptance: A role with `size: 1` renders exactly the Pod it renders today — asserted by
      comparison against the current output, so the common path is provably untouched. A role with
      `size: n > 1` renders n Pods per ReplicaGroup, each **named**
      `<deployment>-<role>-r<replica>-m<member>` with `hostname` set to that name and `subdomain`
      to the ReplicaGroup's headless Service, carrying the group metadata of AC8.5 and the rank
      environment of AC8.6 — leader address and size as literals, member index through a
      `fieldRef` on a label the stamp writes. A test asserts the **container spec is identical**
      across the Members of a group and across two groups of one role, which is the Boundaries
      invariant at the new size, and that the member-index label is the only thing separating two
      Members. `pod-group-total-count` becomes `size` instead of the constant `1`.
      The three rank variables are owned on every engine, because a user entry of one of those names
      would be merged by value onto the entry carrying the `fieldRef`, and an `EnvVar` holding both
      a value and a source is refused by the API server. The case proving the index asserts it **as
      a `fieldRef` and never as a resolved value**: a literal index would satisfy "the container
      knows its rank" while making the container spec a per-Member document.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'Test(Render|Stamp)ModelDeployment' -v`

- [x] **T13 · The converger creates, compares and replaces at ReplicaGroup granularity**
      Blocked by: T12
      Owns: `pkg/worker/controllers/worker/model_deployment.go`, and its tests
      ⚠️ **This task exists because the plan did not have it.** The converger — roughly five hundred
      lines keyed on one Pod per (role, ordinal) — is what actually creates and deletes replicas,
      and no earlier task named it. Every other task in this half depends on it.
      Acceptance: the desired set becomes one entry per ReplicaGroup holding `size` Members, and
      every existing per-ordinal decision (surplus shedding, the create gate that waits for a
      departing Pod's absence, the delete that takes the Workload with it) applies to the group as
      a whole. **A ReplicaGroup is created, compared and deleted as a unit** (AC8.10): no path
      deletes one Member and leaves the others, because the measurement shows that state is not
      recoverable without deleting the Workload anyway. At `size: 1` the decisions are identical to
      today's, asserted by keeping the existing tests unchanged and green.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeploymentReconciler' -v`

- [x] **T14 · Addressability: a headless Service per ReplicaGroup, and a role Service that fronts
      only leaders**
      Blocked by: T12
      Owns: `pkg/worker/controllers/worker/model_deployment_service.go`,
      `pkg/worker/controllers/worker/model_deployment_service_test.go`
      Acceptance: Each ReplicaGroup of a `size > 1` role owns a headless Service
      (`clusterIP: None`) selecting its own Members, created and deleted with that ReplicaGroup.
      The role's own Service gains a selector term that matches only the leader Member, and a test
      asserts a three-Member ReplicaGroup puts exactly one endpoint behind it. At `size: 1` the
      role Service's selector is unchanged, asserted by comparison. ⚠️ The leader must be
      identifiable by a **label** for a selector to match it: the ordinal label F3 already writes
      is per-ReplicaGroup, so this needs the per-Member one T12 adds.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeploymentService' -v`

- [x] **T15 · Readiness, rollout and teardown at ReplicaGroup granularity**
      Blocked by: T13
      Owns: `pkg/worker/controllers/worker/model_deployment_status.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout.go`, and their tests
      Acceptance: A ReplicaGroup counts as ready only when **every** Member is ready, so
      `status.roles[].ready` counts instances and not Pods. The rollout replaces whole
      ReplicaGroups, never individual Members, keeping the one-replica-per-role-per-pass cadence —
      which AC8.10 makes the only workable cadence rather than a stylistic one. Deleting a
      deployment deletes each ReplicaGroup's Members, headless Service and Workload, and the
      Workload-holds-finalizer cycle is broken exactly as it is for single-Member groups.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeployment(Status|Rollout)' -v`

- [x] **T16 · End-to-end: a multi-Member deployment, and a scale that leaves it alone**
      Blocked by: T12, T13, T14, T15
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-79.sh`,
      `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Acceptance: A new case deploys a role at `size: 2, replicas: 1`, and asserts: one Workload
      with one PodSet of **two**; the role's Service holding exactly one endpoint while the
      instance's own holds both; each Member resolving the leader's DNS name; and the rank layout
      reaching the container. It then scales `replicas` to 2 and asserts the first ReplicaGroup's
      Member UIDs are unchanged while a second ReplicaGroup appears — Story 7, and the row that only
      e2e can carry. Case 45 gains the `size` immutability refusal; its size-above-one refusal row
      becomes an acceptance.
      ⛔ **The gated window itself is deliberately NOT asserted**, and the case header carries the
      reason: both Members are scheduling-gated for a window seconds wide on an idle pool, so a poll
      that caught it would pass or fail on how busy the cluster was, and one that missed it would
      report a pass. What is asserted instead is the state that window exists to produce — one
      Workload whose single PodSet declares two — which Kueue can only have reached by waiting for
      both Members to exist. NOT closed by polling for a gated Pod, nor by reading a Member's
      scheduling gates after the fact, since an admitted Member has none left to read.
      Verify: `bash .agents/skills/gpustack-operator-e2e/cases/case-79.sh <NS>`
      ⓘ **Measured on a two-node cluster carrying this operator: all rows PASS**, the DNS row
      included — a Member resolves its leader at the name derived before either Pod exists. The
      regression set (45, 49, 50, 51, 61, 68, 78) runs beside it and is green. What that round
      found in the harness is recorded under the Test Plan.

- [x] **T17 · Documentation for the multi-Member shape**
      Blocked by: T12, T13, T14, T15
      Owns: `docs/reference/model-deployment.md`, `api/worker/v1alpha1/model_deployment.go`
      Acceptance: The reference page states the four-level vocabulary, what `size` costs to change
      (nothing, because it cannot be changed), that a ReplicaGroup is replaced as a unit and why
      (AC8.10), and that engine parallelism arguments remain the user's. ⚠️ The page's `##` budget
      is at nine of ten: the multi-Member content extends the existing sections rather than opening
      a new one.
      ⓘ The `size` field comment half of this is already done — T11 rewrote it to state
      immutability and what a multi-Pod instance publishes, since the rule and the field
      documentation describing it cannot correctly land in different commits.
      Verify: `make lint docs`

**Checkpoints.** After T4 the API is settled and the other tracks can assume it. After T6 the
system is coherent and every goal about replica-count elasticity holds. ⚠️ **It is not shippable
there**: T7 carries both create idempotence and the two changes that keep a rollout from walking a
deployment toward zero admitted replicas on a contended pool (see Risks). What T6 buys is a
reviewable, working intermediate state — not a release. After T9 the documented behavior matches
the built behavior.

⭐ **T9 ends the first half, and it is a shippable line on its own** — a role's replica count is
elastic and nothing about it depends on the second half existing. T10 opens the second: a replica
may be several Members. **T10 is a measurement, not an implementation**, and sequencing it first is
the decision in this plan that paid for itself: it was written to check one inferred premise, and
what it returned instead was that F8's whole substrate was optional. Discovered at T12 that would
have been discovered as most of a rewrite already spent.

T12 and T13 are the two wide ones — the render path and the converger — and T13 is where the
plan's own gap was: the converger belonged to no task until the implementation of T12 walked into
it. T14 and T15 have disjoint `Owns:` and run concurrently behind T13.

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
  marker the renderer never writes; exactly one Delete for a 2→1; removing a role from a two-role
  deployment leaves the survivor's Pods untouched and never places one role's Pods in two groups
  (the adding direction is refused at admission, see AC1.6, so it has no converger case and the
  shared mechanism is covered by the group-name derivation below instead); every rendered Pod
  declares total count 1 and carries its ordinal label; group names are
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
  ⚠️ **T11 then inverts that row**: the size-above-one refusal becomes an **acceptance**, because
  the rule is deleted rather than reworded, and a new refusal takes its place — an **update**
  changing `size`. A deleted rule still needs a row: without one, nothing reports the day it
  returns, and the suite's silence would read as a rule that was never there.
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

- **NEW CASE (T16)** — the multi-Member shape, which nothing below e2e can carry: a role at
  `size: 2, replicas: 1` composes **one** Workload with **one PodSet of two**; both Members stay
  gated until the group is admitted and are then admitted together; the role's Service holds
  exactly one endpoint; each Member resolves the leader's DNS name. Then `replicas` goes to 2 and
  the first ReplicaGroup's Member UIDs are unchanged while a second ReplicaGroup appears — Story 7
  at a size where a rebuild would be most expensive and least visible.

The PoC in T1 is not an e2e case: it runs before any code exists, against hand-applied YAML. Its
result is recorded in this spec rather than in the suite. **T10's PoC is the same kind** and is
recorded the same way, beside F8.

⭐ **What the first e2e round actually found, and why it belongs here rather than in a report.**
Every one of the six failures was in the suite, not in the operator: three assertions still written
for one Workload per role (case 50 read a single Workload's `podSetAssignments` where the shape is
now one PodSet per Workload; case 68 expected one group where two roles of one replica are two;
case 51 asserted a refusal this spec deletes), two instrument defects (three `wait_for` calls whose
predicate was expanded **before** the wait began, so the loop compared one stale sample every
round; one status read taken immediately after a Workload flipped, when the condition is written by
a different controller a reconcile later), and one stale comment in the operator claiming a rule
that no longer exists. ⚠️ **The suite's own instruments failing is the expected shape of this
round** — the assertions were written against the design this spec replaces, so a green first run
would have been the surprising outcome and the reason to distrust the suite.

⭐ **The second round ran against an operator built from this branch, and found the same shape
again.** Case 79 passes on all twelve rows and the regression set beside it is green, so the
operator is measured rather than argued. The three failures were all in the harness, and all three
were one defect: **a bash variable name that silently swallowed an assignment.**

Two of them made a check unable to fail or unable to pass, which is why neither had ever been seen.
`GROUPS` is a bash builtin array of the caller's unix groups and discards what is written to it, so
case 79 compared a group id against 1. `ROWS` was the results accumulator in case 49 and a scalar
assignment to it wrote element zero, erasing every row already recorded while `FAILS` kept its
count — a failure there would have printed a number and no row naming it.

The third is a race rather than a name: case 78 read the ClusterQueue off a Workload that existed
but was not yet admitted, so its two quota rows had never executed. They now do, and report the
charge moving by exactly one replica in each direction and returning in full.

⚠️ **None of the three is caught by anything.** `shellcheck` reports the first as SC2178 plus
SC2128, but this repository never runs it: `# shellcheck disable=` directives appear throughout
`hack/`, and no target invokes the tool. Every `.sh` under `.agents/skills/` — 132 of them — has
therefore only ever been checked by being executed. Wiring a shell lint in is its own change and
its own set of findings, so it is recorded here rather than done inside this one.

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

### Move to a StatefulSet per ReplicaGroup

⚠️ **This was F8's shape for one revision of this spec, and it is recorded here because the
reasoning that reached it was sound and still failed.** One StatefulSet per **ReplicaGroup** —
`spec.replicas: size`, one object per replica — really does remove the conditions the per-Role
objections need:

1. **The declared total moving is the defect this spec exists to remove** — and it moves because the
   count moves. Here the count is `size`, which AC4.3b makes immutable, and `replicas` changes add
   or delete whole StatefulSets while editing none.
2. **The structural obstacle disappears.** Each StatefulSet holds exactly one ReplicaGroup, so the
   group name is a **constant of that object's template** rather than a value varying by ordinal.
3. **Objections (2) and (3) need Kueue's StatefulSet integration to claim these objects**, and T10
   measured, against a control, that it does not when the queue-name label is on the Members alone.

All three hold. ⭐ **What they establish is that the shape is possible, and the question that was
never asked is what the object would then be doing.** T10's own readings answer it: the Workload
Kueue composed was owned by **the Pods**, with one PodSet named for the role-hash and a count of
two — the StatefulSet appears nowhere in it. So the substrate was carrying exactly two things:
replacing a Member that goes away, and rolling Members one at a time.

**Both were then measured to be inert inside a Kueue serving group**, which is what moved F8 off it:

- A deleted Member of an admitted group is held on the API server by Kueue's finalizer. The
  StatefulSet cannot replace it — the name is still taken — and never tried: no second
  `SuccessfulCreate`, for as long as it was watched.
- Releasing it means deleting the Workload, and doing that makes Kueue stop **every surviving
  Member** of the group (`Stopped: Workload is deleted`). So there is no such thing as rolling one
  Member; the group goes as a unit whatever the substrate believes.

⇒ What remained was stable names, and F8 obtains those by naming the Pods — `hostname` and
`subdomain` against a headless Service, measured to resolve with no controller present. The PVCs
the earlier rejection called unwanted stay unwanted and now stay impossible rather than merely
unset.

⭐ **LeaderWorkerSet is the corroboration.** It is built on StatefulSets and still carries its own
Pod controller, its own revision bookkeeping and its own `partition` arithmetic
(`pkg/controllers/pod_controller.go`, `rollingUpdateParameters`) — because it is working around the
same two facts from the other side. A design that adopts the substrate and then reimplements what
the substrate was for has paid for it twice.

⚠️ **What this costs, stated plainly.** A Member that crashes is replaced by this operator's own
convergence rather than by a controller in `kube-controller-manager`. That is a real transfer of
responsibility — it is the one thing the substrate would genuinely have done — and the mitigation
is that the convergence already exists and is watch-driven: the deployment `Owns` its Pods, so a
Member's disappearance is an event rather than a poll. And per AC8.10 the replacement is a whole
ReplicaGroup either way, which is work no StatefulSet was going to do.

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

It is declined rather than taken because it is a substrate change — a new CRD and controller
dependency, a new chart subchart, a new Go module — while the first thing it requires is exactly
what this spec does: move the admission unit to one replica and re-base the barrier on it.

⚠️ **The question it was once deferred to is now answered, and answered the other way.** The
earlier reading was that doing the admission work first would leave "adopt LWS" as a separate,
smaller decision; F8 instead obtains the three properties that decision was about — a stable
leader, addressable members, and a group that scales by whole groups — from `core/v1` alone, with
no new dependency of any kind. **What is given up is
real** and belongs here rather than in a footnote: LWS's `maxSurge` (a Non-Goal anyway), its
two-template leader/worker split (so a leader wanting different arguments to its workers must get
them from the rank environment rather than from a second template), and its policy for restarting a
whole group when one member fails (here that is the operator's own rollout path, at ReplicaGroup
granularity per T15 — and per AC8.10 there is no finer granularity available to anyone, LWS
included). Adopting LWS later remains an increment rather than a rewrite: `size` already
matches its field name, and a ReplicaGroup already maps to one of its groups.

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

### ✅ A ReplicaGroup short of one Member is replaced whole, and does not wait on the rollout guard

**Status: FIXED.** The incomplete-replica exception in the convergence loop's rollout.

A ReplicaGroup can lose one Member and keep the rest: a `Create` that fails after a sibling
succeeded, a node that takes one Pod, an eviction that reaches one of several. Three mechanisms meet
in that state, and two of them point the wrong way.

The create gate is owed nothing — an ordinal holding even one Member is occupied, so `missing` is
zero and no Member is created beside the survivors. That is correct on its own terms: a fresh Member
would join a group Kueue has already refused. The completeness check does condemn the replica, which
is also correct. What is left is the rollout, and it is guarded on every declared replica holding an
**admitted** Workload — a guard that exists to spend admitted capacity one replica at a time.

⛔ **An incomplete replica has no admitted capacity to spend, and cannot acquire any.** Kueue
composes no Workload at all for a group short of its declared total, so the replica contributes zero
to that count forever. Without the exception the role is stuck in both directions at once: the
broken instance is never replaced, **and no later spec edit rolls either**, because the guard can
never hold again for that role. An operator changing the image would watch the change be accepted
and silently not happen.

So a replica short of its Members is turned over **whole and immediately**, ahead of any replica
that is merely outdated. It is the one case where the guard's reasoning inverts: there is nothing
serving to strip and no reservation to trade, and waiting is not caution but a deadlock. Replacing
the whole group rather than filling the gap is AC8.10's rule, for AC8.10's reason — the survivors
hold a group Kueue will neither admit nor release.

The guard keeps its force everywhere else, which is asserted rather than assumed: widening the
exception to fire unconditionally turns five cases red, among them
`TestModelDeploymentReconciler_TheRolloutGuardCountsAdmittedReplicas` and
`TestModelDeploymentReconciler_ATemplateEditRollsOneReplicaAtATime`.

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

### ✅ A Pod claiming no ordinal never converged, whichever Workload owned it

**Status: FIXED.** The same exception in the convergence loop's rollout, widened to cover it.

The rollout guard reads admitted **Workloads** on one side and declared **replicas** on the other. A
Pod carrying no ordinal label seats no replica, so whatever Workload owns it answers for no declared
slot, and the correspondence the comparison rests on is gone. It fails in both directions. One
Workload per role — the shape the admission unit has above this spec's narrowing — makes the count
short: three Pods, one admission, three declared. A Pod holding a Workload of its own makes it long:
a full role plus one such Pod counts one more admission than the role declares. Either way
`admitted == declared` can never hold again for that role.

⛔ **The removal that would repair it is gated behind the same comparison**, so nothing reaches the
state to fix it. The create gate is owed nothing either — such a Pod is credited against the declared
count there — and the surplus path does not fire while the role holds no more Pods than it declares.
The role sits unconverged and **every later edit to it is accepted and silently does nothing**.

So a Pod claiming no ordinal is turned over on the same terms as a ReplicaGroup short of its Members,
and for the same reason: neither can ever be admitted as a declared replica, so waiting on that
admission is waiting for something that cannot arrive. One leaves per pass, the create gate fills the
ordinal behind it, and the role reaches the per-replica shape without a hand.

`TestModelDeployment_ARoleWhosePodsShareOneWorkloadIsRepaired` walks the whole pre-per-replica shape
— no ordinals, one group named for the role — and fails in **12 passes** against the narrower
exception, which is what makes it a gate rather than a description.

⚠️ **The shared-Workload half costs an outage for the role.** Its Pods answer to one Workload, so the
first departure deletes the Workload the survivors are holding and Kueue stops them; the passes that
follow rebuild the role from empty. That is the price of the shape, not of the repair — the
alternative is the silent unconverged state above, where a deployment serves, accepts edits and
applies none of them with no condition naming the cause.

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
3. ⚠️ **SUPERSEDED — the field shipped validated to `1`, and this spec now renders it.** The
   original decision was to separate the two numbers in the API before anything could conflate
   them, accept a field with a single legal value, and defer the render path; what bought it was
   AC4.4, since `resources.accelerator` being per-Pod rather than per-replica is only unambiguous
   once the second number exists. That reasoning stands and is why the field exists at all.
   **What changed is the scope, by decision rather than by discovery:** multi-Pod instances were a
   Non-Goal, and are now F8. The refusal is deleted, and `size` gains immutability (AC4.3b) in its
   place — a field with one legal value needs no immutability rule, and a field that shapes a
   running instance does.
   ⭐ **The reading that made it affordable is that the rejection of a StatefulSet substrate was
   narrower than it looked** — Alternatives had rejected one per *Role*, and one per *ReplicaGroup*
   is a different object graph. ⚠️ **That reading opened the scope and then turned out not to be
   the answer**: T10 measured that a StatefulSet contributes nothing to a Kueue serving group that
   this operator is not already doing, so F8 renders the Members directly. The conclusion the
   reading bought — that this belongs in this spec rather than a second one — survives its own
   premise, because what made it affordable was never the substrate but the discovery that the
   capability needed no new dependency.
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
