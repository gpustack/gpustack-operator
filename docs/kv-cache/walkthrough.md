# KV Cache Walkthrough

> **Purpose** — the shortest path from nothing to a `ModelDeployment` reading and writing a shared KV
> cache: every object in order, what to check after each one, and the three places a working
> configuration is usually got wrong.
> **Audience** operators, users · **Prerequisites** [KV Cache Backend](backend.md) ·
> **Read time** ~11 min

Every other page here documents a field. This one answers **what do I type first**, and the answer is
four objects in a fixed order — a store, a pool, a grant, and a workload that draws on it.

The manifests are meant to be pasted. Replace the node selector, the namespace, the instance type and
the model name; everything else runs as written on a cluster with this operator installed.

## Contents

- [The four objects, and why the order is fixed](#the-four-objects-and-why-the-order-is-fixed)
- [Step 1: the store](#step-1-the-store)
- [Step 2: the pool and the grant](#step-2-the-pool-and-the-grant)
- [Step 3: the workload](#step-3-the-workload)
- [Step 4: high availability](#step-4-high-availability)
- [Step 5: what a failover keeps](#step-5-what-a-failover-keeps)
- [The three things that go wrong](#the-three-things-that-go-wrong)

## The four objects, and why the order is fixed

| Object | Scope | Who creates it | What it decides |
|---|---|---|---|
| `KVCacheBackend` | cluster | administrator | which nodes contribute memory, and how much |
| `KVCachePool` | cluster | administrator | how much of that store may be handed out at all |
| `KVCachePoolBinding` | namespace | administrator | that THIS namespace may draw on it, and up to what |
| `ModelDeployment` | namespace | user | a workload that attaches to the grant |

The order is fixed because each object names the one above it, so creating them the other way round
leaves references that resolve to nothing. The scope split is the one the scheduling chain already
uses: a cluster-scoped object owns capacity, a namespaced object draws on it, and the namespaced one
is what RBAC is written against.

**The Binding is the authorization point.** A namespace gets access to a store by an administrator
creating a Binding in it — not by a user naming a pool, which `poolRef` makes unrepresentable by
being a same-namespace reference.

## Step 1: the store

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: mooncake-dram
spec:
  type: Mooncake
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector:
            kubernetes.io/os: linux      # replace with the nodes that should contribute
          medium: DRAM
          capacityPerMember: 8Gi
```

`spec.image` is left unset on purpose: the cluster-wide `kv-cache-backend-image`
[Setting](../settings.md) supplies this project's own build, which is the one that can elect a leader
later. Naming a published upstream image here works until Step 4 and then does not — see
[High availability](leader.md#high-availability).

`capacityPerMember` is charged to each member Pod's host memory request, so it is a claim on the node
and not a hint. One member Pod runs per node the selector matches.

Wait for it, and read what it actually says:

```console
$ kubectl get kvcb mooncake-dram -w
NAME            TYPE       PHASE   ENDPOINT                                         CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051   16Gi
```

**`CAPACITY` is read from the store, never derived from the spec.** Two nodes at `8Gi` show `16Gi`
because both members registered their segments, so this figure is what says the store is really
assembled. A number smaller than expected means a member has not registered yet, whatever the phase
says — `kubectl describe kvcb mooncake-dram` and its `MembersMounted` condition name which.

## Step 2: the pool and the grant

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata:
  name: shared-dram
spec:
  backends: [mooncake-dram]              # exactly one
  quota:
    total: 16Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: team-a
  namespace: team-a
spec:
  poolRef: {name: shared-dram}
  quota:
    ceiling: 8Gi
  domain:
    name: qwen-7b-v1                     # every field here is immutable
    blockSize: 64
    dtype: bfloat16
```

**The `domain` is the cache's compatibility key, and it is immutable for the reason it exists.** Two
workloads sharing a domain share cached blocks, so a domain that could be edited would let a running
workload start reading blocks another tokenizer wrote. Pick it to match the model and engine
settings the deployments in this namespace will run; a second, different model gets a second Binding.

**A quota ceiling is not a reservation.** It is the most this namespace may hold at once, and going
over it does not fail a write — see
[What a full quota actually does](pool.md#what-a-full-quota-actually-does).

## Step 3: the workload

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vllm
    version: "0.29.0"                    # on the same store line as Step 1's default image
  kvCache:
    poolRef:
      name: team-a                       # the Binding above, in this namespace
  roles:
    - name: server
      replicas: 2
      instanceType: <one from kubectl get instancetype>
      resources:
        accelerator: 1
```

`kvCache` is optional — omit it and the deployment runs with no shared cache at all, which is the
useful comparison to have run once before attributing anything to the pool.

**The engine is configured by injection, not by anything you write here.** The operator resolves the
Binding, reads the backend's endpoint and the domain, and injects the store's environment into every
role's Pod. What is injected per engine, and every refusal, is on
[KV Cache Injection](../reference/kv-cache-injection.md).

**The store version must match the engine's client.** The engine version above decides the client,
and Step 1's unset `spec.image` leaves the store on the Settings default. A pair off DIFFERENT lines
starts healthy and then fails every write, so the version above is a choice — see
[The store version must match the engine's client](backend.md#the-store-version-must-match-the-engines-client).

Check the attachment from the deployment's own status rather than from the Pods:

```console
$ kubectl -n team-a get md qwen-chat -o jsonpath='{.status.conditions[?(@.type=="CacheAttached")]}'
```

## Step 4: high availability

Everything so far runs one leader process. An update or a node failure takes the store's metadata with
it, and every member re-registers into an empty one. Electing between several replicas is one edit:

```yaml
spec:
  connection:
    managed:
      leader:
        replicas: 3
        highAvailability: {}
```

⛔ **The healthy steady state now reads `3 desired / 1 ready`, and that is not a broken Deployment.**
Exactly one leader serves; the other two are standbys, deliberately not ready so the leader Service's
endpoints never include a process that cannot serve. During a healthy failover `2` are briefly ready
as the old leader steps down. Both readings are normal.

⛔ **Crossing `replicas: 1` in either direction restarts the leader and rolls every member**, because
the HA accounts and token mounts change. With an explicit `Lease` address, the member's master entry
also changes shape. The store's cached contents do not survive the crossing, so make this edit before
the cache is worth keeping — or accept a cold start.

**`leader.highAvailability.memberAddressing` chooses how members find the master**, and defaults to
`Service`. An explicit `Lease` value uses the member's API access to read the current holder. See
[High availability](leader.md#high-availability) for the measured failover limits.

Two conditions appear at this point that the phase deliberately does not summarize:

| Condition | True means | False means |
|---|---|---|
| `ElectionObserved` | the Lease names a holder, so an election happened | a ready leader and a holderless Lease — the image, the role binding, or a first campaign still running |
| `RolloutComplete` | every replica runs the current template and none of the previous one is left | a rollout in flight, or one that has stalled |

`RolloutComplete` exists because this workload disables the deployment deadline that would normally
answer it: that deadline requires every replica to be available, and only one ever is here.

## Step 5: what a failover keeps

Nothing in memory. A standby holds no data, so the replica that takes over knows none of the objects
held in member memory and rebuilds from member remounts alone; a single leader that restarts does the
same. High availability shortens the outage, and every one of those objects misses afterwards. Plan
for a cold cache after every failover and every leader restart.

⛔ **`leader.highAvailability.snapshot` is refused at admission, at any replica count**, because
restoring a snapshot can make the cache serve another key's bytes instead of a miss.
[High availability](leader.md#high-availability) says why, and what happens to an object admitted
with it.

## The three things that go wrong

**A published upstream store image, under high availability.** No published `kvcacheai/mooncake`
image carries a leadership backend: the leader answers `UNAVAILABLE_IN_CURRENT_MODE` and runs as a
permanent standby, and members answer `Invalid HA backend entry` and CrashLoopBackOff. `spec.image`
and every `members[].image` need a build that has one — leaving them unset is the simplest way.

**Expecting a failover or a restart to keep the cache.** Neither does: the new leader knows none of
the objects held in member memory, so every lookup for one misses until an engine writes it again.

**Reading `3/1` as a fault.** It is the designed steady state under high availability, and the one
reading on this page most likely to be escalated as an outage.

---

**See also** — [KV Cache Backend](backend.md) (every field of the store, and what status reports) ·
[KV Cache Leader](leader.md) (the election, the snapshot and the member addressing choice in full) ·
[KV Cache Pool](pool.md) (quota, domains and what a full quota does) ·
[Model Deployment Reference](../reference/model-deployment.md) (roles, prefill/decode, rollout) ·
[Model Deployment Prefill and Decode Reference](../reference/model-deployment-prefill-decode.md) (router, transfer) ·
[KV Cache Injection](../reference/kv-cache-injection.md) (what a Pod actually receives)

**Next** → [KV Cache Pool](pool.md) — the quota this walkthrough set once and did not explain.
