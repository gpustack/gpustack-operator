# Spec: ModelDeployment Router — Three Implementations, One Image, and a Configurable Surface

Status: Shipped
Not covered by this document's own acceptance: T17 is met on both hardware shapes, both halves each.
T18 is NOT, and the gap is recorded rather than rounded off — three of its twelve cases failed, one
refused to run, and two of the four router-and-engine attribution rounds produced no transfer
reading at all. T18's entry names each one and says what does not count as closing it. Two of the
three failures had their cause repaired in a separate pull request that is now merged, and NO RERUN
HAS HAPPENED: repairing the cause is not the same fact as the case passing, and only a rerun on a
cluster settles which.
Type: Feature

> **One document for six pieces of work that cannot be DECIDED apart, and one of them ships first.**
> The image is built and published in its own pull request ahead of the rest, because a setting
> naming a tag no registry holds is a deployment that cannot start; F9 records that ordering. Every
> other piece lands together. Renaming the router enum's only
> value, widening that enum to three implementations, teaching each of the three to pair a prefiller
> with a decoder, correcting which Pods a router discovers, lifting two hard-coded values onto the
> API, and building one image that carries all three routers are recorded here together because each
> one decides the shape of the next. The rename is free only while the enum has one value. The widen
> decides whether a configuration field is one router's or every router's. Disaggregation decides
> what the widen actually delivers, and it is what pulls a patched upstream source into the image.
> The discovery correction has to land before the widen rather than after, because two new routers
> written against the current selector would inherit the same defect on their first day. The image
> exists because the widen would otherwise multiply what a cluster has to mirror. A reader holding
> only one of the six cannot check the rest.

## Summary

A `ModelDeployment` can name exactly one router today, `llm-d`, and that router's configuration is
three hard-coded values the user cannot reach. This spec renames the enum value to `llm-d-router`,
widens the enum to also accept `vllm-router` and `sglang-gateway`, and makes all three able to pair
a prefiller with a decoder — which for SGLang means rendering a disaggregated engine this operator
has never rendered, and for the vLLM router means repairing an upstream lookup that silently leaves
the transfer unstarted under Kubernetes discovery. It gives `spec.router` a request timeout that
means the same thing under all three, gives it a disaggregation threshold that means something under
one of them, adds a fixed access log to the proxy the llm-d router needs, and records in code, in
the admission catalog and in the reference page why two security flags are rendered off against an
upstream default of on. It also adds `pack/llm-router`, one image built from all three upstream
sources and carrying that one patch, so that widening the enum costs a cluster nothing it has to
mirror.

## Motivation

### What exists, and what is out of reach

`spec.router.name` is a closed enum with one value (`api/worker/v1alpha1/model_deployment.go:647`),
and the field's own comment says the single value is "a choice taken for now, not the absence of
one". The renderer dispatches on it through a map with one entry
(`pkg/worker/kvcache/router/router.go:66-68`).

Three values inside that rendering are hard-coded and have no path to a user:

- `nonCachedTokens: 8`, the threshold that decides whether a request is worth splitting across a
  prefill and a decode replica (`pkg/worker/kvcache/router/router.go:189-190`).
- `timeout: 86400s`, the proxy's route timeout (`pkg/worker/controllers/worker/model_deployment_router.go:506`).
- `--secure-serving=false` and `--metrics-endpoint-auth=false` on the picker's argument list
  (`pkg/worker/controllers/worker/model_deployment_router.go:187,189`).

The first two are not reachable through `spec.router.extraArgs` and the reason is structural rather
than accidental: `extraArgs` appends to the picker's argv, while the plugin parameters live in an
`EndpointPickerConfig` document that the operator renders whole and mounts as a ConfigMap. Half of
the router's configuration surface therefore has no entrance at all. The third pair is reachable in
neither direction: both flags are in the operator's owned catalog and are refused when supplied
(`pkg/worker/webhooks/worker/model_deployment.go:524-533`, refused at `:544`).

The proxy template has a second kind of gap. It configures no access log, so every failure short of
the proxy crashing — the picker selecting an unhealthy backend, an upstream timing out, a route
that matches nothing — produces no proxy-side record at all. The template is a Go string constant
with no schema behind it, and its last defect was a field name placed on the wrong message: the
proxy failed to parse and crash-looped while the picker beside it stayed healthy, so the symptom
was 503s and timeouts rather than a condition turning red.

### Why a second and third router

The llm-d router is the only one of the three that consumes engine KV-cache events, and it is also
the heaviest shape: it is a decision component that produces a selection and nothing else, so it is
only a complete request path when an L7 proxy speaking `ext_proc` runs in front of it. The vLLM
router and the SGLang model gateway are single processes with no proxy dependency. A deployment
that wants cache-aware routing across a pool of plain servers pays for a two-container Pod and a
proxy configuration it never needed.

The choice belongs on the object because it is the object's, and the field that expresses it already
exists. What is missing is the other three quarters of what widening an enum means: a renderer per
value, the object set each one needs, and an admission rule for the combinations that do not exist.

### Why the value is renamed first

The upstream project is `llm-d/llm-d-router` and the component this operator runs is that
repository's endpoint picker, which is also how the two images this operator already pins spell it
(`pkg/worker/settings/value.go:124,147`). `llm-d` names the umbrella project instead, so the enum
would be about to hold one project name beside two component names.

The rename is a breaking change to an enum, and it is free exactly once. `spec.router` entered the
API after the most recent release, so no released version carries the field and no stored object in
a released cluster can hold the old value. After the widen, the same rename would have to move
three render branches, three admission rows, the reference page and the end-to-end fixtures
together.

### Why one image

Widening the enum to three values without unifying the image means three image settings, three
entries in every air-gapped cluster's mirroring plan, and three pins to keep aligned with one
operator release. The three routers are a Go binary and two Rust binaries; none of them links an
accelerator runtime and none of them runs a model, so one image can carry all three and the
workload names the one it wants.

### Goals

1. `spec.router.name` accepts `llm-d-router`, `vllm-router` and `sglang-gateway`, and each value
   renders a complete, working object set rather than a legal spelling with no product.
2. Every value of the enum can pair a prefiller with a decoder, and each pairing is accepted only on
   a reading that cache blocks actually moved — never on the request having succeeded.
3. A user can declare a request timeout for the router without knowing which of the three renders
   it, and can declare the disaggregation threshold where that concept exists.
4. A request that fails at the proxy leaves a proxy-side record naming the response code, the
   failure flag, the duration and the endpoint the picker chose.
5. The rule that keeps TLS and caller authentication out of the router is written where the next
   reader meets it, together with the upstream default it inverts.
6. One image carries all three routers, built from source, with one version argument each.
7. The cluster-side verification of the `MultiConnector` Prometheus assertion (`#454`) is carried
   as an acceptance item of this work rather than left to be remembered.
8. Every router discovers only the Pods that answer the API, so a role whose instance spans several
   Pods is routed correctly rather than for one member in every instance.

### Non-Goals

- **Structured scoring weights.** The rendered weights equal upstream's defaults today, so there is
  no behaviour difference to expose, and the correct shape is a set per profile rather than a flat
  field. Doing it now would be undone by the profile model.
- **Exposing the `ext_proc` timeout.** It is coupled to `failure_mode_allow` — whether a picker that
  times out means the request is released or refused — and the pair has to be designed together.
- **`flowControl`, session affinity, topology and the other experimental plugins.** Upstream marks
  them alpha or experimental and gates them behind a flag; offering them is endorsing an experiment
  on upstream's behalf.
- **KV event parameters, discovery selectors and target ports.** These are contract values the
  operator derives, and the no-restart property rests on the operator owning them.
- **Aggregating a deployment's `/metrics`.** Whether that is a subresource or a monitor plus a query
  is undecided, and it is not on this path.
- **A `kind` field beside `name`.** Rejected with its reasons in Alternatives.
- **A second vLLM transfer connector.** The point-to-point leg stays Mooncake for every router. An
  alternative connector is recorded as an open question rather than built here.

## Proposal

`spec.router.name` becomes a three-value enum whose values are component names. Each value has its
own renderer and its own object set: `llm-d-router` renders the two-container Pod it needs today,
and the other two render one container with no proxy and no mounted configuration document. Two
scalar fields join `spec.router` — one that every router can express, one that only the llm-d router
has a concept for, refused on the others rather than ignored. The proxy template gains a fixed
access log. One image under `pack/` carries the three binaries.

### User Stories

#### Story 1

As a platform administrator, I want to name `llm-d-router`, `vllm-router` or `sglang-gateway` on the
deployment, so that I can pick a routing implementation that fits my engine and my operational
appetite instead of being locked to the one heavy shape that requires a proxy beside it.

#### Story 2

As a platform administrator, I want to declare the router's request timeout, so that long streaming
completions are not cut off and a deployment that serves short requests does not carry a day-long
hang window — and so that the declaration survives changing which router runs it.

#### Story 3

As a platform administrator running disaggregated inference, I want to set how many non-cached
prompt tokens make a request worth sending to a prefill replica, so that short requests do not pay
for a cross-Pod transfer; and I want to be able to set it to zero so I can turn disaggregation off
for a comparison without deleting the prefill role.

#### Story 4

As an operator diagnosing 503s and timeouts, I want a per-request record from the router's proxy
carrying the response code, the failure flag, the duration and the endpoint that was selected, so
that "the picker chose a bad backend" and "the backend is slow" stop being the same black box.

#### Story 5

As an administrator of an air-gapped or private-registry cluster, I want one router image to mirror
rather than three, so that widening the enum does not become an entry in every cluster's image
synchronisation plan.

#### Story 6

As the next person reading this code, I want the east-west boundary stated next to the two flags it
explains, so that I do not read an inverted upstream default as a bug and propose undoing it.

### Core Features and Acceptance Criteria

#### F1 — The enum value is renamed to `llm-d-router`

The schema enum, the Go constant's value, the renderer's dispatch key, the admission messages, the
reference page and the end-to-end fixtures all read `llm-d-router`, and `make generate` leaves
`git status --porcelain` empty.

The quoted literal `"llm-d"` appears in six non-generated files, and every other Go use is through
the `ModelDeploymentRouterLLMD` constant. **That count is not the work, and reading it as the work is
how the rename ships half done.** The value also appears unquoted where nothing quotes it: as a YAML
value and in prose on the reference page, in the end-to-end skill's own table, and — in
`cases/case-71.sh` — inside label selectors, because the router's Pods carry the router name as a
label value. A whole-word search for the old value additionally matches the two image names that
embed it and must NOT change. So the rename is done per occurrence, from two searches rather than
one, and neither search alone finds the other's hits.

