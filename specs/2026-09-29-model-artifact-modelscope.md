# Spec: ModelScope ModelArtifact Source

Status: Shipped
Type: Feature

## Summary

Open the ModelScope source of `ModelArtifact` end to end. The API has carried a reserved
`modelScope` member since the Hugging Face spec; admission refuses it, with the message naming
exactly what opening it needs. This spec implements those needs — branch resolution cross-checked
against git, a listing that recovers from the API's silent truncation at 3000 entries, errors
classified by the envelope code, and an engine runner whose ModelScope SDK accepts a commit — and
turns the member on: admission accepts it, the controller resolves it to an immutable commit and a
manifest digest, the node plugin downloads and verifies it, and the documentation describes it.

## Motivation

### Goals

ModelScope is one of the two public model hubs Chinese users reach for first; an artifact API that
only speaks Hugging Face leaves half the ecosystem's weights behind a manual copy. The goal is
parity of behavior with the Hugging Face source, with the three ModelScope-specific failure modes
(PoC-D, PoC-C) handled so that none of them can silently deliver wrong content:

1. **Branch resolution cross-checked.** ModelScope's
   `GET /api/v1/models/<id>/commits?Ref=<rev>&PageSize=1` is undocumented, and a misspelled
   parameter silently returns master. Resolution of a non-commit revision must ask git
   (`git ls-remote` over the HTTP smart protocol, `https://www.modelscope.cn/<id>.git`) and refuse
   the resolution when the two answers disagree.
2. **The 3000-entry silent truncation.** `repo/files?Revision=<commit>` truncates silently at 3000
   entries and ignores every pagination parameter. The listing must re-list per directory with
   `Root=<dir>` whenever a listing comes back with exactly 3000 entries, and must refuse a
   directory with 3000 or more direct children, where no decomposition can recover the tree.
3. **An engine that pins the commit.** The vLLM runner's bundled ModelScope SDK must accept a
   commit as the revision. Engine delivery renders for every engine alike; the documentation
   carries the runner SDK ≥ 1.39.1 floor and which measured runners meet it (option B, confirmed
   at the spec gate, below), because the same formula meets the floor on one hardware pool and
   misses it on another, and a user-supplied runner image is always one field away.

Plus, free of new mechanism because everything downstream keys on the manifest digest: the
peer-sync, progress-aggregation, prefetch-and-pinning and placement-preference behaviors of the
earlier seats work for a ModelScope source the moment its manifest digest exists. The spec adds one
acceptance case that proves it for peer sync, and states the others.

### Non-Goals

- An `expectedDigest` on the spec, or any pre-resolution digest check. (A separate topic.)
- Cross-hub content deduplication. The same files resolve to different digests on the two hubs
  (non-LFS files: `gitsha1` on Hugging Face, `sha256` on ModelScope); the manifest format v1 is
  unchanged, and digests stay incomparable across sources.
- Any change to the manifest format v1 (`gpustack-manifest v1`), the modelStore key layout, or the
  CSI volume contract.
- Supporting ModelScope datasets or private endpoints beyond the Setting below.

## Proposal

### Admission

- `validateModelArtifact` drops the `modelScope` refusal and validates the member with the same
  repository and revision rules as Hugging Face (the `ModelArtifactHubSource` shape is shared).
