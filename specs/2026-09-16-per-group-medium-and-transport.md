# Spec: Per-Group Medium and Transport on `KVCacheBackend` — a DRAM Group and a VRAM Group on the Same Nodes

Status: Planned
Blocked on: M1 — a VRAM segment with the local disk tier on, written and read back on the NVIDIA
validation host against the CUDA variant image. It is the one gate below that needs hardware, and this
spec does not flip to Shipped until it is measured.
Type: Feature
Issue: #446

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

The spec ships in two PRs. **PR-A (image, lands first):** the base-image workflow learns an optional
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
0.23.0 carries it), so a 0.3.10 cann build would serve no engine. **PR-B (this API change):** the
enum, the fields, the renderer, the admission, the docs.

NVIDIA validation hardware exists (address held out of band), so the CUDA variant image and the one
measured acceptance item below have somewhere real to run; equivalent Ascend and AMD hardware is not
guaranteed, and their measured coverage is an open question.

## Motivation

A node that has device memory, host memory, and NVMe can contribute only one of them today. The enum has
one value, so the shape cannot be written down; and even with a second value, the backend-wide transport
would force a VRAM group and a DRAM group onto one protocol, which is the one thing the two do not agree
on. Upstream, one binary is one medium: `USE_VRAM_SEGMENT` is a compile-time option that forces
`USE_CUDA=ON` (`mooncake-common/common.cmake:108,216-220` at `v0.3.13.post1`) and switches segment
allocation to `cudaMalloc` under `#ifdef` (`mooncake-store/src/client_buffer_allocation.cpp:57-99`), so
mixing media needs two processes — which is what two member groups already are.

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

## Proposal

### API changes

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

### Render and accounting changes (the silently-wrong branches)

| Site | Today | After |
|---|---|---|
| `memberRequests`, `pkg/worker/kvcache/mooncake/member_workload.go:737-742` | Always charges `capacityPerMember + localBufferSize` to host `ResourceMemory` | DRAM: unchanged. VRAM: host memory for `localBufferSize` only, and no extended resource at all |
| `renderMemberEnv`, `member_workload.go:540-545` | Renders `capacityPerMember` as `MOONCAKE_GLOBAL_SEGMENT_SIZE` | Unchanged in spelling; the comment is corrected — in a VRAM build the same size feeds `cudaMalloc`, verified upstream: the `#ifdef` switches allocation only, the `total_size` plumbing is shared (`client_buffer_allocation.cpp:57-99`) |
| `MemberProtocol`, `member_workload.go:319-331` | One protocol per backend | `MemberProtocolForGroup(kvcb, group)`: group's `transport.protocol` if set, else the backend's; `MemberProtocol` stays for backend-wide callers |
| `applyMemberFabric`, `member_workload.go:757-827` | Fabric privileges keyed on the backend protocol | Keyed on the group's effective protocol, per DaemonSet. Never keyed on the medium, so a VRAM group is not exempt from it |
| Engine wiring, `model_deployment_binding.go:365`, `pod_kv_cache_resolve.go:111` | `Protocol: MemberProtocol(backend)` | `Protocol: MemberProtocolForGroup(backend, matchedGroup)` |

### Admission changes

- `checkTransport` (`pkg/worker/kvcache/inject/engine.go:148-162`) becomes pool-aware: for each group in
  the matched pool's backend, compute the effective protocol; refuse with `TransportUnsupported` when the
  engine's constraint (the `engineTransportConstraint` table) is satisfiable by no group. When several
  groups satisfy, the engine is handed the protocol of the group it was matched to.
- Webhook on `KVCacheBackend`: a `hostPaths[]` mount path is refused when it duplicates another entry's,
  when it is the device tree's (`/dev/infiniband`, unconditionally), or when it is this group's current
  `localDisk.path`. No rule on `image` (see Non-Goals).

### Image build (PR-A)

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

### User Stories

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

### Implementation Plan

**Dependency graph and PR split.** PR-A (T1) lands first and is self-contained: workflow + Dockerfile,
no Go. PR-B (T2-T5) is the API change; T2 is its foundation (schema + `make generate`), T3 and T4 depend
only on T2, T5 depends on the behavior settling. Each task leaves the tree building; a
`make generate`-clean + `make lint` checkpoint sits after T2, T3, and T4. The measured item M1 is an
acceptance gate on shipping and runs on the NVIDIA validation host against the PR-A CUDA image; M2 is
covered by T4's envtest.