**The router's Deployment must be recreated rather than updated.** The rendered Pod selector embeds
`spec.router.name`, and a Deployment's selector is immutable in `apps/v1`
(`pkg/worker/controllers/worker/model_deployment_router.go`, where the aligner states why it never
compares that field). An existing router Deployment therefore cannot be carried across the rename by
an update; it is deleted and recreated, which is the same conclusion the recreate-the-object cost
below reaches by a different road.

Two comments go stale on the widen rather than on the rename, and are corrected with it: both state
that the transfer leg belongs to a managed llm-d router in front of vLLM
(`api/worker/v1alpha1/model_deployment.go:104,253`), which stops being true when F4 widens that leg.

**Existing objects.** No released version carries `spec.router`, so no supported upgrade path holds
the old value. A development cluster that created such an object before this change must recreate
it: the value is frozen after creation, and the renderer refuses an unknown router name, so the
object cannot be edited into the new value or reconciled under the old one. This is recorded as an
accepted cost rather than a migration, and the reason it is acceptable is the release fact above.

#### F2 — The enum accepts three routers, each with a renderer and an object set

`llm-d-router` keeps today's object set — a proxy container beside the picker, an
`EndpointPickerConfig` document, and a sidecar on each decode Pod. Its rendering changes in exactly
one place, described in F4: the sidecar's handshake argument, which today is a constant and becomes
engine-derived. `vllm-router` and `sglang-gateway` each render a
Deployment with one container, a Service, a ServiceAccount, a Role and a RoleBinding, and no proxy
container and no `EndpointPickerConfig` document. Their configuration is entirely argv.

The two single-process routers expose a near-identical flag surface — host, port, Kubernetes service
discovery with a label selector, a namespace and a target port, per-role selectors, a Prometheus
port and a request timeout — so they share one argv renderer with a per-router delta rather than
having one renderer each. The delta known at writing is the name of the disaggregation switch and
`vllm-router`'s extra backend selector.

**The shared surface hides three defaults that must be overridden rather than inherited, and each one
fails silently if it is not.** `vllm-router` binds its API and its metrics to loopback
(`vllm-project/router@v0.1.15:src/main.rs:98,239`) where the gateway binds them to every interface,
so a Service in front of an un-overridden vLLM router resolves to a process nothing outside its own
network namespace can reach. Its KV connector defaults to NIXL, so a router rendered without an
explicit Mooncake selection routes correctly and never transfers — the characteristic failure of this
area, arriving through a default rather than a defect. And both routers watch every namespace when
the discovery namespace is absent, which is what decides whether the Role and RoleBinding this
operator renders are sufficient: a router that watches cluster-wide under a namespaced Role fails to
list at all. The renderer therefore states all three rather than relying on any of them.

Discovery uses the selector path for all three, which is what preserves the property that scaling a
role does not restart the router. The selector itself is corrected in F11: it currently names every
Pod of the deployment, which above one member per instance includes Pods that answer nothing.

Every value renders its objects under the same name, `<deployment>-router`, and that name joins the
set the Service-name uniqueness rule already guards
(`pkg/worker/webhooks/worker/model_deployment.go:887`). The rule enumerates the deployment's own
Service, each role's, and each instance's headless one; the router's is absent from it, so a role
named `router` derives a Service with exactly the router's name and whichever object is written last
leaves the other without the addresses it publishes. The gap predates this work and is repaired here
because this is the change that gives three routers that name.

`llm-d-router` is the value the documentation presents first and the one the reference examples use.
No value is a schema default: the field is required, and a default would make "which router did
this deployment ask for" unanswerable.

#### F3 — An engine-and-router admission table

Accepted combinations are engine-matched: `llm-d-router` with either engine, `vllm-router` with
vLLM, `sglang-gateway` with SGLang. Every other combination is refused, naming both the engine and
the router. `llm-d-router` takes both engines because upstream carries a handshake connector and a
metrics configuration for each of them; the other two are each one project's router for that
project's own engine. A cross pairing would be a claim this repository has not measured, and
admitting it later is a widening.

The engine metrics table that admission consults today
(`pkg/worker/kvcache/router/metrics.go:5-18`) becomes a precondition of `llm-d-router` alone rather
than of every router, because it exists to feed the picker's metrics extractor and the other two
routers score on their own observations instead.

**All three routers support prefill and decode** — with one part of that established by running the
gateway rather than by reading it, recorded as OQ2 and answered before its acceptance case is
written. A routed pair also needs the engine-side
point-to-point transfer leg, and the predicate that renders it is keyed on the router name today
(`pkg/worker/controllers/worker/model_deployment_connector.go:147-151`, whose comment says in
advance that the clause is the one a second router name would silently inherit). That clause is
widened deliberately rather than inherited: each router gets the engine-side rendering its own
handshake needs, described in F4.

#### F4 — Prefill and decode under every admitted pair

The admitted pairs use two handshakes between them, and each one needs something rendered on the
engine side. The transfer itself stays Mooncake in every case; what differs is how the decoder learns
where to pull from. `llm-d-router` is the value that meets both handshakes, because it is the value
admitted with both engines.

**`sglang-gateway`.** The gateway injects `bootstrap_host`, `bootstrap_port` and `bootstrap_room`
into the decode request, and under Kubernetes discovery it reads the port from a prefill Pod's
`sglang.ai/bootstrap-port` annotation. Nothing upstream is missing. What is missing is on this side:
the SGLang renderer refuses the two role kinds outright
(`pkg/worker/kvcache/inject/sglang.go`, through `SupportsRole`), so this operator has never rendered
a disaggregated SGLang engine. This work teaches that renderer the two kinds, renders SGLang's own
disaggregation arguments and bootstrap port, and renders the annotation the gateway reads.

**`vllm-router`.** The engine side is what this operator already renders — the point-to-point
Mooncake connector — plus the engine's bootstrap-server port and a `vllm.ai/bootstrap-port`
annotation on prefill Pods, which the router's Kubernetes discovery already reads and carries into
its worker registry. The decoder requires all three of `remote_engine_id`, `remote_bootstrap_addr`
and `transfer_id` in `kv_transfer_params` before it will pull, so the router has to supply the
engine id, which it can only learn from the prefiller's bootstrap server.

This is the same engine-side handshake the llm-d sidecar performs, and that sidecar performs it
correctly: it queries the bootstrap server inside the request handler, against the prefill address
it was handed for that request, and builds the three fields from the answer
(`llm-d/llm-d-router@v0.10.0:pkg/sidecar/proxy/connector_mooncake.go:59-61`, built into the decode leg at `:110-115`). It is a
working reference for what the patch restores on the vLLM router's side, which makes the defect a
deviation from a shape this project already runs rather than an unexplored design.

**That last step is where upstream is broken, and this repository patches it.** See F9: the patch
ships with the image, and its header states the defect rather than only fixing it.

**`llm-d-router`.** The picker's half is protocol-agnostic: it writes the endpoint it chose into
`x-prefiller-host-port` (`llm-d/llm-d-router@v0.10.0:pkg/common/routing/common.go:14`, written at
`llm-d/llm-d-router@v0.10.0:pkg/epp/framework/plugins/scheduling/profilehandler/disagg/disagg_profile_handler.go:573`) and its
disaggregation path carries no engine branch. The handshake belongs to the decode Pod's sidecar, and
which one it speaks is one argument: `--kv-connector` already accepts `sglang` beside `mooncake`
(`llm-d/llm-d-router@v0.10.0:pkg/sidecar/constants/constants.go:26-30`, dispatched at `llm-d/llm-d-router@v0.10.0:pkg/sidecar/proxy/proxy.go:575-581`).

So this router disaggregates under both engines. Under vLLM the connector stays `mooncake`, which is
what this operator renders today. Under SGLang it becomes `sglang`, and the sidecar injects
`bootstrap_host`, `bootstrap_port` and `bootstrap_room` at the top level of the request body
(`llm-d/llm-d-router@v0.10.0:pkg/sidecar/proxy/connector_sglang.go:179-191`, field names at `llm-d/llm-d-router@v0.10.0:pkg/sidecar/proxy/proxy.go:90-92`)
— the same three fields the gateway injects. The engine side is therefore the same SGLang rendering
described above, minus the Pod annotation, which only the gateway reads.

The sidecar takes the bootstrap host from the header but not the port: that is a process-wide
constant of 8998, overridable through the `SGLANG_BOOTSTRAP_PORT` environment variable and by no
flag (`llm-d/llm-d-router@v0.10.0:pkg/sidecar/proxy/connector_sglang.go:38-50`). It is weaker than the gateway's per-Pod
annotation and still sufficient here, because one rendering writes both ends of the pair — the
prefiller's bootstrap-server argument and the sidecar's environment variable — exactly as the
Mooncake pair is written today, and each prefill Pod holds the port in its own network namespace so
two of them never collide. The shape it cannot cover is a take-over prefill role, whose arguments
this operator did not build and whose bootstrap port it therefore cannot know.

#### F5 — `spec.router.requestTimeoutSeconds`

An optional `*int32` with a minimum of one. It renders as the proxy's route timeout under
`llm-d-router` and as the request timeout flag under the other two.

**Unset does not mean one thing across the three, and the field's documentation says so rather than
implying otherwise.** Leaving it out renders nothing, so each router keeps its own default: a day
under the proxy (`pkg/worker/controllers/worker/model_deployment_router.go:506`) against half an hour
under both single-process routers (`vllm-project/router@v0.1.15:src/main.rs:247`,
`sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:275`) — a factor of forty-eight. The
field is what makes the value the same across them; its absence is what leaves three upstream
opinions in place. Setting it is therefore the only way the declaration survives a change of router,
and the reference page states both defaults beside the field rather than leaving a user to discover
the difference from a timed-out stream.

Zero is not accepted. It would mean "no timeout" under the proxy and nothing in particular under the
other two, and a field that means the same thing across three implementations cannot carry one
implementation's special value. A day is already long enough that the difference is theoretical, and
the floor can be lowered later without breaking anything.

#### F6 — `spec.router.disaggregationThresholdTokens`

An optional `*int32` with a minimum of zero, meaningful under `llm-d-router` and refused on the
other two, naming the field and the router. Unset renders today's value.

It is the number of prompt tokens not already in a prefix cache that makes a request worth splitting.
Below it the decode replica serves the whole request. **Zero disables disaggregation entirely**: the
upstream decider returns "do not disaggregate" before reading anything else when the threshold is
zero. Zero is accepted rather than refused, and the field's documentation says what it does. It is a
value upstream defines, the pointer type already separates "unset" from "explicitly zero", and
narrowing upstream's domain on this operator's own initiative is the thing that would need a reason.

