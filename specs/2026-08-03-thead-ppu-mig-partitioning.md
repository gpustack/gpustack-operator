# Spec: T-Head PPU Hardware Partitioning, Mirroring the NVIDIA MIG Implementation

Status: Shipped
Type: Feature

## Summary
T-Head PPU accelerators support hardware partitioning under the same name and the same shape as
NVIDIA MIG: a card is carved into GPU Instances (GI), each GI into Compute Instances (CI), and an
application runs on a CI. The vendor's management library `hgml` exposes a MIG API that is a
function-for-function analogue of NVML's, and `ppu-smi mig` mirrors `nvidia-smi mig` flag for flag.
This spec brings the `thead` manufacturer to behavioural parity with the existing NVIDIA MIG support:
the detector reports each MIG-enabled card's partition profiles, the `thead` manufacturer advertises
a `.partitioned` resource family, and the device-manager carves, hands out, reclaims and re-names
partitions through the same platform-independent core the NVIDIA path already uses.

Parity is the deliverable, not a T-Head-specific design. Four vendor differences make a literal copy
wrong, and this spec pins how each is resolved: partition profile ids must be discovered rather than
computed, container isolation is by device node rather than by a container-runtime hook, MIG-mode
enablement is an administrator prerequisite the operator never performs, and profile names need
normalizing before they can become resource-name segments.

Parity is of contract, not of every line. A design cross-check found four places where the NVIDIA path
is deliberately permissive — it turns driver-enumeration errors into partial success, adopts a leftover
partition on geometry alone, destroys one by id without re-reading its identity under the lock, and
aggregates same-named profiles without checking they agree. Copying those into a second vendor would
duplicate known-permissive behaviour into new code, so the new code hardens them instead; each
divergence is named in Notes. A fifth, found while building the mirror, is different in kind: a corrupt
ownership marker hides the partition it records, so another Pod can adopt one that is already owned.
That is a defect rather than a permissiveness, and it is fixed on both vendors rather than only avoided
on the new one (F12). Everything else is the NVIDIA implementation with the vendor library swapped.

Parity runs the other way in three places, all prefactors on the vendor being mirrored. First, both
detectors need a profile's memory-slice span, and the NVIDIA one recovers it by dividing card memory by a
hardcoded slice count even though the driver's placement set already states it; rather than copy that
into a second vendor, the shared derivation is corrected to read the driver's own answer, for both (F10).
Second, the NVIDIA reclaim loop lives inside its 860-line partition file while its tests already live in
a `mig_reclaim_test.go`; it is moved to the matching `mig_reclaim.go` so both vendors carry one layout
rather than two (F11). Third, a corrupt ownership marker leaves the partition it owns invisible to the
adoption path, so both vendors fail closed on the one card that marker names (F12).

The facts that can only be established on real hardware are deliberately not guessed. They are
collected into a verification feature (F9) that runs on a T-Head PPU host and feeds its findings back
into a revision of this spec.

## Motivation

### Goals

- The `thead` manufacturer reaches parity with `nvidia` on hardware partitioning at the level of
  contract: the same capability reporting, the same resource keys, the same admission path, the same
  on-demand carve, the same reclaim guarantees and the same sidecar visibility semantics.
- Where the NVIDIA path is knowingly permissive, the new code is strict rather than faithful: an
  enumeration that cannot prove completeness errors instead of returning partial state, an adoption
  compares raw profile identity rather than geometry alone, a destroy re-verifies identity under the
  lock, a normalization collision rejects the profile, and a missing device node fails the allocation.
  These are the only intentional behavioural divergences, and each is named in Notes.
- `binding/hgml` is regenerated from the vendor's current header and exposes the whole MIG surface the
  driver seam needs, so the seam compiles against real symbols rather than a subset.
- A T-Head card with MIG mode enabled advertises its partition profiles as requestable resources; a
  card with MIG mode disabled keeps behaving exactly as it does today.
- Every layer the NVIDIA path already made vendor-neutral is reused unchanged, and the diff proves it:
  no new logic in `pkg/device`, `pkg/deviceplugin`, `api/worker`, the worker controllers or the
  webhooks — with one exception, taken deliberately and named here rather than left for a reviewer to
  find. F15's published profile name is a *naming* difference between two vendors, not a behavioural
  one, so it has to be known wherever a profile name crosses between what a user writes and what the
  driver reports: the resource key (one function, so every key-building caller converts at once), the
  status the `InstanceType` publishes, the request the device plugin reads back, and the per-card
  feasibility lookup. Each of those four is a single delegation to `pkg/nodefeature`, which already
  owns every other per-manufacturer fact, so no shared layer learns a vendor's spelling itself. The
  cheaper alternative — converting in the detector, leaving the shared layers untouched — was
  rejected because it would put the published spelling in the `Devices` record and the ownership
  markers, and those are exactly what the driver seam matches a profile by.

**Success criteria**

- On a Kubernetes cluster with a T-Head PPU node whose cards have MIG mode enabled, the node
  advertises `<base>.partitioned`, `<base>.partitioned.units` and one
  `<base>.partitioned.mig-<profile>` key per offered profile, where `<base>` is the manufacturer's
  accelerator resource name.
- An `InstanceType` for that pool reports a non-empty
  `status.acceleratorPartitioned.remainingProfiles`.
- A workload requesting one partition profile is admitted, starts, and sees exactly one partition —
  neither the parent card nor a sibling partition.
- Deleting that workload returns the profile to `remainingProfiles`, and the partition is destroyed;
  a device-manager restart mid-life neither leaks nor double-destroys it.
- `make lint` and `make test` pass; the pure partition core is table-tested on the local platform
  through the build-tag seam, with no accelerator present.

### Non-Goals

- Enabling or disabling MIG mode on a card. That stays an administrator action performed before the
  device-manager starts, exactly as it is for NVIDIA. The operator reads the mode and never writes it.
- Logical (soft) slicing for T-Head. The existing Exclusive/Shared/Visibility behaviour is untouched;
  physical partitioning is added beside it, and the two remain mutually exclusive per card as the
  shared capability contract already requires.
- Supporting the graphics/media profile variants, or CI profiles narrower than the whole GI. As with
  NVIDIA, only profiles whose compute instance spans its GPU instance are offered.
- Multi-partition-per-card allocation to a single container. One partition per allocated card, as
  NVIDIA does.
- Reworking how MaaS workloads acquire devices. Whether a co-resident privileged workload can block a
  MIG-mode change is a data-plane concern tracked outside this repository; it affects when an
  administrator can perform the prerequisite, not what this feature does.
- Any change to `pack/gpustack-operator/**`, or to `hack/**` beyond what F1's generator needs. Physical
  partitioning drives the vendor management library at runtime, so it needs no build stage and no injected
  artifact — the same asymmetry the NVIDIA MIG path already has against the soft-slicing path. Two narrow
  exceptions, both recorded here rather than left for a reviewer to find:
  - **`hack/generate.sh`, two lines.** One teaches the generator to compile a header the vendor wrote, and
    is placed on the generator's own working copy precisely so no vendor file is edited. The second extends
    the existing cleanup line to delete the object files the generation step leaves behind, which no
    `.gitignore` covers — unordered, and taken because this feature is what makes running that generator
    routine.
  - **The two vendor shared libraries under `pack/`.** These arrived as uncommitted working-tree state
    alongside the newer header — they are the inputs this feature was handed, not something it produced —
    and were committed as a preparatory change so the header refresh was not split from the payload it
    ships with. One of them is not the library this feature mirrors; it moved in the same vendor drop.
- Fixing the shared device-plugin layer's Pod-attribution ambiguity. The device-plugin Allocate RPC
  carries no Pod identity, so the shared layer resolves it by searching the node's pending containers;
  it already serializes the section under one mutex, skips containers a prior call reserved, and
  disambiguates by feasibility including the partition profile, but when feasibility rejects every
  candidate it deliberately falls back to the unfiltered set rather than hard-failing a resolvable
  Allocate. That residual is architectural, affects NVIDIA identically, and is out of scope here.
- Hardening the existing NVIDIA implementation beyond F10 and F12. The cross-check identified four
  pre-existing defects there (see Risks); they are recorded, not fixed, and this spec does not open a
  follow-up for them.

## Proposal

A T-Head card whose MIG mode is enabled stops being a whole-card or soft-sliceable device and becomes
a source of hardware partitions, described by the profiles its driver offers. A user requests a
profile by name, the scheduling chain admits it against the pool's live partition ledger, and the
device-manager carves the GI and its CI at allocation time, hands the container the device nodes that
expose exactly that partition, and destroys it when the owning Pod is gone.

### User Stories

#### Story 1
As a platform administrator with T-Head PPU nodes, I want the operator to advertise the partition
profiles a MIG-enabled card offers, so that I can sell half-card instances without over-committing a
whole card.

#### Story 2
As an AI engineer, I want to request a T-Head partition profile the same way I request an NVIDIA MIG
profile, so that I get a hardware-isolated partition without learning a second request model.

#### Story 3
As a platform operator, I want partitions created on demand at admission and reclaimed once the Pod is
gone, with the ownership record surviving a device-manager restart, so that no partition leaks and no
live partition is destroyed under a running workload.

#### Story 4
As a user of an SSH-enabled Instance, I want the sidecar container to see exactly my partition rather
than the parent card, so that a shell in my Instance cannot reach another tenant's partition.

#### Story 5
As a maintainer, I want `binding/hgml` regenerated from the vendor's current header with the full MIG
surface wrapped, so that the driver seam is written against real symbols instead of a partial subset.

#### Story 6
As a platform administrator, I want a T-Head partitioning runbook that states the prerequisites and
the limits plainly, so that I can enable the mode correctly the first time.

### Core Features & Acceptance Criteria

**F1 — Regenerate `binding/hgml` and complete its MIG surface.**
The vendor header in `gen/binding/hgml/` has moved to a newer API version in the working tree while
`binding/hgml/` still carries code generated from the previous one. Regeneration is blocked twice over
before it can even run, and both blocks must be cleared without editing the vendor's own files. The
newer header uses `bool` once and includes nothing, which the binding generator's C parser cannot read;
the generator already transforms a *working copy* of the header, so the missing type declaration is
prepended there. Two new mask macros normalize to names that are not valid Go identifiers, and are
dropped by a rule in the generator config. Neither the C99 generator the other `bool`-using vendors run
through nor a hand-edit of the vendor header is used, for the reasons recorded in Alternatives. Then
extend the hand-written ergonomic layer with the MIG operations the driver seam needs but which only
exist today as raw generated wrappers: reading a card's GPU-instance profile info, its possible placements and its
remaining capacity; enumerating live GPU instances and a GPU instance's compute instances; reading a
compute-instance profile; and destroying a GPU instance or a compute instance. The generated raw
wrappers for all of these already exist, so this is wrapper work, not generator work.
Fix one defect the cross-check found in that layer while there: `Device.GetGpuInstance` inverts its
third symbol-lookup guard, testing for the symbol being *present* and returning
`ERROR_FUNCTION_NOT_FOUND` in that case, so the call can never succeed against a library that has the
symbol. Its own first two guards use the correct polarity, and the other `== nil` tests in the file are
legitimate versioned-fallback branches, so the fix is that one guard, not a sweep.
*Acceptance:* `make generate hgml` produces no uncommitted drift afterwards; the hand-written layer
exposes the full set the seam calls; the inverted guard is corrected and the versioned-fallback
branches are left alone; the removal of the vendor families the newer header drops breaks no existing
detector call; `make lint` and `make test` pass. The static overlay files that replace generated
helpers survive regeneration untouched.

**F2 — Detector reports each MIG-enabled card's partition profiles.**
Mirror the NVIDIA detector: per card, read the current MIG mode and set exactly one of the physical or
logical slicing capability, never both. Add a pure profile-derivation function, hardware-free through
an injected placement lookup, that turns the driver's probed profile records into the operator's
profile type — name, memory, compute slices, memory slices, per-card instance count, and the full
set of legal empty-card placements cached per profile id. Finish detection with the shared
group-aggregation step the T-Head detector does not call today.
*Acceptance:* a table-driven test over the vendor's documented profile set for the current product
yields the expected profiles, and a MIG-disabled card behaves exactly as it does today. Note that today
is *no* slicing capability at all, not the logical one — this vendor has no soft-slicing capability to
report, which is why the mutual exclusion holds trivially on the disabled branch. A card whose driver
offers no profile reports no physical capability rather than an empty one.
Every derived profile satisfies `MemorySlices == Placements[0].Length` whenever placements are present
**and carry a positive length**, asserted in the same test, so the field's redundancy is pinned rather
than assumed. The qualifier is not a weakening: a placement of zero length is a driver self-contradiction,
and the derivation deliberately keeps the arithmetic span in that case rather than publishing a span of
zero, which is its own asserted case.

**F3 — Declare the partition capability for `thead`.**
Add the manufacturer's partition kind (`mig` — the vendor's own word for the feature) to the known-
manufacturer registry and to the chart's manufacturer table. This one declaration is what turns on the
`.partitioned` resource family, the per-profile resource keys, the credits transformation and the
family classification across every already-vendor-neutral consumer.
*Acceptance:* the existing test that pins the chart's manufacturer table against the registry passes;
the derived resource keys are well-formed; no consumer outside these two files needs editing.

