# Spec: KV Cache Backend Leader High Availability

Status: Shipped
Type: Feature

## The image this needs

`highAvailability` requires a store image built from
[`pack/mirrored-mooncake/Dockerfile`](../pack/mirrored-mooncake/Dockerfile). **No published
`kvcacheai/mooncake` image can run it**, and that is a property of the artifact rather than of
upstream: leadership backend availability is a compile-time switch, `ValidateHABackendAvailability`
(`ha/ha_types.h:55-80`) is pure `#ifdef` dispatch, and every option ships `OFF`. Measured against
`kvcacheai/mooncake:0.3.13` — arm64 and amd64, `.post1` included, no `-ha` variant among the nine
tags — all three of `k8s`, `redis` and `etcd` answer `UNAVAILABLE_IN_CURRENT_MODE` from
`master_service_supervisor.cpp:515`.

**REQUIRED: both roles need it.** A member does not elect, but it does have to find the leader, and
F4 hands it a `k8s://` entry the client validates against the same switch. On a published image a
leader runs as a permanent standby, and a member answers
`client_service.cpp:639 Invalid HA backend entry` and exits after twenty retries. The two failures
are different lines, so testing only the leader does not cover the member.

⇒ **a member group on `RDMA`, `HIP` or `Ascend` cannot run under HA.** The vendor axis and the
leadership axis are orthogonal, so covering them means rebuilding each variant rather than adding one
image. Open at https://github.com/gpustack/gpustack-operator/issues/279, together with the untested
alternative of leaving members on the leader Service address and letting readiness move the endpoint.

**REQUIRED: this image has no oplog, and the choice is not revisitable at deploy time.**
`STORE_USE_K8S_LEASE` and `STORE_USE_ETCD` are mutually exclusive upstream (`CMakeLists.txt:75-86`,
both build Go c-shared libraries), and `master.cpp:1456` raises `LOG(FATAL)` on `enable_oplog` unless
the backend is etcd. So a standby rebuilds from snapshot bootstrap and remounts, never by replay.
Snapshot bootstrap is independent of the backend type (`standby_controller.cpp:42`) and remains
available.

### The four-case probe is a standing gate, not a measurement

It lives in the Dockerfile's final stage and runs on every build. Two of the four cases are controls,
and they carry as much information as the subjects:

| `-ha_backend_type` | Asserted | What its absence would hide |
| --- | --- | --- |
| `k8s` | clears the check, `Master runtime state -> starting, role=standby` | the switch never reached the `#ifdef`. `k8s_lease_helper.cpp:24` reporting `stat /root/.kube/config` is what says the Go wrapper was CALLED rather than merely linked |
| `redis` | clears the check, reaches standby | a **stub** is compiled in when `STORE_USE_REDIS` is off and returns `UNAVAILABLE_IN_CURRENT_MODE`, so only a *connection* error — `redis_leader_coordinator.cpp:707` → `INTERNAL_ERROR` — distinguishes the real implementation from it |
| `etcd` | still `UNAVAILABLE_IN_CURRENT_MODE` | the availability check disabled wholesale, which would make `k8s` passing meaningless |
| `bogus` | `INVALID_PARAMS` | type parsing skipped, so every value would "pass" |

**FORBIDDEN as evidence here:** a successful build; a flag accepted without the behaviour behind it;
the container starting; and the exit code — the two subject cases are killed by a timeout, whose
status says only that time passed. The evidence is what the process printed.

The image is 469 MB, two-stage, and carries both `mooncake_master` and `mc_store_rest_server` from
one compile — smaller than the published image at 607 MB, which carries neither backend.

## Summary

A `KVCacheBackend`'s leader runs exactly one process today. `leader.replicas` refuses anything but
`1`, and its own doc comment names why: *electing a leader among several needs a backend store this
scope does not enter*. This spec enters that scope.

It does **not** implement high availability. The store already has one at the version this project
runs — leadership, oplog, snapshot and standby subsystems, seven runtime states, and a chaos suite.
What is missing is the Kubernetes orchestration around it, and that turns out to be **more than
flipping a flag and less than building a failover**.

Three pieces of real work:

1. **How HA is switched on, and what it depends on** — one empty field, five derived flags, the
   Kubernetes access the leadership backend needs, and two admission rules (F1, F2). The field is
   empty because the leadership record is a Lease carrying the leader's own object name, so there is
   no external component and no connection string for anyone to supply.
2. **The workload shape.** Neither default update strategy holds once there are N leader replicas:
   `Recreate` takes them all down at once, and `RollingUpdate` stalls against a `readyReplicas` that
   is pinned at 1 by design. This is a design question wearing the costume of a parameter (F3).
3. **The member's view of the master** (F4) and **what "healthy" means with N replicas** (F5).

One finding runs through all of it: **the observability of HA is very nearly free**, because two
earlier decisions — one in S2, one in S4 — both delegated a judgement to a single source of truth
("is the service plane active") instead of re-deriving it. Neither was made with HA in mind. Both
pay off here without a line of change.

Every claim about the store is cited to a file and line in its own source at **v0.3.13**
(`e5598b09`). The image this project builds is **v0.3.13.post1** (`71973589`), and the citations are
still exact: the four commits between the two tags touch CI workflows, `pyproject.toml`, one EP
Python module and `mooncake-store/src/CMakeLists.txt`, and not one file cited here. That last one is
the only behavioural change and it moves in this scope's favour — CUDA and HIP staging now follow
the explicit `USE_CUDA` / `USE_HIP` flags instead of detecting an SDK, so a build passing
`-DUSE_CUDA=OFF` in a toolchain image can no longer acquire CUDA by accident.

**REQUIRED: re-read the citations at the tag actually built before trusting them again.** They hold
across this bump because the diff was checked file by file, not because a `.post1` is small.

## Motivation

### Goals

- **G1 (primary)** `leader.replicas` may exceed 1. Failover is driven by the store's own lease, the
  client reconnects itself, and this operator is on **no** data path during it.
- **G2** A leader workload with N replicas can be **updated**. Today's strategy was chosen for
  `replicas: 1` and is actively wrong for N; the replacement is stated as a design with a reason,
  not as a changed field.
- **G3** The failure modes HA introduces are **visible**, and the signals used do not fire during
  ordinary transitions. A health rule that reports a fault on every successful failover is worse
  than the one it replaces.
- **G4** Without the HA dependency, a single leader stays the **default** and stays fully working.
  HA is opt-in, and nothing about this spec degrades the deployment that does not ask for it.
- **G5** No field is added that nothing reads. Three candidates in this scope fail that test and are
  refused below by name.

### Non-Goals

Each is out of scope by decision, and the subject that owns it is named so that "not yet" reads as
"not yet" rather than as an oversight.

- **An etcd of any provenance — shipped as a subchart, or the cluster's own.** The leadership record
  is a Lease, so there is no external store to run, reach or secure.
