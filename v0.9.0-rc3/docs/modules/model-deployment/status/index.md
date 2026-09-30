# Model Deployment Status

A `ModelDeployment` reports its phase, ready replicas and serving endpoint in `status`. Use these
fields and its conditions to diagnose a deployment that is still starting or has lost capacity.

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
`<scheme>://<name>.<namespace>.svc:<port>`. The scheme is read from the same role the port is (the
first one), and is `https` only where that role passes `--ssl-certfile` or `--ssl-keyfile`. With a
router it is absent until the router is ready, then uses the router Service's own `http` transport.

`status.router` publishes the contract used to render the router: its implementation and address,
the cache pool's client endpoint, the metrics names and port, and one entry for every role containing
the effective kind, selector, direct endpoint and any dialable KV-event endpoints. The event entries
are absent when the corresponding rendered Pods do not carry publisher configuration.

This router contract names metrics sources for routing; it is not a live utilization sample.
The separate [Model Deployment Metrics](/gpustack-operator/v0.9.0-rc3/docs/modules/model-deployment/metrics/index.md) subresource reads the current
router and engine Pod endpoints and reports partial coverage explicitly.

Only `--ssl-certfile` or `--ssl-keyfile` turn the scheme to `https`. The rest of the `--ssl-*`
family does not: both engines enable TLS for those two alone, so any other ssl argument passed by
itself leaves an ordinary HTTP server.

**`ready` counts replicas whose engine answered, not replicas whose process started**, except on the
three shapes listed below, which carry no gates and keep the weaker meaning. One still loading its
model counts as not ready for as long as that takes.

**Both figures count replicas, which is the same as counting Pods only while a replica is one Pod.**
A role at `size: 2` with `replicas: 2` runs four Pods and reports `desired: 2`. A replica of several
Pods is ready only when **every** member it declares is, so a role showing `1/2` with three of its
four Pods running is reporting one whole instance, not three quarters of the role.

A gated replica carries a startup gate, a readiness gate and a liveness gate, all reading the
engine's own `GET /health` on the port the Service targets, so `ready == desired` and "the endpoint
answers" are one fact rather than two.

The KV cache store's leader answers a route of the same name that is deliberately **not** a readiness
signal; see [KV Cache Leader](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/leader/index.md). They are different programs, and only the
engine's route is read here.

**The startup gate allows thirty minutes per attempt, not in total.** A replica that has not
answered by then is restarted and gets the same budget again, so a model that never finishes loading
is a restart loop rather than a replica stuck at not-ready.

**The liveness gate never sees that window**, because the kubelet suppresses liveness and readiness
alike until the startup gate succeeds.

**A replica that stops answering loses readiness first, and is restarted only if it stays quiet
for longer.** The liveness gate's failure threshold is wider than the readiness gate's.

> Readiness withdraws a replica from the Service and is undone by answering again; a restart throws
> away a model that took the startup budget to load. Different thresholds keep those two events
> different, and the liveness gate is what gives a quiet replica a way back, since this operator
> deletes a replica only to shed one the spec no longer declares or to turn over an outdated one.

**The operator tells the engine where to listen.** It renders `--host 0.0.0.0` and `--port <the port
the Service targets>` into the engine's own command line, so the address traffic is sent to and the
address the engine opens are one decision. The two engines do not agree on a default (vLLM opens
every interface on 8000, SGLang opens `127.0.0.1:30000`), and an engine left on its own would be
unreachable on one of them.

**Both flags are filled when missing.** A role that passes either through `extraArgs` keeps its own
value, and the operator adds only the one that is absent.

