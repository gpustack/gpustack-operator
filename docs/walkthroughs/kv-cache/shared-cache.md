# Shared Cache Walkthrough

Create a store, a pool, a namespace grant and a workload in that order.

On a cluster with GPUStack installed, replace the node selector, namespace, instance type and model
name in the manifests below, then apply them in that order.

## Contents

- [Creation order](#creation-order)
- [Step 1: the store](#step-1-the-store)
- [Step 2: the pool and the grant](#step-2-the-pool-and-the-grant)
- [Step 3: the workload](#step-3-the-workload)
- [Step 4: high availability](#step-4-high-availability)
- [Step 5: local disk tier](#step-5-local-disk-tier)
- [Step 6: post-failover state](#step-6-post-failover-state)
- [Step 7: a second team and a second domain](#step-7-a-second-team-and-a-second-domain)
- [Troubleshooting](#troubleshooting)

## Creation order

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `KVCacheBackend` | cluster | administrator | which nodes contribute memory, and how much |
| `KVCachePool` | cluster | administrator | how much of that store may be handed out at all |
| `KVCachePoolBinding` | namespace | administrator | that THIS namespace may draw on it, and up to what |
| `ModelDeployment` | namespace | user | a workload that attaches to the grant |

The order is fixed because each object names the one above it, so creating them the other way round
leaves references that resolve to nothing. Why the two scopes split this way is on
[KV Cache Pool](../../modules/kv-cache/pool.md#two-kinds-split-by-scope).

**The Binding is the authorization point.** A namespace gets access to a store when an administrator
creates a Binding in it. A user naming a pool is not a path this API has: `poolRef` is a
same-namespace reference, so the name it accepts is a Binding's.

## Step 1: the store

Replace the `kubernetes.io/os: linux` selector with the labels of the nodes that should contribute memory.

```yaml
apiVersion: worker.gpustack.ai/v1
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
            kubernetes.io/os: linux
          medium: DRAM
          capacityPerMember: 8Gi
```

`spec.image` is left unset on purpose: the cluster-wide `kv-cache-backend-image`
[Setting](../../reference/settings.md) supplies this project's own build, which is the one that can elect a leader
later. Naming a published upstream image here works until Step 4 and then does not. See
[High availability](../../modules/kv-cache/leader.md#high-availability).

`capacityPerMember` is charged to each member Pod's host memory request, so it is a claim on the node
and not a hint. One member Pod runs per node the selector matches.

`leader: {}` takes the field defaults, `multiTenancy` included (it defaults on), so this master keeps
a per-tenant quota ledger and Steps 2 and 3 read against it. A backend pinned to a store image from
before Mooncake 0.3.12 is the one exception and declares `multiTenancy: false` out loud. See
[The project's own build variants](../../modules/kv-cache/backend.md#the-projects-own-build-variants).

Wait for it, then read what it reports:

```console
$ kubectl get kvcb mooncake-dram -w
NAME            TYPE       PHASE   ENDPOINT                                         CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051   16Gi
```

**`CAPACITY` is read from the store, never derived from the spec.** Two nodes at `8Gi` show `16Gi`
because both members registered their segments, so this figure is the evidence that the store is
really assembled. A number smaller than expected means a member has not registered yet, whatever the
phase says: `kubectl describe kvcb mooncake-dram` and its `MembersMounted` condition name which.

## Step 2: the pool and the grant

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePool
metadata:
  name: shared-dram
spec:
  backends: # exactly one
    - mooncake-dram
  quota:
    total: 16Gi
---
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: team-a
  namespace: team-a
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 8Gi
  domain:                                # every field here is immutable
    blockSize: 64
    dtype: bfloat16
```

**The `domain` is the cache's compatibility key, and it is immutable for the reason it exists.** Two
workloads sharing a domain share cached blocks, so a domain that could be edited would let a running
workload start reading blocks another tokenizer wrote. Pick it to match the model and engine
settings the deployments in this namespace will run; a second, different model gets a second Binding.

**`domain.name` is left out, so it is `default`.** The backend from Step 1 runs with multi-tenancy
(`leader.multiTenancy` defaults on), so the engines are handed the tenant id `default`, which is also
the tenant the store uses for a writer that names none. Name each domain once a second Binding shares
the master; the name is what keeps the two apart.

**A quota ceiling is not a reservation.** It is the most this namespace may hold at once, and going
over it does not fail a write. See
[Full-quota behavior](../../modules/kv-cache/pool.md#full-quota-behavior).

**The ceiling is enforced because the Step-1 leader carries its tenant ledger, which the default
gave it.** A backend declared `leader.multiTenancy: false` holds no ledger instead: its pool is
admitted with a warning, the Binding reports `QuotaGranted=True` with reason `Unenforced` and no
`EFFECTIVE` figure, and every write lands in the store's default tenant.

**A multi-tenant master refuses a tenant name absent from its ledger.** An engine that ignores the
injected tenant then needs a second Binding whose domain is `default`, or that leaves `name` out.
See [Tenant compatibility](../../modules/kv-cache/injection.md#tenant-compatibility).

## Step 3: the workload

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vLLM
    version: "0.29.0"                    # on the same store line as Step 1's default image
  kvCache:
    poolRef:
      name: team-a                       # the Binding above, in this namespace
  roles:
    - name: server
      replicas: 2
      instanceType: <one from kubectl get instancetype>
      resources:
        accelerator: "1"
```

`kvCache` is optional. Omit it and the deployment runs with no shared cache at all, which is the
useful comparison to have run once before attributing anything to the pool.

**The engine is configured by injection, not by anything you write here.** The operator resolves the
Binding, reads the backend's endpoint and the domain, and injects the store's environment into every
role's Pod. What is injected per engine, and every refusal, is on
[KV Cache Injection](../../modules/kv-cache/injection.md).

**The store version must match the engine's client.** The engine version above decides the client,
and Step 1's unset `spec.image` leaves the store on the Settings default. A pair on different lines
starts healthy and then fails every write, so the version above is a choice. See
[The store version must match the engine's client](../../modules/kv-cache/backend.md#the-store-version-must-match-the-engines-client).

Check the attachment from the deployment's own status rather than from the Pods:

```console
$ kubectl -n team-a get md qwen-chat -o jsonpath='{.status.conditions[?(@.type=="CacheAttached")]}'
```

## Step 4: high availability

Everything so far runs one leader process that already holds a Kubernetes Lease. An update or a node
failure takes the store's metadata with it. Add standbys with one edit:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: mooncake-dram
 spec:
   type: Mooncake
   connection:
     managed:
-      leader: {}
+      leader:
+        replicas: 3
       members:
         - nodeSelector:
             kubernetes.io/os: linux
           medium: DRAM
           capacityPerMember: 8Gi
```

**The healthy steady state now reads `3 desired / 1 ready`, and that is not a broken Deployment.**
Exactly one leader serves; the other two are standbys, deliberately not ready so the leader Service's
endpoints never include a process that cannot serve. During a healthy failover `2` are briefly ready
as the old leader steps down. Both readings are normal.

**Scaling from one to three keeps the first leader Pod and the member templates** because the Lease
election and accounts were already present at one replica. See
[the leader Deployment](../../modules/kv-cache/leader.md#the-deployment-and-the-two-probes) for the
cost of changing `leader.electionBackend`.

**`leader.memberAddressing` chooses how members find the master**, and defaults to
`Service`. An explicit `Lease` value uses the member's API access to read the current holder. See
[High availability](../../modules/kv-cache/leader.md#high-availability) for the measured failover limits.

These conditions apply at one replica too; the phase does not summarize them:

| Condition | True | False |
|---|---|---|
| `ElectionObserved` | the Lease names a holder, so an election happened | a ready leader and a holderless Lease — the image, the role binding, or a first campaign still running |
| `RolloutComplete` | every replica runs the current template and none of the previous one is left | a rollout in flight, or one that has stalled |

`RolloutComplete` exists because this workload disables the deployment deadline that would normally
answer it: that deadline requires every replica to be available, and only one ever is here.

## Step 5: local disk tier

A running member group cannot gain a disk tier. Admission refuses the edit, because the members
would have to restart to mount the directory. Declare the tier when you create a backend. This diff
turns Step 1's manifest into a second backend that selects the nodes carrying the disk:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
-  name: mooncake-dram
+  name: mooncake-disk
 spec:
   type: Mooncake
   connection:
     managed:
       leader: {}
       members:
         - nodeSelector:
-            kubernetes.io/os: linux
+            kvcache: "true"
           medium: DRAM
           capacityPerMember: 8Gi
+          localDisks:
+            - path: /var/lib/kvcache
+              capacity: 100Gi
```

Create the directory on every selected node before you apply the backend. The published store image
runs as uid 65532, so give the directory to that uid. The uid depends on the image; see
[Directory requirements](../../modules/kv-cache/local-disk-tier.md#directory-requirements).

```console
$ install -d -o 65532 -g 0 -m 0750 /var/lib/kvcache
```

A pool names exactly one backend and cannot be re-pointed. Serving workloads from `mooncake-disk`
therefore takes its own `KVCachePool` and Binding. The tier writes in buckets, so a small cache can
show an empty tier while it works; see [Bucket writes](../../modules/kv-cache/local-disk-tier.md#bucket-writes).

## Step 6: post-failover state

**A failover keeps nothing in memory.** A standby holds no data, so the replica that takes over knows
none of the objects held in member memory and rebuilds from member remounts alone; a single leader
that restarts does the same. High availability shortens the outage, and every one of those objects
misses afterwards. Plan for a cold cache after every failover and every leader restart.

**The store's snapshot is not offered, and its flags are refused in `leader.extraArgs`**, because
restoring a snapshot can make the cache serve another key's bytes instead of a miss.
[High availability](../../modules/kv-cache/leader.md#high-availability) says why.

## Step 7: a second team and a second domain

Each Binding is one ceiling and one reuse domain. To give a second team cache capacity, create a
Binding in its namespace:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: team-b
  namespace: team-b
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 8Gi
  domain:
    name: llama-8b-v1
    blockSize: 16
    dtype: bfloat16
```

A namespace that runs a different dtype needs a second Binding, because a domain is immutable and
holds one dtype. Mixing dtypes in one domain can return wrong blocks silently. The Binding below
serves an `fp8_e4m3` deployment in `team-a`. Replace the instance type as in Step 3.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: qwen-fp8
  namespace: team-a
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 4Gi
  domain:
    name: qwen-fp8-v1
    blockSize: 16
    dtype: fp8_e4m3
---
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat-fp8
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vLLM
    version: "0.29.0"
  kvCache:
    poolRef:
      name: qwen-fp8
  roles:
    - name: server
      replicas: 1
      instanceType: <one from kubectl get instancetype>
      resources:
        accelerator: "1"
```

The operator renders `--kv-cache-dtype fp8_e4m3` from the Binding onto every role. A role that sets
that flag itself is refused. See [Engine dtype](../../modules/kv-cache/pool.md#engine-dtype).

**The three ceilings ask for 20Gi against a pool `quota.total` of 16Gi.** The pool reports
`QuotaWithinTotal=False` with reason `Oversubscribed`. That is not a fault: a ceiling is an ask, and
the store recuts each grant against the capacity it can serve, in proportion to what each Binding
asked. `status.effectiveQuota` on each Binding shows the grant. See
[Ceiling and grant](../../modules/kv-cache/pool.md#ceiling-and-grant).

**A domain separates cached blocks. It does not wall off a tenant.** A Binding is not an enforcement
boundary: a workload that knows another domain's name can still read and write that domain. Isolation
of reuse needs a master that keeps a tenant ledger (the Step 1 default) and an engine that forwards
the injected tenant. See [Limitations](../../modules/kv-cache/pool.md#limitations) and
[Tenant compatibility](../../modules/kv-cache/injection.md#tenant-compatibility).

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| `KVCacheBackend` stuck in `Provisioning` or `Error` | `LeaderAvailable=False` | Leader Deployment unready, image missing election backend, or external endpoint address unreachable |
| `KVCacheBackend` shows `Degraded` | `MembersMounted` shortfall (`SegmentsShort`, `NoSegments`) | DaemonSet member Pods failing to mount or allocate memory/disk segments; check node capacity and daemon logs |
| `KVCachePool` shows `Error` | `BackendResolved=False` / `CapacityAllocatable=False` | Referenced backend does not exist or has zero allocatable capacity |
| `KVCachePool` `QuotaLedgerAvailable=False` | `MultiTenancyDisabled` | Backend explicitly set `multiTenancy: false`; ceiling quotas are intentionally not enforced |
| `KVCachePoolBinding` `effectiveQuota: 0` | `QuotaGranted=False/ZeroGranted` | The master grants this domain nothing. Read the pool's `CapacityAllocatable`: `NothingToAllocate` means no member has mounted yet, and a restarted master can take roughly 30 s to remount its segments. Otherwise a proportional recut left this domain no share |
| Pool reports oversubscribed | `QuotaWithinTotal=False/Oversubscribed` | The Bindings' ceilings sum past the pool's `quota.total`. This is not a fault: grants are recut in proportion and shown in each `status.effectiveQuota`. Lower a ceiling or raise `quota.total` for full grants |
| Binding refused at apply with `Duplicate` on `spec.domain.name` | Domain already registered on the same master | Another Binding holds the name; an omitted name is `default`. Give each Binding a unique `domain.name` |
| Two Bindings report one domain | `DomainExclusive=False/DomainClaimedByMultipleBindings` | Two creates raced past admission; delete one of them |
| Member Pod stuck with `FailedMount ... hostPath type check failed` | Pod event | The tier directory does not exist on that node; create it as in Step 5 |
| Member Pod runs but never becomes Ready | `MembersMounted=False` (`NoSegments`, or `MemberCrashLooping` if the container exits), or a `CAPACITY` below the expected figure | The directory exists but the image's user cannot write it; the container log says `no write permission on directory`. Run `install -d -o 65532 -g 0 -m 0750 <path>` for the default image. See [Directory requirements](../../modules/kv-cache/local-disk-tier.md#directory-requirements) |
| Member init container restarts | `MembersMounted=False/MemberInitCrashLooping` | The directory-survey init container restarted; the condition message gives the restart count, exit code and termination message. The survey runs `sh -c`, so the member image needs a shell. An unwritable directory does not cause this |
| Local disk tier reports preexisting content | `TierWasEmpty=False/PreexistingContent` | The directory held files from an earlier backend. Nothing was removed; empty it yourself. `cleanAfterDelete: true` empties it only when this backend is deleted |
| HA Leader shows `3 desired / 1 ready` | Expected steady state | Only elected active leader is Ready; 2 standby leader replicas remain unready until failover |
| Deletion of `KVCachePoolBinding` blocked | `Releasable=False/HeldByWorkloads` | Active ModelDeployments or Pods reference this binding; delete client workloads first |

---

**See also** — [KV Cache Backend](../../modules/kv-cache/backend.md) (every field of the store, and what status reports) ·
[KV Cache Leader](../../modules/kv-cache/leader.md) (the election, why there is no snapshot, and the member addressing choice in full) ·
[KV Cache Pool](../../modules/kv-cache/pool.md) (quota, domains and what a full quota does) ·
[KV Cache Local Disk Tier](../../modules/kv-cache/local-disk-tier.md) (directory, bucket and cleanup rules) ·
[Model Deployment](../../modules/model-deployment/deployment.md) (roles, prefill/decode, rollout) ·
[Model Deployment Prefill and Decode](../../modules/model-deployment/prefill-decode.md) (router, transfer) ·
[KV Cache Injection](../../modules/kv-cache/injection.md) (what a Pod actually receives)

**Next** → [Model Prefetch Walkthrough](../model-delivery/prefetch.md) — cache weights on GPU nodes before serving.
