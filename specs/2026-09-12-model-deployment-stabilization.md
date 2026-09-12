# Spec: ModelDeployment Stabilization

Status: Planned
Blocked on: the work itself, and nothing else. Every decision this spec opened is answered — what
`spec` freezes and by which criterion (F1), and what a permanently infeasible deployment finally
looks like (F3). One question is left open deliberately and blocks no task: whether a deployment
split across two scheduling groups also deserves a group-level status. Nothing here is in progress
and no task has started. This moves to Shipped when the implementation opens its pull request. T12 is
separate and permanent: it needs two accelerator models or one machine with several cards, and no
code change can lift it.
Type: Feature

## Summary

A `ModelDeployment` is editable in every field, its roles must all name one `instanceType`, and the
KV transfer between a prefiller and a decoder has one implementation with no stated boundary. Each of
those is a decision that was made deliberately and written down with its reason. This spec changes
all three, so it has to answer those reasons rather than write past them.

- **Editing.** `ValidateUpdate` says there is nothing immutable to check, and gives as its reason
  that a mutable template is what makes a rollout possible. What replaces it is not a general freeze:
  the fields that say **which deployment this is** become immutable, and the ones that say how it is
  currently run stay editable.
- **Roles on one pool.** Admission refuses roles on different `instanceType`s, because one Kueue
  Workload carries one queue name and atomicity comes entirely from Kueue's intra-group rule. A
  prefiller and a decoder want different hardware, so that refusal has to be replaced rather than
  relaxed — and replacing it means obtaining atomicity a different way.
- **Transfer.** The operator synthesizes one connector today, and the package that holds it says in
  as many words that it is not a plugin point. Converging on Mooncake is the decision; what this spec
  owes is where the seam is and what re-opening it costs.

Two facts found while writing this decide more than they look like they should:

- **The rule that survives is not the rule this spec started from.** It began as "freeze `spec` to
  shrink the surface two writers can disagree on", and pricing the exceptions is what broke it: each
  exemption returns part of that surface, so the more the set holds the less the rule does, until at
  four or five members it does almost nothing and is pure cost. The way out was not a shorter list
  but a different question — **what makes this deployment the deployment it is** — under which the
  same exemptions stop being concessions and become consequences. F1 is written against that
  question, and the refusal message a user meets says so.
- **An AdmissionCheck runs after quota is reserved, and the one this repository already runs can only
  answer Ready or Retry.** That check is check-only: it never preempts and never rejects. A Retry is
  not a wait — Kueue evicts and drops the reservation — so joint admission built on that verdict
  would destroy the reservations it is supposed to assemble. **The waiting verdict is Pending**,
  which holds the reservation and admits nothing, and the price of holding is the bound F3 states —
  which the direction recorded upstream does not.
- **A check is attached to a ClusterQueue, not selected per Workload.** Which checks a Workload
  carries comes from its queue's `spec.admissionChecksStrategy`, so a new check reaches every
  workload in an accelerated pool — including ones this operator did not create — or it reaches none.
  A design that reads "only multi-group deployments acquire it" is not expressible, and F3 carries
  what replaces it.

## Motivation

### What already exists, and what each decision costs

Measured on `origin/main` at `f134b54a`, and re-measured at `25e6a421` when the base moved. Every
row below still held except the last, which stopped holding while this was being written: the status
kind's enum landed on its own, and the row records that rather than the state it was drafted in.

| Surface | State | The reason written beside it |
| --- | --- | --- |
| `ValidateUpdate` | Nothing immutable | "a mutable template is what makes a rollout possible" |
| `roles[].instanceType` | Must agree across roles, refused otherwise | "ONE KUEUE WORKLOAD CARRIES ONE queueName" |
| Pod group | One group for the whole deployment, one PodSet per role | Atomicity is Kueue's intra-group rule and nothing else |
| Replica change | Deletes and recreates every Pod of the group | Every Pod carries a group total they must all agree on |
| `spec.kvCache.connector` | `enum: ["auto"]`, one value | "it reserves the discriminator, so naming a specific connector later is an enum widening rather than a new field" |
| `pkg/worker/kvcache` | One implementation, in a sub-package | "no interface abstracts over implementations ... The package boundary is where the second one gets added; it is not a seam already built for it" |
| Resource-mode rules | Two rules named in a comment and not written | The obstacle was that the handler read nothing from the cluster; it now does |
| `status.roles[].kind` | **Enum, since `25e6a421`** — it was a plain string when this was drafted | The spec field carried the enum; the status field was left until the writer was known to stay inside it, and that is now established |

None of these is a defect. Each is a decision with a cost that was paid knowingly. What changed is
the requirement, not the quality of the reasoning, and that is why every feature below names the
sentence it overturns.

### Goals

1. A `ModelDeployment`'s identity cannot be edited. The fields that say which deployment it is are
   refused on update with a message saying so; the fields that say how it is currently run stay
   editable, and the criterion separating them is written down rather than only its output.
2. A prefill role and a decode role can name different `instanceType`s and still be admitted as a
   unit — or not at all. Neither half runs while the other waits.
3. A prefill replica and a decode replica never contend for one accelerator. Sharing a card is
   forbidden where it means a logical slice and permitted where the hardware enforces the partition,
   which is F4's distinction rather than a softening of this goal.
4. The KV transfer implementation is one, is named, and the cost of naming a second one later is
   written down rather than discovered.
5. Two admission rules and one status enum that were deferred with stated reasons, and whose reasons
   have expired, are closed here rather than carried further.

### Non-Goals

| Not doing | Why |
| --- | --- |
| A revision or rollout object | Two objects holding two revisions with traffic shifted between them is how a zero-downtime image change is normally done. It is not needed to make an image change *possible* — F1 leaves the image editable — and F9 states the disruption that remains. Removing that disruption is a larger design than this one |
| Renaming replicas to get Kueue-native replacement | Changing `<deployment>-<role>-<ordinal>` would restore per-replica rollout and would change the naming contract, the ordinal-based scale-down and everything that reads a replica's name. It is recorded upstream as a design decision rather than a fix, and F9 keeps it that way while stating what it would buy |
| Routing between the roles | That is the next specification. This one makes the thing it routes to stable |
| Withdrawing `roles[].acceleratorKey` | Already done. The field is absent from the API, the reference page and the render site; one stale comment remains and T4 removes it as a rider, because the file is already open in front of it |
| Cross-backend or cross-pool roles | One deployment attaches to one Binding. Splitting the queue does not split the cache domain, and nothing here asks it to |

### Dependencies

| Depends on | State | Why |
| --- | --- | --- |
| S6 — single-role `ModelDeployment` | **Its specification is no longer in the repository**, removed at `01a2df3c` | Only its shipped code surface is used, and that surface is still here; nothing depended on the document, which is why its removal costs this spec nothing |
| S7 — cross-role atomic admission | Shipped | This spec replaces how its atomicity is obtained, so its mechanism is the starting point |
| Kueue's AdmissionCheck | In use already | The node-devices check is the working precedent, and its ordering relative to quota reservation is the fact F3 is built on |

## Proposal

### User Stories

**An operator upgrades the runner image.** They edit the role's image and apply. It is accepted —
which build serves a model does not change which deployment this is — and every replica of the group
restarts, because that is what any edit does here and the reference page says so beside the field.

**An operator tries to point a deployment at a different model.** The webhook refuses on update,
names the field, and says that a different model is a different deployment. They learn this at
`kubectl apply`, rather than from an object whose name, `status` history and cache domain now describe
something it is no longer serving.

**An operator tries to move a role to other hardware.** They edit `instanceType` and are refused for
the same reason: which pool a role is admitted against is part of what the deployment is, and the
deployment that runs on the other pool is a new one.

**A platform operator runs prefill on fast cards and decode on large ones.** They give the two roles
different `instanceType`s. Both are admitted together or neither is, and no prefiller holds an
accelerator while waiting for a decoder that never arrives.

**An SRE reads a stuck deployment.** Two roles have reserved quota and neither is running because the
third cannot be placed. The deployment says so, names the role that cannot be placed, and stops
holding the reservation after a stated bound rather than retrying silently forever.

### Core Features and Acceptance Criteria

