# Spec: a host fabric grants its device by protocol

Status: Shipped
Type: Feature

## Summary

A `KVCacheBackend` member on `RDMA` or `EFA` needs one thing from its node that a mount cannot give
it: a device-plugin allocation, which is what adds the device cgroup rule that lets the container
`open()` the adapter. Until now the resource to ask for was DECLARED, in
`spec.transport.deviceResourceName`, and an `RDMA` group that declared nothing asked for nothing —
it mounted `/dev/infiniband`, was refused the open, and the store installed TCP while the object
still read `RDMA`.

This removes the field. Each of the two host fabrics has exactly one resource name that is right on
every cluster running this operator, so the protocol names the device:

| `spec.transport.protocol` | the member requests | mounts `/dev/infiniband` |
|---|---|---|
| `RDMA` | `device.gpustack.ai/rdma.shared` | no |
| `EFA` | `vpc.amazonaws.com/efa` | yes |
| everything else | nothing | no |

The RDMA name became derivable only recently: this operator's own Device Manager now advertises one
RDMA key per allocation mode, which is what makes a constant correct across clusters rather than a
guess about what an administrator installed.

It removes an API field and changes what an unset one used to mean, so it is a breaking change to an
unreleased API. No CRD is added and no field is added.

## Motivation

### This answers an open question rather than reopening a decision

`specs/2026-09-20-rdma-extended-resource.md` left deriving this name as its third Open Question. It
named the reason the derivation had not been possible before — no name could be hard-coded — and the
one question it would raise: what a member should do on a node advertising the key with a count of
zero. F7 answers that one, and the answer is that it is not a separate case.

The earlier spec also recorded, correctly at the time, that an RDMA member declaring the name
rendered the request already, so what the keys added was a derivable default rather than a new
capability. That remains true; what changed is that the default now exists and the declaration does
not.

### The declared name was a hole shaped like a default

`deviceResourceName` was optional, and its unset value was the pre-field behaviour rather than a
safe default. The field's own documentation said so. What it could not say is that the unset value
was also the one almost every object would have, because a name that is right on one cluster was
wrong on the next, and nothing could suggest one.

The failure that produces is the worst available shape. The member starts. Its Pod is healthy. The
store logs no error — it reports discovering zero HCAs and installs a TCP transport — and the object
continues to read `RDMA`. Nothing in the cluster says the fabric is not in use.

### The mount was the older half of the same mistake

A `hostPath` of `/dev/infiniband` carries the device node into the container's mount namespace and
grants nothing. Measured: `crw-rw-rw-` device nodes, a process running as uid 0, and `open()`
returning `EPERM` from the device cgroup, which has no rule for a device the Pod never requested.

Once the grant is what carries the device, the mount is not merely redundant on the RDMA path — it
is harmful. The allocation injects the verbs character device of each endpoint it grants, one at a
time and deliberately never the whole directory, so mounting the tree beside it hands a container
granted one endpoint the device nodes of every adapter it was **not** granted: visible, unopenable,
and enumerated by the store on its way to skipping them.

### The reading that settles the RDMA half

On a node carrying two whole-function RDMA endpoints, a container that requested one of them and
mounted nothing received exactly one verbs character device out of the two on the host, was told
which one through `NCCL_IB_HCA`, and opened it `O_RDWR` while unprivileged. So the grant alone is
sufficient on this path, and the mount can go.

### Why EFA keeps its mount

A controlled probe on an EFA host has since taken the matching reading, and it points the same way:
with the resource request kept and the mount removed, the transport still installed, because that
vendor's plugin injects the verbs node itself. The same probe removed the REQUEST instead and the
transport failed loudly — `uverbs0 is not accessible`, zero devices found — with the device node
present and world-readable. That is the asymmetry this whole rendering rests on, now measured on
both fabrics rather than one: **the mount makes a device visible, the request makes it openable.**

Two of the three reasons that held have since been measured away, on EFA hosts:

1. ~~It drove a hand-written Pod, not this renderer's output.~~ **Answered.** A member THIS RENDERER
   produced was patched to drop the mount, rebuilt, and read from its own transport line as
   `provider: efa` — not from its Pod phase.
