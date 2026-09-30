# Spec: ModelArtifact Expected Digest

Status: Shipped
Shipped on: 2026-09-29, as the pull request this commit lands with. T1–T6 delivered; the plan-gate
adjudications (the claim anchor excluded by the user, digest-as-capability accepted and documented,
the #693 migration contract) are recorded in Open Questions and Alternatives.
Type: Feature

## Summary

Add an optional `expectedDigest` to the `ModelArtifact` spec: the manifest digest the user asserts
this artifact resolves to. The manifest format v1 was published precisely so that such an assertion
could later be compared with it; this spec opens that comparison. On a hub source the resolution
proceeds as today and a mismatch refuses it with a new `DigestMismatch` reason; the periodic
revalidation re-computes the manifest instead of probing one file, so a hub that stops serving the
asserted content is caught too. When the hub cannot be reached at all, an anchored hub artifact
resolves to its anchor without contacting the hub (`digestSource: Expected`) and delivers from a
peer that holds the published tree — an air-gapped cluster's working delivery path. The same anchor
is the acceptance contract for the deferred legacy-cache migration
([#693](https://github.com/gpustack/gpustack-operator/issues/693)): whatever form the future import
takes, it may publish only trees whose computed manifest digest equals the artifact's anchor; the
tool itself is out of scope this cycle and the issue stays open.

## Motivation

### Goals

`expectedDigest` is an identity anchor on top of the published `gpustack-manifest v1` format. It
changes neither the format nor the digest algorithm; it asserts which manifest digest this artifact
is, and the system refuses anything else. Two scenarios each get an end-to-end answer:

1. **Hub sources assert what they resolve to.** A user who resolved the artifact once — on another
   cluster, from a mirror, in a change ticket — pins the digest they saw. Resolution still talks to
   the hub; a hub that answers with different content than the anchor asserts fails loudly
   (`Resolved=False`, `DigestMismatch`, both digests in the message) instead of delivering content
   the user never asserted. The revalidation upgrades from a single-file `HEAD` to a full
   manifest re-computation, so the same refusal catches a source that drifts after resolution.
2. **A pre-resolved artifact works without the hub.** An air-gapped cluster cannot resolve: today
   the artifact never resolves and nothing delivers. With an anchor, the controller writes the
   identity from the spec alone (`digestSource: Expected`) after the hub's absence is confirmed,
   and delivery draws its bytes from a peer that holds them; the spec states exactly which
   sub-cases remain unreachable and does not pretend otherwise.

The anchor is also the contract the deferred migration (#693) will be held to: verifying a
directory of unknown-provenance bytes against a manifest digest before anything is published is the
one operation any future import must perform, and this spec publishes the format and the pipeline
it runs through. The tool itself is another cycle's work.

### Non-Goals

- **No change to the manifest format v1, the digest algorithms, or the canonical bytes.** The
  anchor is an assertion over the published format, not a format extension. Any change to the
  format would change digests and strand every anchor.
- **No anchor on claim or image sources.** A claim's content is whatever the provisioner
  materialized at mount time — like an image volume, the consumer only knows what it got when it
  mounts — and a claim's identity is the claim itself, as the documentation has always said; the
  user confirms their claim's content with their own tooling. An image is already pinned by its
  reference's OCI digest. Admission refuses `expectedDigest` on both, so no claim path changes in
  any way: same delivery, same placement, same status, no bridge, no Verified-for-claims.
- **No cross-source digest deduplication.** Digests stay incomparable across hubs (non-LFS files
  hash as `gitsha1` on Hugging Face, `sha256` on ModelScope). The anchor says nothing about content
  equivalence across sources.
- **No engine delivery for anchored artifacts.** An engine downloads by repository+revision and
  cannot anchor-verify what it fetched (an `Expected`-identity artifact additionally has no commit
  to pin). Anchored artifacts deliver through the node only.
- **No automatic upgrade of an anchored artifact to a hub-resolved one.** Once an artifact resolves
  to its anchor (`digestSource: Expected`) it never contacts the hub — not for resolution, not for
  revalidation, not for the token probe. A user who wants the hub-verified form recreates the
  artifact while connected.
- **No anchor-less adoption.** A hub artifact without an anchor resolves and revalidates exactly
  as today; its status, chain and delivery are unchanged.
- **No `Verified` condition.** Its only novel scenario was the claim anchor; for hub sources
  `status.nodes.ready` already answers whether any node holds a published copy. The condition
  stays deferred, as the format's design note left it.
- **No legacy-cache migration tool this cycle.** #693 is deferred by adjudication (2026-09-29):
  the issue stays open and is arranged separately. This spec publishes only the acceptance anchor
  such a migration must satisfy — a future import, whatever its form, verifies a candidate tree's
  computed manifest digest against `expectedDigest` and publishes only through the store's own
  pipeline — and designs no tool body: no CLI, no Job, no import RPC, no `NodeModelStore` section.
- **No DeliveryClass or external provider work** (the reserved S8), and no change to the CSI volume
  contract.

## Proposal

### The API (additive, one generation run)

```yaml
spec:
  source: {...}                          # unchanged, exactly one member
  expectedDigest: sha256:669ed7b1…       # optional; "sha256:" + 64 lowercase hex; hub sources only
status:
  resolved:
    revision: 7ae557604adf67be50417f59c2c2f167def9a775   # absent for an Expected-digestSource artifact
    manifestDigest: sha256:669ed7b1…
    digestSource: Hub                    # Hub (the hub's listing) | Expected (the spec's anchor)
    fileCount: 10                        # absent for an Expected-digestSource artifact
    sizeBytes: 999604126
  nodes: {...}
  conditions:                            # unchanged: Resolved, Degraded
    - {type: Resolved, status: "True", reason: Resolved}
    - {type: Degraded, status: "False", reason: Healthy}
```

- `spec.expectedDigest` is optional, validated as `sha256:` + 64 lowercase hex, and immutable with
  the rest of the spec (the existing whole-spec identity rule already covers it: the webhook's
  deep-equality check sees the new field like any other).
- `status.resolved.digestSource` is written whenever `manifestDigest` is: `Hub` when the digest came
  from a hub listing, `Expected` when it came from the spec's anchor. It makes the two kinds of
  identity visible instead of inferable, which matters to everything downstream that decides
  whether a revision exists.
- The `status.nodes` aggregation, the conditions and every claim/image behavior are untouched.

### Admission

- `expectedDigest` is accepted on hub sources and **refused on claim and image sources**: a claim's
  content is whatever the claim holds at mount time (dynamically provisioned claims differ per
  provisioning — the user confirms it, not the operator), and an image is already pinned by its
  reference's OCI digest; an anchor on either would invite a field that can never say anything.
  The refusal messages follow the existing image-source refusal's shape.
- The shape rule is the image reference's digest rule reused: `^sha256:[a-f0-9]{64}$`.
- Source immutability is unchanged; the spec's deep-equality now covers the new field. An update
  that changes only `expectedDigest` is refused with the identity message.

### Resolution (controller)

**Hub source with an anchor.** Resolution is today's: resolve the revision to a commit, list the
tree, canonicalize, digest. Then compare: equal to `expectedDigest` → `Resolved=True`,
`digestSource: Hub`, exactly as an unanchored artifact; unequal → `Resolved=False` with the new
reason `DigestMismatch` and a message carrying both digests ("expected …, the hub resolved …").
`DigestMismatch` joins the revoking reasons, so it rides the existing staircase: retried on the
refused cadence, because the fix is a corrected anchor — a new artifact, since the spec is
immutable.

**Revalidation with an anchor.** The anchor makes the periodic check stronger: instead of one
`HEAD` of a file, the controller re-lists the tree at the resolved commit, recomputes the digest,
and compares it with the anchor. A hub (or mirror endpoint) that now serves different content at the
same commit fails the same `DigestMismatch` staircase — first refusal sets `Degraded`, the confirmed
refusal sets `Resolved=False`. The cost is one listing per interval for anchored artifacts only;
unanchored artifacts keep the two-request check.

**Hub source, anchor, no hub.** If the resolution fails with `SourceUnavailable` **on the
confirmed cadence** — the refused staircase's second pass, one minute after the first, the same
"a single refused request is not yet a verdict" discipline the revocation staircase uses — and the
artifact carries an anchor, the controller writes the identity from the spec:
`resolved{manifestDigest: expectedDigest, digestSource: Expected}`, `Resolved=True`, no revision,
no `fileCount`/`sizeBytes`. One transient blip at creation therefore does not freeze the
artifact's identity; a genuinely hub-less environment waits one extra minute. Revalidation does
nothing for such an artifact — there is no commit to probe and no hub assumed to exist, including
the token probe (no `ValidToken` call is made on an anchored pass). The state is visible
(`digestSource: Expected`, `revision` absent), never inferred after the fact. A refusal that is
*not* `SourceUnavailable` (`AccessDenied`, `RevisionNotFound`, `DigestMismatch` from a reachable
hub) never falls back: a hub that answered is a hub whose answer decides.

### Node delivery (plugin)

- **Renderer.** An anchored hub artifact renders Node delivery as hub artifacts do. Under Engine
  delivery it is blocked with a new `WeightsReady` reason `AnchorNeedsNodeDelivery`, whose message
  names the honest reason: the engine downloads by repository and revision and cannot anchor-verify
  what it fetched (for an `Expected`-identity artifact there is additionally no commit to pin).
  The mount rule is unchanged — a claim source is still not a hub artifact, and no claim carries an
  anchor anymore.
- **Source order.** A hub-identity artifact's chain is today's: peers first as a byte source, then
  the hub listing + download — the listing carries the manifest and is anchor-checked before any
  byte moves. An `Expected`-identity artifact has no commit to list at, so its chain is **peers
  only**; no peer holding the tree is a loud failure whose message names the missing source ("no
  node holds sha256:<digest> and the artifact is anchored without a hub"), not a hub error dressed
  up as an integrity mismatch. An unanchored artifact keeps today's chain and today's behavior in
  full.

### The migration contract (#693, deferred)

The legacy-cache migration is out of scope this cycle (adjudicated 2026-09-29; the issue stays
open). This spec fixes the interface any future import is held to:

- The import verifies the candidate tree against the artifact's `expectedDigest` — computing the
  canonical manifest from the actual bytes and comparing digests — before anything is published.
- The import publishes only through the node store's own pipeline (verify, seal, one rename), so
  the single-writer invariant and the nothing-unverified-in-`published/` rule hold whatever the
  import's trigger is.
- The import is lazy and per artifact, never a bulk copy; and it is a one-shot import of files at
  rest, never an online migration of a running legacy install.

Everything the future tool needs — the published format and the pipeline — exists today; the
anchor gives it the verdict to publish by.

### Free behaviors

Everything downstream keys on the manifest digest and needs no new mechanism once an anchored hub
artifact resolves: **delivery** (the mount is the same bind of a published tree), **peer sync**,
**progress aggregation** (`status.nodes` counts the digest), **prefetch** (a `ModelPrefetch` warms
the digest through the existing hub warm-up path), **placement preference**, and the **KV reuse
identity** (already keyed on `status.resolved.manifestDigest`). Each is pinned by a test that
proves the mechanism unchanged (AC1, AC5); none is code.

### Documentation

- `docs/modules/model-delivery/artifact.md`: the `expectedDigest` field in the resource example and its rules
  (shape, immutability, hub sources only — the claim and image refusals and why); `digestSource` in
  the status section; the `DigestMismatch` row in the reason table; the anchored-resolution states
  (hub-verified and Expected) and what revalidation does for each; the `AnchorNeedsNodeDelivery`
  row in the delivery table; the air-gapped matrix (an `Expected` artifact delivers from peers; no
  peer and no hub is a loud failure); the **shipped access model for `Expected` identities** —
  digest knowledge is the capability, digests being cluster-visible on `NodeModelStore` status,
  integrity unaffected, hub-verified identities unchanged — per the plan-gate adjudication; the
  migration-contract paragraph (#693 deferred, the anchor
  as the acceptance contract); the **downgrade note** (in a downgrade window the old webhook cannot
  see `expectedDigest`, so its immutability is unenforced and `digestSource` is dropped by old
  workers; re-upgrading recomputes it).
- `docs/modules/model-delivery/node-store.md`: the `Expected`-identity chain (peers only) and the no-source
  failure message.
- `docs/README.md` and any cross-references follow the docs skill's routing; the reason tables and
  the troubleshooting entries gain the new reasons. `docs/reference/settings.md` is untouched: this spec adds
  no Setting.

### Version floors

Unchanged: Kubernetes 1.29 for the feature path, 1.23 install. The spec adds no Kubernetes
capability requirement: one optional spec field, one optional status field, no new feature gate,
no new admission mechanism, no new component.

## User Stories

### Story 1

As a platform operator, I resolved a model on a connected workstation and now create its
`ModelArtifact` in the cluster with `expectedDigest` set to the digest I saw. If the cluster's hub
(or mirror) serves those bytes, the artifact resolves; if anything upstream drifted, the artifact
refuses with both digests in the message instead of delivering content I did not assert — and the
periodic check keeps asserting, so a mirror that drifts later is caught too.

### Story 2

As an operator of an air-gapped cluster, I create a hub artifact with the anchor I resolved before
disconnecting; after the hub's absence is confirmed the artifact resolves to its anchor without
the hub, the one node I seeded while connected serves every other node over peer sync, and no byte
moves that the digest has not verified.

## Core Features & Acceptance Criteria

- **AC1 — Hub resolution asserts the anchor (match).** A hub artifact with `expectedDigest` equal
  to the resolved digest writes `Resolved=True` (`digestSource: Hub`), the same revision, digest,
  counts and delivery as an unanchored artifact; every digest-keyed behavior (peers, nodes,
  prefetch, preference, KV identity) sees nothing new — pinned by tests that prove it.
- **AC2 — Hub resolution refuses a mismatch (red then green).** A fake hub whose listing digests to
  something other than `expectedDigest` produces `Resolved=False`, reason `DigestMismatch`, message
  carrying the expected and the actual digest, riding the revoking staircase. Shown red by
  neutering the comparison (accepting any digest), then green again.
- **AC3 — Revalidation catches post-resolution drift (red then green).** With an anchor set, the
  revalidation pass re-lists and re-compares: a fake hub that answers the first resolution with the
  anchored tree and a later revalidation with a different tree walks the staircase
  (`Degraded`, then `Resolved=False`, `DigestMismatch`). Shown red by reducing the anchored
  revalidation to the single-file `HEAD`, then green. An unanchored artifact's revalidation stays
  the two-request check (request count asserted).
- **AC4 — Anchored identity without a hub.** A hub artifact whose resolution fails
  `SourceUnavailable` twice (the confirmed cadence) with an anchor set writes
  `resolved{manifestDigest: anchor, digestSource: Expected}`, `Resolved=True`, no revision or
  counts; a single blip does not fall back; no hub request of any kind — listing, revalidation, or
  token probe — is made on any later pass (asserted against the fake hub's request log); a hub
  refusal that is not `SourceUnavailable` does not fall back.
- **AC5 — Anchored delivery.** An anchored hub artifact under Engine delivery is blocked with
  `AnchorNeedsNodeDelivery` and creates no Pods. With `Expected` identity on a hub-less cluster: a
  node holding the published tree (seeded while connected) serves a second node over peer sync —
  the second node materializes and mounts with zero hub bytes; with no peer holding the tree, the
  mount fails loudly naming the missing source. A hub-identity artifact's chain is unchanged
  (hub listing anchor-checked before bytes).
- **AC6 — Admission.** `expectedDigest` is accepted on hub sources with the exact shape
  (`sha256:` + 64 lowercase hex), refused on claim and image sources each with its reason, refused
  malformed, and immutable (an update changing only it is refused with the identity message). All
  pre-existing cases stay green.
- **AC7 — Documentation.** The new reasons (`DigestMismatch`, `AnchorNeedsNodeDelivery`) are in
  the docs' reason tables and troubleshooting; `digestSource`, the anchored-resolution states, the
  air-gapped matrix, the migration-contract paragraph and the downgrade note exist as described
  above.

## Notes / Constraints / Caveats

- **The anchor never becomes authorization — with one declared, adjudicated exception.** For
  **hub-verified** identities, nothing changes: a namespace consumes content only through its own
  artifact, resolved with its own credential. For an **`Expected`** identity, no credential
  participates: an artifact whose anchor names a digest already published on a node can mount that
  tree, so digest knowledge becomes the capability. **Adjudicated (plan gate, 2026-09-29):
  accepted and documented** — integrity is never at risk, because only anchor-named, verified
  bytes ever enter `published/` or a mount; what relaxes is the secrecy of content whose digest
  leaked, and private content must not rely on digest secrecy (digests are visible cluster-wide on
  `NodeModelStore` status, a cluster-scoped resource); without the anchor the air-gapped scenario
  cannot exist at all. The docs state the shipped model and this premise; hub-verified identities
  keep today's credential-resolved path. Should the user overturn this later, the `Expected`
  cross-namespace sharing surface comes back for re-adjudication.
- **The manifest is still not stored in the artifact.** An anchored artifact's manifest lives where
  manifests live: recomputed from the source, or served by a peer's published listing. The
  controller holds only the digest.
- **`make generate` runs for the API additions** (`ModelArtifact.spec.expectedDigest`,
  `ModelArtifactResolved.digestSource`), in the private gen tree per the task standards; the chart
  ships the changed CRDs, so the local seven-image chart matrix runs before my-ship (REQUIRED for a
  Go change). The stale ModelScope comment in `api/worker/v1alpha1/model_artifact.go` is corrected
  in the same run (pure comment; S10 opened the source without updating it).
- **Measured conventions restated:** hub digests are hub knowledge (`gitsha1` for non-LFS files on
  Hugging Face, `sha256` everywhere on ModelScope); the mount rule compares the volume attribute
  with `status.resolved.manifestDigest`; the store publishes only through seal-and-rename.
- English code, comments, docs and commit messages; progress and gates to the coordinator in
  Chinese.

## Boundaries

- **Always:** the anchor comparison on every path that produces or re-produces a digest; tests
  proven able to fail (mutation of exactly the guard each test pins) for AC2 and AC3; per-module
  squashed commits with `--signoff`, spec last; rebase on `origin/main` at each task start, before
  every e2e and before my-ship; the file-intersection discipline (name the touched files in the
  day's first progress message before editing existing files).
- **Ask first:** any API change beyond the two named above (`expectedDigest`, `digestSource`);
  any new Setting; any chart change beyond the CRD regeneration ride-along; reviving the #693
  migration or the claim anchor inside this spec (both are adjudicated out; a change of course goes
  back to the coordinator).
- **Never:** a manifest format or digest-algorithm change; cross-source dedup; delivery of an
  unverified tree (nothing enters `published/` without a digest match); following a branch after
  resolution; an anchored artifact contacting the hub after it resolved to `Expected`; touching
  crew-line files (`pkg/setting/types.go`, `node_queue.go`, the MD webhook barrier); the S8
  DeliveryClass surface; any change to claim or image source behavior.
- Review round budget: 4; after that, findings are listed, not fixed — except data loss, a broken
  security guarantee (read-only), or cross-tenant leakage, each raised with the coordinator first.

## Risks and Mitigations

- **An anchored artifact pinned to the wrong digest** fails at first use, loudly, at resolution —
  the failure the feature exists to produce; the message carries both digests for the ticket.
- **Revalidation listing cost** → one listing per interval per anchored artifact; unanchored
  artifacts keep the two-request probe. The anchor is opt-in per artifact.
- **Digest knowledge becomes a capability for `Expected` identities** (see the Notes caveat) →
  flagged for the plan gate: accept-and-document (recommended) or restrict `Expected` mounts to
  self-verified trees. Integrity is unaffected either way.
- **A downgrade window drops the new fields** (the old webhook cannot enforce the anchor's
  immutability; old workers drop `digestSource`) → documented in the docs' downgrade note;
  re-upgrading recomputes. The spec field itself is CRD-served and survives the window.
- **The confirmed-cadence fallback delays an air-gapped artifact by one minute** → deliberate: one
  blip must not freeze an identity; the staircase's own cadence is the established pattern.

## Design Details

### Commands

Everything runs locally on the seat's macOS worktree; kind clusters come from the e2e skill's own
provision scripts, images from the xbuild-and-verify skill, and code generation from the private
gen tree (`gen-trees/mam-s11`, branch `mam-s11-gen`, replayed onto the task branch and required to
replay zero-diff). Per task, the focused form first and the wide sweep before each commit:

```bash
go test ./pkg/worker/webhooks/worker/ -run ModelArtifact      # admission
go test ./pkg/worker/controllers/worker/ -run ModelArtifact   # resolution, rendering
go test ./pkg/modelmanager/...                                # the plugin's Expected chain
go test ./api/worker/...                                      # types, round-trip
make lint                                                     # golangci-lint + the rest
make lint docs && make lint chart                             # docs pages; CRD-bearing chart
bash .agents/skills/gpustack-operator-docs/scripts/check-specs.sh
make generate          # in the private gen tree only; replay zero-diff onto the branch
```

The e2e case runs through the e2e skill's full harness (preflight, `case-1.sh` mandatory first),
with the case number taken at my-ship time as the lowest unused.

### Project structure

| Path | Role in this spec |
| --- | --- |
| `api/worker/v1alpha1/model_artifact.go` | `spec.expectedDigest`; `status.resolved.digestSource`; the stale ModelScope comment corrected |
| `pkg/worker/webhooks/worker/model_artifact.go` (+`_test.go`) | anchor shape, hub-only acceptance (claim/image refusals), immutability message |
| `pkg/worker/controllers/worker/model_artifact.go` (+`_test.go`) | hub anchor compare, `DigestMismatch` in the revoking reasons, the revalidation upgrade, the confirmed-cadence `Expected` fallback, `digestSource` on every write |
| `pkg/worker/controllers/worker/model_artifact_placement.go`, `model_deployment.go` (+tests) | `AnchorNeedsNodeDelivery` under Engine delivery |
| `pkg/modelmanager/materialize/materialize.go` (+ its test) | the `Expected`-identity chain: peers only, the no-source message |
| `docs/modules/model-delivery/artifact.md`, `docs/modules/model-delivery/node-store.md`, `docs/README.md` | the documented behavior listed under Documentation |

### Code style

Follows the tree's standing conventions: English everywhere; reasons are constants mapped verbatim
into condition reasons (`modelartifact.SourceError` is the shape); table-driven tests with a
shared loop and declarative cases; fake clients over real dependencies; errors wrapped with
`fmt.Errorf("...: %w")`; snake_case file names. New condition reasons join the existing reason
blocks, not ad-hoc strings.

### Implementation Plan

- [x] **T1 · API + admission tracer**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_artifact.go`, `pkg/worker/webhooks/worker/model_artifact.go`
      (+ its test), generated artifacts via the gen tree
      Acceptance: `expectedDigest` validates (`sha256:` + 64 lowercase hex), is accepted on hub
      sources, refused on claim sources ("the claim's content is whatever it holds at mount time;
      its identity is the claim") and on image sources ("the reference's digest is the identity"),
      and is immutable with the spec; `digestSource` exists; CRD + deepcopy + protobuf regenerate
      zero-diff through the gen tree; the stale ModelScope comment corrected. (AC6)
      Verify: `go test ./pkg/worker/webhooks/worker/ -run ModelArtifact && go test ./api/worker/...`
- [x] **T2 · Controller: hub anchoring, anchored identity, digestSource**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_artifact.go` (+ its test)
      Acceptance: resolution compares the anchor (`DigestMismatch`, both digests in the message,
      joining the revoking reasons); revalidation with an anchor re-lists and re-compares (the hub
      interface grows the manifest-returning form; the revalidation and the token probe both branch
      on `digestSource`, so an `Expected` artifact makes no hub call on any pass); the
      `SourceUnavailable` fallback fires only on the confirmed cadence and is skipped for any hub
      that answered; `digestSource` is written on every resolved write. Mutations prove AC2 and
      AC3 red. (AC1–AC4)
      Verify: `go test ./pkg/worker/controllers/worker/ -run ModelArtifact`
- [x] **T3 · Delivery surface: Engine block and the Expected chain**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_artifact_placement.go`, `model_deployment.go`
      (+ their tests), `pkg/modelmanager/materialize/materialize.go` (+ its test)
      Acceptance: an anchored hub artifact under Engine delivery is blocked with
      `AnchorNeedsNodeDelivery` (the anchor-verification message) and creates no Pods; the
      plugin's chain for an `Expected`-identity artifact is peers only and its no-peer failure is
      the loud no-source message; a hub-identity artifact's chain and its anchor-checked listing
      are unchanged. (AC5)
      Verify: `go test ./pkg/worker/controllers/worker/ -run ModelArtifact &&
      go test ./pkg/modelmanager/materialize/`
- [x] **T4 · Documentation**
      Blocked by: T2, T3 (docs describe landed behavior)
      Owns: `docs/modules/model-delivery/artifact.md`, `docs/modules/model-delivery/node-store.md`, `docs/README.md`
      Acceptance: every Documentation bullet landed; `make lint docs` and the cross-reference
      checks green. (AC7)
      Verify: `make lint docs &&
      bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh . &&
      bash .agents/skills/gpustack-operator-docs/scripts/check-crossrefs.sh .`
- [x] **T5 · The e2e case**
      Blocked by: T3; branch rebased on `origin/main` first
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-<N>.sh` (N = lowest unused at
      my-ship) and its SKILL.md index row
      Acceptance: on kind — a hub anchor match resolves and delivers as an unanchored artifact;
      a hub anchor mismatch refuses resolution (`DigestMismatch`, both digests); an anchored
      artifact under Engine delivery creates no Pods (`AnchorNeedsNodeDelivery`); the air-gap leg
      (Expected identity, peer-served delivery with the hub unreachable) runs where the
      environment allows, with an offline skip guard in the case-113 style. (AC1, AC2, AC5)
      Verify: the e2e skill's full run on the case, green; evidence under the task's report
      directory.
- [x] **T6 · Ship**
      Blocked by: T4, T5
      Owns: the branch as a whole; the PR
      Acceptance: all gates green locally (`make lint`, `make lint docs`, `make lint chart` for
      the CRD ride-along, the agents-shell gate for the new case, the gen-tree replay zero-diff),
      the spec's status advanced and committed last, the PR opened per the issue-PR conventions,
      review rounds within budget. Mutation results recorded in the build handback.
      Verify: the gates' transcripts; `git log` module-squashed with `--signoff`, spec last.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
Existing suites that assert today's shapes need updating in the tasks that change them: the
webhook's immutability cases (T1) and the controller's resolution/revalidation reason tables (T2).
No test is deleted; every changed expectation is changed in the commit that changes the behavior.

#### Unit tests
- `pkg/worker/webhooks/worker`: `2026-09-29 (T1)` — anchor shape accept/refuse, claim-source
  refusal, image-source refusal, immutable update; the existing ModelArtifact admission table
  grows the rows.
- `pkg/worker/controllers/worker`: `2026-09-29 (T2–T3)` — anchor compare (match/mismatch),
  `DigestMismatch` in the revoking staircase, revalidation re-list vs HEAD with request counts,
  confirmed-cadence fallback vs single blip vs answered refusals, the zero-hub-request pin across
  Secret- and NodeModelStore-triggered passes, `digestSource` on every write, `AnchorNeedsNodeDelivery`.
- `pkg/modelmanager/materialize`: `2026-09-29 (T3)` — the `Expected` chain: peers-only, the
  no-source message; a hub-identity chain unchanged.
- Mutation checks (recorded in the build handback): AC2 — neuter the comparison, the mismatch case
  goes red; AC3 — reduce the anchored revalidation to the HEAD probe, the drift case goes red.
  Revert and re-verify.

#### Integration tests
The controller's fake-hub suites are the integration layer this feature has; the peer-pull leg of
AC5 runs against the existing peer test harness.

#### e2e tests
One new case (T5): hub anchor match and mismatch, the Engine block, and the air-gap peer leg with
an offline skip guard. case-113 (ModelScope) and the existing artifact cases re-run as regression
in the same gate run.

## Alternatives

- **The claim anchor** (`expectedDigest` accepted on claim sources, the node plugin
  reverse-computing the manifest from the claim's bytes): **rejected by the user at the plan gate
  (2026-09-29)** — a claim may be dynamically provisioned, so its content is only known at mount
  time, like an image; the claim's identity is the claim itself, and the user confirms their
  claim's content with their own tooling. The anchor there would have made digest knowledge a
  cross-tenant capability and dragged a kubelet pod-volume bridge behind it. If claim verification
  is ever wanted, it is a new spec.
- **`expectedDigest` on the image source**: rejected at admission — the reference's OCI digest
  already pins an image; an allowed-but-meaningless field invites a permanently failing artifact.
- **The `Verified` condition**: dropped this revision — its only novel scenario was the claim
  anchor; for hub sources `status.nodes.ready` already answers whether a published copy exists.
  The condition stays deferred, as the format's design note left it.
- **Silent anchored fallback for every resolution failure**: rejected — a hub that answered
  (`AccessDenied`, `RevisionNotFound`) is a hub whose answer decides; only `SourceUnavailable`, the
  one outcome that carries no verdict, falls back to the anchor, and only on the confirmed cadence.
- **Upgrading an `Expected` artifact to hub-resolved when the hub appears**: rejected for this spec —
  it would rewrite a frozen identity's revision after the fact; recreate the artifact instead.
- **Carrying the manifest itself in the API** (a ConfigMap reference or a spec field) for the
  air-gapped case: deferred — it is real API surface serving one combination, and the declared
  out-of-scope keeps this spec's API additive and small. Revisit if the combination matters to a
  user.
- **The legacy-cache migration in this spec (#693)**: excluded by adjudication (2026-09-29) — the
  anchor this spec publishes is the acceptance contract the future import must satisfy; the tool
  itself is that issue's own design work.

## Open Questions

1. **#693 inclusion** — **resolved, excluded (spec gate, 2026-09-29)**: the migration tool is
   another cycle's work under its own issue; this spec carries the acceptance contract above.
2. **The claim anchor and the placement question** — **resolved, excluded (plan gate,
   2026-09-29)**: the user adjudicated the claim anchor out (dynamically provisioned claims hold
   whatever the mount brings; the user confirms their own claim), which moots the PVC
   reverse-computation placement, the kind PoC and the `Verified` condition. Claims and images are
   refused an anchor at admission; no claim path changes.
3. **Air-gapped boundary** — **resolved, confirmed (spec gate, 2026-09-29)**: an anchored artifact
   that resolved to `Expected` never contacts the hub again; no Engine delivery for anchored
   artifacts; an anchored hub artifact offline delivers only from a peer holding the published
   tree; no automatic `Expected` → hub-resolved upgrade; behavior without an anchor is unchanged.
4. **Digest-as-capability for `Expected` identities** — **resolved, accepted and documented (plan
   gate, 2026-09-29)**: integrity is unaffected (only anchor-named verified bytes mount), secrecy
   of leaked digests is explicitly not a control (digests are cluster-visible on `NodeModelStore`
   status), hub-verified identities keep credential resolution, and the docs state the shipped
   model and its premise. An overturn reopens the `Expected` sharing surface for re-adjudication.

No question blocks the build; the plan gate confirmed the revised scope (T1–T6) on 2026-09-29 and
my-build starts at T1.