- [ ] **T1 (PR-A: image) — build-target input + variant targets in the one Dockerfile.**
  Add the optional `target` input to `.github/workflows/base-image.yml` (plumbed to `_image.yml`,
  including build-cache-ref isolation per target); add the vendor-generic `cuda` / `cann` / `rocm`
  builder/runtime stage pairs to `pack/mirrored-mooncake/Dockerfile` with dispatch-overridable base ARGs
  (defaults per `gpustack/runner`; CUDA flags per the Mooncake repository's own build); rewrite the
  `Dockerfile:207-209` comment. **Accept:** a dispatch with `target` + `tag` produces
  `0.3.13.post1-cuda13.0`; a dispatch with no `target` reproduces today's `-cpu` image unchanged; the
  `cuda` and `rocm` variant images build on both `0.3.13.post1` and `0.3.10.post2`, the `cann` variant
  on `0.3.13.post1` and `0.3.11.post1`. **Verify:** workflow dispatch on the
  PR-A branch for each variant × version line; smoke-load the store wheel from each produced image on the
  matching vendor validation host.
- [ ] **T2 (PR-B: API + webhook) — widen the enum, add the fields.**
  Widen `members[].medium` to `["DRAM", "VRAM"]` (`api/worker/v1alpha1/kv_cache_backend.go:469`); add
  `members[].transport.protocol` (optional, same enum as the backend field) and
  `members[].securityContext`, `members[].hostPaths[]` and `members[].runtimeClassName`. Update the guard test
  `TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns` (`api/worker/v1alpha1/kv_cache_backend_test.go:78-96`)
  — its failure message says "do not widen without a renderer"; this spec lands the renderer, so the test
  now asserts exactly `["DRAM", "VRAM"]`. Webhook: a colliding `hostPaths[]` mount path is refused; the
  medium-immutability rule becomes live. Run `make generate`. **Accept:** the tree builds; the guard test
  passes with the widened enum; a group whose mount path collides is refused with a
  `field.Error`. **Verify:** `go test ./api/worker/... ./pkg/worker/webhooks/worker/... && make generate && make lint`.
- [ ] **T3 (PR-B: render) — per-group protocol and per-medium accounting.**
  Add `MemberProtocolForGroup` with inheritance; rekey `applyMemberFabric` on the group's effective
  protocol; split `memberRequests` by medium per the table above; correct the `MOONCAKE_GLOBAL_SEGMENT_SIZE`
  comment. **Accept:** table tests render a two-group backend into two DaemonSets covering success
  criterion 3, and fabric privileges follow each group's own protocol. **Verify:**
  `go test ./pkg/worker/kvcache/... ./pkg/worker/controllers/worker/... && make lint`.
- [ ] **T4 (PR-B: admission + engine wiring) — the matched group's protocol, or a refusal.**
  Make `checkTransport` pool-aware per the design above; pass the matched group into
  `MemberProtocolForGroup` at both engine-wiring call sites. **Accept (measured item M2):** an envtest
  binds an engine with a fabric constraint to a pool whose groups serve only an incompatible protocol and
  asserts refusal with `TransportUnsupported`; a second case asserts the engine is handed the matched
  group's protocol when one group satisfies. No hardware needed. **Verify:**
  `go test ./pkg/worker/kvcache/inject/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... && make lint`.
- [ ] **T5 (PR-B: docs) — the reference page and the worked pair.**
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

### Test plan — Gates (ship gates for this spec)

Gates are causally part of #446: if one is red, this spec does not flip to Shipped.

- **M1 — VRAM segment with the local disk tier.** Write keys until offload engages, then read them back
  and assert byte equality, on the NVIDIA validation host against the PR-A CUDA variant image. Upstream
  covers this combination with no test (verified: no test references VRAM segments at all), and the
  orthogonality evidence is source-level only. Until M1 is measured, the docs carry the banner and this
  spec does not flip to Shipped on its account. Equivalent coverage on Ascend (`cann`) and AMD (`rocm`)
  hardware is an open question below, not part of this gate.
- **M2 — engine refused on a transport no group serves.** Covered by T4's envtest; no hardware needed.
  M2 must be green before PR-B merges.

### Test plan — Riders (same cluster trip; NOT gates for this spec)

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

### Cluster-trip sequencing (R1 + R2 + M1 in one trip)

R1 and R2 CAN run in one trip on one P/D deployment: R1 needs the startup window and R2 needs steady
state, and the window comes first by construction. Sequence: (1) roll out the P/D deployment bound to
the trip's backend and capture R1's two curves during the decode replica's startup window; (2) once
serving, run R2's steady-state request with before/after counters, block counts, and the
no-recompute check; (3) bring up (or have already up) a single-role deployment on the same cluster for
R2's TTFT baseline; (4) M1 runs on the NVIDIA validation host against the PR-A CUDA variant image, in
parallel with (1)-(3) since it needs its own hardware. One cluster, one trip; only the TTFT baseline
adds a second deployment.

## Alternatives

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

- A member's segment is on ONE device, measured upstream: one `cudaMalloc` per split, splits sized by
  the transport's registration limit, no `cudaSetDevice` anywhere on the path. So a node with eight
  accelerators contributes a slice of one of them, and the seven others are reachable only by running
  more members on that node — the several-members-per-node shape `capacityPerMember` already names as
  decided and not done. Whether to build it is open, and what would decide it is a deployment wanting
  more device memory in the pool than one accelerator per node can give.
- EFA plus VRAM (GPUDirect over libfabric) is untested upstream and out of scope here; the first VRAM
  deployments are expected on RDMA or TCP.
- Whether the Ascend (`cann`) and AMD (`rocm`) variants get the same measured VRAM-plus-local-disk-tier
  coverage M1 gives NVIDIA is open: no validation host for either vendor is guaranteed. Their images
  build and smoke, but the write-offload-read-back measurement stays NVIDIA-only until hardware exists.
- Whether the operator's `kv-cache-backend-image` default ever flips from `-cpu` to a vendor variant is
  left to a later decision; per-group and per-backend overrides make it unnecessary for this spec.