2. The redundancy is a property of **that plugin**, not of the protocol. An allocator that injects
   nothing turns the mount back into the only way the device node reaches the container, and this
   operator does not own AWS's. **This one still holds.**
3. ~~Nothing has moved bytes without it.~~ **Answered.** 25 GB across nodes with no mount, the byte
   count agreeing between the application, the initiator adapter's counters and the target's.

So the honest position is no longer "unmeasured". **Dropping the mount now needs a decision rather
than a reading**: whether an EFA member may only ever run against an allocator that injects the
device node. That decision belongs to a change that states it, not to this one, which is why F3
keeps the mount unchanged here.

## Proposal

### Core Features & Acceptance Criteria

**F1 — The protocol selects the fabric resource family.**
`fabricDeviceResource` maps a resolved protocol and a member interface count to the extended resource
that protocol's members request. The protocol selects the family; the count selects the quantity and,
for RDMA, shared or exclusive allocation. It **names both protocols explicitly and falls through to neither**: the pair it and
`memberProtocolIsHostFabric` agree on is correct today, and the file's own comments expect a third
host fabric eventually, at which point a fallthrough would hand that fabric's members a seat on an
RDMA adapter — silently, and with the wrong device. Naming them returns the empty resource instead,
and the renderer skips the request rather than writing an empty key no node can satisfy. Acceptance:
a unit case per admitted protocol AND a case for one the function does not name, since the grant
matrix drives the renderer and therefore cannot reach that branch at all. The RDMA name is DERIVED
from
`nodefeature.GetRDMAResourceName` rather than written a second time: the Device Manager serves
whatever that function returns, so a literal here would stop matching the day those keys moved and
nothing would report it. Acceptance: an `RDMA` group's default count requests
`device.gpustack.ai/rdma.shared`, an RDMA count above one requests the exclusive key, and an `EFA`
group requests `vpc.amazonaws.com/efa`, with the count declared on its member.

**F2 — The count selects RDMA allocation mode.**
An endpoint carries one exclusive token and many shared ones. A count of one uses the shared key,
while a larger count uses the exclusive key because repeated shared tokens can resolve to one endpoint
without an error. Acceptance: both keys are asserted as literals for their respective counts.

**F3 — Only EFA mounts the device tree.**
Acceptance: an `RDMA` group's pod template carries no volume whose `hostPath.path` is
`/dev/infiniband` and no volumeMount on that path; an `EFA` group's carries both, its volume still
typed `Directory`. Both halves are asserted in every row, never one of them: a volume nothing mounts
and a mount referencing no volume are different defects, and either alone leaves half the old
rendering in place.

**F4 — The rest of the host-fabric base is unchanged.**
`hostNetwork`, `DNSClusterFirstWithHostNet`, and `IPC_LOCK` + `SYS_RESOURCE` — never `privileged`.
Acceptance: asserted on both fabrics, so a later change that drops them reports as these cases
failing rather than as them passing on a member that lost what it still needs.

**F5 — The medium and the transport are independent axes.**
Nothing in this rendering reads `members[].medium`. Acceptance: a `DRAM` row and a `VRAM` row on the
same protocol are granted identically, and a `VRAM` group is still charged no accelerator. This was
already true and is now asserted, because the absence of a coupling is invisible in code that never
mentions it and a reader can infer one from a suite that only ever pairs `VRAM` with a fabric.

**F6 — The device follows the GROUP's effective protocol.**
`members[].transport.protocol` overrides the backend's for that group. Acceptance: a group
overriding `TCP` with `RDMA` is granted the fabric, and a group overriding `RDMA` with `TCP` is
granted nothing.

**F7 — A node that cannot serve the fabric leaves the member Pending.**
Requesting a resource is also what keeps a member off a node advertising none, and that refusal
replaces the silent TCP fallback. The escape hatch is the protocol, not an empty field: an operator
who wants the member to run anyway says `TCP`, in the object, where the next reader sees it.