#### F1 — The identity fields are immutable

**The rule is that a `ModelDeployment`'s identity cannot be edited.** The fields that answer *which
deployment is this* are refused on update; the fields that answer *how is it being run right now*
stay editable. A user who changes an identity field is not adjusting a deployment — they are asking
for a different one, and the refusal says exactly that.

**Why this rule and not a general freeze.** The requirement arrived as "freeze `spec`, to shrink the
surface on which two writers can disagree". Pricing the exceptions is what broke it: every exemption
returns part of that surface, so a set holding `replicas`, `image`, `extraArgs` and `env` leaves
almost nothing frozen and the rule stops paying for its own complexity. Identity is a different
question with a different answer shape — under it those same exemptions are not concessions bargained
against the rule's purpose, they are **consequences of it**, because none of them changes which
deployment the object is. The rule is stated here rather than only its output, so that the next
person adding a field has a criterion instead of a list to guess from.

| Frozen — it says which deployment this is | Editable — it says how the deployment is run |
| --- | --- |
| `model.name` — what it serves | `roles[].replicas` — how much of it there is |
| `engine` — what serves it | `engineVersion`, `roles[].template.image` — which build serves it |
| `kvCache.poolRef`, `kvCache.connector` — whose cache it shares, which is a reuse-domain fact and therefore an identity one | `roles[].template.imagePullPolicy`, `roles[].template.imagePullSecret` — how that build is fetched |
| `roles[].name`, `roles[].kind`, the set of roles — its shape | `roles[].extraArgs`, `roles[].env`, `roles[].template.env` — how that build is tuned |
| `roles[].instanceType` — which pool it is admitted against | *(prospective)* `router.mode`, `router.replicas`, `router.image`, `router.extraArgs` — whether and how a router fronts it |
| `roles[].template.command` — whether the operator configures this role at all | `roles[].template.privileged`, `roles[].template.ports`, `roles[].template.additionalVolumes` — how the container is shaped |
| `roles[].resources` — **frozen for a reason that is not identity; see below** | |

**Every field of `spec` is on this table, and that completeness is the point rather than a
formality.** Two rounds of checking the role template against the Go type rather than against this
document found two real problems — `engineVersion` stranded on the wrong side, then the pull policy
and pull secret stranded away from the image they fetch — and both were found by counting the type's
fields, never by re-reading the table. `roles[].template.resources` is the one field with no side: an
existing rule refuses it outright, so it is never part of an update either way.

**The `router.*` row is the one entry that is not in the schema, and it is marked so.** Today
`ModelDeploymentSpec` holds `model`, `engine`, `engineVersion`, `kvCache` and `roles` and nothing
else; the router fields arrive with the router specification. They are classified here because the
classification was decided here and a decision left unwritten gets made twice, but **F1's
"one case per editable field" ranges over the fields that exist** — a case cannot be written against
a field the type does not have. That counting went one way only, which is how the row survived: the
type's fields were counted into the table, and the table's rows were never counted back against the
type.

Two of the editable entries carry a consequence worth meeting here rather than in an incident.
**`template.ports` decides the published address**: the Service port and `status.endpoint` are read
from the role's first container port, so editing it moves the address clients resolve. And
**`template.privileged` is the one editable field whose argument is not really about identity** — it
does not change which deployment this is, so the criterion puts it here, but it escalates a running
workload in place. It stays editable because a privilege requested explicitly through the API is a
request, not an inference; if that is ever judged the wrong trade, it moves for a security reason and
not because the criterion changed.

**`engineVersion` and `template.image` are on the same side, and that is not a detail.** The version
is what synthesizes an image for a role that names none, so freezing one while the other is editable
would let a user who overrides the image upgrade in place while a user who relies on synthesis could
not. Two classes of user with different capabilities, produced by an accident of which field they
happened to use, is not something the identity rule asks for.

**`roles[].resources` is the one frozen field the identity criterion does not produce**, and its
reason is recorded here rather than left to be re-derived:

> It does not answer "which deployment is this", but it changes what admission has to find —
> changing it is renegotiating the scheduling, which is not materially different from deleting and
> recreating.

That makes it the single field frozen against the criterion, and it is marked as one in the table. A
reader who applies the criterion and finds this field unaccounted for would otherwise have two bad
options: treat it as an oversight and remove it, or invent a justification that is not the real one.
`template.privileged` is its mirror image — left editable *by* the criterion while a different
argument could move it — and both are labelled so that the table is never mistaken for the criterion's
output alone.

Acceptance:

- An update changing an identity field is refused, naming the field path and saying that changing it
  describes a different deployment, which is created rather than edited. **The message states the
  rule, not the mechanism** — "this is what makes it this deployment" rather than "concurrent writes
  would conflict", because the second is no longer the reason and a message that gives it would send
  the reader looking for a locking problem.
- One case per refused field, so a rule refusing everything is distinguishable from a rule refusing
  the right things; and one case per editable field, so the converse is too.
- An update that changes nothing is accepted, including a re-apply of the identical object. A rule
  comparing objects rather than fields would refuse a no-op apply, which every controller and every
  GitOps agent performs constantly.
- **`roles` is compared by name, not by position.** The field is a `listType=map` keyed by `name`, so
  a reordered list is the same set of roles and the API treats it as one. A positional comparison
  would refuse a declarative apply that changed nothing, which is the previous criterion failing on
  the one field most likely to be re-serialized in a different order.
- `metadata` and `status` are untouched by this rule. Labels, annotations and finalizers stay
  editable, because freezing them would break the controllers that write them, including this one.
- The refusal survives the deletion window: an object being deleted still validates updates, as it
  does today, so a finalizer edit is not refused by a rule aimed at `spec`.
- The comment on `ValidateUpdate` that says there is nothing immutable to check is replaced, not left
  beside code that now contradicts it.
- **Editing an editable field is not free**, and F9 owns that cost rather than restating it here.
- **A rule added here must not strand an object that already exists.** This webhook already holds
  that principle and implements it — a role the object already carried is exempt from the
  Service-name rule, precisely so a rule gained after storage cannot refuse every later edit
  including the one that would fix it. F1, F4 and F6 all run on update, and F1 freezes
  `roles[].resources`, so without the same exemption a stored object that violates F4's or F6's new
  rules becomes permanently un-updatable: every edit is refused by the new rule, and the field that
  would satisfy it is refused by this one. Each of those rules therefore exempts the value the object
  already had, and refuses only a *change* into a violating value.
- **The comparison must survive a field that was never populated.** Defaulting runs on the incoming
  object only; a stored object written before a default existed carries the zero value, so a
  naive comparison sees `nil` becoming `1` and refuses an edit nobody made. The rule compares the
  stored object after the same defaulting the incoming one receives, or it exempts a transition out
  of the zero value — the task picks one and says which.
- **Every field of `spec` is classified, including the ones this table does not name.** The role
  template alone carries nine fields and the table above names three of them; the rest — the pull
  policy, the pull secret, the privileged flag, the extra ports, the additional volumes — must land
  on a side deliberately. **The pull policy and the pull secret travel with the image**, which is
  editable, and freezing them would leave a user able to point a role at a different registry and
  unable to supply credentials for it, which is the same shape of half-usable exemption that
  `engineVersion` was moved to avoid. `template.resources` is refused outright by a rule that already
  exists, so it needs no side.
- **The escape this rule deletes is named rather than discovered.** The defaulting half refuses any
  write while a role's `InstanceType` is absent, and its comment offers the remedy: point the role at
  a type that does exist. Freezing `instanceType` removes that remedy, so a deployment whose type is
  deleted can no longer be edited at all — not even a label — and recovery becomes recreating the
  type or deleting the deployment. That is acceptable and it is written down; discovering it during
  an incident is not.
- **Comments this rule falsifies are replaced in the task that falsifies them.** `ValidateUpdate`'s
  is one. So is the defaulting half's justification for running on update, which reads "because roles
  are not frozen" and stops being true here.