It is refused rather than ignored on the other two routers. A field that is legal to write and
renders nothing is a shape this repository has rejected twice — once when a mode field was deleted
for it, and once when a router `kind` field was rejected for it. The refusal is stable because
`spec.router.name` is frozen after creation, so the object cannot become invalid through a later
edit to a different field.

#### F7 — A fixed access log on the proxy

The proxy template gains one access log: one JSON object per request to the container's standard
output, carrying the response code, the response flags, the response code details, the duration, the
upstream host the picker selected, the method, the path, the request id and the byte counts.

It is not a field. This is an observability gap rather than a configuration gap: there is no value a
user would set, and the failure it exists for is one where nothing is logged at all.

It renders only under `llm-d-router`, because it is a property of the proxy and the other two routers
have none. Whatever the other two log is theirs, configured by their own flags.

No admin or stats listener is added. That is an unauthenticated control surface, and adding one
alongside an access log would conflate an observability gap with a new endpoint.

#### F8 — The east-west boundary is recorded

`--secure-serving=false` and `--metrics-endpoint-auth=false` stay hard-coded and stay in the owned
catalog. Three places carry the reason: the rendering, beside the flags; the owned-args catalog,
saying that these two entries are a boundary rather than a derived value; and the reference page.

Each statement carries the upstream default it inverts. Upstream defaults both to true, so a reader
comparing this argument list against upstream finds two inversions, and without the reason beside
them the next person proposes undoing them.

The rule itself: a router manages east-west traffic, selecting a replica for a request already
inside the cluster. Transport security and caller authentication are north-south concerns owned by
the gateway that admits traffic, where one policy covers every workload. Neither flag would protect
anything where it sits — the `ext_proc` server's only client is the proxy container in the same Pod
over loopback, and the picker's metrics endpoint is scraped in-cluster.

A render assertion pins both flags, and an admission case pins both catalog entries.

#### F9 — `pack/llm-router`, one image for three routers

A new image under `pack/` builds all three routers from source in separate stages and copies three
binaries into one runtime layer. One build argument per router names an upstream reference; they
move independently because the three projects release independently.

The image is built from a plain base rather than from the published endpoint-picker image. That
image's runtime is a static-only distroless layer with no shell, no package manager and no dynamic
loader, so the two dynamically linked Rust binaries cannot be added to it. The only shape that would
work is copying the picker out of a published image, which pins this repository to upstream's
release cadence and leaves a patch or an unreleased reference nowhere to go.

It carries no entrypoint. One image holds three programs, so an entrypoint would name one of them as
the default and leave the other two reachable only by overriding it; the workload names the binary,
which is where its arguments already come from.

It does not carry the llm-d disaggregation sidecar, which runs in a decode role's Pod rather than in
a router Pod.

`model-deployment-router-image` defaults to `gpustack/llm-router:v0.1.0`, and the setting's
documentation says that the binary is chosen by the rendered command rather than by the image. The
proxy image setting is unchanged: a proxy is not a router.

**The tag is this project's own version rather than any upstream's**, because three upstreams that
release independently cannot be named by one of their version strings. The repository already
separates the two cases: a `mirrored-` image carries the single upstream version it mirrors, while
an image this project assembles carries its own, as `ssh-server` does. Which upstream references are
inside is answered by this file's build arguments, which is where a reader who needs that answer
already has to look.

The image is registered with the repository's base-image workflow so that it is built and published
the way the other `pack/` images are, and it joins the list of builds that take the larger runners
whatever changed, because it is three from-source builds rather than one.

**THAT REGISTRATION AND THIS FILE SHIP AHEAD OF THE REST, IN THEIR OWN PULL REQUEST**, and the
ordering is forced rather than preferred: the setting below names a tag, and a default naming an
image no registry holds is a deployment that cannot start. So the image is merged and built first,
and the setting that names it arrives with everything else. This is the one place this document's
work is split across two pull requests, and it is recorded here rather than only in the second one,
because the second one is where a reader would otherwise look for a piece that is already gone.

**It carries one patch, against the vLLM router, and the patch explains the defect it repairs.**

The defect, measured at the reference this image pins and again on that project's current default
branch: the router resolves a prefiller's Mooncake engine id by querying that prefiller's bootstrap
server, and it does so exactly once, at construction, by iterating the STATIC prefill URL list. The
construction path taken under Kubernetes service discovery receives an empty static list, because
workers arrive later through the watcher. The lookup map therefore stays empty for the life of the
process; the per-request lookup finds nothing, logs that it found no bootstrap information, and
sends the decode request with no transfer parameters at all. The decoder needs
`remote_engine_id`, `remote_bootstrap_addr` and `transfer_id` together before it will pull, so the
transfer never starts. Nothing fails loudly: the request is served by the decoder alone, and
disaggregation is silently not happening.

A second defect rides with it. Even where the static list is non-empty, one query at construction
means a prefiller that restarts under a new engine id is never re-read, which is the same
no-restart property this operator's whole discovery design rests on.

The patch makes the lookup lazy and per-prefiller: on a miss it resolves that prefiller's bootstrap
address from the worker registry — which already carries the port, read from the Pod annotation by
the router's own Kubernetes discovery — queries it, and caches the result under the prefiller's URL.
That repairs both defects at once, because a restarted Pod arrives under a new address and so misses
the cache.

Requirements on the patch itself, because a patch that only fixes is a patch nobody can review:

- Its header states **what is broken, where, and why prefill/decode does not work without it** —
  not merely what the change does.
- It names the upstream reference it was written against, so a version bump that silently absorbs it
  is detectable.
- It is accompanied by an upstream report, and the header says so, so that carrying it is a decision
  with an exit rather than a fork.

This follows the shape this repository already uses for patched upstream sources under `pack/`.

#### F10 — The `#454` cluster-side verification is an acceptance item

Configuring a shared cache pool beside prefill and decode roles renders a `MultiConnector` holding a
direct-transfer connector and a store connector. Upstream gives a connector two independent optional
capabilities — reporting transfer statistics, and registering Prometheus metrics — and the direct
connector has the first without the second while the store connector has both. `MultiConnector`
asserts that every child reporting statistics is in the metrics registry, so the assertion fails and
the engine process aborts. It fires only when the feature works: the direct connector has statistics
to report only once it has actually moved cache blocks.

The runner images this operator renders are built and published by `gpustack/runner`, a different
repository (`pkg/worker/controllers/worker/model_deployment_image.go:16`), so the fix below is a
CROSS-REPOSITORY dependency of this acceptance item rather than something this tree can be read to
confirm. Naming the owner is part of the item: a reader checking it here would find nothing.

Those runner images carry a fix on two paths — a post-build operation that repairs
already-published images for three pinned engine versions on the CUDA and ROCm lines, and a patch
applied at build time from a later engine version onward. The correct statement is that **this
project's builds from those versions carry the fix**, not that upstream fixed it; upstream has not.
Ascend is unaffected: its own multi-connector's point-to-point connector has neither capability, and
this operator does not render the direct-transfer leg for Ascend at all.

What does not count as verified, taken from the issue's own terms:

- Starting without an abort does not count. The assertion needs a successful transfer to have
  produced statistics, so the verification must force a real transfer and must carry a reading
  proving the transfer happened.
- An upstream fix landing does not count, because the images in use are pinned builds.
- Documenting it does not count, because the abort happens in the data plane after a transfer
  succeeds.

The acceptance item therefore names the image tag under test, how a real transfer is forced, and
how each of the two readings — that the transfer happened, and that the process did not abort — is
taken. The Test Plan carries it as an end-to-end case.

#### F11 — Discovery excludes the members that serve no API

A role can now declare an instance of several Pods, and only one member of each instance answers the
OpenAI API. The selector this operator feeds a router names neither the member nor the role
(`pkg/worker/controllers/worker/model_deployment_router.go:116-120`): it selects every Pod of the
deployment except the router's own. Above one member that set includes Pods that serve nothing, so a
router picks one of them for most requests and the failures interleave with successes rather than
the router being visibly broken.

The Services that front the same Pods were already narrowed for exactly this reason
(`modelDeploymentFrontLeadersOnly`, `pkg/worker/controllers/worker/model_deployment_service.go:119`),
and the router's discovery path was not, because it is rendered in a different file and the change
that added the narrowing did not touch that file.

**THE SET OF PODS THAT ANSWER THE API IS NOT EXPRESSIBLE AS A LABEL SELECTOR TODAY, and that is the
whole difficulty.** The member index is written only where the instance has more than one member
(`pkg/worker/controllers/worker/model_deployment_pod_group.go:198-200`), so the target set is a union
of two conditions — Pods carrying no member index at all, and Pods carrying index zero — while a
label selector is a conjunction of equalities. Sizes are per-role and nothing requires two roles to
agree on one (`api/worker/v1alpha1/model_deployment.go:368`; the webhook constrains only the bounds
and the freeze), so the union is reachable by ordinary objects: a server role of one member beside a
server role of four, or a two-member prefiller beside a single-member decoder. Adding `index=0` to
the one selector this operator writes would then select the multi-member leaders and drop every
single-member role entirely — turning a partial outage into a total one for those roles.

No narrower expression point escapes this. The picker's configuration filters per KIND rather than
per role (`pkg/worker/kvcache/router/router.go:71-105`), so two server roles of different sizes
collapse into one filter; and both single-process routers take their discovery selector as equality
pairs with no set-based form (`vllm-project/router@v0.1.15:src/main.rs:215`,
`sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:225`), with one selector only when
they are not in prefill/decode mode.

**So the union is collapsed instead of matched: the member index is written on every member, and the
selector gains one equality.** A single-member instance carries index zero like any other leader,
`index=0` names exactly the Pods that answer, and it does so in the one form every expression point
accepts. The label stops being an optimisation and becomes the contract that says which Pod serves.

This reaches all three routers, not one. Every value of the enum discovers endpoints through the
selector path, so a fix written only for the picker would ship two new routers that are broken the
same way on their first day.

The published contract carries the same correction. `status.router.roles[].selector` is built from a
role's identity labels alone (`model_deployment_router.go:103`), and its field comment claims it
matches "exactly this role's replicas" (`api/worker/v1alpha1/model_deployment.go:929`) — a sentence
that stopped being true once a replica became several Pods, since the selector matches Pods.