Acceptance: stated in the field's own documentation and on the operations page, since no unit test
can assert a scheduler outcome — **and observed on hardware**. On an EFA host a second backend's
member Pods stopped at `Pending` with `0/3 nodes are available: 1 Insufficient
vpc.amazonaws.com/efa, 2 node(s) didn't satisfy plugin(s) [NodeAffinity]`, the incumbent member
holding the node's one advertised unit. That is this refusal, in the shape F7 describes, rather than
an inference from the scheduler's documentation.

⚠️ **What that reading does NOT establish.** It was taken on a single-interface instance shape, so
"one member per node" there is the arithmetic of `1 ÷ 1` and not a property of EFA — a denser shape
advertises more and admits more. What carries across shapes is the FAILURE MODE (a resource the node
cannot serve leaves the Pod Pending and visible), not the count. The RDMA keys make the distinction
sharper still, since the shared key carries 64 tokens per endpoint.

**A node advertising the key at ZERO is the same case, deliberately.** It is not a separate rule and
gets no special handling: a member asking for one endpoint is unschedulable there exactly as it is
on a node advertising nothing, and the operator's answer is the same edit. The distinction matters
because a zero count is the COMMON shape rather than a corner — a node whose only fabric is one this
operator's detector does not record as an RDMA endpoint publishes all three keys at zero, so the
keys are present and empty. Treating that as a third state would mean choosing between leaving the
member Pending and silently serving TCP, which is the choice this whole change exists to stop
making.

### Notes / Constraints / Caveats

- **The Device Manager is deployed per accelerator vendor.** A node carrying RDMA adapters and no
  accelerator runs none of them and therefore advertises no RDMA key. A `DRAM` member group needs no
  accelerator and is exactly the kind that would select such a node, where it will now stay Pending.
  This is recorded as an Open Question rather than softened here.
- **EFA is AWS's own protocol and stays that way.** This operator's detector does not record EFA
  adapters as RDMA endpoints — on such a host the ledger carries the interface with no `rdma` field
  and the three RDMA keys read zero — so an EFA host is served by AWS's plugin and its own name, and
  no attempt is made to unify the two.
- **Allocation mode changes the observed contention shape.** Measured on a single-node cluster with
  two whole-function RDMA endpoints: two host-network members each requesting one shared RDMA
  resource and bound to the same host REST port were admitted; the second failed to bind with
  `address already in use` and stopped. Under the exclusive key, the same pair exhausted the
  endpoints first, so the second member stayed Pending and never reached the port.
- **Admission keeps its unconditional device-tree rule.** `hostPaths[].mountPath` and
  `localDisks[].path` are still refused when they overlap `/dev/infiniband`, under every protocol
  including the ones that no longer mount it. The reason is unchanged and still holds: the protocol
  is editable, so a path that merely does not collide today would start colliding the moment someone
  switched the backend to EFA.
- **A side effect worth naming, because it looks incidental.** `hostNetwork` was already granted
  unconditionally on both fabrics, BEFORE the early return that an RDMA group with no declared name
  took — so such a member took the host's network and requested nothing, and could land on any node
  the selector reached, fabric or not. Deriving the request removes that combination: every
  host-fabric member now carries a fabric request, so the scheduler keeps it off nodes that advertise none.
  ⚠️ This is NOT the same as the mutual exclusion EFA has. There the key is advertised once per
  node, so a second member is refused; the RDMA shared key carries 64 tokens per endpoint, so a node
  admits many. Anyone reading the two as equivalent will over-read this.

### Boundaries

- **Engine Pods are out of scope.** Nothing here renders fabric access into the vLLM or SGLang Pods
  that consume a pool; that gap is its own decision about granting `hostNetwork` to tenant-adjacent
  workloads, and it is untouched.
- **The detector is out of scope.** Which interfaces are recorded as RDMA endpoints, and whether EFA
  should ever be among them, is a separate component and a separate answer.
- **No scheduling change.** Nothing here reads a fabric domain label or places two Pods relative to
  one another.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| A backend that ran before now leaves its member Pending on nodes without the key | Intended, and stated in the field documentation and the operations page. The escape hatch is `protocol: TCP`, which is one edit and is visible on the object |
