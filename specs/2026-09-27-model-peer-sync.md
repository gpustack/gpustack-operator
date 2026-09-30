# Spec: Node-to-node sync (peer pull) for the model-manager plugin

Status: Shipped
Type: Feature

## Summary

A node whose plugin already holds a model's published weights serves them to the other nodes'
plugins over a dedicated authenticated endpoint, so that a cold node materializing the same
content pulls its bytes from a peer instead of the hub. This deduplicates hub egress bytes (N
nodes pulling one artifact costs the hub one copy, not N) and keeps cold-node materialization
working while the hub is unreachable, at the cost of one more port on the plugin's DaemonSet
and a NetworkPolicy that keeps tenants out of it. The prototype validated the whole chain on a
4-node cluster; this spec turns it into the shipped design: single-source pulling (multi-source
stays an extension point), ServiceAccount-token authentication with TokenReview, hashing in the
download stream with periodic checkpoints, the `source` accounting field consumed into
per-Peer/per-Hub statistics, and the #655 watermark-cap fix with the #657 documentation
clarification.

## Motivation

### Goals

Every node in a GPU fleet eventually needs the same weights. Today the hub serves each node
separately: N nodes pulling one 16 GB artifact cost the hub 64 GB of egress, and a node whose
network cannot reach the hub cannot materialize at all even when three of its neighbors already
hold the bytes. The model-manager plugin already publishes a per-node, verified,
content-addressed tree on every node's disk; this feature lets the nodes serve each other from
those trees.

1. **Hub egress deduplication.** When N nodes materialize the same artifact, the hub delivers
   one copy's worth of file bytes. Measured on the validation cluster: hub egress for one
   16.4 GB artifact across four nodes drops from 4.0× to 1.0× (`b3a` vs `a1`).
2. **Hub-unreachable resilience.** A cold node materializes a resolved artifact from a peer
   even while the hub's network path is down, because both the manifest listing and the file
   bytes can come from a peer; no tenant credential ever crosses a node-to-node hop.
3. **Honest accounting.** The per-node `source` field of `NodeModelStore.status.models[]`
   (introduced in S3 with only `Hub` legal) gains `Peer`, and hub-side vs peer-side bytes are
   counted separately in the plugin's metrics.
4. **Transfer correctness.** Bytes are hashed in the download stream (not after), and each
   file's progress is checkpointed periodically with a valid hash state, so a peer or connection
   dying mid-file resumes from the last checkpoint instead of re-pulling the file.
5. **Tenant isolation.** The peer endpoints are reachable only from the plugins' Pods; a tenant
   Pod cannot connect.
6. **#655.** On a filesystem shared with kubelet, a byte-form eviction threshold at or above
   the filesystem size currently leaves the cache's high-watermark cap relaxed at kubelet's
   default-derived value (85%). The correct behavior is to drive the cap to its floor, because
   kubelet can never reclaim on an unreachable threshold and the cache's own collection is the
   only space reclaimer. Fixed with a regression test that goes red on the old code.
7. **#657 clarification (docs).** `docs/reference/model-artifact-views.md` states that the v1
   view's printer columns come from the aggregated apiserver's TableConvertor
   (`NewJSONPathTemplateTableConvertor` in `pkg/worker/extensionapis/worker/`), while the
   v1alpha1 CRDs print a smaller column set.

### Non-Goals

- **Multi-source scheduling.** One file is fetched from one source at a time. The fetching
  interface keeps a source-set extension point, but distributing segments across several peers
  is not implemented (measured on the validation cluster: 3 sources did not move single-puller
  throughput — 267–274 s vs 273 s at 16.4 GB — the puller's network path, not source count, is
  the bound).
- **mTLS authentication.** The prototype validated both token and mTLS with no measurable
  per-file overhead difference (17–20 s for a 1.5 GB pull either way); v1 ships the simpler
  token scheme. mTLS stays a possible later enhancement.