**F4 — Add a Partitioned device-plugin server and the vendor driver seam.**
Gate a Partitioned server on the partition family being advertised and on the disable flag, exactly as
NVIDIA does; build one driver instance and share it with both the Partitioned server and the always-
present Visibility server. Put the vendor library calls behind a build-tag seam with a non-linux stub,
so the pure core stays testable on a platform where the cgo binding cannot be linked.
*Acceptance:* the server-set test asserts the Partitioned server appears only when the family is
advertised, and that the Visibility server holds the driver. The non-linux stub errors on every method.

**F5 — Carve a partition per allocated card, idempotently.**
Reuse the NVIDIA partition core in shape: a per-card ownership marker under the Pod work directory
written atomically and parsed fail-closed; deterministic lowest-free-slot selection against the
profile's legal placements minus every live instance on the card regardless of profile; adoption of a
reusable instance that has no compute instance yet; per-card locking around create-and-record; and the
three reservation outcomes that make rollback correct — created, adopted, already-owned. Guard against
instance-id reuse by checking the partition's identity, not only its id.
*Acceptance:* the reservation table covers fresh create, kubelet retry, stale self-marker, profile
mismatch, and concurrent allocation on one card; rollback destroys only what this allocation created.

**F6 — Reclaim partitions whose owner is gone.**
Run the shared reclaim loop with the same two debounces the NVIDIA path uses — a consecutive-miss
count sized above the kubelet allocate-retry window, and a bounded retry on a busy destroy that
surfaces an operator-visible condition at the bound. Keep both fail-closed reads, the attribution
self-check against live claims, and the rule that a marker-less orphan is collected only on a fully
drained card. Destroy the compute instance before its GPU instance.
*Acceptance:* the reclaim matrix covers dead-pod destroy after debounce, live-pod retention, bounded
busy retry, mis-attributed marker retention, fail-closed on either read error, id-reuse retention, and
orphan collection only on a drained card.

**F7 — Name the partition, never the card, for a co-allocating container.**
Implement the physical-sliced responder so an SSH sidecar co-allocated to the owner's cards receives
exactly the owner's partitions, resolved from the durable ownership record, proven live, in the same
card order the owner's own response used. Fail closed on anything unprovable.
*Acceptance:* the visibility matrix covers missing, malformed, wrong-card, unknown-profile, dead and
id-reused records, each an error rather than a fallback to the parent card; the compile-time assertion
that the server implements the capability is present.

**F8 — Container isolation by device node.**
Unlike NVIDIA, T-Head has no container-runtime hook, so the allocation response must carry device
specifications rather than only an environment variable. A partitioned container receives the vendor's
shared control nodes, the parent card's node, and the capability nodes of both the GPU instance and its
compute instance. The capability node minor numbers are read from the driver's procfs capability tree under
the same **card ordinal** the card's device node is named after, so one value names both paths and they
cannot diverge — confirmed on hardware, where a live instance on the card at ordinal 14 appeared under
`capabilities/ppu14/`, and where the tree holds `ppu0`…`ppu15` with no `ppu16`.
The response must fail closed on a node it cannot produce. The shared device-spec helper returns nil for
a path that does not exist and the existing whole-card responder appends only what is non-nil, so a
missing node would otherwise yield a *successful* allocation with an incomplete device set. A partition
allocation requires every node in its set; anything missing, not a character device, or not carrying the
expected major/minor fails the allocation and rolls back.
*Acceptance:* the response for a partition names the two capability nodes and the parent card node; the
number used for the device-node path and for the procfs path comes from one value, the card ordinal, so
they cannot diverge; the ordinal is proven rather than trusted, by comparing the actual kernel minor of the
node it names against the minor recorded for that accelerator; and a card with no recorded minor, or one
whose node's minor disagrees with the record, **fails the allocation** rather than being addressed by a
substitute, so an unprovable card fails closed instead of handing out a neighbouring card —
note that "skip the card" would be the wrong reading of failing closed here,
because a response that silently omits a card the kubelet asked for is the very incomplete-device-set
trap this feature exists to prevent; and a test removes each required node in turn and asserts the
allocation fails and rolls back rather than succeeding with fewer nodes.

**F9 — Verify on hardware, then reconcile this spec.**
The features above are written from the vendor's published API and CLI documentation and from the
generated header. A T-Head PPU host with MIG mode enabled must confirm the following before this
feature is considered done, and each finding is folded back into the relevant feature above by a
revision of this spec:
1. The profile records the driver actually returns — id, name, slice count, instance count, memory —
   and whether the name carries the `MIG ` prefix the CLI displays, so F2's normalization is right.
2. The compute-instance profile that spans a whole GPU instance, and how it is identified among the
   card's compute-instance profiles, so F5 never needs a hard-coded profile id. Specifically: whether the
   product offers **more than one** whole-GI candidate. It cannot be told apart before creation — the
   vendor's pre-create record carries no name and no capability bits — so the seam fails closed on
   ambiguity. If hardware shows more than one, the fix is a versioned pre-create probe added to the
   binding, which is a follow-up task rather than a guess made now.
3. The layout and stability of the capability node minor numbers across partition create/destroy and
   across a host reboot, so F8's lookup is sound.
4. Whether a destroy is rejected as busy while the device-manager itself holds a handle on the card,
   and if so what F6's bounded retry must tolerate.
5. The partition identity string the driver reports for a created partition, and whether the vendor's
   visible-devices environment variable is needed in addition to the capability nodes, so F8 injects
   neither too little nor too much.
6. That the driver returns a non-empty placement set for every offered profile — which is what lets F2
   read the memory-slice span from the placements rather than divide, and what the admission check
   already requires before it will admit a partition request. Confirm too that the whole-card profile's
   span equals the constant the NVIDIA fallback divides by, so the fallback is sound for the product
   families where the driver returns nothing. Confirm also that this vendor behaves as its header
   documents on one point the bindings depend on: the placement list is **every legal placement**,
   irrespective of what is currently occupied. Both bindings size their live-instance buffer from its
   length, so a driver that returned only *free* placements would silently truncate the live set — the
   failure the completeness contract exists to reject. Counting the placements with a partition already
   created distinguishes the two in one observation, and the count must not drop.
*Acceptance:* each item above answered from a real host, the affected feature updated, and the
end-to-end success criteria in Goals demonstrated on that host.

**F10 — Take the memory-slice span from the driver's placements, for both vendors.**
This is the one place where parity runs the other way: the NVIDIA detector is corrected too, so `thead`
mirrors a sound derivation rather than copying a fragile one. The NVIDIA detector recovers a profile's
memory-slice span by dividing card memory by a hardcoded 8 and rounding, even though every legal
placement of that profile already carries the span as its size. Take the span from the placement size.
Landing order matters even though the numbering does not: this precedes or accompanies F2.

**Amended after the build, on the user's decision: the division is deleted rather than kept as a
fallback, and a profile the driver placed nowhere is refused.** It was first ordered as "prefer the
placement, keep the division for a driver that enumerates none", which left a hardcoded slice count in
both detectors covering a case hardware then showed does not arise (F9 item 6: every offered profile
reported a non-empty placement set). Keeping it was the worse of the two failure directions. The span is
what the allocator matches a leftover instance's identity by, so a span read from a placement is proof
while a span computed from an assumed slice count is a guess — and a *wrong non-zero* guess can match
another profile's live instance, where a refusal cannot. Two profiles of equal compute width and
different memory span are told apart by this number alone. So the rule is now one line with no second
source: a profile is published only if the driver enumerated at least one legal placement of positive
length for it, and the span is that length. Nothing is forfeited, because the pool's per-profile ledger
is placement-derived and slot selection has nothing to choose from — such a profile was a requestable
key whose allocation could only fail. This closes known items (a) and (b) below, and removes the
`cardMemoryMiB` argument from both detectors' derivation.

Two tightenings the cross-check showed are load-bearing:
- **The nameless-driver fallback name keeps using the arithmetic value.** ~~That legacy path derives a
  profile's display name from the span, so sourcing the span differently would move a published name.~~
  **Superseded by F13, which deletes that path outright.** The tightening was correct while a nameless
  profile was still published; F13 establishes that such a profile can never be matched back to a vendor
  id and so must not be published at all, which removes the name this clause protected. Do not implement
  this clause: doing so reintroduces exactly the synthesized name F13 exists to remove. Nothing of the
  arithmetic value survives either: the amendment above deletes the last case that used it.
- **A failed placement query must not look like a driver with no placements.** The lookup collapses a
  query error to nil, which after this change would silently select the arithmetic fallback on a card
  whose driver merely failed to answer. Distinguish the two and treat a failure as a failure. **As built,
  the distinction goes further than "distinguish": a profile whose placement query failed is withheld
  from the published inventory entirely rather than published with the arithmetic span.** That was an
  explicit decision during the build, on the grounds that such a profile could not be admitted without a
  placement set anyway. The amendment above extends the same treatment to an *answered* query that
  enumerated nothing, so the two now reach one outcome by two routes — one reports an unreadable card,
  the other an unplaceable profile — and neither publishes a span nobody read.

*Acceptance:* every NVIDIA derivation table case supplies its profiles' placement spans, since a case
that injects no placement lookup now publishes nothing — plus cases for: a profile with placements
reporting the placement size as its span, a profile the driver placed nowhere and one placed at zero
length both refused with a reason naming the profile, a placed sibling surviving an unplaceable one, every
placement of one profile carrying the same positive length, a placement query that errors, and a
nameless profile whose derived name is unchanged. No published span changes for the products the
existing tests cover; if hardware shows a profile where the two sources disagree, that profile's
published span was wrong before this change, and the finding is recorded rather than papered over.

**F11 — Give the NVIDIA reclaim loop its own file, so both vendors carry one layout.**
The NVIDIA partition file holds the reservation core and the reclaim loop together at ~860 lines, while
its reclaim tests already live in a separate `mig_reclaim_test.go` — the split is half-done and the test
file name already names the missing half. Move the reclaim constants, the reclaimer type and its
reconcile, destroy, marker-removal and orphan-collection methods into `mig_reclaim.go`. Everything moved
is package-private, so no caller outside the package is affected and the sibling test files need no edit.
This is a pure move: it is what lets the two halves be built and reviewed independently instead of
serializing on one file, in both vendors.
*Acceptance:* no symbol is added, removed, renamed or changed — the diff is a move; the NVIDIA allocator
package's existing tests pass unchanged; the reservation core and the reclaim loop no longer share a file.

**F12 — A corrupt ownership marker must not cost a partition its owner, on either vendor.**
Three coupled defects, found while mirroring the marker layer, and fixed in both vendors rather than
merely avoided in the new one. All three come from the same root: the marker scan reports the files it
could not parse, and every consumer throws that report away.
The marker scan is deliberately lenient: a marker it cannot parse is collected as a corrupt path for the
caller to log, and the scan continues. The intent was that the fail-closed guard lives at the self-marker
reuse check — but that guard only protects the *owning* Pod's own re-reservation. The set of
partitions any marker owns is built from the parsed markers alone, so a corrupt marker's partition is
absent from it, and the adoption path then treats an already-owned partition as an unbound leftover and
hands it to a second Pod. Two Pods on one partition is the exact outcome the ownership record exists to
prevent, and it needs no hardware fault to reach — a truncated write during an unclean node shutdown is
enough.
The fix is available cheaply because the card is named in the marker's *file name*, which parses even
when its contents do not: a corrupt marker makes that one card's ownership set unknowable, so adoption on
that card fails closed while every sibling card is unaffected. Failing closed node-wide would let one bad
file deny an entire node; failing closed per card cannot. A card in that state can still create a fresh
partition in a free slot — only adoption of an unmarked leftover is refused, because that is the one
decision the missing knowledge would corrupt.
**The second defect is worse, and it is in the reclaim loop.** The same missing knowledge decides whether
a card is *drained*, and a drained card's unmarked partitions are destroyed. The live-partition set is
built from parsed markers and from the Pod-annotation claim view; a corrupt marker feeds neither. So a
card whose only marker is corrupt, and whose annotation view has not caught up, reads as fully drained —
and the orphan collector's own bail-out re-scan discards the corrupt report a second time, so it does not
bail. The outcome is not two Pods sharing a partition but a **running Pod's partition destroyed under
it**: a live container loses its device. Only the annotation view stands between that and a workload
today. The fix is the same shape — a card named by a corrupt marker is never treated as drained.

**The third defect is what makes the first two converge.** Nothing ever removes a corrupt marker, so a
card in that state stays there for the node's lifetime: failing closed would trade destroying a live
partition for leaking one forever. The Pod UID is in the marker's *path*, which parses even when its
contents do not, so a corrupt marker whose Pod is gone can be removed on that evidence alone — and the
partition it shadowed then becomes a genuine orphan, collectable once the card drains. A corrupt marker
whose Pod is still alive is kept, because it is still standing for something.

It is the third place parity runs backwards, and unlike the four hardenings in Notes these are not
permissivenesses to avoid inheriting — they are defects in shipped code. The thead halves land in the
partition core and its reclaim loop as those are written; the NVIDIA halves are their own change.