**A role that enables TLS is still gated**, over HTTPS, on the criterion above. Only the transport
moved (the address is still the operator's own), and the kubelet does not verify the server
certificate on a probe, so a self-signed pair is graded like any other.

**Three shapes carry no gates at all**: a role that replaces the command through
`roles[].command`; a role that moves where its engine listens, with `--host` or `--port`, or that
demands a client certificate a probe has none to present, with `--ssl-cert-reqs` set to anything but
`0` or `1` **alongside a certificate or key**; and a role declaring its port as `UDP` or `SCTP`.

The `--ssl-cert-reqs` values: that flag is an integer, and only `0` (`CERT_NONE`, the default) and
`1` (`CERT_OPTIONAL`) let a certificate-less probe through, so a role passing either keeps all three
gates. Everything else withdraws them: `2`, a value outside that enum, one that is not a number,
and the flag with no value at all. Losing a gate costs a signal, while fitting one to a listener
that refuses it restarts a replica that is serving.

The first two fail one way and the third the other. Gating an address the operator does not know
would restart a replica answering every request; an HTTP engine speaks TCP whatever the declaration
says, so a gate would **pass** while the published endpoint forwards a protocol nothing answers.
Replicas of all three are Ready as soon as their process starts.

`quotaReserved` is how many of the role's replicas hold a quota reservation. Each replica is its own
Workload and reserves on its own, so a role sits anywhere between zero and `desired` while capacity
arrives; a role that shared one Workload passed all-or-nothing, and this figure could not exist.

The field is always present once written, and its zero is an observed zero: it is counted from Pod
and Workload lists that succeeded, and a failed list writes no status at all rather than a zero. The
two readings call for opposite actions: a visible 0 means the pass read everything and no replica
holds quota, so investigate capacity; a field that is not there means the pass could not see, so wait,
or look at the operator rather than the pool.

`assignedFlavors` answers *which accelerator models did this role's replicas actually get*, read from
each replica's own Workload's per-PodSet assignment. It is a **deduplicated, sorted** list, because
each replica is its own Workload and Kueue assigns a flavor per Workload, so two replicas of one role
can land on different flavors.

It is **absent** rather than empty while no assignment exists, and absent does not only mean "not
admitted yet". The field reports the flavor of a replica's *accelerator* credits: a role admitted onto
a pool carrying no accelerator at all reports nothing here while being perfectly healthy: its
Workloads hold assignments, naming a flavor for `cpu`.

One entry means every assigned replica of the role names it. Several entries mean the replicas were
assigned different flavors, the signal to investigate and not a degraded form of one answer.

Which replica carries which flavor is deliberately not here: the ordinal such an answer would key
on is the converger's internal slotting rather than a promise this API makes, and a reader who
needs the mapping reads the replicas' own Pods. A flavor reported here is read through the same
lens the per-accelerator admission gate uses, so the two cannot disagree.

Eight conditions carry the axes a single phase cannot. They are independent: "quota reserved but
cache not attached" is a real and actionable state. Seven are described below; `WeightsReady` is
described with the artifact it reports on, in [Model Artifact](/gpustack-operator/v0.9.0-rc3/docs/modules/model-delivery/artifact/index.md#status).

**`DomainRegistered`** — whether the referenced Binding resolved and its domain was read.

| Value | Reason | Action |
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

It mirrors the Workloads' own conditions, so it stays current after admission rather than
reporting only the moment a Workload was admitted.

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

> How long the wait is worth making depends on what preempted the deployment, which is outside this
> object and this operator. The condition reports the state and leaves that decision with you.

The cost of the wait lands on the queue, not on this deployment. The replicas that kept their quota
hold accelerators while the set cannot serve in full, and Kueue accounts that quota as used: the
next workload drawing from the same ClusterQueue is refused with `insufficient unused quota`. A
deployment preempted in part starves whatever is queued behind it.

What to do about it:

- **Add capacity.** The reclaimed replicas are admitted again on their own once capacity returns.
  The state self-heals but is UNBOUNDED: no timeout, no give-up, nothing makes the holder yield, so
  capacity that never comes back leaves it standing.
- **Delete and recreate the deployment.** Deleting is the other release for the quota the kept
  replicas hold.
- **Move the roles apart.** Each role draws from the ClusterQueue its own `instanceType` resolves
  to; giving the roles different types keeps one preemption from reclaiming both halves out of the
  same queue.

**`CacheAttached`** — whether the cache is observed to be in effect, which is a different question
from whether it was configured. It is judged downstream of the engine and **never** on a rendered flag
or a log line.

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
simply idle looks exactly like an unread one: no supported engine signals "the connector
initialized" before any traffic, so silence is the most common steady state. A connector that
cannot come up keeps its replica from ever becoming Ready, which is reported there.

**`ReplicasUpToDate`** — whether the running replicas match what the convergence renders for them now.
It is not a statement about `spec` alone: the comparison includes the rendered KV cache connector,
so a replica carrying the current `spec` can still differ from the render once that connector stops
resolving — the state behind `RolloutHeldByCache`, which the other conditions do not describe.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `UpToDate` | every replica matches what the pass rendered |
| `False` | `RolloutInProgress` | replicas differ from the render and turn over **one per role per pass**; this pass deleted at most one replica per role, and the pass that finds it gone creates the replacement |
| `False` | `ReplacementInProgress` | every surviving replica matches the render but the declared count is short — what removed them is not this condition's to say, and a preemption reports itself on the quota condition; the pass creates each replacement once the departed replica's ordinal reads empty |
| `False` | `RolloutHeldByCache` | replicas that differed from the render were **left in place**: no connection resolved this pass, and recreating them on that alone would rebuild every replica whenever the store blinks |
| `False` | `RolloutHeldByWeights` | replicas that differed from the render were **left in place**: the model weights their replacements need are blocked, so deleting them would only remove serving replicas; `WeightsReady` says what blocks them |
| `Unknown` | `RolloutNotObserved` | the pass accounted for no replica at all, so it established nothing either way |

A pass answers only for the replicas it read or created from the render it just performed; one that
could account for none reports `Unknown`, rather than a zero indistinguishable from "all up to
date". This happens in ordinary operation — a teardown, and the pass between a rollout's delete and
the create that answers it, both write status this way.

`ReplacementInProgress` and `RolloutInProgress` answer opposite questions — whether an edit landed,
and why capacity moved when nothing was edited. While a replacement waits, watch the departed Pod:
the replacement is created once no Pod for that ordinal reads on the API server.

> The answer is not left standing between passes. After a steady deployment it would keep reporting
> `True` while every replica is on its way out, because a pass that finds every replica departing
> accounts for none of them.

`RolloutHeldByCache` is the condition to read when an edit appears to have no effect; it is the only
condition on the object that names a held rollout.

> The state does not imply an edit is waiting. During an outage the render carries no connector, so
> every attached replica differs from it whether or not anyone changed the deployment, and an
> ordinary store blink puts every deployment on the pool into this state. The message names what
> *would* be delayed, never that something is.
>
> Not every change waits: a `replicas` change proceeds during an outage — the ordinals it adds do
> not exist yet and are created without a connector, and the ordinals it sheds leave on the ordinary
> path. An edit that changes a running replica's rendered Pod is what waits. Scaling out does not
> release it: replicas added during the outage turn over once more when the store returns, and the
> running replicas still wait. The store's own recovery bounds the wait.
>
> The stall is deliberate: the alternative — recreating every replica whose render lost its
> connector — deletes every replica of every deployment on a pool for a few seconds of store
> unavailability. Measured, a withheld edit lands within seconds of the store returning.

**`RoleKindsReady`** — whether every role **kind** the deployment declares has at least one ready
replica. It is deliberately **not** replica completeness: "every role has all the replicas it asked
for" is what `phase` answers, by summing every role's counts before judging.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `AllKindsReady` | every kind present has at least one ready replica |
| `False` | `KindsNotReady` | at least one kind has none; the message names the kinds |
| `Unknown` | `NoRoleStatuses` | the pass accounted for no role at all, so there is nothing to judge |

A deployment whose roles all share one kind (the shape you get when no role names a `kind`, since it
defaults to `Server`) reports `True` as soon as any replica is ready. That is not a blind spot. The
degradation is real and `phase` reports it as `Degraded`; this condition answers a different question
and is silent on that shape by design.

A role that took over its command line counts toward its kind like any other. Such a role gets no
readiness probe, so its `Ready` says its containers started rather than that anything answered on the
serving path. `status.roles[].unmanaged` is published per role for a reader who needs the stronger
reading.

Read this condition alongside `phase`: it reports readiness by role kind, while `phase` reports
replica completeness. Neither guarantees that an inference request succeeds; that also depends on
the engine and Service connectivity.

**`KVEventsPublishing`** — whether every role that produces cache blocks has publisher configuration
in its rendered Pods. It reports configuration, not live traffic: a configured publisher that later
crashes remains `True`.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Publishing` | every producing role is configured to publish |
| `True` | `NotApplicable` | an unrouted deployment declares only `Server` roles |
| `False` | `PublisherDisabled` | a producing role's rendered Pods do not enable publishing |
| `False` | `NoRouter` | a prefill/decode pair has no router to consume events |
| `Unknown` | `RoleUnmanaged` | a producing role replaced its command line, so the operator cannot inspect what it does |

**`RouterReady`** — whether the managed router rendered and has a ready replica. A render refusal is
reported here rather than returned as an error, so the rest of the status (phase, the other
conditions, the role counts) keeps computing with the cause named.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Ready` | the router's Deployment has ready replicas |
| `True` | `NotApplicable` | the deployment declares no `spec.router` |
| `False` | `RenderFailed` | the router's object set could not be rendered; the message carries the refusal |
| `False` | `NoReadyReplicas` | the router's Deployment exists but no replica is ready |
| `Unknown` | `NotDeployed` | the router's Deployment has not been created yet |

---

**See also** — [Model Deployment](/gpustack-operator/v0.9.0-rc3/docs/modules/model-deployment/deployment/index.md) for the contract this status
reports on · [KV Cache Leader](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/leader/index.md) for the leader process the conditions
distinguish from the operator's own.

**Next** → [Model Deployment](/gpustack-operator/v0.9.0-rc3/docs/modules/model-deployment/deployment/index.md)