- Defaulting fills an unset `modelScope.revision` with `master` (ModelScope's default branch), the
  same defaulting Hugging Face gets with `main`, on update as well as creation.
- Patterns (`allowPatterns` / `ignorePatterns`) become legal on a ModelScope source: they select
  files of a hub source, whatever the hub.

### Resolution (controller)

A ModelScope source resolves once, like Hugging Face, with the namespace's token:

1. **Revision.** A 40-character commit is taken as is. Anything else goes to
   `commits?Ref=<rev>&PageSize=1`; a `Commit: null` answer is `RevisionNotFound`. The answer is
   cross-checked against the repository's git endpoint itself — the smart protocol's ref
   advertisement (`https://www.modelscope.cn/<id>.git/info/refs?service=git-upload-pack`),
   answered by the operator in process, with no `git` binary in the image and no subprocess. The
   commit the API named must equal the commit the advertisement names for the same ref (the
   `HEAD` line for a branch, the peeled entry for an annotated tag), and a disagreement fails the
   resolution as `SourceUnavailable` with a message that says the hub's index and its git
   disagree. The advertisement request names itself a git client, the protocol it speaks — the
   endpoint answers a git user agent and refuses others with 421 (measured). For a private
   repository it authenticates as the user `oauth2` with the namespace's token as the password —
   an HTTP header, never an argument.
2. **Listing.** The manifest input is `repo/files?Revision=<commit>&Recursive=true`. A listing of
   exactly 3000 entries is a truncation: each direct child directory is re-listed with
   `Root=<dir>`, recursively, and only a subtree that truncates is descended into further. A
   directory whose direct children reach 3000 fails the resolution as `SourceUnavailable` — the
   API cannot enumerate it, and a partial manifest would be a silent wrong answer.
3. **Per-file digest.** Every entry must carry `Sha256`; a missing one fails the resolution. Each
   file's digest line is `sha256:<Sha256>` — ModelScope gives no non-LFS alternative, so a
   ModelScope manifest has no `gitsha1` lines. Size comes from `Size`. `Type: tree` entries are
   directories; any other unexpected type fails.
4. **Canonicalization, filtering, digest.** Identical to the Hugging Face path: the same
   `Filter`, the same `gpustack-manifest v1` canonical bytes, the same digest. The digest lands in
   `status.resolved.manifestDigest` and drives node delivery, peer sync, progress, prefetch and
   placement preference unchanged.
5. **Reason mapping.** Every access failure ModelScope reports is a 404, so the envelope `Code`
   classifies, with the HTTP status as the fallback:

   | Reading | Reason |
   | --- | --- |
   | `commits?Ref=` answers 200 with `Commit: null` | `RevisionNotFound` |
   | `repo/files` 404 with `Code` 10990101004 (no file tree at the revision) | `RevisionNotFound` |
   | 404 with `Code` 10010205001 (not found) or 10010200001 (no access, private or gated) | `AccessDenied`; the message says "does not exist or is not accessible" |
   | any other 401 / 403 / 404 | `AccessDenied` |
   | 5xx, network error, undecodable body; a directory with 3000 direct children; a listing that stays truncated | `SourceUnavailable` |

   Measured limits, stated here and in the docs: ModelScope has no distinct 401 or 403, and
   "no access" (10010200001) covers private-without-token, gated-without-grant and
   valid-token-without-grant alike. A mistyped token is silently ignored on a public repository,
   exactly as on Hugging Face. **The private-repository cell "a valid token without access" is
   UNADJUDICATED**: distinguishing it needs a second ModelScope account, which this environment
   does not have (PoC-D). It maps to `AccessDenied` with the shared message; a test asserts only
   that mapping, not a distinct message.

6. **Revalidation.** One `HEAD` of
   `/api/v1/models/<id>/repo?Revision=<commit>&FilePath=<first file>` — measured: 200 for an
   accessible file (LFS and not, no redirect on HEAD), 404 for a missing commit, a missing path, a
   missing repository and a gated repository without a grant, with no envelope on HEAD. 200
   confirms; 404 is an `AccessDenied` refusal and goes through the same confirm-then-revoke
   staircase as Hugging Face (first refusal sets `Degraded`, the same refusal after the delay
   sets `Resolved=False`). The first file comes from one root listing, as on Hugging Face.

The controller branch mirrors `reconcileHuggingFace`: same pacing entry, same Secret watch, same
`Resolved` / `Degraded` conditions, same reasons. The mistyped-token warning is aligned too: a
`GET /openapi/v1/users/me` (200/401, measured in PoC-D) plays the part of `whoami-v2`, emitting
the same Warning event when the hub rejects a new token outright, conditions unchanged.

### Node delivery (plugin)

The plugin's downloader is already source-shaped (URL, digest, size, byte ranges, checkpoints);
the ModelScope source contributes a file URL builder:

- URL: `/api/v1/models/<id>/repo?Revision=<commit>&FilePath=<path>`; the answer is 200 for a
  small file or a 302 to a content-addressed CDN path for LFS (signed for a private repository),
  and the CDN honors byte ranges (PoC-D). The redirect is followed with the credential dropped on
  leaving the host, as the downloader already does.
- Every file verifies against its manifest `sha256` while streaming; resume replays the digest
  from the checkpoint. The manifest the node recomputes for comparison uses the same ModelScope
  listing, so a node accepts only bytes the hub itself hashed at the pinned commit.

**How the node knows the hub.** The plugin reads the full `ModelArtifact` from its informer cache
(`driver.artifact`), so the source kind comes from `ma.Spec.Source` — the volume attributes stay
as they are. The hub clients are built from the node's effective configuration
(`NodeModelStoreHub`), which carries one endpoint today; it gains an optional
`modelScopeEndpoint` (the one API change, below), and the materializer picks the client by the
source's kind. A `modelScope` artifact on a node whose configuration predates the field is
refused with a message that names the worker upgrade, never resolved against the wrong hub.

### Engine delivery

The vLLM and SGLang runners download through their bundled ModelScope SDKs. The measured switches
(PoC-C): vLLM `VLLM_USE_MODELSCOPE=True`, SGLang `SGLANG_USE_MODELSCOPE=true`, shared
`MODELSCOPE_CACHE=/var/lib/gpustack/model-cache`, argv unchanged
(`<repository> --revision <commit>`). A token reaches the SDK as `MODELSCOPE_API_TOKEN` (an env
referencing the artifact's Secret, owned key, value never in the Pod spec).

**The engine-runner question (D5 condition 3).** PoC-C measured the vLLM 0.29.0 runner's SDK
1.37.1 refusing a commit (`NotExistError`) and SDK 1.39.1 accepting it. The spike (2026-09-29;
readings in Runner SDK facts below, full log in the task directory's
`spec-10/raw/spike-runner-sdk.md`) measured the newest published builds directly: the CUDA vLLM
lines still carry 1.37.1 (the 2026-09-24 rebuild of `cuda12.9-vllm0.29.0` /
`cuda13.0-vllm0.29.0`, runner commit `80034d19`, hit the dependency-layer cache), the Ascend
vLLM line meets the floor, and vLLM 0.30.0 runners are built upstream (2026-09-23 CI) but not
released — no Docker Hub tag, no `runner.py.json` entry. The runner family is `gpustack/runner`;
`pack/cuda/Dockerfile` installs `modelscope` unpinned, so a runner's SDK is whatever its
dependency layer resolved at build time. Two facts follow: no "vLLM version → SDK version"
mapping exists (the SDK floats with the build), and the same formula meets the floor on one
hardware pool and misses it on another.

**Coordinated direction — option B, confirmed at the spec gate (2026-09-29):** render every engine
path normally and document the floor.
Engine delivery renders `MODELSCOPE_*` for vLLM and SGLang alike, whatever runner the role
resolves to; the documentation carries the SDK ≥ 1.39.1 floor, states which measured runners meet
it today (the Ascend `cann9.1-*-vllm0.23.0` line does; the current CUDA `vllm0.25.1` / `vllm0.29.0`
lines do not), and tells users whose runner bundles an older SDK to name a runner image of their
own that bundles ≥ 1.39.1 — the same freedom every role already has to name its image. Admission
gains no ModelScope-specific refusal: the operator cannot see the runner's contents, a refusal by
combination would wrongly reject the Ascend users the floor already satisfies, and an old runner's
`NotExistError` is a loud, named engine error, not silent wrong content.

### Free behaviors, each with its acceptance

Everything downstream of the digest works for a ModelScope artifact with no new code. Peer sync
gets its own acceptance case below; the rest are covered by stating the digest invariant: a
ModelScope artifact and a Hugging Face artifact never share a digest, and every digest-keyed
behavior (progress aggregation, prefetch, placement preference) is proven digest-keyed already.

### Documentation

- `docs/model-store/artifact.md` (the page the HANDOFF's `docs/reference/model-artifact.md`
  became): the "refused" bullet becomes an accepted source — the resource example, the delivery
  table, the resolution section (ModelScope rows in the reason table, the cross-check, the
  truncation recovery), the digest section (all-`sha256` manifests; cross-hub digests stay
  incomparable), the engine-delivery env table, the requirements list (the runner SDK floor, per
  the coordination).
- `docs/settings.md`: `model-artifact-modelscope-endpoint` joins the ModelArtifact table.
- `docs/README.md` and the docs skill routing follow the docs skill; no new page is expected (the
  artifact page owns the source kind), decided finally by the docs skill's routing.

### Configuration

| Setting | Env | Default | Meaning |
| --- | --- | --- | --- |
| `model-artifact-modelscope-endpoint` | `GPUSTACK_MODEL_ARTIFACT_MODELSCOPE_ENDPOINT` | `https://www.modelscope.cn` | The ModelScope endpoint the controller resolves and revalidates against and the node plugin downloads from. Engine Pods receive its host as `MODELSCOPE_DOMAIN` (the SDK takes a bare host); the node plugin receives the URL through `NodeModelStoreHub.modelScopeEndpoint`. Admission as for the Hugging Face endpoint: a URL with a schema and a host. |

The spike answered the endpoint question against the SDK inside the runner image (1.37.1): the
SDK reads `MODELSCOPE_DOMAIN`, whose value is a **bare host** (`www.modelscope.cn`) that the SDK
itself prefixes with `https://` — not a URL like `HF_ENDPOINT`. The renderer therefore derives
the host from the Setting's URL for engine Pods, and the Setting keeps the full-URL shape the
Hugging Face endpoint uses.

### Runner SDK facts

The readings the engine-runner direction stands on, all taken 2026-09-29 by this seat; the full
evidence log is the task directory's `spec-10/raw/spike-runner-sdk.md`.

- **What the operator renders.** A vLLM or SGLang deployment resolves to
  `gpustack/runner:<backend><runtime-version>[-<variant>]-<engine><engine-version>` — e.g.
  `cuda12.9-vllm0.29.0`, `cann9.1-910b-vllm0.23.0`, `cuda13.0-sglang0.5.18` (`model_deployment_image.go`).
  The operator sets no default: the engine version is whatever the deployment declares, so "the
  current runner" is a family, not one tag.
- **The readings and how they were taken.**

  | Runner image:tag | ModelScope SDK inside | Meets ≥ 1.39.1 | How the reading was taken |
  | --- | --- | --- | --- |
  | `gpustack/runner:cuda12.9-vllm0.29.0` — the 2026-09-24 build, the newest published CUDA vLLM line | 1.37.1 | **no** | [跑] pulled `--platform linux/arm64` (image `ff0298cffc64`, digest-matched against Docker Hub's current tag), `pip show modelscope` in the container |
  | `gpustack/runner:cuda12.9-vllm0.25.1` — an older build (digest `3f15ce95…`, no longer the Hub's tag) | 1.37.1 | **no** | [跑] same, on the image already local |
  | `gpustack/runner:cann9.1-910b-vllm0.23.0` — the Ascend line this formula synthesizes | 1.39.1 (vllm-ascend 0.23.0) | **yes** | [跑] on the coordinator-named amd64 build machine, `pip show modelscope` in the container |
  | SGLang 0.5.18 runners (e.g. `cuda13.0-sglang0.5.18`) | 1.39.1 | **yes** | [读] PoC-C's first-hand runtime reading (GPU cluster); this seat's re-check of the cann variant produced no new reading (an arm64 image whose manifest has since left the registry) — PoC-C stands |

- **Verdict.** No released CUDA vLLM runner meets the SDK floor today; the Ascend vLLM line and
  SGLang 0.5.18 do. The Dockerfile pins no SDK version, so the number is whatever a build's
  dependency layer resolved — the 2026-09-24 rebuild still carries 1.37.1 (cache hit), vLLM 0.30.0
  runners are built upstream but unreleased, and therefore no "vLLM version → SDK version"
  mapping exists for the operator to consult.

### Version floors

Unchanged: Kubernetes 1.29 for the feature path, 1.23 install. ModelScope adds no Kubernetes
capability requirement. The engine path adds a ModelScope SDK floor (≥ 1.39.1) on the runner
image, documented per the coordinated direction, not enforced by the operator (the operator does
not read the runner image).

## User Stories

### Story 1

As a platform operator on ModelScope-hosted weights, I create a `ModelArtifact` whose source is
`modelScope: {repository: qwen/Qwen2.5-0.5B-Instruct}` and see it resolve to a commit and a
manifest digest, so that a `ModelDeployment` can reference it exactly like a Hugging Face one.

### Story 2

As a user, I reference that artifact from a `ModelDeployment` and the weights arrive on a cold
node — downloaded from ModelScope, verified per file, published read-only — without the engine
ever seeing a partial tree.

### Story 3

As a user, I get `Resolved=False` with a truthful reason instead of silently wrong content: a
branch the hub's API and its git disagree about, a directory the API cannot enumerate, or a
repository I have no access to each stops the artifact with a message.

## Core Features & Acceptance Criteria

- **AC1 — Master resolution.** Creating `modelScope: {repository: qwen/Qwen2.5-0.5B-Instruct}`
  (no revision) defaults the revision to `master`, resolves it to the 40-character commit the
  `commits` endpoint and `git ls-remote` agree on, and writes a manifest of only `sha256:` lines
  with `Resolved=True`. Moving the branch afterwards changes nothing (resolution is once).
- **AC2 — Cross-check guard, red then green.** A fake transport whose `commits` answer disagrees
  with the `ls-remote` answer fails the resolution (`SourceUnavailable`) with the disagreement
  message. The test is shown red by neutering the cross-check (accepting the API's answer alone),
  then green again.
- **AC3 — Truncation guard, red then green.** A fake server listing exactly 3000 entries at the
  root and 3000 in one subdirectory: the root truncation is recovered by per-directory re-listing
  (the manifest comes out complete), and a directory with 3000 direct children fails the
  resolution (`SourceUnavailable`). Shown red by short-circuiting the descent, then green.
- **AC4 — Cold-node materialization.** Node delivery from ModelScope: the plugin downloads at the
  pinned commit, hashes while streaming, publishes, and a Pod mounts the files byte-verified. At
  least one case resumes a download from a checkpoint (transport cut mid-file, restart, the
  received bytes are not fetched again).
- **AC5 — Engine delivery pins the commit.** vLLM and SGLang each leave one piece of evidence
  that the served weights and tokenizer are the pinned commit (snapshot path or file hash, as in
  PoC-C), on runners meeting the documented SDK floor — SGLang's 0.5.18 runner, and for vLLM a
  runner with SDK ≥ 1.39.1 (today: the Ascend `cann9.1-*-vllm0.23.0` line, or a user-named
  image). The rendering itself is engine-agnostic: a role on a runner below the floor renders the
  same way and fails in the engine with the SDK's named `NotExistError`, which the documentation
  explains. Whether the vLLM half needs a GPU node is decided in my-plan with the reason stated.
- **AC6 — Peer sync, free.** Two nodes, ModelScope source: the second node pulls the artifact
  from the first over peer sync and the hub sees zero bytes for it (the S5 acceptance shape,
  re-run with a ModelScope artifact).
- **AC7 — Reason mapping.** Every row of the reason table has a test: `RevisionNotFound` (commit
  `null`; code 10990101004), `AccessDenied` (10010205001, 10010200001, other 404), private
  repository without a token, `SourceUnavailable` (5xx, transport). The unadjudicated cell is
  asserted only as its shared `AccessDenied` mapping, labeled unadjudicated in the test.
- **AC8 — Admission.** A `modelScope` source is accepted; the union rule still refuses two
  members; `revision` defaults to `master`; patterns are legal on it and refused on claim and
  image sources unchanged; the refusal message for the reserved member is gone.

## Notes / Constraints / Caveats

- **One additive API change.** The `ModelArtifact` types are unchanged (`ModelArtifactHubSource`
  is shared, the `modelScope` member already exists), but the plugin's effective configuration
  needs the second endpoint: `NodeModelStoreHub` gains an optional `modelScopeEndpoint`, written
  by the worker from the Setting and read by the plugin. It is an additive, optional field — old
  plugins ignore it, old workers leave it empty and a `modelScope` artifact is then refused at
  the node with the upgrade named. `make generate` runs for it, in the private gen tree per
  STANDARDS §14. (This corrects the draft's "no API change" reading; found while planning the
  plugin's hub selection, and the gen verification rides this task.)
- The webhook's reserved-member refusal constant and its test go away; the status-side
  `UnsupportedSource` branch in the controller stays for objects stored before this version.
- The git cross-check runs **in process** over the smart protocol, so the worker image needs no
  `git` binary and the token never leaves an HTTP header. The advertisement's shape was verified
  against the live endpoint (2026-09-29): the service line, the NUL-delimited capabilities of the
  first ref and the peeled entry all parse; a shape the parser cannot read fails the resolution
  as `SourceUnavailable`, never resolves on less evidence.
- A resolution that needs ls-remote adds one subprocess to the reconcile path; the same
  `modelArtifactResolveTimeout` bounds it, and a git failure is `SourceUnavailable`, never a
  panic.
- The ModelScope SDK inside runners is Python; the operator only renders environment for it. The
  operator never imports it.
- Token conventions carry over: Secret key `token`, read by the controller for resolution and
  revalidation, handed to an engine only through an env `secretKeyRef`, to the plugin only
  through `nodePublishSecretRef`.
- Measured on www.modelscope.cn, 2026-09-29 (spike) and 2026-09-24 (PoC-D): the endpoints, the
  HEAD semantics and the reason mapping above. The private-repository "valid token without
  access" cell remains unadjudicated (needs a second account); its mapping is asserted, its
  distinctness is not claimed anywhere.

## Boundaries

- **Always:** the three D5 guards on every resolution path; tests proven able to fail for both
  guards; English code, comments and docs; per-module squashed commits with `--signoff`, spec
  last; rebase on `origin/main` at each task start and before every e2e and ship.
- **Ask first:** any change to `api/` types beyond the one `NodeModelStoreHub` field above; any
  chart change; any new Setting
  beyond the one above; any new test resource on ModelScope beyond reading the existing public
  repositories.
- **Never:** manifest format changes; cross-hub dedup; touching crew-line files
  (`pkg/setting/types.go`, `node_queue.go`, the MD webhook barrier); creating ModelScope accounts
  or repositories; writing tokens anywhere outside a Secret reference.
- Review round budget: 4. After that, findings are listed, not fixed — except data loss, a broken
  security guarantee (read-only), or cross-tenant leakage, each of which is first raised with the
  coordinator.

## Risks and Mitigations

- **Undocumented endpoint drift** (`commits?Ref=`) → the cross-check with git catches a drift on
  the resolution path itself; a drift breaks toward refusal, never wrong content.
- **Silent truncation** → the per-directory walk plus the 3000-children refusal; unit tests with
  fake servers pin all three shapes (no truncation, recovered truncation, unrecoverable).
- **Runner SDK floor** → option B (confirmed at the spec gate): the docs state the floor, name
  the measured runners on
  each side of it, and point at a user-named image for the rest. No admission rule pretends to
  know a runner's contents; an old runner's `NotExistError` is a loud engine error, and the docs
  explain it.
- **The advertisement's undocumented shape drifts** → the parser refuses what it cannot read
  (`SourceUnavailable`), the same direction every other cross-check failure breaks toward.
- **Private-repository edge cells** unadjudicated for lack of a second account → stated in spec,
  docs and tests; nothing claims a measurement it does not have.

## Design Details

### Commands

- Unit tests: `go test ./pkg/modelartifact/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... ./pkg/modelmanager/...`
- Lint: `make lint` (and the docs gate `make lint docs` when docs change in the same commit).
- Generated code: expected none; if needed, the private gen tree per STANDARDS §14.
- e2e: the `gpustack-operator-e2e` skill on a local kind cluster; case number taken at my-ship as
  main's then-maximum plus one.

### Project Structure

- `pkg/modelartifact/modelscope.go` (+ test): the client — revision resolution with ls-remote
  cross-check, the truncation-recovering listing, the reason classifier, revalidation HEAD,
  token check, file URL.
- `pkg/worker/webhooks/worker/model_artifact.go` (+ test): accept and default the member.
- `pkg/worker/settings/value.go` and `model_store.go` (+ tests): the endpoint Setting and its
  ride on the node's effective configuration (`Layer`).
- `api/worker/v1alpha1/node_model_store.go`: the one additive field
  (`NodeModelStoreHub.modelScopeEndpoint`); `make generate` in the private gen tree.
- `pkg/worker/controllers/worker/model_artifact.go` (+ test): the reconcile branch, pacing and
  conditions shared with Hugging Face, the token warning.
- `pkg/worker/controllers/worker/model_artifact_placement.go` and
  `model_deployment_artifact.go` (+ tests): the ModelScope source's Node/Engine delivery and the
  `MODELSCOPE_*` engine environment.
- `pkg/worker/webhooks/worker/model_deployment.go` (+ test): the owned ModelScope environment
  names an artifact forbids, beside the Hugging Face ones.
- `pkg/modelmanager/driver/authorize.go`, `materialize/materialize.go`, `report/report.go`
  (+ tests): the mount rule accepts the source, the download source carries its hub's kind, and
  the node builds the right hub client from its configuration.
- `docs/` per Documentation above.

### Code Style

Follow the repository's Go conventions; the client mirrors `huggingface.go`'s shape (methods take
the token, a shared client carries no credential), and every failure path names a reason the
status can report.

### Implementation Plan

Tasks run in order (one seat, sequential per the task's standards); every task starts with
`git fetch origin && git rebase origin/main` and lands as its own `--signoff` commit with the
affected packages' tests green. Tests are written first where the task states TDD, and both
guards (AC2, AC3) are mutation-verified — each new guard's test is shown red by a compile-safe
mutation of exactly the mechanism it guards, then green again after reverting.

- [x] **T1 · Admission opens the member**
      Blocked by: None
      Owns: `pkg/worker/webhooks/worker/model_artifact.go`, `pkg/worker/webhooks/worker/model_artifact_test.go`
      Acceptance: AC8 — a `modelScope` source is accepted and validated by the shared hub rules;
      `revision` defaults to `master`; patterns are legal on it; the union message names
      `modelScope` among the accepted members; the reserved-member refusal constant and its tests
      are gone; Hugging Face behavior is untouched (existing cases stay green).
      Verify: `go test ./pkg/worker/webhooks/worker/ -run ModelArtifact`
- [x] **T2 · ModelScope client: revision and reasons (TDD)**
      Blocked by: None
      Owns: `pkg/modelartifact/modelscope.go`, `pkg/modelartifact/modelscope_test.go`
      Acceptance: AC2 red-then-green — the `commits?Ref=` resolution, the `git ls-remote`
      cross-check (HEAD line for a branch, peeled line for an annotated tag, `oauth2` + token for
      private), and the reason table (envelope `Code` first, HTTP fallback) with a fake transport
      and a fake `git` (command seam). A disagreement refuses as `SourceUnavailable` with the
      disagreement message. Mutation: drop the cross-check ⇒ the disagreement case fails.
      Verify: `go test ./pkg/modelartifact/` (the task's cases are `ModelScope`-named; before T2
      lands, the package's existing manifest and filter tests carry the green)
- [x] **T3 · ModelScope client: listing, manifest, revalidation (TDD)**
      Blocked by: T2
      Owns: `pkg/modelartifact/modelscope.go`, `pkg/modelartifact/modelscope_test.go`
      Acceptance: AC3 red-then-green — the walk over `repo/files` (no truncation; recovered
      truncation re-lists per `Root=`; a directory with 3000 direct children fails) against a
      fake server; manifests build through the shared `Filter`/`NewManifest` and hash to the same
      digest as an independently computed v1 manifest; `Revalidate` maps HEAD 200/404 per the
      table; `FileURL` builds the `repo?Revision=&FilePath=` URL; `ValidToken` reads
      `users/me`. Mutations: short-circuit the descent ⇒ the truncation cases fail; classify by
      status only ⇒ a `Code` case fails.
      Verify: `go test ./pkg/modelartifact/`
- [x] **T4 · Setting and effective configuration**
      Blocked by: None
      Owns: `pkg/worker/settings/value.go`, `pkg/worker/settings/model_store.go`, `pkg/modelstore/config.go`, `api/worker/v1alpha1/node_model_store.go`, `pkg/worker/settings/model_store_test.go` and the worker's writer of `NodeModelStore.spec`
      Acceptance: the `model-artifact-modelscope-endpoint` Setting (default, admission as the
      Hugging Face endpoint's), `Layer.ModelScopeEndpoint`, `NodeModelStoreHub.modelScopeEndpoint`
      (optional, additive), and the worker writing it into every node's spec. `make generate` runs
      in the private gen tree (`gen-trees/mam-s10`, branch `mam-s10-gen`) with zero drift.
      Verify: `go test ./pkg/worker/settings/... ./pkg/modelstore/... ./api/worker/...`; gen tree replay zero-diff
- [x] **T5 · Controller: reconcile a ModelScope source**
      Blocked by: T2, T3, T4
      Owns: `pkg/worker/controllers/worker/model_artifact.go`, `pkg/worker/controllers/worker/model_artifact_test.go`
      Acceptance: AC1 (fake hub) — a `modelScope` artifact resolves once to the commit and digest,
      `Resolved=True`, `LastValidatedTime` set; revalidation follows the same
      confirm-then-revoke staircase; `SecretNotFound` and unreadable-Secret behavior match
      Hugging Face; the Secret watch enqueues ModelScope artifacts too; a rejected new token
      emits the Warning event; `UnsupportedSource` stays for pre-existing objects only.
      Verify: `go test ./pkg/worker/controllers/worker/ -run ModelArtifact`
- [x] **T6 · Rendering: delivery and engine environment**
      Blocked by: T4, T5
      Owns: `pkg/worker/controllers/worker/model_artifact_placement.go`, `pkg/worker/controllers/worker/model_deployment_artifact.go` and their tests, `pkg/worker/webhooks/worker/model_deployment.go` and its test
      Acceptance: a ModelScope source resolves to Node delivery (plugin volume, same attributes)
      or Engine delivery (`MODELSCOPE_CACHE`, `MODELSCOPE_DOMAIN` = the Setting URL's host,
      `MODELSCOPE_API_TOKEN` from the Secret, `VLLM_USE_MODELSCOPE` / `SGLANG_USE_MODELSCOPE` by
      engine, owned), with `--revision <commit>` as today; the placement preference and KV
      identity paths work unchanged off the digest; an artifact with patterns under Engine is
      blocked as for Hugging Face; admission refuses the owned ModelScope env names on a
      deployment with `artifactRef`.
      Verify: `go test ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...`
- [x] **T7 · Plugin: hub selection and mount rule**
      Blocked by: T4 (API field), T3 (client)
      Owns: `pkg/modelmanager/driver/authorize.go`, `pkg/modelmanager/materialize/materialize.go`, `pkg/modelmanager/report/report.go` and their tests
      Acceptance: the mount rule accepts a resolved `modelScope` artifact (`ruleNotHub` loosens
      to hub sources of either kind); the download source records its hub kind from
      `ma.Spec.Source`; the environment builds the `ModelScope` client from
      `NodeModelStoreHub.modelScopeEndpoint` for such sources and the Hugging Face client as
      today; a `modelScope` source on a configuration without the endpoint is refused with the
      upgrade message, never resolved against the Hugging Face endpoint.
      Verify: `go test ./pkg/modelmanager/...`
- [x] **T8 · Documentation**
      Blocked by: T6, T7
      Owns: `docs/model-store/artifact.md`, `docs/settings.md`, `docs/README.md` as the docs skill routes
      Acceptance: the source is documented per Documentation above — accepted member, ModelScope
      rows in the reason table, the cross-check and truncation recovery, the all-`sha256` digest
      note, the engine env table, the SDK floor with the measured runner list on each side, the
      Settings row.
      Verify: `bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh . && bash .agents/skills/gpustack-operator-docs/scripts/check-crossrefs.sh . && make lint docs`
- [x] **T9 · e2e on local kind**
      Blocked by: T7, T8
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-<N>.sh` (number taken at my-ship), the skill's case table
      Acceptance: AC4 (cold-node materialization from a public ModelScope repository, byte
      verification, one checkpoint-resume case), AC6 (two worker nodes, peer sync serves the
      second node, the hub sees zero bytes), AC5's SGLang half (engine delivery pins the commit —
      snapshot path and file hash, as PoC-C). The vLLM half runs on a self-named runner image
      (upstream vLLM plus `modelscope>=1.39.1`, CPU kind — the SDK call precedes GPU init, as
      PoC-C's crash loop shows); if the engine cannot reach its download phase without a GPU, the
      half falls back to an SDK-level probe in that image and says so.
      Verify: the e2e skill's full run on the case, green; evidence under `spec-10/raw/`

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. The packages this plan touches all have test files today; no base rework is needed before
T1. Existing Hugging Face cases are the regression baseline and must stay green throughout.

#### Unit tests

New cases land with each task (TDD where marked); the mutation-verification protocol above
applies to AC2 and AC3's guards, and to any other load-bearing new gate that a compile-safe
mutation can neuter. Per-package targets, all on the host toolchain (Linux containers run on the
build machine only when a test genuinely needs amd64; none is expected):

- `pkg/modelartifact` — the reason table row by row (AC7), the cross-check disagreement (AC2),
  the three truncation shapes (AC3), annotated-tag peeling in `ls-remote` output, commit-as-is
  short circuit, `FileURL` shape, HEAD revalidation mapping, `users/me` token check, manifest
  equality with an independently computed v1 digest for an all-`sha256` listing.
- `pkg/worker/webhooks/worker` — acceptance and defaulting of the member (AC8), the union
  message, patterns legality, the owned ModelScope env names on a deployment with `artifactRef`,
  every pre-existing case still green.
- `pkg/worker/controllers/worker` — AC1's reconcile path (fake hub, fake clock), the
  confirm-then-revoke staircase, Secret-watch enqueues, the token Warning, Node/Engine delivery
  resolution for a ModelScope source, the `MODELSCOPE_*` environment (owned vs defaulted), the
  patterns-under-Engine block, placement preference and KV identity unchanged off the digest.
- `pkg/worker/settings` — the Setting's default and admission chain, `Layer`/`Merge` carrying the
  endpoint, defaults validation.
- `pkg/modelstore` — `Layer` gains the field; `Validate`/`Merge` unchanged behavior otherwise.
- `pkg/modelmanager` — the mount rule on a `modelScope` artifact (authorized, and each refusal
  rule still firing), source hub-kind recording, environment client selection (Hugging Face
  source ⇒ Hugging Face client, ModelScope source ⇒ ModelScope client, missing endpoint ⇒ the
  upgrade refusal), download of a ModelScope manifest against a fake hub.

#### Integration tests

Covered by the unit suites' fake-server and fake-hub seams: the controller's reconcile against a
fake ModelScope endpoint (resolution → status write → revalidation), and the plugin's
materialization of a ModelScope manifest (download → verify → publish → mount-ready) against a
fake hub — the same shape the Hugging Face tests use today. No separate integration harness.

#### e2e tests

One case (number taken at my-ship) on a local kind cluster (1 control-plane + 2 workers), per
T9: cold-node materialization from a public ModelScope repository with byte verification and one
checkpoint-resume leg; peer sync serving the second node with zero hub bytes; SGLang engine
delivery pinning the commit; the vLLM half on a self-named runner image with `modelscope>=1.39.1`
(falling back to an SDK-level probe if the engine cannot reach its download phase without a GPU,
stated in the evidence). The `NodeModelStoreHub` field changes the CRD the chart ships, so the
local 7-image chart matrix runs before my-ship per the task standards (REQUIRED for a Go
change).

## Alternatives

- **Trust the API alone** (no ls-remote): rejected — a misspelled parameter or an endpoint drift
  resolves the wrong revision with a 200; D5's first condition exists because that failure is
  silent.
- **Refuse non-commit revisions on ModelScope** (commit-only sources): rejected — it drops the
  common branch workflow and the hub can be made truthful with one cross-check.
- **Refuse the `modelScope` + Engine + vLLM combination until a released runner meets the floor**
  (the spike's option A): rejected in the coordination — the same formula meets the floor on
  Ascend today, so a combination refusal would reject users whose runner already pins, while the
  "vLLM version → SDK version" mapping the refusal would need does not exist (the SDK floats with
  the runner's dependency-layer cache).
- **Block the whole engine path until the runner ecosystem upgrades** (option C): rejected in the
  coordination — SGLang already meets the floor with PoC-C end-to-end evidence, and a
  user-supplied runner image covers vLLM today.
- **ModelScope only under Node delivery** (no engine path): superseded by the direction; kept in
  the record because it was the pre-spike contingency.
- **A second Setting for the ls-remote git URL**: rejected — the git URL is the endpoint's
  host plus the repository path, one derived value, not independent configuration.
- **Pinning the SDK version in the runner Dockerfile**: upstream's file, not this repository's;
  noted in the spike record instead of patched here. The docs' floor plus a user-named image
  covers every runner, pinned or not.

## Open Questions

1. ~~The engine-runner adjudication~~ — **directed and confirmed, option B (2026-09-29 spec
   gate)**: render every engine path
   normally and document the SDK ≥ 1.39.1 floor; users on a runner below it name their own image.
   Recorded in the engine-delivery section; it shapes AC5 and the docs, and adds no admission
   rule.

No open question blocks the draft; the remaining gate is the coordinator's confirmation of the
draft itself.