**The criterion is a good question, not a decision procedure, and one field proves it.**
`roles[].template.command` is frozen by the table above, and two reasonable engineers will disagree
about whether it belongs there: a non-empty command is the take-over tier, which stops the operator
synthesizing anything for that role and moves its cache condition to `Unknown`. Read one way that is
a run-time choice about how the role is launched; read the other it decides whether this is a managed
deployment at all. It is frozen because the second reading is the one that survives — a role the
operator does not configure is a different thing from one it does — but the field is named here
because a criterion that needed no argument on it would be a stronger claim than this one is
entitled to. **The table is what governs; the criterion is what a new field is argued against.**

**This rule deliberately breaks updates that work today.** Nothing in `spec` is immutable now, so
changing a model name, an engine, a role's kind or a pool reference is currently accepted and
re-renders. Each of those becomes a refusal, and the reference page carries the migration — delete
and recreate — beside the fields rather than leaving a user to discover it at `kubectl apply` on a
deployment they have been running for months.

#### F1a — Identity is also what a reader diagnoses with

A consequence worth stating separately, because it is what makes the rule worth its cost rather than
merely coherent: once the identity fields cannot move, `status` describes the same deployment for the
object's whole life. A figure read at two moments is comparable, a stale condition is a bug rather
than a possible race with an edit, and a report naming a deployment names one thing. That property is
unavailable under a rule that lets the model or the pool be swapped underneath an object that keeps
its name.

Acceptance: no new mechanism. This is recorded so the rule's value is legible to a reader who only
sees a table of refused fields, and so a later exemption request is weighed against it.

#### F2 — Scheduling groups: one per role, or one for all

When every role names one `instanceType`, the deployment is one pod group, exactly as today. When
roles name different `instanceType`s, each role becomes its own pod group with its own Workload.

The split is forced rather than chosen: a queue name is derived from the `instanceType`, one Workload
carries one queue name, so two `instanceType`s cannot be one Workload. This is the replacement the
refusal in place today was holding the line for.

Acceptance:

- Roles on one `instanceType` render one group, with one group name and one total, byte-identical to
  today's rendering. This is asserted against the existing fixtures, because the single-pool case is
  the one that must not regress.
- Roles on two `instanceType`s render two groups, each with its own name, its own total counting only
  its own role's replicas, and its own queue-name label.
- Two roles on the same `instanceType` and a third on another render **two** groups, not three: the
  grouping key is the `instanceType`, not the role.
- The admission rule that refuses differing `instanceType`s is deleted, and the test that asserted
  the refusal is replaced by one asserting the two-group rendering — not merely removed.
- The group name stays derivable and collision-free when one deployment produces several: a name that
  was unique per deployment must become unique per deployment and `instanceType`.
- The stale comment naming the withdrawn `roles[].acceleratorKey` in the group digest rationale is
  removed here, because this task is already rewriting the sentence around it.

**Seven things hold for one group and stop holding for two, silently.** They are listed because each
is a place where unchanged code produces a plausible wrong answer rather than an error: the group
name is derived from the deployment alone; the group total sums *every* role; one group metadata
object is stamped onto every Pod at render time; the reconciler keeps one Pod set and makes one
rebuild decision; teardown deletes one Workload; status resolves one Workload and reads the queue
from `roles[0]`; and the Workload lookup takes the lexicographically first Workload owning any of the
deployment's Pods. Left alone, two groups either merge, or each claims the deployment-wide total and
waits forever, or one Workload leaks, or every role's status is attributed to one group.

**The role hash stays the role's name.** It is what Kueue groups PodSets by and what status reads to
attribute a Pod, so folding the `instanceType` into it would change PodSet identity and break both.
The new group identity carries the type separately, in the group name.

#### F3 — Joint admission, and the bound it needs

With one Workload per group, Kueue's intra-group atomicity no longer covers the deployment. An
AdmissionCheck claimed by this operator gates every Workload of one deployment on the whole set being
feasible, which is the direction already recorded for this problem and the mechanism this repository
already runs for the node-devices gate.

**What a Retry actually does is not what it sounds like, and the whole bound is designed against
it.** The check runs after Kueue reserves quota and answers only Ready or Retry, never preempting and
never rejecting. But a Retry does not leave the Workload sitting on its reservation: Kueue **evicts**
it — resetting the checks to Pending and dropping the reservation, in two separate writes — and its
scheduler then refuses to reserve again while a check reads Retry, so re-reserving is what clears the
eviction and re-opens evaluation. The check beside this one backs off thirty seconds for exactly this
reason, to keep a transient shortage from hot-looping.

So a deployment gated by a Retry is not sitting on a static hold. It is in a **cycle**: reserve, fail
the check, evict, back off, reserve again. Three things follow, and together they are why this design
does not answer Retry at all.

- **The quota is taken and dropped repeatedly rather than held.** "Holding quota it is not using" is
  intermittent, which is less damaging than a permanent hold and much harder to see.
- **A Retry destroys exactly the reservation a joint barrier needs.** Two siblings waiting on each
  other through Retry trade the same quota indefinitely: each one's wait drops the reservation the
  other was waiting to observe, so the set can fail to assemble even when the cluster could hold it.
- **The deployment's `QuotaReserved` condition flaps** on the cycle, because it is read from the
  Workload's reservation condition.

An earlier draft of this section described a static hold and was wrong about what a Retry does; the
paragraph above is the correction. **A hold does appear in the finished design, and it is a different
one** — it comes from choosing Pending deliberately, with the bound as its stated price, rather than
from a Retry that was imagined to hold.

Acceptance:

- Every Workload of one deployment carries the check, and the check reports Ready only when every
  role of that deployment is feasible.
- One infeasible role holds the whole deployment: no role is admitted, asserted by observing that no
  Pod of any role is ungated.
- **The waiting state is Pending, and this check never answers Retry.** A Pending check does not
  evict: Kueue admits on a reservation *and* every check Ready, so a Pending one leaves the Workload
  holding its quota with its Pods still gated, and nothing times it out. Retry would do the opposite,
  which is why it is excluded rather than merely unused. So the verdicts are **Pending while waiting,
  Ready when every role is feasible, Rejected at the bound** — the opposite shape to the check beside
  it, and deliberate.
- **The barrier is a coexistence of reservations, not an atomic write.** Every sibling reserves and
  then waits in Pending, so by the time any of them turns Ready they all already hold their quota and
  none can lose a race for it. The Ready writes are still one per Workload, so one group's Pods can
  ungate a moment before its sibling's; that window is closed by the next reconcile and the sibling's
  admission was never in doubt, because its reservation was taken before the wait began. The claim is
  therefore **"no role is admitted before every role holds its quota"**, which Pending delivers, and
  not "the two groups start in the same instant", which nothing here delivers and no criterion below
  assumes.
- **An infeasible set is bounded.** After a stated period without becoming feasible, the deployment
  reports it on a condition naming the role that cannot be placed, and the reservation is released
  rather than held silently forever. The bound's value is a field comment's to state and a constant's
  to hold; what this criterion fixes is that some bound exists, **because Pending is exactly a hold**.
  It is chosen so that siblings' reservations coexist, and that same property leaves an infeasible set
  sitting on quota it will never use until something ends it. The bound is the price of the barrier,
  named beside it rather than discovered later.
- **Two deployments can hold each other's quota, and the bound is what makes that recoverable.**
  Deployment A reserves in pool X while waiting on Y; B reserves in Y while waiting on X. At equal
  priority, or in a queue where preemption is off, neither is resolved by Kueue's ordinary
  requeueing — preemption is policy and cannot be assumed. The bound is therefore not a nicety for a
  single stuck deployment; it is the only thing that breaks this cycle, and a design that omitted it
  would ship a deadlock reachable from two ordinary manifests.
- **Releasing is a protocol, not a delete.** Deleting a Workload does not release anything on its
  own: the serving pod group is rebuilt by the next reconcile and composes a new Workload, so the
  reservation returns. Release needs durable state on the deployment that stops the reconciler
  recreating the group until something changes. The shape of that state is T7's; what is fixed here
  is that "delete the Workload" is not an answer.
