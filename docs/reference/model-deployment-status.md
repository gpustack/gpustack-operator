# Model Deployment Status Reference

> **Purpose** — what each `ModelDeployment` status condition and published field means, and how to
> read them when a deployment misbehaves.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~8 min

## Contents

- [Status](#status)

## Status

`status.phase` is the field to read first: `Starting`, `Ready`, `Degraded` or `Deleting`. `Degraded`
means some required serving capacity is ready and some is not: either some role replicas are down, or
all role replicas are ready but a declared router has no ready replica.
`status.roles[]` carries `name`, `kind`, `desired`, `ready`, `quotaReserved`, `unmanaged` and
`assignedFlavors` per role.

`kubectl get modeldeployments` shows `status.roleSummary` in its `ROLES` column. It counts current
Ready instances by kind: `R` is managed router Pods, `S` is ordinary servers, `P` is prefillers,
and `D` is decoders. For example, `1R2S` means one Ready router and two Ready servers; `1R3P4D`
means one Ready router, three Ready prefillers and four Ready decoders.

Without a router, the `R` part is absent; a declared kind with nothing Ready shows zero. Several
roles of the same kind are summed. A serving instance made of several Pods still counts once; use
`status.roles[].desired` to see the requested count.

Without a router, `status.endpoint` is the address the deployment-wide Service serves on, in the form
`<scheme>://<name>.<namespace>.svc:<port>`. The scheme is read from the same role the port is — the
first one — and is `https` only where that role passes `--ssl-certfile` or `--ssl-keyfile`. With a
router it is absent until the router is ready, then uses the router Service's own `http` transport.

`status.router` publishes the contract used to render the router: its implementation and address,
the cache pool's client endpoint, the metrics names and port, and one entry for every role containing
the effective kind, selector, direct endpoint and any dialable KV-event endpoints. The event entries
are absent when the corresponding rendered Pods do not carry publisher configuration.

This router contract names metrics sources for routing; it is not a live utilization sample.
The separate [Model Deployment Metrics](model-deployment-metrics.md) subresource reads the current
router and engine Pod endpoints and reports partial coverage explicitly.

> **Why** — either flag alone is enough, and the rest of the `--ssl-*` family is not enough. Both
> engines hand every ssl argument to uvicorn, which turns on TLS for those two and for nothing else,
> so `--ssl-ca-certs`, `--ssl-ciphers`, `--ssl-keyfile-password` or `--ssl-cert-reqs` passed by
> itself leaves an ordinary HTTP server.

**`ready` counts replicas whose engine answered, not replicas whose process started** — for every
role except the three shapes listed below, which carry no gates and keep the weaker meaning. One
still loading its model counts as not ready for as long as that takes.

**Both figures count replicas, which is the same as counting Pods only while a replica is one Pod.**
A role at `size: 2` with `replicas: 2` runs four Pods and reports `desired: 2`. A replica of several
Pods is ready only when **every** member it declares is, so a role showing `1/2` with three of its
four Pods running is reporting one whole instance, not three quarters of the role.

A gated replica carries a startup gate, a readiness gate and a liveness gate, all reading the
engine's own `GET /health` on the port the Service targets, so `ready == desired` and "the endpoint
answers" are one fact rather than two.

The KV cache store's leader answers a route of the same name that is deliberately **not** a readiness
signal — see [KV Cache Leader](../kv-cache/leader.md). They are different programs, and only the
engine's route is read here.

**The startup gate allows thirty minutes per attempt, not in total.** A replica that has not
answered by then is restarted and gets the same budget again, so a model that never finishes loading
is a restart loop rather than a replica stuck at not-ready.

**The liveness gate never sees that window**, because the kubelet suppresses liveness and readiness
alike until the startup gate succeeds.

⛔ **A replica that stops answering loses readiness first, and is restarted only if it stays quiet
for longer.** The liveness gate's failure threshold is wider than the readiness gate's.

> **Why** — the two cost different things: losing readiness withdraws a replica from the Service and
> is undone by answering again, while a restart throws away a model that took the startup budget to
> load. Equal thresholds would collapse them into one event. And without the liveness gate such a
> replica has no way back at all, since this operator deletes a replica only to shed one the spec no
> longer declares, or to turn over an outdated one — never because it went quiet.

**The operator tells the engine where to listen.** It renders `--host 0.0.0.0` and `--port <the port
the Service targets>` into the engine's own command line, so the address traffic is sent to and the
address the engine opens are one decision. The two engines do not agree on a default — vLLM opens
every interface on 8000, SGLang opens `127.0.0.1:30000` — and an engine left on its own would be
unreachable on one of them.

⛔ **Both flags are filled, not owned.** A role that passes either through `extraArgs` keeps its own
value, and the operator adds only the one that is missing.

**A role that enables TLS is still gated**, over HTTPS, on the criterion above. Only the transport
moved — the address is still the operator's own — and the kubelet does not verify the server
certificate on a probe, so a self-signed pair is graded like any other.

⛔ **Three shapes carry no gates at all**: a role that replaces the command through
`roles[].command`; a role that moves where its engine listens, with `--host` or `--port`, or that
demands a client certificate a probe has none to present, with `--ssl-cert-reqs` set to anything but
`0` or `1` **alongside a certificate or key**; and a role declaring its port as `UDP` or `SCTP`.

> **Why** the value — that flag is an integer, and only `0` (`CERT_NONE`, the default) and `1`
> (`CERT_OPTIONAL`) let a certificate-less probe through, so a role passing either keeps all three
> gates. Everything else withdraws them: `2`, a value outside that enum, one that is not a number,
> and the flag with no value at all. Losing a gate costs a signal, while fitting one to a listener
> that refuses it restarts a replica that is serving.

The first two fail one way and the third the other. Gating an address the operator does not know
would restart a replica answering every request; an HTTP engine speaks TCP whatever the declaration
says, so a gate would **pass** while the published endpoint forwards a protocol nothing answers.
Replicas of all three are Ready as soon as their process starts.

`quotaReserved` is how many of the role's replicas hold a quota reservation. Each replica is its own
Workload and reserves on its own, so a role sits anywhere between zero and `desired` while capacity
arrives — where a role that shared one Workload passed all-or-nothing and this figure could not exist.

The field is always present once written, and its zero is an observed zero: it is counted from Pod
and Workload lists that succeeded, and a failed list writes no status at all rather than a zero. The
two readings call for opposite actions — a visible 0 means the pass read everything and no replica
holds quota, so investigate capacity; a field that is not there means the pass could not see, so wait,
or look at the operator rather than the pool.

`assignedFlavors` answers *which accelerator models did this role's replicas actually get*, read from
each replica's own Workload's per-PodSet assignment. It is a **deduplicated, sorted** list, because
each replica is its own Workload and Kueue assigns a flavor per Workload — two replicas of one role
can land on different flavors.

It is **absent** rather than empty while no assignment exists, and absent does not only mean "not
admitted yet". The field reports the flavor of a replica's *accelerator* credits: a role admitted onto
a pool carrying no accelerator at all reports nothing here while being perfectly healthy — its
Workloads hold assignments, naming a flavor for `cpu`.

One entry means every assigned replica of the role names it. Several entries mean the replicas were
assigned different flavors — the signal to investigate, not a degraded form of one answer. Which
replica carries which flavor is deliberately not here: the ordinal such an answer would key on is the
converger's internal slotting rather than a promise this API makes, and a reader who needs the mapping
reads the replicas' own Pods.

That is the field's contract rather than a gap in it. The answer is read through the same lens the
per-accelerator admission gate uses, and a flavor reported here that the gate would not fit against
would be worse than none.

Eight conditions carry the axes a single phase cannot. They are independent: "quota reserved but
cache not attached" is a real and actionable state. Seven are described below; `WeightsReady` is
described with the artifact it reports on, in [Model Artifact Reference](model-artifact.md#status).

**`DomainRegistered`** — whether the referenced Binding resolved and its domain was read.

| Value | Reason | Where it sends you |
|---|---|---|
| `True` | `Registered` | nowhere; the domain in `status.kvCache` is current |
| `True` | `NotApplicable` | nowhere; the deployment declares no `kvCache`, so there is no Binding to resolve |
| `False` | `BindingNotFound` | create the Binding — an admin doing so is what grants access |
| `False` | `BindingNotReady` | wait for it, or look at the pool it points at |
| `False` | `BindingDeleting` | find who deleted it; the replicas keep writing to the domain they attached to |

**`QuotaReserved`** — whether **every one of** the deployment's Workloads holds Kueue quota. Every
replica is its own group with its own Workload, so a deployment of N replicas is N Workloads. `True`
is therefore an answer about the whole set and not about whichever Workload sorts first, because half
a deployment holding quota is not the deployment holding quota.

A message that names groups names them **by role**, never by `instanceType`: two roles can name one
type, so a type would point at two roles' groups at once while a role names exactly the set that
waits. The queue an operator has to free follows from the named role's own `instanceType`.

It reads those Workloads' own conditions rather than asking the admission gate. The gate stops
evaluating a Workload once it is admitted, so anything derived from it would answer for the moment of
admission and never again.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Reserved` | every replica has quota reserved; with one role the message names its cluster queue |
| `False` | `Pending` | at least one replica is waiting for quota; the message names how many, and where they wait |
| `False` | `PodGroupIncomplete` | fewer replicas exist than a role declares, so Kueue composes **no Workload at all** for the missing ones; the message carries `<have>/<want>` and the role's cluster queue |
| `False` | `PreemptedInPart` | a higher-priority workload reclaimed some replicas' quota while others are still admitted; the message says whether any role kind has no admitted replica left, and so whether the loss is of capacity or of a whole role |
| `False` | `Parked` | the set failed to assemble for long enough that the joint check deactivated its Workloads; an identical re-apply does not clear it |
| `False` | `NoQueueInReservedNamespace` | the deployment is in a reserved namespace, which has no LocalQueue, so it will never be scheduled |
| `Unknown` | `AdmissionInFlight` | a group is complete and has no Workload yet — Kueue composes it asynchronously, so absence is admission in flight, not refusal |
| `Unknown` | `NoReplicas` | no replica has been created yet |
| `Unknown` | `AllReplicasTerminating` | every replica is on its way out, so none holds quota to report on |

`PodGroupIncomplete`, `NoQueueInReservedNamespace` and `AdmissionInFlight` all observe no Workload
but mean different things. The first clears when the missing replica exists; the second is permanent
because reserved namespaces deliberately have no LocalQueue; the third clears when Kueue composes the
Workload. Telling them apart is why the reason exists.

`Pending`, `PreemptedInPart` and `Parked` all say the set does not hold quota, and they are separate
because the action differs. `Pending` resolves itself when capacity appears. `Parked` is over already:
those workloads were deactivated and no longer ask for anything. `PreemptedInPart` is the one to act
on, and the replicas that kept their quota hold accelerators until either the reclaimed replicas are
admitted again or the deployment is deleted.

> **Why it is not just a slower `Pending`.** How long the wait is worth making depends on what
> preempted the deployment, which is outside this object and outside this operator. That is a
> decision, so it is reported rather than made.

The cost of the wait lands on the queue, not on this deployment. The replicas that kept their quota
hold accelerators while the set cannot serve in full, and Kueue accounts that quota as used: the
next workload drawing from the same ClusterQueue is refused with `insufficient unused quota`. A
deployment preempted in part starves whatever is queued behind it.

What to do about it:

- **Add capacity.** The reclaimed replicas are admitted again on their own once capacity returns.
  The state self-heals but is UNBOUNDED — no timeout, no give-up, nothing makes the holder yield, so
  capacity that never comes back leaves it standing.
- **Delete and recreate the deployment.** Deleting is the other release for the quota the kept
  replicas hold.
- **Move the roles apart.** Each role draws from the ClusterQueue its own `instanceType` resolves
  to; giving the roles different types keeps one preemption from reclaiming both halves out of the
  same queue.

**`CacheAttached`** — whether the cache is observed to be in effect, which is a different question
from whether it was configured. It is judged downstream of the engine and **never** on a rendered flag
or a log line.

> **Why** — measured on the shipped store, `--enable_kv_events=true` is accepted, the startup log
> echoes `enable_kv_events=1`, and `GET /kv_events/status` still answers `{"enabled":false}` with the
> socket never bound. In the same project another undeclared switch fails loudly instead. One
> switch's failure mode cannot be inferred from another's.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `NotApplicable` | the deployment declares no `kvCache`, so there is no cache to attach |
| `True` | `CacheActive` | a ready replica reports succeeding store operations |
| `True` | `CacheActive` | no replica gave an account, and the reuse domain holds data — this attributes to the domain, which is shared by every deployment on its Binding |
| `False` | `CacheOperationsFailing` | a ready replica reports store operations of which **none** succeeded; the engine is serving without the cache |
| `Unknown` | `Unmanaged` | a role took over its command line, so the operator rendered no cache client |
| `Unknown` | `NoReplicaReady` | no replica is ready, so no engine has an account to give |
| `Unknown` | `NoObservationAvailable` | ready replicas gave no account and the domain reports nothing held |

`NoObservationAvailable` is `Unknown` rather than `False` because an attached deployment that is
simply idle looks exactly like an unread one. No supported engine publishes anything that says "the
connector initialized" before any traffic, so reading silence as a detachment would be a false alarm
on the most common steady state there is. A connector that cannot come up takes its replica with it,
and that is already reported as a replica that never becomes Ready.

**`ReplicasUpToDate`** — whether the running replicas match what the convergence renders for them now.
It is deliberately **not** a statement about `spec` alone: the hash it compares covers the synthesized
KV cache connector too, so a replica still carrying the current spec differs from the render as soon as
that connector stops resolving — which is the one state the other three conditions describe correctly
while saying nothing about.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `UpToDate` | every replica matches what the pass rendered |
| `False` | `RolloutInProgress` | replicas differ from the render and turn over **one per role per pass**; this pass deleted at most one replica per role, and the pass that finds it gone creates the replacement |
| `False` | `ReplacementInProgress` | every surviving replica matches the render but the declared count is short — what removed them is not this condition's to say, and a preemption reports itself on the quota condition; the pass creates each replacement once the departed replica's ordinal reads empty |
| `False` | `RolloutHeldByCache` | replicas that differed from the render were **left in place**: no connection resolved this pass, and recreating them on that alone would rebuild every replica whenever the store blinks |
| `Unknown` | `RolloutNotObserved` | the pass accounted for no replica at all, so it established nothing either way |

A pass answers only for the replicas it can vouch for — ones whose hash it read, or ones it created
from the render it just performed. A pass that can vouch for none reports `Unknown`, because "nothing
was outdated" and "nothing was looked at" are the same zero and only one of them is an answer.

That is not a rare path, and it is not a quiet one. A teardown and the pass between a rollout's
delete and the create that answers it both reach the status write that way — and those are the
moments the replicas are least current.

`ReplacementInProgress` and `RolloutInProgress` are separate because they answer opposite questions.
A rollout answers "did my edit land"; a replacement answers "why is capacity moving when I changed
nothing" — reading the first for the second sends you to diff a spec that did not change. While the
pass waits to replace a missing one, what to watch is the departed Pod itself: the replacement is
created only once no Pod for that ordinal reads on the API server.

> **Why `Unknown` rather than leaving the last answer standing.** Leaving it alone keeps whatever the
> last answering pass wrote, and after a steady deployment that is an authoritative `True`. A pass
> that finds every replica on its way out accounts for none of them — and a sticky answer would go on
> reporting that every replica matches the render while none exists.

`RolloutHeldByCache` is the answer to "I changed the image and nothing happened". Nothing else on the
object is about that edit: the only false condition names a reuse domain whose figures could not be
read, which is accurate and about a different subject.

> **It does not mean an edit is waiting.** During an outage the render carries no connector, so every
> attached replica differs from it whether or not anyone changed the deployment — an ordinary store
> blink puts every deployment on the pool into this state. The message therefore says what *would* be
> delayed, and never that something is.

> **And not every change is delayed.** A `replicas` change proceeds during an outage: the ordinals it
> adds do not exist yet and are created without a connector, and the ordinals it sheds leave on the
> ordinary path. What waits is an edit that changes a running replica's rendered Pod.

> **A scale-out is no longer a lever for the waiting edit.** Replicas added during the outage are
> created without a connector and turn over once more when the store returns — they pay the second
> reload themselves — but the edit waiting on the running replicas still waits. What bounds the wait
> is the store's own recovery, and the condition's message says what waits rather than promising a way
> out.

> **The stall is a decision, not a missing feature.** The guard exists because the alternative —
> recreating every replica whose render lost its connector — deletes every replica of every deployment
> on a pool for a few seconds of store unavailability. Measured, a withheld edit lands within seconds
> of the store returning.

**`RoleKindsReady`** — whether every role **kind** the deployment declares has at least one ready
replica. It is deliberately **not** replica completeness: "every role has all the replicas it asked
for" is what `phase` answers, by summing every role's counts before judging.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `AllKindsReady` | every kind present has at least one ready replica |
| `False` | `KindsNotReady` | at least one kind has none; the message names the kinds |
| `Unknown` | `NoRoleStatuses` | the pass accounted for no role at all, so there is nothing to judge |

A deployment whose roles all share one kind — the shape you get when no role names a `kind`, since it
defaults to `server` — reports `True` as soon as any replica is ready. That is not a blind spot. The
degradation is real and `phase` reports it as `Degraded`; this condition answers a different question
and is silent on that shape by design.

A role that took over its command line counts toward its kind like any other. Such a role gets no
readiness probe, so its `Ready` says its containers started rather than that anything answered on the
serving path. `status.roles[].unmanaged` is published per role for a reader who needs the stronger
reading.

> **Why a separate condition rather than a finer `phase`.** The two answer different questions over
> the same numbers. A sum cannot express this one: a deployment missing an entire kind and a
> deployment one replica short produce the same sum, so the same phase. Adding the axis as a
> condition leaves every existing consumer of `phase` reading exactly what it read before.

> **Why it never claims the deployment is serving.** Whether a request can be answered depends on
> what the engine does with a role told it is a producer, and on whether a Service's endpoints reach
> the ready replicas. Neither is observable from this object, so this condition reports readiness by
> kind and stops there — which is what keeps it correct however those two are later settled.

**`KVEventsPublishing`** — whether every role that produces cache blocks has publisher configuration
in its rendered Pods. It reports configuration, not live traffic: a configured publisher that later
crashes remains `True`.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Publishing` | every producing role is configured to publish |
| `True` | `NotApplicable` | an unrouted deployment declares only `server` roles |
| `False` | `PublisherDisabled` | a producing role's rendered Pods do not enable publishing |
| `False` | `NoRouter` | a prefill/decode pair has no router to consume events |
| `Unknown` | `RoleUnmanaged` | a producing role replaced its command line, so the operator cannot inspect what it does |

**`RouterReady`** — whether the managed router rendered and has a ready replica. A render refusal is
projected here rather than returned: it is a spec problem, so the rest of the status — phase, the
other conditions, the role counts — keeps computing instead of going stale with no axis naming the
cause.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Ready` | the router's Deployment has ready replicas |
| `True` | `NotApplicable` | the deployment declares no `spec.router` |
| `False` | `RenderFailed` | the router's object set could not be rendered; the message carries the refusal |
| `False` | `NoReadyReplicas` | the router's Deployment exists but no replica is ready |
| `Unknown` | `NotDeployed` | the router's Deployment has not been created yet |

---

**See also** — [Model Deployment Reference](model-deployment.md) for the contract this status
reports on · [KV Cache Leader](../kv-cache/leader.md) for the leader process the conditions
distinguish from the operator's own.

**Next** → [Model Deployment Reference](model-deployment.md)