**The bill is one rollout, and it is stated rather than discovered.** Writing the index on every
member moves the fingerprint of every single-member replica, so the release carrying this rolls every
deployment in the cluster once — the cost the current condition was written to avoid, when the label
it withheld carried no information. It now carries the only information that makes the set
expressible, which is what changes the trade. The digest table that pins the rendered Pod
(`pkg/worker/controllers/worker/model_deployment_render_test.go:1604-1780`) is re-baselined by the
procedure written in its own header; it is a tripwire for unintended movement, not a prohibition.

Acceptance is a rendered-object reading plus a transition reading. Rendered: every member of every
instance carries the index, and the discovery selector carries `index=0` for all three routers, in
the shapes above — mixed sizes among server roles, and a multi-member prefiller beside a
single-member decoder. Transition: a deployment created before this change must not lose discovery
except for the roll that replaces its Pods, because the selector narrows in the same release that
starts writing the label.

### Notes, Constraints and Caveats

**The rename and the widen cannot be separated.** The rename is free only while the enum holds one
value, and the widen is what makes the old value's spelling wrong. Splitting them means either doing
the rename twice as much work or shipping an enum that mixes a project name with two component
names.

**`status.router` needs no new shape for the approx-only routers.** The predicate that renders the
KV-event publisher configuration is keyed on the router name, so under the two single-process
routers nothing is rendered and `status.router.roles[].kvEvents` is simply absent. The concern that
this operator would render an event stream with no consumer does not arise.

**One predicate answers three questions, and they do not have one answer.**
`modelDeploymentRoutesManagedVLLM` gates the KV-event publisher, the point-to-point transfer leg and,
through the connector flag it sets, the decode Pod's sidecar. Events are correctly keyed on
`llm-d-router`, because only that router consumes them. The transfer leg is not: every router needs
one to disaggregate at all. The sidecar is keyed correctly today for the wrong reason — it belongs to
`llm-d-router` because it speaks a header the other two routers never write, not because that is the
only router. Widening the one predicate would therefore widen the sidecar with the transfer leg and
attach it to decode Pods under routers that do not drive it, where it fronts the serving port and
shadows the pairing those routers perform themselves. Splitting it three ways is part of F4 rather
than an incidental refactor, and the comment that predicted this inheritance is the thing to update
rather than delete.

**SGLang disaggregation renders no topology arguments, and before engine version 0.5.10 that leaves
one assumption.** Three arguments once declared the decode side's shape to the prefiller —
`--disaggregation-decode-tp`, `--disaggregation-decode-dp` and `--disaggregation-prefill-pp`. They
exist at 0.5.7 and 0.5.9 and are gone from 0.5.10 onward, where the bootstrap server establishes the
same facts at run time. Rendering none of them is correct at every version this project ships: the
decode engine refuses two of them outright (`sgl-project/sglang@v0.5.9:python/sglang/srt/server_args.py:2546-2553` at v0.5.9,
"Cannot set --disaggregation-decode-tp for the decode engine"), and on the prefiller they default
from its own tensor and data parallel sizes while the third is overwritten with the pipeline size
and any supplied value discarded (`:2559-2564`). What remains is one case: before 0.5.10 a prefiller
ASSUMES the decoder's parallelism equals its own. Parallelism is not something this operator renders
— it reaches the engine through a role's `extraArgs` — so a pair whose two roles declare different
values is expressible, and on those versions it would be mis-assumed rather than refused. This
belongs on the reference page rather than in the admission table, because `spec.engine.version` is
free text that this operator deliberately does not validate, so a gate would have no trustworthy
version to read.

**The proxy template has no schema.** It is a Go string constant, and its failures are parse errors
at startup that stop the proxy while the picker beside it stays healthy. Every change to it is
therefore asserted on the rendered text, and the access log is asserted for both the field it must
contain and the shape it must not repeat.

**The route timeout is one string in the whole repository.** Nothing in the documentation, the
specs or the end-to-end cases restates it, so lifting it onto the API falsifies no other statement.

**The two new fields are unreleased surface.** `spec.router` has never been released, so if a later
round finds that the llm-d-only field belongs in a per-router block rather than flat on
`spec.router`, that restructure is still free. Choosing the flat shape now is choosing the smaller
surface for one field, not foreclosing the other.

**Replacing the endpoint-picker image default is a ruling, and its bill is stated here.** The
`model-deployment-router-image` default stops naming a mirror of upstream's published picker and
names this project's own build. What this project takes on: producing the picker binary, following
upstream's build if it changes, and publishing on this repository's own cadence rather than
inheriting upstream's. What it buys: one image per cluster to mirror instead of three, one pin per
operator release, and a place for the vLLM router patch to live — without which the patch would have
no image to ship in and disaggregation under that router would not work at all. The alternative that
keeps the mirror is recorded in Alternatives.

**Build verification runs on this project's own amd64 build hosts.** Their addresses are held out of
this document, as this repository requires of every durable artefact.

### Boundaries

- **Always:** keep the router-name clause in the two connector predicates deliberate — a new router
  value inherits it silently, and that inheritance is a decision either way.
- **Always:** state an inverted upstream default beside the inversion.
- **Always:** give every new enum value a renderer and an object set in the same change.
- **Ask first:** before pushing, and before opening the pull request.
- **Ask first:** before widening the engine-and-router table beyond engine-matched pairs.
- **Never:** add a field that is legal to write and renders nothing.
- **Never:** put a host address, a cloud vendor or a report path into a committed artefact.
- **Never:** put a task identifier from this document into a code comment; state the rule instead.

### Risks and Mitigations

- **The rename misses an occurrence and the miss is silent.** A whole-word search for the old value
  also matches the two image names embedding it, so a mechanical substitution corrupts them, while a
  narrow search misses the prose. → Rename per occurrence, and let the schema enum plus the
  renderer's dispatch map fail closed on anything missed: an unrenamed value is refused at admission
  rather than rendered wrongly.
- **The new image's build is long and its failure is late.** Two Rust release builds with large
  dependency trees. → Separate builder stages per router so a failure names the router, and version
  arguments so a bad upstream reference is changed without editing the file.
- **Owning the picker's build means owning its build changes.** Upstream can change how the binary
  is produced. → One build argument names the reference, the build follows upstream's own flags, and
  the cost is stated here rather than discovered.
- **The access log is written into a template with no schema.** → Assert the rendered text, and keep
  the assertion specific enough that a field name placed on the wrong message fails it.
- **A threshold of zero silently disables disaggregation.** A user could turn off the feature they
  asked for without an error. → The value is documented on the field, in the reference page and in
  the refusal-free path; the pointer type keeps "unset" distinguishable, so nothing is disabled by
  accident.
- **The vLLM router patch is a fork this repository now maintains.** A version bump can absorb it,
  conflict with it, or — worst — apply cleanly onto code that has been restructured around it. →
  The patch names the reference it was written against, an upstream report accompanies it, and the
  end-to-end case that forces a real transfer is what detects a bump that quietly undoes it. A patch
  that applies is not a patch that works, and only the transfer reading tells them apart.
- **Disaggregation failing silently is this area's characteristic failure.** The defect the patch
  repairs produces a served request and no error; so does a missing bootstrap annotation, and so
  does a prefiller whose bootstrap server never started. → Every acceptance reading for a routed
  pair is a reading that the transfer HAPPENED, never that the request succeeded.
- **Teaching the SGLang renderer two role kinds touches a path this operator has never rendered, and
  opening that path alone renders a Pod that looks configured and transfers nothing.** The predicate
  that attaches the decode sidecar is engine-agnostic
  (`pkg/worker/controllers/worker/model_deployment_render.go:361-362`), so the moment the role table
  admits SGLang the sidecar appears on its decode Pods still carrying the hardcoded
  `--kv-connector=mooncake` (`pkg/worker/controllers/worker/model_deployment_render.go:671`): an
  SGLang engine paired by the wrong handshake, which fails in exactly the silent way this area is
  characterised by. → The role table, the connector argument and the bootstrap-port pairing change
  together rather than in sequence, the engine-side arguments are asserted against upstream's own
  flag names, and the acceptance reading for the pair is a transfer reading rather than a served
  request.

- **The owned-argument catalog is keyed by router, so a router with no row refuses nothing.** The
  lookup is a map index whose miss yields an empty list
  (`pkg/worker/webhooks/worker/model_deployment.go:544`), so between the commit that admits a new
  enum value and the commit that gives it a row, every argument that router's rendering derives is
  settable through `spec.router.extraArgs` — two values for one setting, which is the exact defect the
  catalog exists to prevent. → The enum entry and the catalog row land together, and a case asserts
  the refusal for each new value rather than only for the first.
- **The reserved-port rule is keyed by engine and not by router.** It reserves both KV-event ports and
  the Mooncake bootstrap port whenever the engine is vLLM, under any router. Under `vllm-router` the
  event ports are reserved but never rendered, which refuses a port nothing uses; re-keying the whole
  rule to `llm-d-router` would instead drop the bootstrap reservation that `vllm-router` genuinely
  needs. → The rule splits by which listener each router actually renders, and the two halves are
  asserted separately so a later edit cannot collapse them back into one.
- **Three predicates read "is this routed", and they do not have one answer.** KV events belong to
  `llm-d-router` alone; the engine-side transfer leg belongs to every admitted pair; and the decode
  sidecar belongs to `llm-d-router` alone, because it speaks a header the other two routers never
  write. Widening the one predicate that gates all three would attach that sidecar to decode Pods
  under the new routers, where it fronts the serving port and shadows the router's own pairing. → The
  split is three-way rather than the two-way split the note above describes, and each way carries a
  case naming which router it is for.
- **The image and the patch create obligations no one is assigned.** Three upstream references move on
  three cadences and are bumped by hand; the patch is detected as broken only by an end-to-end
  transfer reading that runs on dispatch; the two Rust builds resolve their toolchains by two
  different strategies, and the fragile one guards the router that carries the patch; and the
  gateway's pin must handshake with an engine version this operator deliberately does not validate,
  with no compatibility floor written down. → These are stated here so that the next version bump
  reads them, and the Test Plan's transfer readings are what make a silent regression loud.

## Design Details

### Commands

```bash
make generate                       # after any api/ or webhook source change; then git status must be clean
make lint                           # golangci-lint; it edits files, so compare hashes rather than trusting rc=0
make lint docs < /dev/null          # the documentation and spec contract
make test                           # go test over the module
make package llm-router             # build pack/llm-router/Dockerfile via docker buildx (Linux hosts only)
go test ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
```

### Project Structure