- **A check is attached to a queue, not chosen per Workload, and that decides what the single-group
  case can mean.** A ClusterQueue names its checks in `spec.admissionChecksStrategy`, and this
  operator already sets that field on every accelerated derived queue for the node-devices check.
  There is no per-Workload opt-in, and one queue serves single-group and multi-group deployments
  alike — and workloads that are not deployments at all. So every Workload in an accelerated pool
  carries this check too, and the check answers Ready at once for everything that is not a
  multi-group ModelDeployment. A single group stays atomic by Kueue's own rule and no second
  mechanism runs over it; what does not hold is that the check is absent. **A controller that
  answered only for multi-group deployments would park every other workload in the cluster**, which
  is the failure mode this criterion exists to name.
- **"Every Workload of the deployment carries the check" is a claim about queue coverage, and the
  node-devices check's coverage is not wide enough to borrow.** That one is referenced only from a
  queue that is `acceleratable`, and only while the cluster-wide derived-from-node setting is on —
  its own comment says the gate runs on every accelerated queue or on no queue at all. A multi-group
  deployment can put one role on an accelerated pool and another on a CPU pool, and with the setting
  off no pool carries a check at all. Either case leaves a group ungated and free to run alone, which
  is the whole failure this feature exists to prevent. So this check is referenced from **every
  operator-owned queue** rather than only the accelerated ones, and where even that cannot hold — the
  setting off, or a queue the administrator authored and this operator does not fill — **the
  multi-group shape is refused at admission, naming the setting**, rather than admitted with a
  barrier that is not there. Refusing is the conservative half of a real choice, and it is recorded
  as a choice.
- The check never preempts. **It does reject, and only at the bound** — which is the opposite of the
  check beside it and is the whole mechanism F3's bound rests on, so the difference is deliberate
  rather than an inconsistency. While the set may still become feasible the verdict is Pending; once
  the bound passes it is Rejected, and Kueue deactivates the Workload itself.

#### F4 — Prefill and decode do not contend for one accelerator

A prefill replica and a decode replica must not contend for the resources of one accelerator.

**The requirement is about contention, not about the card, and saying it the other way would forbid
a shape that is fine.** Two Pods contend for one accelerator only when both hold a logical slice of
it — a per-card memory or cores percentage. A whole-card request never shares. A hardware partition
profile *does* put two consumers on one physical card, and this rule accepts that: the Devices API
carves an accelerator into partitions whose isolation the hardware enforces, and the accelerator must
be put into a partitioning mode to offer them at all. Written as "never the same physical card", the
rule would refuse the partitioned shape for a reason that does not apply to it.

Where this lands is admission rather than placement: the condition is answerable from the submitted
object together with the `InstanceType` the webhook already reads, and it needs no scheduler-level
affinity rule and no device-plugin change.

Acceptance:

- A deployment with a `prefill` role and a `decode` role **on `instanceType`s that can select the
  same accelerator** where both request a logical slice is refused, naming both roles and the field
  that makes them shareable.
- **The same pair is accepted when the two `instanceType`s cannot select the same accelerator — and
  two different names do not establish that.** An admin can author an InstanceType against an
  `acceleratorGroup` a derived type already covers; the two then land as siblings over one card, and
  the walkthrough in this repository shows exactly that, as "one accelerator, two views of it". So
  the qualifier is a disjoint accelerator population, which the webhook computes from the
  `InstanceType` it already reads. Name inequality is the wrong predicate in a way worth stating,
  because it leaves open precisely the case the rule was written to close, while dropping the
  qualifier altogether over-refuses and blocks the heterogeneous shape F2 exists to enable.
- The same shape with whole-card requests is accepted.
- The same shape with partition profiles is accepted **even on one card**, and the field comment
  states why: the partitions are hardware-isolated, which is the property this rule is about.
- A single-role deployment is unaffected in every combination — this rule is about a pair.
- **Not asserted here:** that two replicas actually landed on different cards. That is the one claim
  in this spec needing hardware, and it is F8.

#### F5 — The transport boundary, and what re-opening it costs

The KV transfer between a prefiller and a decoder converges on Mooncake, because it is the
implementation that supports heterogeneous prefill and decode. NIXL and ROCm NIXL stay reachable.

**Reachable does not mean a plugin point, and building one now would be the wrong move.** The seam
already exists and is already named: `spec.kvCache.connector` is a single-value enum whose own
comment says it reserves the discriminator so that naming a specific connector later is an enum
widening rather than a new field. The package holding the implementation says the same thing from the
other side — there is no interface and no dispatch, and the package boundary is where a second
implementation gets added rather than a seam already built for it. Two independent places in this
repository already priced this question and gave the same answer.

So this feature is a written decision plus the test that keeps the decision honest, not a mechanism.

Acceptance:

- The convergence is stated where a reader of the API meets it: the connector field's comment says
  Mooncake is the one implementation and what widening the enum would take.
- **The reservation is in the schema and in nothing else, and the comment says so.** `kvCache.connector`
  is read by no code: the binding resolution passes a domain, an endpoint and a protocol; the
  connector synthesis takes an engine, a kind, a manufacturer and that connection; and the renderer
  dispatches on the *engine*. The struct named `Connector` in the render path is the synthesized
  result, not this field. So "the discriminator is reserved" is true of the API and false of the
  implementation, and a comment claiming the seam is ready would be claiming half of it.
- **No test asserts that the renderer dispatches on the field, because it cannot.** With one legal
  value, a renderer that reads the field and one that ignores it are observationally identical — and
  the field is in fact ignored. What is pinned instead is the enum's single value together with a
  test that the field is *inert*, so that the day it stops being inert is the day a test says so.
- No interface is introduced, no dispatch is abstracted, and no second sub-package is created. The
  full cost of a second implementation is stated where the reservation is: one sub-package, one entry
  in the connector enum, one renderer, **and the wiring that threads the field to a dispatch point
  which does not exist yet**. That last item is the part the reservation does not already cover.
- **A widened enum reaches new deployments only.** F1 freezes `kvCache.connector`, so an existing
  deployment cannot be edited onto a second connector once one exists — it is recreated. That is
  consistent with the identity rule and it is stated here, because "widening the enum" otherwise
  reads as a migration path for deployments that are already running.
- **Not asserted here:** that NIXL would work. Nothing in this repository has run it, and a claim
  that it would is a claim no test backs.

#### F6 — Two admission rules the resource request has been missing

The comment on the role-resources validator names two rules that are not written and states that
what prevented them is gone. They are written here.

Acceptance:

- A request naming a resource mode the named `InstanceType` does not offer is refused, naming both
  the mode and the type.
- A request exceeding that type's per-unit ceiling is refused, naming the ceiling.
- Both rules read the `InstanceType` through the same path the defaulting half already uses, so there
  is one reader and not two that could disagree.
- A request naming an offered mode and fitting the ceiling is accepted — and both rules get that
  baseline separately, because a request satisfying one can violate the other.
- **A type whose observed detail has not been computed yet gets a transient refusal, not a permanent
  one.** Both rules read `InstanceType.status.detail`, and an empty detail is the not-yet-synced
  state rather than "this type offers no modes" — the accessor that reports it says so in as many
  words. The Instance webhook already meets this case and answers with a retryable internal error so
  the same request succeeds once the reconciler fills the status; these rules take the same outcome
  through the same helper rather than a second reading of the same emptiness. Written without it, a
  valid deployment applied in the seconds after its pool appears gets a permanent field error naming
  a mode the type does in fact offer, and the user's fix is to wait — which the message does not say.
- The comment naming these as unwritten is removed, not left describing work that is done.

#### F7 — The status kind enum, proved from the writer

`status.roles[].kind` gains the enum its spec counterpart already carries.

**This one landed before the rest of this spec did, and the record says so rather than claiming it.**
The marker and both halves of its verification arrived on `main` at `bb0ef941` while the tasks below
were still being written. The acceptance is kept here because it is what the delivered work is judged
against, and T2 records where it was delivered — a feature quietly dropped because somebody else got
to it reads, later, as a feature that was never needed.

**The check is not that the enum is present.** Adding it changes what the API server accepts on a
status write, so a value the controller can still produce and the enum does not list turns every
later status write into a failure and freezes every other figure on the object. The verification has
to come from the writer's value set, not from the enum's.

Acceptance:

- The marker sits on the field, not on the type — the type's Go-level marker does not become CRD
  validation.
