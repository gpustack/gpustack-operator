# Spec: KV Cache and ModelDeployment — Per-Group Transport, API Consolidation, and Replaceable Replicas

Status: Shipped
Every design question this spec opened is answered and recorded below, and every task has landed.
`V1` made its visit to real hardware: both ship gates are green, the eight leader high availability
questions and the third quota comparison have readings, and five end-to-end cases were written from
what the trip found. One rider, R2, is unreachable rather than unmeasured — the reason is recorded
with it — and a rider does not gate this spec.
Type: Feature
Issue: #446

> **One spec for three pieces of work that turned out to be one.** A member group declaring its own
> medium and transport, the consolidation of the four resources' API surface, and a replica becoming
> replaceable were planned separately and cannot ship separately: the consolidation renumbers
> protobuf fields that the per-group work introduced and compacts around one it leaves in place, and
> the replaceable-replica work rests on a pod group keyed the way the consolidation leaves it. They
> are recorded here as one document because a reader who has only one of the three cannot check the
> other two.
>
> **It absorbs, and therefore replaces, two earlier planning documents** — the per-group medium and
> transport spec and the API consolidation spec. Neither reached main, so nothing here rewrites a
> published record.
>
> **It also carries later work belonging to two specs that DID reach main** — the leader high
> availability spec's `F8` through `F11` with their tasks, and two injection rulings that supersede
> what the injection spec states. Neither of those documents is edited. A shipped spec records what
> was decided when it shipped, so work designed afterwards is recorded here and says whose it is; the
> section "Carried from the leader high availability spec" holds it, keeps the original numbering,
> and explains why. Where `F8` or `F11` could mean either document's, the owner is named.

## Summary

A member group in a `KVCacheBackend` cannot declare its own storage medium or its own transport today:
`members[].medium` is a single-value enum (`DRAM` only, `api/worker/v1alpha1/kv_cache_backend.go:469`) and
`spec.transport.protocol` is backend-wide (`api/worker/v1alpha1/kv_cache_backend.go:386-412`). One backend
carrying a DRAM group and a VRAM group on the same nodes is therefore unrepresentable, even though two
groups selecting one node already render two DaemonSets (`syncMemberWorkloads`,
`pkg/worker/controllers/worker/kv_cache_backend.go:1970-1990`).

