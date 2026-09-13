# Spec: ModelDeployment role-kind readiness condition

Status: Shipped

It is not a bug-fix spec: it adds a status axis that did not exist, which is why it belongs here
rather than being recorded only in the change that made it.

## Summary

A `ModelDeployment` can declare roles of different kinds — `prefill` and `decode`, or one or more
`server` roles. Today nothing in the status answers **which kinds currently have a ready replica**.
The one summary field, `status.phase`, sums every role's counts together before judging, so "two of
three replicas of one role are ready" and "an entire kind has no ready replica at all" produce the
same value, `Degraded`, with the same shape of message.

This spec adds ONE condition that reports role-kind readiness, and changes nothing else.

### The scope criterion

Everything in this spec, and everything excluded from it, follows from one rule:

> **An item belongs to this spec only if it can be computed from the fields the
> `ModelDeployment`'s own status already carries.**

The criterion is generative rather than a list, so it also decides items nobody enumerated. Three
exclusions fall out of it directly, and they are corollaries rather than separate prohibitions:

1. **Whether a `prefill` process can serve a request on its own** is a property of the inference
   engine. It is not in this status, so this spec does not depend on it. See "Immunity to the engine
   question".
2. **Whether traffic can reach a role** is a property of the Services and their endpoints. It is not
   in this status, so this spec reports readiness and NEVER claims availability.
3. **Which workload reclaimed a group's quota** requires reading a Kueue `Workload`. It is not in
   this status, and the state that names it is not durable — see "Inputs that are FORBIDDEN".

### The criterion is pinned to a field list, not to the word "status"

"The fields the status already carries" is a set that grows. A future field would silently widen this
spec's scope, and nothing would report that widening. So the criterion is pinned to THESE fields of
`ModelDeploymentRoleStatus`, as they exist when this spec is written:

`Name`, `Desired`, `Ready`, `Unmanaged`, `Kind`, `AssignedFlavor`

Of these the condition reads exactly two: `Kind` and `Ready`. The other four are dispositions rather
than omissions, and they are written down because the difference between the two is invisible later:

- `Desired` is NOT read. Comparing against it is replica completeness, excluded by F1's threshold.
- `Unmanaged` is NOT read, and that is a decision with a stated cost. See its own section.
- `AssignedFlavor` is NOT read. It would be the only route by which group completeness could re-enter
  this status, so it has its own section establishing that it cannot carry that.
- `Name` is NOT read. The condition's message names kinds, never roles, because a kind is what the
  answer is about and a role name would suggest the missing thing is one particular role.

A later field does not extend this spec by existing. Extending it is a decision someone makes.

## Motivation

A disaggregated deployment is not a uniform pool of replicas. A `decode` role with every replica
ready and a `prefill` role with none is a materially different situation from both halves being
partly up, and the two need to be told apart by something an alert rule can branch on.

`status.phase` cannot carry the distinction, and not by oversight: it is one string summarising four
independent axes, and its own API comment says so. The finer view is the condition list, whose
comment states the design directly — one condition per axis, because that is what a single phase
string cannot carry. Role-kind readiness is such an axis and has no condition.

### What exists today, exactly

- `status.phase` reports `Starting`, `Ready`, `Degraded`, `Deleting`. `Degraded` is reached when the
  summed ready count is above zero and below the summed desired count.
- `status.conditions` carries four types: `DomainRegistered`, `QuotaReserved`, `CacheAttached`,
  `ReplicasUpToDate`.
- `status.roles[]` carries, per role, the six fields listed above. `Kind` is required and enumerated,
  and its API comment states its purpose: reading the status alone answers which half of a
  disaggregated deployment an entry describes.

So the inputs this spec needs are already published. Nothing new is observed; something already
observed is summarised along an axis nothing summarises today.

### User Stories

**An SRE is paged for a deployment reporting `Degraded`.** Today they open the object and compare
per-role counts by hand to learn whether this is "one replica short" or "an entire half is down".
With this condition the object answers it: the condition is False and its reason names the kinds that
have no ready replica.

**A platform team writes an alert rule.** They want to page on "a kind of this deployment has nothing
ready" and not on "this deployment is one replica short during a rollout". Today the only stable
signal is `phase=Degraded`, which fires for both. With this condition they branch on a condition type
and a reason, neither of which is a sentence they have to substring-match.

**An operator runs an ordinary single-role deployment.** Nothing they read changes. `phase` reports
exactly what it reported before, and the new condition degenerates to "is anything of mine ready",
which is true precisely when at least one replica is ready.