- A test enumerates every value the controller can write into that field, from the function that
  produces it, and asserts each is in the enum. A test that writes one legal kind and sees it
  accepted does not cover the failure mode.
- A status write is exercised end to end against the generated schema, so that the enum is proven to
  be on the served CRD rather than only in the Go marker.

#### F8 — Admission rules, in one place

| Rule | Refused because |
| --- | --- |
| An update touching an identity field | Changing it describes a different deployment, which is created rather than edited |
| A resource mode the named `InstanceType` does not offer | The admission chain would refuse it later, on another object, naming none of this |
| A request over the type's per-unit ceiling | Same |
| A `prefill` and a `decode` role both requesting a logical slice | They could land on one card, which the pair exists to avoid |

The rule refusing roles on differing `instanceType`s is **deleted** by F2 and is listed here only so
that its absence is deliberate rather than an oversight.

Two of these rules read the `InstanceType` from the cluster. The validating half of this webhook
answered everything from the object alone until the mutating half arrived and began reading; that
sentence is now true only of the rules that do not need the type, and any comment asserting it
flatly is corrected by F6's task.

#### F9 — Every permitted edit is a full restart, and this is where that is stated

**This feature owns the fact.** Changing a replica count, a template, an image or an argument deletes
every Pod of the group and recreates it, because every Pod carries a group total that Kueue requires
them all to agree on. Changing the *set* of roles would do the same and is not listed, because F1
freezes it — naming it here would promise an edit the webhook refuses. There is no rolling update and no per-replica replacement;
the Kueue-native alternative that would give one is blocked on replica naming. Other features point
here rather than restate it, so that a change in this behaviour has one sentence to correct.

**F1 makes this the whole cost surface rather than part of it.** The edits it leaves available —
image, arguments, environment, replicas — are exactly the ones an operator performs routinely, and
every one of them restarts every replica of the group. A user upgrading an image is not doing
something cheaper than recreating the object; they are keeping the object's name, its `status`
history and its cache-pool registration, and paying the same restart.

**F2 reduces the blast radius without removing it.** When the roles sit on different `instanceType`s
they are different groups, so editing decode rebuilds decode and leaves prefill serving. When they
share one `instanceType` they are one group, and editing either rebuilds both.

That is a coupling worth naming, because it is not where anyone would look for it: **how expensive a
scale is depends on whether the roles happen to share an `instanceType`**, a field chosen for
hardware reasons. A user who wants cheap scaling now has a reason to split types that has nothing to
do with hardware.

Acceptance:

- The reference page states the blast radius in both shapes, in the section that already describes
  recreate rollout.
- A scale of one role in a two-group deployment leaves the other group's Pods untouched, asserted by
  their identities being unchanged across the operation rather than by counting them.
- A scale in a one-group deployment rebuilds both roles, which is today's behaviour, asserted so the
  difference between the two shapes is pinned rather than inferred.
- **Not decided here:** whether to rename replicas to obtain Kueue-native replacement. It is recorded
  as a design decision upstream and stays one; what this spec adds is the observation that F1 raises
  its value, because the operation it would make cheap is now the only operation there is.

### Verification

**L0, no cluster.** F1's update rules, F4's pair rule, F6's two rules and F8's table as table-driven
webhook tests. F2's grouping and F9's blast radius as renderer tests comparing whole objects. F5's
enum and dispatch, and F7's writer-side enumeration, as unit tests.

**L1, single node, no accelerator.** A two-`instanceType` deployment produces two Workloads and is
admitted as a unit or not at all; one infeasible role leaves every role ungated; the bound in F3
fires and the condition names the role. A frozen field is refused on a live object. A scale of one
role in a two-group deployment leaves the other group running.

**L2, two accelerator models or one multi-card machine.** F4's claim that a prefill replica and a
decode replica do not end up contending for one accelerator: separate cards where the types are
disjoint, hardware-isolated partitions where the pair is partitioned. Nothing else needs hardware,
and this criterion is separated so the rest cannot be held behind it.

### Notes, Constraints and Caveats

**The bound ends a hold, and only because this check declines the verdict that would have made it a
loop.** A Retry evicts the Workload and Kueue re-reserves after a backoff, so a Retry-based design
would have to end a cycle rather than a reservation — and would also destroy the coexisting
reservations the barrier is built out of. Answering Pending instead keeps the reservation, which is
what makes the barrier work and what makes the bound necessary. Either way the ending needs durable
state that survives the next reconcile — a mark on the deployment that stops the group being rebuilt
— because deleting a Workload only removes one turn and the next pass composes another. At the bound
the check answers Rejected and Kueue deactivates the Workload itself, so the actor is Kueue and no
mark of our own is invented for that half.

**Deleting the instanceType-must-agree rule removes a refusal users have seen.** Its message names
the tracking issue for heterogeneous placement, so a user who read it is expecting the capability
this spec delivers. The replacement is the capability, not a different refusal.

**Freezing `spec` interacts with defaulting.** The mutating half of this webhook writes into `spec`
on update as well as create. A freeze comparing the submitted object against the stored one must
compare after defaulting, or the operator's own default becomes a refused edit on the next apply.

**One deployment, one cache domain.** Splitting the scheduling group does not split the Binding. Both
groups attach to the same pool and the same reuse domain, which is what makes the split safe for the
cache and is why no cache-side change appears here.

### Boundaries

Owns `pkg/worker/webhooks/worker/model_deployment.go` (F1, F4, F6, F8),
`pkg/worker/controllers/worker/model_deployment_pod_group.go` (F2, F9),
`pkg/worker/controllers/worker/model_deployment.go` and `model_deployment_status.go` (the group set
and its status), a new AdmissionCheck reconciler beside the node-devices one,
`api/worker/v1alpha1/model_deployment.go` (F5's comment, F7's marker) and
`docs/reference/model-deployment.md`.

Does not touch `pkg/devicemanager/**`, the `KVCacheBackend` or `KVCachePool` controllers, or
`pkg/worker/kvcache/mooncake/**`. F5 is a decision and a test, not a refactor.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The identity set is read as a list and grown one exemption at a time until it means nothing | F1 states the criterion above the table and F1a states what the rule buys, so an exemption request is answered against a question rather than against precedent. The three fields where the criterion and the delivered set disagree are named in F1 rather than left for someone to discover as an inconsistency |
| A user reads "image is editable" as "image changes are cheap" | F9 owns the restart fact and the reference page carries it beside the field. The user story for an image upgrade states the restart in the same sentence as the acceptance |
| Two deployments each hold partial reservations and neither completes | F3's bound is what makes this recoverable. Whether Kueue's own preemption already resolves it is unverified, and the bound is specified so that the answer is not load-bearing |
| A no-op re-apply is refused by the freeze | An explicit acceptance criterion, because every GitOps agent re-applies constantly and a rule comparing objects rather than fields fails exactly there |
| The freeze refuses the operator's own defaulting write | Named in the constraints: the comparison happens after defaulting. This is the failure that appears only on the second apply, which is the hardest kind to attribute |
| Two groups drift apart — one admitted, one not — despite the check | Reachable for a moment, not indefinitely, and stated that way rather than as an atomicity the writes do not have. Each sibling holds its reservation while Pending, so a group that ungates first cannot take quota its sibling then fails to get; the remaining window is one reconcile wide. The acceptance is observed on the Pods rather than on a Workload's own status |
| F4's admission rule passes while the replicas still contend | The rule bounds what is expressible; only the hardware criterion proves placement. Both are stated, and the second is not claimed by the first |
| The e2e suite cannot produce a two-`instanceType` cluster | The case creates the second type rather than assuming one, and states which, because "the cluster has two pools" must be a fact the case creates |

## Design Details

### Project Structure

```
api/worker/v1alpha1/model_deployment.go          F5's comment, F7's enum marker
pkg/worker/webhooks/worker/model_deployment.go   F1, F4, F6, F8
pkg/worker/controllers/worker/
  model_deployment_pod_group.go                  F2's grouping, F9's blast radius
  model_deployment.go                            creating and pruning a set of groups
  model_deployment_status.go                     the group set and the infeasibility condition
  model_deployment_joint_admission.go            NEW: the AdmissionCheck reconciler
  node_queue.go                                  where the new check is referenced from a queue
docs/reference/model-deployment.md               the identity fields, the two shapes, the blast radius
```