| The derived RDMA key stops matching what the Device Manager advertises | The renderer derives it from the same function the plugin serves, and the tests assert a LITERAL — so a rename breaks the test rather than the cluster |
| EFA's mount turns out to be unnecessary and is carried forever | Recorded as an Open Question with the exact reading that would remove it |

## Design Details

### Commands

```bash
go test ./pkg/worker/kvcache/mooncake/... ./pkg/worker/controllers/worker/... \
        ./pkg/worker/webhooks/worker/... ./api/worker/v1alpha1/...
make generate   # the Protocol comment is copied into generated code; the removed field leaves it
make lint
make lint docs < /dev/null
```

### Project Structure

```text
api/worker/v1alpha1/
  kv_cache_backend.go             # DeviceResourceName removed; Protocol's comment carries the rule
  kv_cache_backend_test.go        # the schema-pattern test over plugin names, removed with it
pkg/worker/kvcache/mooncake/
  member_workload.go              # fabricDeviceResource; the mount narrowed to EFA; two protocol constants
  member_workload_test.go         # the grant matrix, and the two context tests it does not replace
pkg/worker/webhooks/worker/
  kv_cache_backend.go             # validateKVCacheBackendTransport removed; it has nothing left to judge
  kv_cache_backend_test.go
pkg/worker/controllers/worker/
  kv_cache_backend_test.go        # switching a live backend between the two fabrics and back
docs/kv-cache/
  backend.md                      # what each protocol grants, and why the failure is loud
  local-disk-tier.md              # one sentence: which transport mounts the tree
.agents/skills/gpustack-operator-e2e/
  cases/case-85.sh                # the rendering read from a live cluster
```

### Code Style

```go
// fabricDeviceResource selects the resource family from the host-fabric protocol and the RDMA mode
// from the member's interface count.
func fabricDeviceResource(protocol string, interfaceCount int32) core.ResourceName {
	if protocol == memberProtocolEFA {
		return efaDeviceResource
	}

	if interfaceCount > 1 {
		return nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeExclusive)
	}

	return nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeShared)
}
```

Conventions this follows: the reason for a prohibition sits beside it in the comment; the two
resolved protocol spellings are constants because three places have to agree on the pair; no spec
identifiers appear in Go comments; file names are snake_case.

### Implementation Plan

- [x] **T1 · The device follows the protocol, and only EFA mounts the tree**
      Blocked by: None
      Owns: `pkg/worker/kvcache/mooncake/member_workload.go`,
      `pkg/worker/kvcache/mooncake/member_workload_test.go`
      Gate: review
      Acceptance: F1–F6. `applyMemberFabric` loses its third parameter; the mount and its volume are
      rendered on the EFA path alone; `RDMADevicePath`'s comment states why the other half stopped.
      Verify: `go test ./pkg/worker/kvcache/mooncake/...`, and a mutation restoring the mount on the
      RDMA path turns the matrix row red on the assertion rather than on a compile error.

- [x] **T2 · Remove the field and regenerate**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`,
      `api/worker/v1alpha1/kv_cache_backend_test.go`, `api/worker/v1alpha1/generated.pb.go`,
      `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/zz_generated.crds.go`,
      `api/worker/zz_generated.openapi.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendtransport.go`
      Gate: review
      Acceptance: `DeviceResourceName` appears nowhere outside this spec's own prose; `protocol`
      keeps field number 1 and nothing is renumbered; the generated copies match the source.
      Verify: `make generate` from a checkout whose path ends in the module import path, then
      `git status` names only the generated files above.

- [x] **T3 · Remove the admission rule that judged the declared name**
      Blocked by: T2
      Owns: `pkg/worker/webhooks/worker/kv_cache_backend.go`,
      `pkg/worker/webhooks/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: `validateKVCacheBackendTransport` and its call site are gone, together with the
      exemption that kept it from stranding an undeletable object; the device-tree overlap rules are
      untouched and still unconditional.
      Verify: `go test ./pkg/worker/webhooks/worker/...`