**A maintainer reads the status code a year from now.** They find the condition declared beside the
code that writes it, like every other condition in this object, and they find written down why
role-kind readiness is not the same question as replica completeness.

## Proposal

### F1 — One new condition reporting role-kind readiness

**The rule is that a deployment's status states, as a machine-readable classification, whether every
kind it declares has at least one ready replica.**

- The condition is True when EVERY distinct `status.roles[].Kind` present on the object has at least
  one role entry with `Ready > 0`.
- The condition is False when at least one distinct kind has no role entry with `Ready > 0`. The
  reason classifies it; the message names the kinds.
- The condition is Unknown when `status.roles` is empty. Absence of an answer is reported as absence,
  never as False.

**A deployment with no replicas yet reports False, not Unknown**, and the difference is what was
observed rather than how much of it. The per-role counts are counted from a Pod list that SUCCEEDED,
so a zero there is an observed zero — the API says so on the field. "No replica of any kind is ready"
is therefore an observation. Unknown is reserved for the narrower case of a pass that accounted for
no role at all, which is a different input and carries a different reason.

The condition type is `RoleKindsReady`, and its reasons are `AllKindsReady`, `KindsNotReady` and
`NoRoleStatuses`.

**At least one, NEVER all.** The threshold is deliberately "a kind has some ready replica" and not
"a kind has all its ready replicas". The second question is replica completeness, which `phase`
already answers; asking it here would make the two fields answer the same question in two places.
See "Orthogonality" for why that matters more than it looks.

**Acceptance.** A deployment with a `prefill` role at 0/1 ready and a `decode` role at 1/1 ready
reports the condition False with a reason that classifies it and a message naming `prefill`. The same
deployment with both roles at 1/1 reports True. A deployment whose roles are all `server`, one of
them at 1/2 and another at 2/2, reports True, because every kind present has a ready replica.

### F2 — `status.phase` is NEVER touched

**The rule is that this spec changes no byte of `phase` or of the function deriving it.**

This has its own feature entry because it is the thing most likely to be widened later by someone
who reads only the summary. The condition is additive. `deriveModelDeploymentPhase` keeps summing
role counts, keeps reporting `Degraded` on a partial sum, and keeps its existing message.

**Acceptance.** For a fixed set of role counts, `phase` and `phaseMessage` are byte-identical before
and after this change. The test that carries this REQUIRES a multi-kind fixture: a single-kind
fixture would pass even if the derivation had been rewritten to branch on kind, because a single-kind
deployment cannot distinguish the two rules.

### F3 — Single-kind deployments behave as they do today

**The rule is that a deployment whose roles all share one kind sees no change in any field it reads
today, and a well-defined value in the new one.**

The default kind is `server`, and the webhook refuses only MIXING a `server` role with the other
kinds — two `server` roles are accepted. So the single-kind shape is the common shape, and it is the
one where a regression would be widest.

**Acceptance.** Two falsifiable checks, and the second is the one with information in it:

1. For a single-kind deployment, `phase` and `phaseMessage` are byte-identical before and after.
2. For a single-kind deployment with two roles at 0/1 and 1/1, the new condition is True — because
   the one kind present does have a ready replica. A rule that had been written as "every ROLE has a
   ready replica" instead of "every KIND does" reports False here, so this case fails on the wrong
   rule and passes on the right one.

**This second check was run against the wrong rule before being written down here, and the result is
stronger than the claim.** Both rules were implemented standalone and evaluated over four fixtures:

| Fixture | "every kind" | "every role" | Tells them apart |
| --- | --- | --- | --- |
| Single-kind, two roles at 0/1 and 1/1 | True | False | YES |
| Multi-kind, prefill at zero, decode ready | False | False | no |
| Multi-kind, both ready | True | True | no |
| Single-kind, all ready | True | True | no |

**Only the single-kind case discriminates.** The two multi-kind fixtures — the obvious ones to write
for a feature about disaggregated deployments — agree under both rules, so a test suite made of them
passes with the wrong implementation and reports nothing.

That is why this check is REQUIRED rather than an extra: it is the only one of the four that can
fail. A reviewer who asks why a single-kind case is load-bearing for a multi-kind feature should be
shown this table.

### F4 — The reason is the classification, the message is not a contract

**The rule is that anything a machine branches on is the condition type and its reason.**

This repository has already settled this question once: a reason is the machine-readable
classification, and substring-matching a sentence is not a stable interface. The message names which
kinds are affected, for a human; the reason says which situation this is, for a rule.