### Commands

**Everything runs locally** except F4's hardware criterion. T1 through T9 are Go tests on this
machine; the cluster cases need a local single-node cluster with the operator deployed.

| Purpose | Command |
| --- | --- |
| One package's tests | `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/...` |
| The whole tree | `make test` |
| Build one package | `go build ./api/...` |
| Lint Go and shell | `make lint` |
| Lint markdown and specs | `make lint docs < /dev/null` |
| Regenerate the API | `make generate`, from a module-suffixed checkout, never from a worktree |

The same four properties that govern the neighbouring specifications govern this one: `make lint`
edits files; `make generate` cannot run from a worktree and corrupts generated files before failing
if attempted; `make test` takes exclusion patterns rather than inclusion patterns; and a
`go test -run` pattern that selects nothing still exits zero, which the spec linter checks. **No
`Verify:` line below carries `-run`**, because the tests it would select do not exist yet;
tightening them is part of moving this spec to Shipped.

### Code Style

Follows the file it lands in. The immutability rule is a field-by-field comparison driven by one
table naming the identity fields, not a chain of if-statements, so that the set is readable in one
place, testable as data, and sits directly under the criterion that selects it. The group set is produced by one function returning a slice, so the
one-group and two-group cases are the same code path with different inputs rather than two branches.

### Implementation Plan

**No decision gate stands above these tasks.** Two did while this spec was being written, and both
are answered: which fields the freeze exempts — where the answer changed the rule rather than filling
in a list, and F1 carries it — and what a permanently infeasible deployment ends up looking like,
which F3 carries. Every task below can start as soon as its own blockers land.

Ordering: **T1, T2 and T4 start immediately and in parallel** — three tasks, three disjoint file
sets. T3 and T8 follow T1 because all three rewrite the one webhook file and its one table-driven
test; T9 follows T2 for the same reason on the API file. Those edges are **file dependencies, not
substantive ones**, and they are labelled as such so that a later reader splitting the files can
delete them rather than preserving a dependency that was never real. T4 is the structural change and
everything about grouping follows it.

Checkpoints: after T3 (the deferred admission debt is closed); after T4 (two `instanceType`s produce
two groups); after T6 (a deployment is admitted as a unit across groups); after T9 (the cluster
agrees).

- [ ] **T1 · The identity fields, and the rule that selects them**
      Blocked by: None
      Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: one table naming the identity fields, with the criterion stated above it so that a
      later field has something to be judged against rather than a list to be appended to. A
      field-by-field comparison of the submitted `spec` against the stored one, performed **after
      defaulting**. A refusal naming the field path and saying that changing it describes a different
      deployment — **the message states the rule, not a locking concern**, and a test pins that
      wording, because the wrong reason sends a reader hunting for a conflict that does not exist.
      One case per refused field and one per editable field; one for a byte-identical re-apply; one
      for a metadata-only edit; one for an edit during deletion. The `ValidateUpdate` comment
      asserting nothing is immutable is replaced by the criterion.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/`

- [x] **T2 · The status kind enum, proved from the writer**
      **Delivered outside this spec, at `bb0ef941`.** The marker sits on the field rather than the
      type, and both halves of the verification are there: `TestModelDeploymentRoleKindEnumsCannotDiverge`
      compares the two enums so neither is a literal, and
      `TestModelDeploymentEffectiveRoleKindStaysInsideTheStatusEnum` drives the producing function
      over every value the spec enum admits plus the empty one a Go caller can build, and asserts
      each result is a value the status enum names. That second one is the acceptance's "sourced from
      the writer, not from the enum", and it is the half an enum change usually leaves out.
      **This entry stays rather than being deleted**, because a task removed once someone else did it
      reads later as a task that was never needed, and the next reader has no way to tell the two
      apart. What is recorded is where the work is, not that it was planned.

- [ ] **T3 · The two resource-mode admission rules**
      Blocked by: T1 — not a dependency of substance, a file one: both rewrite
      `pkg/worker/webhooks/worker/model_deployment.go` and its table-driven test, so they serialize
      Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: the mode-offered rule and the per-unit-ceiling rule, both reading the
      `InstanceType` through the path the defaulting half already uses. A refusal naming the mode and
      the type; a refusal naming the ceiling. A separate positive baseline per rule, because a
      request satisfying one can violate the other. The comment naming these as unwritten is removed.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/`

- [ ] **T4 · One pod group per `instanceType`**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go` + its test,
      `pkg/worker/controllers/worker/model_deployment_render.go` + its test — the group metadata is
      stamped onto the Pod there, so a task owning only the group file cannot make two groups reach
      two sets of Pods
      Gate: review
      Acceptance: a pure function returning the set of groups, keyed by `instanceType`. One group for
      a single-type deployment, rendering byte-identical to today's fixtures. Two groups for a
      two-type deployment, each with its own total counting only its own roles. Three roles across
      two types render two groups, not three. Group names unique per deployment and type, and stable
      across passes. The stale comment naming the withdrawn `roles[].acceleratorKey` is removed.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [ ] **T5 · Creating, pruning and reporting a set of groups**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/model_deployment.go` + its test,
      `pkg/worker/controllers/worker/model_deployment_status.go` + its test
      Gate: review
      Acceptance: one reconcile pass creates every group's every replica; a role moving between types
      rebuilds only the groups it leaves and joins; deleting the deployment removes every group's
      Workload, not only the first. Status reports per role as it does today, and the existing
      single-group cases pass unchanged.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [ ] **T6 · The joint-admission check**
      Blocked by: T5, and T8 — a file dependency, not a substantive one: both edit
      `pkg/worker/webhooks/worker/model_deployment.go`
      Owns: `pkg/worker/controllers/worker/model_deployment_joint_admission.go` (new) + its test,
      `pkg/worker/controllers/worker/node_queue.go` + its test — a queue is where a check is
      attached, so a new check absent from `spec.admissionChecksStrategy` reaches no Workload at all
      — and `pkg/worker/webhooks/worker/model_deployment.go` + its test, for the one refusal below
      Gate: review
      Acceptance: an AdmissionCheck claimed under this operator's controller name, applied at
      startup beside the node-devices one and referenced from **every operator-owned queue**, not
      only the accelerated ones — the node-devices reference is gated on `acceleratable` and on the
      cluster-wide derived-from-node setting, and borrowing that coverage would leave a CPU-pool
      group of a multi-group deployment ungated. Every Workload in those queues carries it, and it
      answers **Ready at once** for everything that is not a multi-group ModelDeployment — including
      workloads this operator did not create, which is the case that must be present or the check
      parks the cluster. For a multi-group deployment it answers Ready only when every role is
      feasible, and **Pending otherwise — never Retry**, because a Retry evicts and drops the
      reservation the barrier is made of. One infeasible role leaves every role's Pods gated while
      every role keeps its quota, asserted on the Pods and on the Workloads' `QuotaReserved`. The
      check never preempts, and **within this task it never rejects** — the rejection at the bound is
      T7's, and T7 is where that sentence stops being true of the finished check.
      **And where the barrier cannot be installed, the shape is refused rather than admitted
      unguarded**: with the derived-from-node setting off no queue carries a check, so a multi-group
      deployment is refused on create and on update with a message naming that setting. A positive
      case with the setting on is required beside it, or the refusal is indistinguishable from one
      that refuses every multi-group deployment.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [ ] **T7 · The infeasibility bound**
      Blocked by: T6
      Owns: `pkg/worker/controllers/worker/model_deployment_joint_admission.go` + its test,
      `pkg/worker/controllers/worker/model_deployment_status.go` + its test
      Gate: review
      Acceptance: a stated bound held in a constant with its reason in the comment; after it, the
      deployment reports a condition naming the role that cannot be placed and the **re-reservation
      cycle stops** — durable state that keeps the group from being rebuilt, not a Workload deletion,
      which only removes one turn of the loop. A set that becomes feasible before the bound is
      admitted normally and the condition never appears. The bound is exercised by a fake clock, not
      by waiting. **The condition's message names the action that clears the parked state**, because
      an identical re-apply does not: it bumps no `resourceVersion` and delivers no event, so an
      operator following "re-apply it" without being told what counts would watch nothing happen.
      **Two constraints this task inherits and must not break.** Status is rebuilt from observed state
      by one function, and that file says in as many words that anything a later task adds folds into
      it, because a second writer is free to leave its own field behind — so this condition is
      observed by the deployment's reconciler even though the bound is measured elsewhere. And the
      existing `QuotaReserved` vocabulary has no word for "deliberately parked": a complete group with
      no Workload currently reads as waiting for admission, which after the bound fires is the
      opposite of the truth.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [ ] **T8 · Prefill and decode do not contend for one card**
      Blocked by: T3
      Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: a `prefill`/`decode` pair both requesting a logical slice **from `instanceType`s
      that can select the same accelerator** is refused naming both roles and the field; the same
      pair over `instanceType`s with disjoint accelerator populations is accepted, and that positive
      case is required so the rule cannot land as a blanket refusal of every sliced pair; whole-card
      and partition-profile pairs are accepted, each with its own case and the partition case
      carrying the reason it differs; single-role deployments are unaffected across every
      combination. **F4's qualifier travels with this task** — implemented without it, T8 rejects the
      heterogeneous sliced shape that F2 exists to enable.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/`

- [ ] **T9 · The transport boundary, written down and pinned**
      Blocked by: T2 — a file dependency, not a substantive one: both edit
      `api/worker/v1alpha1/model_deployment.go`
      Owns: `api/worker/v1alpha1/model_deployment.go` (the connector comment),
      `pkg/worker/controllers/worker/model_deployment_connector_test.go` — the field's only possible
      reader is the controller's connector path, and `pkg/worker/kvcache/inject` never receives it;
      no non-test file in that package is touched, so this does not collide with T4 to T7
      Gate: review
      Acceptance: the connector field's comment states the convergence and what widening the enum
      would take. A test pins that the enum has one value, and a second pins that the field is
      **inert**: rendering the same deployment with `kvCache.connector` unset and with it set to its
      one legal value produces an identical connector, because the value the renderer receives is
      synthesized from the engine and the backend kind. **F5 says why the opposite test cannot be
      written today** — with one legal value a renderer that reads the field and one that ignores it
      are indistinguishable — so this task must not be read as asking for a dispatch assertion. No
      interface, no dispatch abstraction, no second sub-package.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [ ] **T10 · The reference page**
      Blocked by: T1, T3, T4, T8, T9
      Owns: `docs/reference/model-deployment.md`
      Gate: review
      Acceptance: the identity fields, the criterion that selects them, and what to do instead of
      editing one; the two grouping shapes and the blast radius of an edit in each, stated beside the
      editable fields so a reader meets the restart where they meet the field; the new refusals in
      the admission table; the transport convergence. The `## Contents` list stays in sync and the page stays inside its line cap.
      Verify: `make lint docs < /dev/null`