- [x] **T4 · Pin the switch between the two fabrics on a live object**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: switching `EFA` to `RDMA` takes the device tree off and swaps the request in the
      same render; switching to `TCP` takes both off along with the host network.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T5 · Repair the statements this work makes false**
      Blocked by: T1, T2
      Owns: `docs/kv-cache/backend.md`, `docs/kv-cache/local-disk-tier.md`
      Gate: review
      Acceptance: no sentence in the documentation says a name is declared, that both fabrics mount
      the tree, or that an unset field is the way to ask for nothing; the loud-failure paragraph and
      the independence of medium and transport are both stated.
      Verify: `make lint docs < /dev/null`

- [x] **T6 · The rendering becomes a cluster reading**
      Blocked by: T1
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-85.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/run-rdma-block.sh`,
      `.agents/skills/gpustack-operator-e2e/SKILL.md`,
      `.agents/skills/gpustack-operator-e2e/references/rdma-host-shapes.md`
      Gate: review
      Acceptance: a case gated on a whole-function endpoint applies an `RDMA` backend and reads the
      rendered DaemonSet for the request and the absence of the mount. It pulls no store image and
      waits for no member Pod, so it needs no working fabric and no arch-matched store build.
      Verify: `cases/run-rdma-block.sh --report-only <NS>` lists it with its requirements.

T2 and T3 are ordered by the compiler rather than by design — the webhook reads the field T2 deletes.
T5 and T6 depend on T1 for truth rather than for order; either landing alone leaves the repository
asserting something the code does not do.

### Test Plan

The unit surface is the grant matrix in `pkg/worker/kvcache/mooncake`: one row per protocol, each
asserting the requested resource AND whether the device tree is mounted. The expected names are
LITERALS. Deriving them from the function the renderer calls would make the rows pass by
construction, while what has to hold is that the request matches what the Device Manager advertises
— two packages, which one rename can separate while leaving every derived assertion green and every
running member Pending.

Rows cover `DRAM` and `VRAM` on one protocol (F5), and a member group overriding the backend's
protocol in both directions (F6). The two older context tests are kept rather than folded in: they
assert the whole rendering of one fabric each, which is the thing a matrix row is too narrow to
notice losing.

Reconciler coverage is the switch between fabrics on a live object, which is where a rendering that
is correct when created and never taken back off would otherwise survive.

Cluster coverage is CASE 85, and it is deliberately only the rendering. The other end of the chain
is CASE 81 — a granted endpoint opening inside an unprivileged container that mounts nothing — and
neither covers the chain alone: 85 proves the operator asks, 81 proves the ask is what admits the
open. Splitting them is what keeps either from needing the other's preconditions.

**What is NOT covered**: no test starts a store and reads which transport it installed. That needs a
member Pod on a working fabric with an arch-matched store image, and it is the reading that would
also settle the EFA question below.

## Alternatives

### A boolean rather than a derived name

`enableRdma` is the smaller API. It cannot express EFA, whose resource name is AWS's rather than
this operator's, so the field would have to be read alongside the protocol anyway — at which point
the protocol may as well carry it, which is what this does.

### Keep the field with a per-protocol fallback

Defaulting `deviceResourceName` per protocol preserves a third-party plugin's name as an escape
hatch. Rejected because the escape hatch it preserves is the wrong one: an administrator whose
cluster cannot serve the fabric needs to say so in `protocol`, and a field that lets them name some
other plugin's resource invites the member to be granted something this operator cannot reason
about. The protocol is the contract; the device follows from it.

### Teach the detector to record EFA as an RDMA endpoint

That would unify the two names and delete the EFA special case. Rejected as a deliberate scope
decision: EFA is AWS's own fabric with its own plugin and its own libfabric provider, and treating
it as one more RDMA endpoint would make this operator responsible for a device tree it does not
allocate.

## Open Questions

**Whether EFA can drop its mount too — now a decision, not a measurement.** Both readings that were
missing have been taken on EFA hosts: a member this renderer produced ran without the mount, and
25 GB crossed nodes without it, with the byte count agreeing across the application and both
adapters' counters. What remains is not evidence but a contract question: **may an EFA member run
only against an allocator that injects the verbs node itself?** AWS's does; this project does not own
it, and an allocator that injects nothing turns the mount back into the only path. Answer that and
F3 can change in a follow-up.

**Answered: yes, and EFA no longer mounts the tree.** The mount broke a partial grant: a member
granted fewer EFA devices than its node has saw the rest through it, got `EPERM` opening one, and
libfabric's EFA provider abandons its whole device list on the first `EPERM`. An allocator that
injects nothing is not rescued by the mount either, since the request is what makes a node openable.

⚠️ One related fact for whoever writes it, measured in the same round and easy to mistake for a
reason to keep `hostNetwork`: **RDMA's sysfs is namespaced**, so a Pod without `hostNetwork` cannot
see `/sys/class/infiniband` at all. That does NOT affect the data plane — 25 GB crossed nodes with
both ends in the pod netns — but it does mean any future design that reads adapter counters or
enumerates devices FROM INSIDE the member depends on `hostNetwork` for that reason alone.

**Should the member REST port be derived per backend rather than per group?** The measured contention
shape above exposes this remaining decision. Choosing the exclusive key does NOT answer it: that only
moves the failure from a port collision to Pending and leaves the shared path exactly as it is.

**Whether the object should say anything when its members cannot be placed.** F7 calls the Pending
the "visible" failure, and it is — on the Pod. The `KVCacheBackend` itself reports nothing: no
condition and no event names the resource that could not be satisfied, so an operator who looks at
the object they wrote sees a backend that is simply not ready. The reading that makes this concrete
already exists — a member stopped with `Insufficient vpc.amazonaws.com/efa` while its backend said
nothing about why.

This is a status-surface change rather than a rendering one, which is why it is not here. It also
gets sharper with this change, since deriving the name removes the field an operator would otherwise
have re-read to diagnose it.

**What a cluster running the Device Manager with `--no-shared` should do.** This one is NOT covered
by F7's reasoning and is the sharpest gap this change leaves.

`pkg/devicemanager/allocator/rdma.New` appends the shared mode only when `!opts.NoShared`, and
`--no-shared` is an operator-settable flag. On such a cluster the exclusive key is still served and
the hardware is still there, but `device.gpustack.ai/rdma.shared` is never advertised — so every
RDMA member stays Pending forever.

F7 says that refusal is the wanted one because "a cluster that cannot serve the fabric is one whose
backend should say TCP". **That argument does not hold here.** The fabric IS serviceable; only one
allocation mode was switched off. Telling such an operator to fall back to TCP gives up a working
RDMA fabric, which is the opposite of what F7 is for — and the declared name used to be their way
out, which this change removed.

Three shapes, none of them free, and the choice is a maintainer's:

1. **The RDMA family ignores `--no-shared`.** That flag is about sharing an ACCELERATOR; an RDMA
   endpoint's 64 shared tokens are a different thing wearing the same word. Cheapest, and it is a
   change to the Device Manager rather than to this renderer.
2. **Render the exclusive key when shared is unavailable.** The renderer has no cluster view — it
   builds a Pod template from an object — so it cannot see which modes a node serves. This would
   need the choice to move somewhere that can, which is a larger design.
3. **Document it and leave the failure loud.** Cheapest to ship and the worst to meet: the Pod says
   `Insufficient device.gpustack.ai/rdma.shared` while `device.gpustack.ai/rdma` sits unused on the
   same node.

Until it is decided, a cluster on `--no-shared` cannot run an RDMA backend at all.

**Whether a node with a fabric but no Device Manager should be reachable at all.** The Device
Manager is deployed per accelerator vendor, so a node carrying RDMA adapters and no accelerator runs
none of them and advertises no RDMA key. A `DRAM` member group — which needs no accelerator — is
exactly the kind that would select such a node, and on `RDMA` it will now stay Pending there. The
answer may be to widen where the Device Manager runs rather than to soften F7, and that is a change
to a different component.