**Acceptance.** The reason values are a closed, named set declared as constants, and a test asserts
the set. The message is asserted for content, never used as the discriminator in production code.

### Tasks

- [x] **Task 1:** Declare the condition type and its reason constants beside the code that writes
      them, following this object's existing convention (see "Where the constant goes").
- [x] **Task 2:** Implement the predicate over `status.roles[].{Kind, Ready}`, returning the set of
      kinds with no ready replica. Pure: no client, no clock.
- [x] **Task 3:** Write the condition in the status build, from the role statuses computed earlier in
      the same pass, so it cannot disagree with the counts published beside it.
- [x] **Task 4:** Table-driven tests for F1, including the multi-kind and single-kind cases named in
      F3's second acceptance check. Every multi-kind fixture REQUIRES an explicitly set `Kind`; see
      "A role's name does not carry its kind" for why reusing the existing helpers silently produces
      a single-kind fixture.
- [x] **Task 5:** The F2 regression test, on a multi-kind fixture.
- [x] **Task 6:** Document the condition in the reference page's condition table, including its
      reason values.

## Design Details

### Orthogonality: why adding this condition does NOT require changing `phase`

There are three independent ways a multi-group deployment can be short of what was asked for. Naming
them separately is what makes the rest of this spec decidable:

| Axis | Question | Answered today by |
| --- | --- | --- |
| Kind completeness | Does every declared kind have a ready replica? | Nothing |
| Group completeness | Does every pod group have ready replicas? | Nothing |
| Replica completeness | Does every role have all its replicas ready? | `phase` |

`phase` answers replica completeness. This condition answers kind completeness. They are different
questions over the same numbers, so the new one can be added without disturbing the old one.

This is a stronger justification than "the change was scoped that way", because it says WHY `phase`
may be left alone rather than WHO decided to leave it alone. A reader who disagrees can attack the
argument; nobody can attack a decision.

**The replica-completeness axis still has a defect, and this spec does not fix it.** `phase` sums
across roles before judging, so it cannot distinguish "one role is short" from "one kind is empty",
and its own comment asserts that a `Degraded` deployment "is serving" — which is not established for
every shape. That is recorded here so the next person reading this spec does not conclude the axis
was cleaned up. It was surveyed and left.

### Division of labour with `phase`, and the gap that neither covers

The condition is deliberately quiet on the commonest shape, and a reader who does not expect that will
conclude it is broken. So it is stated here.

Take the shape the defaults produce: two roles, neither naming a kind, on two different
`instanceType`s. Both roles are effectively `server`, so there is ONE kind and TWO pod groups. One
group loses every replica:

| Signal | What it reports | Why |
| --- | --- | --- |
| This condition | True, "every kind has a ready replica" | The single kind `server` does still have ready replicas, in the surviving group |
| `phase` | `Degraded` | The summed ready count is below the summed desired count |

So on the default shape the degradation is carried by `phase`, and this condition correctly says
nothing. On a multi-kind shape where one kind empties, `phase` also reports `Degraded` but cannot say
which half is gone, and this condition is what says it. The two are complementary rather than
redundant, which is the same orthogonality the previous section argues from.

**The gap this exposes is group completeness, and it is REAL, default-reachable, and deliberately
not covered here.** Consider a deployment with two `prefill` groups and one `decode` group, and one
prefill group at zero. This condition reports True, because the `prefill` kind still has ready
replicas elsewhere. `phase` reports `Degraded`, because replicas are missing. Between them the
operator learns that something is short, and NEITHER names the fact that an entire group is gone.

That is axis two. It stays out of scope, and the reason is now stronger than "the criterion excludes
it":

- It is not hypothetical. Two roles on two `instanceType`s is accepted by both the schema and the
  webhook, and an existing admission test asserts that acceptance.
- It is not rare. It is what the defaults produce, because a role that names no kind is a `server`.
- It is therefore a KNOWN, reachable gap that this spec chooses not to fill, rather than a case
  nobody thought of.

Recording it this way matters more than where it lands: a gap left unrecorded reads, to the next
person, as a gap that was surveyed and found not to exist.

### Immunity to the engine question

Whether a `prefill` process can answer a complete request by itself is not settled. In this
repository, the only difference between how a prefill container and a decode container are rendered
is one string inside the connector's transfer configuration, which selects the direction that
container uses against the KV store; the start-up command is identical and takes no role, and the
readiness probes do not branch on kind at all.