```text
api/worker/v1alpha1/model_deployment.go      # the enum value, the two new fields, their documentation
pkg/worker/kvcache/router/router.go          # the renderer registry and the llm-d document renderer
pkg/worker/kvcache/router/metrics.go         # the engine metrics table, becoming llm-d-router's precondition
pkg/worker/controllers/worker/
  model_deployment_router.go                 # the object sets, the proxy template, the access log
  model_deployment_connector.go              # the predicate that gates events and the transfer leg
pkg/worker/kvcache/inject/
  sglang.go                                  # the role kinds SGLang has never rendered, and its
                                             # disaggregation arguments
  vllm.go                                    # the engine bootstrap-server port for the direct leg
pkg/worker/webhooks/worker/model_deployment.go
                                             # the owned-args catalog and the engine-and-router table
pkg/worker/settings/value.go                 # the router image default
pack/llm-router/Dockerfile                   # three upstream sources, three binaries, one runtime layer
pack/llm-router/patches/                     # the vLLM router patch, its defect statement beside it
docs/reference/model-deployment.md           # the router section, the editable-fields table
docs/settings.md                             # the router image setting
.agents/skills/gpustack-operator-e2e/cases/  # the end-to-end cases
```

### Code Style

Values the operator owns are refused rather than merged, and the refusal says why in the terms of
the object rather than of the mechanism. The catalog is keyed by router because a second router's
owned set is not the first one's:

```go
// modelDeploymentRouterOwnedArgs is the flags a router's rendering derives, keyed by router.
//
// THE ENTRIES DIVIDE INTO TWO KINDS, and the refusal message is the same for both only because the
// user-facing consequence is. Most of them are refused because the operator already computes the
// value -- the selector, the target ports and the configuration file path all restate something this
// deployment's own objects decide, and two values for one setting cannot be told apart.
//
// --secure-serving and --metrics-endpoint-auth are refused for a DIFFERENT reason: they are not
// derived, they are a boundary. A router manages east-west traffic inside the cluster; transport
// security and caller authentication are north-south concerns owned by the gateway that admits
// traffic. Both are rendered off, against an upstream default of on, and the rendering states why.
var modelDeploymentRouterOwnedArgs = map[string][]string{
	workercore.ModelDeploymentRouterLLMD: {
		"--endpoint-selector",
		"--endpoint-target-ports",
		"--config-file",
		"--secure-serving",
		"--grpc-health-port",
		"--metrics-endpoint-auth",
	},
}
```

Conventions this work follows: exported API fields document behaviour, expectations and the reason
for a constraint; a prohibition keeps its reason beside it; no comment names a task identifier from
this document; reconciliation stays level-based and idempotent; tests are table-driven with one
behaviour per case and assert observable state.

### Implementation Plan

Go builds, tests, generation and the documentation gate run on a developer workstation. The image is
built on this project's own amd64 build hosts, because the local engine is arm64 and two Rust release
builds under emulation are neither fast enough nor the architecture that ships. The cluster the last
two tasks need is chosen when they are reached.

Four tasks open together because none blocks another, and the two that carry the most uncertainty are
among them: the patch decides whether disaggregation under the vLLM router works at all, and the
reading decides whether any pairing can be accepted.

- [x] **T1 · The vLLM router patch, and the image that carries it**
      Blocked by: None
      Owns: `pack/llm-router/**`
      Gate: review
      Acceptance: a patch whose header states, before it states any change, what is broken, where, and
      why disaggregation cannot work without it — the Mooncake engine-id map is filled only on the
      static-URL construction path, so under Kubernetes discovery it stays empty for the life of the
      process, the decoder receives none of the three transfer parameters it requires, and the request
      is served by the decoder alone with nothing logged as an error. The lookup becomes lazy and per
      prefill URL, taking the port from the worker registry that already holds it. The build applies
      the patch, and each of the three binaries still starts and reports its own version.
      Verify: `make package llm-router` on an amd64 build host, then run each binary's version flag.

- [x] **T2 · Name the reading that proves cache blocks moved**
      Blocked by: None
      Owns: none — the result is recorded in this document's Test Plan
      Gate: review
      Acceptance: one engine-side counter or record that is present only after a transfer, named with
      its source coordinate at a pinned engine version, TOGETHER WITH what the same reading says when
      no transfer happened. A reading with no stated negative is not an instrument, because a reading
      that is absent for two different reasons cannot tell them apart.
      Verify: both readings are quoted from engine source at a pinned tag.
      ANSWERED in the Test Plan's end-to-end section. It took two carriers rather than one: the
      decoder's counter states its zero, the prefiller's log line is absent rather than zero, and the
      branch that would have made the prefiller state its zero is unreachable from an idle interval.

- [x] **T3 · Answer whether the gateway routes to workers carrying no model id**
      Blocked by: None
      Owns: none — the result closes OQ2 or changes F2 and F4
      Gate: review
      Acceptance: the gateway is run with Kubernetes service discovery against at least one discovered
      worker, and a request naming a model either reaches that worker or does not. The answer is
      recorded; if it does not, `sglang-gateway` needs a different discovery shape and F2 changes.
      Verify: a completion returned by the discovered worker, or the gateway's own log naming zero
      eligible backends.
      ANSWERED in Open Questions: it does not, and F2 stands anyway, because the id a worker registers
      under is its own rather than one discovery assigns, and this operator renders the field both
      ends read. The run carried its own positive control — the same worker served a request naming
      the id it did register under — so the refusal is attributable to the match and not to a worker
      that was merely unreachable.

- [x] **T4 · Rename the enum value**
      Blocked by: None
      Owns: `api/worker/v1alpha1/**`, `pkg/kubeclients/**`, `pkg/worker/kvcache/router/router.go`,
      `pkg/worker/controllers/worker/model_deployment_test.go`,
      `pkg/worker/controllers/worker/model_deployment_status_test.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`, `docs/reference/model-deployment.md`,
      `docs/settings.md`, `.agents/skills/gpustack-operator-e2e/**`
      Gate: review
      Acceptance: the quoted literal, the unquoted uses in prose and YAML, and the label values in the
      end-to-end cases all read the new value; the two image names that embed the old spelling are
      unchanged; the two comments that limit the transfer leg to one router-and-engine pair are
      corrected; generation leaves the tree clean.
      Verify: `make generate` then `git status --porcelain` empty; `go test ./pkg/worker/... ./api/...`
      DONE for the sources, and generation is a separate step here rather than part of it: the
      generators refuse to run from a checkout whose path does not end in the module import path, so
      they run from one that does and the regenerated files are reconciled against a named list of
      every place the old value survives. Six generated files carry it, and exactly one of those
      places is functional — the CRD schema's enum bytes; the rest are description text.