This spec widens `medium` to `["DRAM", "VRAM"]`, adds an optional per-group `transport.protocol` that
inherits the backend value when unset, and makes the renderer, the resource accounting, the fabric
privileges, and the engine wiring per-group-aware. The medium change is small at the schema level and
large at the render level: no production code compares `Medium` against a value today, but three render
paths hard-code DRAM behavior without reading the field at all (host-memory-only resource requests, the
host segment-size env's accounting, and the CPU-build default image), and four transport paths assume one
protocol per backend.

**The image work landed first and separately.** It reached main ahead of this spec, because a
vendor variant image is buildable and testable on its own and the API that names one is not. The
base-image workflow learns an optional
docker build `target` input (empty means "build to the final stage", today's behavior), so every vendor
variant lives in the ONE `pack/mirrored-mooncake/Dockerfile` as its own target — differing in base image
and final stage, not in file count. The targets are vendor-generic — `cuda`, `cann`, `rocm` — because
each variant's builder and runtime base images are ARGs that a dispatch can repoint from outside (CUDA
13.0 today, 12.9 tomorrow) without editing the Dockerfile; the toolchain version lives only in the
dispatch-time tag, e.g. `0.3.13.post1-cuda13.0`. The default bases follow the approach of the
`gpustack/runner` project. The `cuda` and `rocm` variants are built on two Mooncake version lines:
`0.3.13.post1` (for vLLM 0.28.0 and later) and `0.3.10.post2` (for vLLM before 0.28.0); the `cann`
variant builds `0.3.13.post1` and `0.3.11.post1` instead — 0.3.10's upstream CI pairs with CANN 9.0,
and the Ascend engine line already ships 0.3.11.post1 (the `gpustack/runner` cann image for vLLM
0.23.0 carries it), so a 0.3.10 cann build would serve no engine. **Everything else ships here:** the
enum, the fields, the renderer, the admission, the docs, and the two parts that follow.

NVIDIA validation hardware exists (address held out of band), so the CUDA variant image and the one
measured acceptance item below have somewhere real to run; equivalent Ascend and AMD hardware is not
guaranteed, and their measured coverage is an open question.

This spec does two things that would otherwise be two rounds of breaking change against the same
objects, and pays the API churn once.

**Part A consolidates the API surface of the four custom resources.** Nothing here is a feature. Every
item is a field that carries no information, a field that exists only to be refused, a name that
describes a mechanism rather than a subject, a shape that differs from the same concept spelled
elsewhere, or a gap that makes a legitimate configuration unwritable. They were found by reading the
four resources end to end as a user would, which is not how any of them was reviewed when it shipped.

**Part B makes a replica replaceable.** A replica's name is derived from its ordinal
(`modelDeploymentPodName`, `pkg/worker/controllers/worker/model_deployment_render.go:230-237`), so a
replacement wants the name the departed replica still holds; Kueue releases that name only once a
replacement exists; and the single way out of the circle is to delete the group's Workload, which
stops every remaining member. The blast radius of that teardown is decided by `instanceType`
(`modelDeploymentPodGroups`, `.../model_deployment_pod_group.go:108-130`), a field nobody chooses to
express which replicas should live and die together.

**Part A runs first, and the order is forced rather than preferred.** The renames touch
`model_deployment.go`, `model_deployment_render.go`, `model_deployment_connector.go` and every
webhook, which are the same files Part B rewrites. Running B first means writing those files twice.

## Motivation

### Per-group medium and transport: one backend served one medium and one fabric

A node that has device memory, host memory, and NVMe can contribute only one of them today. The enum has
one value, so the shape cannot be written down; and even with a second value, the backend-wide transport
would force a VRAM group and a DRAM group onto one protocol, which is the one thing the two do not agree
on. Upstream, one binary is one medium: `USE_VRAM_SEGMENT` is a compile-time option that forces
`USE_CUDA=ON` (`mooncake-common/common.cmake:108,216-220` at `v0.3.13.post1`) and switches segment
allocation to `cudaMalloc` under `#ifdef` (`mooncake-store/src/client_buffer_allocation.cpp:57-99`), so
mixing media needs two processes — which is what two member groups already are.

### Part A: what reading the four resources end to end turned up

| Finding | Where | Why it is a defect |
|---|---|---|
| `leader.offload.enabled` is fully determined by whether any member declares `localDisk`, in BOTH directions | `pkg/worker/webhooks/worker/kv_cache_backend.go:604-619` | the field can never take a value the rest of the object does not already imply |
| `leader.offload.onEvict` is fully determined by `enabled` | `.../kv_cache_backend.go:621-654` | a choice-shaped field with exactly one legal value |
| `extraArgs` and `extraEnvs` are maps on the cache backend and lists on the deployment | `api/worker/v1alpha1/kv_cache_backend.go:349,625,644` against `model_deployment.go:301,554` | one concept, two JSON shapes; a map has no stable member order, which every consumer that renders or diffs it has to invent for itself |
| `extraEnvs` is the only plural-with-s spelling in the API | `kv_cache_backend.go:644` | `Container.Env`, `EnvFrom` and `roles[].env` are all singular |
| `template.resources` exists only so that supplying it can be refused | `model_deployment.go:424-431`, stated in its own comment | a field whose only behavior is rejection is a field that should not be in the schema |
| `roles[].template` duplicates the role's own tiers: two `env` lists with the same rules, and an image block that belongs to the role | `model_deployment.go:296-328, 377-443` | the nesting expresses a precedence that does not exist, since both tiers are refused and appended identically |
| `router` has `image` but no `imagePullPolicy` and no pull secret | `model_deployment.go:539-544` | a router image in a private registry cannot be pulled, and no field can express it |
| `imagePullSecret` is singular on a role and `imagePullSecrets` is a list on the cache backend | `model_deployment.go:436` against `kv_cache_backend.go:87` | same concept, two names and two cardinalities |
| `engine` and `engineVersion` are two required siblings that are never meaningful apart | `model_deployment.go:53,72` | the version field's own comment spends three paragraphs explaining how it combines with the other one |
| `kvCache.connector` reserves the Mooncake discriminator under the value `auto` | `model_deployment.go:200-202` | the field is an identity field, frozen after creation, and `auto` reads as "derive it" |
| `directTransfer` names a mechanism, not a subject | `model_deployment.go:129` | it does not say what moves or between whom, which is the whole of what a reader needs |
| `quota.total` is read by three lines, all in admission, and none of them sums anything | `webhooks/worker/kv_cache_pool.go:137`, `webhooks/worker/kv_cache_pool_binding.go:647,654` | ten Bindings may each declare the pool's whole ceiling and all ten are admitted; the field is named for a total it never computes |

### Part B: the deadlock is arithmetic, and it is ours rather than Kueue's

Measured against Kueue v0.18.4, the version this project deploys. Every line below is
`pkg/controller/jobs/pod/pod_controller.go` in that tree.

| Fact | Where | What it means here |
|---|---|---|
| `isPodRunnableOrSucceeded` counts a terminating Pod as ACTIVE while it still has a `nodeName` | `:888-893` | a replica we just deleted is not yet absent |
| `countAbsentPods` is `max(0, podSet.Count - activePods)` | `:1301-1308` | absence is counted per role, not per group |
| `WaitingForReplacementPods` is True exactly when `absentPods > 0` | `:1397-1440` | Kueue ASKS for a replacement, in its own status |
| a departed Pod is finalized when `min(inactive, inactive+active-count) > 0` | `:1262-1268` | with one dead replica and no replacement this is `min(1, 0) = 0` |
| `equivalentToWorkload` compares the Workload name, the PodSet names and the PodSet counts, and NOTHING about the pod template | `:1311-1342` | a replacement carrying a DIFFERENT template keeps the Workload |
| `getRoleHash` prefers the `role-hash` annotation over a digest of the Pod spec | `:661-668` | and this project writes the role's own name there (`model_deployment_pod_group.go:192`) |

The last two rows are the green light. Kueue does not re-derive a PodSet from a replacement's spec
and does not compare templates, so a role can be rolled inside an admitted Workload: the replacement
joins the PodSet its role name names, the counts still match, and the Workload is untouched.

The fourth row is the wall. A departed Pod is released only once a replacement is active, and a
replacement can only be created under a name nothing holds. Our name is ordinal-deterministic, so
the departed Pod holds it. Deleting the Workload is the only exit, and it costs the group. The
reconciler already says so (`pkg/worker/controllers/worker/model_deployment.go:278-286`) and names
this spec's remedy: give replacements fresh names.

### The teardown unit is a hardware field

`modelDeploymentPodGroups` keys on `instanceType`, and the comment at
`model_deployment_pod_group.go:101-103` states the reason: keying on the role would produce one group
per role, each separately admitted, so the atomicity a group exists for would cover one role at a
time.

That reason has been overtaken. The cross-group barrier exists — `ModelDeploymentJointAdmission`
holds every group of a multi-group deployment until all of them can reserve quota
(`model_deployment_joint_admission.go:261,400-434,636`) — and it engages whenever a deployment has two
or more groups. Per-role grouping does not lose atomicity; it moves atomicity from the group to the
barrier for EVERY prefill/decode deployment instead of only for the ones whose roles happen to sit on
different hardware.

What per-role grouping cannot break: a queue name is derived from the `instanceType` and one Workload
carries one queue name, so a group may never span two types. A role has exactly one type, so a
per-role group is always within one type, and therefore always at least as fine as per-type grouping.

### Goals

- **A group declares its own medium.** `members[].medium` accepts `DRAM` and `VRAM`. The value stays an
  identity on the status path (already agreement-based and VRAM-safe,
  `agreedMemberFacts` at `pkg/worker/controllers/worker/kv_cache_backend.go:1258-1274`) and becomes a
  selection on the render path, where today nothing branches.
- **A group declares its own transport.** `members[].transport.protocol` is optional; unset inherits the
  backend's `spec.transport.protocol`. Fabric privileges are rendered per group from the group's
  effective protocol, not from the backend's.
- **The engine is handed the matched group's protocol**, and an engine whose transport constraint no
  group in the pool satisfies is refused at admission rather than started. Today a mismatch is silent at
  every layer: the allocator does not filter on transport (verified upstream:
  `mooncake-store/include/allocation_strategy.h:415-501` never consults `Segment.protocol`), and the
  member status echoes the registration request rather than the installed transport
  (`pkg/worker/kvcache/mooncake/admin.go:251-255`).
- **The project ships VRAM-capable images.** The `cuda` / `cann` / `rocm` variant targets in the one
  Dockerfile produce versioned tags (`0.3.13.post1-cuda13.0` and friends) on their Mooncake version
  lines (two per variant; the cann line is `0.3.13.post1` + `0.3.11.post1`), so a VRAM group has a
  defaultable image to point at — through `members[].image`, `spec.image`,
  or the `kv-cache-backend-image` setting, all of which already resolve today
  (`pkg/worker/controllers/worker/kv_cache_backend.go:1697-1713`).
- **Success criteria (testable):**
  1. A backend with a DRAM group and a VRAM group on one node renders two DaemonSets, each with its own
     image and its own transport, and each member reports its own medium in status.
  2. A group that declares no transport inherits the backend's; the value an engine is handed is the one
     belonging to the group it was matched to.
  3. Resource accounting differs by medium: a DRAM member requests host memory for
     `capacityPerMember + localBufferSize`; a VRAM member requests host memory for `localBufferSize`
     only and charges its device memory to nothing, since claiming device memory is allocating it.
  3a. A group reaches its nodes' devices only through what it declares: `securityContext` merged onto
     the protocol's own (capabilities unioned), `hostPaths`, and `runtimeClassName`.
  4. An engine whose transport constraint no group in the pool serves is refused at admission with the
     typed `TransportUnsupported` reason.
  5. Dispatching the base-image workflow with a `target` and a `tag` produces the named variant tag from
     the single Dockerfile; dispatching without a `target` reproduces today's image bit-for-bit in
     behavior.

1. No field in these four resources can take a value the rest of the object does not already imply.
2. One concept has one name and one shape across the four resources.
3. A configuration a user legitimately needs is writable.
4. A replica that departs is replaced without its siblings being stopped.
5. An edit that changes a replica's rendered Pod rolls the role ONE replica at a time.
6. A group is one role.
7. The documentation states none of what this spec makes false.

### Non-Goals

- **Admission rules forcing `image` on VRAM groups.** Deliberately absent: the cluster default image
  lives in an administrator-editable setting, and a webhook that hard-requires a group-level image would
  break exactly the cluster whose administrator pointed that setting at a VRAM build. The guidance ("a
  VRAM group needs a build with `USE_VRAM_SEGMENT=ON`; the stock CPU default is not one") is a
  documentation fact, not a gate.
- **Segment-name fields, allocation-preference isolation, and `mediums: [DRAM, VRAM]` on one group.**
  The RFC's three deliberate exclusions stand; upstream verification confirmed the underlying facts
  (segment names are client-derived and not caller-settable; preference falls back unconditionally and
  the client API exposes no exclusion counterpart; one binary is one medium).
- **Multi-tenancy changes.** `leader.multiTenancy` already exists; the engine side is a documentation
  matter per the RFC, not a code one.

- **A conversion path.** Every resource here is unreleased, so a stored object carrying an old field
  is a branch-tracking cluster's problem and is handled by recreating the object. No conversion
  webhook, no alias, no deprecation window.
- **Surge.** Kueue treats an extra active Pod of a role as excess and deletes it, and
  `sortActivePods` (`pod_controller.go:944-967`) prefers the Pod that has no finalizer, is gated, and
  is newest — which is exactly a surge replacement. Surge is a shape the scheduler refuses, not a
  knob we are declining to add.
- **Rollout knobs.** No `maxUnavailable`, no `maxSurge`.
- **Making a `replicas` edit cheap.** Every Pod of a group carries the group's declared total and
  Kueue refuses to compose a Workload for a group whose Pods disagree on it, so a shape change still
  tears the group down. What changes is that the group is one role.
- **Stable per-replica identity.** No ordinals, no per-replica DNS, no per-replica volumes.
- **Relaxing any frozen field**, and no change to which fields are frozen.
- **Widening any enum**, other than the one value rename in F11.

## Proposal

### The shape this lands on

`KVCacheBackend`, cluster-scoped:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: mooncake-dram
spec:
  type: Mooncake                      # optional  default Mooncake  enum [Mooncake]
  image: ""                           # optional  maxLength 512
  imagePullPolicy: IfNotPresent       # optional  enum [Always IfNotPresent Never]
  imagePullSecrets: []                # optional  atomic  maxItems 32

  transport:                          # optional  default {}
    protocol: Auto                    # optional  default Auto
    deviceResourceName: ""            # optional  consulted on RDMA and EFA only

  connection:                         # required  exactly one of managed, external
    managed:
      leader:                         # required
        replicas: 1                   # optional  default 1  min 1  max 5
        allocationStrategy: FreeRatioFirst
        multiTenancy: false
        extraArgs:                    # CHANGED  was map[string]string
          - -max_threads=16           # []string  atomic
        extraEnv:                     # NEW
          - name: GLOG_v              # []InstanceEnvVar  listType map  listMapKey name
            value: "2"
        highAvailability:             # optional  declaring the block is the switch
          memberAddressing: Lease
          snapshot:
            persistentVolumeClaimName: mooncake-snapshots
            intervalSeconds: 600
            retentionCount: 2
      members:                        # required  1..32
        - nodeSelector:
            kubernetes.io/os: linux
          medium: DRAM
          capacityPerMember: 8Gi
          localBufferSize: ""
          image: ""
          extraArgs: []               # CHANGED  was map[string]string
          extraEnv: []                # RENAMED and RESHAPED  was extraEnvs, map[string]string
          transport:
            protocol: Auto
          runtimeClassName: ""
          securityContext: {}
          hostPaths: []
          localDisks:                 # RESHAPED  was the singular localDisk block
            - path: /mnt/nvme/mooncake # optional  declaring an entry turns offloading on
                                       # listType=map on path, maxItems=1
            capacity: 200Gi
            keyLimit: 0
            cleanAfterDelete: false
            eviction:
              enabled: true
              policy: LRU
              watermark: {high: 90, low: 70}
      scaleIn:
        gracePeriodSeconds: 0
    external:
      endpoints:
        - address: "host:50051"
          name: Client
```

`leader.offload` is gone. `KVCachePool` and `KVCachePoolBinding`:

```yaml
kind: KVCachePool                     # cluster-scoped
spec:
  backends: [mooncake-dram]           # required  exactly one  immutable
  quota:
    total: 16Gi                       # required  semantics widened, see F6
---
kind: KVCachePoolBinding              # namespaced
spec:
  poolRef:
    name: shared-dram
  domain:                             # immutable as a whole
    name: qwen-7b-v1
    blockSize: 64
    dtype: bf16
  quota:                              # RENAMED  was the flat quotaCeiling
    ceiling: 8Gi
```

`ModelDeployment`, namespaced:

```yaml
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct

  engine:                             # CHANGED  was two flat siblings
    name: vllm                        # required  enum [vllm sglang]
    version: "0.27.1"                 # required  1..64  unvalidated by design

  kvCache:                            # optional  KV reaches other replicas through a shared store
    poolRef:
      name: team-a
    connector: mooncake               # CHANGED value  was auto

  kvTransfer:                         # RENAMED  was directTransfer
    protocol: ""                      # configures the point-to-point path ONLY, see F12

  router:                             # optional
    name: llm-d
    replicas: 1
    image: ""
    imagePullPolicy: IfNotPresent     # NEW
    imagePullSecrets: []              # NEW
    extraArgs: []

  roles:
    - name: prefill
      kind: prefill
      replicas: 2
      instanceType: h100-80gb
      resources:                      # the only resources block, template.resources is gone
        accelerator: 1
      image: ""                       # FLATTENED from template
      imagePullPolicy: IfNotPresent   # FLATTENED
      imagePullSecrets: []            # FLATTENED and pluralized
      privileged: false               # FLATTENED
      ports: []                       # FLATTENED
      additionalVolumes: []           # FLATTENED
      command: []                     # FLATTENED  the take-over tier
      extraArgs: []                   # the append tier, unchanged
      env: []                         # MERGED  was role.env and template.env
```

### Per-group medium and transport

This part shipped before the consolidation below and is recorded as it was built. **Read it against
F4:** it introduces the disk tier under the name `localDisk`, and F4 reshapes that field into the
`localDisks` list. Nothing else here is renamed, and the rules stated in this part hold unchanged
with the list in place -- a mount path colliding with the group's declared disk tier is still
refused, and the comparison now reads every entry.

#### API changes

```yaml
spec:
  transport:
    protocol: TCP            # unchanged; the value a group inherits when it declares none
  connection:
    managed:
      members:
        - nodeSelector: {kvcache: "yes"}
          medium: DRAM
          capacityPerMember: 64Gi
          localDisk: {path: /mnt/nvme/mooncake, capacity: 2Ti}
        - nodeSelector: {kvcache: "yes"}        # the SAME nodes
          medium: VRAM                           # widened enum
          image: mirrored-mooncake:0.3.13.post1-cuda13.0
          capacityPerMember: 16Gi
          transport:
            protocol: RDMA                       # optional; overrides the backend default for this group
```

- `KVCacheBackendMember.Medium`: enum widened to `["DRAM", "VRAM"]` at
  `api/worker/v1alpha1/kv_cache_backend.go:469`. The field stays immutable (the webhook rule at
  `pkg/worker/webhooks/worker/kv_cache_backend.go:1123-1126`, today unreachable, becomes live).
- `KVCacheBackendMember.Transport`: new optional struct carrying only `Protocol` (same enum as the
  backend field). `DeviceResourceName` on the backend transport stays backend-wide; it describes the
  fabric, not the group.
- **No per-group device-resource field.** A VRAM member charges its device memory to nothing. Measured
  upstream at `v0.3.13.post1`, a member's segment is one `cudaMalloc` per split
  (`client_buffer_allocation.cpp`), the splits stay under a transport's registration limit rather than
  spanning devices (`GetTransportRegistrationLimit`, `real_client.cpp`), and nothing on that path calls
  `cudaSetDevice` — so one member's segment is on ONE device, and requesting one accelerator would take
  a whole one from inference to account for a fraction of one device's memory. What holds the member and
  the engine apart on a shared device is sizing `capacityPerMember` against the engine's memory
  fraction, and the documentation says so. Fabric privileges stay keyed on the protocol, never on the
  medium, so a VRAM group is not exempt from them.
- `KVCacheBackendMember.SecurityContext`: new OPTIONAL `core.SecurityContext`, merged ONTO the one the
  fabric path derived. Per field, with `capabilities.add` UNIONED — a host-fabric group keeps IPC_LOCK
  and SYS_RESOURCE whatever it declares, because without them the transfer engine fails at memory
  registration, long after the container started and looked healthy.
- `KVCacheBackendMember.HostPaths`: new OPTIONAL list of `{path, mountPath, type, readOnly}`. Host paths
  only: what a member needs from outside its image is the node's driver tree and device nodes. The
  backing volume is named from the entry's POSITION, so it collides with neither another entry nor the
  two volumes the renderer owns. Admission refuses a mount path that duplicates another entry's, that
  equals the device tree's (`/dev/infiniband`, unconditionally — the protocol may change while a mount
  path is judged only when written), or that equals this group's current `localDisk.path`.
- `KVCacheBackendMember.RuntimeClassName`: new OPTIONAL field, declared rather than derived. A member
  group has no `InstanceType` to ask — it selects nodes by label, and a label carries no manufacturer
  this operator can map — so the equivalent derivation on a model deployment has no counterpart here.

#### Render and accounting changes (the silently-wrong branches)

| Site | Today | After |
|---|---|---|
| `memberRequests`, `pkg/worker/kvcache/mooncake/member_workload.go:737-742` | Always charges `capacityPerMember + localBufferSize` to host `ResourceMemory` | DRAM: unchanged. VRAM: host memory for `localBufferSize` only, and no extended resource at all |
| `renderMemberEnv`, `member_workload.go:540-545` | Renders `capacityPerMember` as `MOONCAKE_GLOBAL_SEGMENT_SIZE` | Unchanged in spelling; the comment is corrected — in a VRAM build the same size feeds `cudaMalloc`, verified upstream: the `#ifdef` switches allocation only, the `total_size` plumbing is shared (`client_buffer_allocation.cpp:57-99`) |
| `MemberProtocol`, `member_workload.go:319-331` | One protocol per backend | `MemberProtocolForGroup(kvcb, group)`: group's `transport.protocol` if set, else the backend's; `MemberProtocol` stays for backend-wide callers |
| `applyMemberFabric`, `member_workload.go:757-827` | Fabric privileges keyed on the backend protocol | Keyed on the group's effective protocol, per DaemonSet. Never keyed on the medium, so a VRAM group is not exempt from it |
| Engine wiring, `model_deployment_binding.go:365`, `pod_kv_cache_resolve.go:111` | `Protocol: MemberProtocol(backend)` | `Protocol: MemberProtocolForGroup(backend, matchedGroup)` |

#### Admission changes

- `checkTransport` (`pkg/worker/kvcache/inject/engine.go:148-162`) becomes pool-aware: for each group in
  the matched pool's backend, compute the effective protocol; refuse with `TransportUnsupported` when the
  engine's constraint (the `engineTransportConstraint` table) is satisfiable by no group. When several
  groups satisfy, the engine is handed the protocol of the group it was matched to.
- Webhook on `KVCacheBackend`: a `hostPaths[]` mount path is refused when it duplicates another entry's,
  when it is the device tree's (`/dev/infiniband`, unconditionally), or when it is this group's current
  `localDisk.path`. No rule on `image` (see Non-Goals).

#### Image build

- `.github/workflows/base-image.yml` gains an optional `target` input, plumbed through to the reusable
  `_image.yml` docker build. Empty target = build to the final stage = today's behavior, so every
  existing dispatch is unaffected.
- `pack/mirrored-mooncake/Dockerfile` grows one target per vendor variant, named for the vendor and
  nothing else: `cuda`, `cann`, `rocm`. Each variant is a builder/runtime stage pair whose base images
  are per-vendor ARG pairs (`CUDA_BUILDER_IMAGE` / `CUDA_RUNTIME_IMAGE` and friends), overridable at
  dispatch time through the workflow's existing `args` input — retargeting CUDA 13.0 to 12.9 is a build
  argument, not a Dockerfile edit. The default bases follow the `gpustack/runner` project's approach.
  Each variant differs in the cmake flag set of its build stage (`cuda` enables `USE_CUDA` +
  `USE_VRAM_SEGMENT`, following the Mooncake repository's own CUDA build; `cann` and `rocm` the vendor
  equivalents). The `cann` build additionally reserves an `ASCEND_TRANSPORT` build ARG selecting the
  ubshmem flavor: upstream's `USE_UBSHMEM` composes with `USE_ASCEND` rather than replacing it, so the
  flavor is a build-time choice on the same target, not a new target. The comment at
  `Dockerfile:207-209` is rewritten: its stated reason ("the API's medium
  is a single-value enum") is overturned by this spec.
- Tags are pure dispatch-time input and carry the toolchain version: `<mooncake-version>-<variant>`,
  e.g. `0.3.13.post1-cuda13.0`, `0.3.10.post2-rocm7.2`. The `MOONCAKE_VERSION` ARG (`Dockerfile:84`)
  already makes the version a dispatch axis, and the Dockerfile already carries version-conditional
  patching (the `0.3.11*` case at `Dockerfile:230-236`); `0.3.10.post2` joins as the line for vLLM
  before 0.28.0 — for `cuda` and `rocm` only, since the `cann` variant builds `0.3.13.post1` and
  `0.3.11.post1` (0.3.10 predates the CANN 9.1 pairing the Ascend engine line ships with).
  Note `USE_VRAM_SEGMENT` exists only on the 0.3.13 line (introduced upstream in 0.3.13),
  so the `cuda` target builds 0.3.10.post2 as the CUDA transfer engine without VRAM segments — the
  Dockerfile guards this fail-loud rather than silently dropping the flag.

### The API surface of the four resources, and a replaceable replica

F1 through F12 consolidate what reading the four resources turned up; F13 through F18 are what
makes a replica replaceable. They are numbered in one sequence because they land on the same
objects and several of the second group depend on a rename in the first.

### F1 `leader.offload` is removed

Declaring a `members[].localDisks` entry turns offloading on, and the mode is always the deferred one.
The leader's flags are derived from whether any member group declares a disk tier.

Acceptance:

- A backend whose members declare no `localDisks` entry renders no offload flag.
- A backend with one member group declaring a `localDisks` entry renders the enable flag AND the
  deferred-mode flag, together, with no field saying so.
- The two admission rules that enforced the pair in both directions are gone, and their tests with
  them. The rule that refuses a second member group carrying a `localDisks` entry STAYS, and its
  message keeps its own reason: `status.capacity` reports one pair of figures, so two tiers could not
  be attributed (`kv_cache_backend.go:656-664`).

### F2 `extraArgs` becomes a list on the cache backend

`[]string`, `listType=atomic`, on the leader and on each member group, matching
`ModelDeployment.roles[].extraArgs` and `router.extraArgs`.

Two admission rules replace what the map gave for free:

- Every entry must begin with `-`. This is what rules out a split pair written as two entries
  (`["-rpc_timeout", "5000"]`), whose second entry carries no key and would make the collision check
  see nothing to collide with. A value that itself begins with `-` must use the inline form.
- The key is the substring before the first `=`, or the whole entry when there is none, so a boolean
  flag written as one token keeps working. Two entries with one key are refused.

THE DEPLOYMENT SIDE DOES NOT GET THESE RULES, and the asymmetry is the parsers rather than an
oversight. The store's leader and members parse with Go's flag package, where one flag is one token;
vLLM and SGLang parse with argparse, where `--tensor-parallel-size 4` as two arguments is the
idiomatic form and refusing it would break every deployment.

Acceptance:

- A leader and a member group each render their entries in the order written, after the derived ones.
- `["-rpc_timeout", "5000"]` is refused, naming the second entry.
- `["-enable_ha"]` is accepted.
- `["-a=1", "-a=2"]` is refused as a duplicate key.

### F3 `extraEnv` is a list, on the leader and on each member group

`[]InstanceEnvVar` with `listType=map` and `listMapKey=name`, which is the shape `roles[].env`
already has. The list form is what the JSON consumers need; the map key is what keeps one name from
carrying two values, which the Go map gave and a plain list would not.

`members[].extraEnvs` is renamed `extraEnv` in the same change. Nothing else in this API spells a
list of environment entries with a trailing `s`.

The leader gains the field, which it did not have. `validateExtraEnvs`
(`webhooks/worker/kv_cache_backend.go:905-917`) is reused rather than copied, and the rendering
follows `member_workload.go:669-670`.

Acceptance:

- A leader with `extraEnv` renders those variables after the derived ones.
- A reserved name is refused on the leader with the same message it is refused with on a member.
- Two entries sharing a name are refused by the schema, without the webhook.

### F4 `members[].localDisk` becomes the `localDisks` list

The noun stays — the block describes what a group of nodes HAS, a path and a capacity and an eviction
policy, while offloading is what the store DOES with it, and after F1 no field names the behavior at
all. `path` also keeps its name: `diskPath` would repeat the word the parent already carries.

What changes is arity. `connection.managed` is the implementation-neutral layer — `spec.type` is the
discriminator that says WHO does placement and eviction, and its own comment records that a second
implementation widens that enum rather than reinterpreting an absent field. A singular field would
bind every future implementation to the one path this one reads, and widening a singular field to a
list later is a breaking change while starting as a list is not. So it ships as a list now:

```yaml
localDisks:                     # listType=map, listMapKey=path, maxItems=1
  - path: /mnt/nvme/mooncake
    capacity: 1Ti
```

The name is `localDisks`, NOT `disks`: the parent already says these belong to a member group, and
`disks` alone would read as any disk a node has rather than the tier this operator manages.

⭐ The cap is ONE entry, and the reason must be written as the status contract, NOT as what one
renderer reaches for: `status.capacity` is a single pair of figures for the whole backend and cannot
attribute a tier's bytes to one disk, so two entries would describe neither. That same sentence, at
the group level, is why admission allows only one group to carry a list at all — the two bounds are
lifted together with that status shape, or not at all. Writing the cap's reason as "the store this
implementation renders reads a single directory" would tie a schema bound to one renderer, and the
two have different lifetimes.

Acceptance:

- The schema refuses two entries naming one directory, and refuses a second entry outright.
- The reason recorded beside the cap names `status.capacity`, and the group-level rule's message
  names the same thing.

### F5 `quotaCeiling` becomes `quota.ceiling`

A nested block matching `KVCachePool.spec.quota`, so the two resources spell one concept one way.
The semantics are unchanged: the value is written verbatim into the master's per-tenant quota
(`controllers/worker/kv_cache_pool.go:709-712`), and exceeding it evicts this namespace's own older
objects rather than refusing a write.

### F6 `quota.total` gains the total it is named for

The per-Binding admission rule stays: a Binding asking for more than the pool declares can never be
granted. What is added is a Condition on the pool, `QuotaWithinTotal`, reporting whether the sum of
every Binding's ceiling still fits inside `total`.

It is spelled POSITIVELY, like every other condition this operator reports: the type names the state
where each Binding's ceiling can be granted in full, and False is the side worth looking at. Naming
the fault instead — an `Oversubscribed` that is True when the pool is full — would be the first
condition in this repository that reads backwards from all the rest.

It is a Condition and NOT a refusal. Refusing would break the ordinary sequence of creating Bindings
and then growing the backend, and the store already handles oversubscription by reducing every
tenant's effective quota in proportion — `status.effectiveQuota` on each Binding is what reports the
result. False here says the pool is oversubscribed; it does not say anything is broken.

Acceptance:

- Two Bindings each at half the pool's total leave the Condition True.
- A third Binding taking the sum past the total turns it False, with reason `Oversubscribed` and a
  message naming the sum and the total.
- Neither Binding is refused, and neither reports an error of its own.

### F7 `engine` becomes an object

`spec.engine: {name, version}`, both required, replacing `spec.engine` and `spec.engineVersion`.
`name` keeps the `["vllm","sglang"]` enum; `version` keeps its 1..64 bounds and stays unvalidated.

The two were never meaningful apart: both are required, and the image tag is assembled from both
together with the role's own `InstanceType`.

### F8 `roles[].template` is flattened away

`image`, `imagePullPolicy`, `privileged`, `ports`, `additionalVolumes` and `command` move onto the
role. `template.env` merges into `roles[].env`. `template.resources` is deleted outright, and
`template` itself ceases to exist.

- **`resources` is deleted rather than moved.** It has no behavior but refusal, which its own comment
  states. The refusal it produced is replaced by strict decoding's unknown-field error, which is a
  worse message for a better reason: a field kept solely to improve one error message is a promise
  the schema makes and nothing keeps.
- **The two `env` tiers merge because they were never two.** Both are appended, both are refused for
  a name the operator owns, and the renderer drops owned names from both. The nesting expressed a
  precedence that does not exist.
- **`command` and `extraArgs` both stay, under those names.** They are two tiers with a real
  difference: `command` is the take-over tier, where the operator synthesizes no engine argument and
  no client environment, the role is marked unmanaged and `CacheAttached` goes to Unknown; `extraArgs`
  is appended after the derived arguments. Renaming `extraArgs` to `args` would make it read as the
  whole argv, which is what `command` means.

### F9 `imagePullSecret` becomes `imagePullSecrets`

A list, on the role, matching `KVCacheBackend.spec.imagePullSecrets`. The list is atomic for the
reason that one already is: a structural schema keys a list only by a required, non-nullable field,
and `LocalObjectReference`'s name is neither.

### F10 The router gains the rest of its image block

`router.imagePullPolicy` and `router.imagePullSecrets`, spelled and defaulted exactly as the role's.
Without them a router image in a private registry cannot be pulled and no field can express it.

### F11 `kvCache.connector` takes the value `mooncake`

The field is an identity discriminator, frozen after creation, reserved so that a second connector
widens an enum rather than appearing as a new field. `auto` reads as "derive it", which is what the
field explicitly does not do. The enum becomes `["mooncake"]`.

### F12 `directTransfer` becomes `kvTransfer`

The old name says the mechanism and not the subject. The new name says what moves.

WHAT THE NAME DOES NOT SAY, THE COMMENT MUST. `kvTransfer` and `kvCache` are ORTHOGONAL and both may
be set at once; they are not two branches of one choice. The direct path does not read `kvCache` at
all — it turns on when the deployment has an `llm-d` router, runs vLLM, is not on Ascend, and the
role's kind is prefill or decode (`model_deployment_connector.go:146-162`) — and when both apply they
are synthesized into ONE connector and one `--kv-transfer-config`
(`model_deployment.go:755-780`). The field's documentation states both facts, because the name
carries neither.

### F13 A replica's name carries a per-instance suffix

`metadata.generateName` is set to `<deployment>-<role>-` and `metadata.name` is left empty; the API
server assigns the suffix. `ModelDeploymentRenderInput.Ordinal` and `modelDeploymentPodName` are
removed.

Removing the ordinal is smaller than it reads. It has exactly one consumer: the name
(`model_deployment_render.go:410`). Keeping it while adding a suffix would mean carrying the slot in
a new label or annotation, and `model_deployment_pod_group.go:19-34` already argues against minting a
second carrier for a derivable fact.

Acceptance:

- A rendered replica has an empty `metadata.name` and a `generateName` of `<deployment>-<role>-`.
- `modelDeploymentPodSpecHash` still does not cover the name, so a replica created under the old
  naming is not stale merely for being old.

### F14 The reconciler converges by count per role

`renderModelDeploymentPods` returns one rendered Pod PER ROLE rather than one per replica, and the
converge loop compares, for each role: the Pods it owns, their spec hash, and the count declared.

- Fewer live than declared, and the group's Workload reports `WaitingForReplacementPods`, or there is
  no Workload yet: create the difference.
- More live than declared: delete the surplus, NEWEST first by `creationTimestamp` and then by name,
  so the longest-serving replicas survive a scale-down.
- Hash differs on a live replica: F15.

Acceptance:

- Scaling 3 to 2 deletes the newest replica, and the choice is asserted rather than observed.
- A replica deleted by hand is replaced under a new name, and the group's Workload is not deleted.

### F15 A replacement is created when Kueue asks for one

The signal is the group Workload's `WaitingForReplacementPods` condition. It is read rather than
re-derived: the predicate deciding whether a departed Pod still counts is Kueue's, it has three
branches, and a copy of it here would agree until the day it did not.

Creating a replacement while the departed Pod is still ACTIVE is not merely early, it is harmful. The
role then has one more active Pod than its PodSet declares, Kueue computes an excess, and
`sortActivePods` selects the newest un-finalized gated Pod to delete. That is the replacement.

A pass deletes AT MOST ONE outdated replica per role, and the replacement is created by the pass that
sees Kueue ask for it. The `RolloutHeldByCache` guard (`model_deployment.go:372-425`) is unchanged and
still runs first.

Acceptance:

- With a departed Pod still active, the pass creates nothing and requeues.
- With `WaitingForReplacementPods=True`, the pass creates exactly the absent count.
- With no Workload at all, the pass creates freely. A group short of its total composes no Workload,
  so waiting on a condition that cannot exist would deadlock a deployment's first pass.
- A template edit on a role with three replicas takes at least three passes, the role never has more
  than one replica away at once, and the Workload is the same object at the end, asserted by UID.

### F16 The pod group's key is the role

`modelDeploymentPodGroupSpec.InstanceType` becomes `Role`. `modelDeploymentPodGroups` returns one
group per role in the roles' order. `modelDeploymentPodGroupNameOf` hashes `namespace/name/role`, and
a deployment with ONE role keeps the readable deployment name.

`modelDeploymentRoleReplicasAnnotation` (`model_deployment_pod_group.go:60`) is removed with it. It
exists because a group could hold two roles, so the group total alone was not a fingerprint of the
shape. A group is now one role, and the group total IS the role's declared count.

Acceptance:

- A one-role deployment renders the same group name it renders today.
- A two-role deployment on ONE `instanceType` renders two group names, both hashed.
- A `replicas` edit on one role marks that role's group resizing and no other.
- No rendered replica carries the removed annotation, and the resizing predicate still catches a role
  rename and a replica count change.

### F18 The protobuf field numbers stay contiguous

Every rename and deletion above vacates a field number. The convention a deletion normally establishes
— retire the number, never reuse it — exists to keep a new field from being decoded as the old one by
a peer built before the change. **These API groups have never appeared in a release**, so no such peer
exists, and carrying the holes would buy nothing while making every later reader wonder what used to
live at 5.

So the numbers are compacted back to a contiguous run from 1, across `KVCacheBackend`, `KVCachePool`,
`KVCachePoolBinding` and `ModelDeployment`. A number a rename frees is reused by the field that
replaces it.

⭐ The gate is NOT the test suite. There is no protobuf round-trip test over these types, and the wire
tags in the generated marshalling code are hard-coded byte literals — so Go tags edited without
regenerating leave every test green and the serialization wrong. Read the numbers out of
`api/worker/v1alpha1/generated.proto`, message by message, and require `make generate` to converge on
a second pass.

Acceptance:

- Every struct in those four files carries protobuf numbers starting at 1, with no hole and no
  duplicate.
- `generated.proto` reports the same numbers as the Go tags.

### F17 The documentation states none of what is now false

| What the page says now | Where | After this spec |
|---|---|---|
| `## Rollout is recreate` | `docs/reference/model-deployment.md:551` | rollout is a rolling replacement, one replica at a time |
| `### One group, or one per instanceType` and its cost table | `:609-627` | one group per role; the two shapes collapse to one |
| "Any replica leaving rebuilds its group" | `:629-635` | a replica leaving is replaced; its siblings keep serving |
| the departure table under "these all restart every role" | `:637-646` | the same causes, and none restarts a role |
| "an evicted replica is held by Kueue's finalizer and cannot leave until the Workload does" | `:649-650` | already false before this spec: Kueue releases it once a replacement exists |
| the frozen and editable field table | `:578-584` | same rules, new field paths |
| "roles on one instanceType are one Workload" | `docs/reference/model-deployment-status.md:118` | one Workload per role |
| "this operator deletes a replica only for a group rebuild" | `docs/reference/model-deployment-status.md:59` | and for a rollout, and for a scale-down |
| every manifest naming a renamed field | `docs/kv-cache/*.md`, `docs/reference/*.md`, `docs/walkthrough.md` | the shapes in Proposal |

Acceptance:

- No page under `docs/` states that a departure rebuilds a group.
- No page under `docs/` contains `engineVersion:`, `quotaCeiling:`, `extraEnvs:`, `directTransfer:`,
  `template:` under a role, or `offload:` under a leader.
- The reference page states the replacement path and names `WaitingForReplacementPods` as what an
  operator reads while it happens, and states the unreachable-node limitation from Risks.

### Carried from the leader high availability spec

Four features and two injection rulings were designed after
[the leader high availability spec](2026-09-06-kv-cache-backend-high-availability.md) and
[the injection spec](2026-08-28-kv-cache-injection.md) shipped. They are recorded here rather than
appended to those documents, because a shipped spec is a record of what was decided at the time it
shipped, and editing one to carry later work leaves a reader unable to tell the two apart.

**THEY KEEP THEIR ORIGINAL NUMBERING, which is not this document's.** The four below are the high
availability spec's `F8` through `F11`, and this spec's own `F8` and `F11` are different features
entirely. Where either document must tell them apart it names the owner.

**Inside this section every other bare label is that document's too** — `F1` through `F7`, the `G`
goals, the `Q` open questions, and a bare "Alternatives". The exceptions are `C1`, `C5` and `C8`,
which are this document's trip questions: they were written in that spec and are planned here, so
they point forward rather than back.

The reason for keeping the numbering rather than renumbering into this document's sequence: the
implementation tasks, the shipped condition names and the trip questions `C1` through `C8` already
name these features by those numbers, in code comments and in a suite. Renumbering here would leave
every one of those pointing at a number this section no longer uses.

#### F8 — `leader.highAvailability.snapshot`: the baseline a standby starts from

A standby under the Kubernetes backend replicates nothing. `standby_controller.cpp:43-44` grants
oplog following only on the etcd backend, and F2 refuses that backend, so the standby the high
availability spec ships runs as a controller with no source of state. What it can have instead is the snapshot
subsystem, which `standby_controller.cpp:42` gates on `enable_snapshot_restore` **alone** — no
backend condition, unlike the line directly below it.

Who writes and who reads: `master_service.cpp:526` starts the snapshot manager when
`enable_snapshot && !enable_oplog_`, which is this project's combination, so **the primary writes
the snapshot and the standby reads it**. (Under the oplog the ownership inverts and the primary
skips generation, `:544`. That branch is unreachable here.)

**The flags are already reachable. The storage is not, and that is the whole reason this field
exists.** `enable_snapshot`, `enable_snapshot_restore` and the object-store keys are absent from
`LeaderExtraArgsRules`, so an administrator can set all of them today through `leader.extraArgs`.
`snapshot_object_store_type=local` resolves its root from the `MOONCAKE_SNAPSHOT_LOCAL_PATH`
environment variable, with **no default** (`local_file_snapshot_object_store.h:19-20`), and
`leader.extraArgs` renders flags rather than environment — so there is no way to supply it.

**CORRECTION, measured rather than assumed:** an earlier draft here said the reachable flags buy a
backend that "reads as configured and restores nothing". They do not. The constructor
**throws** on the unset variable, `master_service.cpp:256-266` rethrows it as a `runtime_error`, and
nothing between there and `main` catches it on either the HA or the single-master path. The hatch
therefore yields a leader that does not start, which is the loud failure this project prefers. What
is unreachable is the capability, not the reporting — and the silent version of the failure is still
the one this field has to avoid producing: a variable pointing INSIDE each Pod's own filesystem
means the primary writes where the standby cannot read, and nothing logs that.

The field:

```yaml
leader:
  highAvailability:
    snapshot:
      persistentVolumeClaimName: <name>   # REQUIRED when snapshot is present
      intervalSeconds: <n>                # optional, the artifact's default otherwise
      retentionCount: <n>                 # optional
```

**One storage shape, not two, and the narrower one.** The store also speaks S3
(`snapshot_object_store_type=s3`), which would need an endpoint, a bucket and a credential
reference — a group of fields plus Secret handling, for a capability a `ReadWriteMany` PVC already
covers inside the cluster. S3 is recorded in Alternatives rather than shipped; widening to it later
adds a field beside this one and breaks nothing.

**REQUIRED: the volume must be `ReadWriteMany`, and admission does not check it.** The PVC may not
exist when the backend is created, and a webhook that reached for it would need a read it does not
have. The reconciler checks the claim's access modes when it can see them and reports the mismatch
as a condition; the walkthrough in F7 states the requirement where a reader meets it first. **Not
counted as the check:** the backend reaching Ready, which it does either way.

**The condition reads the BOUND volume's modes, never the claim's request.** `spec.accessModes` is
what somebody asked for, and a cluster is free to bind a volume that does not honour it; reading the
request would make this check pass on exactly the cluster it exists to catch. So the answer is
`Unknown` until the claim is `Bound` — as it is when the claim cannot be read at all, because a
briefly unreachable API server and a single-writer volume call for different remedies and `False`
would put them in one state.

**The snapshot is not gated on the replica count, and the election beside it is.** A single leader
restores its own last snapshot when it restarts, which is worth having on its own, so the flags
arrive as soon as the field is set. The election's do not, because an image without the Lease
backend refuses to start the moment they appear. The two gates sit in the same block and differ, so
the API type says which is which and a unit test holds them apart.

**No catalog field, because the catalog rides in the object store.**
`EmbeddedSnapshotCatalogStore(SnapshotObjectStore*, cluster_id)` takes the object store itself, so
one shared volume carries both the payload and the index of what exists. The Redis catalog upstream
offers would add a second external dependency to a spec whose entire premise is not having one.

**Thirteen keys join the escape hatch's reserved list, not five.** The earlier count in this
section named four derived keys and one forbidden; tracing each rendered setting to its write points
in the artifact's own source — which `LeaderExtraArgsRules` states as a claim a reader can check,
not as a summary — turns up eight more, and one of the original five is on the wrong side.

Derived, because this API renders the flag: `enable_snapshot`, `enable_snapshot_restore`,
`snapshot_object_store_type`, `snapshot_interval_seconds`, `snapshot_retention_count`. The last two
render only when their fields are set and are reserved all the same, because a key accepted here
would win over a field left unset — an object stating a default it is not running with.

Forbidden, because nothing collides by name and the key costs more than the hatch is worth:

| Key | What it does |
|---|---|
| `memory_allocator` | `master_service.cpp:527` builds the snapshot manager only under the `OFFSET` allocator. That is the artifact's default (`master.cpp:320`), the `if` has no `else` and logs nothing, so any other value turns generation off while every rendered flag, the mounted claim and the object all go on saying it is on |
| `snapshot_backup_dir` | **Moved out of Derived, and the reason inverts what its name suggests.** It is not the object store's path — it is a forensic copy written beside a failed upload, and `master_snapshot_manager.cpp:488` returns the upload error to its caller ONLY while it is empty. Set, each failed payload upload is logged, saved locally and stepped over, and the round reports itself finished |
| `snapshot_payload_store_type`, `snapshot_payload_backend_type` | Deprecated aliases of `snapshot_object_store_type`. `master.cpp:1304-1319` tests the canonical flag first, and this operator renders it unconditionally, so these move nothing while reading as a store that moved — the `port` shape already in the list |
| `snapshot_catalog_store_type`, `snapshot_catalog_backend_type` | Not rendered here at all, and that omission is what leaves them reachable: the design rests on `""` parsing to the embedded catalog (`master_service.cpp:85-87`), which is what puts the index on the same claim as the payloads. Pointed elsewhere, a store nothing here creates or backs up holds the index while the claim holds every payload |
| `snapshot_catalog_store_connstring`, `snapshot_catalog_backend_connstring` | Read only under the catalog kind refused above — the `cxl_path` shape, an accepted key that configures nothing |

**What the baseline does not restore.** The objects a client wrote since the last snapshot are not
in it, so the window is `intervalSeconds` wide and the cache is partially cold after a failover
rather than entirely cold. That is the difference between this and the oplog, and it is the whole
of it **for objects**.

**REQUIRED: C1 reads what came back, not whether something came back.** What a snapshot carries is
the master's metadata shards and its segment state; whatever the store holds as pure runtime state
beside them is a separate question that spec has not settled per property — soft pinning is the one
the earlier survey flagged, and a survey is not a measurement. A restore that recovers the objects
and silently drops a property attached to them is the failure this feature can have while every
assertion about flags, mounts and files passes.

#### F9 — A handover is recorded as an Event, not as a field

A failover is invisible from the object. A pool that lost its cache twice overnight and one that has
been stable for a week present identically, and the first diagnostic question after a latency spike
has no answer anywhere a user is already looking.

**A status field is the wrong carrier, by the high availability spec's own G5.** F5 applied that goal to a
near-identical candidate and refused it: a field copying a value another object already publishes,
whose only purpose is being looked at. `spec.leaseTransitions` on the Lease is exactly that shape, so
a `status.leader.transitions` mirroring it fails the same test — and a goal that stopped three
candidate fields does not get an exception for the fourth.

**An Event is not a field.** When the reconciler observes the Lease's holder change, it records one
Event against the `KVCacheBackend` naming that the leader moved. It reads a Kubernetes object this
operator already watches with access F1 already grants, it names no replica — so the Non-Goal above
is untouched — and it appears in `kubectl describe`, which is where someone diagnosing a spike
already is.

**What it costs, stated here rather than discovered later:** Events are garbage-collected on the
cluster's event TTL, one hour by default. This answers *did service just move* and does **not**
answer *how many times last night*. The durable count remains the Lease's own `leaseTransitions`,
and F7 documents how to read it — that is the answer to the longer-horizon question, and there is
no field for it by design.

**Not counted as this feature:** an Event that only ever fires when a human deletes a Pod. C5
requires it under an ordinary rollout too, since that is the failover users actually meet.

**What "the holder changed" is compared against, since a reconcile has no memory.** The last holder
is kept in a map on the reconciler and nowhere else. A field would be the thing G5 already refused
three times over — a copy of a value the Lease publishes, kept to be looked at — and the Lease's own
`leaseTransitions` is the durable count. **The cost, stated rather than discovered:** a handover
during an operator restart produces no Event, because nothing observed the before. That is the same
class of gap as the Event TTL above, and the same answer covers it.

**The watch on the Lease carries a predicate, and the predicate is load-bearing rather than tidy.**
A Lease is renewed every few seconds for the life of every leader, and one reconcile of this backend
costs three sequential reads of its admin surface — so an unfiltered watch would put a permanent
load on every store in the cluster to deliver one Event per failover. The filter compares the holder
because it is the only field a handover moves.

**The Event names no replica**, which a test asserts directly rather than by reading the format
string. The identity is read to compare two observations and does not leave the map; an Event
carrying it would answer the refused question by accident and would read as entirely reasonable.

#### F10 — A second way for a member to reach the master

F4 hands every member a `k8s://` entry, and that choice is what forces the member image to carry the
leadership backend — which is why the vendor variants had to be built. Q1 records a second path
found afterwards and never measured: the leader Service already contains only the leader, because a
standby is not ready.

This feature **renders both and defaults to neither being a rewrite**: a field selects the entry, and
its default is the `k8s://` shape shipped today. Q1 stops being a question about a design and becomes
a measurement of two rendered objects, run on the cluster trip.

**The measurement already exists and is not this feature's to design.** `case-63` in the end-to-end
suite deletes the serving leader and records, per path, the interval from the delete to the first
store operation that completes after an observed error window — a completed put, because it needs the
client to reach the new master AND a member to have re-registered its segment there. Its verdict
carries `DID_NOT_RECOVER` and `NO_ERROR_WINDOW` beside the number, so a run in which no failover
happened reports that rather than a small interval. What F10 owes it is the second rendered entry to
point at.

**FORBIDDEN: switching the default on the strength of the official deployment guide.** That guide
describes a label selector and a Service, which is the same shape, and it says nothing about how long
a member is unable to reach a master after the leader Pod is deleted. That number is what decides
this, and the guide is evidence about a shape.

**The member's API access does NOT move with the field, and that is the measurement's requirement
rather than an oversight.** Under the Service form the client never reads the Lease, so the account
it is given goes unused — withdrawing it as well would make the two arms differ in two things at
once, and an interval measured across them could not be attributed to either. Dropping the account
is what a verdict for Service would unlock, and it belongs after the number, not before it.

**The field is read only when an election runs, and an EMPTY value reads as the default rather than
as the new form.** The schema defaults it, so an empty string means the object never went through
admission — the same reading every other enum in this design gives — and a renderer that took it for
`Service` would rewrite the master entry of every backend on a cluster whose webhook is absent.

#### F11 — A condition for an image that cannot elect

The Kubernetes leadership backend does not exist before `0.3.11`: `v0.3.10.post2` carries no
`k8s_leader_coordinator` source and no `STORE_USE_K8S_LEASE` option in its `CMakeLists.txt`. A
backend pinned to such an image and configured with `replicas > 1` and `highAvailability` renders
flags the process does not implement, and reports Ready while electing nothing.

**The condition is observed, not parsed.** Reading the version out of the image tag is not a check —
a tag is a name, and the one users pin most often is a digest or a local rebuild. What is observable
without touching the store: **the Lease is the artifact of election**, so a backend whose leader
replicas are Ready while its Lease is absent or holderless has told us what we need, from the
Kubernetes side only.

**REQUIRED: the condition states what was observed, not why.** A holderless Lease is also what a
missing RBAC grant and a still-starting process look like, and the message that names the image as
the cause would be wrong in both. It carries the observation and the two or three things that
produce it, in that order.

**The leader being ready is half the predicate, and it is the half that carries the information.**
A holderless Lease under a leader that has not become ready says nothing at all — it has not had the
chance to campaign — so that reading is `Unknown` rather than `False`. The condition is
`ElectionObserved`, and it is absent below two replicas, where there is nothing to elect between and
therefore no Lease to be the artifact of anything.

#### Injection: two rulings the injection spec predates

The injection spec's acceptance criteria state that a user-set
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is injected and left alone, and that a user-set value variable
is yielded to. **Both halves are superseded, and the reason is measured rather than preferred.**

On SGLang main at `66c7bc83`, `mooncake_store.py:294-314`, `_load_config` is an `if`/`elif`/`else`
over three mutually exclusive SOURCES, and every variable this webhook emits is read only in the
last branch. A user-set config-path key takes an earlier branch, so the Pod carries a full set of
injected variables that nothing reads while the record says the injection succeeded. That is the
silent outcome this design refuses everywhere else, so the key is refused — together with
`--hicache-storage-backend-extra-config`, which outranks even the file and had never been named.

The value variables follow from the same reasoning rather than from the same measurement: injection
is opt-in and its opt-out is explicit, so a declaration is a second answer to a question the Binding
already answered, and it is OVERWRITTEN. The two observability toggles still yield, because they
change no result.

`MOONCAKE_TENANT_ID` is unaffected: the webhook does not write it, so refusing over it would block
the one workaround an operator running a patched engine has.

### Notes / Constraints / Caveats

- **The Workload's PodSet template is captured at first composition and never updated.** Kueue
  composes it from the Pods it first saw and `equivalentToWorkload` never looks at a template again,
  so after a rolling template edit the admitted PodSet describes the old Pod. Harmless for what
  decides quota, because `roles[].resources` is frozen and the request is derived from it.
- **A pass that creates must not read a stale cache.** The replica list is a cached read
  (`listModelDeploymentPods`, `ctrlclix.WithoutQuorum`). Under name-keyed converge a create the cache
  had not caught up to was rejected as `AlreadyExists` and cost one requeue; under count-keyed
  converge with generated names it creates a DUPLICATE. The read is therefore taken through the
  `APIReader` on the one path that needs it: a role short of its declared count.
- **Nothing addresses a replica by name.** The Service selects on the role's labels, status
  attributes a Pod to a role by `app.kubernetes.io/component` (`model_deployment_status.go:357`), and
  the direct-transfer pairing is role-level.

### Boundaries

- No enum is widened. F11 renames one value of a single-value enum.
- No frozen field is unfrozen, and no field becomes frozen.
- The joint-admission controller is not changed. F16 widens WHEN it engages, by making more
  deployments multi-group, without changing what it does.
- No e2e case is written here. The cluster readings belong to the standing verification matrix.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| A replica on a node that is NotReady but still registered is never replaced: its Pod keeps `nodeName` and a Running phase, so Kueue counts it active and never reports it absent. | Stated in the documentation rather than worked around. Force-deleting a Pod whose node may still be running it is how two processes end up holding one accelerator. Deleting the Node object resolves it, which is what a cluster that replaces nodes already does. |
| Park moves from an edge path to the main path: with per-role groups every prefill/decode deployment is multi-group, so "one half admitted, the other half waiting" becomes the ordinary shape. | The barrier and its 30-minute park already exist and already report themselves. What changes is exposure, so F16's acceptance includes a park reaching status on a same-`instanceType` prefill/decode deployment, which no test covers today. |
| `extraArgs` as a list makes a split flag pair representable, and the collision check cannot see a key in the second entry. | The refusal in F2, whose acceptance includes the exact pair that motivates it. |
| Deleting `template.resources` replaces a good message with an unknown-field error. | Accepted, and the reason is in F8. The reference page gains a line naming where the request goes. |
| A create the API server accepted but whose response was lost leaves an extra replica. | Self-correcting: the next pass counts one too many and deletes the newest. The cost is one wasted Pod start. |
| Replica names stop being predictable, so a runbook naming `<deployment>-<role>-0` breaks. | These resources are in no released version. The reference page gains the label selector to use instead. |
| A cluster tracking the default branch holds objects carrying the removed fields, which become unwritable once the schema drops them. | Out of scope by the first Non-Goal, and the same exposure `KVCacheBackendTransport`'s own comment already records for its enum respelling. Recreating the object is the procedure. |

## User Stories

#### Story 1 — Device memory joins the pool without a second backend

As a **cluster administrator** sizing a cache pool on nodes that already run inference, I want one
backend to carry a DRAM group and a VRAM group on the same nodes, so that device memory holds hot blocks
and the host memory plus NVMe behind it holds the rest, without a second backend and a second master to
operate.

#### Story 2 — A group speaks its own fabric

As a **cluster administrator** whose VRAM group reaches its peers over RDMA while the DRAM group stays on
TCP, I want to declare the protocol on the group and have unset groups keep inheriting the backend value,
so that I never have to split the backend just because the two media disagree on transport.

#### Story 3 — A VRAM image I do not have to build myself

As a **cluster administrator**, I want the project to publish `mirrored-mooncake` builds with the vendor
runtimes compiled in (`cuda`, `cann`, `rocm` targets, tagged with the toolchain version, e.g.
`0.3.13.post1-cuda13.0`) on the Mooncake version line matching my engine release, so that enabling a
VRAM group is choosing a tag — through the group, the backend, or the cluster setting — not maintaining
a forked image build.

#### Story 4 — An engine is refused, not started, on a transport no group serves

As a **platform engineer** deploying an engine with a fabric requirement, I want admission to refuse the
deployment when no group in the bound pool serves a compatible transport, so that the failure is a typed
condition at submit time instead of a container that starts and never caches.

#### Story 5 — Reaching the device without a plugin to name

As a **cluster administrator** on a cluster whose accelerator nodes are not carved by a device plugin
into extended resources, I want to grant a VRAM group what it needs by declaring it — a security
context, the host paths carrying the vendor's user-space driver, and the runtime class that injects one
— so that the group runs on nodes no plugin advertises, and so that reading the object tells me exactly
what was granted.

Every grant is declared and none is inferred, and the reason is measured: privilege opens the node's
device tree under `/dev`, while a vendor's user-space driver lives outside it
(`/usr/local/Ascend/driver` and the DCMI library on Ascend; `libcuda.so` injected by the container
runtime on NVIDIA). An earlier design that inferred privilege produced a member that started, reported
healthy, and could not allocate a segment on two of the three vendors.

## Design Details

### Commands

Local, in this worktree. No cluster.

```bash
go test ./api/... ./pkg/worker/...
make lint
make generate                       # REQUIRED after every task that touches api/
git status --porcelain              # must be empty after generate
make lint docs < /dev/null
```

`make generate` is in the loop for Part A and out of it for Part B, which touches no API type. Run
`make lint` BEFORE `make generate`, since lint is an edit pass; judge whether lint changed anything
by comparing file hashes rather than by its exit code.

### Project Structure

- `api/worker/v1alpha1/` — the four types and their generated artifacts.
- `pkg/worker/webhooks/worker/` — every admission rule this spec adds, removes or moves.
- `pkg/worker/kvcache/mooncake/` — leader and member rendering.
- `pkg/worker/controllers/worker/` — the pool ledger, the connector synthesis, the render, the
  converge loop, the group bookkeeping and the status.
- `docs/kv-cache/`, `docs/reference/` — F17.

### Code Style

Comments state the rule directly and carry no task identifiers. The comments this change deletes are
load-bearing arguments for a design being replaced: each is either rewritten to argue for the new
design or removed with the code it explained, and none is left describing a mechanism that no longer
exists. Where a rename drops information the old name carried, the field's doc comment states what
the name no longer says: F12 is the case that makes this a rule rather than a preference.

### Implementation Plan

Four groups, and the order between them is forced rather than preferred. The per-group work (T1-T5)
lands first because the consolidation renumbers fields it introduces. Part A (A1-A6) lands before
Part B (P1-P6) because the renames touch the same files Part B rewrites, and running B first means
writing those files twice. `V1` is last because it needs all of it standing on real hardware.

Within a group the tasks are sequential wherever their `Owns` intersect, which is most of them,
because one API file has one set of consumers.

#### Per-group medium and transport

no Go. T2-T5 are the API change; T2 is its foundation (schema + `make generate`), T3 and T4 depend
only on T2, T5 depends on the behavior settling. Each task leaves the tree building; a
`make generate`-clean + `make lint` checkpoint sits after T2, T3, and T4. The measured item M1 is an
acceptance gate on shipping and runs on the NVIDIA validation host against the cuda variant image; M2 is
covered by T4's envtest.

- [x] **T1 — the image: build-target input + variant targets in the one Dockerfile.**
  Add the optional `target` input to `.github/workflows/base-image.yml` (plumbed to `_image.yml`,
  including build-cache-ref isolation per target); add the vendor-generic `cuda` / `cann` / `rocm`
  builder/runtime stage pairs to `pack/mirrored-mooncake/Dockerfile` with dispatch-overridable base ARGs
  (defaults per `gpustack/runner`; CUDA flags per the Mooncake repository's own build); rewrite the
  `Dockerfile:207-209` comment. **Accept:** a dispatch with `target` + `tag` produces
  `0.3.13.post1-cuda13.0`; a dispatch with no `target` reproduces today's `-cpu` image unchanged; the
  `cuda` and `rocm` variant images build on both `0.3.13.post1` and `0.3.10.post2`, the `cann` variant
  on `0.3.13.post1` and `0.3.11.post1`. **Verify:** workflow dispatch on the
  workflow branch for each variant × version line; smoke-load the store wheel from each produced image on the
  matching vendor validation host.
- [x] **T2 — the API and the webhook: widen the enum, add the fields.**
  Widen `members[].medium` to `["DRAM", "VRAM"]` (`api/worker/v1alpha1/kv_cache_backend.go:469`); add
  `members[].transport.protocol` (optional, same enum as the backend field) and
  `members[].securityContext`, `members[].hostPaths[]` and `members[].runtimeClassName`. Update the guard test
  `TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns` (`api/worker/v1alpha1/kv_cache_backend_test.go:78-96`)
  — its failure message says "do not widen without a renderer"; this spec lands the renderer, so the test
  now asserts exactly `["DRAM", "VRAM"]`. Webhook: a colliding `hostPaths[]` mount path is refused; the
  medium-immutability rule becomes live. Run `make generate`. **Accept:** the tree builds; the guard test
  passes with the widened enum; a group whose mount path collides is refused with a
  `field.Error`. **Verify:** `go test ./api/worker/... ./pkg/worker/webhooks/worker/... && make generate && make lint`.
- [x] **T3 — the render: per-group protocol and per-medium accounting.**
  Add `MemberProtocolForGroup` with inheritance; rekey `applyMemberFabric` on the group's effective
  protocol; split `memberRequests` by medium per the table above; correct the `MOONCAKE_GLOBAL_SEGMENT_SIZE`
  comment. **Accept:** table tests render a two-group backend into two DaemonSets covering success
  criterion 3, and fabric privileges follow each group's own protocol. **Verify:**
  `go test ./pkg/worker/kvcache/... ./pkg/worker/controllers/worker/... && make lint`.
- [x] **T4 — admission and engine wiring: the matched group's protocol, or a refusal.**
  Make `checkTransport` pool-aware per the design above; pass the matched group into
  `MemberProtocolForGroup` at both engine-wiring call sites. **Accept (measured item M2):** an envtest
  binds an engine with a fabric constraint to a pool whose groups serve only an incompatible protocol and
  asserts refusal with `TransportUnsupported`; a second case asserts the engine is handed the matched
  group's protocol when one group satisfies. No hardware needed. **Verify:**
  `go test ./pkg/worker/kvcache/inject/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... && make lint`.
- [x] **T5 — the docs: the reference page and the worked pair.**
  Update `docs/kv-cache/backend.md`: the widened enum, per-group transport and its inheritance, the
  variant tags and the vLLM version mapping (`0.3.13.post1` for vLLM 0.28.0+, `0.3.10.post2` before;
  cann ships `0.3.13.post1` and `0.3.11.post1`), the
  declared device grants and why nothing charges device memory, and a clear "measured on the
  NVIDIA validation host" pointer for the VRAM-plus-local-disk combination (banner until M1 lands). Add
  the RFC's companion worked pair (`KVCachePoolBinding` + `ModelDeployment`, and the plain-Pod injection
  annotations) where the docs skill routes it; state that tenancy needs an engine build that reads
  `tenant_id`; state that a VRAM group needs a `USE_VRAM_SEGMENT=ON` build and that the stock `-cpu`
  default is not one. **Accept:** `make lint docs` clean; every claim on the page traces to this spec or
  to the verified upstream coordinates.

#### Part A: the API surface

of them, because one API file has one set of consumers.

- [x] **A1 · KVCacheBackend: the passthrough tiers and the offload block**
      Blocked by: None
      Owns: `api/worker/v1alpha1/kv_cache_backend*.go`,
      `pkg/worker/webhooks/worker/kv_cache_backend*.go`,
      `pkg/worker/controllers/worker/kv_cache_backend*.go`,
      `pkg/worker/kvcache/mooncake/**`
      Gate: review
      Acceptance: F1, F2, F3, F4. `leader.offload` is gone with its two paired rules; `extraArgs` is a
      list on both the leader and the member with the two new rules; `extraEnv` is a keyed list on
      both; `extraEnvs` no longer appears anywhere.
      The controller package and the API package's own tests are owned here because turning
      `localDisk` into a list reaches every fixture that spells the field, and that set is not
      visible from the API file: the tier survey, the tier cleanup and their tests carry the literal
      too. Leaving any one of them behind leaves the tree uncompilable, so the rename and the fixture
      migration are one change and cannot be split across tasks.
      Verify: `go test ./api/... ./pkg/worker/kvcache/... ./pkg/worker/webhooks/...` then `make generate` with an empty `git status --porcelain`

- [x] **A2 · KVCachePool and KVCachePoolBinding: the quota pair**
      Blocked by: None
      Owns: `api/worker/v1alpha1/kv_cache_pool.go`,
      `api/worker/v1alpha1/kv_cache_pool_binding.go`,
      `pkg/worker/webhooks/worker/kv_cache_pool.go`,
      `pkg/worker/webhooks/worker/kv_cache_pool_binding.go`,
      `pkg/worker/controllers/worker/kv_cache_pool.go`
      Gate: review
      Acceptance: F5, F6.
      Verify: `go test ./api/... ./pkg/worker/controllers/worker/... -run 'KVCachePool'` then `make generate` with an empty `git status --porcelain`

- [x] **A3 · ModelDeployment: engine, connector, kvTransfer, router**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment*.go`,
      `pkg/worker/controllers/worker/model_deployment_connector.go`,
      `pkg/worker/controllers/worker/model_deployment_router.go`
      Gate: review
      Acceptance: F7, F10, F11, F12, including the doc comment F12 requires.
      Verify: `go test ./api/... ./pkg/worker/controllers/worker/... -run 'ModelDeployment'` then `make generate` with an empty `git status --porcelain`

- [x] **A4 · ModelDeployment: flatten the role template**
      Blocked by: A3
      Owns: `api/worker/v1alpha1/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment*.go`,
      `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment.go`
      Gate: review
      Acceptance: F8, F9. `ModelDeploymentTemplate` no longer exists.
      Verify: `go test ./pkg/worker/... -run 'RenderModelDeploymentPod|ModelDeploymentWebhook'` then `make generate` with an empty `git status --porcelain`

- [x] **A5 · Documentation for Part A**
      Blocked by: A1, A2, A3, A4
      Owns: `docs/kv-cache/**`, `docs/reference/kv-cache-injection.md`, `docs/walkthrough.md`
      Gate: none
      Acceptance: the last two rows of F17's acceptance, for the Part A names only.
      Verify: `make lint docs < /dev/null`

- [x] **A6 · Compact the protobuf field numbers**
      Blocked by: A1, A2, A3, A4
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`, `api/worker/v1alpha1/kv_cache_pool.go`,
      `api/worker/v1alpha1/kv_cache_pool_binding.go`, `api/worker/v1alpha1/model_deployment.go`
      Gate: review
      Acceptance: F18.
      Verify: read every message in `api/worker/v1alpha1/generated.proto` against the Go tags, then
      `make generate` twice with per-file content compared. The test suite has no discriminating
      power here; see F18.

#### Part B: a replaceable replica

- [x] **P1 · Group by role**
      Blocked by: A4
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go`,
      `pkg/worker/controllers/worker/model_deployment_pod_group_test.go`
      Gate: review
      Acceptance: F16.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'ModelDeploymentPodGroup|ModelDeploymentGroupsResizing'`

- [x] **P2 · Generated names**
      Blocked by: P1
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`,
      `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Gate: review
      Acceptance: F13.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'RenderModelDeploymentPod|ModelDeploymentPodSpecHash'`

- [x] **P3 · Converge by count, replace on request**
      Blocked by: P2
      Owns: `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_test.go`
      Gate: review
      Acceptance: F14, F15. The `DeletionTimestamp` sweep that widened `rebuild` to any group with a
      terminating member is removed; `rebuild` is the resizing set alone. The quorum read on the
      short-role path is in place and asserted.
      AND: **F13 is not in effect until this task lands.** P2 renders `generateName` but the
      reconciler immediately overwrites it with the ordinal name, because the converge loop still
      identifies a replica BY NAME and a server-assigned suffix is nothing it can predict. So every
      Pod this operator creates today still carries `<deployment>-<role>-<ordinal>`, and the deadlock
      F13 exists to break is still there. **Delete that bridge** — the comment block beginning "The
      render produces a Pod with a name prefix" in `renderModelDeploymentPods`, plus the two lines
      under it that clear `GenerateName` and mint `Name` — and assert that a created replica carries
      an empty name. A pass that leaves the bridge standing satisfies every test in the suite while
      delivering none of F13.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **P4 · What the status says while a replica is being replaced**
      Blocked by: P3
      Owns: `pkg/worker/controllers/worker/model_deployment_rollout.go`,
      `pkg/worker/controllers/worker/model_deployment_status.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout_test.go`,
      `pkg/worker/controllers/worker/model_deployment_status_test.go`
      Gate: review
      Acceptance: `ReplicasUpToDate` distinguishes a rollout in flight from a replacement in flight,
      and the group-count messages read correctly now that every prefill/decode deployment is
      multi-group.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'ModelDeploymentRollout|ObserveModelDeploymentQuota|ComputeModelDeploymentStatus'`

- [x] **P5 · A park on a same-instanceType pair**
      Blocked by: P1
      Owns: `pkg/worker/controllers/worker/model_deployment_joint_admission_test.go`
      Gate: none
      Acceptance: a prefill/decode deployment whose roles name ONE `instanceType` is held by the
      barrier and parked after the bound, and the parked state reaches status. No production code
      changes; if one is needed, it belongs to P3.
      Verify: `go test ./pkg/worker/controllers/worker/... -run JointAdmission`

- [x] **P6 · Documentation for Part B**
      Blocked by: P3, P4
      Owns: `docs/reference/model-deployment.md`, `docs/reference/model-deployment-status.md`
      Gate: review
      Acceptance: F17, every row not covered by A5.
      Verify: `make lint docs < /dev/null` and `go test ./pkg/worker/controllers/worker/... -run Docs`

#### The verification trip

- [x] **V1 — The verification trip**, answering `M1`, R1, R2, C1 through C8, and the third quota
      comparison, in one visit to real hardware.
      Blocked by: T5, A6, P6
      Owns: `.agents/skills/gpustack-operator-e2e/**`
      Gate: review
      **SCOPE: NVIDIA and CUDA only.** The Ascend and AMD variants build and smoke, and no validation
      host for either is guaranteed, so the write-offload-read-back measurement stays NVIDIA-only
      until that hardware exists. That is the position the per-group Open Question already took; this
      task carries it out rather than widening it. The cann and rocm cells of every table below are
      recorded as not covered by this trip, with that reason, so a later reader sees an absence that
      was decided rather than one that was overlooked.
      **Failure first: no end-to-end case is authored before the trip.** Each question is run by hand
      until it yields a counter-example, and only then does a case get written against the corrected
      behaviour and re-run. The suite additions are an output of this task, not an input to it.
      It also closes the one verification gap `T1` left: `T1` asks that the store wheel be
      smoke-loaded from each produced image on the matching vendor validation host, and the tree
      cannot say whether every variant and version line was. The cuda variant is loaded on the NVIDIA
      validation host for the `0.3.13.post1` line.
      **THE `0.3.10.post2` LINE IS OUT OF SCOPE, AND NOT FOR WANT OF A HOST.** No configuration of
      this operator can start that binary: the leader renders `-pod_name` and `-pod_namespace`
      unconditionally, and a master of that line exits at flag parsing on both. Multi-tenancy adds two
      more it does not know, so turning multi-tenancy off does not recover it either — and
      multi-tenancy is the premise of every quota and reuse-domain rule this spec carries, which is
      why recovering that line would yield a version that cannot answer most of these questions
      anyway. WHAT DOES NOT COUNT AS COVERING IT: building the image, loading its wheel outside a
      cluster, or a green reading from the other line. Whether to make those flags conditional, or to
      state the floor this operator supports, is left open and is not decided here.
      Acceptance: every row of the three tables below — the gates, the riders, and the leader high
      availability questions — carries a reading and a verdict, and each red one names the carrier it
      reports back to. The third quota comparison carries a reading for all three of a pool's
      numbers: the declared ceiling, the sum its Bindings claim, and the capacity the backend itself
      reports.
      Verify: the run report, with the raw readings rather than a summary of them.

#### Carried from the leader high availability spec (H)

These tasks belong to
[the leader high availability spec](2026-09-06-kv-cache-backend-high-availability.md) and are
recorded here for the same reason its `F8` through `F11` are, in that spec's own numbering. They all
landed before this spec opened; none is work this spec plans. Its `T17` is not among them, because
`V1` above IS that task.

- [x] **T9 — e2e: an induced failover** on a single-node Kubernetes cluster. Service moves, no member
      restarts, and no fault is reported during the transition. Tracked as
      https://github.com/gpustack/gpustack-operator/issues/278. Two things it must check that no unit
      test can: that the members' `k8s://` entry actually carries them across an election, and that
      the two rendered ServiceAccounts are sufficient — every RBAC assertion here is against a
      rendered object, never against an API server that enforced it. **DONE, and the record was
      elsewhere:** `case-62` (three replicas elect one, the two accounts are sufficient and no more,
      an induced failover moves the Lease) and `case-64` (a hard-killed leader still yields it, the
      members come through) carry it, and the issue was closed against them. This task went on
      reading unticked because nothing connects a suite case back to the spec task it satisfies.

The second round:

- [x] **T10 — `leader.highAvailability.snapshot`** (F8): the field, the volume and its mount, the
      derived flags plus `MOONCAKE_SNAPSHOT_LOCAL_PATH`, the keys joining the reserved list, and the
      admission rule that refuses a snapshot naming no claim. The reconciler check on the claim's
      access modes lands here too, reported as a condition rather than a refusal. **Two counts in
      the feature above were wrong and are corrected there:** the reserved list takes thirteen keys
      rather than five, and `snapshot_backup_dir` is forbidden rather than derived — it is a
      forensic copy directory whose effect is to stop reporting failed uploads, not the object
      store's path. The condition is `SnapshotStorageShared`, and it reads the bound volume's access
      modes rather than the claim's request.
- [x] **T11 — The handover Event** (F9), recorded when the Lease's holder changes. No new access:
      F1's Role already carries the read. FORBIDDEN: a status field; G5 and F5 say why. It brought
      two things the feature did not name: a Lease watch whose predicate is what makes the watch
      affordable at all, and an in-memory last-holder map, whose cost is recorded in the feature.
- [x] **T12 — The second member entry** (F10), as
      `leader.highAvailability.memberAddressing`. Both render, the default does not move, and the
      field's doc comment says which one has been measured — neither, until T17. The member's
      account is rendered identically under both, so the two arms of that measurement differ in one
      thing.
- [x] **T13 — The capability condition** (F11) as `ElectionObserved`, keyed on a holderless Lease
      under a Ready leader, worded as an observation with its candidate causes. A holderless Lease
      under a leader that is NOT ready reports `Unknown`: that reading has no information in it.
- [x] **T14 — The rollout-completion predicate** F3 designed and left unbuilt, surfaced as its own
      condition, `RolloutComplete`. FORBIDDEN: adding it to `leaderPodIsReady`; Q3 says why. The
      unit test's first case is three updated replicas of which one is ready — the steady state
      here — so a predicate that carried the `AvailableReplicas` clause back fails the table rather
      than merely describing it.
- [x] **T15 — Documentation** (F7): what `TCP` buys and costs, on the backend page beside the
      transport it qualifies; the snapshot, its storage requirement and its failure mode, on the
      leader page beside the election. The new conditions join the status section, with the note
      that none of the three moves the phase.
- [x] **T16 — The walkthrough page** (F7): a `KVCacheBackend` and a `ModelDeployment` attached to
      it, end to end, with pasteable manifests. It is the delivery surface for both CRs. It owns the
      ORDER and nothing else -- every field explanation is one clause plus a link to the page that
      owns it, recorded in the docs page map so a second account of a field does not grow here.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Part A moves or deletes fields that the existing table-driven fixtures build by hand, so the fixture
helpers move first in each task rather than being patched case by case. Part B's
`model_deployment_render_test.go` and `model_deployment_test.go` assert rendered Pod NAMES in many
places; those assertions move to the `generateName` prefix plus the role label. An assertion that
merely spells the old name is rewritten rather than deleted, because what it pinned — which role a
replica belongs to — is still worth pinning.

#### Unit tests

- `api/worker/v1alpha1`, `pkg/worker/webhooks/worker`, `pkg/worker/kvcache/mooncake`,
  `pkg/worker/controllers/worker`: the acceptance of every feature above. Every new predicate is
  mutation-tested — broken, confirmed red, restored, confirmed green — before it is trusted, and each
  negative case is given a positive baseline in the same table.

New cases, at minimum:

- a backend with no member disk tier renders no offload flag, and one with a tier renders both flags
- `["-rpc_timeout", "5000"]` refused; `["-enable_ha"]` accepted; `["-a=1","-a=2"]` refused
- a leader `extraEnv` entry renders after the derived ones, and a reserved name is refused
- a pool whose Bindings oversubscribe it reports the Condition and refuses no Binding
- a role carrying a flattened `image` renders it, and a role naming none still gets a synthesized one
- a deployment setting BOTH `kvCache` and `kvTransfer` renders one connector carrying both
- a departed replica is replaced under a new name and the Workload UID does not change
- a replacement is not created while the departed Pod is still active
- a template edit rolls a three-replica role over at least three passes, one away at a time
- scale-down deletes the newest replica
- two roles on one `instanceType` are two groups, and a `replicas` edit on one leaves the other alone

#### Integration tests

None. The controller suites in these packages use a fake client and cover the converge loop directly.

#### e2e tests

Five cases came out of this spec's cluster trip and are in the suite: a store write guard carrying a
detector for the read half that does not work yet, and four leader high availability cases covering a
claim that is not shared, the handover Event, a rollout that stays truthful mid-update, and the
tenant ledger gate.

The readings below still need a cluster and belong to the standing verification matrix. None of them
gates this spec.

- A real preemption replacing one replica while its siblings keep serving.
- A park on a same-`instanceType` prefill/decode pair clearing when quota arrives.
- `ElectionObserved` asserted against an image that carries the election flags: the condition reads
  `True` with reason `Electing` and a Lease holder present. No case asserts this condition today
  (measured: zero references to it across the suite). What does NOT count: asserting the
  crash-looping condition on an image that lacks those flags, which is the current workaround's
  carrier and which a fix may legitimately change.
- KV transfer between a prefill and a decode replica, measured as bytes that actually moved rather
  than as a log line naming the connector. What does NOT count: a reading taken over the fallback
  transport, which measures a path this spec did not render.
- The pool-on against pool-off comparison, measured as time-per-output-token flatness under
  concurrency and as time-to-first-token from the second turn onward. What does NOT count: a
  single-turn time-to-first-token comparison, which disaggregation is expected to make worse.

#### Gates (ship gates for the per-group work)

Gates are causally part of #446: if one is red, this spec does not flip to Shipped.

- **M1 — VRAM segment with the local disk tier.** Write keys until offload engages, then read them back
  and assert byte equality, on the NVIDIA validation host against the cuda variant image. Upstream
  covers this combination with no test (verified: no test references VRAM segments at all), and the
  orthogonality evidence is source-level only. Equivalent coverage on Ascend (`cann`) and AMD (`rocm`)
  hardware is an open question below, not part of this gate.
  - **ANSWERED: GREEN.** 192 objects of 4 MiB were written against a 256Mi VRAM segment with a 4Gi
    tier, on the cuda variant image, so eviction was forced rather than hoped for. Placement taken
    BEFORE any read: 137 objects held a disk replica and no memory replica, 55 held both, none held
    memory alone. One of the 137 was read back at full length with a digest matching what was
    written. Every offload switch was rendered by the operator; none was set by hand. The docs banner
    that this gate governed now records the reading instead of the gap.
  - **A trap this gate walked into, recorded because the next writer against this tier will meet
    it:** a put that fails against a full segment means eviction has not caught up, not that the
    write is refused. The offload heartbeat runs on an interval, so a writer that abandons on first
    failure measures "cannot write" where the truth is "eviction in progress" — the first attempt
    here read exactly 64 successes against a 256Mi segment of 4 MiB objects and was nearly reported
    as a product defect. With retries the same probe wrote all 192.
- **M2 — engine refused on a transport no group serves.** Covered by T4's envtest; no hardware needed.
  M2 must be green before this spec ships.

#### Riders (same trip; NOT gates)

Riders pay down existing verification debts that have NO causal relationship to per-group
medium/transport; they ride the same cluster trip because that trip already stands up a P/D
disaggregated deployment over Mooncake. A red rider does not block #446 — it reports back to its own
carrier (R1 to issue #458, R2 to task T10 of #448). The criteria below are taken verbatim from those
carriers, not re-invented here.

- **R1 — #458: is `/v1/models` on the service port a readiness signal for direct-decode.**
  Background: `#462` (`8c7073c2`) switched the directDecode branch's three gates from `/health` to
  `/v1/models`, still on the service port; the argument chain (sidecar answers `/health` itself with an
  unconditional 200, forwards everything else, and answers 503 while the engine is not listening) was
  derived from reading source and has never run on a real cluster. The PR used `Addresses`, not
  `Fixes`; #458 is still OPEN.
  - **Environment:** one real direct-decode replica of the trip's P/D deployment, watched from Pod start
    until the engine is serving. The engine must run WITHOUT an API key — `/v1/models` sits in upstream
    vLLM's `GUARDED_PREFIX`, so a configured key turns the probe into a 401 that reads as "probe broken"
    (`#465` already documents auth parameters as unsupported on the role). The run report must state
    that no API key was configured.
  - **Measure (two curves, not one):** poll BOTH `/v1/models` and `/health` on the service port through
    the whole startup window and record the status-code-over-time series for each. Expected shape:
    `/v1/models` is constant 503 during loading (the sidecar's fixed JSON whose body contains
    `The decode node is not ready`) and turns 200 once the engine serves; `/health` is 200 throughout —
    that constant-200 IS the defect #458 records, which makes it the control curve. Only the two curves
    together are evidence that `/v1/models` discriminates where `/health` does not.
  - **What does NOT count:** a single-moment 200 on `/v1/models` — that is the same shape as "the
    defect persists and the probe was always green"; and the `/v1/models` curve alone without the
    `/health` control.
  - **ANSWERED: GREEN. `/v1/models` discriminates where `/health` does not.** Both endpoints were
    sampled together every two seconds across the whole startup window, with no API key configured on
    either role. `/v1/models` answered 503 on 43 consecutive samples, carrying the sidecar's fixed
    body naming the decode node as not ready, and turned 200 at 86 seconds; three further samples
    after the turn stayed 200. `/health` answered 200 from the first sample onward, while the engine
    was not yet listening — that constant 200 is the defect #458 records, and having it beside the
    other curve is what makes the reading evidence rather than a snapshot.
  - **Two mechanical facts the run turned up, both of which cost an attempt:** the sidecar is a
    NATIVE sidecar, declared in `initContainers` with `restartPolicy: Always`, so judging its
    presence from the container list finds nothing while the Pod reports two containers ready of
    two. And a Service has no endpoints until its Pod is ready, so sampling through the Service
    cannot see the window this question is about — "on the service port" means that port number,
    reached on the Pod's own address.
- **R2 — #448 task T10: GPU data-plane acceptance.**
  - **Environment:** RDMA-capable nodes. T10's own `Blocked by:` asks for RDMA, and the 2026-09-15 round
    went over TCP — so if this trip's cluster is TCP-only, T10 stays unticked no matter how green the
    run is, and the report must say so. A green TCP run is NOT T10.
  - **Measure (what fills it, verbatim from T10):** for one successful request, the before/after
    counter deltas on BOTH the prefill and decode sides + the router's P/D decision counters + the
    shared pool's blocks, cross-validated against each other. Plus the readings T10 still misses: TTFT
    compared against a single-role deployment (baseline on the same trip); the number of blocks moved;
    and "the decoder does not recompute the prompt" verified on its own for the first time.
  - **What does NOT count (all three were observed on 2026-09-14 while the transfer still failed):**
    metrics merely existing on both ends; the request reaching prefill; Mooncake "starting to send"
    (that round recorded 240 descriptors / 983040 bytes with the transfer never completing). The
    instrument must be read in a state where it is forced to report a different value.
  - **ANSWERED, and the answer is that it is UNREACHABLE — not on this cluster and not on any
    other.** The blocker is this operator, not the hardware. Host-fabric access — `hostNetwork`,
    `/dev/infiniband`, `IPC_LOCK`/`SYS_RESOURCE`, and on EFA the device resource request that makes
    the adapter openable — is rendered into a backend's MEMBER Pods only. The engine Pods that
    consume the pool get none of it: the injection path renders the transport's NAME and nothing the
    fabric needs, so an engine either fails to install the transport or falls back to TCP silently.
    Provisioning RDMA nodes would therefore buy a green TCP run, which this question's own
    environment clause already rules out.
  - **What that costs, stated so it is not read as a gap in this trip:** R2 is a rider, and the rule
    above applies — it reports back to task T10 of #448, which stays unticked. Closing it needs a
    design decision that has not been taken (whether tenant-adjacent engine Pods should receive
    `hostNetwork` and those capabilities at all, and if so whether the opt-in sits on the deployment
    or is derived from the referenced backend), and then an implementation. Both are outside this
    spec.
  - **What does NOT count as covering it:** an RDMA-capable cluster with a green run on it;
    member-side fabric readings, which exist and are not this; or reading the absence of an engine-side
    reading as "untested" rather than as "unreachable through any published path".

#### The leader high availability questions (C1 through C8)

These eight questions and the rule for answering them belong to
[the leader high availability spec](2026-09-06-kv-cache-backend-high-availability.md) and are
planned here, because they need the same cluster and the same images as `M1` and the riders. That
spec is not edited to say so; this section and `V1` above are what its trip became. Every question
here answers for the leader high availability work, NOT for per-group medium and transport, so a red
one does not block #446 — it reports back to that spec exactly as a rider reports back to its own
carrier.

`F8` and `F11` in the table below are THAT spec's, carried in the section "Carried from the leader
high availability spec" and numbered as they are there. They are not this spec's `F8` and `F11`.

**The working order is failure first.** No end-to-end case is written ahead of the trip. Each
question below is run by hand against a live cluster until it produces a **counter-example** — the
concrete way the thing is wrong, or the reading that settles it — and only then is a case written
that asserts the corrected behaviour and re-run. A case authored before the trip encodes what the
author expected to happen, and the failures worth having are the ones nobody predicted.

The questions, each with what would settle it and what does not count:

| # | Question | Settled by | Not counted |
|---|---|---|---|
| C1 | Does a key survive a failover with F8 on? | The same client reading a key it wrote before the handover, after it | The standby reaching Ready; a snapshot file existing on the volume |
| C2 | What is lost in the window? | Keys written inside one `intervalSeconds` before the handover, counted | That "most" keys came back |
| C3 | Does F8 fail loudly on a `ReadWriteOnce` claim? | The condition, and the second Pod's own error | The backend reaching Ready, which it does either way |
| C4 | Q1: how long is a member without a master, per path? | `case-63` against both rendered entries: two intervals, each the first put completing AFTER an error window | Either path working at all; any interval recorded when the verdict reads `NO_ERROR_WINDOW` |
| C5 | Does the handover Event fire on a rollout? | An image bump on a 3-replica leader | A hand-deleted Pod |
| C6 | Does F11 discriminate? | The silent half alone: `0.3.13.post1` stays quiet. The firing half is UNREACHABLE, see below | Only the firing case; also any reading that calls the firing half untested rather than unreachable |
| C7 | Does the rollout complete under HA with the Q3 predicate? | `kubectl rollout status` returning on a multi-replica leader mid-update | A single-replica rollout |
| C8 | `M1` above: a VRAM segment and a local disk tier, written and read | Per the `M1` gate's own criteria | — |

C8 and `M1` are one run, not two. `M1` is a gate for this spec and C8 is a question for the leader
high availability spec, and the same reading answers both.

**C6's firing half cannot be run, and the reason is this operator rather than the hardware.** The
leader renders `-pod_name` and `-pod_namespace` on every command line it builds, and a
`0.3.10.post2` master exits at flag parsing rather than starting, so the condition that half asks
about is never reached on any published image. Turning multi-tenancy off removes two other flags
that line also rejects, and still leaves those two. So C6 is answered by its silent half plus this
statement, and a later reader should not plan a run that the rendered command line forecloses.

**REQUIRED before C8 is run: read the cluster's default container runtime.** `M1` exercises three
device-injection paths and two of them exist only for a cluster **without** a default vendor runtime
— with one configured, `extraEnvs` is satisfied automatically and `hostPaths` and `runtimeClassName`
are never reached. A green `M1` on such a cluster reports nothing about the two, so the reading
(`containerd config dump`, or the RuntimeClass list) is taken first and decides whether the trip
uses that cluster.

The e2e cases for C1 through C8 are **authored after the trip, not before it**. The exception is
what the suite already holds, which is not a pre-written expectation but a measurement someone else
already built: `case-62` and `case-64` cover the induced failover, and `case-63` is C4's instrument.
Those are run, not authored. The authoring rule governs C1, C2, C3, C5, C6 and C7, which have
nothing.

#### Trip sequencing

R1 and R2 CAN run in one trip on one P/D deployment: R1 needs the startup window and R2 needs steady
state, and the window comes first by construction. Sequence: (1) roll out the P/D deployment bound to
the trip's backend and capture R1's two curves during the decode replica's startup window; (2) once
serving, run R2's steady-state request with before/after counters, block counts, and the
no-recompute check; (3) bring up (or have already up) a single-role deployment on the same cluster for
R2's TTFT baseline; (4) M1 runs on the NVIDIA validation host against the cuda variant image, in
parallel with (1)-(3) since it needs its own hardware, and that same run answers C8. One cluster, one
trip; only the TTFT baseline adds a second deployment.

C1 through C7 run on the trip's own cluster against the leader backend, and they are ordered by what
each one disturbs: C3 and C6 change a manifest and are run before anything that measures a window;
C1, C2 and C4 induce a handover and are run next; C5 and C7 need a rollout and are run last, because
a rollout leaves the leader on a different Pod than the one every earlier reading was taken against.

## Alternatives

**Ship Part A and Part B as two specs and two pull requests.** Refused for the reason the user's own
instruction gives: both are breaking changes to the same objects, and landing them apart makes every
consumer absorb two rounds of churn for one release. The ordering constraint is the second reason —
Part A rewrites the files Part B rewrites, so separate PRs would rebase one onto the other anyway.

**Keep the ordinal and add a suffix beside it.** `<deployment>-<role>-<ordinal>-<suffix>` preserves a
readable slot and a deterministic scale-down. Refused because the slot then needs a carrier of its
own, and the project has written down why a second carrier for a derivable fact is a bad trade. The
determinism it protects is recovered by ordering on `creationTimestamp`.

**Put the spec hash in the name.** `<deployment>-<role>-<hash8>` makes a template edit produce new
names with no per-instance randomness. Refused because it solves the smaller half: a replacement for
a DEPARTED replica renders the same hash and wants the same name, and the departure case is the one
that costs an operator a restart nobody asked for.

**Leave the grouping key on `instanceType`.** Coherent and better than today. Refused as a stopping
point: with replaceable names the only thing a group still decides is what a SHAPE change tears down,
and leaving that on a hardware field means a `replicas` edit on prefill restarts decode whenever the
two happen to share a profile.

**Make `extraArgs` a list of `{name, value}` rather than `[]string`.** Keeps key uniqueness in the
schema, like `extraEnv`. Refused because it would be a third shape for arguments, and the deployment
side cannot adopt it: argparse's split form has no name/value pair to map onto.

**Rename `extraArgs` to `args` once the template is flattened.** Refused in F8: `args` reads as the
whole argv, which is `command`'s meaning.

- **One Dockerfile per variant.** Rejected in favor of build targets: the variants share almost
  everything, and the workflow's `target` input keeps the difference to a base image and a final stage —
  one file to maintain, tags chosen at dispatch.
- **Ship per-group transport first, defer the medium value.** Rejected: the only user story for
  per-group transport is the medium mismatch (Story 2 rides on Story 1); shipping it alone adds API
  surface whose motivation does not exist yet.
- **`mediums: [DRAM, VRAM]` on one group.** Rejected by the RFC and confirmed by upstream: one binary is
  one medium, so the API would express something no build can produce.
- **Hard-require `image` on VRAM groups.** Rejected: the default image is an administrator-editable
  setting, so a hard requirement breaks the cluster that configured its default correctly.
- **A per-group `deviceResourceName`, charging one accelerator per VRAM member.** Implemented, then
  removed before this spec shipped, on a measurement: a member's segment is one `cudaMalloc` on one
  device, so the request took a whole accelerator from inference to account for a fraction of one
  device's memory, on a node where the member could not use the rest. It also did not do the thing it
  read as doing — a member is a DaemonSet placed by `nodeSelector`, so no scheduler consults the request
  to choose its node. What it did do is bookkeeping, and the bookkeeping bought a worse trade than the
  double-use it prevented.
- **Infer privilege from an empty `deviceResourceName`.** Implemented, then removed in the same pass. It
  granted the node's device tree, which is where the device nodes are and is not where the vendor's
  user-space driver is, so it covered AMD and left NVIDIA and Ascend with a member that started and
  could not allocate. A privilege inferred from an absent field is also one nobody can see in the object
  that granted it. Superseded by the declared grants in Story 5.

## Open Questions

Three questions the planning opened and closed are recorded beside the rule they belong to rather
than here, because an open-question list is the wrong carrier for something already decided: the
replacement rate is per role and its consequence is stated with F14; the coupling between a frozen
field and the admitted PodSet is stated under Notes; and whether the Ascend and AMD variants get the
same measured coverage as NVIDIA is answered by `V1`'s scope, which is the position the fourth
question below already took.

- A member's segment is on ONE device, measured upstream: one `cudaMalloc` per split, splits sized by
  the transport's registration limit, no `cudaSetDevice` anywhere on the path. So a node with eight
  accelerators contributes a slice of one of them, and the seven others are reachable only by running
  more members on that node — the several-members-per-node shape `capacityPerMember` already names as
  decided and not done. **Whether to build it stays open, and it is not built here.** What would
  decide it is a deployment wanting more device memory in the pool than one accelerator per node can
  give, and no such deployment exists yet.
- EFA plus VRAM (GPUDirect over libfabric) is untested upstream and out of scope here; the first VRAM
  deployments are expected on RDMA or TCP.
- Whether the operator's `kv-cache-backend-image` default ever flips from `-cpu` to a vendor variant
  is left to a later decision and **stays `-cpu` here**. Per-group and per-backend overrides make the
  flip unnecessary, and making it would change the image under every existing backend that names
  none.
- Whether `quota.total` wants a third reading is **answered by `V1` rather than deferred again**. The
  pool holds a declared ceiling, a sum of what its Bindings claim, and the capacity the backend
  reports, and only the first two are compared. The third comparison needs a reading from a cluster,
  and `V1` is the visit that takes one.