- **Dragonfly or any external P2P system** (the S8 candidate), tenant credential forwarding
  (never crosses a node-to-node hop), GC policy per pool (S6's domain), and any change to the
  semantics S1–S4 shipped.

## Proposal

The plugin gains a second listener on its own TCP port (a chart value, default `32445`; `0` = off).
It serves one thing: the node's *published* trees, read-only, per file, with byte ranges, under
a serving-stream limit, authenticated by the requester plugin's ServiceAccount token verified
through a TokenReview. A node materializing a digest discovers the other nodes that hold it
published and ready (from the NodeModelStores plus the plugins' Pods), picks one source, and
pulls file bytes from it with in-stream hashing and periodic checkpoints; on failure or a bad
byte it moves to the next candidate, with the hub as the last resort. The manifest listing
itself also prefers a peer (credential-free) over the hub. A chart-level NetworkPolicy admits
only plugin-to-plugin traffic on the port. The `port` chart value plus a
`model-store-peer-sync` Setting control the feature; metrics and the `source` status field tell
peer bytes from hub bytes.

### User Stories

#### Story 1
As a cluster administrator running model serving on N nodes, I want the second through Nth node
to pull a model's weights from the first node's cache, so that my hub bill and egress
bottleneck grow with model diversity, not with node count.

#### Story 2
As an operator in an environment where the hub is intermittently unreachable, I want a node
that mounts an already-resolved artifact to materialize from a neighbor that holds it, so that
node churn does not stall on hub connectivity.

#### Story 3
As a platform engineer, I want every node's status to say where its weights came from and
metrics to split hub-delivered from peer-delivered bytes, so that I can verify the feature is
actually saving hub traffic.

#### Story 4
As a tenant, I must not be able to read arbitrary model caches through the peer port, so that
node-to-node sync does not become a tenant-side data path.

### Core Features & Acceptance Criteria

#### F1 — Peer server on the plugin's own port

The plugin serves `GET /peer/v1/trees/{hex}` (a tree's manifest: the root digest, size, file
count, and per-file path, size and digest) and `GET /peer/v1/trees/{hex}/files/{path}` (byte
ranges via HTTP Range) for *published* trees only. Path resolution refuses anything that
escapes the tree. At most `--peer-sync-max-serving-streams` file answers are in flight;
further requests wait.

- The manifest a listing returns is written at publish time: the materializer already holds the
  assembled manifest object when it publishes, so publishing also stores it beside the marker.
  The server reads that stored manifest; it never re-hashes the tree per request. A published
  tree without a stored manifest (published before this feature) is not a listing source.
- Serves only content under `published/<hex>/tree/`; unknown hex, unpublished tree, or escaping
  path answers 404; not-a-digest answers 400.
- A tenant Pod's connection to the port is dropped by the NetworkPolicy (F5) and, absent the
  policy, refused by authentication (F3) — never served.

#### F2 — Single-source pulling client with hub fallback

Discovery lists the NodeModelStores and the plugins' Pods on demand (through the API reader at
attempt start, not a new informer: the plugin's cache watches only its own NodeModelStore, and
an O(N) list per attempt beats O(N) watches per node during a fleet download), excluding self
and excluding Pods that do not declare the peer port. It yields the ordered candidate sources
for a digest: ready peers first, the hub last. The interface between discovery and fetching
keeps the source-set extension point for multi-source scheduling.

A peer-provided manifest is accepted only after the pulling node reassembles it into a
canonical manifest and requires the recomputed root digest to equal the artifact's resolved
manifest digest — the same binding the hub path applies today. Per-file digest verification
alone is not the trust anchor: a self-consistent forged listing (attacker content with
recomputed per-file digests) must fail this root rebinding, be rejected, and rotate the source.
The same reassembly refuses duplicate paths and directory/file overlaps, which closes
truncation and substitution by construction.

Rotation semantics are the peer path's own, distinct from the hub path's credential rule (the
shipped attempt loop rotates only on a credential refusal; peer sources fail differently and
must not be reported as credential refusals): a peer source goes bad on transfer failure,
verification failure, or a rejected manifest; the next candidate takes over; the hub is the
last resort. An attempt-level budget bounds how much a dead peer can cost — once a source has
burned its failure budget the attempt stops dialing it, so one dead peer cannot multiply into
retry storms across all parallel files. Status messages distinguish a peer-source failure from
a hub credential refusal.

- Cold node + ready peer + hub reachable: the node's entry lands `source=Peer` and the node's
  `download_bytes_total{source="hub"}` stays 0 (file bytes; the hub serves nothing).
- Peer pod deleted mid-pull: the attempt completes from another candidate (peer or hub) and the
  node ends Ready.
- Peer serving bytes that do not match the manifest's per-file digest: the bytes are rejected
  and the next source is used; nothing bad is published.
- Peer serving a forged manifest whose recomputed root digest differs from the artifact's
  resolved manifest digest: the listing is rejected, the source is marked bad, the next
  candidate serves, nothing tampered is published.
- No peer holds the digest: the attempt runs exactly as today (hub-only behavior unchanged).

#### F3 — Token authentication with TokenReview

The pulling side sends a *projected* ServiceAccount token minted for a single feature-only
audience (`gpustack-model-peer`, the same audience every plugin checks — per-node audiences are
unworkable, since a server cannot review against an audience list it does not know). The
serving plugin's TokenReview pins that audience in the review request — a username check alone
would admit any operator-namespace token minted for the API server — and admits only the
plugins' ServiceAccount. Reviews are cached positively, with the cache lifetime bounded by the
token's own expiry and no negative-result caching (a freshly rolled Pod must not flake); a
steady stream therefore costs no review per request.

Client-side TLS: the peers' server certificates are self-signed (the plugin's secure-port
pattern), so the client skips verification. The projected token bounds what a man-in-the-middle
on the node-to-node path could steal: a feature-audience token with roughly an hour of life
that grants access only to peer endpoints, behind the NetworkPolicy — the residual risk is
documented in the docs page. mTLS is a documented future enhancement that removes it.