- [x] **T5 · Every member carries its index, and discovery selects the ones that answer**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go`,
      `pkg/worker/controllers/worker/model_deployment_pod_group_test.go`,
      `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`,
      `pkg/worker/controllers/worker/model_deployment_router.go`,
      `pkg/worker/controllers/worker/model_deployment_router_test.go`,
      `api/worker/v1alpha1/model_deployment.go`
      Gate: review
      Acceptance: the index is written for every member at every size; the discovery selector carries
      the leader term; THE PUBLISHED ROLE SELECTOR CARRIES IT TOO, and its field comment describes
      what is now selected — that field's own documentation says it is published so a router is
      configured from it, and nothing in this repository reads it, so a published selector that still
      matched every member would hand an outside consumer the defect this task removes from the
      inside path. A group-shape assertion that pins the exact label set of a replica group goes red
      here, and the correction is to state what it now guards rather than to raise its count: it
      exists to stop a second carrier of a REPLICA's identity, and the member index answers a
      different question. The pinned render digests are re-baselined by the procedure in their own
      header. Cases cover two server roles of different sizes, and a multi-member prefiller beside a
      single-member decoder.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T6 · One renderer entry point, returning a document or arguments**
      Blocked by: T5
      Owns: `pkg/worker/kvcache/router/**`,
      `pkg/worker/controllers/worker/model_deployment_router.go`
      Gate: review
      Acceptance: the entry point returns both halves and each router fills exactly one; a renderer
      that fills neither fails a test, because that is the shape a fourth router would arrive in.
      Verify: `go test ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/...`
      DONE. The rule is enforced in the entry point rather than in each renderer, so it covers a
      renderer nobody has written yet, and it refuses both wrong shapes: filling neither, and filling
      both — the second because a router reads one half and silently ignores the other. The case that
      holds every registered renderer to the rule calls the renderer DIRECTLY rather than through the
      entry point, because going through it would fail on the entry point's own error and leave an
      assertion that can never fire.

- [x] **T7 · Split the object-set renderer along the router it renders for**
      Blocked by: T6
      Owns: `pkg/worker/controllers/worker/model_deployment_router.go`,
      `pkg/worker/controllers/worker/model_deployment_router_test.go`
      Gate: review
      Acceptance: the single function that renders every object becomes a shared part and a per-router
      part, with the proxy container, the mounted document and the access log on the llm-d branch
      alone. No rendered object changes for the router that exists today, which the digests state.
      Verify: `go test ./pkg/worker/controllers/worker/...`
      DONE. THE DIGESTS DID NOT EXIST AND WERE CAPTURED BEFORE THE SPLIT, on the same procedure the
      Pod renderer's own table records from when it was split: five inputs off the renderer's
      branches, pinned against the pre-split renderer, and reproduced byte for byte after. A SECOND
      CASE STATES THE BOUNDARY, because the digests structurally cannot: the shared half and the
      per-router half write into one ConfigMap, so a key rendered from the wrong side produces
      identical bytes. Measured — moving the proxy's document to the shared half leaves every digest
      green and turns only that case red. The dispatcher refuses a router with no branch, which the
      caller cannot reach because the configuration renderer refuses an unknown name first; it is
      asserted anyway, since this is the function a fourth router is added to and a branch returning
      the zero value renders a Deployment with no containers at all.

- [x] **T8 · The vLLM router, end to end**
      Blocked by: T7
      Owns: `pkg/worker/kvcache/router/**`,
      `pkg/worker/controllers/worker/model_deployment_router.go`,
      `pkg/worker/controllers/worker/model_deployment_router_test.go`,
      `api/worker/v1alpha1/model_deployment.go`, `pkg/worker/webhooks/worker/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: the enum entry, the owned-argument row and the engine-pairing row land together,
      because a value admitted without its row is a value whose derived arguments a user can also
      supply. The rendering names the transfer connector, both bind addresses and the discovery
      namespace rather than inheriting any of them. One container, no proxy, no mounted document. The
      router's own object name joins the Service-name uniqueness set.
      Verify: `go test ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/...
      ./pkg/worker/webhooks/worker/...` plus `make generate`, which the enum entry drifts and this
      task's own verify line does not name.
      DONE, and THE SELECTOR IS A MAP RATHER THAN AN EXPRESSION, which is a measured correction to
      what the shared discovery string implied. This router matches on equality alone: it splits
      each selector entry on its first "=" and DROPS an entry carrying none, silently
      (`vllm-project/router@v0.1.15:src/main.rs:345-355`, matched at
      `src/service_discovery.rs:74-82`). The published expression's negation therefore has no
      representation, and it needs none — the member index is written on every answering member and
      not on the router's own Pod, so the equalities already exclude it. The equalities are now the
      source and the expression is derived from them, in a listed order, because that string is a
      ConfigMap value under a configuration hash and reordering it would roll every router in the
      cluster for a change nothing reads.
      A ROUTER CONFIGURED BY ARGV RENDERS NO CONFIGMAP AT ALL. The selector, the target ports and
      the user's arguments reach it as arguments, so writing them into an object as well would put a
      second copy of one derivation where nothing reads it and leave a reader unable to tell which
      copy the process uses. The three keys that looked router-independent moved to the picker's
      branch with the other two, and the object is rendered only when a branch asks for one.
      THE PAIRING TABLE IS NOT SYMMETRIC and the case asserting it is one case, not two: a refusal
      alone passes for a handler that refuses the router value outright, and an acceptance alone
      passes for one carrying no rule.

- [x] **T9 · The SGLang gateway, as the delta from the router beside it**
      Blocked by: T8, T3
      Owns: same as T8
      Gate: review
      Acceptance: as T8, expressed as the difference from it rather than as a second renderer. The
      discovery shape is the plain one, which T3 settled: the gateway keys its registry on the id a
      worker reports for itself, and this operator renders that id and the router's model name from
      one field. A case asserts the gateway registers the discovered worker under `spec.model.name`,
      rather than asserting only that some request was served.
      Verify: as T8, plus `make generate`.
      DONE, and THE DIFFERENCE IS MEASURED RATHER THAN DESIGNED. Both flag surfaces were read at
      their pinned tags, and the shared claim holds: host, port, both Prometheus settings, the four
      service-discovery flags and the two per-role selectors are spelled identically in each. The
      whole delta is the disaggregation switch's spelling and the vLLM router's transfer connector,
      which the gateway has no flag for at all. The case asserting it COMPARES THE TWO OUTPUTS
      rather than listing the gateway's own, because a case listing them would pass just as well if
      the two had drifted into unrelated surfaces.
      THE GATEWAY IS TOLD NO MODEL NAME, which is a decision. Turning on service discovery turns on
      its inference-gateway mode by itself
      (`sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:1056-1060`), and that mode
      routes by the id each worker reports for itself. The flag that would carry a name here loads a
      TOKENIZER (`:937`, `maybe_model_path`), so rendering it would make the router Pod depend on
      reaching a model repository at startup for a name it does not route by.
      THE ONE-FIELD CASE COMPARES THE TWO DERIVATIONS WITH EACH OTHER. Every case in this repository
      that touched the engine's model argument compared it with a LITERAL, which keeps passing if
      one side starts reading somewhere else; this one renders a deployment whose model name the
      fixture does not carry and asserts the rendered argument is that field. The cluster half --
      that the gateway does register the worker under it -- is an end-to-end case, because nothing
      rendered here can observe a registry.

- [x] **T10 · The admission rules that are about all three rather than about one**
      Blocked by: T9
      Owns: `pkg/worker/webhooks/worker/**`, `pkg/worker/kvcache/router/metrics.go`,
      `pkg/worker/controllers/worker/model_deployment_router.go`
      Gate: review
      Acceptance: the engine metrics table becomes a precondition of the llm-d router alone, at BOTH
      of its call sites — the admission check and the renderer — because its refusal names the router
      from a package constant rather than from the object
      (`pkg/worker/kvcache/router/metrics.go:33-34,38-39`), so a call left unconditional reports the
      wrong router's name to a user who never asked for it. A case asserts the refusal is not reached
      under the other two routers. The reserved-port rule splits by which listener each router
      actually renders, so neither half refuses a port nothing uses nor releases one that is used.
      Verify: `go test ./pkg/worker/webhooks/worker/... ./pkg/worker/kvcache/router/...
      ./pkg/worker/controllers/worker/...`
      DONE. The renderer's half also drops the PUBLISHED metrics contract for the other two routers,
      which the acceptance does not name and which follows from the same fact: a status listing
      engine metrics under a router that scrapes none describes a router that is not running.
      THE RESERVED-PORT RULE WAS WRONG IN BOTH DIRECTIONS AT ONCE, which is what a single list makes
      possible. It reserved the Mooncake bootstrap port on a SERVER role, where the transfer leg is
      never rendered and nothing binds it; and it ran for vLLM alone, so it released that same
      number on an SGLang prefiller, which now renders its own bootstrap registry there under any
      router. The set is computed per role from the conditions that actually render each listener:
      the event publisher and its replay socket under the picker on a non-decoder, the Mooncake
      bootstrap on a vLLM prefiller under the picker, and SGLang's on any prefiller. Measured —
      putting the one list back turns three cases red, two of them releases and one a refusal.

- [x] **T11 · SGLang disaggregation on the engine side**
      Blocked by: T5
      Owns: `pkg/worker/kvcache/inject/**`,
      `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`,
      `pkg/worker/webhooks/worker/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: the role table admits the two kinds, the sidecar's handshake argument is derived
      from the engine rather than fixed, and the bootstrap port is one value written at both ends —
      all in one change, because admitting the kinds alone renders a decode Pod that looks configured
      and pairs by the wrong handshake.
      THE SPLIT FOLLOWS THE ROLE, NOT THE TRANSFER FLAG, and that is a correctness requirement
      rather than a preference. Admission asks the support table, which is answered per engine and
      role and carries no notion of a transfer leg; a renderer that also required the flag refuses a
      shape admission already stored, and the refusal lands in the reconciler where the user cannot
      act on it. Measured: under the flag-gated reading, a deployment the webhook admits fails to
      reconcile with `role "prefill" cannot be given a cache client`, permanently. The table exists
      precisely so that a role an engine cannot render is refused while the user can still fix it.
      THE ROLE-KIND ADMISSION RULE SURVIVES THIS AND ITS CASES MOVE. Widening the table leaves every
      engine the API accepts supporting every kind, so the rule stops firing for them and the two
      cases asserting SGLang refuses `prefill` assert something no longer true. The rule is kept
      rather than removed, and gains the reason beside it: an engine added to the renderer without a
      support-table entry reports no term for every kind, and this is the only place that becomes a
      refusal the user can act on. Both cases are moved rather than deleted — one inverted to assert
      acceptance so the widening has a witness, and one repointed at a kind with no rendering term so
      the kept rule keeps one. A cross-manufacturer case used the SGLang refusal as its POSITIVE
      BASELINE, without which its acceptances hold vacuously; its baseline becomes a duplicate kind,
      which is the refusal that rule set still reaches.
      THE OWNED-ARGUMENT ROW LANDS WITH THE RENDERING, on the same rule the vLLM router's entry
      follows: a key the renderer emits and the table does not own is a key a user may also supply,
      and a user entry lands after the operator's on one command line. The three disaggregation keys
      are owned because they are the halves of a pair rather than a loader switch — an unowned mode
      lets a role declared as one half start as the other while both ends still advertise the first.
      The invariant that reads the renderer's own output to catch this was BLIND to them: its matrix
      ran engines against manufacturers and no role at all, so it read every key an unsplit replica
      renders and none of the ones a half renders. It gains the role axis.
      Verify: `go test ./pkg/worker/kvcache/inject/... ./pkg/worker/controllers/worker/...
      ./pkg/worker/webhooks/worker/...`

- [x] **T12 · The three predicates that ask "is this routed" answer separately**
      Blocked by: T10, T11
      Owns: `pkg/worker/controllers/worker/model_deployment_connector.go`,
      `pkg/worker/controllers/worker/model_deployment_connector_test.go`,
      `pkg/worker/kvcache/inject/vllm.go`, `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Gate: review
      Acceptance: KV events stay with the llm-d router; the engine-side transfer leg follows every
      admitted pair; the decode sidecar stays with the llm-d router, because it speaks a header the
      other two never write. Each branch carries a case naming the router it is for. The shared
      gate's own doc comment states the widened rule rather than the single pair it names today
      (`pkg/worker/controllers/worker/model_deployment_connector.go:140`), which the rename left
      standing because that file belongs to this task.
      Verify: `go test ./pkg/worker/controllers/worker/... ./pkg/worker/kvcache/inject/...`
      DONE. THE DECODE PROXY NEEDED A CARRIER OF ITS OWN, which the acceptance implies and does not
      name: the renderer decided it by reading the transfer leg, so widening the leg would have put
      that router's proxy on the other two, where it waits for a header nothing writes and the
      replica never answers. The decision moves into the connector as its own flag and the renderer
      reads that. The three touched files outside the stated Owns are where that carrier had to be
      threaded, and the shared gate shrank to what the three decisions still agree on.
      THE CASE THAT CATCHES THE INHERITANCE HAD TO BE WRITTEN, because every existing decode fixture
      sets both flags at once and passes whichever one the renderer reads. Measured: reverting the
      renderer to read the transfer leg leaves every one of those green, and turns red only the row
      whose decoder carries the leg without the proxy.

- [x] **T13 · The two fields, on the API and in admission**
      Blocked by: T10
      Owns: `api/worker/v1alpha1/**`, `pkg/worker/webhooks/worker/**`
      Gate: review
      Acceptance: both fields declared with their bounds and their documentation, the threshold
      refused on the two routers that have no concept for it, and the timeout's documentation stating
      what each router does when it is unset rather than implying one answer.
      Verify: `make generate` then `git status --porcelain` empty;
      `go test ./pkg/worker/webhooks/worker/... ./api/...`

- [x] **T14 · Rendering the two fields, and the access log**
      Blocked by: T13
      Owns: `pkg/worker/kvcache/router/**`,
      `pkg/worker/controllers/worker/model_deployment_router.go`,
      `pkg/worker/controllers/worker/model_deployment_router_test.go`
      Gate: review
      Acceptance: the timeout renders into the proxy template and into both argument lists; the
      threshold renders into the picker's document; the access log renders on the llm-d branch alone.
      The template is asserted on its rendered text, including that no placeholder survives.
      Verify: `go test ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/...`
      DONE. THE TEMPLATE IS SUBSTITUTED RATHER THAN FORMATTED, because it is full of Envoy's own
      percent-delimited operators and a format verb would either mangle them or need every one
      escaped -- a second copy of that format language living in Go's. Substitution is what makes
      the surviving-placeholder assertion worth writing: it fails quietly, leaving its own text in a
      duration field, and the proxy then does not start with nothing saying why.
      The pinned render digests moved once here, by intent, and their header records it.

- [x] **T15a · The build learns to publish the image, and ships ahead of everything else**
      Blocked by: T1
      Owns: `pack/llm-router/**`, `.github/workflows/base-image.yml`
      Gate: review
      Acceptance: the image joins the workflow's own list of what it can build, and joins the shorter
      list of builds that take the larger runners whatever changed — the detection those two lists
      exist around reads the tip commit only, so an image whose build file landed earlier is
      otherwise dispatched onto a runner it does not fit. Both architectures, which needs no entry
      because both is the default.
      Verify: the workflow builds and publishes `gpustack/llm-router:v0.1.0`, and each of the three
      binaries in the published image reports its own version.
      THIS IS ITS OWN PULL REQUEST and it merges first. See F9: a setting naming an image no registry
      holds is a deployment that cannot start, so the image exists before anything names it.

- [x] **T15b · The setting names the image**
      Blocked by: T15a, and on the image existing in the registry rather than merely being buildable
      Owns: `pkg/worker/settings/value.go`, `docs/settings.md`
      Gate: review
      Acceptance: the router image setting names `gpustack/llm-router:v0.1.0`; its documentation says
      the binary is chosen by the rendered command rather than by the image, which is what makes one
      image serving three routers legible to whoever reads the setting rather than the renderer. The
      proxy image setting and the routing-sidecar image setting are both untouched.
      Verify: `go test ./pkg/worker/settings/...`; `make lint docs < /dev/null`
      DONE, and the blocker is cleared by observation rather than by assumption: the tag exists in
      the registry with both architectures published. The pinned render digests moved a second time
      here, because the default image is part of every rendered router Pod.

- [x] **T16 · The reference page**
      Blocked by: T4, T10, T14, T15b
      Owns: `docs/reference/model-deployment.md`
      Gate: none
      Acceptance: the three routers, the engine-and-router table, both fields with their per-router
      unset defaults, and the east-west boundary. The sentence that scopes the transfer leg to one
      router in front of one engine is widened with them (`docs/reference/model-deployment.md:302`);
      the rename changed the value in it and left its scope claim standing, which is the shape a
      value-only rename leaves behind. The page gains no new second-level heading: it carries nine
      against a cap of ten, and any heading it did gain would also have to appear in its own contents
      list.
      Verify: `make lint docs < /dev/null`
      DONE. The page gained the pairing table, both fields with their per-router unset defaults, and
      the access log, and it stayed at nine headings by putting them under one new third-level
      heading. The scope sentence was widened where the transfer leg widened. Three paragraphs had
      to be split to stay inside the page's own five-line cap, which is the gate that catches an
      addition written as prose rather than as a table.

- [x] **T17 · Prove the reading before trusting it**
      Blocked by: T2, T12, T14, T15b
      Gate: review
      Acceptance: the reading from T2 is taken after a forced transfer and reads as having happened,
      AND is taken with the threshold at zero and reads as not having happened. Both halves, because a
      reading that cannot say "no" cannot be trusted when it says "yes".
      Verify: the two readings, taken on the cluster shape that renders the transfer leg, recorded
      with the image tag under test.
      DONE, and on BOTH hardware shapes rather than the one this task asked for. Each shape produced
      the positive half and a negative half taken beside it in the same run, on an unchanged Pod
      pair — the pair being unchanged is what makes the two halves comparable, and it was read off
      the Pods themselves rather than assumed. The negative half is an ENTIRE LINE THAT DOES NOT
      APPEAR, not a counter reading zero, and which of those two shapes a silent engine produces was
      settled by measurement before either half was trusted. On the second shape the instrument was
      checked a third way: the transfer counter is a process-wide running mean, so switching the leg
      off DILUTES it rather than zeroing it, and the diluted values match the token arithmetic they
      should. A reading that can only ever go up would have passed a naive check here.

- [ ] **T18 · The cluster cases**
      Blocked by: T17, T16
      Gate: review
      Acceptance: the scenarios in the Test Plan below. Every case that reads a transfer is accepted
      on that reading rather than on a request having succeeded. The case fronting a deployment with
      no split is the exception and is named here so the rule is not read as covering it: it has no
      transfer to read and is accepted on the router having been the hop that answered.
      Verify: the end-to-end cases, run on the two cluster shapes the Test Plan names.
      NOT DONE, and these are the readings rather than a summary of them. Twelve cases ran: eight
      passed every check in their own table, ONE REFUSED TO RUN AT ALL, and three had at least one
      check fail. The one that refused is counted here as outstanding and NEVER AS A PASS — its gate
      exits zero, so a suite-level "all checks passed" covers a case that executed nothing.
      Separately, the pairing rounds reached a verdict for two of the four router-and-engine
      combinations; the two SGLang rounds produced NO TRANSFER READING IN EITHER DIRECTION, because
      the engine died in its store warmup before a leg could be exercised, and the cause of that is
      not established — one attempt at a fix MOVED the failure to a different call rather than
      clearing it, which is not the same thing and is recorded as not the same thing.
      WHAT DOES NOT CLOSE THIS: a green suite run whose green includes that skipped case; a rerun of
      the three failing cases alone, since two of the three had their cause repaired elsewhere and a
      rerun must show the repaired cases passing AND the third one still measured; a SGLang round
      that answers requests correctly, since a pair with no leg between it answers correctly too; and
      an attribution made from the set of files a fix touched, which is not the same evidence as
      having rerun the case on the base it failed on.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

The pinned render digests are re-baselined once, in T5, by the procedure their own header states.
They are a tripwire for movement nobody intended; writing the member index on every member is
movement this document intends, so the baseline moves with it rather than the tripwire being
loosened.

The end-to-end suite has no reading that a transfer happened — every existing case that mentions the
transfer stack asserts what was RENDERED. T2 names the reading and T17 proves it can say "no" before
any case depends on it, because a suite built on an unproven instrument reports the instrument, not
the product.

#### Unit tests

Per-package coverage before this work, measured 2026-09-20:

- `pkg/worker/kvcache/router`: `2026-09-20` - `95.0%`
- `pkg/worker/kvcache/inject`: `2026-09-20` - `87.6%`
- `pkg/worker/controllers/worker`: `2026-09-20` - `80.3%`
- `pkg/worker/webhooks/worker`: `2026-09-20` - `91.4%`

Each task's own tests are named in its acceptance. Four assertions are called out because they fail
open rather than closed — they pass while the thing they describe is broken:

- The discovery selector under mixed sizes: two server roles of different sizes, and a multi-member
  prefiller beside a single-member decoder. The selected set is the leaders of multi-member roles
  together with every Pod of single-member roles. A version that narrows only multi-member roles
  passes every other assertion in the suite.
- The rendered arguments of both single-process routers name the transfer connector, both bind
  addresses and the discovery namespace. Each of those has an upstream default that produces a router
  which starts, routes, and silently fails at one leg.
- The owned-argument catalog refuses each new router's derived arguments, asserted per router rather
  than once. The lookup misses into an empty list, so a router without a row refuses nothing and the
  single assertion that exists today would still pass.
- The rendered proxy template contains the access log's fields and no surviving placeholder, asserted
  on the text. The template has no schema, and its last defect was a field name on the wrong message.

#### Integration tests

None separate from the unit and end-to-end tiers. The admission rules are exercised through the
webhook's own table, and the rendering through the object renderers; neither has a middle tier in
this repository. Concrete test names are added to this section after the implementation lands.

#### e2e tests

Every acceptance below is a reading that cache blocks moved. A served request is not evidence: the
defect the patch repairs, a missing bootstrap annotation, and a prefiller whose bootstrap server
never started all produce a correct answer and no error.

**The cases split across two cluster shapes, and the split is structural rather than convenient.**
`modelDeploymentRoutesManaged` gates all three connector decisions on the accelerator not being
Ascend (`pkg/worker/controllers/worker/model_deployment_connector.go:164-165`), so on Ascend no KV
events, no transfer leg and no decode proxy are rendered at all. The router's own objects carry no
such gate: that renderer never reads the manufacturer. So a cluster of NVIDIA accelerators is the
only place the transfer readings exist, while an Ascend cluster is where the router's own lifecycle,
its discovery and an undivided deployment behind it are exercised on the shape that renders no
transfer leg. Neither cluster can stand in for the other.

**The reading is two carriers, one on each side of the pair, because neither alone can say "no".**

**The decoder's carrier states its negative.** `vllm:external_prefix_cache_hits_total`, scraped from
the decode role's engine. The scheduler sets the hit count to the number of tokens the connector
reports it will supply from remote
(`vllm-project/vllm@v0.25.1:vllm/v1/core/sched/scheduler.py:757`, recorded at `:936-944`), and the
counter is registered at engine startup under no condition
(`vllm-project/vllm@v0.25.1:vllm/v1/metrics/loggers.py:583-592`, incremented at `:1099-1100`). **Its
no-transfer reading is therefore the value zero, present — not an absence.** This repository already
reads that carrier and already records its zero, in `cases/case-73.sh:23,507,595`. It counts what the
decoder was SCHEDULED to take, before a byte moves, so it is necessary and not sufficient.

**The prefiller's carrier does not, and the shape of that is the trap.** The engine log line
`KV Transfer metrics: Num successful transfers=<N>, ...`
(`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/metrics.py:103`). The count
is the length of the transfer-duration series
(`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/mooncake/stats.py:133`,
computed at `:144-146`), appended only where the transfer call returns zero
(`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/mooncake/mooncake_connector.py:1630-1642`).
It is read on the PREFILL Pod, because Mooncake pushes: upstream states at that file's `:593-601`
that the producer records successful transfers and the decoder records only failures, so the same
reading taken on the decoder is zero in every case and says nothing. Three states are
distinguishable, and the third is what makes the second worth having:

- blocks moved — the line is present and the count is at least one;
- a transfer was attempted and failed — the line is present, the count is zero and the failure count
  is at least one, because the emptiness test is false while any failure is recorded
  (`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/mooncake/stats.py:85-91`);
- nothing was attempted — **the line is not emitted at all.** Two guards make it absent rather than
  zero: the worker returns no statistics when its container is empty
  (`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/mooncake/mooncake_connector.py:1812-1817`),
  and the logger prints only a non-empty accumulator
  (`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/metrics.py:95-98`).

**The zero branch that the reducer does carry
(`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/mooncake/stats.py:106-117`)
is unreachable from an idle interval.** Reading that file alone says the instrument states its zero;
reading its two callers says it does not. That absence has at least four causes — nothing was
transferred, the reading was taken on the wrong Pod, no connector is configured, or the previous
logging interval already consumed and reset the accumulator
(`vllm-project/vllm@v0.25.1:vllm/distributed/kv_transfer/kv_connector/v1/metrics.py:106`). **Three of
the four are eliminated by taking the positive half of the same run from the same Pod**, which is
what the negative control below is for and why it is a control rather than a case.

Both carriers require the engine's statistics to be on
(`vllm-project/vllm@v0.25.1:vllm/v1/core/sched/scheduler.py:141-142`). Nothing in this repository
renders `--disable-log-stats`; a deployment that supplies it through extra arguments loses both
readings, which is stated rather than refused, because the argument is the user's.

1. **The `MultiConnector` assertion (`#454`).** A shared cache pool beside prefill and decode roles,
   a forced transfer, and two readings: that the transfer happened, and that the engine process did
   not abort. The case names the image tag it ran against. Starting without an abort is not evidence,
   because the assertion only fires once a transfer has produced statistics.
2. **One pairing per admitted router-and-engine combination**, the llm-d router with SGLang included.
   That pair is newly rendered here and differs from the same router's vLLM pair by one argument, so
   a suite covering only the vLLM pair stays green while the new one transfers nothing. Both SGLang
   rows need a shared cache pool to deploy at all, because store-less rendering is refused for every
   engine but vLLM: written like the vLLM rows they produce a render failure and no Pod.
3. **Discovery under mixed sizes**, per router: the endpoints a router actually discovers, in a
   deployment whose roles do not agree on size.
4. **Discovery across the upgrade.** A deployment created before this change, then the operator
   replaced: discovery must not empty except for the roll that replaces its Pods.
5. **A restarted prefiller.** Force a transfer, restart the prefill Pod so its engine identity
   changes, force another, and READ the second. The first transfer alone passes even when the lookup
   is resolved once and cached forever, which is the second half of the defect the patch repairs.
6. **Sidecar placement.** Decode Pods under the two single-process routers carry no sidecar and the
   engine holds the serving port; under the llm-d router with SGLang the sidecar carries the SGLang
   handshake and a bootstrap port equal to the one rendered on the prefill role.
7. **The annotation as the only source.** Prefill Pods carry the bootstrap annotation equal to the
   rendered argument, and a transfer is then forced in which the annotation is the only place the
   port could have come from.
8. **A threshold of zero, under `llm-d-router` alone.** The decoder's counter stays at zero and the
   prefiller's line does not appear, in a run whose positive half produced both from those same two
   Pods. This is the negative control, and it is why the instrument is trustworthy when it reads
   "yes". The prefiller's half is an absence rather than a stated zero, so it is a negative only
   beside that positive half.

   **Its scope is one router, and the other two are recorded as uncovered rather than assumed
   covered.** `disaggregationThresholdTokens` is refused on the other two routers (F6), so neither
   has this carrier. What does NOT fill that gap: a positive reading on those two routers, which
   establishes that the instrument can read "yes" there and says nothing about whether it can read
   "no"; nor this router's negative, whose validity rests on the threshold's own code path and
   therefore transfers to no router that lacks the field. A carrier for the other two is left open.
9. **Admission.** A role named `router` is refused; each new router's derived arguments are refused;
   the reserved-port rule is asserted per router rather than per engine.
10. **A deployment with no split, fronted by a router.** One undivided serving role behind a router,
    answering a request end to end. Every case above accepts on a transfer reading, so not one of
    them fails when routing to a deployment that has no split breaks -- a role fronted without a
    split has no transfer to read, and the suite's whole instrument is blind there. The acceptance
    is the answer together with the router having been the hop that produced it; the answer alone is
    also what a client reaching the role directly produces.

**One combination is named as uncovered rather than left to be discovered.** On an Ascend
accelerator the gate the connector decisions share admits no router, so the two routers added here
render no transfer leg and no decode proxy while the deployment still reports ready. No case above
can see it: every one of them accepts on a transfer reading, and a pair with no leg still answers
correctly because the decoder does the whole inference itself.

**It is documented rather than refused, by decision.** Admission can read the manufacturer -- the
webhook already fetches each role's InstanceType from the API server, and its status carries the
field -- so a refusal was available and was not taken: the combination is legal and serves requests,
and refusing it would take a working deployment away to prevent a slower one. What a reader gets
instead is API text naming which router does move blocks on that accelerator.

What settles the gap is a transfer reading on those two pairs. What does NOT count: that text, which
says what is known rather than measuring it; a transfer reading from the one pairing that does move
blocks there, which is a different render path and leaves these two exactly as they were; nor a unit
test over the gate, which pins what the gate returns rather than what the user is told when it
returns false.

## Alternatives

**A `kind` field beside `name`, with a `native` value meaning "whichever router matches the
engine".** Rejected. Two fields would answer one question, and their product would need admission
rules for the combinations that mean nothing. `kind` is already taken inside this resource by a
role's kind, so the word would carry two unrelated meanings in one object. And `native` would have a
value that renders nothing: SGLang cannot declare prefill and decode roles here, so that pairing
would be a legal spelling with no product — the shape this repository deleted a mode field to avoid.
Widening the enum expresses the same capability, and adding a resolution strategy later is an
optional field with a default, which is compatible.

**Renaming the field from `name` to `kind` or `type`.** Rejected. `spec.router.name` is the same
shape as `spec.engine.name` — a closed enum naming an implementation, frozen after creation, on the
same object — and renaming one without the other would break a real symmetry. `kind` is taken.
`type` has precedent elsewhere in this repository but not inside this resource, where the existing
spelling for "which implementation, frozen after creation" is `name`.

**Basing the unified image on the published endpoint-picker image.** Rejected on a measured fact:
that image's runtime layer is static-only distroless, with no shell, no package manager and no
dynamic loader, so two dynamically linked Rust binaries cannot be installed into it.

**Keeping the mirrored endpoint-picker default and adding a second setting for the new image.**
Rejected. It reads as the cautious option and is not: two settings that both resolve a router leave
the cluster to work out which one a given `spec.router.name` reads, and the deployment would run one
picker built by upstream beside two routers built here — three provenances for one enum. It also has
nowhere to put the vLLM router patch, since the patched binary must be in the image the workload
actually runs. The cost of replacing is real and is stated in Notes rather than avoided.

**Installing the two Rust routers from their published Python wheels instead of building them.**
Rejected. Both are published, and both wheels ship a shared object plus a Python launcher rather
than a standalone binary, so the image would carry a Python runtime and a web framework stack to
start a process that needs neither. Building from source yields three binaries and keeps any
upstream reference buildable, released or not.

**Accepting the disaggregation threshold on every router and ignoring it where it means nothing.**
Rejected for the reason in F6: a legal field that renders nothing is a shape already rejected twice
here.

**Exposing the two security flags as fields.** Rejected by the boundary in F8. The question is not
whether the values are right for one cluster; it is that the router is not where that policy is
expressed.

**Splitting this into two pull requests — the configuration surface, then the widen.** Considered,
and the trade was real: the widen is the larger half, and the two new routers had no footprint in
this repository at all. It was rejected because one spec ships with its implementation in one pull
request here, and because the fields' shapes depend on the widen — whether a field means the same
thing under three routers is not answerable while only one exists.

**Supporting only `server` roles under the two new routers, deferring disaggregation.** Rejected.
It was proposed on the reading that the vLLM router's disaggregation could not work with the
connector this operator renders, and the reading was right about the defect and wrong about the
remedy: the break is one lookup wired to the wrong source, in a project this image already builds
from source, so repairing it costs a patch rather than a redesign. Deferring would also have left
the enum holding two values that route a pool but cannot pair, which is the shape users would
discover rather than read.

**Rendering an alternative vLLM transfer connector for the vLLM router instead of patching.** Its
pull-based path needs no engine-id lookup at all — the router forwards what the prefiller returned —
so it sidesteps the defect entirely. Rejected for this round because it introduces a second transfer
stack beside the one every other part of this operator is built on: the cache backend, the pools and
the store connector are all Mooncake, and a deployment whose direct leg and shared store disagree is
a shape nothing here has measured. Recorded as OQ1 rather than closed, because it remains the right
answer if the patch ever becomes unmaintainable.

## Open Questions

**OQ1 — Should the vLLM router's disaggregation eventually use a pull-based connector instead of
this repository's patch?** Deferred deliberately, not undecided: the patch ships now, and this
question is what to do when the patch's cost changes. It becomes live if upstream restructures the
area so the patch stops applying cleanly, if upstream declines the report that accompanies it, or if
a deployment appears that wants a transfer stack other than Mooncake on the direct leg. Answering it
means rendering a second connector in the vLLM injection path and deciding what happens when the
direct leg and the shared store name different stacks — which is why it is not a follow-up task but
a design question.

**OQ2 — ANSWERED, and the answer leaves F2 and F4 standing. Does the SGLang gateway route to workers
that carry no model id? No — and no worker this operator renders is one.** Turning on Kubernetes
service discovery also turns on the gateway's inference-gateway mode, which it does by itself and
without a way to decline (`sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:1057-1059`),
and the discovery path registers every worker it finds with no model id at all
(`sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/service_discovery.rs:381`). That mode's
routing is model-aware, and it matches exactly: measured against a discovered worker reporting no
model, a request naming a model is refused by the gateway itself with `no_available_workers` and
never reaches the worker, while the same worker serves a request naming what it did register under.

**The measurement's consequence is not the one the question anticipated, because the missing model
id is a starting value rather than a final one.** The registration id comes from the worker's own
`/server_info`, falling back through its served model name and then its model path before it becomes
the placeholder. This operator renders SGLang as `--model-path <spec.model.name>`
(`pkg/worker/controllers/worker/model_deployment_connector.go:511`), and SGLang defaults its served
model name to that path (`sgl-project/sglang@v0.5.9:python/sglang/srt/server_args.py:849-850`), so a
worker this operator renders always registers under `spec.model.name`. The router is handed that same
field (`pkg/worker/controllers/worker/model_deployment_router.go:164`). **Both ends read one field, so
the shape that fails is unreachable here**, and `sglang-gateway` needs no discovery shape of its own.

What the answer does change is the acceptance case: it asserts the registration id, not merely that a
request was served. The precondition it rests on — that the name a caller requests equals
`spec.model.name` — is now stated rather than assumed, because a deployment whose callers use some
other name gets the refusal above from a router that is working correctly.

**OQ3 — Which engine versions does the `#454` case run against?** The fix reaches pinned published
images on one path and later builds on another, so the case has to name a tag from one of those two
sets. The set is known; the specific tag is chosen when the case is written.