That is evidence about what this operator renders. It is NOT a finding about engine behaviour, and
this repository records no such finding.

**This spec does not need one.** The condition reports WHICH KINDS have ready replicas, not whether
the deployment is serving. Both possible answers to the engine question leave that report correct;
they change only how an operator should interpret "only prefill is ready" — which is documentation
and alert-threshold work, not field shape.

Stating the condition as a boolean "is it serving" would import the unresolved question into the
field itself. That is the shape to avoid, and it is the most likely simplification a later reader
would make, which is why the reason is written here rather than only in a review thread.

### Inputs that are FORBIDDEN, and why

**The `PreemptedInPart` state, or any other Kueue `Workload` condition, is NEVER an input.**

Two reasons, either sufficient:

1. It is outside the criterion: it requires reading another object.
2. It is not durable. When a higher-priority workload preempts a group, Kueue deletes that group's
   Pods; those Pods carry a Kueue finalizer and cannot leave; this operator's converge loop sees a
   departing replica, rebuilds the whole group, and DELETES that group's Workload as the only way to
   release the finalizer. The preemption conditions go with the object. A condition computed from
   them would flap on a reconcile timescale while the situation it describes persists.

The second reason is the one to keep, because it survives any later widening of the criterion.

### Whether `AssignedFlavor` re-admits group completeness through the back door

`AssignedFlavor` is in the pinned field list, and a ResourceFlavor is closer to hardware than a role
is. If it identified a role's pod group, then group completeness would be partly computable from this
status and the criterion would not exclude it on its own strength.

It does not identify the group. Three independent reasons:

1. **It is empty in exactly the state of interest.** It is read from the group Workload's per-PodSet
   assignment and is nil when there is no admission, when the PodSet is not named, and when the
   assignment is ambiguous. A group with no ready replicas is usually a group with no admission, so
   the field is nil precisely where group completeness would need it to speak.
2. **One group can report several flavors.** Roles sharing one `instanceType` are one group and one
   Workload with one PodSet per role, and flavors are assigned per PodSet from a ClusterQueue that
   may cover many node flavors. Two roles of the same group can be assigned different flavors, so the
   field's partition of the roles is not the group partition.
3. **It is nil for every CPU-only deployment.** The read only considers assignments keyed by an
   accelerator credits resource, so a deployment on a CPU-only pool reports nothing here at all.

So group completeness stays outside on the criterion, with no substitute reason needed. Reason 1 is
the one to cite: it is about the state the field would have to describe, not about how the field
happens to be spelled today.

### What `Ready` means for a role that took over its command line

`Unmanaged` is one of the six pinned fields, so by this spec's own criterion the interaction between
it and the condition is IN SCOPE and has to be decided rather than inherited.

An unmanaged role is one that replaced the whole command line, so the operator synthesized no engine
argument for it. The renderer only attaches startup, readiness and liveness probes to a role whose
argv it built; a take-over role therefore gets NO readiness probe, and a Pod with no readiness probe
is Ready as soon as its containers start.

So `Ready > 0` is a weaker statement for an unmanaged role than for a managed one: it says the
container is running, not that anything answered on the serving path.

**The decision is that an unmanaged role's ready replicas DO count toward its kind.** Two reasons:

1. The operator did not build that command line and has nothing better to judge it by. Discounting it
   would mean this condition asserting something about a container it knows nothing about.
2. The alternative makes an all-unmanaged deployment report False forever, which is a worse answer
   than a weaker true one.

**What this costs is stated rather than hidden:** for a kind whose only ready replicas are unmanaged,
this condition's True rests on kubelet's container-start signal alone. `status.roles[].Unmanaged` is
already published per role, so a consumer that needs the stronger reading can apply it; this condition
does not fold that judgement in, because folding it in would put a decision about the user's own
command line inside an operator-owned field.

### A role's name does not carry its kind

A test fixture that names its roles `prefill` and `decode` is not a prefill/decode fixture. `Kind` is
a separate field, it defaults to `server`, and the resolution of an unset value happens in Go as well
as in the schema — so a role built in a test with no `Kind` set is a `server` role whatever it is
called.

This is not a hypothetical. The existing admission tests build roles through a helper that sets a
name, a replica count and an instance type and NEVER sets `Kind`, and at least two cases use that
helper with the names `prefill` and `decode`. Read by name they look like disaggregated coverage.
They are single-kind cases.

The consequence for this spec is specific and severe: a fixture built that way lands exactly on the
shape where this condition is correctly silent (see the previous section). A multi-kind test written
on it would report True for every arrangement, and pass — for the wrong reason, with no symptom.