- Token of the plugins' ServiceAccount minted for `gpustack-model-peer`: served.
- No token, wrong audience, expired token, or another account's token: 401, nothing served.
- A valid token's cached review is reused; a review is not sent per request.

#### F4 — In-stream hashing and periodic checkpoints

Peer-delivered bytes are hashed in arrival order inside the fetch loop (the same verifier
machinery the hub downloader uses), and a file's checkpoint — byte offset plus valid hash
state — is written periodically, not only at the end.

- Serving peer killed mid-file: after it returns (or from the next candidate) the puller
  resumes from the most recent checkpoint; hub bytes for that file are bounded by
  (file size − checkpoint offset) — never a full-file re-pull.
- A completed file's checkpoint says the whole file, so a later attempt (peer or hub) skips it.

#### F5 — NetworkPolicy for the peer port

The chart ships an optional (default on) NetworkPolicy that selects the plugin Pods and
admits only plugin-to-plugin ingress on the peer port, the worker's reads on the existing
secure port (S3's progress fan-out), and the metrics scrapers' ingress on the metrics port,
following the device-manager policy's shape. Known limits, stated in the docs: kubelet probes
originate from the node and their treatment varies by CNI (the device-manager precedent works
in practice); a `hostNetwork` tenant Pod bypasses podSelector-based policies on such CNIs, so
authentication (F3) is then the only gate.

- Tenant Pod → peer port: blocked.
- Plugin Pod → plugin Pod on the peer port: allowed.
- Worker → plugin secure port: allowed (progress reads unchanged).
- `port=0` with the policy applied: verified a no-op — discovery yields no candidates and the
  node pulls from the hub exactly as today.

#### F6 — Configuration and accounting

Chart value `modelManager.port` (default `32445`; `0` = off) opens the listener; extra args carry the
limits (`max-serving-streams`, `streams-per-source`). L2 Setting `model-store-peer-sync`
(default `true`) turns peer *pulling* on or off independently of the listener. The plugin's
RBAC gains two permanent rules — TokenReview create, and Pods get/list in the operator
namespace — rendered unconditionally (static YAML; `port=0` makes them unused but harmless).
The `source` field of `NodeModelStore.status.models[]` gains `Peer`, and the accounting has two
write sites to keep consistent: the live `Progress()` report and the published tree's marker —
both currently hardcode `Hub`. The plugin's `download_bytes_total` gains `source="peer"`
counted on the peer fetcher's own path, not on the shared hub downloader, so a peer-fed file
cannot leak into the hub counter.

- `port=0`: no listener, behavior identical to today (RBAC rules unused).
- `model-store-peer-sync=false` with a port open: the node still serves peers but pulls from
  the hub only.
- A node whose bytes came mostly from peers reports `source=Peer` in both the live progress
  and the published marker; the aggregated statistics split Peer and Hub counts.
- Metrics: a peer-fed file's bytes appear under `source="peer"` and the hub counter stays 0.

#### F7 — #655: unreachable kubelet threshold drives the cap to the floor

In the cache's watermark cap, a byte-form kubelet eviction threshold at or above the
filesystem's size is currently excluded from the cap's minimum with kubelet's default applied —
the cap stays at the default-derived 85%. The fix: such a threshold signals that kubelet can
never reclaim on that filesystem, so the cap is driven to its floor (2%) and the note says why.

- Node with `nodefs.available` (byte form) ≥ filesystem size: the cache's high watermark is
  capped at the floor, with the ignored/reason note explaining the unreachable threshold.
- Percent-form and sub-size byte-form thresholds behave exactly as today.
- The regression test goes red on the pre-fix code (cap stays at the default-derived value).

#### F8 — Documentation

A new `docs/reference/` page for node-to-node sync (what serves what, the port, the Setting,
the NetworkPolicy, the `source` field, the capacity interplay), indexed in `docs/README.md`,
and the #657 clarification line in `docs/reference/model-artifact-views.md`.

#### F9 — End-to-end cases

On the validation cluster (nebius, context `mam-poc-sd095l7k`), or on kind where the case
needs no real network split — the plan fixes the environment per case with reasons:

- Cold node pulls from a peer with hub byte counters at 0 for that node.
- All peer pods down mid-attempt: the attempt still completes (hub fallback).
- A peer serving corrupted bytes: the bytes are refused and another source is used.
- Tenant Pod cannot reach the peer port (NetworkPolicy on).
- An unreachable kubelet byte threshold caps the watermark at the floor (#655).

### Notes / Constraints / Caveats

**Performance framing (from the validation cluster, 4 × 2-vCPU nodes, 16.4 GB artifact).**
The feature's value is hub-egress deduplication and hub-unreachable resilience, *not* wall
clock. On the validation cluster the node-to-node path (single stream ≈ 15 MB/s, one hot node
serving three pullers ≈ 105–110 MB/s aggregate) is slower than each node's own hub path
(≈ 130 MB/s per node), so all-hub finished the 4-node fan-out faster (303–343 s vs 455–495 s);
this is a property of that cluster's networking (its NAT egress is faster than its inter-node
link), not of the mechanism — doubling every concurrency knob changed nothing (274 s vs 273 s
single-source; 443–477 s vs 439–455 s fan-out). Consequently:

- The wall-clock acceptance is "peer distribution is not significantly worse than all-hub on
  the same cluster, and any gap is attributable to the inter-node link rather than the
  implementation", measured on nebius and reported alongside the egress numbers.
- The egress acceptance is the primary quantitative claim: hub-side bytes ≈ one copy for an
  N-node fan-out (validated: 4.0× → 1.0×).
- Serving cost is real but small: ≈ 0.4–0.54 vCPU of a 2-vCPU node while serving three
  concurrent pullers (idle ≈ 1m). On GPU nodes this shares headroom with inference — the docs
  say so.

**Trust model.** The manifest listing may come from a peer; per-file digest verification is the
trust anchor (a tampered listing yields wrong paths/sizes that fail the digest check), so no
manifest-level signature is required, and no tenant credential is needed node-to-node. A
private-repo artifact whose hub credential is required only falls back to hub listing/bytes
when no peer holds the content.

**Version floors.** The chart value and Setting ride the existing two-layer versioning (L1
chart value seeds, L2 Setting stores); the only new CRD payload is the `Peer` enum value. The
CRDs ship embedded in the worker binary and apply at worker startup, so during an upgrade a
newer plugin Pod can come up before the CRD schema accepts `Peer` and its status write is
rejected by validation — the plugin must tolerate that (retry on the existing cadence, log
once) and never crash-loop on it; the rollout note pins worker-before-plugin when practical.

**Related work in flight.** The crew line is touching `pkg/setting/types.go` and
`docs/reference/settings.md` (C77/C78/C79). This spec does not touch `pkg/setting/types.go` at all —
its Setting declares beside its siblings in `pkg/worker/settings/model_store.go` — and its
only `docs/reference/settings.md` change is one row, coordinated through the coordinator before the PR.

### Boundaries

- **Always:** rebasing onto origin/main at every task start and before every push; regenerating
  instead of hand-editing generated files; running affected-package unit tests after a rebase;
  sanitizing (no tokens, no host addresses) in anything that leaves the machine.
- **Ask first:** any change to files outside the listed paths; large rearrangements of existing
  files in `pkg/modelmanager` or `pkg/worker` (name the file list in the day's first progress
  message before touching existing files there); the `docs/reference/settings.md` edit (crew
  coordination).
- **Never:** forward a tenant credential over a node-to-node hop; implement multi-source
  scheduling; touch Dragonfly or GC pooling semantics; alter S1–S4 delivered behavior.

### Risks and Mitigations

- **A peer serves stale or corrupted bytes → Mitigation:** published trees are immutable and
  content-addressed; the manifest is rebound to the resolved root digest before any byte
  is pulled, and every byte range lands under per-file digest verification (F4); verification
  or manifest failure rotates to the next source (F2).
- **The peer port becomes a tenant data path → Mitigation:** NetworkPolicy by default (F5) plus
  token authentication (F3) as the second layer; e2e asserts the tenant block. The known
  `hostNetwork`-tenant bypass on some CNIs is documented, with authentication as the remaining
  gate.
- **TokenReview load on the API server → Mitigation:** positive cache bounded by the token's
  own expiry; measured steady-state cost is nil per request.
- **Mid-file failures waste bytes (the prototype re-pulled whole files: 2.9× egress in its
  fallback arm) → Mitigation:** periodic checkpoints with valid hash state are an acceptance
  criterion (F4), not an optimization.
- **Watermark cap change alters capacity behavior for existing users (#655) → Mitigation:** the
  change is confined to the previously-wrong case (unreachable byte threshold), and its real
  blast radius is owned in the docs and release notes: on such a node the effective high
  watermark drops to the floor, so the next plugin start collects unreferenced content down to
  it and refuses materializations that do not fit — the documented outcome of a kubelet
  configuration that can never reclaim; regression tests pin both the floored case and the
  unchanged percent/sub-size byte cases.
- **Upgrade skew rejects `Peer` status writes → Mitigation:** the plugin tolerates a rejected
  status write (retry on the existing cadence, log once, no crash loop); rollout note prefers
  worker-before-plugin.
- **A dead peer multiplies into retry storms → Mitigation:** the attempt-level failure budget
  (F2) stops dialing a source that has burned its budget; status messages name the peer-source
  failure instead of blaming a credential.
- **Crew line edits the same Setting/docs files → Mitigation:** coordinator relays; this spec
  adds one row and one enum, rebase resolves.

## Design Details

### Commands

- Build/lint/test: `make lint`, `go test ./pkg/modelmanager/... ./pkg/modelstore/...`,
  `make lint docs` for the docs change, `make generate` after any API marker change (in the
  private gen worktree per the standing convention).
- Chart checks: `helm template` render + `make lint chart` for the port/RBAC/NetworkPolicy
  changes.
- Validation cluster: images are built on the build machine
  (`PACKAGE_NAMESPACE=<registry-user> PACKAGE_PUSH=true PACKAGE_TAG=dev-<hash> make package`,
  worktree `~/mam/gpustack.ai/gpustack`, lock `~/mam-build.lock`) and pinned by digest at
  install; the cluster is nebius `mam-poc-sd095l7k`, namespace `mam-poc-p` only for installs,
  `mam-s5` for anything else.
- e2e: `.agents/skills/gpustack-operator-e2e/cases/` — case numbers are taken at `my-ship`
  time as (current maximum + 1) against the then-main, never the numbers a plan first wrote.

### Project Structure

- `pkg/modelmanager/peer/` — the node-to-node sync: server (published trees, stored manifests,
  ranges, serving-stream cap), auth (audience-pinned TokenReview middleware, TLS config
  builders), discovery (NodeModelStores + Pods → ordered candidates), client (single-source
  segmented fetching, root-digest rebinding, in-stream hashing, periodic checkpoints, candidate
  rotation with a failure budget). Snake-case filenames, one responsibility per file.
- `pkg/modelmanager/{option,config,manager}.go` — additive wiring: flags, config struct
  field, listener start and puller injection.
- `pkg/modelmanager/materialize/` — the puller hook in the per-file fetch path, the per-attempt
  source accounting (`source` = majority kind, consistent at the live-progress and marker
  write sites), and the manifest stored beside the marker at publish.
- `pkg/modelmanager/download/` — the exported in-stream hashing + checkpoint surface the peer
  fetcher builds on, additive only.
- `pkg/modelmanager/gc/watermark.go` — the #655 fix in `availablePercent`/`KubeletCap`.
- `api/worker/v1alpha1/node_model_store.go` — `Peer` enum value (+ `make generate`).
- `pkg/worker/settings/model_store.go`, `docs/reference/settings.md` — the `model-store-peer-sync`
  Setting (the Setting declaration lives with its siblings; `docs/reference/settings.md` is coordinated
  with the crew line).
- `deploy/gpustack-operator/chart/` — `modelManager.port` value, DaemonSet args/env,
  plugin RBAC (TokenReview create; Pods get/list in the operator namespace), NetworkPolicy
  template.
- `docs/reference/` new page + `docs/README.md` + the #657 line in
  `docs/reference/model-artifact-views.md`.
- `.agents/skills/gpustack-operator-e2e/cases/` — the new cases.

### Code Style

Repository conventions apply as everywhere else: snake_case multi-word filenames; exported
APIs documented with behavior, expectations and constraints; errors typed and wrapped with
context; no spec-task identifiers in comments; table-driven tests with one behavior per case.

### Implementation Plan

Single-seat execution order (T1 → T6); the DAG edges are what matter, not the sequence. Every
task starts with `git fetch origin && git rebase origin/main` and rebase-affected-package
tests per the standing discipline. Existing files in `pkg/modelmanager` and `pkg/worker` are
named here so the day's first progress message can carry the list before any of them is
touched: `option.go`, `config.go`, `manager.go` (T3/T4 assembly lines only, additive),
`materialize/materialize.go` (T4), `download/verify.go` (T2), `gc/watermark.go` (T1),
`pkg/worker/settings/model_store.go` (T4).

- [x] **T1 · #655 watermark cap fix (independent tracer)**
      Blocked by: None
      Owns: `pkg/modelmanager/gc/**`
      Acceptance: `KubeletCap` with a byte-form threshold at or above the filesystem size caps
      at the floor (2%) with the reason note naming the unreachable threshold; percent-form and
      sub-size byte-form thresholds keep today's exact outputs; `Effective` unchanged. The
      regression test is written first and shown red on the pre-fix code (the red run is
      recorded in the task report).
      Verify: `go test ./pkg/modelmanager/gc/`

- [x] **T2 · In-stream hashing + periodic checkpoint API (prefactor)**
      Blocked by: None
      Owns: `pkg/modelmanager/download/**`
      Acceptance: an exported streaming surface that hashes bytes in arrival order against a
      manifest line, writes a checkpoint (offset plus serialized hash state) at a byte
      interval, and restores one — rejecting a checkpoint inconsistent with the file on disk —
      so a peer fetcher resumes mid-file. The hub path's behavior is unchanged.
      Verify: `go test ./pkg/modelmanager/download/`

- [x] **T3 · Peer server, auth, and chart wiring (tracer: serve + admit + isolate)**
      Blocked by: None
      Owns: `pkg/modelmanager/peer/` (server, auth, discovery and their tests); server
      assembly lines in `pkg/modelmanager/{option,config,manager}.go`;
      `deploy/gpustack-operator/chart/**` — the `modelManager.port` value, DaemonSet args, the
      peer containerPort declaration, the projected-token volume with the
      `gpustack-model-peer` audience, the RBAC rules (TokenReview create; Pods get/list in the
      operator namespace), the NetworkPolicy template
      Acceptance: unit tests for listing/range/escape/401/audience/pinned-audience review and
      the positive cache; on the validation cluster two plugins exchange a listing and a range
      with tokens, a tokenless or wrong-audience request gets 401, a tenant Pod cannot connect,
      and `port=0` is a verified no-op.
      Verify: `go test ./pkg/modelmanager/peer/` + the cluster smoke captured under the task
      report's `raw/`

- [x] **T4 · Single-source puller, materializer integration, accounting, Setting**
      Blocked by: T2, T3
      Owns: `pkg/modelmanager/peer/client*.go` + its tests; the puller assembly in
      `pkg/modelmanager/manager.go`; `pkg/modelmanager/materialize/**` (the peer hook in the
      per-file path, per-attempt source accounting, the manifest stored at publish);
      `api/worker/v1alpha1/node_model_store.go` (`Peer` enum, + `make generate`);
      `pkg/worker/settings/model_store.go` (`model-store-peer-sync`)
      Acceptance: unit tests for discovery (self and port-less Pods excluded), root-digest
      rebinding (forged / truncated / duplicate-path / dir-file-overlap listings rejected),
      rotation with the attempt-level failure budget, checkpoint resume, gitsha1 manifest
      lines, source accounting at both write sites, the metrics split, and the Setting
      off-switch; on the validation cluster a cold node materializes from a peer with
      `source=Peer` and hub bytes 0.
      Verify: `go test ./pkg/modelmanager/... ./pkg/worker/settings/` + cluster capture in raw/

- [x] **T5 · Resilience evidence, e2e cases, validation-cluster measurements**
      Blocked by: T4
      Owns: `.agents/skills/gpustack-operator-e2e/cases/**`
      Acceptance: the e2e rows land (cold-node peer pull with hub bytes 0; peers-down
      mid-attempt completes from another candidate; corrupted bytes refused and rotated; tenant
      blocked; #655 capped node), the mid-file kill resume is demonstrated from a real
      checkpoint (hub bytes for that file bounded by size − offset, never a full-file re-pull),
      and hub-egress (≈1×) plus wall-clock measurements are re-taken on nebius against the
      shipped implementation rather than the prototype.
      Verify: the new e2e cases pass; measurement captures under the task report's `raw/`

- [x] **T6 · Documentation**
      Blocked by: T4
      Owns: `docs/**`
      Acceptance: the new reference page (what serves what, the port, the Setting, the
      NetworkPolicy, the `source` field, the token scheme's residual risk, the #655 capacity
      outcome), the `docs/README.md` index entry, the #657 clarification line in
      `docs/reference/model-artifact-views.md`, and the `model-store-peer-sync` row in
      `docs/reference/settings.md` (coordinated with the crew line before the PR).
      Verify: `make lint docs`

Ship sequencing notes: the spec file itself is committed in the final commit with `Status:
Shipped`; e2e case numbers are taken at `my-ship` time as the then-current maximum + 1; the
PR keeps `--signoff` and per-module squashes.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None beyond today's suite; the peer package starts green under `go test ./pkg/modelmanager/...`,
and T1's red-first regression is part of its own task, not a prerequisite.

#### Unit tests

- `pkg/modelmanager/gc` (T1): the #655 regression — a byte threshold ≥ filesystem size floors
  the cap at 2% with the note; percent-form and sub-size byte-form thresholds pin today's
  exact outputs both directions; `Effective` untouched.
- `pkg/modelmanager/download` (T2): in-stream hashing in segment arrival order; checkpoint
  write/restore; a checkpoint inconsistent with the file on disk is rejected; hub regression
  suite stays green.
- `pkg/modelmanager/peer` (T3/T4): listing/range/escape/401/audience-pinned TokenReview with a
  positive cache bounded by token expiry and no negative caching; discovery excludes self,
  non-ready, and port-less Pods; **root-digest rebinding — the forged-listing case (attacker
  entries with recomputed per-file digests and a claimed root equal to the resolved digest)
  must be rejected and rotate, and the case doubles as the design check: it is red unless the
  listing carries per-file digests and the client recomputes the root**; truncated listing
  (omitted entry) rejected by recomputation; duplicate path and dir/file overlap refused;
  served path with `..` or a symlink answers 404; size mismatch and short bodies rotate;
  checkpoint resume starts at `ResumableOffset`; gitsha1 lines flow through; single-source
  success and rotation across candidates.
- `pkg/modelmanager/materialize` (T4): source accounting at both write sites (live progress and
  marker) with the majority rule; hub fallback on peer failure; the Setting off-switch pins
  hub-only; the metrics split guard (a peer-fed file never moves `source="hub"`).
- `pkg/worker/settings` (T4): `model-store-peer-sync` admission (true/false only, default true).

#### Integration tests

On the validation cluster (nebius `mam-poc-sd095l7k`, namespace `mam-poc-p`), captured under
the task report's `raw/`: two-plugin exchange (listing + range) with tokens; tokenless and
wrong-audience 401s; tenant Pod blocked; mid-file kill resume from a real checkpoint with the
hub-byte bound asserted; a forged-listing peer (a crafted fake) rejected with rotation and
correct content published via fallback; `port=0` no-op; mixed-version pair (old plugin without
a peer port + new puller) falling back to the hub without burning candidates on dial errors.

#### e2e tests

New cases under `.agents/skills/gpustack-operator-e2e/cases/` (numbers taken at `my-ship`):
cold-node peer pull with hub byte counters at zero; peers-down mid-attempt completes;
corrupted bytes refused with rotation; tenant blocked from the peer port; #655 node caps at
the floor with the note and existing mounts unaffected. Environment: nebius for the
network-dependent rows (peer traffic, hub-byte accounting — a single-host kind has no
meaningful node-to-node link or hub egress), kind acceptable for the #655 row; the per-case
environment and the reason are recorded in the case headers and the final SUMMARY.

## Alternatives

- **Dragonfly (S8 candidate).** A mature P2P distributor would replace the custom protocol.
  Rejected for now per the standing decision: the plugin already holds verified,
  content-addressed trees, so the incremental cost of serving them is a listener, not a
  system; Dragonfly stays the fallback if operational burden proves too high.
- **Multi-source segment splitting.** Measured as unnecessary on the bounded path (source
  count does not move single-puller throughput); the source-set extension point keeps the
  door open without shipping the scheduler.
- **mTLS.** Equivalent cost in measurement; more machinery (issuance, rotation, distribution).
  Deferred.
- **Manifest via artifact status.** Storing the resolved file list in the artifact's status
  would let nodes skip listing entirely but grows a CRD by the file count; peer listing with
  per-file digest verification achieves the same correctness without the API growth.
- **Hash-after-fetch (the prototype's shortcut).** Simpler, measured acceptable at 16 GB, but
  it wastes CPU and breaks resume semantics mid-file; the acceptance criteria require in-stream
  hashing and periodic checkpoints instead.

## Open Questions

None — the three carried into review were adjudicated on 2026-09-27: the manifest listing
comes from peers and is bound to the artifact's resolved root digest (forged listings are
rejected and rotate); `model-store-peer-sync` defaults to `true` (setting the port to `0` is the
chart-level master switch); #655 floors the cap at 2%, consistent with #634's reading that an
unreachable threshold means permanent DiskPressure and a minimal cache footprint. The docs
page's name is decided at T6 by the docs routing rules.