- **A cross-backend unified quota ledger.** This was the second half of this subject in the phased
  plan, gated on decision D2. D2's three trigger conditions are evaluated in
  [What is not here](#what-is-not-here-and-why) and **none holds**, so it is settled as not
  triggered rather than left open.
- **Draining a member's data before it goes away.** The store's `POST /api/v1/drain_jobs` exists and
  works, and this operator will not call it. Two reasons, both measured, in
  [What is not here](#what-is-not-here-and-why). Tracked as https://github.com/gpustack/gpustack-operator/issues/212 with the condition that would
  reopen it.
- **Reporting which replica is the leader.** No `status` field and no controller read of `/role`:
  reading the admin API from a controller is the interaction shape refused above, and F5 states what
  is reported instead.

  The store labels its own Pod `mooncake.io/store-role=leader` once it wins, so
  `kubectl get pod -l mooncake.io/store-role=leader` answers it from the Kubernetes side. That is not
  a feature this scope chose and not one it can decline: `HasPodIdentity` needs `pod_name` and
  `pod_namespace` non-empty and the backend to be `k8s`
  (`ha/leadership/master_service_supervisor.cpp:35-37`), and this operator has rendered both flags
  unconditionally since before HA existed (`leader_flags.go:100-102`).

  **REQUIRED: the Role in F1 grants `pods: patch` for that reason, not for the feature.** Withholding
  the verb does not switch the label off — the reconciler is a thread that logs `LOG(WARNING)` and
  retries every second forever (`ha/leadership/leader_label_reconciler.h:71-83`), never blocking
  election. It buys one warning per second for the life of every leader, which is worse than the
  grant.
- **Moving the tenant quota policy to a shared store.** F6 measures the window this leaves and
  settles it. The short version: the operator's own reconcile loop is the authority and re-converges
  the ledger within `kvCachePoolObserveInterval`, so the file-backed policy store does not lose a
  quota across a failover.
- **`enable_oplog`, and it is given up permanently rather than merely defaulted off.** It defaults to
  off upstream (`mooncake-store/src/master.cpp:310`) and it requires the etcd backend specifically
  (`:1456`). **REQUIRED: record the mechanism, not just the choice.** Reversing this is not a flag —
  `:1456` is a `LOG(FATAL)`, and `STORE_USE_K8S_LEASE` cannot be compiled with `STORE_USE_ETCD`
  (`CMakeLists.txt:78`), so wanting the oplog back means **rebuilding the image on a different
  backend** and giving up the Lease. F2 refuses the key at admission for that reason.

## Proposal

### What the store implements, and what is left to orchestrate

Every row read in the store's own source at v0.3.13.

| Fact | Where |
|---|---|
| HA is a full subsystem: `leadership/`, `oplog/`, `snapshot/`, `kv/`, plus a standby controller | `mooncake-store/include/ha/` |
| Seven runtime states: starting, standby, candidate, recovering, catching_up, leader_warmup, serving | `ha/ha_types.h:113` |
| HA is the **same binary and the same process**, entered by a different top-level call | `master.cpp:1624` — `enable_ha` true dispatches to `MasterServiceSupervisor::Start()` |
| `enable_ha` with an empty backend connection string is a startup failure, not a degrade | `master.cpp:1444` |
| It is tested against faults, not only unit-tested | `tests/ha/master_service_ha_test.cpp` (4413 lines), `tests/e2e/chaos_test.cpp` |

The word "abstraction" undersells this. It is not an interface waiting for an implementation.

#### The finding that decides the shape: failover is a client-side concern

This is the one that determines how much orchestration is needed, and it is the one most likely to
be guessed wrong.

- **The scheme prefix of an existing string selects HA.** `Client::ConnectToMaster` parses
  `master_server_entry` for a `://` prefix: a recognised backend type means HA, a bare address means
  a direct connection (`mooncake-store/src/client_service.cpp:593-612`, `:636`). There is **no new
  client flag**.
- **The client watches for a leadership change and reconnects itself.**
  `Client::LeaderMonitorThreadMain` blocks on the backend's watch and calls `SwitchLeader` as soon as
  the view changes (`client_service.cpp:719`).

**Therefore this operator does not orchestrate failover at all.** No Service selector has to follow
the leader, no leader label has to be reconciled, no endpoint has to be rewritten. The member's
existing master address gains a prefix, and everything else is the store's own business.

#### The backend, and why it is the Lease

The store ships three leadership backends — `ETCD`, `REDIS`, `K8S` (`ha/ha_types.h:21`). This
operator renders **`k8s` and only `k8s`**, because it only ever runs inside Kubernetes; the redis
half of the image serves the same binary under plain Docker, and nothing here configures it.

The Lease is not merely the most convenient of the three. The etcd backend **has no transport
security**: all three `clientv3.Config` sites pass only endpoints and timeouts
(`mooncake-common/etcd/etcd_wrapper.go:95`, `:148`, `:326`), the import set has no `crypto/tls`, no
`transport` and no `credentials`, and a repository-wide search for certificate, CA, username or
password handling returns nothing. Choosing it would have meant requiring an operator to run a
**plaintext, unauthenticated** store and keep it reachable. The Lease carries the same record through
the API server's own TLS and RBAC, and there is no component to run at all.

What it costs is the oplog, and that cost is exact rather than approximate: `STORE_USE_K8S_LEASE`
cannot be compiled with `STORE_USE_ETCD` (`CMakeLists.txt:78`, both build Go c-shared libraries), and
`master.cpp:1456` is a `LOG(FATAL)` when `enable_oplog` is set with any other backend. With the
defaults the etcd path would have held only the leader's address and a view counter
(`etcd_leader_coordinator.cpp:577`) — the tenant names and cache keys are the **oplog's** row
(`master_service.cpp:2074`), and it ships off (`master.cpp:310`). So nothing of value was traded
away with it.

**FORBIDDEN: reaching for `STORE_USE_ETCD` to get the oplog back.** It is not a build flag away; it
means giving up the Lease and reintroducing the external store above.

#### Two earlier decisions to do less pay off here

Both were made for local reasons. Both make HA observable at no cost, and neither was written with
HA in view. They are recorded because the pattern is the reusable part.

1. **The readiness probe asks a gated route.** It targets `/get_all_segments`, chosen because the
   route is wrapped in a service-plane check and answers `503` until that plane is up
   (`pkg/worker/kvcache/mooncake/leader_workload.go:44` and its comment;
   `master_admin_service.cpp:546`). Under HA, only `ActivateServingState` marks the plane available
   (`ha/leadership/master_service_supervisor.cpp:162`), and the leadership-lost callback clears it
   **first**, before anything else (`:441`). So a standby is not ready, and a leader that loses its
   lease stops being ready — with no change to the probe.
2. **`leaderPodIsReady` does not form a second opinion.** It reads `ReadyReplicas > 0` and its
   comment says why: readiness was already settled by Kubernetes from the gated route, and
   re-deciding it here could disagree (`pkg/worker/controllers/worker/kv_cache_backend.go:510`).
   That predicate is *also* the correct one for N replicas, for a reason its author did not need.

**The general form:** both delegated to one source of truth rather than deriving a parallel answer.
The payoff arrived two subjects later. `/health` and `/ha_status`, by contrast, answer `200`
unconditionally and carry the state in the body (`master_admin_service.cpp:493`, `:577`) — they are
**not** usable as probes, and the route already in place is better than the one they suggest.

### What is not here, and why

Two exclusions carry measurements rather than preferences.

**D2's trigger conditions do not hold.** D2 gated a cross-backend control plane on three conditions
holding together. (1) Two or more backend types *the store cannot absorb itself* — since the store
grew a `DistributedStorageBackend`, heterogeneity is absorbed in the storage layer, so the condition
that used to be satisfied by "NFS and 3FS" no longer is. (2) A quota ledger spanning those types.
(3) Automatic spillover between them. None holds; the first cannot hold today. **D2 is settled as
not triggered**, and the second half of this subject goes with it.

**The drain job is not called from a controller.** Not because calling the admin API is forbidden —
`convergeTenantLedger` already does, including writes (`kv_cache_pool.go:954`, `:976`, `:1006`) —
but because of what *kind* of interaction it is:

| | `convergeTenantLedger` | a drain job |
|---|---|---|
| Shape | Read observed, compare to desired, write the difference | Create, poll, decide it finished |
| Idempotent | Yes; the whole pass re-runs every 30s | No; re-entry must first ask whether the previous one is still alive |
| On failure | Next pass heals it | Its lifecycle has to be managed |

The repository's Kubernetes convention — reconcile continuously rather than execute an imperative
workflow — separates these two precisely, and it is the second that is refused.

A second, independent reason is measured: **the identifier a drain would key on is not unique in the
configuration that needs it most.** A segment's name is the value this operator renders into
`MOONCAKE_LOCAL_HOSTNAME` verbatim — the client stores it and neither it nor the leader rewrites it
(`client_service.cpp:3530`; the field is `const`, `client_service.h:947`). On the RDMA path members
run with `hostNetwork`, so the pod IP is the node IP and **every member group on one node reports the
same segment name** (`member_workload.go:479`). Drain takes segment names
(`CreateDrainJobRequest.segments`, `rpc_types.h:214`), so a member draining itself would drain its
neighbour. This is the same collision the members-mounted condition already reports as
`AmbiguousMemberIdentity` rather than guessing at — and a drain has no equivalent of reporting the
ambiguity: it either acts or it does not.

Both are recorded on https://github.com/gpustack/gpustack-operator/issues/212, with the condition that reopens it: **a segment name that identifies a
member group uniquely**. That is changeable without upstream — the name is this operator's own
rendered value — but it moves the pod template, so it rebuilds every member DaemonSet and drops the
cache. https://github.com/gpustack/gpustack-operator/issues/217 carries an identical cost for an unrelated reason, so **the two should be done in one
rebuild, never two**.

### User Stories

#### Story 1

An operator runs a KV cache backend that several inference deployments read from. Restarting the
leader to change a flag currently empties the index for everyone. They set `leader.replicas: 3`, and
a leader restart moves service to a standby instead of interrupting it.

#### Story 2

An operator applies a change to the leader's flags on a backend with three replicas. The rollout
completes. Under today's strategy it would either take the whole leader down or never finish.

#### Story 3

The leadership backend becomes unreachable. The operator looks at the `KVCacheBackend` and sees that
it is not serving, rather than seeing three healthy replicas none of which is a leader.

#### Story 4

An operator has no leadership backend and does not want one. They never set the field, the leader
runs as a single replica exactly as before, and nothing in the object suggests they are missing a
part of it.

### Core Features & Acceptance Criteria

#### F1 — `leader.highAvailability`: the switch, and the access it needs

One new field on `leader`, and it carries **nothing**. The leadership record lives in a Lease named
after the backend itself, so there is no connection target for a user to supply.

```go
// HighAvailability elects the leader through a Kubernetes Lease, and it is what allows more than
// one replica. Unset, the leader runs as a single process exactly as before.
//
// It has no fields: the Lease this backend elects through is named after the backend, and the
// access it needs is rendered alongside it. Present with an empty body is the way to turn HA on.
type KVCacheBackendLeaderHighAvailability struct{}
```

**REQUIRED: it stays a struct rather than becoming a `bool`.** A `bool` admits `enabled: false` next
to `replicas: 3`, which is a third state F2 would then have to adjudicate. Presence has no such state,
and lease tuning (duration, renew deadline) can be added later without a breaking change.

Everything else is derived, and each is derived rather than exposed because nothing else would read
it:

- **`-enable_ha`** whenever the field is present. Never an `extraArgs` key: rendering it without the
  connection string it needs is a process that fails at startup (`master.cpp:1444`), so the pair is
  rendered together or not at all.
- **`-ha_backend_type=k8s`.** Not a field. It is the only backend this operator can use — the image
  also carries `redis`, but that one exists for the same image under plain Docker. A single-value
  enum in an API is a name, not a choice; widening it later is not a breaking change.
- **`-ha_backend_connstring=<namespace>/<name>-leader`.** Not a field. `ParseConnstring` takes
  `namespace/lease-name`, falling back to the `default` namespace for a bare name and refusing a
  second `/` (`ha/leadership/backends/k8s/k8s_leader_coordinator.cpp:423-450`). Two backends must not
  share a Lease, and the object already carries a unique identity — the same argument that decides
  `cluster_id`, now doing double duty.
- **`-cluster_id`** derived from the backend's namespace and name. The artifact's default is a fixed
  string, so leaving it alone is what would collide.
- **`-rpc_address=$(KUBERNETES_POD_IP)`**, and this one does not look like it belongs to the group.
  It is what the election ADVERTISES, not just what the process binds. `master_config.h:347` folds
  it with `rpc_port` into `local_hostname`; `ha/leadership/master_service_supervisor.cpp:269`
  campaigns with exactly that string; `k8s_leader_coordinator.cpp:159` makes it both the election
  identity and the `MasterView.leader_address` written into the Lease; and `client_service.cpp:709`
  is a member connecting straight to it. Left at its `0.0.0.0` default (`master.cpp:162`), every
  replica campaigns under ONE identity and every member following the Lease is sent to an address
  that resolves back to itself. The Pod IP is the only value that is both unique per replica and
  reachable from another Pod, and it binds correctly too — the same string reaches the RPC server
  (`master_service_supervisor.cpp:375`).

  **This is why `rpc_address` and `rpc_interface` left the `Exclusive` group for `Derived`.** They
  were an either-or pair while nothing rendered them; now a passthrough would not shade a setting,
  it would decide which host every member connects to, with no say in whether the Pod answers
  there. `rpc_interface` goes with it because the artifact resolves it INTO `rpc_address`
  (`master.cpp:430-446`), so leaving it reachable would be the same override by another name.

  **LIMITED: no unit test can see this one.** It is an omission rather than a wrong value — the
  renders were self-consistent and every assertion passed while the feature could not work on a
  cluster. It is the clearest argument for T9.

**The access, which is the part with no precedent in this operator.** The lease backend talks to the
API server: `rest.InClusterConfig()` first, then `$KUBECONFIG`, then `$HOME/.kube/config`
(`mooncake-common/k8s-lease/k8s_lease_wrapper.go:90-100`). Today neither Pod can reach any of them,
because `leader_workload.go:249` and `member_workload.go:253` both set
`AutomountServiceAccountToken: false` — the leader's with a comment saying nothing in the workload
talks to the API server, **which this image falsifies**. So HA renders **two** sets of three objects
in the shared system namespace where a backend's other objects already live, owned by the
`KVCacheBackend`:

| Set | `Role` grants | Why that set |
|---|---|---|
| leader, named `<backend>-leader` | `coordination.k8s.io`/`leases`: `create`, `get`, `update`; `""`/`pods`: `patch` | WIN the election, and label its own Pod |
| member, named `<backend>-member` | `coordination.k8s.io`/`leases`: `get` | FIND the leader, and nothing else |

Each set is a `ServiceAccount`, a `Role` and a `RoleBinding`; the matching PodSpec names the account
and flips `automountServiceAccountToken` to `true`. `Role`, never `ClusterRole` — everything named is
in one namespace and nothing reads across. One member account per BACKEND rather than per group,
because every group of one backend reads one Lease.

**REQUIRED: the two sets are separate, and that is the point.** A member is a client. Sharing the
leader's account would hand every member `update` on the Lease, which is the ability to take
leadership from the leader it is supposed to be following.

**REQUIRED: the verbs are read from what the C++ side CALLS, not from what the Go wrapper offers.**
The two differ. The wrapper exports `K8sLeaseWatchHolder` and issues a collection `Watch`
(`k8s_lease_wrapper.go:407`), but **nothing in the C++ tree calls it** — only the etcd helper's watch
has callers, and `K8sLeaderCoordinator::WaitForViewChange` re-reads the holder every 200 ms instead.
A verb list derived from the wrapper's surface therefore includes `watch`, which nothing uses — and
the test asserting the exact verb set cannot catch that, because the renderer and the test are
written from the same reading and agree by construction.

**LIMITED: that re-read is a POLL, and it is on the member's side.** Each member issues roughly five
Lease reads per second for as long as it runs, so the API-server cost scales with the member count
rather than with the failover rate. The client's own comment claims the opposite — it says the wait
"blocks on a backend watch" and collapses load to one read per thirty seconds — and that is true of
the etcd and redis coordinators, not of this one.

**LIMITED: the same missing access fails LOUDLY on one side and silently on the other**, which is
the fact to carry into any runbook. Both are measured.

| Side | Without its account |
|---|---|
| leader | `Init` returns `K8S_LEASE_OPERATION_ERROR`, the coordinator retries every second **forever**, the process never exits. Liveness targets `/health`, ungated and `200` regardless, so nothing restarts and nothing crash-loops — the Deployment sits at `0/N` ready |
| member | The client retries the read twenty times, then the entrypoint raises `RuntimeError: Failed to start store service` and the process **exits 1** — a CrashLoopBackOff |

F5's predicate does catch the leader case as unhealthy, but as "not ready", never as "the Role is
missing". That asymmetry is why the RBAC is rendered rather than left to whoever writes the manifest:
half the failure is invisible.

- **Acceptance:** a backend with `highAvailability` set renders all five flags plus both sets of
  objects, and one without it renders **none** of them and leaves `automountServiceAccountToken` at
  `false` on both workloads — the same Pods and the same command line as before the field existed.
- **Acceptance:** the advertised address is a REFERENCE the Pod resolves, `$(KUBERNETES_POD_IP)`,
  and the Deployment defines that variable from `status.podIP`. Asserted as unresolved argv paired
  with the variable's definition, because a literal would read perfectly in the golden list and give
  every replica the same one — which is the whole defect (see below).
- **Acceptance:** two backends render different `cluster_id` **and** different
  `-ha_backend_connstring` values. Asserted by comparing two renders rather than either against a
  literal, because the failure guarded here is SAMENESS, which a single golden list cannot see.
- **Acceptance:** each rendered `Role` grants exactly its row of the table above and nothing else.
  The test asserts the whole set rather than a subset, because a rule that over-grants passes every
  functional test there is. **LIMITED: exactness is all such a test can hold** — whether the set is
  the RIGHT one is a question for the store's sources, as the `watch` verb showed.
- **Acceptance:** removing `highAvailability` from a backend that had it deletes both sets rather
  than leaving them behind. The owner reference does not cover this: the backend is not deleted, only
  the field is, so a leader with no election left would keep an account that can still take a Lease.
- **Acceptance:** turning `highAvailability` on for a backend whose Deployment and DaemonSets ALREADY
  EXIST reaches both account fields. This is the aligner's job and no renderer test can reach it, and
  the two fields are one setting: an account named with no token mounted, or a token mounted for no
  named account, both authenticate as nobody and neither says so.

#### F2 — `replicas` widens, and two admission rules follow it

- The **minimum stays 1 and the default stays 1.** A `KVCacheBackend` that never mentions HA is
  unaffected in every respect, which is G4 stated as an admission-level guarantee rather than a
  convention.
- **`replicas > 1` without `highAvailability` is refused**, with a message naming the field that is
  missing. Running several leaders without an election is the failure the current single-replica
  rule exists to prevent, and it stays prevented — by a rule that can now be satisfied.
- **REQUIRED: the schema's `maximum` moves with it.** The ceiling exists in two layers, and they
  catch different absences: the webhook explains, and the schema still holds when the webhook is not
  installed — which is exactly when a second leader would be *rendered* rather than refused. Raising
  one without the other produces the worst pairing, a webhook that admits what the schema then
  rejects, reporting an error against a field the user did not get wrong.
- The doc comment on `Replicas` states the pairing rule and the reason the ceiling exists. It must
  not describe a limitation the API no longer has, since that comment generates into the CRD
  description and is what `kubectl explain` shows.
- **`enable_oplog` in `leader.extraArgs` is refused, and NOT as a security decision taken on the
  user's behalf.** `master.cpp:1456` is a `LOG(FATAL)` when `enable_oplog` is set with any backend
  other than `etcd`, and the lease backend cannot be combined with etcd at build time — so the key
  produces a leader that **cannot start**, and admitting it would trade a message at apply time for a
  crash-loop at run time. Reading the key's presence is enough: its value is not consulted, the same
  way argument names are matched elsewhere, and `enable_oplog=false` is refused too because it is
  equally inert and equally likely to be a misunderstanding worth naming.

- **Acceptance:** `replicas: 3` without `highAvailability` is refused and the message names
  `highAvailability`; with it, the object is accepted.
- **Acceptance:** `replicas: 1` is accepted in both cases, and neither path changes what is rendered
  for a backend that never asked for HA.
- **Acceptance:** `enable_oplog` in `extraArgs` is **refused**, and the message says the image has no
  etcd backend rather than saying the flag is unsupported — the flag is supported upstream, and a
  message that denies it sends the reader to the wrong project.

#### F3 — The workload shape: readiness marks a role, and no built-in controller reads it that way

This is the core task, and it is a design rather than a setting. It begins from one fact that is
easy to state and easy to miss:

**Under HA, `readyReplicas` is pinned at 1 by design.** Exactly one replica serves; the standbys are
deliberately not ready. Every built-in rollout mechanism reads "not ready" as "unhealthy", and here
it means "not the leader".

**Why the standbys must stay not-ready.** This is not a reporting preference — it is the only
mechanism that locates the leader among N replicas, and **two** things depend on it:

1. `status.endpoints` publishes the leader's **Service** DNS name for both the client and the admin
   port (`leader_workload.go:107`), and that Service selects **every** leader Pod without
   distinguishing role (`:383`). Readiness is what keeps a standby out of its endpoints.
2. This operator's own admin calls go to that address (`kv_cache_pool.go:284`, `:1821`). A ready
   standby would be load-balanced into, and every route behind `WithActiveService` answers `503`
   there — so `convergeTenantLedger` would fail intermittently, on a pool that is perfectly healthy.

**FORBIDDEN:** making the standbys ready to satisfy a rollout mechanism. It would break the tenant
ledger, not merely remove an observable.

**What each built-in mechanism does with that**, read from the Kubernetes source (`v1.34.1`):

| Shape | Outcome |
|---|---|
| `Recreate` (today's) | Takes down every replica at once — precisely the outage HA exists to prevent, on every flag change |
| `RollingUpdate` | Rolls, but `DeploymentComplete` requires `AvailableReplicas == *Spec.Replicas` (`deployment_util.go:744`), which is permanently false here. `DeploymentProgressing` (`:755`) then stops advancing `LastUpdateTime`, and `DeploymentTimedOut` (`:774`) reports `ProgressDeadlineExceeded` after `progressDeadlineSeconds` — **600 by default** (`apps/v1/defaults.go:71`) |
| StatefulSet | **Stalls outright**: it returns as soon as `unavailablePods >= maxUnavailable` (`stateful_set_control.go:745`), and `maxUnavailable` is hard-coded to 1 unless `MaxUnavailableStatefulSet` is enabled — Alpha and off by default from 1.24 through 1.34 (`kube_features.go:1468`) |

**The shape.** `RollingUpdate`, with the progress deadline effectively disabled:

- `maxUnavailable` must be `replicas`, and `replicas - 1` is one short of enough.
  `scaleDownOldReplicaSetsForRollingUpdate` removes an old Pod only while `availablePodCount >
  replicas - maxUnavailable` (`rolling.go`), and exactly one Pod is ever available here — so
  `replicas - 1` makes that `1 > 1`, the old leader is never removed, and no new replica can become
  ready until it releases the Lease. `[跑]` Measured on a single-node Kubernetes cluster, three
  replicas of which one can ever be available: at `maxUnavailable: 2` the old ReplicaSet still held
  `1` replica after 90s while the new one sat at `3` with none available; at `maxUnavailable: 3` the
  old ReplicaSet reached zero within 15s.
- `maxSurge` may exceed zero, because the lease admits only one leader. **This is the exact inverse
  of the non-HA case**, where a surging second master is the split brain `Recreate` exists to
  prevent — the same parameter is unsafe there and required here.
- `progressDeadlineSeconds` is set high enough to never fire.

**Disabling that deadline is not a loss, and the reason is that it cannot discriminate here.**
`DeploymentTimedOut` fires on `LastUpdateTime` going stale, and under HA "rolled out successfully"
and "the image cannot be pulled" both look like exactly that — the counters stop moving either way.
A check whose two outcomes are indistinguishable on this workload is not a check.

**The need it served is real, and the replacement is designed but NOT built.** It would be
`DeploymentComplete` **minus the one clause that is structurally false**: `UpdatedReplicas ==
Spec.Replicas` and `Replicas == Spec.Replicas` both hold normally under HA, and only the
`AvailableReplicas` clause does not. Nothing reads that pair today, so a rollout that cannot finish
has no failure signal — the decision sits at Q3.

**LIMITED:** `leaderPodIsReady` does **not** answer this. It reports whether a leader exists, which
a freshly elected leader satisfies while two standbys still run the old image.

**One consequence of this change is not visible in its diff.** The replica count was a hard-coded
`1` in the renderer and becomes a read of the field — but only where something elects. Where
admission is present nothing changes, because it refuses the unpaired combination outright. Where it
is **absent** — a development cluster, a CRD installed without the webhook — the renderer clamps back
to one, so that cluster keeps the failure it already had rather than gaining a worse one:

| | With the webhook absent | |
|---|---|---|
| Before | `replicas: 3` is **silently ignored**; one leader runs | a bug, but a safe one |
| After, no `highAvailability` | still ignored, now by the renderer's clamp | the same safe bug |
| After, with `highAvailability` | **obeyed** | three masters, one of them elected |

**Turning a hard-coded constant into a field read would otherwise swap a failure that discards intent
for one that discards safety, and the diff would show only a missing literal.** The ceiling is in the
schema for the same reason: the webhook explains, and the schema still holds where the webhook is not
installed. A test holds the two thresholds **equal** rather than both equal to one, so it survives F2
raising them and reddens if F2 raises only one.

- **Acceptance:** with HA configured, a change to a rendered leader flag **completes** a rollout —
  asserted from the store's own view.
- **NOT claimed: a serving master at every point during that rollout.** A floor of zero available
  replicas is what lets the old leader go, and it is also what lets it go before a replacement has
  taken the Lease. There is no third setting: the Deployment's availability signal is readiness,
  readiness here means "is the leader", only the OLD Pod satisfies it, and making the standbys ready
  to satisfy the rollout is FORBIDDEN above for a reason that has nothing to do with rollouts. What
  the shape does buy is the length of the window — the replacements are already running as standbys
  and contending, so it is bounded by lease expiry plus activation rather than by a Pod start.
- **Acceptance:** with HA not configured, the rendered strategy is **byte-identical** to today's.
- **Acceptance:** the rollout-finished predicate goes true only once every replica is updated, and is
  false while a standby still runs the old revision — the case `leaderPodIsReady` gets wrong.
- **Acceptance:** each parameter is pinned by a mutation that reddens an assertion rather than the
  build: `maxUnavailable` set to `replicas - 1` instead of `replicas`; `maxSurge` at zero; the
  deadline at any finite value; the single-replica path rolling instead of recreating; and the clamp
  removed, so a three-replica object without `highAvailability` renders three. A mutation that only
  fails to compile has tested nothing.
- **LIMITED:** `maxSurge` is 1 because it is *safe* here, not because a smaller value stalls — zero
  would still roll, one replica at a time, briefly running fewer. The parameter that must track the
  replica count is `maxUnavailable`.

#### F4 — The member's master address becomes an HA entry

Members receive `MOONCAKE_MASTER` as `<leader service host>:<rpc port>` today
(`member_workload.go:336`). With HA the client has to discover the leader through the backend rather
than through a Service, and the mechanism is the scheme prefix rather than a new variable.

**The mechanism, read out of the client and then measured.** `ParseHABackendSpec` splits on `://`
and, when there is no delimiter, returns "no HA backend" — which is what makes the non-HA form
byte-identical rather than merely similar (`client_service.cpp:592-614`). With a scheme it runs the
**same** `ParseHABackendType` and `ValidateHABackendAvailability` the master uses, so `k8s://` works
exactly when the client half was compiled with `STORE_USE_K8S_LEASE`. It then reads the current
holder, connects, and starts a monitor thread that follows view changes (`:636-673`). That thread is
the entirety of failover, and none of it is this operator's.

`[跑]` Confirmed on the built image: a member given `k8s://default/mooncake-master-lease` reaches
`k8s_lease_helper.cpp:24` and reports `Failed to create HA backend coordinator:
K8S_LEASE_OPERATION_ERROR` from `client_service.cpp:647`. Not `INVALID_PARAMS`, not
`UNAVAILABLE_IN_CURRENT_MODE` — it parsed the scheme and called the backend, and failed only for the
absent kubeconfig a bare container has.

**REQUIRED in one direction only.** A scheme without HA names a Lease no leader ever takes, so that
substitution is always wrong. The reverse is open: the Service publishes ready endpoints and a
standby is not ready, so an address under HA does resolve to the serving leader, and whether a
member's reconnect follows the endpoint across an election is unmeasured — see Q1.

- **Acceptance:** with HA on, the rendered value is an HA entry; with HA off it is byte-identical to
  today's.
- **Acceptance:** the Lease a member is told to READ is the one the leader is told to TAKE. Asserted
  by reading the leader's own rendered `-ha_backend_connstring` rather than by restating the name,
  because two literals agree until one of the two derivations moves — and a member following a Lease
  nobody holds looks exactly like a member waiting for a leader to come up.
- **Acceptance:** a member rendered against an HA backend reaches the store **after the leader that
  was serving at mount time is replaced**, with no member restart. This asserts the client-side
  reconnection is actually in play rather than assumed.

#### F5 — `status`, and what "healthy" means with N replicas

`leaderPodIsReady` already reads `ReadyReplicas > 0`, which is the correct predicate here. This
feature keeps it and writes down why, because the stricter-looking rule is wrong:

**"Exactly one ready" is not usable as an instantaneous predicate.** A leader that loses its lease
clears the service plane inside the process immediately, but the kubelet only removes it after the
probe fails — `periodSeconds: 5` with `failureThreshold: 3` is up to 15 seconds
(`leader_workload.go:285`). Every ordinary failover therefore passes through a window with two ready
replicas. A health rule of "exactly one" reports a fault on every successful failover.

- The rule is **at least one ready**, and the reason it is not "exactly one" is written next to it.
  Stated because the stricter form reads as more rigorous and will otherwise be "fixed" into place.
- **No status field is added for the replica counts**, by G5: nothing would read it. The Deployment
  already publishes them, the conditions already say whether the backend serves, and a copy here
  would have one purpose — being looked at — which is the test G5 exists to fail.
- **What does need saying is that the counts look wrong.** `3 desired / 1 ready` is the healthy
  steady state under HA and reads as a fault to anyone who has not been told. That belongs in the
  documentation of the field that turns HA on and in F7, where a reader meets it, rather than in a
  status field that repeats a number without explaining it.
- It does **not** report which replica leads — the Non-Goal above — and that reason travels in the
  field's own documentation rather than being absent.
- **Acceptance:** with the leadership backend made unreachable, the backend's condition reports it as
  not serving within the probe's own window, without any new probe or route.
- **Acceptance:** across an induced failover, the backend does **not** report a fault. This is the
  assertion that fails if "exactly one" is ever substituted.

#### F6 — The tenant quota policy across a failover: the window, measured

The tenant quota policy is seeded from a ConfigMap into an `EmptyDir` by an init container, and the
leader loads it with the file-backed policy store (`leader_flags.go:83`,
`quota_policy_workload.go`). With N replicas each Pod has its own copy, so it is worth stating
exactly what is and is not lost.

**Not lost: the quota itself.** The seed *is* the desired value — the operator renders it from the
pool's tenants — and `KVCachePoolReconciler` re-converges the ledger every
`kvCachePoolObserveInterval` (30 seconds, `kv_cache_pool.go:153`) by listing the master's quotas and
writing back any difference. The store's own `Save()` has exactly two call sites, both on the admin
write path (`master_service.cpp:808`, `:864`), so nothing drifts on its own.

**The bounded exposure**, which is what this feature has to judge: a standby's seed is the ConfigMap
**as of that Pod's start**, because the init container runs once into an `EmptyDir`. A standby that
has been running since before a quota increase takes over holding the older value, for up to one
reconcile interval. And an over-quota condition in this store **evicts the tenant's own older
objects** rather than refusing the write — irreversibly, and without moving any eviction counter.

- **Acceptance:** the exposure is stated in the field documentation of whatever F1 adds, with its
  bound (one reconcile interval) and its consequence (eviction, not refusal) in the same sentence as
  the bound. Splitting them produces a claim that reads as harmless.
- **Decision: the window is accepted, and the policy store stays file-backed.** Moving it is
  available — `CreateTenantQuotaPolicyStore` accepts `"etcd"` — and is not taken, on three grounds
  that have to hold together:

  1. **It is bounded and self-healing.** The operator is the authority, not the store: every pass
     lists the master's quotas and writes back any difference, so the divergence cannot outlive one
     interval. Moving the store to etcd would add a dependency to a path that already recovers.
  2. **The two directions differ, and one of them can charge a neighbour.**
     - A quota *raised* since the standby started: the window applies the older, lower ceiling, so
       the tenant evicts more than it should. Quota-driven eviction takes that tenant's **own**
       objects (`master_service.cpp:4504`), so the cost stays with whoever made the change.
     - A quota *lowered*: the older, higher ceiling merely evicts less, and the tenant holds more
       than its new ceiling for one interval. **Harmless while the pool is under its watermark.**
       Once the pool reaches it, the watermark path is a different one: `BatchEvict` walks every
       tenant in each shard (`master_service.cpp:10307`) and ranks by lease timeout, so it does
       **not** confine itself to the tenant that overran. And the over-holding tenant's hot data
       does not merely rank late — objects whose lease has not expired are skipped before ranking
       happens at all (`:10314`), so they are **never candidates**. What is taken is whatever is
       coldest cluster-wide, which is a neighbour's.

     So the cost is still hit rate rather than correctness, but it is **not always borne by the
     tenant whose quota changed** — which is worth stating because the mental model when lowering a
     quota is "I am constraining this one tenant".
  3. **The cost is hit rate, not correctness.** Evicting under a ceiling that is about to rise
     discards cache entries, and cache entries are recomputable by construction. It is the same
     eviction that any pressure produces, thirty seconds early.

  **FORBIDDEN as a reason to revisit:** "eviction is irreversible and invisible". That is true and it
  is why the exposure is written down, but it argues for the *disclosure*, not for the dependency —
  the same property holds every time the ceiling is legitimately reached.

  It reopens if the reconcile interval grows enough that "bounded" stops carrying the argument, or
  if a reader of this policy appears that the operator does not itself write.

#### F7 — Documentation

The environment and architecture pages gain what HA changes: the dependency, the opt-in, the
workload shape, and the health rule. Routing per `gpustack-operator-docs`.

**One item is load-bearing rather than descriptive:** with N replicas the healthy steady state reads
`N desired / 1 ready`, and to anyone who has not been told, that is what a broken Deployment looks
like. F5 declines to add a status field for it, so this page is where a reader meets the explanation.
Omitting it does not leave a gap in the documentation — it leaves users diagnosing a working system.

### Verification

Two properties are load-bearing and each names what does not count:

- **A failover actually moves service.** Not counted: a second replica reaching Ready; the store's
  own view has to show the role moving, and a client has to survive it without restarting.
- **The rollout completes under HA.** Not counted: `kubectl rollout status` returning on a
  single-replica backend, which exercises neither failing shape.

A single-node Kubernetes cluster is sufficient for both.

**REQUIRED for any acceptance that turns on a capability being present or absent: a case expected to
go the OTHER way, run at the same time.** A probe that can only produce the answer it is looking for
reports that answer whether or not the world agrees.

This is not abstract here. The measurement of the published image asked whether `k8s` is refused. It
is — and had that been the whole test, the refusal would have read as confirming that *etcd* works,
which is the opposite of the truth: all three backends are refused. The `etcd` case was there as one
expected to **succeed**, and it is the one that carried the information.
`--ha_backend_type=bogus` is the second control, answering `INVALID_PARAMS` — a *different* code,
which is what establishes the check can tell its outcomes apart at all.

**FORBIDDEN as evidence** for a capability claim, each having been available while a false one stood:
a successful build; a flag being accepted; the container starting; the exit code. Build configuration
is not evidence either — every upstream release workflow passes `-DSTORE_USE_ETCD=ON`, and the wheel
on PyPI is not what those workflows produce.

**Every feature below splits into a half that unit tests can settle and a half that cannot.** The
split is recorded per feature rather than as a general caveat, because the half that needs a cluster
is, in both cases, the half carrying the feature's reason for existing.

| Feature | Settled without a cluster | Needs a cluster, and why it is the load-bearing half |
|---|---|---|
| **F3** workload shape | The rendered strategy and its parameters, asserted directly | **That a rollout completes.** F3 exists because both defaults fail during an update; a third strategy that has never been updated is as unverified as the two it replaces |
| **F5** health rule | The predicate, including a simulated two-ready window | **That an ordinary failover reports no fault.** The transient the rule is designed around is produced by a real kubelet, not by a fixture |

**FORBIDDEN:** marking either feature done on the first column alone.

### Notes / Constraints / Caveats

- The store's HA area is under active upstream development — two of the three leadership backends
  are recent. On an image bump, every citation in the Proposal is re-read rather than assumed.
- **FORBIDDEN: reading `standby_controller.cpp:361` as the reason a standby does nothing.** It prints
  `HA standby controller falls back to noop, backend_type=k8s`, which reads as *because it is k8s*.
  The actual cause is two config switches both being off; the backend type is merely interpolated
  into the message. A log line that names a variable will be read as naming the cause.

### Boundaries

Owns: `api/worker/v1alpha1/kv_cache_backend.go` (the leader's HA surface),
`pkg/worker/kvcache/mooncake/leader_workload.go`, `leader_flags.go`, `ha_rbac.go`,
`pkg/worker/kvcache/mooncake/member_workload.go` (the master entry and its account only),
`pkg/worker/webhooks/worker/kv_cache_backend.go` (the replicas rule and the oplog refusal),
`pkg/worker/controllers/worker/kv_cache_backend.go` (the RBAC sync and the two aligners' account
fields), and `pack/mirrored-mooncake/`.

Three files outside that list were touched, each as the mechanical counterpart of something inside
it, and they are named here so the difference is a decision rather than a surprise:
`.github/workflows/base-image.yml` (the new image is unbuildable in CI without its enum entry),
`pkg/worker/settings/value.go` and `docs/settings.md` (the image Setting's own text claimed no image
this project publishes had been measured end to end, which stopped being true).

Does not own: `KVCachePool`, `KVCachePoolBinding`, the tenant ledger convergence, the member's disk
tier, or anything under `pkg/worker/controllers/worker/kv_cache_pool.go`.

### Risks and Mitigations

- **The leadership dependency is a new operational surface.** HA stops being a switch and becomes a
  prerequisite. Accepted: HA is an advanced feature and G4 keeps the default path untouched.
- **The workload shape is the part most likely to be got wrong quietly.** A wrong strategy does not
  fail at apply time; it fails during an upgrade, which is when it is most expensive. F3's acceptance
  asserts against the store's view rather than the Deployment's for this reason.

## Design Details

### Commands

`make lint`, `make generate` after any API change, `make test`.

### Project Structure

No new package. The flag rendering is in `leader_flags.go` and the workload shape in
`leader_workload.go`; the RBAC fits neither, since it renders three object kinds for two roles, so it
lives in `ha_rbac.go` beside them.

### Code Style

Per `CLAUDE.md`. Comments state the result, not the path taken to it; long ones are broken into
points; `SUGGESTED:` / `LIMITED:` / `REQUIRED:` / `FORBIDDEN:` where a note is a recommendation or a
bound.

### Implementation Plan

- [x] **T1 — Measure the shipped image.** No leadership backend is compiled into a published image;
      the rebuild in [`pack/mirrored-mooncake`](../pack/mirrored-mooncake/Dockerfile) is what
      replaced it, and its four-case probe is now a build-time assertion rather than a task.
- [x] **T2 — `leader.highAvailability`, the five derived flags, and the access they need** (F1).
- [x] **T3 — Admission: the replicas rule and the `enable_oplog` refusal** (F2), with the rewritten
      `Replicas` doc comment and the ceiling moved in both layers together.
- [x] **T4 — The workload shape** (F3), with the mutation tests that fail for each rejected strategy
      by name.
- [x] **T5 — The member's master entry** (F4).
- [x] **T6 — The health rule and its reason** (F5). No probe change; the predicate stays
      `ReadyReplicas > 0` and gained the note about why it is not "exactly one".
- [x] **T7 — F6's exposure, written where it is read** — in the `highAvailability` doc comment, with
      the bound and the consequence in one sentence rather than two, because split apart the bound
      reads as harmless.
- [x] **T8 — Documentation** (F7), in [`docs/kv-cache/backend.md`](../docs/kv-cache/backend.md).
- [ ] **T9 — e2e: an induced failover** on a single-node Kubernetes cluster. Service moves, no member
      restarts, and no fault is reported during the transition. Tracked as
      https://github.com/gpustack/gpustack-operator/issues/278. Two things it must check that no unit
      test can: that the members' `k8s://` entry actually carries them across an election, and that
      the two rendered ServiceAccounts are sufficient — every RBAC assertion here is against a
      rendered object, never against an API server that enforced it.

### Where a green suite is not coverage

Five defects in this work survived a green suite, a clean lint and a spec review. **Four share one
shape: a rule that is correct for the case it was written against and silently stops being correct
when a neighbouring value moves.** The shape is the reusable part; the list is the evidence for it.

| Defect | Why the suite was green |
|---|---|
| The leader aligner compared the strategy's TYPE only | Correct while one replica was the only case — every rollout was `Recreate`, which has no fields. Raising three replicas to five leaves `maxUnavailable` below what a one-ready workload needs, and the rollout stalls |
| `ProgressDeadlineSeconds` was never converged at all | The renderer gained it with the workload shape; nothing ever asked the aligner to carry it |
| `RoleBinding.roleRef` was never compared | The comment beside it described reporting the API server's refusal. The code never attempted the field, so no refusal was ever produced — a comment describing behaviour its code does not have |
| The three RBAC objects synced no note, label or owner reference | Every other rendered object here does, through code the RBAC path did not reuse. An adopted object is written to and then refused by the teardown path, which reads the note first |
| `enable_oplog` was refused on EVERY update | The rule was tested on create. The precedent against it was already in this file's own webhook — the fallback-image check is scoped to updates that could change its answer, "because not every update is the user's" |

**REQUIRED: the fifth one is the one to generalize from.** Adding a key to a forbidden list
retroactively condemns objects admitted before the list had it, and the reconciler removing a
finalizer is an update — so the refusal strands the object undeletable after teardown has already
removed its workloads. Every future addition to `ExtraArgsRules.Forbidden` inherits this, which is
why the scoping lives in a named predicate rather than in the one rule that needed it.

**FORBIDDEN: reading "the tests pass" as coverage of an aligner.** Four of the five are convergence
on a LIVE object, and a renderer test cannot reach any of them — it compares what would be created,
never what happens to what already exists. Each fix here was verified by removing it again and
watching the new test go red.

### Test Plan

- **Unit** — the rendered strategy under HA and without it; the replicas admission rule and its
  message; the master entry in both forms; the health predicate across a simulated two-ready window.
- **Integration (envtest)** — a backend moving from one replica to N and back; the condition when the
  leadership backend is unreachable.
- **e2e** — an induced failover on a single-node Kubernetes cluster: service moves, a member does not
  restart, no fault is reported during the transition.

## Alternatives

- **Orchestrating failover from the controller** — watching the leadership backend and steering a
  Service to the current leader. Refused: the client already does this itself, so it would be a
  second mechanism that can disagree with the first.
- **A leader Pod label reconciled by this operator** to make the leader identifiable from the
  Kubernetes side. Refused: it would have to be derived from the admin API, which is the refused
  interaction shape — and the store already writes the label itself under this backend. See the
  Non-Goal above for what that costs.
- **StatefulSet instead of Deployment** for the leader. **Refused, and not on a trade-off:** its
  rolling update returns as soon as `unavailablePods >= maxUnavailable` (`stateful_set_control.go:745`),
  and `maxUnavailable` is hard-coded to 1 unless the `MaxUnavailableStatefulSet` gate is on — Alpha
  and off by default from 1.24 through 1.34. With N-1 standbys permanently unavailable it does not
  stall *sometimes*; it never advances. It is strictly worse than the Deployment, which at least
  rolls and then misreports.
- **Making the standbys ready** so the built-in mechanisms behave. Refused for a reason stronger
  than the lost signal: readiness is what keeps a standby out of the leader Service's endpoints, and
  this operator's own admin calls resolve through that Service. Ready standbys would put `503`s into
  the tenant ledger's convergence path. See F3.

## Open Questions

**Q1 — Must a member follow the Lease at all, or does the leader Service already suffice?**
https://github.com/gpustack/gpustack-operator/issues/279

F4 gives every member a `k8s://` entry, which is what forces a member image to carry the leadership
backend and therefore excludes the vendor variants. There is a second path that was found after the
first was built and has **not** been measured: the leader Service already contains only the leader,
because a standby is not ready, and the client's ping loop reconnects to the same address on its own
after `max_ping_fail_count` failures (`client_service.cpp`, the non-HA branch). If that converges,
the member ServiceAccount, the token mount and the image constraint all disappear. What it pays is
endpoint propagation — up to about 15s at the rendered probe settings — against `k8s://` learning the
new leader's Pod address directly. The figure that decides it is how long a member cannot reach a
master after the leader Pod is deleted, measured both ways.

**Q2 — Is the namespace-wide Lease grant narrow enough?**

F1's Roles are namespace-scoped, not backend-scoped: one backend's leader can `update` every Lease in
the shared namespace. `resourceNames` cannot restrict `create`, but it can restrict `get` and
`update` — and `update` is the verb that takes a Lease from its holder. Left as it is because every
account here is in one trust domain in one namespace this operator owns; reopened by anything that
puts a backend outside that assumption.

**Q3 — Should readiness require the rollout to have finished?**

The readiness predicate is `ReadyReplicas > 0`, which does not distinguish a finished rollout from one
still carrying old-revision standbys, so a failover during an update can elect the previous binary.
Whether that matters enough to add `UpdatedReplicas == Spec.Replicas` has not been settled. It is the
same missing predicate the disabled progress deadline left behind, so one answer serves both.

## Appendix: what the build cost

The recipe lives in [`pack/mirrored-mooncake/Dockerfile`](../pack/mirrored-mooncake/Dockerfile) and
its comments carry the reasons. What is here is the set of traps, because each cost a full build to
find and each reads as a different kind of failure than it is:

- **FORBIDDEN: a base image that ships Go.** Upstream's `dependencies.sh` installs its own toolchain
  at `/usr/local/go`, exactly where `golang:*` images put theirs. The collision does not surface at
  install time — it surfaces much later as a link failure in the Go c-shared wrapper, which reads
  like a source problem.
- **`dependencies.sh` pulls roughly 200 MB in one apt transaction**, and a single dropped connection
  fails all of it. The retry loop is inside ONE `RUN`: apt keeps what it fetched and the script never
  cleans it, so each attempt starts closer to done — a layer boundary between attempts would throw
  that cache away.
- **Parallelism is bounded by MEMORY, not by cores.** `-j16` on a 31 GB builder is killed partway
  through and reports `c++: fatal error: Killed signal terminated program cc1plus`, which reads as a
  compiler crash. The pybind translation units want a couple of gigabytes each.
- **The base image exports `PYTHON_VERSION` of its own.** `scripts/build_wheel.sh` reads
  `${PYTHON_VERSION:-${1:-…}}`, so `python:3.12-bookworm`'s `3.12.14` beats the positional argument,
  the script forms the interpreter name `python3.12.14`, and the build dies at "command not found"
  **after the entire compile has succeeded**.
- **`BUILD_EXAMPLES` cannot be turned off.** `build_wheel.sh` copies `transfer_engine_bench` with no
  guard, so a build without examples fails during packaging rather than during compilation.
- **`libcurl4` is not vendored by auditwheel.** Without it `import mooncake.store` fails, so
  `mc_store_rest_server` dies before parsing an argument while `mooncake_master` runs fine. That is
  why the smoke test IMPORTS rather than checking `PATH`: the two claims come apart exactly here.
- **FORBIDDEN: `grep -qv` to assert a refusal is absent.** The `-v` form asks whether *some* line
  does not match, which every multi-line log satisfies whether or not the refusal is also present —
  an assertion that cannot fail. The Dockerfile uses `! grep -q`.
- **The release is asserted from the installed artifact, not from the build argument.** Reading
  `importlib.metadata.version` out of the finished image catches the failure that matters — a tag
  moved back to its base release — which a commit pin compared before building cannot.