- [ ] **T11 · The cluster case**
      Blocked by: T7, T10
      Owns: two new e2e cases (numbers re-checked when the task starts, because they have collided
      before)
      Gate: review
      Acceptance: on a single-node cluster with no accelerator, a two-`instanceType` deployment
      produces two Workloads and no role is admitted while one is infeasible; the bound fires and the
      condition names the role; a frozen field is refused on a live object; a scale of one role in a
      two-group deployment leaves the other group's Pod identities unchanged. The case **creates**
      the second `InstanceType` rather than assuming the cluster has one.
      Verify: `bash .claude/skills/_e2e-lib/scripts/preflight.sh` then the case scripts against a
      deployed namespace

- [ ] **T12 · The card-separation measurement**
      Blocked by: T8, T11, and **two accelerator models or one multi-card machine**
      Owns: nothing in the tree until it runs; its result is recorded in this spec's Test Plan
      Gate: review
      Acceptance: a prefill replica and a decode replica of one deployment are observed to occupy
      different physical accelerators, read as **two non-empty device identities obtained
      independently from each Pod**. **The shape measured is the sliced pair over `instanceType`s
      with disjoint accelerator populations**, which is the shape where card separation is the
      property; F4 accepts a partitioned pair on one card, so a card-identity assertion is the wrong
      instrument there and is not run against it. The case **fails, rather than passes, when the environment
      cannot distinguish two cards** — comparing two empty strings is how this assertion becomes
      vacuously true, and it is the failure mode most likely to go unnoticed because it looks like a
      pass. **Nothing above depends on this task**, and no acceptance criterion above is written in
      terms of it.
      Verify: recorded by hand from the run

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- The test asserting that roles on differing `instanceType`s are refused must be **replaced** by one
  asserting the two-group rendering. Deleting it would remove the only coverage of that input shape.
- A **second `InstanceType` fixture** with a different queue entrance does not exist in the controller
  package and is what makes every two-group case expressible.
- A **fake clock** in the joint-admission tests, so T7's bound is exercised rather than waited on.
- The existing single-group pod-group fixtures become **regression baselines**: T4 asserts the
  one-type rendering is byte-identical to them.
- The e2e suite needs a way to create a second `InstanceType` in a no-accelerator cluster.

#### Unit tests

Table-driven coverage of every case below, measured with `go test -cover` when the tasks land; this
repository carries no coverage threshold and none is invented here.

**Every positive case in this plan passes against an implementation that does nothing, and that is
the defect to design against rather than a reason to drop them.** A table of refusals passes when the
validator refuses everything; a table of acceptances passes when it refuses nothing; and a new rule's
acceptance cases pass against today's code, which has no rule at all. So each table below is required
to hold **both directions of the same input** — the object that must be refused and the
minimally-different object that must be accepted — and a refusal case asserts the **typed field error
on the specific path**, never merely that admission failed, because an unrelated validation error
refuses the object just as convincingly.

- `pkg/worker/webhooks/worker`: `<date>` - `<coverage %>`
- `pkg/worker/controllers/worker`: `<date>` - `<coverage %>`
- `pkg/worker/kvcache/inject`: `<date>` - `<coverage %>`

**Immutability cases** (`pkg/worker/webhooks/worker`).

| Case | Condition | Expected |
|---|---|---|
| `identity_field_refused` | one case per identity field | reject, asserting the **typed error on that field's path**, so an unrelated validation failure cannot stand in for it |
| `refusal_states_the_rule` | any identity field | the message says a different deployment, **not** that writers would conflict |
| `run_field_accepted` | one case per editable field | accept |
| `identical_reapply` | byte-identical object re-applied | **accept** — the case that fails a whole-object comparison |
| `roles_reordered` | the same roles, serialized in a different order | **accept** — `roles` is a `listType=map`, so this changed nothing; the case that fails a positional comparison |
| `defaulted_field_unchanged` | second apply after the operator defaulted a field | accept; the comparison happens after defaulting |
| `metadata_only_edit` | a label added | accept |
| `edit_during_deletion` | finalizer removed on a deleting object | accept |
| `create_unaffected` | any object | the rule runs on update only |

**Pair and resource cases** (`pkg/worker/webhooks/worker`).

| Case | Condition | Expected |
|---|---|---|
| `pd_both_sliced` | prefill and decode both request a slice, types over one accelerator population | reject; names both roles |
| `pd_sliced_disjoint_types` | the same pair, types whose accelerator populations do not overlap | **accept** — the case that fails a blanket refusal, and the shape F2 exists to enable |
| `pd_sliced_sibling_types` | the same pair, two differently named types over one `acceleratorGroup` | **reject** — the case that fails a name-inequality predicate |
| `pd_whole_cards` | both request whole cards | accept |
| `pd_partitioned` | both name a partition profile | accept |
| `single_role_sliced` | one `server` role, sliced | accept — the rule is about a pair |
| `mode_not_offered` | a mode the type does not offer | reject; names mode and type |
| `mode_offered_over_ceiling` | offered mode, request over the ceiling | reject; names the ceiling |
| `mode_offered_within_ceiling` | both satisfied | accept |
| `type_detail_not_synced` | a slice or partition request against a type whose `status.detail` is empty | a **transient** error, not a typed field error — the case that fails an implementation reading an empty detail as "offers no modes" |