**So every fixture for this condition sets `Kind` explicitly, and at least one test asserts that a
fixture it believes to be multi-kind really carries two distinct kinds.** The second half is the part
that survives someone later refactoring the helpers.

### Where the constant goes

This object's convention is that a condition type is declared in the file that WRITES it, so its
reason vocabulary sits with the code that can observe it. All four existing conditions follow it:
`DomainRegistered`, `CacheAttached` and `ReplicasUpToDate` are declared beside their own reconciler
code, and `QuotaReserved` is declared in the status file because that is where it is written.

`QuotaReserved` is therefore NOT an exception to the convention. It is the convention applied to a
condition whose observation happens to live in the status file. This spec's condition is the same
case — its inputs are the role statuses computed in that same pass — so it is declared there too, by
the rule rather than despite it.

### Relationship to the open bug about the partial-preemption message

A separate open issue records five places asserting that a partially preempted deployment "cannot
serve" or holds accelerators "the deployment cannot use". Those assertions are false for a shape the
defaults produce: two `server` roles on two instance types, one group preempted, the other still
serving.

**This spec changes none of them.** That issue has its own enumerated set of five carriers, and its
own list of what does not count as closing it; adding a sixth location or fixing a subset here would
blur a denominator whose whole value is that it is fixed.

What this spec does is give those five a correct thing to say. Once role-kind readiness is computed,
each of them can state the fact the predicate already establishes — whether the preempted groups left
some kind with no ready replica — instead of asserting a blanket "cannot serve". The wording of the
"no kind left" branch still depends on the engine question above; the wording of the other branch does
not.

### What was actually run

The discrimination claim above is not left as a claim. The wrong rule was implemented in place of the
right one and the suite was run against it: the content change was confirmed by comparing the file
rather than a diffstat, the build succeeded so the failures are behavioural rather than a compile
error, and six cases went red — every one of them a single-kind case, with both multi-kind cases
still passing. Restoring the implementation returned the package to green.

The same falsification is also ENCODED as a test rather than left as a one-off run: it implements the
per-role rule beside the real one and asserts that the single-kind fixture separates them while the
multi-kind fixtures do not. A later refactor that destroys the discrimination fails there, instead of
leaving the table above true only of the day it was written.

`make lint` and `make lint docs` both return zero, and neither edited the files — checked by hashing
them before and after rather than by reading a diffstat.

## Alternatives

**Change `phase`'s derivation to judge per kind.** Rejected: it changes the meaning of a field that
is already published and already read, in order to answer a question a new condition answers without
disturbing anything. The orthogonality argument above is why the cheaper option is also the correct
one rather than merely the smaller one.

**Add a new `phase` value.** Rejected: `phase` is the field a human reads first, and every consumer
switching on it silently loses a branch when a value is added. The failure is quiet and appears at
each consumer rather than here.

**Carry the distinction in `phaseMessage` only.** Rejected on an already-settled principle: a message
is not a contract, and a rule that has to substring-match a sentence is not a rule anybody can
maintain.

**Publish nothing and let consumers compute it from `status.roles[]`.** Rejected: which combination
of kinds constitutes a working deployment is knowledge this operator has and a consumer does not.
Pushing the computation out also pushes out the unresolved engine question, to every consumer
separately.

## Open Questions

**The condition's name — SETTLED as `RoleKindsReady`.** The candidates were weighed by which
misreading each one invites, not by which is most descriptive. `RoleKindsReady` invites "all replicas
are ready", which is replica completeness — the one misreading two sections of this spec already
argue against, and the one the condition's own doc comment opens by denying. The alternatives each
opened a direction with no section behind it: `KindsRepresented` invites "every kind is declared",
`ShapeComplete` invites group completeness, and `AllKindsServing` writes the unresolved engine
question into the name itself.

**Whether group completeness deserves its own condition later.** It is a real axis, nothing answers
it, and it is reachable from the defaults today — the shape is accepted by both the schema and the
webhook, and an existing admission test asserts that acceptance. It is out of scope here because it
cannot be computed from this status, NOT because it is rare and NOT because it does not matter. It
should be reopened on its own terms, with its own input — the grouping key is `instanceType`, which
this status does not publish, so answering it means deciding whether to publish it.

**Whether a `prefill`-only deployment is serving.** An engine question, stated above. It changes
documentation and alert thresholds, and it changes the wording of one branch of the message in the
related bug. It does not change anything this spec specifies.