**T19 later removed this feature's most likely trigger, without removing the need for it.** Asked what
actually produces a corrupt marker, the answer turned out to be that the publish was atomic but never
durable, so the unclean shutdown named above was not a remote possibility but the expected outcome of one.
T19 makes the write durable on every vendor. What remains is everything that does not go through that
write at all — an out-of-band edit or a restored backup, a copy of one card's record under another card's
name (which the record's own card field refuses), a media or filesystem error, and a schema skew across a
downgrade, the one route that would surface fleet-wide at once rather than on a single card. So this
feature is still the defence; it is just no longer the routine one.
*Acceptance:* on both vendors — an unmarked leftover on a card with a corrupt marker is not adopted,
while a fresh create in a free slot on that same card still succeeds and an unmarked leftover on a
*sibling* card is still adopted; a card named by a corrupt marker is never treated as drained, so its
partitions are not collected as orphans; a corrupt marker whose Pod is gone is removed, and one whose Pod
is alive is kept; a corrupt path that names no card at all fails closed on every card, since the scope of
what is unknown is itself unknown; and the scan stays lenient, so one bad file still cannot fail a whole
pass. The existing NVIDIA tests pass unchanged.

**F13 — A profile the driver did not name must not be published, on either vendor.**
The published resource key is the profile's name, and the driver seam resolves that key back to a vendor
profile id by probing every id and comparing **names**. So a profile the driver reports without a name
cannot complete that round trip: the detector synthesizes a display name for it arithmetically, the seam
asks the driver for a name and gets nothing, and the two never meet. Publishing it produces a key a user
can request and admission will accept, and that then fails at allocation — the worst place to discover it.
The rule is therefore that only a profile the driver itself named is published; a nameless one is dropped
with a warning, and the arithmetic value survives solely as the memory-slice span fallback F10 gave it.
This is reachable only on a driver so old that the *named* profile-info versions are all unavailable —
the vendor library tries the newest first, accepts it only when it carries a name, and falls back through
a second named version before reaching the nameless one. On any such driver the seam's name matching was
already going to fail, so nothing that ever worked stops working: the failure moves from allocation time
to detection time, where it is a warning about a profile rather than a broken workload.
*Acceptance:* on both vendors a profile whose driver-reported name is empty is dropped with a warning
rather than published under a synthesized name; the memory-slice span still falls back to the arithmetic
value for a driver that enumerates no placement; and no profile that the driver *did* name changes in any
way. The existing NVIDIA tests pass unchanged except where they assert the synthesized name, which is the
behaviour being removed — that one expectation may change, and the change must be visible in the diff.

**F14 — A device node the container must have and the responder cannot produce fails the allocation, on
the whole-card path too.**
This vendor has no container-runtime hook, so the nodes the device plugin injects *are* the whole of a
container's access to its card. The partition path was therefore built to refuse when it cannot produce
every node it names. The whole-card path — the one Exclusive, Shared and non-partition Visibility
allocations take — was not: it assembled its three nodes with the shared device-spec helper, which returns
nil for a path that does not exist, and appended only the non-nil results. A missing node produced a
*successful* allocation carrying a silently short set, which starts a container that cannot address the
card it was granted. Two things follow from closing this. First, the per-card node must be named the way the
partition path names it and must carry the same proof — the **card ordinal** names the node, and the
recorded minor is compared against that node's actual kernel minor to prove the ordinal addresses the card
the detector measured. An unguarded ordinal is what the old path used, which is the error class the ordinal
guard closed for partitions, and why this supersedes the follow-up recorded as known item (c). (This clause
first ordered the opposite — name the node from the recorded minor, per the vendor's documented rule. F9
item 3 refuted that on hardware; see the card-addressing note in Notes for the measurement and both
superseded readings.)
Second, the two shared control nodes go through the same fail-closed helper, which also removes the last
hardcoded `/dev` literals from the responder and so makes its success case hermetically testable rather
than dependent on what the machine running the tests happens to have under `/dev`.
Two allocations that used to succeed now fail, both surfacing as an `Allocate` error — the kubelet does
not start the container — rather than as a container that starts blind: a card whose driver reported no
minor number, which F13's sibling rule in the detector records as *nothing* rather than as a fabricated
index, is now unallocatable; and a node missing `/dev/alixpu` or `/dev/alixpu_ctl`, or whose card node is
not a character device, now fails rather than under-delivering. The second has no practical blast radius —
such a node has no usable driver — but the first is a card that leaves service rather than one that
misbehaves visibly, and that is the intended trade.
*Acceptance:* the whole-card response injects the node the **card ordinal** names, proven with a fixture
that carries a real offset — an ordinal whose recorded minor is not equal to it, and a decoy character node
at the path the recorded minor would name, which must not appear in the response; an allocated card with no
recorded minor, a card node whose kernel minor disagrees with the record, a missing card node, a card node
that is not a character device, and either control node missing each fail the allocation with a nil
response; an *unallocated* card missing its record does not fail it; and the responder is assembled from the
same helpers as the partition path, so neither path can drift from the other's fail-closed rule.

**F15 — A thead partition profile is published with a separator, and handled internally without one.**
This vendor names its GPU-instance profiles `MIG <slices>g<memory>gb`, with no separator between the two
numbers, where NVIDIA names its own `<slices>g.<memory>gb`. Normalization keeps whatever the vendor wrote —
it lowercases and takes the last whitespace-separated field, and a `.` is a legal character it never strips
— so the published key today reads `alibabacloud.com/ppu.partitioned.mig-4g48gb` while the equivalent NVIDIA
key reads `…mig-3g.40gb`. The two are the same shape of thing and should read the same way to whoever writes
a Pod spec, so the **published** form gains the separator: the resource key, the per-profile ledgers a user
reads, and the request a user writes all use `4g.48gb`. Everything the operator only says to itself keeps the
vendor's own spelling: the `Devices` record, the ownership markers on disk, and every name handed to or
matched against the vendor library stay `4g48gb`. The conversion is therefore a boundary, not a rename — one
function each way, applied where a name crosses out of the operator and back in — and the driver-facing seam
is deliberately left untouched, because a name it does not recognise cannot create a partition. A profile
name that does not have the vendor's two-number shape is published unchanged rather than guessed at.
*Acceptance:* the node advertises `<base>.partitioned.mig-4g.48gb`; a Pod requesting that key is admitted and
gets a `4g48gb` partition; the `Devices` record, the marker file and the vendor-library calls all still carry
`4g48gb`; the reverse conversion resolves the published key back to the vendor's name for every offered
profile; a name of another shape survives the round trip untouched; and the NVIDIA published keys are
byte-identical to before, since that vendor already writes the separator itself.

### Notes / Constraints / Caveats

- **Four places where the new code is strict and the NVIDIA path is not.** These are the spec's only
  intentional behavioural divergences from the implementation it mirrors. Each exists because copying a
  known-permissive behaviour into fresh code is a choice, not an inheritance:
  1. **Enumeration completeness.** The NVIDIA seam skips every failed profile, handle, UUID and instance
     query and returns success with partial state, so a live partition can read as absent — which then
     lets its marker be removed as "already gone", an occupied slot be overlooked, or an orphan be leaked
     indefinitely. The thead seam returns an error whenever it cannot prove its enumeration is complete.
  2. **Adoption identity.** The NVIDIA live-instance record carries no profile, so adoption of a leftover
     partition accepts any unmarked instance with matching compute slices and placement length — a
     different profile of the same geometry, including a media or graphics variant, qualifies. The thead
     live-instance record carries the raw profile id, and adoption requires it to match.
  3. **Destroy identity.** The NVIDIA destroy locates its target by card and instance id alone, without
     re-reading identity under the lock, so an out-of-band destroy/recreate between the snapshot and the
     destroy can reuse the id and the replacement is destroyed instead. The thead destroy re-reads and
     verifies the partition's identity inside the lock before tearing it down.
  4. **Normalization collisions.** Aggregation merges profiles by name, summing counts and keeping the
     first profile's memory, so two raw names normalizing to one — or one name exposed with differing
     geometry or memory — silently produces wrong capacity and wrong credits. A thead collision rejects
     the profile with a warning instead of aggregating it.
- **The fifth hardening is not a divergence, so it is not in the list above.** The four items above are
  places the mirror is deliberately stricter than the code it mirrors, and each leaves the original as it
  is. The corrupt-marker hole (F12) is a defect in the original, found while writing the mirror, so it is
  fixed in both — which is why it reads as a feature rather than as a divergence note.
- **The memory-slice span is a vestigial field, carried for parity, and worth an invariant rather than
  trust.** Its declared purpose — a second request axis beside the compute-slice count — was never
  built: requests name a profile, and the Kueue credit fold divides the profile's reported memory by the
  card's, never touching the span. Its only semantic consumer is the adoption path, which reuses a
  leftover GPU instance that has no compute instance yet only when the instance's live placement length
  equals the profile's recorded span; the driver seam takes it as a parameter that the NVIDIA
  implementation ignores outright. Since the span is provably the placement length, that comparison
  checks a value against its own source. Populate the field as F2 describes for parity, and assert
  `span == Placements[0].Length` in the detector test so the redundancy is pinned instead of assumed.
  Removing the field is an API change and is out of scope here; this note exists so a later cleanup is a
  decision rather than a discovery.
- **Profile ids must be discovered, never computed.** The vendor header retains the upstream numbering
  in which a profile constant encodes a slice count, but the vendor's own documentation shows its
  product reporting different ids for those slice counts, and a compute-instance profile id outside the
  slice-based range entirely. The NVIDIA driver seam already matches profiles **by name** over a probe
  of every id, which transfers unchanged; the NVIDIA helper that maps a slice count to profile ids
  through a static table does **not** transfer and must not be ported. The compute-instance profile is
  likewise discovered by enumerating the created GPU instance's profiles and taking the one that spans
  it.
- **Profile names need normalizing before they are resource-name segments.** The vendor displays names
  with a feature-name prefix and a space. The published resource key must be the bare geometry so it is
  a valid qualified-name segment; a name that cannot be normalized into one makes the profile
  unrequestable and must be dropped with a warning rather than published broken.
- **Take the memory-slice span from the driver's placements, not from a memory-size division.** A
  profile's compute-slice count and its memory-slice span are different quantities — the NVIDIA
  whole-card profile reports 7 compute slices while occupying all 8 memory slices — and the vendor's
  profile record carries no memory-slice field, only a memory size. The NVIDIA detector therefore
  recovers the span arithmetically, dividing card memory by a hardcoded 8 and rounding. That constant
  happens to be right on both vendors' current products, but the same quantity is *also* available
  authoritatively: every legal placement of a profile carries the profile's span as its size, so
  `Placements[0].Length` is the driver's own answer to the same question. Populate the span from the
  placements when the driver returns any, and keep the division only as the fallback for a driver that
  returns none. The published value is identical to what the NVIDIA derivation would produce — this
  changes where the number comes from, not what it is — and it removes the denominator as a failure
  mode. It matters because a wrong span is silent: the adoption path compares the detect-time span
  against a live placement length (see the note below), and a mismatch makes it never match rather than
  raise.
- **MIG mode enablement is a prerequisite with a wider blast radius than on NVIDIA.** The vendor driver
  refuses a mode change on a card that has any active process, and it reports the holders in the kernel
  log: a line naming the card and the number of active processes, one line per holding process, a
  failure line, and the control ioctl returning `EBUSY`. Because any process that opens the driver may
  hold every card's node, a workload running on one card can block the mode change on another. The
  runbook must state that the mode is changed only while the node is free of accelerator workloads,
  including the device-manager itself.
- **Toggling the mode requires a device-manager restart.** The re-detect trigger key does not include
  the partitioning mode, so a mode change is not noticed until the DaemonSet restarts. This is a
  pre-existing property of the shared detector, documented for NVIDIA, and must be documented here too.
- **A path is named by the card's ordinal; the recorded minor exists only to prove the ordinal is right.**
  **Settled on hardware** (F9 item 3), which is the third and final answer this question got — the two
  earlier ones are recorded below because each was reasonable and each was wrong. Measured on a 16-card
  host, uniformly across all sixteen:
  - the card's device node is `/dev/alixpu_ppu<ordinal>`, and that node's kernel minor is `ordinal + 1`,
    because the shared `/dev/alixpu` control node occupies minor 0 of the same character-device major;
  - the minor the vendor library reports for a card — what the detector stores in `PhysicalIndexes[0]` —
    **is** that kernel minor, so it is `ordinal + 1` too. The operator's own `Devices` object on the host
    reads `index=14 → physicalIndexes=[15]` for all sixteen cards;
  - the procfs capability tree contains exactly `ppu0` … `ppu15` — there is a `ppu0` and no `ppu16` — so it
    is keyed by the **ordinal**, not by the minor, and a live GPU instance created on the card at ordinal 14
    appeared at `capabilities/ppu14/mig/gi0/`, not under `ppu15`;
  - the driver names the card by its ordinal in its own kernel log too (`PPU0014` for ordinal 14).

  So the accelerator index — which is the ordinal — names both paths, and the recorded minor names neither.
  **The vendor documentation is wrong on this point**: it states the rule as
  `/dev/alixpu_ppu[minor number]`, and building a path from the reported minor addresses **the next card**,
  or, for the last card, a node that does not exist.

  **The guard therefore proves the ordinal instead of assuming an offset.** It stats the node the ordinal
  names and compares that node's *actual* kernel minor against the *recorded* one; equal means the ordinal
  addresses the card the detector measured. On the measured host that is `15 == 15` for ordinal 14. A card
  with no recorded minor, a missing node, a node that is not a character device, or a minor that disagrees
  refuses the allocation before any reservation is taken. No `+1` appears anywhere in the code: had the
  offset been zero, or two, or absent on another product, the guard would still be correct — which is the
  only reason this question got a wrong answer twice without shipping a wrong path.

  **The two superseded answers, kept because they explain the shape of the code.** The first ruling said
  both paths were keyed by the ordinal and named the accelerator index as that ordinal — correct, but it
  justified itself with reconstructed arithmetic (minor = ordinal + 1) rather than a proof, and it declared
  the existing whole-card path already correct and out of scope. The second reversed it to key both paths by
  the recorded minor, on the strength of the vendor's documented naming rule; that reading is what the
  hardware refuted, and under it the whole-card path silently injected a neighbour. What survived both
  reversals is the guard, because it never depended on the answer.
- **The accelerator index is a post-filter ordinal, which is a second reason it cannot name a path.** It
  increments only for a card the detector accepted, so a card skipped mid-enumeration (handle, UUID or PCI
  lookup failure) shifts every later card's index down by one. A path built from it would then address a
  different card than the one the request resolved to, with nothing in the system able to notice. The
  recorded minor carries no such hazard: it is read from the driver per card, so a skipped card removes a
  record rather than renumbering the rest.
- Go, Kubernetes controller-runtime; the cgo binding is loaded at runtime by name, so the whole module
  including the vendor detectors builds and tests on the development platform without any vendor SDK.

### Boundaries

- **Always:** report exactly one slicing capability per card. Discover profile ids by probe and match by
  name. Keep the pure partition core behind the build-tag seam so it is table-tested without hardware.
  Fail closed on any unprovable partition identity. Destroy a compute instance before its GPU instance.
  Verify on hardware before declaring the feature done.
- **Ask first:** before changing any shared vendor-neutral layer — if parity appears to require a change
  in `pkg/device`, `pkg/deviceplugin`, `api/worker`, the worker controllers or the webhooks, that is a
  signal the mirror has diverged and needs review rather than a patch. This gate fired once, for F15,
  and the answer was to proceed: a difference in how two vendors *spell* a profile is not the mirror
  diverging, and the alternative placement would have corrupted the record the driver seam matches by.
  What the gate bought was the shape of the change — four single delegations to the package that already
  holds every per-manufacturer fact, rather than the spelling rule copied into four layers. It fired a
  second time for T19, over a wider surface: three manufacturers this spec never otherwise touches
  (hygon, metax, cambricon) carried their own copy of the record-publishing dance with the same missing
  syncs. The answer was again to proceed, because converting all five to one helper is a net deletion and
  leaves no vendor holding a copy that still cannot survive a reboot, whereas converting two would have
  left the defect live on three and a helper with two callers where five apply. Also before choosing a
  partition kind word other than the vendor's own, and before regenerating any binding other than the
  vendor's.
- **Never:** set or clear MIG mode from operator code. Never port the static slice-count-to-profile-id
  table. Never fall back to naming the parent card when a partition cannot be proven. Never publish a
  profile whose name cannot be normalized into a valid resource-name segment. Never hand-edit generated
  binding files.

### Risks and Mitigations

- The driver's real profile ids differ from the documented ones → **Mitigation:** nothing derives a
  profile id; every id comes from a probe, and F9 item 1 confirms the records before release.
- The compute-instance profile cannot be identified generically → **Mitigation:** F9 item 2 is a release
  gate; until it is answered the create path has no hard-coded id to be wrong about.
- Capability node minors are reassigned across create/destroy or reboot → **Mitigation:** resolve them
  at allocation time from the procfs tree rather than caching them at detect time; F9 item 3 confirms.
- The device-manager's own handle blocks a destroy, turning reclaim into an endless retry →
  **Mitigation:** the bounded busy-retry and its operator-visible condition already exist in the shared
  reclaim core; F9 item 4 sizes the bound against observed behaviour.
- A card skipped mid-enumeration desynchronizes the accelerator index from the driver's card ordinal, so
  a partition is addressed on the wrong card → **Mitigation:** F8 asserts the minor-number offset
  invariant per card and skips a card that violates it, turning a silent mis-injection into a visible
  skip.
- Injecting too few device nodes leaves the partition unusable; too many re-opens the isolation hole →
  **Mitigation:** the vendor documents the exact node set for partition isolation; F9 item 5 confirms it
  on hardware before release.
- A mode change cannot be performed on a busy node, so the feature looks broken to an administrator →
  **Mitigation:** the runbook leads with the prerequisite and the kernel-log signature that identifies
  the blocking processes.
- F10 changes a shipped vendor's detector, so a profile's published memory-slice span could move on a
  GPU no test covers → **Mitigation:** the fallback keeps the old arithmetic for any driver that
  enumerates no placements, and the two sources are provably equal wherever both exist, so a move means
  the old value was wrong; the existing tests are required to pass unchanged, which bounds the blast
  radius to profiles whose placements contradict the division.
- Regenerating the binding drops vendor families the detector still calls → **Mitigation:** F1's
  acceptance includes a clean build and test run after regeneration, which is where such a removal
  surfaces.
- F12 changes a shipped vendor's adoption path, so a card that adopted a leftover before could refuse to
  now → **Mitigation:** the refusal is reachable only on a card that has a corrupt marker, which is
  already a broken state; the card keeps creating fresh partitions in free slots, so the only capacity
  lost is the reuse of a leftover that may not have been free to reuse; and the existing NVIDIA tests are
  required to pass unchanged, which bounds the change to the corrupt-marker case.
- F12 also makes a shipped vendor *delete* a marker file, which no code did before → **Mitigation:** only
  a file that could not be parsed at all is ever removed, and only once its Pod is gone — evidence read
  from the path, not from the unreadable contents; a corrupt marker whose Pod is alive is kept. Without
  this the other two fixes would trade destroying a live partition for leaking one for the node's
  lifetime, so the deletion is what makes the fail-closed state converge rather than accumulate.
- F14 changes this vendor's shipped whole-card allocation, so a card that used to be allocatable can now
  be refused → **Mitigation:** both refusals replace an allocation that was already wrong rather than one
  that worked — a device set silently short of the card, or a set carrying a neighbour's node — so what is
  lost is a container that would have started blind. The refusal is reachable only on a card whose driver
  reported no minor number, or on a node missing a control node the driver itself needs, and it surfaces as
  an `Allocate` error the kubelet reports on the Pod rather than as a silent failure inside it. The blast
  radius is bounded further by the responder being a per-vendor file: no other manufacturer's whole-card
  path is touched.
- **Known items the end-of-build review surfaced and this spec records rather than fixes.** Recorded so
  that meeting this spec is not mistaken for the path being sound end to end:
  (a) ~~**A placement-query failure now removes that profile from a shipped vendor's published
  inventory.**~~ **Closed by F10's amendment**, which makes withholding the rule rather than an
  undocumented divergence: an unreadable placement query and an answered one that enumerated nothing both
  cost the profile its place in the inventory, because the span has no second source to publish from. The
  item is kept rather than deleted because it records that the behaviour change on a shipped vendor was
  first taken silently during the build, and only became a stated rule once the fallback it diverged from
  was removed.
  (b) ~~**A profile the driver offers with no legal placement is still published.**~~ **Closed by F10's
  amendment.** It could never be satisfied — the ledger is placement-derived and slot selection has
  nothing to choose — so it was a requestable key whose allocation had to fail, the same class F13 closes
  for nameless profiles. F9 item 6 gated the assumption that it cannot arise and the measured product held
  (every offered profile reported a non-empty placement set), but one product at one driver version is not
  a guarantee, which is why the profile is now refused rather than trusted not to appear.
  (c) ~~**The whole-card device-node path is not covered by the ordinal guard.**~~ **Closed by F14**, which
  the user chose to take rather than leave as a follow-up. The item is kept rather than deleted because it
  records why the path was out of scope in the first place — the spec said "change nothing about the
  existing one", and widening it had been declined once already; F14 is that decision reversed on the
  evidence that the path was not merely unguarded but was naming the node by the wrong key.
  (d) **Instance capacity is inferred from the placement count** in both vendors' bindings. Both headers
  document the list as every legal placement irrespective of occupancy, which makes the count a proven
  ceiling. **No longer an inference: confirmed on hardware** (F9 item 6). The placement list was
  byte-identical with no instance, with one instance live, and with the card carved to capacity, and it
  keeps listing placements for a profile that can no longer be created at all. The item stays recorded
  because the guarantee is the vendor's to keep, on one product, at one driver version — but the buffer
  being exactly filled is now demonstrably *full occupancy* rather than truncation, so treating a full
  buffer as an error would break a fully partitioned card, and the code correctly does not.
  (e) **A hand-written binding method passes the wrong handle kind.** A gpu-instance method reads a
  compute-instance id by handing the parent card's handle to a call the header documents as taking a MIG
  device handle. It has **no callers**, and a gpu instance holds no MIG device handle to pass, so the method
  may be misconceived rather than repairable — which makes it a design decision rather than a fix, and it is
  recorded here rather than changed under a review round.
- **A pre-existing shared-layer defect this feature's hardware window surfaced, outside this spec's scope.**
  The `Devices` object's controller owner was moved from the `NodeFeature` to the `Node` — correctly, since a
  cluster-scoped dependent needs a cluster-scoped owner — but the alignment path *adds* the new controller
  reference instead of replacing the old one: it matches an existing reference by api-version and kind, and
  the two owners differ in kind, so both end up marked controller. The API server refuses two controllers, so
  on any cluster whose `Devices` was created by a version that owned it via the `NodeFeature`, **every detect
  report fails for the object's lifetime**. The record then freezes at its old content: no capability, no
  partition profiles, and a node that can never advertise this feature. The only signal is one error line per
  detect cycle in the device manager's log. Deleting the `Devices` object is the workaround and the object is
  recreated correctly within seconds. It is not reproduced by any test because no test upgrades across that
  ownership change. Recorded here because it gates the deployment of this feature on every existing cluster,
  and it belongs to the shared detector rather than to any manufacturer.
- **Pre-existing NVIDIA defects this spec records but does not fix.** A design cross-check surfaced four,
  all in code declared out of scope above. A fifth, surfaced while building the mirror, is fixed rather
  than recorded, because it lets two Pods hold one partition: see F12. They are written down so that meeting this spec is not mistaken
  for the partitioned path being sound end to end, and so a later reader finds a record rather than a
  discovery: (a) the live-claims read skips a Pod whose allocation annotation cannot be parsed and still
  returns success, which defeats reclaim's fail-closed guard — a live Pod with an unreadable annotation and
  no marker on that card can have its partition collected as an orphan; (b) destroy locates by instance id
  without re-verifying identity under the lock, so an id reused between snapshot and destroy loses the
  replacement; (c) adoption matches geometry, not profile identity; (d) the reclaim debounce counts
  reconciles rather than elapsed time, and broadcasts as well as the resync ticker drive it, so three rapid
  broadcasts can exhaust it in seconds — the "sized above the kubelet retry window" claim is not
  guaranteed. **Mitigation for this spec's scope:** the four thead-side hardenings in Notes mean none of
  (a)–(c) is reproduced in the new code; (d) is shared-layer timing the thead reclaim inherits, and F9 item 4
  has now been observed rather than assumed — with the result that the retry has less to do than its name
  implies. A passive holder does not block a destroy at all, and the one condition that does (a live compute
  instance under the GPU instance being destroyed) is deterministic and is what the reclaim already resolves
  by ordering. So the debounce's weakness is real but its blast radius is smaller than feared: the case a
  wider window would have bought — waiting out a co-resident holder — does not exist on this driver. The
  case that could still need one, an active compute process on the partition, remains untested.

## Design Details

### Commands

Three environments, because one of them cannot do what the others can.

**Local (the default for everything).** Verified: the whole module, the cgo bindings included, builds and
tests on the development platform without any vendor SDK — the bindings resolve their symbols at runtime,
so no accelerator and no vendor toolkit is needed.

```bash
make deps                 # sync vendored dependencies and subcharts
make generate hgml        # regenerate binding/hgml from gen/binding/hgml (c-for-go + cgo -godefs)
make generate chart       # regenerate values.schema.json + README.md — required after any values.yaml edit
make lint                 # whole-module golangci-lint; cold cache needs a long timeout (~2 min)
make test                 # go test -race -cover ./...  (trailing args EXCLUDE packages, they do not select)
go test -race ./pkg/devicemanager/allocator/thead/...   # target one package directly
make lint chart           # offline chart checks (make test chart mutates a live cluster — do not use)
```

**A linux container (the only way to compile the build-tag seam).** No local check compiles
`*_linux.go`: the formatter filters those files, the linter runs without the linux tag, and the build
script builds the host platform only. A seam-touching task is therefore unverified locally unless it is
compiled on linux:

```bash
docker run --rm -v "$PWD":/src -w /src golang:<go-version-from-go.mod> \
  bash -c 'CGO_ENABLED=1 go build ./... && go vet ./pkg/devicemanager/allocator/thead/...'
```

`bash -c`, never `bash -lc`: a login shell re-reads the profile and resets `PATH`, and the toolchain
lives outside the profile's `PATH`, so `go` is not found (`go: command not found`, exit 127) — measured
in this image. Scope `go vet` by package as above; `go vet ./...` cannot pass, because a generated file
under `binding/hsa` trips `possible misuse of unsafe.Pointer`. `go build ./...` is whole-module and does
pass.

**A remote linux host (a second, stronger check for the seam).** For the seam-touching tasks only, sync
the working tree to a linux checkout and lint it there — on linux the linter does compile the seam files.
Host and path are supplied out of band and are deliberately not recorded here. Four constraints, each
learned by getting it wrong:

- **Do not run `make lint` on a synced copy.** It passes `--fix`, so it *repairs the copy* and then
  reports what is left; the repairs die with the copy and the run reports a clean tree that is not clean.
  Locally `make lint` is trustworthy only because `git status` afterwards proves nothing was absorbed, and
  a disposable copy affords no such check. For a verdict, run the linter directly with no `--fix`:

  ```bash
  golangci-lint run --timeout 15m --max-same-issues=0 --max-issues-per-linter=0 \
    --build-tags="goccy netgo" ./pkg/devicemanager/...
  ```

- **Pass those two `--max-*` flags or the count lies.** No `issues:` section is configured, so
  golangci-lint's defaults apply and `max-same-issues: 3` collapses one repeated message to its first
  three hits in filename order. A defect class spanning several files then reports as three findings
  attributed to whichever package sorts first, not to the one being worked on.
- **Sync to the same commit first**, or the result describes a tree nobody has.
- **Preserve a worktree's `.git`.** This repository may be a git worktree whose `.git` is a *file*, so the
  sync must carry it (`rsync --filter='P .git'`); excluding `.git/` as a directory lets `--delete` destroy
  the receiver's repository. Sync into a scratch directory rather than a working checkout for the same
  reason.

**Environment prerequisites for F1.** The binding generator is installed on demand into a local tool
directory, so the first `make generate hgml` needs network access. The generator's cgo type-dump step is
also sensitive to the checkout path; if it fails inside a worktree, run it from a checkout whose path
ends in the module path.

### Project Structure

```
binding/hgml/                              # vendor management-library binding
  hgml.h, hgml.go, const.go, doc.go        #   generated by the binding generator
  zz_generated.types.go                    #   generated by the cgo type dumper
  cgo_helpers_static.go, const_static.go   #   hand-maintained overlays, survive regeneration
  library.go, library_device.go            #   hand-written ergonomic layer  (F1 extends this)
  library_device_gpm.go
gen/binding/hgml/                          # generator inputs: vendor header + generator config

pkg/devicemanager/detector/nvidia/
  mig_profile.go                           # F10: span from placements, division as fallback
  mig_profile_test.go                      # F10: existing cases unchanged + span/placement assertion

pkg/devicemanager/allocator/nvidia/
  mig.go                                   # F11: reservation core stays; reclaim moves out
                                           # F12: a corrupt marker fails adoption closed on its card
  mig_test.go                              # F12: three new adoption cases, existing ones unchanged
  mig_reclaim.go                           # F11: reclaim loop, moved verbatim  (new);
                                           # F12: then extended in place by T11
  mig_reclaim_test.go                      # F11: already existed — it named the missing half before it
                                           # existed; F12: T11 added cases to it

pkg/devicemanager/detector/thead/
  device.go                                # F2: MIG-mode read, capability selection, aggregation
  mig_profile.go                            # F2: pure profile derivation  (new)
  mig_profile_test.go                       # F2: table-driven derivation tests  (new)

pkg/devicemanager/allocator/thead/
  deviceplugin.go                          # F4: Partitioned server, driver wiring; F8: device specs
  mig.go                                   # F5: markers, slot selection, reservation (T4);  (new)
                                           #     the actuator and its rollback (T6, with the
                                           #     device specs its response carries)
  mig_reclaim.go                           # F6: reclaim loop  (new — see below)
  mig_driver_linux.go                      # F4: vendor-library actuator  (new)
  mig_driver_other.go                      # F4: non-linux stub  (new)
  mig_visibility.go                        # F7: physical-sliced responder  (new)
  mig_test.go, mig_reclaim_test.go, mig_visibility_test.go, deviceplugin_test.go   (new)

Both vendors carry the same layout, which F11 is what makes true: the reservation core and the reclaim
loop own disjoint files in each, so they can be built and reviewed independently instead of serializing
on one file.

pkg/nodefeature/knowns.go                  # F3: partition kind for the manufacturer
deploy/gpustack-operator/chart/values.yaml # F3: manufacturer table entry

docs/operation/thead-mig.md                # F8/F9: runbook  (new)
docs/README.md, README.md, docs/accelerator-requests.md,
docs/architecture/{discovery,scheduling-chain}.md   # index and cross-reference updates
```

Unchanged and reused as-is, and the diff should prove it: `pkg/device/**`,
`pkg/deviceplugin/**`, `api/worker/**`, `pkg/worker/controllers/worker/**`,
`pkg/worker/webhooks/worker/**`, `pkg/worker/extensionapis/worker/**`, `pkg/workergateway/**`.

### Code Style

The vendor driver seam mirrors the NVIDIA one; this is the shape to follow — profiles matched by
name over a probe, never by a computed id:

```go
// profileID matches profile by name against the card's probed GPU-instance profiles and returns
// its profile id. The vendor's header keeps the upstream numbering, but its driver does not assign
// those ids the upstream slice-count meaning, so the id is never derived — only read back.
func (d *hgmlMigDriver) profileID(dev hgml.Device, profile string) (uint32, error) {
	for id := uint32(0); id < hgml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		if normalizeProfileName(info.GetName()) == profile {
			return info.Id, nil
		}
	}
	return 0, fmt.Errorf("card has no gpu-instance profile named %q", profile)
}
```

Conventions: multi-word Go files in snake_case; a doc comment states the behaviour and the constraint
that motivates it, not the symbol names; errors are typed and returned early; reconciliation is
level-based and idempotent; tests are table-driven over a shared loop with a fake driver and a
temporary marker root, asserting observable state rather than call sequences.

### Implementation Plan

T0, T1, T2 and T4 are unblocked and own disjoint paths, so the build starts four-wide. T0, T1 and T11 are
prefactors on the vendor being mirrored — make the change easy, then make the easy change: T1 corrects
the shared derivation so T3 mirrors a sound one instead of a fragile one, T0 splits the NVIDIA reclaim
file so T7 mirrors a layout that already exists rather than inventing one, and T11 closes on NVIDIA the
hole T4 was about to fail closed on for thead alone. T11 is listed with them rather than at the end of
the numbering: its id is later only because it was found during the build, and it must not be read as
following the hardware gate.

- [x] **T0 · Prefactor — move the NVIDIA reclaim loop into its own file**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/nvidia/mig.go`, `pkg/devicemanager/allocator/nvidia/mig_reclaim.go`
      Gate: —
      Acceptance: the reclaim constants, the reclaimer type and its reconcile, destroy, marker-removal and
      orphan-collection methods live in `mig_reclaim.go`; the reservation core stays in `mig.go`; no symbol
      is added, removed, renamed or changed, so the diff is a move and nothing outside the package is
      affected; the package's existing tests — including the `mig_reclaim_test.go` that already named this
      file — pass unchanged.
      Verify: `go test -race ./pkg/devicemanager/allocator/nvidia/... && make lint`

The usual de-risk-first ordering is inverted here by the environment, and deliberately so. The riskiest
assumptions are the hardware ones, and they cannot be probed until a T-Head host can be emptied of
accelerator workloads — the driver refuses a partition-mode change while any process holds a card. So
the hardware gate is last (T10) rather than first, and the mitigation is containment: every unverified
assumption lives in T5's seam or T6's node contract, both of which fail closed by construction, and
neither of which any other task depends on for its own correctness.

- [x] **T1 · Prefactor — memory-slice span from the driver's placements**
      Blocked by: None
      Owns: `pkg/devicemanager/detector/nvidia/mig_profile.go`, `pkg/devicemanager/detector/nvidia/mig_profile_test.go`
      Gate: review
      Acceptance: the published span comes from the profile's placement size when the driver enumerates
      any, and from the existing division otherwise; a placement-query failure is distinguished from a
      driver that enumerates none. Both existing derivation tests pass unchanged; new cases cover
      span-from-placement, uniform positive placement lengths, and a query error.
      **Amended after T12 landed:** the clause that required the nameless-driver fallback name to keep
      using the arithmetic value, and the test case asserting that name, are void — F13 deletes the path.
      As built, this task did produce them and T12 then removed them, which is why the diff shows that one
      expectation changing meaning.
      Verify: `go test -race ./pkg/devicemanager/detector/nvidia/... && make lint`

- [x] **T11 · Prefactor — a corrupt marker must not cost a partition its owner, on NVIDIA**
      Blocked by: T0
      Owns: `pkg/devicemanager/allocator/nvidia/mig.go`,
      `pkg/devicemanager/allocator/nvidia/mig_test.go`,
      `pkg/devicemanager/allocator/nvidia/mig_reclaim.go`,
      `pkg/devicemanager/allocator/nvidia/mig_reclaim_test.go`
      Gate: review
      Acceptance: the marker scan's corrupt paths reach all three decisions they bear on, keyed by the
      card each corrupt file names — the card parses out of the file name even when the contents do not.
      Adoption of an unmarked leftover on a card with a corrupt marker is refused, while a fresh create in
      a free slot on that card still succeeds and adoption on a sibling card is unaffected. A card named by
      a corrupt marker is never treated as drained, in the reclaim pass and in the orphan collector's own
      bail-out re-scan, so a running Pod's partition cannot be destroyed under it. A corrupt marker whose
      Pod is gone is removed — the Pod UID parses out of the path — and one whose Pod is alive is kept, so
      the fail-closed state converges instead of darkening the card for the node's lifetime. A corrupt path
      naming no card fails closed on every card. The scan stays lenient, so one corrupt file still cannot
      fail a whole pass. The package's existing tests pass unchanged, and every new outcome above is
      asserted.
      Verify: `go test -race ./pkg/devicemanager/allocator/nvidia/... && make lint`

- [x] **T12 · Prefactor — stop publishing a profile the driver did not name, on NVIDIA**
      Blocked by: T1
      Owns: `pkg/devicemanager/detector/nvidia/mig_profile.go`,
      `pkg/devicemanager/detector/nvidia/mig_profile_test.go`
      Gate: review
      Acceptance: a probed profile whose driver-reported name is empty is dropped with a warning naming the
      card and the profile id, instead of being published under a name synthesized from card memory; the
      arithmetic memory-slice span survives as the placement fallback F10 gave it, so no named profile's
      published span or name moves; the only existing expectation that may change is the one asserting the
      synthesized name, and that change is visible in the diff. Confirm before implementing that the
      nameless path is unreachable on any driver whose named profile-info versions work, and report it if
      not — that premise is what makes this safe on a shipped vendor.
      Verify: `go test -race ./pkg/devicemanager/detector/nvidia/... && make lint`

- [x] **T2 · Regenerate `binding/hgml` at the current header and complete its MIG surface**
      Blocked by: None
      Owns: `binding/hgml/**`, `gen/binding/hgml/**`, `hack/generate.sh`
      Gate: review
      Acceptance: the missing `bool` declaration is prepended to the generator's working copy of the
      header rather than to the vendor's file, and is scoped to this vendor so the C99 vendors that
      already declare it are unaffected; the two new mask macros are dropped by a config rule rather
      than by editing the header; the vendor's own header ends the task byte-identical to how it started,
      which is the point of putting the declaration on the working copy — the generator config beside it
      is ours and is where the ignore rule belongs, since the copy of it made during generation is
      deleted again; `make generate hgml` regenerates the package from the working-tree header, and a second
      run produces no further change; the hand-written layer exposes every MIG operation the driver seam
      calls — profile info, possible placements, remaining capacity, live GPU instances, a GPU instance's
      compute instances, compute-instance profile info, and destroy for both instance kinds — each shaped
      like its NVML counterpart; `Device.GetGpuInstance`'s inverted symbol-lookup guard is corrected while
      the legitimate versioned-fallback branches are left alone; the vendor families the newer header
      drops break no existing detector call; the static overlay files survive regeneration untouched.
      Verify: `make generate hgml && go build ./... && go test ./pkg/devicemanager/... && make lint`,
      then the linux-container compile

- [x] **T3 · thead detector reports partition profiles, and `thead` declares its partition capability**
      Blocked by: T1, T2
      Owns: `pkg/devicemanager/detector/thead/**`, `pkg/nodefeature/**`,
      `deploy/gpustack-operator/chart/values.yaml`, `deploy/gpustack-operator/chart/values.schema.json`,
      `deploy/gpustack-operator/chart/README.md`
      Gate: review
      Acceptance: a partition-mode-enabled card reports the physical capability and a disabled one keeps
      behaving exactly as it does today (which is no slicing capability at all — see F2), never both;
      the name normalization lives in `pkg/nodefeature`, beside the function that already decides whether
      a name can be a resource key, because the detector that publishes the key and the driver seam that
      matches it back must apply byte-identical rules and the two packages do not otherwise depend on each
      other; a profile the driver did not name is dropped per F13; profile names are normalized (vendor
      prefix and whitespace stripped) and a name that still cannot form a valid resource-name segment is
      dropped with a warning; two raw names normalizing to one, or one normalized name exposed with
      differing geometry or memory, is rejected rather than aggregated; the span satisfies
      `MemorySlices == Placements[0].Length` wherever placements exist; group aggregation is called as the
      final detection step; `thead` carries partition kind `mig` in both the registry and the chart row.
      Verify: `go test -race ./pkg/devicemanager/detector/thead/... ./pkg/nodefeature/... ./pkg/worker/kuberess/... && make generate chart && make lint`

- [x] **T4 · thead partition core — driver seam, ownership markers, slot selection, reservation**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/thead/mig.go`, `pkg/devicemanager/allocator/thead/mig_test.go`,
      `pkg/devicemanager/allocator/thead/mig_driver_other.go`
      Gate: review
      Acceptance: the four-method seam interface and its non-linux stub exist; the live-instance record
      carries the raw profile id, and adoption of an unmarked leftover requires that id to match, not only
      the geometry; per-card locking guards create-and-record; markers are written atomically and parsed
      fail-closed; slot selection is deterministic lowest-free-first; the three reservation outcomes are
      returned distinctly, so T6's actuator can roll back exactly what one allocation did; a reused
      instance id is caught by an identity check. Table tests over a fake driver cover fresh create,
      kubelet retry, stale self-marker, profile mismatch, adoption, same-geometry-different-profile
      refusal, empty-identity skip, and concurrent allocation on one card. The whole task is pure: it
      addresses no device-plugin server, so it compiles and tests without the server wiring T6 adds.
      Verify: `go test -race ./pkg/devicemanager/allocator/thead/... && make lint`

- [x] **T5 · thead driver seam over the vendor library (linux)**
      Blocked by: T2, T3, T4
      Why T3: the seam resolves a published profile key back to a vendor profile id by name, so it calls
      the very normalization T3 places in `pkg/nodefeature`. Sharing one implementation is what keeps the
      round trip closed; duplicating it would make a drift between the two silently unrequestable.
      Owns: `pkg/devicemanager/allocator/thead/mig_driver_linux.go`
      Gate: review
      Acceptance: the profile name is normalized by calling `nodefeature.NormalizePartitionedProfileName`,
      never by a local copy of its rules — nothing mechanically enforces this, and a drift between the two
      would make a published profile silently unrequestable, which is the whole reason the function was
      placed in a shared package; a profile is matched by name over a probe of every id and never computed; the
      compute-instance profile is discovered as the one spanning the whole GPU instance, with no
      hard-coded id; create makes the GPU instance at the chosen placement then its compute instance, and
      tears the GPU instance down on any later failure; destroy re-reads and verifies the partition's
      identity inside the lock, then removes the compute instance before the GPU instance, mapping the
      vendor's busy return to the shared sentinel; every enumeration returns an error when it cannot prove
      completeness rather than partial state.
      Three findings from building it, each resolved by the spec's own posture rather than by preference:
      the vendor's pre-create compute-instance profile record carries **no name and no capability bits**
      (only its versioned forms do, and those are not constructible outside the binding), so a whole-GI
      *variant* cannot be filtered by name the way the GPU-instance side is — more than one whole-GI
      candidate therefore **fails closed with the ids in the error** rather than picking one by id order,
      which the no-computed-ids rule distrusts by construction; the destroy enumerates and removes **every**
      compute instance the GPU instance reports, not only those of the profile it computed, because an
      out-of-band partial compute instance would otherwise leave the teardown permanently busy — the same
      non-convergence F12 exists to prevent, traded for about twenty-seven extra probes on a cold path; and
      the profile probe space is **not cached**, because a cache is mutable shared state whose staleness is
      the exact failure the completeness contract exists to reject. If the probe cost ever matters, the only
      safe unit is the per-card profile inventory, which is immutable while the partition mode is — never
      the live instance set.
      Verify: the linux-container compile, then the remote `make lint`. Note the container command needs a
      non-login shell with an explicit `PATH`: a login shell resets it, `go` then is not found, and the
      wrapper still exits zero — a green run that compiled nothing.

- [x] **T6 · thead Partitioned and Visibility server wiring, the actuator, and partition device-node injection**
      Blocked by: T3, T4
      Owns: `pkg/devicemanager/allocator/thead/deviceplugin.go`,
      `pkg/devicemanager/allocator/thead/deviceplugin_test.go`,
      `pkg/devicemanager/allocator/thead/mig.go`
      Gate: review
      Acceptance: the Partitioned server registers only when the partition family is advertised and not
      disabled; one driver is built and shared with the Partitioned and Visibility servers; the reclaim
      goroutine is gated on the same condition; the actuator reserves one partition per allocated card
      through T4's core and rolls back exactly what this call did, per the per-card reservation outcome —
      destroying only what it created, dropping only the marker it wrote, and leaving a prior allocation's
      marker and instance intact; a partition response carries the shared control nodes, the
      parent card node and both capability nodes, with minors resolved at allocation time from the driver's
      capability tree under the same card ordinal the device-node path uses; a card whose ordinal cannot be
      proven — no recorded minor, or a node whose own kernel minor disagrees with the record — fails the
      allocation before any reservation, rather than being omitted from an otherwise successful response;
      removing any required node in turn fails the allocation and rolls back instead of succeeding with
      fewer nodes.
      Verify: `go test -race ./pkg/devicemanager/allocator/thead/... && make lint`

- [x] **T7 · thead reclaim loop**
      Blocked by: T4, T11
      Why T11: this task now carries F12's reclaim half, and T11 carries the same fix for the vendor being
      mirrored. Landing T11 first makes this a mirror of a decided implementation rather than a second,
      independently invented one — the same prefactor discipline as T0 and T1.
      Owns: `pkg/devicemanager/allocator/thead/mig_reclaim.go`,
      `pkg/devicemanager/allocator/thead/mig_reclaim_test.go`,
      `pkg/devicemanager/allocator/thead/mig.go`,
      `pkg/devicemanager/allocator/thead/mig_test.go`
      Why the last two, which T4 created: F12 grew its "a corrupt path naming no card fails closed on every
      card" clause after T4 was already building, so T4's per-card predicate satisfies the requirement it
      was given and not the one F12 now states. Completing it means widening that predicate where it lives,
      and both vendors must end with one predicate of the same name and the same rule — which also tightens
      the reservation path, exactly as it does on the vendor being mirrored, since both consult it.
      Gate: review
      Acceptance: both debounces are present, the destroy retry is bounded and surfaces an operator-visible
      log at its bound; either read failing skips the pass; the live-claims attribution self-check prevents
      destroying a partition a running Pod still claims; a marker-less orphan is collected only on a fully
      drained card, re-checked under the card lock; a reused instance id is retained, not destroyed; the
      compute instance is destroyed before its GPU instance. F12's reclaim half lands here: a card named by
      a corrupt marker is never treated as drained — in the pass and in the bail-out re-scan — and a corrupt
      marker whose Pod is gone is removed while one whose Pod is alive is kept, so the fail-closed state
      converges. T4 already provides the per-card corrupt-marker predicate and the adoption half.
      Verify: `go test -race ./pkg/devicemanager/allocator/thead/... && make lint`

- [x] **T8 · thead partition visibility responder**
      Blocked by: T4, T6
      Owns: `pkg/devicemanager/allocator/thead/mig_visibility.go`,
      `pkg/devicemanager/allocator/thead/mig_visibility_test.go`
      Gate: review
      Acceptance: the compile-time assertion that the server implements the physical-sliced responder is
      present; the owner's partitions are resolved from the durable marker, proven live, and returned in
      the same card order the owner's own response used; a missing, malformed, wrong-card, unknown-profile,
      dead or id-reused record is an error, never a fallback to the parent card.
      Verify: `go test -race ./pkg/devicemanager/allocator/thead/... && make lint`

- [x] **T9 · Partitioning runbook and documentation cross-references**
      Blocked by: T3, T6
      Owns: `docs/operation/thead-mig.md`, `docs/README.md`, `README.md`,
      `docs/accelerator-requests.md`, `docs/architecture/discovery.md`,
      `docs/architecture/scheduling-chain.md`
      Gate: —
      Acceptance: a new runbook mirroring the NVIDIA page's structure, leading with the prerequisite that
      the node must be free of accelerator workloads before the mode can be changed and with the kernel-log
      signature that names the blocking processes, and stating that a mode change is not noticed until the
      device-manager restarts; the index, the resource-key table and the architecture cross-references name
      the second vendor wherever they currently name only the first.
      Verify: `make lint` plus the documentation skill's index, link and table-of-contents checks

- [x] **T10 · Hardware verification and spec reconciliation**
      Blocked by: T5, T6, T7, T8, T9
      Owns: `specs/2026-08-03-thead-ppu-mig-partitioning.md`
      Gate: review
      Acceptance: the six F9 items are answered on a T-Head PPU host with partition mode enabled, each
      finding is folded into the feature it affects, and the end-to-end success criteria in Goals are
      demonstrated on that host.
      Verify: the project's end-to-end verification on that host, then the spec updated and re-read
      top-to-bottom for consistency
      **Done over two windows.** The first answered the six F9 items with no operator deployed; the second
      deployed this branch and demonstrated the seven end-to-end criteria. Three findings changed the design
      rather than confirming it — the capability tree is keyed by the card ordinal, a passive holder does not
      block an instance destroy, and the whole-card profile could not be created at all until the
      compute-instance probe stopped counting one profile twice. **Two things are recorded as not verified**
      rather than passed: whether the partition mode survives a host reboot, and the real `Instance` path with
      an SSH login (both the partitioned and the whole-card case). The sidecar mechanism itself was proven
      with a hand-written two-container Pod, which exercises the visibility responder but not the `Instance`
      controller's own rendering of it. Deciding to stop there was the user's call.

- [x] **T17 · Publish a thead profile name with the separator, keep the vendor's spelling inside**
      Blocked by: T3, T5, T10
      Owns: `pkg/nodefeature/**`, `pkg/devicemanager/detector/thead/**`, `pkg/deviceplugin/**`,
      `pkg/worker/controllers/worker/**`
      Gate: review
      Acceptance: F15's acceptance, and no other manufacturer's published keys change.
      Verify: `make lint && make test`, plus the node's advertised keys and one admitted request on hardware
      One function converts the vendor's two-number spelling to the separated form and one converts back,
      both applied only where a name crosses the operator's boundary — the published resource key, the
      user-facing ledgers, and the reverse lookup that resolves a requested key. The `Devices` record, the
      markers and every driver-facing name are left alone, so the seam that matches a vendor profile by name
      never sees the converted form. A name that does not match the two-number shape passes through
      unchanged, which is what keeps the conversion from guessing at a future profile naming.
      Tracing the comparisons extended the owned paths beyond the two planned: the published name is
      built into the key inside the one function that defines the key, so every key-building caller is
      covered at once, but two more crossings only became visible against the real call graph. The device
      plugin reads a request back in the vendor's spelling, because everything it does with the name —
      match the record, write the allocation and the marker, call the library — is below the boundary. And
      the admission check converts per card rather than carrying a second spelling on the demand: a
      per-card ledger is keyed by that card's own manufacturer, whereas a demand carries only what the
      user wrote, and a demand-side copy would read as a full card whenever it was left unset.

- [x] **T18 · Delete the memory-slice span's arithmetic fallback, on both vendors**
      Blocked by: T2, T3
      Owns: `pkg/devicemanager/detector/nvidia/**`, `pkg/devicemanager/detector/thead/**`
      Gate: review
      Acceptance: F10's amended acceptance; no detector derives a span from card memory any more, and a
      profile the driver placed nowhere is refused with a reason naming it.
      Verify: `make lint && make test`
      Ordered after the build, on the user's decision, once F10's own reasoning was traced to its end: the
      span is the number a leftover instance's identity is matched by, so keeping a computed second source
      kept a guess in the one place a guess can hand out somebody else's partition — and a wrong non-zero
      guess fails open where a refusal fails closed. Both detectors lose the `cardMemoryMiB` argument they
      only ever used as a denominator, and the rule collapses to one line with no fallback. This is the
      third place parity runs toward the vendor being mirrored rather than away from it.

- [x] **T19 · Make an ownership record's write durable, in one shared helper**
      Blocked by: T6
      Owns: `pkg/utils/osx/**`, `pkg/devicemanager/allocator/**`
      Gate: review
      Acceptance: no allocator hand-rolls the temp-file-and-rename dance, and a record that has been
      written survives an unclean shutdown; every vendor's file mode and directory mode are what they
      were before.
      Verify: `make lint && make test`
      Ordered after the build, on the user's decision, from a question about F12: what actually produces
      the corrupt marker that feature defends against. The answer is mostly an unclean shutdown, because
      the publish was atomic but never durable — a rename is journaled while the data blocks it points at
      are not, so a crash inside the writeback window leaves the new name over a truncated file, and the
      records live on a host path that survives the reboot. Five vendors had each hand-copied the same
      thirty-line dance and all five were missing the same two syncs, so the fix belongs in one place:
      `osx.DurableWrite`, beside the `DurableRemove` that already existed. It deliberately does not create
      the parent directory — the vendors' directory modes differ from `osx.WriteFile`'s, and a helper that
      created directories would decide that on their behalf — so each call site keeps its own `MkdirAll`
      and the only behaviour that changes anywhere is the added durability.

#### T10 execution checklist

Written before the window is scheduled, so the window is spent collecting rather than deciding. Card
ordinals below are written `<N>`; substitute the ones actually being used. Record raw output as it comes
and desensitize it in one pass at the end — never inline, which is how a real identifier survives into a
commit.

**Phase 0 — prove the node is idle, before touching anything.** The mode change is refused while any
process holds the driver, and a process that opens it may hold *every* card's node, so "the card I want
is idle" is not the property to check.
1. Stop the workloads: every accelerator workload on the node, then the operator's own device-manager
   DaemonSet. Record what was stopped, because Phase 4 puts it back.
2. Prove it, and do not skip this: `ls -l /proc/*/fd 2>/dev/null | grep alixpu` must return nothing, and
   `dmesg -T | tail` must show no recent driver-busy lines. The vendor CLI's own output is **not**
   evidence — it reports the card, not the holders.
3. If a holder remains, identify it from the file-descriptor listing rather than guessing. A privileged
   workload that declares no device request is the likely one, and it holds every card.

**Phase 1 — enable the mode.** `ppu-smi -i <N> -mig 1`, then confirm with `ppu-smi -i <N> -q | grep -i
mig`. On failure, capture `dmesg -T | grep -i alixpu` verbatim: the driver names the card, the number of
active processes and one line per holder, and that signature is what F8's runbook documents. A busy
failure here means Phase 0 was incomplete — return to it rather than retrying.
**Rollback:** `ppu-smi -i <N> -mig 0`. The mode is node-state, not operator state, so nothing in the
cluster has to be undone to reverse this step.

**Phase 2 — collect the six F9 answers.** Each item names what to record, not merely what to look at.
1. **Profile records.** For every profile the driver offers: id, name **exactly as reported including any
   prefix and spacing**, slice count, instance count, memory size. Then confirm the name the operator
   publishes is the normalized form of that name. Compare against the shape-derived test fixture and
   record every difference — the fixture asserts a derivation, not a vendor fact.
2. **The whole-GI compute-instance profile.** Every compute-instance profile of a created GPU instance,
   with its slice count, and specifically **how many** span the whole GPU instance. The seam fails closed
   on more than one and names the candidates; if that fires, the finding is that the binding needs a
   versioned pre-create probe, which is a follow-up task and not a decision to take on the host.
3. **Capability node layout and stability.** `cat /proc/driver/alixpu/capabilities/ppu<N>/mig/gi<G>/access`
   and the `ci<C>` one, before and after a create/destroy cycle **and** across a host reboot. Record
   whether a minor is reused, and whether the tree matches the documented shape.
   **First, settle what `<N>` is** — and on the first window this is what reversed the design, so run it
   before anything else on any new product too. A single `ls /proc/driver/alixpu/capabilities/` answers it:
   it needs no partition, touches no card, and is safe on a fully loaded host, because the directory names
   are either the minor set or the ordinal set and on this hardware those differ. **Answered: the ordinal.**
   The tree held exactly `ppu0`…`ppu15` on a 16-card host — a `ppu0` and no `ppu16`, while the minors run
   1…16 — and a live GPU instance created on the card at ordinal 14 appeared under `ppu14/mig/gi0/`, not
   under `ppu15`. Confirm the same way on any product where the two numbers could differ: if a future driver
   ever keys the tree by the minor, a container gets its own card's device node together with a neighbour's
   capability node, which is the one silent mis-isolation this design can still contain.
4. **Destroy under the device-manager's own handle.** Attempt a destroy while the device-manager is
   running and holding the card. Record whether it is refused as busy and how long the refusal persists —
   that is what sizes the bounded retry, which is currently a guess.
5. **The partition identity string**, as the driver reports it for a created partition; and whether the
   container needs the vendor's visible-devices environment variable **in addition to** the device nodes.
   The vendor's own isolation examples inject nodes only, so the expected answer is no; record it either
   way, because injecting it defensively would be as wrong as omitting it.
6. **Placement sets.** Confirm a non-empty placement set for every offered profile, and that the
   whole-card profile's span equals the constant the arithmetic fallback divides by. A profile with no
   placements cannot be admitted, so an empty set is a finding about the product, not about the code.

**Phase 3 — demonstrate the Goals' success criteria, in this order.** Restart the device-manager first:
the re-detect trigger does not include the partitioning mode, so nothing is noticed until it restarts.
1. The node advertises the partition family, its units key, and one key per offered profile.
2. An `InstanceType` for that pool reports a non-empty per-profile remaining ledger.
3. A workload requesting one profile is admitted and starts. Its image must carry the vendor tooling, or
   there is no way to observe from inside what it was given.
4. Inside the container, the vendor tooling shows exactly one partition — neither the parent card nor a
   sibling. Record the device nodes the container actually received.
5. An SSH-enabled Instance's sidecar sees the same partition as its workload container, not the card.
6. Deleting the workload returns the profile to the ledger and destroys the partition.
7. Restart the device-manager **mid-partition-life** and confirm neither a leak nor a double destroy.

**Phase 4 — teardown, as its own mandatory phase rather than a per-step afterthought.**
1. Destroy every partition created, then `ppu-smi -i <N> -mig 0` on every card touched.
2. Restore the device-manager DaemonSet and the workloads recorded in Phase 0, and verify the node's
   whole-card resources are advertised again.
3. Check the node for stale extended resources: the teardown path does not reverse-patch them, so a
   partition key can linger after the mode is off. Patch the reconciler-owned keys away; device-plugin
   keys zero out, and full removal needs a kubelet restart.
4. If the capability the operator recorded looks stale afterwards, the known cause is that it is written
   at detect startup and the re-detect trigger ignores the slicing capability — deleting the `Devices`
   record is the documented workaround, not a new finding.

**Then reconcile this spec:** fold each answer into the feature it affects, update the fixtures whose
values were shape-derived, and re-read the document top to bottom for consistency. A finding that
contradicts a Goal or a Note is reconciled at that statement, not appended as a caveat.

#### What the window answered

Run on a 16-card host, Phases 0, 1, 2 and 4 complete; **Phase 3 was not run** — see below. Card ordinals
and identifiers are omitted deliberately. Each answer is folded into the feature it affects; this list is
the index, not the record.

1. **Profile records — answered.** The product reports exactly two GPU-instance profiles: a half-card
   profile with two instances of four memory slices, and a whole-card profile with one instance of eight.
   Three things the code depends on are now facts rather than assumptions: the driver's display names carry
   a `MIG ` prefix and a space, so the normalization that strips them is load-bearing rather than defensive;
   the profile ids are **sparse** (the half-card profile's id is higher than the whole-card profile's), which
   is why nothing may index profiles by position and why the seam probes ids and matches names; and the two
   profiles are mutually exclusive in the obvious way — creating one half-card instance takes the whole-card
   profile to zero free. Confirming that the operator's published key equals the normalized name needs the
   operator running, so it moves to Phase 3.
2. **The whole-GI compute-instance profile — answered, then corrected by the second window.** The driver
   offers exactly **one** compute-instance profile spanning a GPU instance and marks it as its default, for
   both GPU-instance profiles — the vendor CLI lists one entry for each. What the first window got wrong was
   the conclusion drawn from it: that the seam's fail-closed branch therefore cannot fire. It fired on the
   **whole-card** profile and refused every such allocation, because the probe walks the profile *index*
   space and this driver answers **several indices with the same profile**. Two indices returned the one
   default profile, the seam counted two candidates, and it failed closed on an ambiguity that did not
   exist. The half-card profile happened not to alias, which is why the first window — measured on a
   half-card instance only — saw a single candidate. The fix is to skip a candidate whose id was already
   seen; the versioned name-carrying probe that branch would otherwise have forced is still not needed. The
   lesson for a future product is that this item must be measured on **every** offered profile, not one.
3. **Capability node layout — answered, and it reversed the design.** The tree is keyed by the card
   **ordinal**; see the card-addressing note in Notes for the measurement, which is the load-bearing finding
   of the whole window. Layout as documented: `…/ppu<ordinal>/mig/gi<G>/access` and `…/gi<G>/ci<C>/access`,
   each carrying `DeviceFileMinor`, `DeviceFileMode` (`292`, i.e. `0444`) and `DeviceFileModify`. The driver
   creates `/dev/alixpu-caps/alixpu-cap<minor>` when the instance is created, and the minors are large and
   unrelated to the instance ids. Two behaviours worth having in writing: the procfs entries **are** removed
   with the instance, but the `/dev` capability nodes are **not**, so they accumulate — which is why reading
   the live procfs record, rather than trusting a node's existence, is what keeps a stale node from being
   mistaken for a live partition. And the minors proved **deterministic**, reused when the same instance is
   recreated. Stability across a **host reboot was not tested**: it would mean rebooting a host in use, and
   it is not load-bearing, because the code reads the minor per allocation and caches nothing.
4. **Destroy under a holder — answered, and it removes the premise the retry was sized against.** A process
   merely holding the card's device nodes does **not** block an instance destroy: with the device manager
   holding all sixteen card nodes plus the control nodes, a compute-instance destroy succeeded on the first
   attempt. What blocks a destroy is a condition rather than a delay — a GPU instance with a live compute
   instance under it is refused with a non-zero exit until that instance is gone. So destroying compute
   instances first is *required*, not merely tidy, and the "bounded retry sized above the kubelet retry
   window" framing no longer describes a real failure mode. Whether an active compute *process* on a
   partition blocks its destroy is untested, and is the one case that could still justify a retry window.
5. **The partition identity string — answered.** The driver reports a `MIG`-prefixed UUID for a created
   partition, the same shape NVIDIA uses, listed alongside the parent card. The second half — whether the
   container needs the vendor's visible-devices environment variable in addition to the device nodes —
   requires a real container, so it moves to Phase 3.
6. **Placement sets — answered, twice.** Both profiles report a non-empty placement set, and the whole-card
   profile's span equals the constant the arithmetic fallback divides by. More importantly the placement list
   is **occupancy-independent**, exactly as both vendors' headers document: it was byte-identical before any
   instance existed, with one instance live, and with the card carved to capacity — and it keeps listing
   placements for a profile that can no longer be created at all. That confirms known item (d) rather than
   leaving it an inference, and it confirms the refusal to treat a full live-instance buffer as truncation:
   a full buffer is full occupancy.

**Also observed, and folded in as operational facts rather than answers to a question.** The mode-change
flag is a top-level device flag, not a `mig` subcommand flag, so the runbook's command was wrong and is
corrected. The driver's busy log names the card by ordinal, reports a process count that can exceed the
number of holder lines it prints, and advises a reset that is not in fact needed — an idle card flips in
place. `-dgi` without a `-gi` scope destroys **every** instance on the card. And the operator's own worker
Pod was seen opening every card node briefly at startup, so a mode change attempted seconds after a control
plane restart can fail on a holder already leaving.

**Phase 3 ran in a second window, with this branch deployed, and all seven criteria passed.** Recorded here
because the criteria are the release gate, not a checklist:

1. The node advertised the partition family, the units key and one key per offered profile, and the two
   partitioned cards left both the whole-card and the shared pools — sixteen cards became fourteen and the
   shared count fell with them, so a card is in exactly one family at a time.
2. The pool's `InstanceType` reported a per-profile remaining ledger, and the per-card records carried one
   too: only the two mode-enabled cards had one, the other fourteen had none.
3. A Pod requesting one profile was admitted through Kueue — a Workload was created, quota reserved, and the
   operator's own admission check passed with a reason naming the profile's free placement — and it started.
   The same request without a queue label also started, so the device-plugin path does not depend on Kueue.
4. Inside the container the vendor tooling showed **exactly one** partition and neither the parent card's
   other partition nor a sibling card. The device set was the shared control node, the vendor's second
   control node on its own major, the card node **named by the card ordinal**, and the two capability nodes
   of that partition alone. The neighbouring card's node was absent, which is the mis-isolation this design
   exists to prevent, observed on real hardware rather than argued.
5. A second container in the same Pod, asking only for a visibility token, saw **the same partition** — same
   identity string, same capability minors — and not the card. This is the SSH sidecar's mechanism; the
   `Instance` controller's own rendering of it is what stayed untested.
6. Deleting the Pod returned the profile to the ledger and destroyed the partition, and the ledger came back
   to its starting values exactly.
7. Restarting the device manager mid-partition-life left the partition record byte-identical, the Pod
   running, and the ledger unchanged, with no destroy attempted.

**The four questions Phase 3 owed.** The published key is `<base>.partitioned.<kind>-<normalized profile>`,
the normalized name being the driver's own last name field — confirmed against the live node. The container
needs **no** vendor visible-devices environment variable: device nodes alone were sufficient, and nothing of
the sort was injected. An active compute process **does** block a destroy, with a distinct vendor message and
a non-zero exit, and the driver names the busy compute instance in its log — which restores a purpose for the
bounded busy-destroy retry that finding 4 had taken away: the blocking condition is a live *process* on the
partition, not a co-resident holder of the card. Measured on the normal path: with a compute process running,
the partition was destroyed about three quarters of a minute after the Pod object disappeared, the reclaim
debounce absorbing the gap, with no leak. Whether the mode survives a **host reboot** is still not tested.

**Phase 4 completed.** Every instance destroyed, the mode returned to disabled on both cards touched, and
the two paused DaemonSets and the scaled-down control plane restored to exactly their recorded state, each
verified rather than assumed. The stale `/dev` capability nodes were deliberately left in place, per item 3.
Item 3 of the Phase 4 list — stale extended resources — did not arise, because the deployed release predates
this feature and never advertised a partition key.

**Checkpoints.** After T1+T2+T11 the foundations are green and the seam has real symbols to compile against.
After T3+T4 the capability and the pure core are green with no hardware. After T6+T7+T8 the feature is
complete and locally verified. T10 is the hardware gate.

**Shipped as.** The task ids above are the plan's own order — a DAG, and the order the build ran in. The
branch is published in a different order, by subsystem, so that each commit is one subject a reviewer can
read whole: a task's review and hardware findings are folded into the commit they correct rather than
following it, and the whole of this file lands in one commit rather than a paragraph in each. Task numbering
is left alone, because every commit's trailer and every `Blocked by:` above reads by it. The two orders map
as follows.

| Commit | Tasks |
| --- | --- |
| `chore(lint)`: report every occurrence, and sort the imports no check ever formatted | — |
| `refactor(devicemanager)`: wrap vendor return codes so `errors.Is` reaches them | T15 findings |
| `build(binding)`: refresh the vendor hgml surface and wrap its whole mig api | T2, T13 findings |
| `refactor(devicemanager)`: give the nvidia mig reclaim loop its own file | T0 |
| `fix(devicemanager)`: stop a corrupt mig marker from costing a partition its owner | T11, T14 findings |
| `fix(devicemanager)`: publish only a mig profile the driver fully described | T1, T12 |
| `feat(devicemanager)`: add the thead hardware-partitioning core and its reclaim loop | T4, T7, T14 findings |
| `feat(devicemanager)`: drive thead partitions through the vendor library | T5, T13 findings |
| `feat(devicemanager)`: report thead partition profiles and declare the capability | T3 |
| `feat(devicemanager)`: serve thead partitions and name them for a sidecar | T6, T8 |
| `fix(devicemanager)`: address a thead card by its ordinal, proven with the recorded minor | T10, T14, T15 findings |
| `docs`: add the thead partitioning runbook and name the second vendor | T9 |
| `feat(nodefeature)`: publish a thead partition profile name with the separator | T17 |
| `refactor(devicemanager)`: read a mig profile's span only from the driver's placements | T18 |
| `fix(devicemanager)`: publish an allocator's ownership record durably, not just atomically | T19 |
| `docs(spec)`: record the plan, both hardware windows and what shipped | T10, and this file |

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/devicemanager/allocator/thead` and `pkg/devicemanager/detector/thead` have no test files at all
  today. T4 creates the allocator package's first test, and with it the shared harness the later tasks
  assert through: a concurrency-safe fake driver with error injection and seeded live state, plus a
  temporary marker root. T6, T7 and T8 depend on that harness existing, not on new base test work.
- Two pre-existing test files this plan edits are additive only:
  `pkg/devicemanager/detector/nvidia/mig_profile_test.go` (T1) and
  `pkg/devicemanager/allocator/nvidia/mig_test.go` (T11). Every case already in them must pass
  **unchanged** — that requirement is what bounds F10's and F12's blast radius on a shipped vendor.
- T0 edits no test file at all. It is a pure move of package-private symbols, so the NVIDIA allocator
  package's four existing test files are its verification: if they pass untouched, the move changed
  nothing. `mig_reclaim_test.go` already existed against the file T0 creates.
- No changes are required to the shared layers' tests: `pkg/device`, `pkg/deviceplugin`,
  `pkg/worker/controllers/worker` and `pkg/worker/webhooks/worker` are reused as-is, and
  `pkg/worker/kuberess`'s chart-versus-registry test already covers the T3 registry edit.

#### Unit tests

Baselines measured on 2026-08-03 with `go test -cover`:

- `pkg/devicemanager/detector/nvidia`: `2026-08-03` - `15.7%` → `20.0%` after T1's four derivation cases,
  then `19.4%` after T12. The dip is expected and is not a coverage regression: T12 deletes tested pure
  logic (a name synthesis and an id-range branch it orphans) and adds three statements at the hardware
  edge that no unit test can reach, because the device handle is a concrete cgo struct. The *decision* it
  adds is fully covered; only the emission is not. Holding the ratio would mean testing the cgo seam.
  `deriveSlicedProfiles` itself is at 95.8%.
- `pkg/devicemanager/detector/thead`: `2026-08-03` - `0.0%` → `32.4%`, under the 40% this plan targeted.
  Recorded rather than met. Every *pure* function T3 adds is covered (its derivation at 94.3%, the rest at
  100%). The shortfall is the hardware surface, which is **mostly but not entirely pre-existing**: the two
  detect/monitor loops and the stringify helpers were already there, and T3 also adds one uncovered
  function of its own — the probe loop that drives the vendor library, at 0%. All of them take a concrete
  cgo device handle and cannot be faked, which is precisely why every decision they make was pushed into
  the pure functions that are covered. Reaching 40% would have meant testing the pre-existing helpers to
  move a number, which this project's conventions rule out. The target was set before that surface was
  measured.
- `pkg/devicemanager/allocator/thead`: `2026-08-03` - `0.0%` → `87.7%`, against a target of ≥ 75%
  (T4/T6/T7/T8, sized against the 81.6% the mirrored NVIDIA package achieved with the same harness shape).
  The last 2.7 points came from F14, whose whole-card responder had no test of its own before.
- `pkg/devicemanager/allocator/nvidia`: `2026-08-03` - `81.6%` → `83.2%` (T11 adds adoption cases to an
  already well-covered package; T0 moves code without changing which statements run)
- `pkg/nodefeature`: `2026-08-03` - `85.0%` → unchanged or better (T3 adds a registry row covered by the
  existing table tests)
- `pkg/device`: `2026-08-03` - `82.2%` → unchanged (not touched)

Scenarios the mirrored NVIDIA suite does **not** cover and which the hardenings therefore require:

- An unmarked leftover partition whose geometry matches but whose raw profile differs — including a
  media or graphics variant — must not be adopted (T4).
- An unmarked leftover on a card that also carries a corrupt marker must not be adopted, while a fresh
  create on that card and an adoption on a sibling card must both still succeed (T4 and T11 — the one
  scenario asserted twice, once per vendor, because it is a defect fixed in both).
- A card named by a corrupt marker must not read as drained, so the orphan collector leaves its
  partitions alone even when the annotation claim view has not caught up — the case where the defect
  destroys a running Pod's partition (T7 and T11).
- A corrupt marker whose Pod is gone must be removed, and one whose Pod is still alive must be kept, so
  the previous two refusals converge instead of darkening the card permanently (T7 and T11).
- An enumeration that fails at each step in turn — profile probe, handle, identity, instance list — must
  produce an error, not partial state that allocation or reclaim then consumes (T4 via the fake driver,
  T5 in the seam).
- A partition destroyed and recreated with the same instance id between the reclaim snapshot and the
  destroy: the final destroy must reject the replacement's identity (T5, T7).
- Each required device node removed in turn — shared control, parent card, GPU-instance capability,
  compute-instance capability — must fail the allocation and roll back; likewise a path that exists but
  is not a character device, and a capability minor reassigned between visibility resolution and
  response construction (T6, T8).
- A card skipped mid-enumeration, so the post-filter ordinal no longer matches the driver's: the
  invariant must reject it rather than address a neighbour (T6). It is an allocation-time guard only:
  enforcing it in the detector would drop whole cards from detection, changing behaviour for a
  non-partitioned card, which no part of this spec asks for.
- Normalization cases: vendor prefix, whitespace, case, invalid characters, over-long names, a name that
  normalizes to empty, and two raw names colliding on one normalized name (T3).
- Cards in one group exposing the same normalized profile with differing memory, count, placements or
  geometry: rejected rather than aggregated (T3).
- Reservation and reclaim under concurrency, with `-race`: two allocations on one card, and a reclaim
  pass racing an allocation on the same card (T4, T7).

#### Integration tests

- The device-plugin server's Partitioned path against the thead responder with a fake driver: an Allocate
  carrying a partition profile reserves a card, actuates a partition, publishes the placement upward, and
  rolls back when the post-actuation annotation patch fails. **Not built, and recorded as such.** Driving
  it through the existing shared server needs fake reconciler and client fixtures that live in
  `pkg/deviceplugin`, which no task here owns and which the mirrored vendor has no per-vendor equivalent
  of either. The vendor-side half is covered by T6 calling the actuator directly; the shared server's own
  path is covered by that package's existing tests. Building it would mean adding fixtures to an ask-first
  shared layer to test code this spec does not change.
- Two pending Pods requesting the same coarse partition quantity with different profiles, with the RPC
  order reversed: each response must carry its own profile's nodes and its own marker. This is the shared
  layer's attribution path, which this spec does not change — the test documents the boundary rather than
  asserting a fix, and a failure here is a finding against the shared layer, not against this feature.
- Detector-to-capability flow: a fake vendor library reporting a partition-enabled card produces a
  `Devices` capability whose profiles, spans and placements the allocator's geometry lookup can consume
  (T3 + T4 seam boundary).
- Concrete test names are added after the implementing changes merge.

#### e2e tests

- On a Kubernetes cluster with a T-Head PPU node whose cards have partition mode enabled: the node
  advertises the partition family, its units key and one key per offered profile; an `InstanceType` for
  that pool reports a non-empty per-profile remaining ledger; a workload requesting one profile is
  admitted and starts; inside the container the vendor's tooling shows exactly one partition and neither
  the parent card nor a sibling; deleting the workload returns the profile to the ledger and destroys the
  partition; and a device-manager restart in the middle of a partition's life neither leaks nor
  double-destroys it (T10).
- Also on hardware, because no unit test can reach them: a destroy attempted while the device-manager
  holds a handle on the card, and capability-minor stability across partition create/destroy and across a
  host reboot (T10, F9 items 3 and 4).
- This e2e run is gated on the node being free of accelerator workloads for the mode change, which is why
  it is the last task rather than the first.

## Alternatives

- **Run this vendor's header through the C99 binding generator, as the other `bool`-using vendors do.**
  The obvious fix for a header the plain generator cannot parse, and it was chosen before being measured.
  Measured, it fails three ways: it does not read `bool` natively either, so it still needs a line written
  into the header and buys nothing on that count; it emits sixty-three constant/type name collisions that
  do not compile; and it is **not deterministic** — three enum-aliasing macros receive an address-shaped
  value that changes on every run, so `make generate` can never satisfy "a second run produces no further
  change" for a committed artifact. It would also correct four affinity signatures the pinned generator
  gets wrong, at the price of breaking an exported hand-written method. Rejected: a generated file that is
  committed must be reproducible, and that is not negotiable against a signature fix.
- **Hand-edit the vendor header to declare the missing type.** Verified to work, and the smallest possible
  change, but the declaration then lives in the one file that is replaced wholesale on the next vendor
  header drop, where its loss reappears as an unexplained parse error. Rejected in favour of teaching the
  generator, which owns the working copy and survives the refresh.
- **Shell out to the vendor CLI instead of binding the library.** The CLI mirrors its NVIDIA
  counterpart and is easy to read, but parsing human-formatted tables is brittle, the CLI is not
  guaranteed present in the device-manager image, and the repository already binds the vendor library
  for detection. Rejected.
- **Port the NVIDIA slice-count-to-profile-id table with T-Head values.** A table keyed by slice count
  would be shorter than a probe, but the vendor's own documentation shows ids that do not follow the
  slice-count numbering, so the table would encode a coincidence of one product. Rejected in favour of
  discovery.
- **Have the operator enable MIG mode when a pool asks for partitions.** Attractive operationally, but
  the mode change requires the node to be free of accelerator workloads — including the device-manager
  itself — and the vendor documents it as needing a restart to take effect. It would turn a reconcile
  into a node-disrupting action. Rejected; the NVIDIA precedent of an administrator prerequisite holds.
- **Model T-Head partitioning as a new capability family rather than reusing the partitioned family.**
  The vendor's model — instances carved from a card at fixed placements, one profile per request — is
  the same model the partitioned family already expresses. A second family would duplicate the
  scheduling chain for no semantic gain. Rejected.
- **Inject only the vendor's visible-devices environment variable, as the NVIDIA path does.** There is
  no container-runtime hook to act on it, so the container would not receive the partition's device
  nodes. Rejected; device specifications are required, with the environment variable as a possible
  addition pending F9 item 5.

## Open Questions

None outstanding.

The card-index question this spec opened with — whether the partition path should address a card by the
detector's accelerator index or by its recorded physical index — is settled by hardware observation and
recorded in Notes and F8: **the index**, for both the device node and the procfs capability tree, with the
recorded minor demoted to proving that the index still reaches the card the detector measured. It took
three answers to get there, and the code deliberately encodes none of the arithmetic that made the first
two look convincing. The remaining hardware-dependent unknowns are not open questions but release gates:
F9's items are now answered except where the first window deferred them, and what is left is enumerated in
T10's progress note.