**Grouping cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `one_type_one_group` | every role on one type | one group, byte-identical to today's fixture. **A regression baseline, not evidence of the feature** — it passes against today's code by construction, which is the point |
| `two_types_two_groups` | prefill and decode on two types | two groups, two queue names |
| `three_roles_two_types` | two roles share a type | **two** groups, not three |
| `totals_are_per_group` | 2 + 3 across two types | totals 2 and 3, not 5 |
| `group_names_unique` | two groups of one deployment | distinct, stable across passes |
| `role_moves_type` | a role's type edited | only the groups it leaves and joins rebuild. **Reachable only below the webhook** — F1 freezes `instanceType`, so this is a renderer-level case and is labelled one; it is kept because the reconciler must still converge if the field ever changes, and deleting it would leave that path untested |
| `scale_isolated_across_groups` | scale decode in a two-group deployment | prefill Pod identities unchanged |
| `scale_rebuilds_within_group` | scale decode in a one-group deployment | both roles rebuilt |

**Joint-admission cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `check_on_every_workload` | two groups | both carry the check |
| `single_group_ready_at_once` | one group | the check is carried — the queue attaches it — and answers Ready immediately; Kueue's own rule already covers the group |
| `foreign_workload_ready_at_once` | a Workload this operator did not create, in the same queue | Ready immediately — the case that fails a controller answering only for multi-group deployments |
| `one_infeasible_holds_all` | one role infeasible | no role's Pods ungated |
| `all_feasible_admits` | every role feasible | Ready on every Workload |
| `pending_before_the_bound` | an infeasible set, clock short of the bound | **Pending**, never Retry and never a rejection, **and the Workload still reads `QuotaReserved`** — the assertion that fails a Retry-based implementation, which would look the same on the Pods |
| `multi_group_refused_without_coverage` | a multi-group deployment, derived-from-node setting off | refused, naming the setting; the same deployment with the setting on is accepted |
| `bound_fires` | infeasible past the bound, fake clock | **Rejected**, Kueue deactivates the Workload, and the deployment's condition names the role and the action that clears it |
| `bound_not_reached` | becomes feasible first | admitted; condition never appears |

**Enum and transport cases.**

| Case | Condition | Expected |
|---|---|---|
| `status_kind_enum_covers_writer` | every value the producing function returns | each is in the enum |
| `status_kind_enum_on_field` | the generated CRD | the enum is on the field, not only the Go type |
| `connector_enum_single_value` | the connector enum | exactly one value |
| `connector_is_inert` | the same deployment with the field unset and with it set to its one legal value | an identical rendered connector — the value comes from the engine and the backend kind, and the day that stops being true this case says so |

#### Integration tests

This repository has no envtest; these drive whole reconcile passes against the controller-runtime
fake client in the same packages. Concrete names are added after the implementation pull request
merges.

- **The two-group pass**: one reconcile creates every group's every replica, and a second pass over
  an unchanged spec writes nothing.
- **Teardown across groups**: deleting the deployment removes every group's Workload, asserted for
  each rather than for the first.
- **The check's lifecycle**: applied at startup, carried by the right Workloads, cleared when the set
  becomes feasible.

#### e2e tests

Run against a local single-node cluster with the operator deployed. No accelerator.

- **The two-group case.** A deployment across two `InstanceType`s the case creates itself; two
  Workloads; **exactly one role made infeasible while the other would be admitted on its own** — with
  both infeasible, "no role is admitted" is true whether or not the joint check exists, and the case
  would prove nothing. The bound fires and the condition names the role. A scale of one role leaves
  the other group's Pods untouched, asserted against **Pod UIDs captured before the scale**, which
  also requires the case to have admitted Pods in the first place.
- **The frozen-field case.** A live deployment refuses an edit to a frozen field with the expected
  message, and accepts a `replicas` edit.
- **Not covered, and stated so:** that the two roles do not contend for one card. That is T12, it
  needs hardware, and no cluster case substitutes for it.

## Alternatives

**Relax the instanceType rule without replacing atomicity.** Rejected where it was first proposed and
still rejected: Kueue has no all-or-nothing primitive between two Workloads, so the guarantee would
simply disappear, leaving prefillers holding accelerators and serving nothing.

**A manufacturer-level ClusterQueue with per-role model selection inside it.** Previously chosen and
then overturned. It reaches same-manufacturer cross-model only, and it changes every pool's
behaviour; per-role queues reach both cross-model and cross-manufacturer and leave pools alone.

**An operator-run two-phase commit instead of an AdmissionCheck.** Rejected: it reimplements the
reserved-but-not-running state that Kueue already holds, in a controller that would have to be
correct across restarts. The check is the mechanism this repository already runs.

**A revision object, so a frozen spec can still roll forward.** Not taken here. It is the right shape
for a zero-downtime image change and it is a larger design than this spec. An image change is
already possible without it — F1 leaves the image editable — so what a revision object would buy is
the absence of the restart F9 describes, not the ability to upgrade.

**Build the transport plugin point now.** Rejected twice already in this repository, once at the API
and once at the package boundary, both times on the same ground: an abstraction drawn against a
single implementation is drawn in the wrong place. The decision is recorded and the price of
re-opening it is stated instead.

## Open Questions

**Settled: an infeasible deployment stops, and does not start itself again.** This was carried as an
open question — what a deployment that never becomes feasible finally looks like — and it is now
decided. At the bound the joint-admission check answers **Rejected**, Kueue deactivates the Workload
itself, and the deployment reports a condition naming the role that could not be placed.

**The cost was accepted rather than overlooked, and it is recorded in the words it was accepted in:**

> the deployment does not come back on its own when capacity frees up later — somebody has to
> re-apply it.

That sentence is here because the alternative is a later reader finding no self-healing, reading it
as a gap, and building the thing that was turned down: a cool-off timer that re-activates the
Workload. That candidate existed, it was the one weighed against this, and it lost because two
actors writing `spec.active` can race.

**What "re-apply" has to mean, because an identical one is not enough.** An apply that changes no
byte bumps no `resourceVersion` and delivers no watch event, so nothing observes it and the parked
state is never reached — and Kueue does not reactivate a Workload it deactivated just because the
parent was submitted again. The action that clears the parked state is therefore an edit to one of
the fields F1 leaves editable, or deleting and recreating the object. T7 owns putting that action in
the condition's message, so an operator reads it rather than infers it. The accepted cost is
unchanged by saying this precisely: the deployment still does not restart itself.

**What happens without the bound, and why it is not optional.** A check that answers Pending holds
its reservation by design — that is exactly what makes the barrier work — so with no bound an
infeasible deployment sits on quota it will never use, indefinitely, while `QuotaReserved` reads
true and nothing reaches a final state. "Is this broken or still starting" then has no answer, and
the quota is really gone rather than intermittently taken. The bound is the price of choosing
Pending, and this is the behaviour it replaces.

**The bound is fifteen minutes, and the number is an engineering judgement rather than a product
one.** The criterion it has to meet is that a deployment waiting for capacity **that is already on
its way** must not be parked: a cluster that provisions a node in response to a pending workload
needs the node to come up and register before the bound expires, or autoscaling and this feature
fight each other. Fifteen minutes is chosen against that criterion and is roughly thirty turns of the
eviction cycle. It is a starting value: the constant carries this reasoning in its comment so that a
measured provisioning window can move it without anyone having to re-derive why it exists.

**OQ1 — does a two-group deployment still deserve one status?** Every figure in `status.roles[]` is
per role today and stays correct. What has no home is a group-level fact — which group is admitted,
which is waiting.

**This is worth more than it first looked, because during a joint-admission wait the existing status
actively misleads.** `QuotaReserved` is read from the Workload's reservation condition, which Kueue
sets at reservation and therefore before the checks complete. So while nothing runs, the first
condition a reader checks says quota is reserved — and on the eviction cycle it flaps rather than
sitting still. The bound's condition only appears when the wait *ends*, so for the whole window the
objects say the opposite of the truth. Not adding a group-level fact leaves the answer only in
Kueue's own objects. **It blocks nothing and T5 can render either, but it is the difference between a
diagnosable hold and an invisible one.**
